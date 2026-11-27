// Package settings holds the settings of the dictionary: the ones of the whole system (rows of
// dictionary_settings, edited by an administrator) and the preferences of a user (a JSON object that
// identity keeps for the application "dictionary"). Both are typed here with their defaults and checks;
// a key that is missing takes its default, so a new setting needs no migration.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// App is the name under which identity keeps the preferences of a user.
const App = "dictionary"

// Limits.
const (
	MaxSavedWordsLimit = 1_000_000
	MaxLookupLimit     = 20
	// MaxReviewNewPerDay is the most new words a user can ask the review to show per day.
	MaxReviewNewPerDay = 100
	// DefaultReviewNewPerDay is the number of new words per day of a user who never chose.
	DefaultReviewNewPerDay = 10
	// MaxReviewMaxPerDay is the most review cards (due and learning) a user can ask the review to show per day.
	MaxReviewMaxPerDay = 9999
	// DefaultReviewMaxPerDay is the number of review cards per day of a user who never chose.
	DefaultReviewMaxPerDay = 200
	// The algorithms that schedule the review (the setting is "sm2" or "fsrs").
	AlgorithmSM2  = "sm2"
	AlgorithmFSRS = "fsrs"
	// MinDesiredRetention, MaxDesiredRetention and DefaultDesiredRetention are the chance (percent) to remember a
	// card on the day it is due that FSRS aims for.
	MinDesiredRetention     = 70
	MaxDesiredRetention     = 97
	DefaultDesiredRetention = 90
	// MaxRetentionHours is one year.
	MaxRetentionHours = 8760
	// MaxCategoriesLimit and MaxCategoryNameLimit bound what an administrator can set for the categories.
	MaxCategoriesLimit   = 200
	MaxCategoryNameLimit = 100
	// DefaultMaxCategories and DefaultMaxCategoryName apply until an administrator chooses.
	DefaultMaxCategories   = 50
	DefaultMaxCategoryName = 40
	// MinExportBytes is the smallest size limit of an export an administrator can set.
	MinExportBytes = 64 << 10
	// The size limits of an export until the configuration or an administrator says otherwise: a synchronous
	// export (a deck in one gRPC message), a part of an export job, and the ceilings an administrator cannot pass.
	DefaultExportMaxBytes        = 3 << 20
	DefaultExportPartMaxBytes    = 3 << 20
	DefaultExportMaxBytesCeiling = 10 << 20
	DefaultExportPartCeiling     = 50 << 20
	// The most words a synchronous export packs: the smallest and the largest value an administrator can set, and
	// the default until the configuration or an administrator says otherwise.
	// The size of the content of one word, in bytes: the smallest and the default an administrator can set, and the
	// ceiling until the configuration says otherwise.
	MinContentBytes          = 1 << 10
	DefaultMaxContentBytes   = 64 << 10
	DefaultMaxContentCeiling = 256 << 10
	// The quotas of one user (the content of the words of the list, the images, the audio files), in bytes: the
	// smallest an administrator can set, and the defaults and ceilings until the configuration says otherwise.
	MinUserQuota                = 1 << 10
	DefaultContentUserQuota     = 256 << 20
	DefaultContentUserQuotaCeil = 1 << 30
	DefaultImageUserQuota       = 20 << 20
	DefaultImageUserQuotaCeil   = 1 << 30
	DefaultAudioUserQuota       = 10 << 20
	DefaultAudioUserQuotaCeil   = 1 << 30
	MinExportSyncWords          = 1
	MaxExportSyncWords          = 5000
	DefaultExportSyncWords      = 2000
)

// Keys of the rows of dictionary_settings.
const (
	keyMaxSaved   = "max_saved_words"
	keyExportDeck = "allow_export_deck"
	keyExportLink = "allow_export_link"
	// keyDictionaries holds one JSON object {dictionary id: enabled}; an id the registry has and the object lacks
	// takes the default of the registry.
	keyDictionaries = "dictionaries"

	keyRemoveAfterExport = "remove_after_export"
	keyRetentionHours    = "exported_retention_hours"

	keyMaxCategories   = "max_categories"
	keyMaxCategoryName = "max_category_name_length"

	keyExportMaxBytes     = "export_max_bytes"
	keyExportPartMaxBytes = "export_part_max_bytes"
	keyExportSyncWords    = "export_sync_max_words"

	keyMaxContentBytes  = "max_content_bytes"
	keyContentUserQuota = "content_user_quota"
	keyImageUserQuota   = "image_user_quota"
	keyAudioUserQuota   = "audio_user_quota"
)

