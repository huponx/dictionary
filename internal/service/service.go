// Package service implements the gRPC DictionaryService.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/cardref"
	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/srs"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/authn"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/logging"
	"github.com/huponx/gocommon/pagination"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

// Permissions is the rule of every method, read from the authz.v1.rule options of dictionary.proto (the permissions
// themselves are declared in permissions.proto and registered with identity at start). grpcserver.WithMethodPermissions
// is deny by default: an RPC without a rule is closed, and TestCoverage fails the build for it.
var Permissions = grpcserver.PermissionsFromProto(dictionaryv1.File_dictionary_v1_dictionary_proto, dictionaryv1.File_dictionary_v1_permissions_proto)

// Messages of the errors a client may want to recognize start with these codes.
const (
	CodeMaxSavedWords = "MAX_SAVED_WORDS_REACHED"
	// CodeMaxSavedBytes: the content of the list of the user would pass the quota of the user.
	CodeMaxSavedBytes = "SAVED_BYTES_EXCEEDED"
	CodeNoWords       = "NO_WORDS_TO_EXPORT"
	CodeExportTooBig  = "EXPORT_TOO_LARGE"
	// CodeExportUseJob: the list is too long for a synchronous export (ExportSyncMax words).
	CodeExportUseJob = "EXPORT_USE_JOB"
	// CodeExportJobActive: the user already has an export job queued or running. CodeExportJobsDisabled: no export
	// directory is configured.
	CodeExportJobActive    = "EXPORT_JOB_ACTIVE"
	CodeExportJobsDisabled = "EXPORT_JOBS_DISABLED"
	// Categories: the user has the maximum number of them; the name is in use.
	CodeCategoryLimit  = "CATEGORY_LIMIT_REACHED"
	CodeCategoryExists = "CATEGORY_EXISTS"
	// CodeFeatureDisabled: an administrator turned the export off. CodeDictionaryDisabled: same for a dictionary.
	CodeFeatureDisabled    = "FEATURE_DISABLED"
	CodeDictionaryDisabled = "DICTIONARY_DISABLED"
	// Images: no image directory is configured; the bytes are not an image the service accepts; the image is too
	// large (pixels or bytes) even after it was shrunk; the user is over the quota; the card refers to an image
	// that is not the user's.
	CodeImagesDisabled = "IMAGES_DISABLED"
	CodeImageInvalid   = "IMAGE_UNSUPPORTED"
	CodeImageTooLarge  = "IMAGE_TOO_LARGE"
	CodeImageQuota     = "IMAGE_QUOTA_EXCEEDED"
	CodeImageUnknown   = "IMAGE_NOT_FOUND"
	// Audio files, the same five cases (AUDIO_UNSUPPORTED: not an mp3, ogg, wav or m4a file).
	CodeAudioDisabled = "AUDIO_DISABLED"
	CodeAudioInvalid  = "AUDIO_UNSUPPORTED"
	CodeAudioTooLarge = "AUDIO_TOO_LARGE"
	CodeAudioQuota    = "AUDIO_QUOTA_EXCEEDED"
	CodeAudioUnknown  = "AUDIO_NOT_FOUND"
)

// UncategorizedID as a category_id stands for uncategorized = true (for the GET requests, whose query string has no bool).
const UncategorizedID = "uncategorized"

// Limits of the requests.
const (
	maxLookupWords = 20
	maxURLBytes    = 2048
	maxDeckRunes   = 64
	maxQueryLen    = 100
	maxCardImages  = 20
	defaultDeck    = "My Words"
)

// Looker finds the entries of words.
type Looker interface {
	Lookup(ctx context.Context, d string, word string) (*lookup.Word, error)
	LookupMany(ctx context.Context, d string, words []string) (found []*lookup.Word, notFound []string, err error)
}

