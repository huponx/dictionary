package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huponx/dictionary/internal/settings"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

type fakePrefs struct {
	raw     string
	getErr  error
	putErr  error
	written string
}

func (f *fakePrefs) Get(context.Context) (string, error) { return f.raw, f.getErr }
func (f *fakePrefs) Put(_ context.Context, s string) error {
	f.written = s
	return f.putErr
}

func allOn(max int32) *dictionaryv1.Settings {
	return &dictionaryv1.Settings{MaxSavedWords: max, AllowExportDeck: true, AllowExportLink: true,
		Dictionaries:      map[string]bool{"english": true, "english-vietnamese": true},
		RemoveAfterExport: true, ExportedRetentionHours: 168, MaxCategories: 50, MaxCategoryNameLength: 40}
}

func TestSettingsDefaultsFromConfig(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	r, err := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if err != nil || r.Settings.MaxSavedWords != 3 || !r.Settings.AllowExportDeck || !r.Settings.Dictionaries["english-vietnamese"] || r.Settings.Dictionaries["test-x"] {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestUpdateSettingsWritesOnlyChangedRows(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	in := allOn(50)
	in.AllowExportLink = false
	r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in})
	if err != nil || r.Settings.MaxSavedWords != 50 {
		t.Fatalf("%+v, %v", r, err)
	}
	if len(st.settings) != 2 || string(st.settings["max_saved_words"]) != "50" || string(st.settings["allow_export_link"]) != "false" {
		t.Fatalf("rows: %v", st.settings)
	}
	g, _ := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if g.Settings.MaxSavedWords != 50 || g.Settings.AllowExportLink || !g.Settings.AllowExportDeck {
		t.Fatalf("%+v", g.Settings)
	}
}

func TestUpdateSettingsValidation(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	bad := []*dictionaryv1.Settings{
		nil, allOn(0), allOn(1_000_001),
		{MaxSavedWords: 10, AllowExportDeck: true},
	}
	for i, in := range bad {
		if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); code(err) != codes.InvalidArgument {
			t.Errorf("%d: %v", i, err)
		}
	}
}

func TestSaveWordUsesTheStoredMax(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: allOn(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "world"})
	if code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), CodeMaxSavedWords) {
		t.Fatalf("%v", err)
	}
}

func TestExportFlags(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "hello"}); err != nil {
		t.Fatal(err)
	}
	in := allOn(10)
	in.AllowExportDeck = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeFeatureDisabled) {
		t.Fatalf("deck: %v", err)
	}
	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{Group: "en"}); err != nil {
		t.Fatalf("link must still work: %v", err)
	}
	in.AllowExportLink = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{Group: "en"}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeFeatureDisabled) {
		t.Fatalf("link: %v", err)
	}
}

func TestDisabledDictionary(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	in := allOn(10)
	in.Dictionaries["english-vietnamese"] = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	vi := "english-vietnamese"
	_, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "hello", Dictionary: vi})
	if code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeDictionaryDisabled) {
		t.Fatalf("%v", err)
	}
	if _, err := s.LookupWords(userCtx(), &dictionaryv1.LookupWordsRequest{Words: []string{"hello"}, Dictionary: vi}); code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}
	if _, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Dictionary: "english", Word: "hello"}); err != nil {
		t.Fatalf("english still works: %v", err)
	}
}