// System is the settings of the whole system.
type System struct {
	MaxSavedWords   int
	AllowExportDeck bool
	AllowExportLink bool
	// Dictionaries tells for every dictionary of the registry whether it can be looked up; DictionaryOrder is the
	// registry order (the first one that is on is the fallback of a default dictionary).
	Dictionaries    map[string]bool
	DictionaryOrder []string
	// RemoveAfterExport: an export takes the words out of the list (a user can turn it off for themself).
	RemoveAfterExport bool
	// ExportedRetentionHours is how long a word that left the list is kept before the cleanup deletes it.
	ExportedRetentionHours int
	// MaxCategories is the most categories a user can have; MaxCategoryNameLength the longest name (in characters).
	// They only stop a new category or a rename: what exists stays.
	MaxCategories         int
	MaxCategoryNameLength int
	// ExportMaxBytes is the largest deck of a synchronous export (ExportAnki, CreateDeckLink), ExportPartMaxBytes the
	// largest part of an export job.
	ExportMaxBytes     int
	ExportPartMaxBytes int
	// ExportSyncMaxWords is the most words a synchronous export packs; a longer list is an export job.
	ExportSyncMaxWords int
	// The ceilings are the configuration of the deployment, not rows: the most an administrator can set.
	ExportMaxBytesCeiling     int
	ExportPartMaxBytesCeiling int
	// MaxContentBytes is the largest content of one word. ContentUserQuota, ImageUserQuota and AudioUserQuota are
	// what one user may keep of each kind (a user can have an own quota, see the store); the ceilings are the
	// configuration of the deployment: the most an administrator can set.
	MaxContentBytes                                                       int
	ContentUserQuota, ImageUserQuota, AudioUserQuota                      int64
	MaxContentBytesCeiling                                                int
	ContentUserQuotaCeiling, ImageUserQuotaCeiling, AudioUserQuotaCeiling int64
}

// Defaults are the settings of a system nobody changed: everything on.
func Defaults(maxSaved, retentionHours int) System {
	return System{
		MaxSavedWords: maxSaved, AllowExportDeck: true, AllowExportLink: true,
		RemoveAfterExport: true, ExportedRetentionHours: retentionHours,
		MaxCategories: DefaultMaxCategories, MaxCategoryNameLength: DefaultMaxCategoryName,
		ExportMaxBytes: DefaultExportMaxBytes, ExportPartMaxBytes: DefaultExportPartMaxBytes, ExportSyncMaxWords: DefaultExportSyncWords,
		ExportMaxBytesCeiling: DefaultExportMaxBytesCeiling, ExportPartMaxBytesCeiling: DefaultExportPartCeiling,
		MaxContentBytes: DefaultMaxContentBytes, ContentUserQuota: DefaultContentUserQuota,
		ImageUserQuota: DefaultImageUserQuota, AudioUserQuota: DefaultAudioUserQuota,
		MaxContentBytesCeiling: DefaultMaxContentCeiling, ContentUserQuotaCeiling: DefaultContentUserQuotaCeil,
		ImageUserQuotaCeiling: DefaultImageUserQuotaCeil, AudioUserQuotaCeiling: DefaultAudioUserQuotaCeil,
	}
}

// StorageLimits are the size limits of the content, the images and the audio files: the defaults of the
// settings and their ceilings, from the configuration. A zero value keeps the default of this package.
type StorageLimits struct {
	MaxContentBytes, MaxContentBytesCeiling   int
	ContentUserQuota, ContentUserQuotaCeiling int64
	ImageUserQuota, ImageUserQuotaCeiling     int64
	AudioUserQuota, AudioUserQuotaCeiling     int64
}

// WithStorageLimits sets the limits of the content, the images and the audio files from the configuration.
func (s System) WithStorageLimits(l StorageLimits) System {
	if l.MaxContentBytes > 0 {
		s.MaxContentBytes = l.MaxContentBytes
	}
	if l.MaxContentBytesCeiling > 0 {
		s.MaxContentBytesCeiling = l.MaxContentBytesCeiling
	}
	for _, c := range []struct {
		dst  *int64
		from int64
	}{
		{&s.ContentUserQuota, l.ContentUserQuota}, {&s.ContentUserQuotaCeiling, l.ContentUserQuotaCeiling},
		{&s.ImageUserQuota, l.ImageUserQuota}, {&s.ImageUserQuotaCeiling, l.ImageUserQuotaCeiling},
		{&s.AudioUserQuota, l.AudioUserQuota}, {&s.AudioUserQuotaCeiling, l.AudioUserQuotaCeiling},
	} {
		if c.from > 0 {
			*c.dst = c.from
		}
	}
	return s
}