// Store keeps the saved words (the store package).
type Store interface {
	SaveWord(ctx context.Context, userID string, w store.SavedWord, max int, contentQuota int64, categoryIDs []string) (int, error)
	CountPending(ctx context.Context, userID string, f store.WordFilter) (total, fresh int, capped bool, err error)
	ListPending(ctx context.Context, userID string, afterCreated time.Time, afterWord, query string, f store.WordFilter, limit int) ([]store.SavedWord, error)
	Pending(ctx context.Context, userID string, scope store.Scope, f store.WordFilter, words []string, limit int) ([]store.SavedWord, error)
	ListCategories(ctx context.Context, userID, group string) (store.Categories, error)
	CategoryName(ctx context.Context, userID, id string) (string, error)
	CreateCategory(ctx context.Context, userID, name string, max int) (store.Category, error)
	RenameCategory(ctx context.Context, userID, id, name string) (store.Category, error)
	DeleteCategory(ctx context.Context, userID, id string) error
	SetWordCategories(ctx context.Context, userID, group string, words, add, remove []string) (int, error)
	DeletePending(ctx context.Context, userID, group, word string) error
	SavedByNames(ctx context.Context, userID, group string, words []string) (map[string][]string, error)
	PendingPage(ctx context.Context, userID string, scope store.Scope, f store.WordFilter, afterCreated time.Time, afterWord string, limit int) ([]store.SavedWord, error)
	CountExportable(ctx context.Context, userID string, scope store.Scope, f store.WordFilter) (int, error)
	CreateExportJob(ctx context.Context, userID string, p store.ExportParams, ttl time.Duration) (store.ExportJob, error)
	GetExportJob(ctx context.Context, userID, id string) (store.ExportJob, []store.ExportPart, error)
	CancelExportJob(ctx context.Context, userID, id string, ttl time.Duration) (store.ExportJob, error)
	ClaimExportJob(ctx context.Context, leaseFor time.Duration) (*store.ExportJob, error)
	RenewExportLease(ctx context.Context, id, leaseID string, leaseFor time.Duration) error
	SetExportTotal(ctx context.Context, id, leaseID string, total int) error
	SetExportBuilding(ctx context.Context, id, leaseID string, n int) error
	AddExportPart(ctx context.Context, job store.ExportJob, leaseID string, p store.ExportPart, words []string, after store.SavedWord) error
	FinishExportJob(ctx context.Context, id, leaseID, status, errMsg string, ttl time.Duration) error
	SetExportPartToken(ctx context.Context, userID, id string, n int, hash []byte) (store.ExportPart, time.Time, error)
	ExportPartByToken(ctx context.Context, hash []byte, now time.Time) (userID string, p store.ExportPart, err error)
	DeleteExpiredExportJobs(ctx context.Context, now time.Time, limit int) (int, []store.ExpiredFile, error)
	ExistingExportFileIDs(ctx context.Context, ids []string) (map[string]bool, error)
	MarkExported(ctx context.Context, userID, group string, words []string, remove bool) (int64, error)
	RemoveExported(ctx context.Context, userID, group string) (int64, error)
	PutDeckLink(ctx context.Context, userID string, tokenHash []byte, l store.DeckLink) error
	TakeDeckLink(ctx context.Context, tokenHash []byte, now time.Time) (*store.DeckLink, error)
	Settings(ctx context.Context) (map[string]json.RawMessage, error)
	PutSettings(ctx context.Context, rows map[string]json.RawMessage) error
	GetStorageUsage(ctx context.Context, userID string) (store.StorageUsage, error)
	SetStorageQuota(ctx context.Context, userID string, q store.Quotas) error
	RecomputeUserCounters(ctx context.Context, userID string) (was, now store.Counters, err error)
	ImageBySHA(ctx context.Context, userID string, sha []byte) (*store.Image, error)
	GetImage(ctx context.Context, userID, id string) (*store.Image, error)
	ImagesByIDs(ctx context.Context, userID string, ids []string) ([]store.Image, error)
	InsertImage(ctx context.Context, im store.Image) (existing *store.Image, inserted bool, err error)
	AudioBySHA(ctx context.Context, userID string, sha []byte) (*store.Audio, error)
	GetAudio(ctx context.Context, userID, id string) (*store.Audio, error)
	AudioByIDs(ctx context.Context, userID string, ids []string) ([]store.Audio, error)
	InsertAudio(ctx context.Context, a store.Audio) (existing *store.Audio, inserted bool, err error)
	ReviewQueue(ctx context.Context, userID string, now time.Time, f store.WordFilter, limit, dueAllowed, newAllowed int) ([]store.ReviewCard, error)
	ReviewCounts(ctx context.Context, userID string, now time.Time, f store.WordFilter) (store.ReviewCounts, error)
	ReviewToday(ctx context.Context, userID, group string, now time.Time) (newWords, again int, err error)
	ReviewBonus(ctx context.Context, userID string, now time.Time) (store.ReviewBonus, error)
	AddReviewBonus(ctx context.Context, userID string, now time.Time, newWords, reviews int) (store.ReviewBonus, error)
	PutReview(ctx context.Context, userID, group, word string, now time.Time, grade srs.Grade, next func(srs.State) srs.State) (srs.State, error)
	FSRSParams(ctx context.Context, userID string) (*store.FSRSParams, error)
	PutFSRSParams(ctx context.Context, userID string, p store.FSRSParams) error
	DeleteFSRSParams(ctx context.Context, userID string) error
	ClaimOptimize(ctx context.Context, userID string, now time.Time, gap time.Duration) (bool, error)
	LockOptimize(ctx context.Context, userID string) (unlock func(), ok bool, err error)
	ReviewLogs(ctx context.Context, userID string, limit int) ([][]srs.Review, error)
}

// Prefs keeps the preferences of the user of the request (identity, see the identityclient package).
type Prefs interface {
	Get(ctx context.Context) (string, error)
	Put(ctx context.Context, settingsJSON string) error
}

// Decks builds an Anki deck.
type Decks interface {
	Build(ctx context.Context, deckName string, words []anki.Word, fresh bool) ([]byte, error)
}

// Config of the service.
type Config struct {
	MaxSavedWords int
	// ExportedRetentionHours is the default of the system setting: how long a word that left the list is kept.
	ExportedRetentionHours int
	AudioHosts             []string
	// DeckLinkTTL is how long a download link works, default 15 minutes.
	DeckLinkTTL time.Duration
	// ExportSyncMax is the default of the system setting "export sync max words": the most words a synchronous export
	// packs (2000 when 0); more is EXPORT_USE_JOB.
	ExportSyncMax int
	// ExportJobTTL is how long the parts of a finished export job can be downloaded, default 24 hours.
	ExportJobTTL time.Duration
	// ExportMaxBytes and ExportPartMaxBytes are the defaults of the system settings "export size" and "export part
	// size", and the ceilings are the most an administrator can set (0: the defaults of the settings package).
	ExportMaxBytes, ExportPartMaxBytes               int
	ExportMaxBytesCeiling, ExportPartMaxBytesCeiling int
	// Storage holds the defaults of the system settings of the size of the content and of the quotas, and their
	// ceilings (zero: the defaults of the settings package). The defaults of the quotas of the images and of the audio
	// files are the ones of WithImages and WithAudio unless set here.
	Storage settings.StorageLimits
	// ExportJobBatchMax is the most words an export job reads and packs at once (default 20000).
	ExportJobBatchMax int
}

// Service is the gRPC implementation.
type Service struct {
	dictionaryv1.UnimplementedDictionaryServiceServer

	looker Looker
	// registry lists the dictionaries and their groups
	registry *lookup.Registry
	store    Store
	decks    Decks
	cfg      Config
	prefs    Prefs
	// now is the clock; tests set it
	now func() time.Time

	files    *filestore.Store
	imgOpts  filestore.ImageOptions
	imgQuota int64

	audio      *filestore.Store
	audioMax   int
	audioQuota int64

	// sched keeps the schedule settings of a user for a short time (nil: no cache); optimizing limits the runs of
	// the optimizer of this instance.
	sched *scheduleCache
	// exports keeps the deck files of the export jobs (nil: no jobs)
	exports *filestore.Store
	// cats keeps the category counts of a user for a short time (nil: no cache)
	cats       *categoryCache
	optimizing chan struct{}
}

