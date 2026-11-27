package service

import (
	"strings"
	"testing"

	"github.com/huponx/dictionary/internal/lookup"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

// groupService has the dictionary "test-x" of the group "x" enabled by an administrator.
func groupService(t *testing.T, st *fakeStore) *Service {
	t.Helper()
	s := reviewService(st)
	s.looker = fakeLooker{words: map[string]*lookup.Word{"hello": {Text: "hello"}}}
	s.cfg.MaxSavedWords = 100
	in := allOn(100)
	in.Dictionaries["test-x"] = true
	if _, err := s.UpdateSettings(userCtx(), &dictionaryv1.UpdateSettingsRequest{Settings: in}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestListDictionaries(t *testing.T) {
	s := newService(newFakeStore(), &fakeDecks{})
	r, err := s.ListDictionaries(userCtx(), &dictionaryv1.ListDictionariesRequest{})
	if err != nil || len(r.Groups) != 2 || r.Groups[0].Id != "en" || r.Groups[1].Id != "x" {
		t.Fatalf("%+v, %v", r, err)
	}
	en := r.Groups[0].Dictionaries
	if len(en) != 2 || en[0].Id != "english" || en[1].Id != "english-vietnamese" || !en[0].Enabled || !en[1].Enabled || en[1].Target != "vi" || en[1].Group != "en" {
		t.Errorf("group en = %+v", en)
	}
	if x := r.Groups[1].Dictionaries; len(x) != 1 || x[0].Id != "test-x" || x[0].Enabled {
		t.Errorf("a new dictionary is off until an administrator enables it: %+v", x)
	}

	// enabled by the administrator, narrowed by the user
	s = groupService(t, newFakeStore()).WithPrefs(&fakePrefs{raw: `{"dictionaries":{"english":false}}`})
	r, _ = s.ListDictionaries(userCtx(), &dictionaryv1.ListDictionariesRequest{})
	if !r.Groups[1].Dictionaries[0].Enabled || r.Groups[0].Dictionaries[0].Enabled || !r.Groups[0].Dictionaries[1].Enabled {
		t.Errorf("%+v", r.Groups)
	}
}

func TestLookupNeedsARegisteredEnabledDictionary(t *testing.T) {
	s := groupService(t, newFakeStore())
	for _, id := range []string{"", "latin"} {
		_, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "hello", Dictionary: id})
		if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "unknown dictionary") {
			t.Errorf("%q: %v", id, err)
		}
	}
	if _, err := s.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "hello", Dictionary: "test-x"}); err != nil {
		t.Errorf("the test dictionary: %v", err)
	}
	// off by default: a registry entry the administrator did not enable
	off := newService(newFakeStore(), &fakeDecks{})
	if _, err := off.LookupWord(userCtx(), &dictionaryv1.LookupWordRequest{Word: "hello", Dictionary: "test-x"}); code(err) != codes.FailedPrecondition {
		t.Errorf("%v", err)
	}
	if _, err := off.LookupWords(userCtx(), &dictionaryv1.LookupWordsRequest{Words: []string{"hello"}, Dictionary: "latin"}); code(err) != codes.InvalidArgument {
		t.Errorf("%v", err)
	}
}

func TestGroupIsRequiredAndKnown(t *testing.T) {
	s := groupService(t, newFakeStore())
	ctx := userCtx()
	for _, g := range []string{"", "nope"} {
		calls := map[string]error{}
		_, calls["save"] = s.SaveWord(ctx, &dictionaryv1.SaveWordRequest{Group: g, Word: "a"})
		_, calls["list"] = s.ListSavedWords(ctx, &dictionaryv1.ListSavedWordsRequest{Group: g})
		_, calls["check"] = s.CheckSavedWords(ctx, &dictionaryv1.CheckSavedWordsRequest{Group: g, Words: []string{"a"}})
		_, calls["delete"] = s.DeleteSavedWord(ctx, &dictionaryv1.DeleteSavedWordRequest{Group: g, Word: "a"})
		_, calls["export"] = s.ExportAnki(ctx, &dictionaryv1.ExportAnkiRequest{Group: g})
		_, calls["link"] = s.CreateDeckLink(ctx, &dictionaryv1.CreateDeckLinkRequest{Group: g})
		_, calls["remove"] = s.RemoveExportedWords(ctx, &dictionaryv1.RemoveExportedWordsRequest{Group: g})
		_, calls["queue"] = s.GetReviewQueue(ctx, &dictionaryv1.GetReviewQueueRequest{Group: g})
		_, calls["grade"] = s.SubmitReview(ctx, &dictionaryv1.SubmitReviewRequest{Group: g, Word: "a", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD})
		_, calls["categories"] = s.ListCategories(ctx, &dictionaryv1.ListCategoriesRequest{Group: g})
		_, calls["update"] = s.UpdateWordCategories(ctx, &dictionaryv1.UpdateWordCategoriesRequest{Group: g, Words: []string{"a"}})
		for name, err := range calls {
			if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "unknown group") {
				t.Errorf("%s with group %q: %v", name, g, err)
			}
		}
	}
}