// WithExportLimits sets the size limits of the exports (the defaults of the settings and their ceilings) from
// the configuration.
func (s System) WithExportLimits(maxBytes, partMaxBytes, maxBytesCeiling, partMaxBytesCeiling int) System {
	s.ExportMaxBytes, s.ExportPartMaxBytes = maxBytes, partMaxBytes
	s.ExportMaxBytesCeiling, s.ExportPartMaxBytesCeiling = maxBytesCeiling, partMaxBytesCeiling
	return s
}

// WithExportSyncMaxWords sets the default of the most words of a synchronous export from the configuration.
func (s System) WithExportSyncMaxWords(n int) System {
	s.ExportSyncMaxWords = n
	return s
}

// WithDictionaries sets the dictionaries of the registry: their order and whether each is on until somebody
// chooses. Call it before FromRows.
func (s System) WithDictionaries(order []string, defaults map[string]bool) System {
	s.DictionaryOrder = append([]string(nil), order...)
	s.Dictionaries = make(map[string]bool, len(order))
	for _, id := range order {
		s.Dictionaries[id] = defaults[id]
	}
	return s
}

// Enabled reports whether a dictionary of the registry can be looked up; false for an unknown id.
func (s System) Enabled(dictionary string) bool { return s.Dictionaries[dictionary] }

// known reports whether the id is in the registry the settings were built for.
func (s System) known(dictionary string) bool {
	for _, id := range s.DictionaryOrder {
		if id == dictionary {
			return true
		}
	}
	return false
}

// Validate checks the values an administrator sends.
func (s System) Validate() error {
	if s.MaxSavedWords < 1 || s.MaxSavedWords > MaxSavedWordsLimit {
		return fmt.Errorf("max saved words must be between 1 and %d", MaxSavedWordsLimit)
	}
	if s.ExportedRetentionHours < 1 || s.ExportedRetentionHours > MaxRetentionHours {
		return fmt.Errorf("retention must be between 1 and %d hours", MaxRetentionHours)
	}
	if s.MaxCategories < 1 || s.MaxCategories > MaxCategoriesLimit {
		return fmt.Errorf("max categories must be between 1 and %d", MaxCategoriesLimit)
	}
	if s.MaxCategoryNameLength < 1 || s.MaxCategoryNameLength > MaxCategoryNameLimit {
		return fmt.Errorf("max category name length must be between 1 and %d", MaxCategoryNameLimit)
	}
	if s.ExportMaxBytes < MinExportBytes || s.ExportMaxBytes > s.ExportMaxBytesCeiling {
		return fmt.Errorf("export size must be between %d and %d bytes", MinExportBytes, s.ExportMaxBytesCeiling)
	}
	if s.ExportPartMaxBytes < MinExportBytes || s.ExportPartMaxBytes > s.ExportPartMaxBytesCeiling {
		return fmt.Errorf("export part size must be between %d and %d bytes", MinExportBytes, s.ExportPartMaxBytesCeiling)
	}
	if s.MaxContentBytes < MinContentBytes || s.MaxContentBytes > s.MaxContentBytesCeiling {
		return fmt.Errorf("content size must be between %d and %d bytes", MinContentBytes, s.MaxContentBytesCeiling)
	}
	for _, q := range []struct {
		name       string
		v, ceiling int64
	}{
		{"content", s.ContentUserQuota, s.ContentUserQuotaCeiling}, {"image", s.ImageUserQuota, s.ImageUserQuotaCeiling},
		{"audio", s.AudioUserQuota, s.AudioUserQuotaCeiling},
	} {
		if q.v < MinUserQuota || q.v > q.ceiling {
			return fmt.Errorf("%s quota must be between %d and %d bytes", q.name, MinUserQuota, q.ceiling)
		}
	}
	if s.ExportSyncMaxWords < MinExportSyncWords || s.ExportSyncMaxWords > MaxExportSyncWords {
		return fmt.Errorf("words of a synchronous export must be between %d and %d", MinExportSyncWords, MaxExportSyncWords)
	}
	on := false
	for id, enabled := range s.Dictionaries {
		if !s.known(id) {
			return fmt.Errorf("unknown dictionary %q", id)
		}
		on = on || enabled
	}
	if !on {
		return errors.New("at least one dictionary must stay enabled")
	}
	return nil
}