// WithAudio turns the audio files of the cards on: the files go to files, a file is at most maxBytes and a user
// may keep quota bytes. Without it UploadAudio and GetAudio fail with AUDIO_DISABLED.
func (s *Service) WithAudio(files *filestore.Store, maxBytes int, quota int64) *Service {
	s.audio, s.audioMax, s.audioQuota = files, maxBytes, quota
	return s
}

// WithImages turns the images of the cards on: the files go to files, every image is normalized with opts and a
// user may keep quota bytes. Without it UploadImage fails with IMAGES_DISABLED.
func (s *Service) WithImages(files *filestore.Store, opts filestore.ImageOptions, quota int64) *Service {
	s.files, s.imgOpts, s.imgQuota = files, opts, quota
	return s
}

// WithPrefs sets where the preferences of the users are kept; without it they are always the defaults and
// cannot be changed.
func (s *Service) WithPrefs(p Prefs) *Service {
	s.prefs = p
	return s
}

// New builds the service.
func New(looker Looker, registry *lookup.Registry, st Store, decks Decks, cfg Config) *Service {
	if cfg.ExportJobTTL <= 0 {
		cfg.ExportJobTTL = 24 * time.Hour
	}
	if cfg.DeckLinkTTL <= 0 {
		cfg.DeckLinkTTL = 15 * time.Minute
	}
	return &Service{looker: looker, registry: registry, store: st, decks: decks, cfg: cfg, now: time.Now, optimizing: make(chan struct{}, maxOptimizing)}
}

// userID is the user of the request: the subject of the token, never a field of the request. A service
// token (client credentials) has no user and is refused.
func userID(ctx context.Context) (string, error) {
	p := authn.FromContext(ctx)
	if p == nil || p.Subject == "" {
		return "", apperror.Unauthenticated("a user token is required").WithReason("user_token_required")
	}
	if p.IsService() {
		return "", apperror.PermissionDenied("a user token is required, not a service token").WithReason("service_token_not_allowed")
	}
	if _, err := uuid.Parse(p.Subject); err != nil {
		return "", apperror.Unauthenticated("a user token is required").WithReason("user_token_required")
	}
	return p.Subject, nil
}

// entryOf finds the dictionary of a request in the registry: the id is required.
func (s *Service) entryOf(id string) (lookup.Entry, error) {
	e, ok := s.registry.Entry(id)
	if !ok {
		return lookup.Entry{}, apperror.InvalidArgument("unknown dictionary").WithReason("unknown_dictionary")
	}
	return e, nil
}

// groupOf finds the group of a request in the registry: the group is required, a request has no default.
func (s *Service) groupOf(id string) (lookup.Group, error) {
	g, ok := s.registry.Group(id)
	if !ok {
		return lookup.Group{}, apperror.InvalidArgument("unknown group").WithReason("unknown_group")
	}
	return g, nil
}

// lookupErr maps the errors of a lookup. Details of a provider failure are logged, not sent.
func lookupErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, lookup.ErrInvalidWord):
		return apperror.InvalidArgument("not a valid word: letters, digits, spaces, apostrophes, hyphens and dots, at most 64 characters").WithReason("invalid_word")
	case errors.Is(err, lookup.ErrNotFound):
		return apperror.NotFound("word not found").WithReason("word_not_found")
	case errors.Is(err, lookup.ErrUnavailable):
		logging.FromCtx(ctx).Warn("dictionary provider failed", zap.Error(err))
		return apperror.Unavailable("the dictionary is not available, try again later").WithReason("dictionary_unavailable")
	}
	logging.FromCtx(ctx).Error("lookup", zap.Error(err))
	return apperror.Internal("internal error")
}

func internal(ctx context.Context, what string, err error) error {
	logging.FromCtx(ctx).Error(what, zap.Error(err))
	return apperror.Internal("internal error")
}

func toProtoWord(w *lookup.Word) *dictionaryv1.Word {
	out := &dictionaryv1.Word{
		Text: w.Text, Phonetic: w.Phonetic, PhoneticUk: w.PhoneticUK, PhoneticUs: w.PhoneticUS,
		AudioUk: w.AudioUK, AudioUs: w.AudioUS, PartOfSpeech: w.PartOfSpeech,
	}
	for _, m := range w.Meanings {
		out.Meanings = append(out.Meanings, &dictionaryv1.Meaning{
			Definition: m.Definition, Examples: m.Examples, MachineTranslated: m.MachineTranslated,
		})
	}
	return out
}