func TestMySettings(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})

	// no identity configured: defaults, no changes
	r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.MaxLookupWords != 20 || !r.User.Dictionaries["english"] || r.User.Dictionaries["test-x"] || r.User.DefaultDictionary != "english" || r.System.MaxSavedWords != 3 {
		t.Fatalf("%+v, %v", r, err)
	}
	put := &dictionaryv1.UpdateMySettingsRequest{User: &dictionaryv1.UserSettings{DeckName: "Words", MaxLookupWords: 5}}
	if _, err := s.UpdateMySettings(userCtx(), put); code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}

	p := &fakePrefs{raw: `{"dictionaries":{"english-vietnamese":false},"deckName":"Mine","maxLookupWords":7}`}
	s.WithPrefs(p)
	r, err = s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.Dictionaries["english-vietnamese"] || r.User.DeckName != "Mine" || r.User.MaxLookupWords != 7 {
		t.Fatalf("%+v, %v", r, err)
	}

	// identity down: defaults, not an error
	p.getErr = errors.New("down")
	r, err = s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || !r.User.Dictionaries["english-vietnamese"] || r.User.MaxLookupWords != 20 {
		t.Fatalf("%+v, %v", r, err)
	}

	// stored
	if _, err := s.UpdateMySettings(userCtx(), put); err != nil {
		t.Fatal(err)
	}
	if u := settings.ParseUser(p.written); u.DeckName != "Words" || u.MaxLookupWords != 5 || u.Dictionaries["english-vietnamese"] || u.DefaultDictionary != "" {
		t.Fatalf("%s", p.written)
	}

	// invalid: nothing enabled, too many words, default is off, long deck name
	for name, u := range map[string]*dictionaryv1.UserSettings{
		"nothing on":    {MaxLookupWords: 5, Dictionaries: map[string]bool{"english": false, "english-vietnamese": false}},
		"too many":      {MaxLookupWords: 21},
		"default off":   {MaxLookupWords: 5, Dictionaries: map[string]bool{"english-vietnamese": false}, DefaultDictionary: "english-vietnamese"},
		"unknown":       {MaxLookupWords: 5, Dictionaries: map[string]bool{"latin": true}},
		"off by system": {MaxLookupWords: 5, DefaultDictionary: "test-x"},
		"long deck":     {MaxLookupWords: 5, DeckName: strings.Repeat("a", 65)},
		"missing user":  nil,
	} {
		p.written = ""
		if _, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: u}); code(err) != codes.InvalidArgument || p.written != "" {
			t.Errorf("%s: %v (written %q)", name, err, p.written)
		}
	}

	// identity refuses the write
	p.putErr = errors.New("down")
	if _, err := s.UpdateMySettings(userCtx(), put); code(err) != codes.Unavailable {
		t.Fatalf("%v", err)
	}
}

func TestMySettingsFollowTheSystem(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{}).WithPrefs(&fakePrefs{raw: `{"defaultDictionary":"english-vietnamese"}`})
	in := allOn(10)
	in.Dictionaries["english-vietnamese"] = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.Dictionaries["english-vietnamese"] || r.User.DefaultDictionary != "english" {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestMySettingsNeedAUser(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	if _, err := s.GetMySettings(context.Background(), &dictionaryv1.GetMySettingsRequest{}); code(err) != codes.Unauthenticated {
		t.Fatalf("%v", err)
	}
}

func TestFreshDeckPreference(t *testing.T) {
	for raw, want := range map[string]bool{"": false, `{"deckName":"x"}`: false, `{"freshDeck":true}`: true} {
		decks := &fakeDecks{}
		s := newService(newFakeStore(), decks).WithPrefs(&fakePrefs{raw: raw})
		saveWords(t, s, "one")
		if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
			t.Fatal(err)
		}
		if decks.fresh != want {
			t.Errorf("prefs %q: fresh = %v, want %v", raw, decks.fresh, want)
		}
	}

	// the preference is stored and read back through the settings calls
	p := &fakePrefs{}
	s := newService(newFakeStore(), &fakeDecks{}).WithPrefs(p)
	put := &dictionaryv1.UpdateMySettingsRequest{User: &dictionaryv1.UserSettings{MaxLookupWords: 5, FreshDeck: true}}
	if r, err := s.UpdateMySettings(userCtx(), put); err != nil || !r.User.FreshDeck {
		t.Fatalf("%+v, %v", r, err)
	}
	if !settings.ParseUser(p.written).FreshDeck {
		t.Fatalf("not stored: %s", p.written)
	}
	p.raw = p.written
	if r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{}); err != nil || !r.User.FreshDeck {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestReviewMaxPerDaySetting(t *testing.T) {
	p := &fakePrefs{}
	s := newService(newFakeStore(), &fakeDecks{}).WithPrefs(p)
	if r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{}); err != nil || r.User.ReviewMaxPerDay != 200 {
		t.Fatalf("%+v, %v", r, err)
	}
	put := func(n int32) error {
		_, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: &dictionaryv1.UserSettings{
			MaxLookupWords: 5, ReviewMaxPerDay: n}})
		return err
	}
	for _, n := range []int32{0, 500, 9999} {
		if err := put(n); err != nil || settings.ParseUser(p.written).ReviewMaxPerDay != int(n) {
			t.Fatalf("%d: %v %s", n, err, p.written)
		}
	}
	p.raw = p.written
	if r, _ := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{}); r.User.ReviewMaxPerDay != 9999 {
		t.Fatalf("%+v", r.User)
	}
	p.written = ""
	for _, n := range []int32{-1, 10000} {
		if err := put(n); code(err) != codes.InvalidArgument || p.written != "" {
			t.Errorf("%d: %v", n, err)
		}
	}
}