// FromRows reads the rows of the table; a key without a row, a row of an unknown key or one with a value of
// the wrong type keeps the default.
func FromRows(rows map[string]json.RawMessage, def System) System {
	out := def
	if v, ok := rows[keyMaxSaved]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil && n > 0 {
			out.MaxSavedWords = n
		}
	}
	if v, ok := rows[keyRetentionHours]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil && n >= 1 && n <= MaxRetentionHours {
			out.ExportedRetentionHours = n
		}
	}
	// a stored size above a ceiling that was lowered since keeps the default
	for key, c := range map[string]struct {
		dst      *int
		min, max int
	}{
		keyMaxCategories:      {&out.MaxCategories, 1, MaxCategoriesLimit},
		keyMaxCategoryName:    {&out.MaxCategoryNameLength, 1, MaxCategoryNameLimit},
		keyExportMaxBytes:     {&out.ExportMaxBytes, MinExportBytes, out.ExportMaxBytesCeiling},
		keyExportPartMaxBytes: {&out.ExportPartMaxBytes, MinExportBytes, out.ExportPartMaxBytesCeiling},
		keyExportSyncWords:    {&out.ExportSyncMaxWords, MinExportSyncWords, MaxExportSyncWords},
		keyMaxContentBytes:    {&out.MaxContentBytes, MinContentBytes, out.MaxContentBytesCeiling},
	} {
		if v, ok := rows[key]; ok {
			var n int
			if json.Unmarshal(v, &n) == nil && n >= c.min && n <= c.max {
				*c.dst = n
			}
		}
	}
	for key, c := range map[string]struct {
		dst      *int64
		min, max int64
	}{
		keyContentUserQuota: {&out.ContentUserQuota, MinUserQuota, out.ContentUserQuotaCeiling},
		keyImageUserQuota:   {&out.ImageUserQuota, MinUserQuota, out.ImageUserQuotaCeiling},
		keyAudioUserQuota:   {&out.AudioUserQuota, MinUserQuota, out.AudioUserQuotaCeiling},
	} {
		if v, ok := rows[key]; ok {
			var n int64
			if json.Unmarshal(v, &n) == nil && n >= c.min && n <= c.max {
				*c.dst = n
			}
		}
	}
	for key, dst := range map[string]*bool{
		keyExportDeck: &out.AllowExportDeck, keyExportLink: &out.AllowExportLink,
		keyRemoveAfterExport: &out.RemoveAfterExport,
	} {
		if v, ok := rows[key]; ok {
			var b bool
			if json.Unmarshal(v, &b) == nil {
				*dst = b
			}
		}
	}
	if v, ok := rows[keyDictionaries]; ok {
		var m map[string]bool
		if json.Unmarshal(v, &m) == nil {
			// a copy: def is shared and its map must not change; an id the registry does not have (a dictionary that
			// was removed) is ignored
			out.Dictionaries = make(map[string]bool, len(def.Dictionaries))
			for id, on := range def.Dictionaries {
				if set, ok := m[id]; ok {
					on = set
				}
				out.Dictionaries[id] = on
			}
		}
	}
	return out
}

func (s System) rows() map[string]json.RawMessage {
	enc := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	return map[string]json.RawMessage{
		keyMaxSaved: enc(s.MaxSavedWords), keyExportDeck: enc(s.AllowExportDeck), keyExportLink: enc(s.AllowExportLink),
		keyDictionaries:      enc(s.Dictionaries),
		keyRemoveAfterExport: enc(s.RemoveAfterExport), keyRetentionHours: enc(s.ExportedRetentionHours),
		keyMaxCategories: enc(s.MaxCategories), keyMaxCategoryName: enc(s.MaxCategoryNameLength),
		keyExportMaxBytes: enc(s.ExportMaxBytes), keyExportPartMaxBytes: enc(s.ExportPartMaxBytes), keyExportSyncWords: enc(s.ExportSyncMaxWords),
		keyMaxContentBytes: enc(s.MaxContentBytes), keyContentUserQuota: enc(s.ContentUserQuota),
		keyImageUserQuota: enc(s.ImageUserQuota), keyAudioUserQuota: enc(s.AudioUserQuota),
	}
}