// LookupWord returns the entry of one word.
func (s *Service) LookupWord(ctx context.Context, req *dictionaryv1.LookupWordRequest) (*dictionaryv1.LookupWordResponse, error) {
	e, err := s.entryOf(req.GetDictionary())
	if err != nil {
		return nil, err
	}
	if err := s.requireDictionary(ctx, e.ID); err != nil {
		return nil, err
	}
	w, err := s.looker.Lookup(ctx, e.ID, req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	return &dictionaryv1.LookupWordResponse{Word: toProtoWord(w)}, nil
}

// LookupWords returns the entries of several words; the words without an entry are listed apart.
func (s *Service) LookupWords(ctx context.Context, req *dictionaryv1.LookupWordsRequest) (*dictionaryv1.LookupWordsResponse, error) {
	e, err := s.entryOf(req.GetDictionary())
	if err != nil {
		return nil, err
	}
	if n := len(req.GetWords()); n == 0 || n > maxLookupWords {
		return nil, apperror.InvalidArgument("send between 1 and 20 words").WithReason("word_batch_range", "max", "20")
	}
	if err := s.requireDictionary(ctx, e.ID); err != nil {
		return nil, err
	}
	found, notFound, err := s.looker.LookupMany(ctx, e.ID, req.GetWords())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	resp := &dictionaryv1.LookupWordsResponse{NotFound: notFound}
	for _, w := range found {
		resp.Words = append(resp.Words, toProtoWord(w))
	}
	return resp, nil
}

// SaveWord adds a word to the pending list of the user.
func (s *Service) SaveWord(ctx context.Context, req *dictionaryv1.SaveWordRequest) (*dictionaryv1.SaveWordResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	group, err := s.groupOf(req.GetGroup())
	if err != nil {
		return nil, err
	}
	word, err := lookup.NormalizeWord(req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireGroupAllowed(ctx, sys, group); err != nil {
		return nil, err
	}
	if len(req.GetContent()) > sys.MaxContentBytes {
		return nil, apperror.InvalidArgument(fmt.Sprintf("content is larger than %s", byteSize(sys.MaxContentBytes))).WithReason("content_too_large", "limit", byteSize(sys.MaxContentBytes))
	}
	for _, u := range []string{req.GetAudioUk(), req.GetAudioUs()} {
		if _, uploaded := cardref.AudioID(u); u != "" && !uploaded && (len(u) > maxURLBytes || !anki.AllowedURL(u, s.cfg.AudioHosts)) {
			return nil, apperror.InvalidArgument("audio must be an uploaded file or an https URL on an allowed host").WithReason("audio_source_invalid")
		}
	}
	if ids := cardref.IDs(req.GetContent()); len(ids) > maxCardImages {
		return nil, apperror.InvalidArgument("a card can have at most 20 images").WithReason("too_many_images", "max", "20")
	}
	n, err := s.store.SaveWord(ctx, user, store.SavedWord{
		Group: group.ID, Word: word, Content: req.GetContent(), AudioUK: req.GetAudioUk(), AudioUS: req.GetAudioUs(),
	}, sys.MaxSavedWords, sys.ContentUserQuota, req.GetCategoryIds())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("category not found").WithReason("category_not_found")
	case errors.Is(err, store.ErrLimit):
		return nil, apperror.Wrap(codes.ResourceExhausted, CodeMaxSavedWords+": maximum number of saved words reached, export or delete some first", nil).WithReason("max_saved_words_reached")
	case errors.Is(err, store.ErrBytesLimit):
		return nil, apperror.Wrap(codes.ResourceExhausted, CodeMaxSavedBytes+": the storage of the saved words is full, export or delete some first", nil).WithReason("saved_bytes_exceeded")
	case errors.Is(err, store.ErrUnknownImage):
		return nil, apperror.InvalidArgument(CodeImageUnknown + ": the card refers to an image that does not exist").WithReason("image_not_found")
	case errors.Is(err, store.ErrUnknownAudio):
		return nil, apperror.InvalidArgument(CodeAudioUnknown + ": the card refers to an audio file that does not exist").WithReason("audio_not_found")
	case errors.Is(err, store.ErrUnknownUser):
		return nil, apperror.Unauthenticated("unknown user").WithReason("unknown_user")
	default:
		return nil, internal(ctx, "save word", err)
	}
	return &dictionaryv1.SaveWordResponse{SavedCount: int32(n)}, nil
}

// ListSavedWords lists the pending words of the user, newest first.
func (s *Service) ListSavedWords(ctx context.Context, req *dictionaryv1.ListSavedWordsRequest) (*dictionaryv1.ListSavedWordsResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(req.GetPageSize())
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(req.GetQuery())
	if utf8.RuneCountInString(query) > maxQueryLen {
		return nil, apperror.InvalidArgument("query is too long").WithReason("query_too_long")
	}
	var (
		afterCreated time.Time
		afterWord    string
	)
	if req.GetPageToken() != "" {
		parts, err := pagination.Decode(req.GetPageToken(), 2)
		if err != nil {
			return nil, err
		}
		afterCreated, err = time.Parse(time.RFC3339Nano, parts[0])
		if err != nil || parts[1] == "" {
			return nil, apperror.InvalidArgument("invalid page token").WithReason("invalid_page_token")
		}
		afterWord = parts[1]
	}
	filter, err := s.wordFilter(req.GetGroup(), req.GetCategoryId(), req.GetUncategorized())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListPending(ctx, user, afterCreated, afterWord, query, filter, size+1)
	if err != nil {
		return nil, internal(ctx, "list saved words", err)
	}
	total, fresh, capped, err := s.store.CountPending(ctx, user, filter)
	if err != nil {
		return nil, internal(ctx, "count saved words", err)
	}
	resp := &dictionaryv1.ListSavedWordsResponse{Total: int32(total), NewTotal: int32(fresh), CountsCapped: capped}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		resp.NextPageToken = pagination.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.Word)
	}
	for _, r := range rows {
		next := ""
		if !r.NextReview.IsZero() {
			next = r.NextReview.UTC().Format(time.RFC3339)
		}
		resp.Words = append(resp.Words, &dictionaryv1.SavedWord{
			Group: r.Group, Word: r.Word, Content: r.Content, AudioUk: r.AudioUK, AudioUs: r.AudioUS,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), Exported: r.Exported, CategoryIds: r.CategoryIDs,
			NextReviewAt: next, Learning: r.Learning,
		})
	}
	return resp, nil
}

// maxCheckWords is the most words CheckSavedWords takes (a lookup has at most 20, plus the word of the new card).
const maxCheckWords = 100