func TestReviewNewPerDaySetting(t *testing.T) {
	p := &fakePrefs{}
	s := newService(newFakeStore(), &fakeDecks{}).WithPrefs(p)
	r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.ReviewNewPerDay != 10 {
		t.Fatalf("%+v, %v", r, err)
	}
	put := func(n int32) error {
		_, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: &dictionaryv1.UserSettings{
			MaxLookupWords: 5, ReviewNewPerDay: n}})
		return err
	}
	for _, n := range []int32{0, 25, 100} {
		if err := put(n); err != nil || settings.ParseUser(p.written).ReviewNewPerDay != int(n) {
			t.Fatalf("%d: %v %s", n, err, p.written)
		}
	}
	p.raw = p.written
	if r, _ := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{}); r.User.ReviewNewPerDay != 100 {
		t.Fatalf("%+v", r.User)
	}
	p.written = ""
	for _, n := range []int32{-1, 101} {
		if err := put(n); code(err) != codes.InvalidArgument || p.written != "" {
			t.Errorf("%d: %v", n, err)
		}
	}
}

func TestUpdateSettingsCategoryLimits(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	in := allOn(50)
	in.MaxCategories, in.MaxCategoryNameLength = 7, 12
	r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in})
	if err != nil || r.Settings.MaxCategories != 7 || r.Settings.MaxCategoryNameLength != 12 {
		t.Fatalf("%+v, %v", r, err)
	}
	g, _ := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if g.Settings.MaxCategories != 7 || g.Settings.MaxCategoryNameLength != 12 {
		t.Fatalf("%+v", g.Settings)
	}
	// a client that does not send them keeps the current values
	old := allOn(50)
	old.MaxCategories, old.MaxCategoryNameLength = 0, 0
	if r, err = s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: old}); err != nil || r.Settings.MaxCategories != 7 {
		t.Fatalf("%+v, %v", r, err)
	}
	for _, bad := range [][2]int32{{settings.MaxCategoriesLimit + 1, 40}, {50, settings.MaxCategoryNameLimit + 1}, {-1, 40}} {
		in := allOn(50)
		in.MaxCategories, in.MaxCategoryNameLength = bad[0], bad[1]
		if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); code(err) != codes.InvalidArgument {
			t.Errorf("%v accepted: %v", bad, err)
		}
	}
}

func TestUpdateSettingsAcceptsAMillionWords(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: allOn(1_000_000)})
	if err != nil || r.Settings.MaxSavedWords != 1_000_000 {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestExportSizeSettingsAreBoundedByTheCeilings(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	s.cfg.ExportMaxBytesCeiling, s.cfg.ExportPartMaxBytesCeiling = 10<<20, 50<<20
	g, _ := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if g.Settings.ExportMaxBytes != 3<<20 || g.Settings.ExportPartMaxBytes != 3<<20 ||
		g.Settings.ExportMaxBytesCeiling != 10<<20 || g.Settings.ExportPartMaxBytesCeiling != 50<<20 {
		t.Fatalf("defaults: %+v", g.Settings)
	}
	for name, mut := range map[string]func(*dictionaryv1.Settings){
		"sync over the ceiling": func(in *dictionaryv1.Settings) { in.ExportMaxBytes = 10<<20 + 1 },
		"part over the ceiling": func(in *dictionaryv1.Settings) { in.ExportPartMaxBytes = 50<<20 + 1 },
		"part under 64 KiB":     func(in *dictionaryv1.Settings) { in.ExportPartMaxBytes = 1000 },
	} {
		in := allOn(10)
		mut(in)
		if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := allOn(10)
	in.ExportMaxBytes, in.ExportPartMaxBytes = 10<<20, 25<<20
	in.ExportMaxBytesCeiling = 1 // read only: ignored
	r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in})
	if err != nil || r.Settings.ExportMaxBytes != 10<<20 || r.Settings.ExportPartMaxBytes != 25<<20 || r.Settings.ExportMaxBytesCeiling != 10<<20 {
		t.Fatalf("%+v, %v", r, err)
	}
	// a client that predates the settings sends none: the stored values stay
	old := allOn(10)
	if r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: old}); err != nil || r.Settings.ExportPartMaxBytes != 25<<20 {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestUpdateSettingsExportSyncMaxWords(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	in := allOn(50) // a client that predates the setting sends 0: the current value stays
	if r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil || r.Settings.ExportSyncMaxWords != 2000 {
		t.Fatalf("%+v, %v", r, err)
	}
	in.ExportSyncMaxWords = 4000
	if r, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil || r.Settings.ExportSyncMaxWords != 4000 {
		t.Fatalf("%+v, %v", r, err)
	}
	if string(st.settings["export_sync_max_words"]) != "4000" {
		t.Errorf("rows: %v", st.settings)
	}
	in.ExportSyncMaxWords = 5001
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); code(err) != codes.InvalidArgument {
		t.Errorf("5001: %v", err)
	}
}