// Changed returns the rows that differ between the current settings and the new ones: only they are written,
// so two administrators changing different settings do not overwrite each other.
func Changed(current, next System) map[string]json.RawMessage {
	old, now := current.rows(), next.rows()
	out := map[string]json.RawMessage{}
	for k, v := range now {
		if string(old[k]) != string(v) {
			out[k] = v
		}
	}
	return out
}

// User is the preferences of one user. They only narrow what System allows.
type User struct {
	// Dictionaries is what the user turned on or off, by id; an id that is missing is on (the system decides which
	// dictionaries exist and which are enabled).
	Dictionaries map[string]bool
	// DefaultDictionary is the id selected first; empty until the user chose (the first dictionary that is on).
	DefaultDictionary string
	DeckName          string
	MaxLookupWords    int
	// RemoveAfterExport: an export takes the words out of the list (needs the system setting too).
	RemoveAfterExport bool
	// RemoveAcknowledged: remove after export is in effect and the user answered what to do with the words that
	// were exported earlier and are still in the list. It is cleared when the setting goes off, so the question
	// comes again the next time it is on.
	RemoveAcknowledged bool
	// FreshDeck: every export is a new deck for Anki, so importing it again adds the cards again. Off (the default):
	// the cards keep their identity and Anki updates them.
	FreshDeck bool
	// ReviewNewPerDay is how many words never reviewed the review shows per 24 hours (0: none).
	ReviewNewPerDay int
	// ReviewMaxPerDay is how many review cards (due and learning) the review shows per 24 hours (0: none).
	ReviewMaxPerDay int
	// ReviewAlgorithm is AlgorithmSM2 or AlgorithmFSRS.
	ReviewAlgorithm string
	// DesiredRetention (percent) is the aim of FSRS; SM-2 ignores it.
	DesiredRetention int
}

// UserDefaults are the preferences of a user who never chose: every dictionary on, the first one selected.
func UserDefaults() User {
	return User{MaxLookupWords: MaxLookupLimit, RemoveAfterExport: true,
		ReviewNewPerDay: DefaultReviewNewPerDay, ReviewMaxPerDay: DefaultReviewMaxPerDay,
		ReviewAlgorithm: AlgorithmSM2, DesiredRetention: DefaultDesiredRetention}
}

// userJSON is what is stored; a pointer tells a missing key from a false one.
type userJSON struct {
	Dictionaries      map[string]bool `json:"dictionaries,omitempty"`
	DefaultDictionary string          `json:"defaultDictionary,omitempty"`
	DeckName          string          `json:"deckName,omitempty"`
	MaxLookupWords    int             `json:"maxLookupWords,omitempty"`

	RemoveAfterExport  *bool `json:"removeAfterExport,omitempty"`
	RemoveAcknowledged bool  `json:"removeAcknowledged,omitempty"`
	FreshDeck          bool  `json:"freshDeck,omitempty"`

	ReviewNewPerDay *int `json:"reviewNewPerDay,omitempty"`
	ReviewMaxPerDay *int `json:"reviewMaxPerDay,omitempty"`

	ReviewAlgorithm  string `json:"reviewAlgorithm,omitempty"`
	DesiredRetention int    `json:"desiredRetention,omitempty"`
}

// ParseUser reads the stored object; anything missing or unreadable keeps the default.
func ParseUser(raw string) User {
	out := UserDefaults()
	var j userJSON
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &j) != nil {
		return out
	}
	out.Dictionaries = j.Dictionaries
	out.DefaultDictionary = j.DefaultDictionary
	if j.RemoveAfterExport != nil {
		out.RemoveAfterExport = *j.RemoveAfterExport
	}
	out.RemoveAcknowledged = j.RemoveAcknowledged
	out.FreshDeck = j.FreshDeck
	out.DeckName = j.DeckName
	if j.MaxLookupWords >= 1 && j.MaxLookupWords <= MaxLookupLimit {
		out.MaxLookupWords = j.MaxLookupWords
	}
	if j.ReviewNewPerDay != nil && *j.ReviewNewPerDay >= 0 && *j.ReviewNewPerDay <= MaxReviewNewPerDay {
		out.ReviewNewPerDay = *j.ReviewNewPerDay
	}
	if j.ReviewMaxPerDay != nil && *j.ReviewMaxPerDay >= 0 && *j.ReviewMaxPerDay <= MaxReviewMaxPerDay {
		out.ReviewMaxPerDay = *j.ReviewMaxPerDay
	}
	if j.ReviewAlgorithm == AlgorithmSM2 || j.ReviewAlgorithm == AlgorithmFSRS {
		out.ReviewAlgorithm = j.ReviewAlgorithm
	}
	if j.DesiredRetention >= MinDesiredRetention && j.DesiredRetention <= MaxDesiredRetention {
		out.DesiredRetention = j.DesiredRetention
	}
	return out
}

