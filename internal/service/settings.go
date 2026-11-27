package service

import (
	"context"
	"strings"
	"time"

	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/logging"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

// system reads the settings of the system: the rows of the table over the defaults of the configuration.
func (s *Service) system(ctx context.Context) (settings.System, error) {
	rows, err := s.store.Settings(ctx)
	if err != nil {
		return settings.System{}, internal(ctx, "read settings", err)
	}
	return settings.FromRows(rows, s.systemDefaults()), nil
}

// systemDefaults are the settings of a system nobody changed: the configuration over the defaults of the package.
func (s *Service) systemDefaults() settings.System {
	def := settings.Defaults(s.cfg.MaxSavedWords, s.cfg.ExportedRetentionHours)
	pick := func(v, fallback int) int {
		if v > 0 {
			return v
		}
		return fallback
	}
	defaults := map[string]bool{}
	for _, e := range s.registry.Entries() {
		defaults[e.ID] = e.DefaultOn
	}
	return def.WithDictionaries(s.registry.IDs(), defaults).WithExportLimits(
		pick(s.cfg.ExportMaxBytes, def.ExportMaxBytes), pick(s.cfg.ExportPartMaxBytes, def.ExportPartMaxBytes),
		pick(s.cfg.ExportMaxBytesCeiling, def.ExportMaxBytesCeiling), pick(s.cfg.ExportPartMaxBytesCeiling, def.ExportPartMaxBytesCeiling),
	).WithExportSyncMaxWords(pick(s.cfg.ExportSyncMax, def.ExportSyncMaxWords)).WithStorageLimits(s.storageLimits())
}

// storageLimits are the configured defaults and ceilings of the size of the content and of the quotas; the quotas of
// the images and of the audio files default to the ones the service was given for them.
func (s *Service) storageLimits() settings.StorageLimits {
	l := s.cfg.Storage
	if l.ImageUserQuota == 0 {
		l.ImageUserQuota = s.imgQuota
	}
	if l.AudioUserQuota == 0 {
		l.AudioUserQuota = s.audioQuota
	}
	return l
}

func (s *Service) requireDictionary(ctx context.Context, id string) error {
	sys, err := s.system(ctx)
	if err != nil {
		return err
	}
	if !sys.Enabled(id) {
		return apperror.Wrap(codes.FailedPrecondition, CodeDictionaryDisabled+": this dictionary is turned off", nil).WithReason("dictionary_disabled")
	}
	return nil
}

// requireExport checks the export is allowed and returns the settings of the system, which the export needs
// for remove after export.
func (s *Service) requireExport(ctx context.Context, link bool) (settings.System, error) {
	sys, err := s.system(ctx)
	if err != nil {
		return sys, err
	}
	if (link && !sys.AllowExportLink) || (!link && !sys.AllowExportDeck) {
		return sys, apperror.Wrap(codes.FailedPrecondition, CodeFeatureDisabled+": this export is turned off", nil).WithReason("feature_disabled")
	}
	return sys, nil
}

// requireGroupAllowed checks that a word can be saved in the group: at least one of its dictionaries is on for the
// system and for the user (a user who turned one off still saves the words of the others, and the card then holds
// the parts that were looked up).
func (s *Service) requireGroupAllowed(ctx context.Context, sys settings.System, g lookup.Group) error {
	prefs := s.userPrefs(ctx)
	for _, e := range g.Entries {
		if prefs.Allows(sys, e.ID) {
			return nil
		}
	}
	return apperror.Wrap(codes.FailedPrecondition, CodeDictionaryDisabled+": the dictionaries of this group are turned off", nil).WithReason("dictionary_disabled")
}

// ListDictionaries lists the dictionaries of the registry by group, with whether each is on for the user.
func (s *Service) ListDictionaries(ctx context.Context, _ *dictionaryv1.ListDictionariesRequest) (*dictionaryv1.ListDictionariesResponse, error) {
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	prefs := s.userPrefs(ctx)
	out := &dictionaryv1.ListDictionariesResponse{}
	for _, g := range s.registry.Groups() {
		pg := &dictionaryv1.DictionaryGroup{Id: g.ID}
		for _, e := range g.Entries {
			pg.Dictionaries = append(pg.Dictionaries, &dictionaryv1.DictionaryInfo{
				Id: e.ID, Group: e.Group, Source: e.Source, Target: e.Target, Enabled: prefs.Allows(sys, e.ID),
			})
		}
		out.Groups = append(out.Groups, pg)
	}
	return out, nil
}

func toProtoSystem(s settings.System) *dictionaryv1.Settings {
	return &dictionaryv1.Settings{
		MaxSavedWords: int32(s.MaxSavedWords), AllowExportDeck: s.AllowExportDeck, AllowExportLink: s.AllowExportLink,
		Dictionaries:      s.Dictionaries,
		RemoveAfterExport: s.RemoveAfterExport, ExportedRetentionHours: int32(s.ExportedRetentionHours),
		MaxCategories: int32(s.MaxCategories), MaxCategoryNameLength: int32(s.MaxCategoryNameLength),
		ExportMaxBytes: int32(s.ExportMaxBytes), ExportPartMaxBytes: int32(s.ExportPartMaxBytes),
		ExportMaxBytesCeiling: int32(s.ExportMaxBytesCeiling), ExportPartMaxBytesCeiling: int32(s.ExportPartMaxBytesCeiling),
		ExportSyncMaxWords: int32(s.ExportSyncMaxWords),
		MaxContentBytes:    int32(s.MaxContentBytes), ContentUserQuota: s.ContentUserQuota, ImageUserQuota: s.ImageUserQuota, AudioUserQuota: s.AudioUserQuota,
		MaxContentBytesCeiling: int32(s.MaxContentBytesCeiling), ContentUserQuotaCeiling: s.ContentUserQuotaCeiling,
		ImageUserQuotaCeiling: s.ImageUserQuotaCeiling, AudioUserQuotaCeiling: s.AudioUserQuotaCeiling,
	}
}

func toProtoUser(u settings.User) *dictionaryv1.UserSettings {
	return &dictionaryv1.UserSettings{
		Dictionaries: u.Dictionaries, DefaultDictionary: u.DefaultDictionary,
		DeckName: u.DeckName, MaxLookupWords: int32(u.MaxLookupWords),
		RemoveAfterExport: u.RemoveAfterExport, RemoveAcknowledged: u.RemoveAcknowledged, FreshDeck: u.FreshDeck,
		ReviewNewPerDay: int32(u.ReviewNewPerDay), ReviewMaxPerDay: int32(u.ReviewMaxPerDay),
		ReviewAlgorithm: u.ReviewAlgorithm, DesiredRetention: int32(u.DesiredRetention),
	}
}

// GetSettings returns the settings of the system.
func (s *Service) GetSettings(ctx context.Context, _ *dictionaryv1.GetSettingsRequest) (*dictionaryv1.GetSettingsResponse, error) {
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	return &dictionaryv1.GetSettingsResponse{Settings: toProtoSystem(sys)}, nil
}

// UpdateSettings replaces the settings of the system; only the values that changed are written. Words a user
// saved above a lowered maximum stay: the maximum only stops a new save.
func (s *Service) UpdateSettings(ctx context.Context, req *dictionaryv1.UpdateSettingsRequest) (*dictionaryv1.UpdateSettingsResponse, error) {
	in := req.GetSettings()
	if in == nil {
		return nil, apperror.InvalidArgument("settings are required").WithReason("settings_required")
	}
	cur, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	next := settings.System{
		MaxSavedWords: int(in.GetMaxSavedWords()), AllowExportDeck: in.GetAllowExportDeck(), AllowExportLink: in.GetAllowExportLink(),
		RemoveAfterExport: in.GetRemoveAfterExport(), ExportedRetentionHours: int(in.GetExportedRetentionHours()),
		MaxCategories: int(in.GetMaxCategories()), MaxCategoryNameLength: int(in.GetMaxCategoryNameLength()),
		ExportMaxBytes: int(in.GetExportMaxBytes()), ExportPartMaxBytes: int(in.GetExportPartMaxBytes()), ExportSyncMaxWords: int(in.GetExportSyncMaxWords()),
		ExportMaxBytesCeiling: cur.ExportMaxBytesCeiling, ExportPartMaxBytesCeiling: cur.ExportPartMaxBytesCeiling,
		MaxContentBytes: int(in.GetMaxContentBytes()), ContentUserQuota: in.GetContentUserQuota(),
		ImageUserQuota: in.GetImageUserQuota(), AudioUserQuota: in.GetAudioUserQuota(),
		MaxContentBytesCeiling: cur.MaxContentBytesCeiling, ContentUserQuotaCeiling: cur.ContentUserQuotaCeiling,
		ImageUserQuotaCeiling: cur.ImageUserQuotaCeiling, AudioUserQuotaCeiling: cur.AudioUserQuotaCeiling,
	}
	// a dictionary the request leaves out keeps its value; one the registry does not have is refused
	next.Dictionaries, next.DictionaryOrder = make(map[string]bool, len(cur.Dictionaries)), cur.DictionaryOrder
	for id, on := range cur.Dictionaries {
		next.Dictionaries[id] = on
	}
	for id, on := range in.GetDictionaries() {
		if _, ok := cur.Dictionaries[id]; !ok {
			return nil, apperror.InvalidArgument("unknown dictionary").WithReason("unknown_dictionary", "dictionary", id)
		}
		next.Dictionaries[id] = on
	}
	// A client older than these settings does not send them: the current values stay.
	if next.MaxCategories == 0 {
		next.MaxCategories = cur.MaxCategories
	}
	if next.MaxCategoryNameLength == 0 {
		next.MaxCategoryNameLength = cur.MaxCategoryNameLength
	}
	if next.ExportMaxBytes == 0 {
		next.ExportMaxBytes = cur.ExportMaxBytes
	}
	if next.ExportPartMaxBytes == 0 {
		next.ExportPartMaxBytes = cur.ExportPartMaxBytes
	}
	if next.ExportSyncMaxWords == 0 {
		next.ExportSyncMaxWords = cur.ExportSyncMaxWords
	}
	if next.MaxContentBytes == 0 {
		next.MaxContentBytes = cur.MaxContentBytes
	}
	if next.ContentUserQuota == 0 {
		next.ContentUserQuota = cur.ContentUserQuota
	}
	if next.ImageUserQuota == 0 {
		next.ImageUserQuota = cur.ImageUserQuota
	}
	if next.AudioUserQuota == 0 {
		next.AudioUserQuota = cur.AudioUserQuota
	}
	if err := next.Validate(); err != nil {
		return nil, apperror.InvalidArgument(err.Error())
	}
	if err := s.store.PutSettings(ctx, settings.Changed(cur, next)); err != nil {
		return nil, internal(ctx, "write settings", err)
	}
	return &dictionaryv1.UpdateSettingsResponse{Settings: toProtoSystem(next)}, nil
}

// userPrefs reads the preferences of the user of the request. identity being down must not take the lookups
// down with it: the defaults answer.
func (s *Service) userPrefs(ctx context.Context) settings.User {
	if s.prefs == nil {
		return settings.UserDefaults()
	}
	raw, err := s.prefs.Get(ctx)
	if err != nil {
		logging.FromCtx(ctx).Warn("user settings unavailable, using the defaults", zap.Error(err))
		return settings.UserDefaults()
	}
	return settings.ParseUser(raw)
}

// GetMySettings returns the preferences of the user next to the settings of the system.
func (s *Service) GetMySettings(ctx context.Context, _ *dictionaryv1.GetMySettingsRequest) (*dictionaryv1.GetMySettingsResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	out := &dictionaryv1.GetMySettingsResponse{User: toProtoUser(s.userPrefs(ctx).Effective(sys)), System: toProtoSystem(sys)}
	if p, err := s.store.FSRSParams(ctx, user); err != nil {
		logging.FromCtx(ctx).Warn("read fsrs parameters", zap.Error(err))
	} else if p != nil {
		out.FsrsTrainedAt, out.FsrsReviewCount = p.TrainedAt.UTC().Format(time.RFC3339), int32(p.ReviewCount)
	}
	return out, nil
}

// UpdateMySettings stores the preferences of the user.
func (s *Service) UpdateMySettings(ctx context.Context, req *dictionaryv1.UpdateMySettingsRequest) (*dictionaryv1.UpdateMySettingsResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	in := req.GetUser()
	if in == nil {
		return nil, apperror.InvalidArgument("settings are required").WithReason("settings_required")
	}
	if s.prefs == nil {
		return nil, apperror.FailedPrecondition("settings of a user are not available").WithReason("user_settings_unavailable")
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	u := settings.User{
		Dictionaries: in.GetDictionaries(), DefaultDictionary: in.GetDefaultDictionary(), DeckName: in.GetDeckName(), MaxLookupWords: int(in.GetMaxLookupWords()),
		RemoveAfterExport: in.GetRemoveAfterExport(), RemoveAcknowledged: in.GetRemoveAcknowledged(), FreshDeck: in.GetFreshDeck(),
		ReviewNewPerDay: int(in.GetReviewNewPerDay()), ReviewMaxPerDay: int(in.GetReviewMaxPerDay()),
		ReviewAlgorithm: in.GetReviewAlgorithm(), DesiredRetention: int(in.GetDesiredRetention()),
	}
	// a client that does not know the fields sends none: empty is SM-2 and 0 is 90%
	if u.ReviewAlgorithm == "" {
		u.ReviewAlgorithm = settings.AlgorithmSM2
	}
	if u.DesiredRetention == 0 {
		u.DesiredRetention = settings.DefaultDesiredRetention
	}
	if u.DeckName, err = deckNameOrEmpty(u.DeckName); err != nil {
		return nil, err
	}
	if err := u.Validate(sys); err != nil {
		return nil, apperror.InvalidArgument(err.Error())
	}
	if err := s.prefs.Put(ctx, u.JSON()); err != nil {
		logging.FromCtx(ctx).Warn("store user settings", zap.Error(err))
		return nil, apperror.Unavailable("settings could not be saved, try again later").WithReason("settings_save_failed")
	}
	s.sched.forget(user)
	return &dictionaryv1.UpdateMySettingsResponse{User: toProtoUser(u), System: toProtoSystem(sys)}, nil
}

// deckNameOrEmpty checks a default deck name; blank stays blank (the export falls back to the built-in name).
func deckNameOrEmpty(name string) (string, error) {
	if trimmed := strings.TrimSpace(name); trimmed == "" {
		return "", nil
	}
	return deckName(name)
}