// CheckSavedWords tells which of the words are in the list of the user, with their categories.
func (s *Service) CheckSavedWords(ctx context.Context, req *dictionaryv1.CheckSavedWordsRequest) (*dictionaryv1.CheckSavedWordsResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	group, err := s.groupOf(req.GetGroup())
	if err != nil {
		return nil, err
	}
	if len(req.GetWords()) > maxCheckWords {
		return nil, apperror.InvalidArgument(fmt.Sprintf("send at most %d words", maxCheckWords)).WithReason("word_batch_max", "max", fmt.Sprint(maxCheckWords))
	}
	seen := map[string]bool{}
	var names []string
	for _, w := range req.GetWords() {
		n, err := lookup.NormalizeWord(w)
		if err != nil || seen[n] { // a word that cannot be saved is not saved
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	found, err := s.store.SavedByNames(ctx, user, group.ID, names)
	if err != nil {
		return nil, internal(ctx, "check saved words", err)
	}
	resp := &dictionaryv1.CheckSavedWordsResponse{}
	for _, n := range names { // the order of the request
		if ids, ok := found[n]; ok {
			resp.Words = append(resp.Words, &dictionaryv1.SavedWordRef{Word: n, CategoryIds: ids})
		}
	}
	return resp, nil
}

// DeleteSavedWord removes a word of the list of the user.
func (s *Service) DeleteSavedWord(ctx context.Context, req *dictionaryv1.DeleteSavedWordRequest) (*dictionaryv1.DeleteSavedWordResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	group, err := s.groupOf(req.GetGroup())
	if err != nil {
		return nil, err
	}
	word, err := lookup.NormalizeWord(req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	switch err := s.store.DeletePending(ctx, user, group.ID, word); {
	case err == nil:
		return &dictionaryv1.DeleteSavedWordResponse{}, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("saved word not found").WithReason("saved_word_not_found")
	default:
		return nil, internal(ctx, "delete saved word", err)
	}
}

// maxBatchWords is the most words one request names (the selected words of an export, the words of
// UpdateWordCategories), whatever the limit of the list is.
const maxBatchWords = 5000

// useJob is the error of a list too long for a synchronous export; it tells how many words the export has (a
// bounded count, see CountPending).
func (s *Service) useJob(ctx context.Context, user string, src exportSource, f store.WordFilter, syncMax int) error {
	total, fresh, capped, err := s.store.CountPending(ctx, user, f)
	if err != nil {
		return internal(ctx, "count words to export", err)
	}
	n := fresh
	if src.scope == dictionaryv1.ExportScope_EXPORT_SCOPE_ALL {
		n = total
	}
	more := ""
	if capped && n >= store.CountCap(f) {
		more = "+"
	}
	return apperror.FailedPrecondition(fmt.Sprintf("%s: %d%s words are too many for one deck (at most %d), export in parts",
		CodeExportUseJob, n, more, syncMax)).WithReason("export_use_job", "count", fmt.Sprint(n)+more, "max", fmt.Sprint(syncMax))
}

// builtDeck is a deck made of the words of a user.
type builtDeck struct {
	group string
	name  string
	data  []byte
	words []string
}

// exportSource tells which words an export packs (the fields of ExportAnkiRequest and CreateDeckLinkRequest).
type exportSource struct {
	group         string
	scope         dictionaryv1.ExportScope
	categoryID    string
	uncategorized bool
	words         []string
}

// buildDeck packs the new words of the user (or the whole list, see scope; only the words of a category, or
// exactly the words asked for) in a deck. A deck that could not be built or is too large is an error and leaves
// the words as they were. The name of a deck without one is the name of its category.
func (s *Service) buildDeck(ctx context.Context, user, rawName string, src exportSource) (*builtDeck, error) {
	name, err := deckName(rawName)
	if err != nil {
		return nil, err
	}
	var (
		filter store.WordFilter
		only   []string
	)
	group, err := s.groupOf(src.group)
	if err != nil {
		return nil, err
	}
	filter.Group = group.ID
	switch {
	case len(src.words) > 0:
		seen := map[string]bool{}
		only = []string{}
		for _, w := range src.words {
			if n, err := lookup.NormalizeWord(w); err == nil && !seen[n] {
				seen[n] = true
				only = append(only, n)
			}
		}
		if len(only) > maxBatchWords {
			return nil, apperror.InvalidArgument("too many words to export").WithReason("export_too_many_words")
		}
	default:
		filter = categoryFilter(group.ID, src.categoryID, src.uncategorized)
		if filter.CategoryID != "" {
			catName, err := s.store.CategoryName(ctx, user, filter.CategoryID)
			if errors.Is(err, store.ErrNotFound) {
				return nil, apperror.NotFound("category not found").WithReason("category_not_found")
			}
			if err != nil {
				return nil, internal(ctx, "read category", err)
			}
			if strings.TrimSpace(rawName) == "" {
				name = catName
			}
		}
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	// read one word more than the limit to know the list is too long, without reading all of it
	limit := sys.ExportSyncMaxWords + 1
	if only != nil {
		limit = 0 // at most maxBatchWords, checked above
	}
	words, err := s.store.Pending(ctx, user, storeScope(src.scope), filter, only, limit)
	if err != nil {
		return nil, internal(ctx, "read pending words", err)
	}
	if only == nil && len(words) > sys.ExportSyncMaxWords {
		return nil, s.useJob(ctx, user, src, filter, sys.ExportSyncMaxWords)
	}
	if len(words) == 0 {
		return nil, apperror.InvalidArgument(CodeNoWords + ": no saved words to export").WithReason("no_words_to_export")
	}
	keys := make([]string, len(words))
	for i, w := range words {
		keys[i] = w.Word
	}
	data, err := s.packDeck(ctx, user, name, words, s.userPrefs(ctx).FreshDeck, sys.ExportMaxBytes)
	if errors.Is(err, errDeckTooLarge) {
		return nil, apperror.FailedPrecondition(CodeExportTooBig + ": the deck is too large, delete some words and export again").WithReason("export_too_large")
	}
	if err != nil {
		return nil, err
	}
	return &builtDeck{group: group.ID, name: name, data: data, words: keys}, nil
}

// packDeck builds the deck of the words: the images and the audio files of the cards are attached, then the deck
// is built. A deck larger than maxBytes is anki.ErrTooLarge (wrapped in an error for the caller of a request).
func (s *Service) packDeck(ctx context.Context, user, name string, words []store.SavedWord, fresh bool, maxBytes int) ([]byte, error) {
	cards := make([]anki.Word, len(words))
	for i, w := range words {
		cards[i] = anki.Word{Word: w.Word, Content: w.Content, AudioUK: w.AudioUK, AudioUS: w.AudioUS}
	}
	if err := s.attachImages(ctx, user, cards); err != nil {
		return nil, err
	}
	if err := s.attachAudio(ctx, user, cards); err != nil {
		return nil, err
	}
	data, err := s.decks.Build(anki.WithMaxBytes(ctx, maxBytes), name, cards, fresh)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, anki.ErrTooLarge):
		return nil, fmt.Errorf("%w: %w", errDeckTooLarge, err)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	default:
		return nil, internal(ctx, "build deck", err)
	}
}

// errDeckTooLarge marks the error of packDeck for a deck over the size limit.
var errDeckTooLarge = errors.New("deck too large")

// storeScope: only ALL packs the exported words again; new is the default.
func storeScope(s dictionaryv1.ExportScope) store.Scope {
	if s == dictionaryv1.ExportScope_EXPORT_SCOPE_ALL {
		return store.ScopeAll
	}
	return store.ScopeNew
}

// removesAfterExport: remove after export is in effect for the user, which is the system setting and the
// preference of the user both on. identity being down means the defaults, that is remove (as it always was).
func (s *Service) removesAfterExport(ctx context.Context, sys settings.System) bool {
	return s.userPrefs(ctx).RemovesAfterExport(sys)
}

// ExportAnki packs the new words (or all of the list) in a deck and marks them exported; with remove after
// export they also leave the list. A deck that could not be built or is too large leaves the words as they were.
func (s *Service) ExportAnki(ctx context.Context, req *dictionaryv1.ExportAnkiRequest) (*dictionaryv1.ExportAnkiResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	sys, err := s.requireExport(ctx, false)
	if err != nil {
		return nil, err
	}
	d, err := s.buildDeck(ctx, user, req.GetDeckName(), exportSource{req.GetGroup(), req.GetScope(), req.GetCategoryId(), req.GetUncategorized(), req.GetWords()})
	if err != nil {
		return nil, err
	}
	if _, err := s.store.MarkExported(ctx, user, d.group, d.words, s.removesAfterExport(ctx, sys)); err != nil {
		return nil, internal(ctx, "mark exported", err)
	}
	return &dictionaryv1.ExportAnkiResponse{Filename: fileName(d.name), Data: d.data, WordCount: int32(len(d.words))}, nil
}

// RemoveExportedWords takes the exported words out of the list; the new ones stay.
func (s *Service) RemoveExportedWords(ctx context.Context, req *dictionaryv1.RemoveExportedWordsRequest) (*dictionaryv1.RemoveExportedWordsResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	group, err := s.groupOf(req.GetGroup())
	if err != nil {
		return nil, err
	}
	n, err := s.store.RemoveExported(ctx, user, group.ID)
	if err != nil {
		return nil, internal(ctx, "remove exported words", err)
	}
	return &dictionaryv1.RemoveExportedWordsResponse{Removed: int32(n)}, nil
}

// tokenHash is what the store keeps of a link token.
func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CreateDeckLink is ExportAnki that keeps the deck behind a random token for DeckLinkTTL. The link is
// stored before the words are marked exported, so a failure in between leaves the words pending.
func (s *Service) CreateDeckLink(ctx context.Context, req *dictionaryv1.CreateDeckLinkRequest) (*dictionaryv1.CreateDeckLinkResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	sys, err := s.requireExport(ctx, true)
	if err != nil {
		return nil, err
	}
	d, err := s.buildDeck(ctx, user, req.GetDeckName(), exportSource{req.GetGroup(), req.GetScope(), req.GetCategoryId(), req.GetUncategorized(), req.GetWords()})
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, internal(ctx, "token", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(s.cfg.DeckLinkTTL)
	link := store.DeckLink{Filename: fileName(d.name), Data: d.data, WordCount: len(d.words), ExpiresAt: expires}
	if err := s.store.PutDeckLink(ctx, user, tokenHash(token), link); err != nil {
		return nil, internal(ctx, "store deck link", err)
	}
	if _, err := s.store.MarkExported(ctx, user, d.group, d.words, s.removesAfterExport(ctx, sys)); err != nil {
		return nil, internal(ctx, "mark exported", err)
	}
	return &dictionaryv1.CreateDeckLinkResponse{
		Token: token, Filename: link.Filename, WordCount: int32(len(d.words)), ExpiresAt: expires.UTC().Format(time.RFC3339),
	}, nil
}

// DownloadDeck hands the deck of a link over once. Only a service token (the gateway) may call it; the
// token of the link is the whole authorization of the download.
func (s *Service) DownloadDeck(ctx context.Context, req *dictionaryv1.DownloadDeckRequest) (*dictionaryv1.DownloadDeckResponse, error) {
	if p := authn.FromContext(ctx); p == nil || !p.IsService() {
		return nil, apperror.PermissionDenied("a service token is required").WithReason("service_token_required")
	}
	token := req.GetToken()
	if len(token) < 20 || len(token) > 128 {
		return nil, apperror.NotFound("the download link does not exist or has expired").WithReason("download_link_gone")
	}
	l, err := s.store.TakeDeckLink(ctx, tokenHash(token), time.Now())
	switch {
	case err == nil:
		return &dictionaryv1.DownloadDeckResponse{Filename: l.Filename, Data: l.Data}, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("the download link does not exist or has expired").WithReason("download_link_gone")
	default:
		return nil, internal(ctx, "take deck link", err)
	}
}

// deckName defaults and checks the name of a deck: it ends up in the file name.
func deckName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultDeck, nil
	}
	if utf8.RuneCountInString(name) > maxDeckRunes {
		return "", apperror.InvalidArgument("deck name is longer than 64 characters").WithReason("deck_name_too_long")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", apperror.InvalidArgument("deck name has control characters").WithReason("deck_name_control_chars")
		}
	}
	return name, nil
}

