// Package service implements the gRPC DictionaryService.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/cardref"
	"github.com/huponx/dictionary/internal/imagestore"
	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/settings"
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

// Permissions required per method. grpcserver.WithMethodPermissions is deny by default, so a new RPC
// has no entry here until somebody decides who may call it.
var Permissions = map[string]string{
	"/dictionary.v1.DictionaryService/LookupWord":          "dictionary:word:read",
	"/dictionary.v1.DictionaryService/LookupWords":         "dictionary:word:read",
	"/dictionary.v1.DictionaryService/ListSavedWords":      "dictionary:word:read",
	"/dictionary.v1.DictionaryService/SaveWord":            "dictionary:word:write",
	"/dictionary.v1.DictionaryService/DeleteSavedWord":     "dictionary:word:write",
	"/dictionary.v1.DictionaryService/ExportAnki":          "dictionary:word:write",
	"/dictionary.v1.DictionaryService/CreateDeckLink":      "dictionary:word:write",
	"/dictionary.v1.DictionaryService/RemoveExportedWords": "dictionary:word:write",
	"/dictionary.v1.DictionaryService/UploadImage":         "dictionary:word:write",
	"/dictionary.v1.DictionaryService/GetImage":            "dictionary:word:read",
	"/dictionary.v1.DictionaryService/GetSettings":         "dictionary:settings:read",
	"/dictionary.v1.DictionaryService/UpdateSettings":      "dictionary:settings:write",
	"/dictionary.v1.DictionaryService/GetMySettings":       "dictionary:word:read",
	"/dictionary.v1.DictionaryService/UpdateMySettings":    "dictionary:word:read",
	// the gateway calls it for the anonymous download request with its service token: any authenticated
	// caller passes the permission check, the method itself accepts only a service token
	"/dictionary.v1.DictionaryService/DownloadDeck": grpcserver.Public,
}

// Messages of the errors a client may want to recognize start with these codes.
const (
	CodeMaxSavedWords = "MAX_SAVED_WORDS_REACHED"
	CodeNoWords       = "NO_WORDS_TO_EXPORT"
	CodeExportTooBig  = "EXPORT_TOO_LARGE"
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
)

// Limits of the requests.
const (
	maxLookupWords  = 20
	maxContentBytes = 64 << 10
	maxURLBytes     = 2048
	maxDeckRunes    = 64
	maxQueryLen     = 100
	maxCardImages   = 20
	defaultDeck     = "My Words"
)

// Looker finds the entries of words.
type Looker interface {
	Lookup(ctx context.Context, d lookup.Dictionary, word string) (*lookup.Word, error)
	LookupMany(ctx context.Context, d lookup.Dictionary, words []string) (found []*lookup.Word, notFound []string, err error)
}

// Store keeps the saved words (the store package).
type Store interface {
	SaveWord(ctx context.Context, userID string, w store.SavedWord, max int) (int, error)
	CountPending(ctx context.Context, userID string) (total, fresh int, err error)
	ListPending(ctx context.Context, userID string, afterCreated time.Time, afterWord, query string, limit int) ([]store.SavedWord, error)
	Pending(ctx context.Context, userID string, scope store.Scope) ([]store.SavedWord, error)
	DeletePending(ctx context.Context, userID, word string) error
	MarkExported(ctx context.Context, userID string, words []string, remove bool) (int64, error)
	RemoveExported(ctx context.Context, userID string) (int64, error)
	PutDeckLink(ctx context.Context, userID string, tokenHash []byte, l store.DeckLink) error
	TakeDeckLink(ctx context.Context, tokenHash []byte, now time.Time) (*store.DeckLink, error)
	Settings(ctx context.Context) (map[string]json.RawMessage, error)
	PutSettings(ctx context.Context, rows map[string]json.RawMessage) error
	ImageBytes(ctx context.Context, userID string) (int64, error)
	ImageBySHA(ctx context.Context, userID string, sha []byte) (*store.Image, error)
	GetImage(ctx context.Context, userID, id string) (*store.Image, error)
	ImagesByIDs(ctx context.Context, userID string, ids []string) ([]store.Image, error)
	InsertImage(ctx context.Context, im store.Image) (existing *store.Image, inserted bool, err error)
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
}

