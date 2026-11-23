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
	MaxSavedWordsLimit = 1000
	MaxLookupLimit     = 20
)

// Keys of the rows of dictionary_settings.
const (
	keyMaxSaved   = "max_saved_words"
	keyExportDeck = "allow_export_deck"
	keyExportLink = "allow_export_link"
	keyEnglish    = "english_enabled"
	keyEnglishVi  = "english_vi_enabled"
)

// Dictionary names used in the preferences.
const (
	English           = "english"
	EnglishVietnamese = "english_vi"
)

// System is the settings of the whole system.
type System struct {
	MaxSavedWords    int
	AllowExportDeck  bool
	AllowExportLink  bool
	EnglishEnabled   bool
	EnglishViEnabled bool
}

// Defaults are the settings of a system nobody changed: everything on.
func Defaults(maxSaved int) System {
	return System{MaxSavedWords: maxSaved, AllowExportDeck: true, AllowExportLink: true, EnglishEnabled: true, EnglishViEnabled: true}
}

// Enabled reports whether a dictionary ("english", "english_vi") can be looked up.
func (s System) Enabled(dictionary string) bool {
	switch dictionary {
	case English:
		return s.EnglishEnabled
	case EnglishVietnamese:
		return s.EnglishViEnabled
	}
	return false
}

// Validate checks the values an administrator sends.
func (s System) Validate() error {
	if s.MaxSavedWords < 1 || s.MaxSavedWords > MaxSavedWordsLimit {
		return fmt.Errorf("max saved words must be between 1 and %d", MaxSavedWordsLimit)
	}
	if !s.EnglishEnabled && !s.EnglishViEnabled {
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
	for key, dst := range map[string]*bool{
		keyExportDeck: &out.AllowExportDeck, keyExportLink: &out.AllowExportLink,
		keyEnglish: &out.EnglishEnabled, keyEnglishVi: &out.EnglishViEnabled,
	} {
		if v, ok := rows[key]; ok {
			var b bool
			if json.Unmarshal(v, &b) == nil {
				*dst = b
			}
		}
	}
	return out
}

func (s System) rows() map[string]json.RawMessage {
	enc := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	return map[string]json.RawMessage{
		keyMaxSaved: enc(s.MaxSavedWords), keyExportDeck: enc(s.AllowExportDeck), keyExportLink: enc(s.AllowExportLink),
		keyEnglish: enc(s.EnglishEnabled), keyEnglishVi: enc(s.EnglishViEnabled),
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
	EnglishEnabled    bool
	EnglishViEnabled  bool
	DefaultDictionary string
	DeckName          string
	MaxLookupWords    int
}

// UserDefaults are the preferences of a user who never chose: everything on, English first.
func UserDefaults() User {
	return User{EnglishEnabled: true, EnglishViEnabled: true, DefaultDictionary: English, MaxLookupWords: MaxLookupLimit}
}

// userJSON is what is stored; a pointer tells a missing key from a false one.
type userJSON struct {
	EnglishEnabled    *bool  `json:"englishEnabled,omitempty"`
	EnglishViEnabled  *bool  `json:"englishViEnabled,omitempty"`
	DefaultDictionary string `json:"defaultDictionary,omitempty"`
	DeckName          string `json:"deckName,omitempty"`
	MaxLookupWords    int    `json:"maxLookupWords,omitempty"`
}

// ParseUser reads the stored object; anything missing or unreadable keeps the default.
func ParseUser(raw string) User {
	out := UserDefaults()
	var j userJSON
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &j) != nil {
		return out
	}
	if j.EnglishEnabled != nil {
		out.EnglishEnabled = *j.EnglishEnabled
	}
	if j.EnglishViEnabled != nil {
		out.EnglishViEnabled = *j.EnglishViEnabled
	}
	if j.DefaultDictionary == English || j.DefaultDictionary == EnglishVietnamese {
		out.DefaultDictionary = j.DefaultDictionary
	}
	out.DeckName = j.DeckName
	if j.MaxLookupWords >= 1 && j.MaxLookupWords <= MaxLookupLimit {
		out.MaxLookupWords = j.MaxLookupWords
	}
	return out
}

// JSON is the object to store.
func (u User) JSON() string {
	b, _ := json.Marshal(userJSON{
		EnglishEnabled: &u.EnglishEnabled, EnglishViEnabled: &u.EnglishViEnabled,
		DefaultDictionary: u.DefaultDictionary, DeckName: u.DeckName, MaxLookupWords: u.MaxLookupWords,
	})
	return string(b)
}

// Allows reports whether a dictionary is on for the user and for the system.
func (u User) Allows(sys System, dictionary string) bool {
	switch dictionary {
	case English:
		return u.EnglishEnabled && sys.EnglishEnabled
	case EnglishVietnamese:
		return u.EnglishViEnabled && sys.EnglishViEnabled
	}
	return false
}

// Validate checks what a user sends against what the system allows: one dictionary at least, and the default
// is one of the dictionaries that are on.
func (u User) Validate(sys System) error {
	if u.MaxLookupWords < 1 || u.MaxLookupWords > MaxLookupLimit {
		return fmt.Errorf("words per lookup must be between 1 and %d", MaxLookupLimit)
	}
	if !u.Allows(sys, English) && !u.Allows(sys, EnglishVietnamese) {
		return errors.New("at least one dictionary must stay enabled")
	}
	if u.DefaultDictionary != English && u.DefaultDictionary != EnglishVietnamese {
		return errors.New("unknown default dictionary")
	}
	if !u.Allows(sys, u.DefaultDictionary) {
		return errors.New("the default dictionary must be an enabled one")
	}
	return nil
}

// Effective returns the preferences as they work now: a dictionary the system turned off since is off for
// the user too and the default moves to one that is on.
func (u User) Effective(sys System) User {
	u.EnglishEnabled = u.Allows(sys, English)
	u.EnglishViEnabled = u.Allows(sys, EnglishVietnamese)
	if !u.Allows(sys, u.DefaultDictionary) {
		switch {
		case u.EnglishEnabled:
			u.DefaultDictionary = English
		case u.EnglishViEnabled:
			u.DefaultDictionary = EnglishVietnamese
		}
	}
	return u
}