// fileName is the suggested download name: the deck name without path characters.
func fileName(deck string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, deck) + ".apkg"
}

// UploadImage normalizes and stores an image of the user.
func (s *Service) UploadImage(ctx context.Context, req *dictionaryv1.UploadImageRequest) (*dictionaryv1.UploadImageResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	if s.files == nil {
		return nil, apperror.FailedPrecondition(CodeImagesDisabled + ": images are not enabled").WithReason("images_disabled")
	}
	if len(req.GetData()) == 0 {
		return nil, apperror.InvalidArgument(CodeImageInvalid + ": no image").WithReason("image_unsupported")
	}
	res, err := filestore.NormalizeImage(req.GetData(), s.imgOpts)
	switch {
	case err == nil:
	case errors.Is(err, filestore.ErrImageFormat):
		return nil, apperror.InvalidArgument(CodeImageInvalid + ": the file is not a png, jpeg, gif or webp image").WithReason("image_unsupported")
	case errors.Is(err, filestore.ErrPixels), errors.Is(err, filestore.ErrImageTooLarge):
		return nil, apperror.InvalidArgument(CodeImageTooLarge + ": the image is too large").WithReason("image_too_large")
	default:
		return nil, internal(ctx, "normalize image", err)
	}
	sha := filestore.Hash(res.Data)
	// the same picture of the same user is stored once
	switch ex, err := s.store.ImageBySHA(ctx, user, sha); {
	case err == nil:
		return &dictionaryv1.UploadImageResponse{Id: ex.ID}, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, internal(ctx, "find image", err)
	}
	used, limit, err := s.storageOf(ctx, user, func(u store.StorageUsage, sys settings.System) (int64, int64) {
		return u.ImageBytes, effectiveQuota(u.ImageQuota, sys.ImageUserQuota)
	})
	if err != nil {
		return nil, err
	}
	if used+int64(len(res.Data)) > limit {
		return nil, apperror.Wrap(codes.ResourceExhausted, CodeImageQuota+": the image storage of the user is full, remove images from cards", nil).WithReason("image_quota_exceeded")
	}
	im := store.Image{ID: uuid.NewString(), UserID: user, ContentType: res.ContentType, Size: len(res.Data), SHA256: sha}
	ext := filestore.Ext(res.ContentType)
	if err := s.files.Write(user, im.ID, ext, res.Data); err != nil {
		return nil, internal(ctx, "write image", err)
	}
	ex, inserted, err := s.store.InsertImage(ctx, im)
	switch {
	case err != nil:
		_ = s.files.Remove(user, im.ID, ext)
		if errors.Is(err, store.ErrUnknownUser) {
			return nil, apperror.Unauthenticated("unknown user").WithReason("unknown_user")
		}
		return nil, internal(ctx, "record image", err)
	case !inserted: // a parallel upload of the same picture won
		_ = s.files.Remove(user, im.ID, ext)
		return &dictionaryv1.UploadImageResponse{Id: ex.ID}, nil
	}
	return &dictionaryv1.UploadImageResponse{Id: im.ID}, nil
}