// Service is the gRPC implementation.
type Service struct {
	dictionaryv1.UnimplementedDictionaryServiceServer

	looker Looker
	store  Store
	decks  Decks
	cfg    Config
	prefs  Prefs

	files    *imagestore.Store
	imgOpts  imagestore.Options
	imgQuota int64
}

// WithImages turns the images of the cards on: the files go to files, every image is normalized with opts and a
// user may keep quota bytes. Without it UploadImage fails with IMAGES_DISABLED.
func (s *Service) WithImages(files *imagestore.Store, opts imagestore.Options, quota int64) *Service {
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
func New(looker Looker, st Store, decks Decks, cfg Config) *Service {
	if cfg.DeckLinkTTL <= 0 {
		cfg.DeckLinkTTL = 15 * time.Minute
	}
	return &Service{looker: looker, store: st, decks: decks, cfg: cfg}
}

// userID is the user of the request: the subject of the token, never a field of the request. A service
// token (client credentials) has no user and is refused.
func userID(ctx context.Context) (string, error) {
	p := authn.FromContext(ctx)
	if p == nil || p.Subject == "" {
		return "", apperror.Unauthenticated("a user token is required")
	}
	if p.IsService() {
		return "", apperror.PermissionDenied("a user token is required, not a service token")
	}
	if _, err := uuid.Parse(p.Subject); err != nil {
		return "", apperror.Unauthenticated("a user token is required")
	}
	return p.Subject, nil
}

func dictionaryOf(d dictionaryv1.Dictionary) (lookup.Dictionary, error) {
	switch d {
	case dictionaryv1.Dictionary_DICTIONARY_UNSPECIFIED, dictionaryv1.Dictionary_DICTIONARY_ENGLISH:
		return lookup.English, nil
	case dictionaryv1.Dictionary_DICTIONARY_ENGLISH_VIETNAMESE:
		return lookup.EnglishVietnamese, nil
	}
	return "", apperror.InvalidArgument("unknown dictionary")
}

// lookupErr maps the errors of a lookup. Details of a provider failure are logged, not sent.
func lookupErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, lookup.ErrInvalidWord):
		return apperror.InvalidArgument("not a valid word: letters, digits, spaces, apostrophes, hyphens and dots, at most 64 characters")
	case errors.Is(err, lookup.ErrNotFound):
		return apperror.NotFound("word not found")
	case errors.Is(err, lookup.ErrUnavailable):
		logging.FromCtx(ctx).Warn("dictionary provider failed", zap.Error(err))
		return apperror.Unavailable("the dictionary is not available, try again later")
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
	d, err := dictionaryOf(req.GetDictionary())
	if err != nil {
		return nil, err
	}
	if err := s.requireDictionary(ctx, d); err != nil {
		return nil, err
	}
	w, err := s.looker.Lookup(ctx, d, req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	return &dictionaryv1.LookupWordResponse{Word: toProtoWord(w)}, nil
}