func TestSaveNeedsAnAllowedGroup(t *testing.T) {
	// "x" is off until an administrator enables its dictionary
	s := newService(newFakeStore(), &fakeDecks{})
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "x", Word: "a"}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeDictionaryDisabled) {
		t.Errorf("group x is off: %v", err)
	}
	// the user turned off one dictionary of the group "en": the other still allows it, both off do not
	p := &fakePrefs{raw: `{"dictionaries":{"english-vietnamese":false}}`}
	s.WithPrefs(p)
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "a"}); err != nil {
		t.Errorf("one dictionary of the group is on: %v", err)
	}
	p.raw = `{"dictionaries":{"english":false,"english-vietnamese":false}}`
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Group: "en", Word: "b"}); code(err) != codes.FailedPrecondition {
		t.Errorf("both off: %v", err)
	}
}

// The same word saved in two groups is two cards; listing, checking, deleting, reviewing and removing the exported
// words work on one group at a time.
func TestTheSameWordInTwoGroups(t *testing.T) {
	st := newFakeStore()
	s := groupService(t, st)
	ctx := userCtx()
	save := func(g, w, content string) {
		t.Helper()
		if _, err := s.SaveWord(ctx, &dictionaryv1.SaveWordRequest{Group: g, Word: w, Content: content}); err != nil {
			t.Fatalf("save %s/%s: %v", g, w, err)
		}
	}
	save("en", "hello", "<p>en</p>")
	save("x", "hello", "<p>x</p>")
	save("x", "only-x", "<p>only</p>")

	list := func(g string) *dictionaryv1.ListSavedWordsResponse {
		t.Helper()
		r, err := s.ListSavedWords(ctx, &dictionaryv1.ListSavedWordsRequest{Group: g})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := list("en"); len(r.Words) != 1 || r.Total != 1 || r.Words[0].Content != "<p>en</p>" {
		t.Errorf("en: %+v", r)
	}
	if r := list("x"); len(r.Words) != 2 || r.Total != 2 || r.Words[0].Group != "x" {
		t.Errorf("x: %+v", r)
	}

	chk, err := s.CheckSavedWords(ctx, &dictionaryv1.CheckSavedWordsRequest{Group: "en", Words: []string{"hello", "only-x"}})
	if err != nil || len(chk.Words) != 1 || chk.Words[0].Word != "hello" {
		t.Errorf("check en: %+v %v", chk, err)
	}

	// review: one group's queue holds its words only, a grade is for its group's card
	q, err := s.GetReviewQueue(ctx, &dictionaryv1.GetReviewQueueRequest{Group: "x"})
	if err != nil || len(q.Cards) != 2 || q.TotalCount != 2 {
		t.Fatalf("queue x: %+v %v", q, err)
	}
	for _, c := range q.Cards {
		if c.Group != "x" {
			t.Errorf("card %s has group %q: the client grades it with that group", c.Word, c.Group)
		}
	}
	if _, err := s.SubmitReview(ctx, &dictionaryv1.SubmitReviewRequest{Group: "x", Word: "only-x", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitReview(ctx, &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "only-x", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD}); code(err) != codes.NotFound && err == nil {
		t.Errorf("a word of another group was graded: %v", err)
	}
	if q, _ := s.GetReviewQueue(ctx, &dictionaryv1.GetReviewQueueRequest{Group: "en"}); len(q.Cards) != 1 || q.TotalCount != 1 {
		t.Errorf("queue en: %+v", q)
	}

	// export of one group removes its words only
	if r, err := s.ExportAnki(ctx, &dictionaryv1.ExportAnkiRequest{Group: "x"}); err != nil || r.WordCount != 2 {
		t.Fatalf("export x: %+v %v", r, err)
	}
	if r := list("x"); len(r.Words) != 0 {
		t.Errorf("x after the export: %+v", r)
	}
	if r := list("en"); len(r.Words) != 1 {
		t.Errorf("en after the export of x: %+v", r)
	}
	if _, err := s.ExportAnki(ctx, &dictionaryv1.ExportAnkiRequest{Group: "x"}); code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), CodeNoWords) {
		t.Errorf("nothing left in x: %v", err)
	}

	// delete and categories
	if _, err := s.DeleteSavedWord(ctx, &dictionaryv1.DeleteSavedWordRequest{Group: "x", Word: "hello"}); code(err) != codes.NotFound {
		t.Errorf("hello left x with the export: %v", err)
	}
	if _, err := s.DeleteSavedWord(ctx, &dictionaryv1.DeleteSavedWordRequest{Group: "en", Word: "hello"}); err != nil {
		t.Errorf("delete in en: %v", err)
	}
	if r := list("en"); len(r.Words) != 0 {
		t.Errorf("en: %+v", r)
	}
}

func TestCategoryCountsAreOfTheGroup(t *testing.T) {
	st := newFakeStore()
	s := groupService(t, st)
	ctx := userCtx()
	for _, g := range []string{"en", "x", "x"} {
		w := "w" + g
		if g == "x" && len(st.in("x").rows) > 0 {
			w = "w2"
		}
		if _, err := s.SaveWord(ctx, &dictionaryv1.SaveWordRequest{Group: g, Word: w}); err != nil {
			t.Fatal(err)
		}
	}
	for g, want := range map[string]int32{"en": 1, "x": 2} {
		r, err := s.ListCategories(ctx, &dictionaryv1.ListCategoriesRequest{Group: g})
		if err != nil || r.UncategorizedTotal != want {
			t.Errorf("%s: %+v %v, want %d uncategorized", g, r, err, want)
		}
	}
}