// GetImage returns an image of the user.
func (s *Service) GetImage(ctx context.Context, req *dictionaryv1.GetImageRequest) (*dictionaryv1.GetImageResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	if s.files == nil {
		return nil, apperror.FailedPrecondition(CodeImagesDisabled + ": images are not enabled").WithReason("images_disabled")
	}
	notFound := apperror.NotFound("image not found").WithReason("image_not_found")
	if _, err := uuid.Parse(req.GetId()); err != nil {
		return nil, notFound
	}
	im, err := s.store.GetImage(ctx, user, strings.ToLower(req.GetId()))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound
	default:
		return nil, internal(ctx, "get image", err)
	}
	data, err := s.files.Read(user, im.ID, filestore.Ext(im.ContentType))
	switch {
	case err == nil:
	case errors.Is(err, filestore.ErrNotFound):
		return nil, notFound
	default:
		return nil, internal(ctx, "read image", err)
	}
	return &dictionaryv1.GetImageResponse{ContentType: im.ContentType, Data: data}, nil
}

// UploadAudio stores an audio file of the user as it is.
func (s *Service) UploadAudio(ctx context.Context, req *dictionaryv1.UploadAudioRequest) (*dictionaryv1.UploadAudioResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	if s.audio == nil {
		return nil, apperror.FailedPrecondition(CodeAudioDisabled + ": audio files are not enabled").WithReason("audio_disabled")
	}
	ct, err := filestore.SniffAudio(req.GetData(), s.audioMax)
	switch {
	case err == nil:
	case errors.Is(err, filestore.ErrAudioFormat):
		return nil, apperror.InvalidArgument(CodeAudioInvalid + ": the file is not an mp3, ogg, wav or m4a audio file").WithReason("audio_unsupported")
	case errors.Is(err, filestore.ErrAudioTooLarge):
		return nil, apperror.InvalidArgument(CodeAudioTooLarge + ": the audio file is too large").WithReason("audio_too_large")
	default:
		return nil, internal(ctx, "check audio", err)
	}
	data := req.GetData()
	sha := filestore.Hash(data)
	// the same file of the same user is stored once
	switch ex, err := s.store.AudioBySHA(ctx, user, sha); {
	case err == nil:
		return &dictionaryv1.UploadAudioResponse{Id: ex.ID}, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, internal(ctx, "find audio", err)
	}
	used, limit, err := s.storageOf(ctx, user, func(u store.StorageUsage, sys settings.System) (int64, int64) {
		return u.AudioBytes, effectiveQuota(u.AudioQuota, sys.AudioUserQuota)
	})
	if err != nil {
		return nil, err
	}
	if used+int64(len(data)) > limit {
		return nil, apperror.Wrap(codes.ResourceExhausted, CodeAudioQuota+": the audio storage of the user is full, remove audio from cards", nil).WithReason("audio_quota_exceeded")
	}
	a := store.Audio{ID: uuid.NewString(), UserID: user, ContentType: ct, Size: len(data), SHA256: sha}
	ext := filestore.Ext(ct)
	if err := s.audio.Write(user, a.ID, ext, data); err != nil {
		return nil, internal(ctx, "write audio", err)
	}
	ex, inserted, err := s.store.InsertAudio(ctx, a)
	switch {
	case err != nil:
		_ = s.audio.Remove(user, a.ID, ext)
		if errors.Is(err, store.ErrUnknownUser) {
			return nil, apperror.Unauthenticated("unknown user").WithReason("unknown_user")
		}
		return nil, internal(ctx, "record audio", err)
	case !inserted: // a parallel upload of the same file won
		_ = s.audio.Remove(user, a.ID, ext)
		return &dictionaryv1.UploadAudioResponse{Id: ex.ID}, nil
	}
	return &dictionaryv1.UploadAudioResponse{Id: a.ID}, nil
}