// LookupWords returns the entries of several words; the words without an entry are listed apart.
func (s *Service) LookupWords(ctx context.Context, req *dictionaryv1.LookupWordsRequest) (*dictionaryv1.LookupWordsResponse, error) {
	d, err := dictionaryOf(req.GetDictionary())
	if err != nil {
		return nil, err
	}
	if n := len(req.GetWords()); n == 0 || n > maxLookupWords {
		return nil, apperror.InvalidArgument("send between 1 and 20 words")
	}
	if err := s.requireDictionary(ctx, d); err != nil {
		return nil, err
	}
	found, notFound, err := s.looker.LookupMany(ctx, d, req.GetWords())
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
	word, err := lookup.NormalizeWord(req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	if len(req.GetContent()) > maxContentBytes {
		return nil, apperror.InvalidArgument("content is larger than 64 KiB")
	}
	for _, u := range []string{req.GetAudioUk(), req.GetAudioUs()} {
		if u != "" && (len(u) > maxURLBytes || !anki.AllowedURL(u, s.cfg.AudioHosts)) {
			return nil, apperror.InvalidArgument("audio must be an https URL on an allowed host")
		}
	}
	if ids := cardref.IDs(req.GetContent()); len(ids) > maxCardImages {
		return nil, apperror.InvalidArgument("a card can have at most 20 images")
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.store.SaveWord(ctx, user, store.SavedWord{
		Word: word, Content: req.GetContent(), AudioUK: req.GetAudioUk(), AudioUS: req.GetAudioUs(),
	}, sys.MaxSavedWords)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrLimit):
		return nil, apperror.Wrap(codes.ResourceExhausted, CodeMaxSavedWords+": maximum number of saved words reached, export or delete some first", nil)
	case errors.Is(err, store.ErrUnknownImage):
		return nil, apperror.InvalidArgument(CodeImageUnknown + ": the card refers to an image that does not exist")
	case errors.Is(err, store.ErrUnknownUser):
		return nil, apperror.Unauthenticated("unknown user")
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
		return nil, apperror.InvalidArgument("query is too long")
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
			return nil, apperror.InvalidArgument("invalid page token")
		}
		afterWord = parts[1]
	}
	rows, err := s.store.ListPending(ctx, user, afterCreated, afterWord, query, size+1)
	if err != nil {
		return nil, internal(ctx, "list saved words", err)
	}
	total, fresh, err := s.store.CountPending(ctx, user)
	if err != nil {
		return nil, internal(ctx, "count saved words", err)
	}
	resp := &dictionaryv1.ListSavedWordsResponse{Total: int32(total), NewTotal: int32(fresh)}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		resp.NextPageToken = pagination.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.Word)
	}
	for _, r := range rows {
		resp.Words = append(resp.Words, &dictionaryv1.SavedWord{
			Word: r.Word, Content: r.Content, AudioUk: r.AudioUK, AudioUs: r.AudioUS,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), Exported: r.Exported,
		})
	}
	return resp, nil
}

// DeleteSavedWord removes a word of the list of the user.
func (s *Service) DeleteSavedWord(ctx context.Context, req *dictionaryv1.DeleteSavedWordRequest) (*dictionaryv1.DeleteSavedWordResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	word, err := lookup.NormalizeWord(req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	switch err := s.store.DeletePending(ctx, user, word); {
	case err == nil:
		return &dictionaryv1.DeleteSavedWordResponse{}, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("saved word not found")
	default:
		return nil, internal(ctx, "delete saved word", err)
	}
}

// builtDeck is a deck made of the words of a user.
type builtDeck struct {
	name  string
	data  []byte
	words []string
}

// buildDeck packs the new words of the user (or the whole list, see scope) in a deck. A deck that could not
// be built or is too large is an error and leaves the words as they were.
func (s *Service) buildDeck(ctx context.Context, user, rawName string, scope dictionaryv1.ExportScope) (*builtDeck, error) {
	name, err := deckName(rawName)
	if err != nil {
		return nil, err
	}
	words, err := s.store.Pending(ctx, user, storeScope(scope))
	if err != nil {
		return nil, internal(ctx, "read pending words", err)
	}
	if len(words) == 0 {
		return nil, apperror.InvalidArgument(CodeNoWords + ": no saved words to export")
	}
	cards := make([]anki.Word, len(words))
	keys := make([]string, len(words))
	for i, w := range words {
		cards[i] = anki.Word{Word: w.Word, Content: w.Content, AudioUK: w.AudioUK, AudioUS: w.AudioUS}
		keys[i] = w.Word
	}
	if err := s.attachImages(ctx, user, cards); err != nil {
		return nil, err
	}
	data, err := s.decks.Build(ctx, name, cards, s.userPrefs(ctx).FreshDeck)
	switch {
	case err == nil:
	case errors.Is(err, anki.ErrTooLarge):
		return nil, apperror.FailedPrecondition(CodeExportTooBig + ": the deck is too large, delete some words and export again")
	case ctx.Err() != nil:
		return nil, ctx.Err()
	default:
		return nil, internal(ctx, "build deck", err)
	}
	return &builtDeck{name: name, data: data, words: keys}, nil
}

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
	sys, err := s.requireExport(ctx, false)
	if err != nil {
		return nil, err
	}
	d, err := s.buildDeck(ctx, user, req.GetDeckName(), req.GetScope())
	if err != nil {
		return nil, err
	}
	if _, err := s.store.MarkExported(ctx, user, d.words, s.removesAfterExport(ctx, sys)); err != nil {
		return nil, internal(ctx, "mark exported", err)
	}
	return &dictionaryv1.ExportAnkiResponse{Filename: fileName(d.name), Data: d.data, WordCount: int32(len(d.words))}, nil
}