// JSON is the object to store.
func (u User) JSON() string {
	b, _ := json.Marshal(userJSON{
		Dictionaries:      u.Dictionaries,
		DefaultDictionary: u.DefaultDictionary, DeckName: u.DeckName, MaxLookupWords: u.MaxLookupWords,
		RemoveAfterExport: &u.RemoveAfterExport, RemoveAcknowledged: u.RemoveAcknowledged, FreshDeck: u.FreshDeck,
		ReviewNewPerDay: &u.ReviewNewPerDay, ReviewMaxPerDay: &u.ReviewMaxPerDay,
		ReviewAlgorithm: u.ReviewAlgorithm, DesiredRetention: u.DesiredRetention,
	})
	return string(b)
}

// Allows reports whether a dictionary is on for the user and for the system.
func (u User) Allows(sys System, dictionary string) bool {
	if !sys.Enabled(dictionary) {
		return false
	}
	on, chosen := u.Dictionaries[dictionary]
	return on || !chosen
}

// firstAllowed is the first dictionary of the registry order that is on for the user, "" when none is.
func (u User) firstAllowed(sys System) string {
	for _, id := range sys.DictionaryOrder {
		if u.Allows(sys, id) {
			return id
		}
	}
	return ""
}

// RemovesAfterExport reports whether an export takes the words out of the list: the system allows it and the
// user did not turn it off.
func (u User) RemovesAfterExport(sys System) bool {
	return u.RemoveAfterExport && sys.RemoveAfterExport
}

// Validate checks what a user sends against what the system allows: only dictionaries of the registry, one at
// least, and the default is one of the dictionaries that are on.
func (u User) Validate(sys System) error {
	if u.MaxLookupWords < 1 || u.MaxLookupWords > MaxLookupLimit {
		return fmt.Errorf("words per lookup must be between 1 and %d", MaxLookupLimit)
	}
	if u.ReviewNewPerDay < 0 || u.ReviewNewPerDay > MaxReviewNewPerDay {
		return fmt.Errorf("new words per day must be between 0 and %d", MaxReviewNewPerDay)
	}
	if u.ReviewMaxPerDay < 0 || u.ReviewMaxPerDay > MaxReviewMaxPerDay {
		return fmt.Errorf("reviews per day must be between 0 and %d", MaxReviewMaxPerDay)
	}
	if u.ReviewAlgorithm != AlgorithmSM2 && u.ReviewAlgorithm != AlgorithmFSRS {
		return errors.New("review algorithm must be sm2 or fsrs")
	}
	if u.DesiredRetention < MinDesiredRetention || u.DesiredRetention > MaxDesiredRetention {
		return fmt.Errorf("desired retention must be between %d and %d", MinDesiredRetention, MaxDesiredRetention)
	}
	for id := range u.Dictionaries {
		if !sys.known(id) {
			return fmt.Errorf("unknown dictionary %q", id)
		}
	}
	if u.firstAllowed(sys) == "" {
		return errors.New("at least one dictionary must stay enabled")
	}
	// empty: the user never chose, the first dictionary that is on is selected (see Effective)
	if u.DefaultDictionary != "" && !sys.known(u.DefaultDictionary) {
		return errors.New("unknown default dictionary")
	}
	if u.DefaultDictionary != "" && !u.Allows(sys, u.DefaultDictionary) {
		return errors.New("the default dictionary must be an enabled one")
	}
	return nil
}

// Effective returns the preferences as they work now: every dictionary of the registry with whether it is on for
// the user and the system, and a default that is on (a dictionary the system turned off since moves it to the
// first one that is on).
func (u User) Effective(sys System) User {
	on := make(map[string]bool, len(sys.DictionaryOrder))
	for _, id := range sys.DictionaryOrder {
		on[id] = u.Allows(sys, id)
	}
	if !u.Allows(sys, u.DefaultDictionary) {
		if first := u.firstAllowed(sys); first != "" {
			u.DefaultDictionary = first
		}
	}
	u.Dictionaries = on
	return u
}
