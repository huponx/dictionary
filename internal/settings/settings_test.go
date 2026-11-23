package settings

import (
	"encoding/json"
	"testing"
)

func TestSystemDefaultsAndRows(t *testing.T) {
	def := Defaults(100)
	if got := FromRows(nil, def); got != def {
		t.Fatalf("no rows: %+v", got)
	}
	rows := map[string]json.RawMessage{
		"max_saved_words": json.RawMessage(`250`), "allow_export_link": json.RawMessage(`false`),
		"unknown": json.RawMessage(`1`), "english_vi_enabled": json.RawMessage(`"yes"`), // wrong type: default
	}
	got := FromRows(rows, def)
	want := System{MaxSavedWords: 250, AllowExportDeck: true, AllowExportLink: false, EnglishEnabled: true, EnglishViEnabled: true}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestChangedWritesOnlyWhatDiffers(t *testing.T) {
	cur := Defaults(100)
	next := cur
	next.MaxSavedWords = 50
	next.AllowExportLink = false
	ch := Changed(cur, next)
	if len(ch) != 2 || string(ch["max_saved_words"]) != "50" || string(ch["allow_export_link"]) != "false" {
		t.Fatalf("%v", ch)
	}
	if len(Changed(cur, cur)) != 0 {
		t.Fatal("nothing changed, nothing written")
	}
}

func TestSystemValidate(t *testing.T) {
	ok := Defaults(100)
	bad := []System{
		{MaxSavedWords: 0, EnglishEnabled: true},
		{MaxSavedWords: MaxSavedWordsLimit + 1, EnglishEnabled: true},
		{MaxSavedWords: 10},
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, s := range bad {
		if s.Validate() == nil {
			t.Errorf("%d accepted", i)
		}
	}
}

func TestUserRoundTripAndDefaults(t *testing.T) {
	if got := ParseUser(""); got != UserDefaults() {
		t.Fatalf("%+v", got)
	}
	if got := ParseUser("{not json"); got != UserDefaults() {
		t.Fatalf("%+v", got)
	}
	u := User{EnglishEnabled: false, EnglishViEnabled: true, DefaultDictionary: EnglishVietnamese, DeckName: "Words", MaxLookupWords: 5}
	if got := ParseUser(u.JSON()); got != u {
		t.Fatalf("%+v", got)
	}
	// a missing key is the default, not false
	if got := ParseUser(`{"deckName":"x"}`); !got.EnglishEnabled || got.DeckName != "x" || got.MaxLookupWords != MaxLookupLimit {
		t.Fatalf("%+v", got)
	}
}

func TestUserValidateAgainstSystem(t *testing.T) {
	sys := Defaults(100)
	u := UserDefaults()
	if err := u.Validate(sys); err != nil {
		t.Fatal(err)
	}
	cases := map[string]User{
		"no dictionary":       {MaxLookupWords: 5, DefaultDictionary: English},
		"default is off":      {EnglishViEnabled: true, DefaultDictionary: English, MaxLookupWords: 5},
		"unknown default":     {EnglishEnabled: true, DefaultDictionary: "latin", MaxLookupWords: 5},
		"too many per lookup": {EnglishEnabled: true, DefaultDictionary: English, MaxLookupWords: MaxLookupLimit + 1},
		"none per lookup":     {EnglishEnabled: true, DefaultDictionary: English},
	}
	for name, c := range cases {
		if c.Validate(sys) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// the system turned a dictionary off: the user cannot choose it
	noVi := sys
	noVi.EnglishViEnabled = false
	if (User{EnglishViEnabled: true, DefaultDictionary: EnglishVietnamese, MaxLookupWords: 5}).Validate(noVi) == nil {
		t.Error("a dictionary the system turned off was accepted")
	}
}

func TestEffectiveFollowsTheSystem(t *testing.T) {
	sys := Defaults(100)
	sys.EnglishEnabled = false
	got := UserDefaults().Effective(sys) // English was the default
	if got.EnglishEnabled || !got.EnglishViEnabled || got.DefaultDictionary != EnglishVietnamese {
		t.Fatalf("%+v", got)
	}
}