// RemoveExportedWords takes the exported words out of the list; the new ones stay.
func (s *Service) RemoveExportedWords(ctx context.Context, _ *dictionaryv1.RemoveExportedWordsRequest) (*dictionaryv1.RemoveExportedWordsResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.store.RemoveExported(ctx, user)
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
	sys, err := s.requireExport(ctx, true)
	if err != nil {
		return nil, err
	}
	d, err := s.buildDeck(ctx, user, req.GetDeckName(), req.GetScope())
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
	if _, err := s.store.MarkExported(ctx, user, d.words, s.removesAfterExport(ctx, sys)); err != nil {
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
		return nil, apperror.PermissionDenied("a service token is required")
	}
	token := req.GetToken()
	if len(token) < 20 || len(token) > 128 {
		return nil, apperror.NotFound("the download link does not exist or has expired")
	}
	l, err := s.store.TakeDeckLink(ctx, tokenHash(token), time.Now())
	switch {
	case err == nil:
		return &dictionaryv1.DownloadDeckResponse{Filename: l.Filename, Data: l.Data}, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("the download link does not exist or has expired")
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
		return "", apperror.InvalidArgument("deck name is longer than 64 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", apperror.InvalidArgument("deck name has control characters")
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
		return nil, apperror.FailedPrecondition(CodeImagesDisabled + ": images are not enabled")
	}
	if len(req.GetData()) == 0 {
		return nil, apperror.InvalidArgument(CodeImageInvalid + ": no image")
	}
	res, err := imagestore.Normalize(req.GetData(), s.imgOpts)
	switch {
	case err == nil:
	case errors.Is(err, imagestore.ErrFormat):
		return nil, apperror.InvalidArgument(CodeImageInvalid + ": the file is not a png, jpeg, gif or webp image")
	case errors.Is(err, imagestore.ErrPixels), errors.Is(err, imagestore.ErrTooLarge):
		return nil, apperror.InvalidArgument(CodeImageTooLarge + ": the image is too large")
	default:
		return nil, internal(ctx, "normalize image", err)
	}
	sha := imagestore.Hash(res.Data)
	// the same picture of the same user is stored once
	switch ex, err := s.store.ImageBySHA(ctx, user, sha); {
	case err == nil:
		return &dictionaryv1.UploadImageResponse{Id: ex.ID}, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, internal(ctx, "find image", err)
	}
	used, err := s.store.ImageBytes(ctx, user)
	if err != nil {
		return nil, internal(ctx, "image quota", err)
	}
	if used+int64(len(res.Data)) > s.imgQuota {
		return nil, apperror.Wrap(codes.ResourceExhausted, CodeImageQuota+": the image storage of the user is full, remove images from cards", nil)
	}
	im := store.Image{ID: uuid.NewString(), UserID: user, ContentType: res.ContentType, Size: len(res.Data), SHA256: sha}
	ext := imagestore.Ext(res.ContentType)
	if err := s.files.Write(user, im.ID, ext, res.Data); err != nil {
		return nil, internal(ctx, "write image", err)
	}
	ex, inserted, err := s.store.InsertImage(ctx, im)
	switch {
	case err != nil:
		_ = s.files.Remove(user, im.ID, ext)
		if errors.Is(err, store.ErrUnknownUser) {
			return nil, apperror.Unauthenticated("unknown user")
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
		return nil, apperror.FailedPrecondition(CodeImagesDisabled + ": images are not enabled")
	}
	notFound := apperror.NotFound("image not found")
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
	data, err := s.files.Read(user, im.ID, imagestore.Ext(im.ContentType))
	switch {
	case err == nil:
	case errors.Is(err, imagestore.ErrNotFound):
		return nil, notFound
	default:
		return nil, internal(ctx, "read image", err)
	}
	return &dictionaryv1.GetImageResponse{ContentType: im.ContentType, Data: data}, nil
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
		ext := imagestore.Ext(im.ContentType)
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
