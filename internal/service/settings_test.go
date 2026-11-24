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
	return &dictionaryv1.Settings{MaxSavedWords: max, AllowExportDeck: true, AllowExportLink: true, EnglishEnabled: true, EnglishViEnabled: true,
		RemoveAfterExport: true, ExportedRetentionHours: 168}
}

func TestSettingsDefaultsFromConfig(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	r, err := s.GetSettings(userCtx(), &dictionaryv1.GetSettingsRequest{})
	if err != nil || r.Settings.MaxSavedWords != 3 || !r.Settings.AllowExportDeck || !r.Settings.EnglishViEnabled {
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
		nil, allOn(0), allOn(1001),
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
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "world"})
	if code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), CodeMaxSavedWords) {
		t.Fatalf("%v", err)
	}
}

func TestExportFlags(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "hello"}); err != nil {
		t.Fatal(err)
	}
	in := allOn(10)
	in.AllowExportDeck = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeFeatureDisabled) {
		t.Fatalf("deck: %v", err)
	}
	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{}); err != nil {
		t.Fatalf("link must still work: %v", err)
	}
	in.AllowExportLink = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeFeatureDisabled) {
		t.Fatalf("link: %v", err)
	}
}

func TestDisabledDictionary(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	in := allOn(10)
	in.EnglishViEnabled = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	vi := dictionaryv1.Dictionary_DICTIONARY_ENGLISH_VIETNAMESE
	_, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "hello", Dictionary: vi})
	if code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeDictionaryDisabled) {
		t.Fatalf("%v", err)
	}
	if _, err := s.LookupWords(userCtx(), &dictionaryv1.LookupWordsRequest{Words: []string{"hello"}, Dictionary: vi}); code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}
	if _, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "hello"}); err != nil {
		t.Fatalf("english still works: %v", err)
	}
}

func TestMySettings(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})

	// no identity configured: defaults, no changes
	r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.MaxLookupWords != 20 || !r.User.EnglishEnabled || r.System.MaxSavedWords != 3 {
		t.Fatalf("%+v, %v", r, err)
	}
	put := &dictionaryv1.UpdateMySettingsRequest{User: &dictionaryv1.UserSettings{EnglishEnabled: true, DeckName: "Words", MaxLookupWords: 5}}
	if _, err := s.UpdateMySettings(userCtx(), put); code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}

	p := &fakePrefs{raw: `{"englishViEnabled":false,"deckName":"Mine","maxLookupWords":7}`}
	s.WithPrefs(p)
	r, err = s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.EnglishViEnabled || r.User.DeckName != "Mine" || r.User.MaxLookupWords != 7 {
		t.Fatalf("%+v, %v", r, err)
	}

	// identity down: defaults, not an error
	p.getErr = errors.New("down")
	r, err = s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || !r.User.EnglishViEnabled || r.User.MaxLookupWords != 20 {
		t.Fatalf("%+v, %v", r, err)
	}

	// stored
	if _, err := s.UpdateMySettings(userCtx(), put); err != nil {
		t.Fatal(err)
	}
	if u := settings.ParseUser(p.written); u.DeckName != "Words" || u.MaxLookupWords != 5 || u.EnglishViEnabled || u.DefaultDictionary != settings.English {
		t.Fatalf("%s", p.written)
	}

	// invalid: nothing enabled, too many words, default is off, long deck name
	for name, u := range map[string]*dictionaryv1.UserSettings{
		"nothing on":   {MaxLookupWords: 5},
		"too many":     {EnglishEnabled: true, MaxLookupWords: 21},
		"default off":  {EnglishEnabled: true, MaxLookupWords: 5, DefaultDictionary: dictionaryv1.Dictionary_DICTIONARY_ENGLISH_VIETNAMESE},
		"long deck":    {EnglishEnabled: true, MaxLookupWords: 5, DeckName: strings.Repeat("a", 65)},
		"missing user": nil,
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
	s := newService(newFakeStore(), &fakeDecks{}).WithPrefs(&fakePrefs{raw: `{"defaultDictionary":"english_vi"}`})
	in := allOn(10)
	in.EnglishViEnabled = false
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || r.User.EnglishViEnabled || r.User.DefaultDictionary != dictionaryv1.Dictionary_DICTIONARY_ENGLISH {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestMySettingsNeedAUser(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	if _, err := s.GetMySettings(context.Background(), &dictionaryv1.GetMySettingsRequest{}); code(err) != codes.Unauthenticated {
		t.Fatalf("%v", err)
	}
}