// GetAudio returns an audio file of the user.
func (s *Service) GetAudio(ctx context.Context, req *dictionaryv1.GetAudioRequest) (*dictionaryv1.GetAudioResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	if s.audio == nil {
		return nil, apperror.FailedPrecondition(CodeAudioDisabled + ": audio files are not enabled").WithReason("audio_disabled")
	}
	notFound := apperror.NotFound("audio not found").WithReason("audio_not_found")
	if _, err := uuid.Parse(req.GetId()); err != nil {
		return nil, notFound
	}
	a, err := s.store.GetAudio(ctx, user, strings.ToLower(req.GetId()))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound
	default:
		return nil, internal(ctx, "get audio", err)
	}
	data, err := s.audio.Read(user, a.ID, filestore.Ext(a.ContentType))
	switch {
	case err == nil:
	case errors.Is(err, filestore.ErrNotFound):
		return nil, notFound
	default:
		return nil, internal(ctx, "read audio", err)
	}
	return &dictionaryv1.GetAudioResponse{ContentType: a.ContentType, Data: data}, nil
}

// attachAudio reads the files of the uploaded audio the cards refer to, for the deck. A missing file is left
// out of the map: the card has no sound for it.
func (s *Service) attachAudio(ctx context.Context, user string, cards []anki.Word) error {
	var ids []string
	for _, c := range cards {
		ids = append(ids, cardref.AudioIDs(c.AudioUK, c.AudioUS)...)
	}
	if len(ids) == 0 || s.audio == nil {
		return nil
	}
	rows, err := s.store.AudioByIDs(ctx, user, ids)
	if err != nil {
		return internal(ctx, "read audio", err)
	}
	loaded := map[string]anki.Image{}
	for _, a := range rows {
		ext := filestore.Ext(a.ContentType)
		data, err := s.audio.Read(user, a.ID, ext)
		if err != nil {
			logging.FromCtx(ctx).Warn("audio skipped", zap.String("id", a.ID), zap.Error(err))
			continue
		}
		loaded[cardref.AudioPrefix+a.ID] = anki.Image{Data: data, Ext: ext}
	}
	for i := range cards {
		cards[i].AudioFiles = loaded
	}
	return nil
}

// attachImages reads the files of the images the cards refer to, for the deck. A missing image is left out
// of the map: the deck drops its tag.
func (s *Service) attachImages(ctx context.Context, user string, cards []anki.Word) error {
	var ids []string
	for _, c := range cards {
		ids = append(ids, cardref.IDs(c.Content)...)
	}
	if len(ids) == 0 || s.files == nil {
		return nil
	}
	rows, err := s.store.ImagesByIDs(ctx, user, ids)
	if err != nil {
		return internal(ctx, "read images", err)
	}
	loaded := map[string]anki.Image{}
	for _, im := range rows {
		ext := filestore.Ext(im.ContentType)
		data, err := s.files.Read(user, im.ID, ext)
		if err != nil {
			logging.FromCtx(ctx).Warn("image skipped", zap.String("id", im.ID), zap.Error(err))
			continue
		}
		loaded[im.ID] = anki.Image{Data: data, Ext: ext}
	}
	for i := range cards {
		cards[i].Images = loaded
	}
	return nil
}

// wordFilter is the filter of a request: its group (required, in the registry) and its category filter.
func (s *Service) wordFilter(group, categoryID string, uncategorized bool) (store.WordFilter, error) {
	g, err := s.groupOf(group)
	if err != nil {
		return store.WordFilter{}, err
	}
	return categoryFilter(g.ID, categoryID, uncategorized), nil
}

// categoryFilter is the filter of a group with the category filter of a request: uncategorized wins over a category id.
func categoryFilter(group, categoryID string, uncategorized bool) store.WordFilter {
	// a GET reaches the service as a query string, where the gateway cannot write a bool: the id "uncategorized" says the same
	if uncategorized || strings.EqualFold(strings.TrimSpace(categoryID), UncategorizedID) {
		return store.WordFilter{Group: group, Uncategorized: true}
	}
	return store.WordFilter{Group: group, CategoryID: strings.ToLower(strings.TrimSpace(categoryID))}
}
