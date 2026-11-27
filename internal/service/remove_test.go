package service

import (
	"context"
	"encoding/json"
	"testing"

	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

func saveWords(t *testing.T, s *Service, words ...string) {
	t.Helper()
	for _, w := range words {
		if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: w, Content: w}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportRemovesOnlyWhenSystemAndUserAllow(t *testing.T) {
	cases := []struct {
		name       string
		sysOff     bool
		prefs      string
		prefsErr   error
		wantRemove bool
	}{
		{"both on", false, "", nil, true},
		{"system off", true, "", nil, false},
		{"user off", false, `{"removeAfterExport":false}`, nil, false},
		{"both off", true, `{"removeAfterExport":false}`, nil, false},
		{"identity down means the default, remove", false, "", context.DeadlineExceeded, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newFakeStore()
			s := newService(st, &fakeDecks{}).WithPrefs(&fakePrefs{raw: c.prefs, getErr: c.prefsErr})
			if c.sysOff {
				st.settings = map[string]json.RawMessage{"remove_after_export": json.RawMessage(`false`)}
			}
			saveWords(t, s, "one", "two")
			if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
				t.Fatal(err)
			}
			if st.lastRemove != c.wantRemove {
				t.Errorf("remove = %v, want %v", st.lastRemove, c.wantRemove)
			}
			r, _ := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{Group: "en"})
			wantTotal, wantNew := int32(0), int32(0)
			if !c.wantRemove {
				wantTotal = 2
			}
			if r.Total != wantTotal || r.NewTotal != wantNew {
				t.Errorf("total=%d new=%d, want %d and %d", r.Total, r.NewTotal, wantTotal, wantNew)
			}
			for _, w := range r.Words {
				if !w.Exported {
					t.Errorf("%s must be flagged exported", w.Word)
				}
			}
		})
	}
}

func TestExportScope(t *testing.T) {
	st := newFakeStore()
	decks := &fakeDecks{}
	s := newService(st, decks).WithPrefs(&fakePrefs{raw: `{"removeAfterExport":false}`})
	saveWords(t, s, "one", "two")
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
		t.Fatal(err)
	}
	saveWords(t, s, "three")

	// new only (also the default): just "three"
	r, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en", Scope: dictionaryv1.ExportScope_EXPORT_SCOPE_NEW})
	if err != nil || r.WordCount != 1 {
		t.Fatalf("new: %+v %v", r, err)
	}
	// nothing new now: the usual error, even though the list is not empty
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); code(err) != codes.InvalidArgument {
		t.Errorf("no new words: %v", err)
	}
	// all: the three words
	r, err = s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en", Scope: dictionaryv1.ExportScope_EXPORT_SCOPE_ALL})
	if err != nil || r.WordCount != 3 {
		t.Fatalf("all: %+v %v", r, err)
	}
	// the same for a link
	l, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{Group: "en", Scope: dictionaryv1.ExportScope_EXPORT_SCOPE_ALL})
	if err != nil || l.WordCount != 3 {
		t.Fatalf("link all: %+v %v", l, err)
	}
	if _, err := s.CreateDeckLink(userCtx(), &dictionaryv1.CreateDeckLinkRequest{Group: "en"}); code(err) != codes.InvalidArgument {
		t.Errorf("link, no new words: %v", err)
	}
}

func TestRemoveExportedWords(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{}).WithPrefs(&fakePrefs{raw: `{"removeAfterExport":false}`})
	saveWords(t, s, "one", "two")
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
		t.Fatal(err)
	}
	saveWords(t, s, "three")
	r, err := s.RemoveExportedWords(userCtx(), &dictionaryv1.RemoveExportedWordsRequest{Group: "en"})
	if err != nil || r.Removed != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	if l, _ := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{Group: "en"}); l.Total != 1 || l.NewTotal != 1 {
		t.Errorf("the new word stays: %+v", l)
	}
	if _, err := s.RemoveExportedWords(context.Background(), &dictionaryv1.RemoveExportedWordsRequest{Group: "en"}); code(err) != codes.Unauthenticated {
		t.Errorf("needs a user: %v", err)
	}
}

func TestRemoveSettingsRoundTrip(t *testing.T) {
	st := newFakeStore()
	p := &fakePrefs{}
	s := newService(st, &fakeDecks{}).WithPrefs(p)
	// the administrator turns it off and sets the retention
	in := allOn(10)
	in.RemoveAfterExport, in.ExportedRetentionHours = false, 24
	if _, err := s.UpdateSettings(context.Background(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSettings(context.Background(), &dictionaryv1.GetSettingsRequest{})
	if got.Settings.RemoveAfterExport || got.Settings.ExportedRetentionHours != 24 {
		t.Errorf("%+v", got.Settings)
	}
	for _, bad := range []int32{0, -1, 8761} {
		in.ExportedRetentionHours = bad
		if _, err := s.UpdateSettings(context.Background(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); code(err) != codes.InvalidArgument {
			t.Errorf("retention %d accepted: %v", bad, err)
		}
	}
	// the choice of the user survives the system being off, so turning the system on again restores it
	u := &dictionaryv1.UserSettings{
		MaxLookupWords: 5, RemoveAfterExport: true, RemoveAcknowledged: true,
	}
	if _, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: u}); err != nil {
		t.Fatal(err)
	}
	p.raw = p.written
	my, _ := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if !my.User.RemoveAfterExport || !my.User.RemoveAcknowledged || my.System.RemoveAfterExport {
		t.Errorf("user=%+v system=%+v", my.User, my.System)
	}
}
