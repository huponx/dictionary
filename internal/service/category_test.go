package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/authn"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

// The category methods of the fake store.

func (f *fakeStore) category(id string) int {
	return slices.IndexFunc(f.cats, func(c store.Category) bool { return c.ID == id })
}

func (f *fakeStore) link(word, id string) {
	if f.wordCats == nil {
		f.wordCats = map[string]map[string]bool{}
	}
	if f.wordCats[word] == nil {
		f.wordCats[word] = map[string]bool{}
	}
	f.wordCats[word][id] = true
}

func (f *fakeStore) idsOf(word string) []string {
	var out []string
	for id := range f.wordCats[word] {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (f *fakeStore) match(word string, flt store.WordFilter) bool {
	switch {
	case flt.Uncategorized:
		return len(f.wordCats[word]) == 0
	case flt.CategoryID != "":
		return f.wordCats[word][flt.CategoryID]
	}
	return true
}

func (f *fakeStore) ListCategories(context.Context, string) (store.Categories, error) {
	f.listCalls++
	out := slices.Clone(f.cats)
	for i, c := range out {
		out[i].Total, out[i].New, _ = f.count(store.WordFilter{CategoryID: c.ID})
	}
	total, fresh, _ := f.count(store.WordFilter{Uncategorized: true})
	return store.Categories{List: out, UncatTotal: total, UncatNew: fresh}, nil
}

func (f *fakeStore) CategoryName(_ context.Context, _, id string) (string, error) {
	for _, c := range f.cats {
		if c.ID == id {
			return c.Name, nil
		}
	}
	return "", store.ErrNotFound
}

func (f *fakeStore) count(flt store.WordFilter) (int, int, error) {
	t, n, _, err := f.CountPending(context.Background(), "", flt)
	return t, n, err
}

func (f *fakeStore) CreateCategory(_ context.Context, _, name string, max int) (store.Category, error) {
	if len(f.cats) >= max {
		return store.Category{}, store.ErrLimit
	}
	for _, c := range f.cats {
		if strings.EqualFold(c.Name, name) {
			return store.Category{}, store.ErrCategoryExists
		}
	}
	c := store.Category{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.cats)+1), Name: name}
	f.cats = append(f.cats, c)
	return c, nil
}

func (f *fakeStore) RenameCategory(_ context.Context, _, id, name string) (store.Category, error) {
	i := f.category(id)
	if i < 0 {
		return store.Category{}, store.ErrNotFound
	}
	for _, c := range f.cats {
		if c.ID != id && strings.EqualFold(c.Name, name) {
			return store.Category{}, store.ErrCategoryExists
		}
	}
	f.cats[i].Name = name
	return f.cats[i], nil
}

func (f *fakeStore) DeleteCategory(_ context.Context, _, id string) error {
	i := f.category(id)
	if i < 0 {
		return store.ErrNotFound
	}
	f.cats = slices.Delete(f.cats, i, i+1)
	for _, m := range f.wordCats {
		delete(m, id)
	}
	return nil
}

func (f *fakeStore) SetWordCategories(_ context.Context, _ string, words, add, remove []string) (int, error) {
	for _, id := range append(slices.Clone(add), remove...) {
		if f.category(id) < 0 {
			return 0, store.ErrNotFound
		}
	}
	n := 0
	for _, w := range words {
		if _, ok := f.rows[w]; !ok {
			continue
		}
		n++
		for _, id := range add {
			f.link(w, id)
		}
		for _, id := range remove {
			delete(f.wordCats[w], id)
		}
	}
	return n, nil
}

func categoryService(t *testing.T, words ...string) (*Service, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	for _, w := range words {
		if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: w, Content: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	return s, st
}

func mustCategory(t *testing.T, s *Service, name string) string {
	t.Helper()
	r, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: name})
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return r.GetCategory().GetId()
}

func assign(t *testing.T, s *Service, id string, words ...string) {
	t.Helper()
	if _, err := s.UpdateWordCategories(userCtx(), &dictionaryv1.UpdateWordCategoriesRequest{Words: words, AddCategoryIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
}

func TestCategoryNameValidation(t *testing.T) {
	s, _ := categoryService(t)
	for _, name := range []string{"", "   ", strings.Repeat("a", 41), "bad\x00name"} {
		if _, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: name}); code(err) != codes.InvalidArgument {
			t.Errorf("name %q: %v", name, err)
		}
	}
	r, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: "  Travel  "})
	if err != nil || r.GetCategory().GetName() != "Travel" {
		t.Fatalf("trimmed: %v %v", r, err)
	}
	if _, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: "TRAVEL"}); code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), CodeCategoryExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.RenameCategory(userCtx(), &dictionaryv1.RenameCategoryRequest{Id: "nope", Name: "x"}); code(err) != codes.NotFound {
		t.Errorf("rename unknown: %v", err)
	}
}

func TestCategoryLimit(t *testing.T) {
	s, _ := categoryService(t)
	for i := 0; i < settings.DefaultMaxCategories; i++ {
		mustCategory(t, s, fmt.Sprintf("c%d", i))
	}
	_, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: "one more"})
	if code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), CodeCategoryLimit) {
		t.Errorf("limit: %v", err)
	}
}

func TestCategoryPermissionsNeedAUser(t *testing.T) {
	s, _ := categoryService(t)
	svc := authn.WithPrincipal(context.Background(), &authn.Principal{Subject: "gatekeeper", ClientID: "gatekeeper"})
	if _, err := s.ListCategories(svc, &dictionaryv1.ListCategoriesRequest{}); err == nil {
		t.Error("a service token must be refused")
	}
	for m, want := range map[string]string{"ListCategories": "read", "CreateCategory": "write", "RenameCategory": "write", "DeleteCategory": "write", "UpdateWordCategories": "write"} {
		if got := Permissions["/dictionary.v1.DictionaryService/"+m]; got != "dictionary:word:"+want {
			t.Errorf("%s needs %q", m, got)
		}
	}
}

func TestListSavedWordsFiltersByCategory(t *testing.T) {
	s, _ := categoryService(t, "alpha", "beta", "gamma")
	a, b := mustCategory(t, s, "A"), mustCategory(t, s, "B")
	assign(t, s, a, "alpha", "beta")
	assign(t, s, b, "beta")

	list := func(req *dictionaryv1.ListSavedWordsRequest) *dictionaryv1.ListSavedWordsResponse {
		r, err := s.ListSavedWords(userCtx(), req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := list(&dictionaryv1.ListSavedWordsRequest{CategoryId: a})
	if len(r.Words) != 2 || r.Total != 2 {
		t.Errorf("category A: %v total=%d", r.Words, r.Total)
	}
	if r := list(&dictionaryv1.ListSavedWordsRequest{CategoryId: "uncategorized"}); len(r.Words) != 1 || r.Words[0].Word != "gamma" {
		t.Errorf("uncategorized by id: %v", r.Words)
	}
	if r := list(&dictionaryv1.ListSavedWordsRequest{Uncategorized: true}); len(r.Words) != 1 || r.Words[0].Word != "gamma" || r.Total != 1 {
		t.Errorf("uncategorized: %v", r.Words)
	}
	r = list(&dictionaryv1.ListSavedWordsRequest{})
	if r.Total != 3 {
		t.Errorf("all: %v", r.Total)
	}
	for _, w := range r.Words {
		if w.Word == "beta" && len(w.CategoryIds) != 2 {
			t.Errorf("beta categories: %v", w.CategoryIds)
		}
	}
}

func TestResaveKeepsCategoriesAndSaveAdds(t *testing.T) {
	s, _ := categoryService(t, "alpha")
	a, b := mustCategory(t, s, "A"), mustCategory(t, s, "B")
	assign(t, s, a, "alpha")
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "alpha", Content: "new", CategoryIds: []string{b}}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{})
	if got := r.Words[0].CategoryIds; len(got) != 2 {
		t.Errorf("categories after re-save: %v", got)
	}
	if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: "alpha", CategoryIds: []string{"unknown"}}); code(err) != codes.NotFound {
		t.Errorf("unknown category on save: %v", err)
	}
}

func TestUpdateWordCategoriesAddsAndRemoves(t *testing.T) {
	s, _ := categoryService(t, "alpha", "beta")
	a, b := mustCategory(t, s, "A"), mustCategory(t, s, "B")
	assign(t, s, a, "alpha", "beta")
	r, err := s.UpdateWordCategories(userCtx(), &dictionaryv1.UpdateWordCategoriesRequest{
		Words: []string{"alpha", "beta", "ghost"}, AddCategoryIds: []string{b}, RemoveCategoryIds: []string{a}})
	if err != nil || r.Updated != 2 {
		t.Fatalf("update: %v %v", r, err)
	}
	l, _ := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{CategoryId: b})
	if len(l.Words) != 2 {
		t.Errorf("B: %v", l.Words)
	}
	if l, _ := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{CategoryId: a}); len(l.Words) != 0 {
		t.Errorf("A: %v", l.Words)
	}
	if _, err := s.UpdateWordCategories(userCtx(), &dictionaryv1.UpdateWordCategoriesRequest{Words: []string{"alpha"}, AddCategoryIds: []string{"nope"}}); code(err) != codes.NotFound {
		t.Errorf("unknown category: %v", err)
	}
	if _, err := s.UpdateWordCategories(userCtx(), &dictionaryv1.UpdateWordCategoriesRequest{AddCategoryIds: []string{a}}); code(err) != codes.InvalidArgument {
		t.Errorf("no words: %v", err)
	}
}

func TestDeleteCategoryKeepsWords(t *testing.T) {
	s, st := categoryService(t, "alpha")
	a := mustCategory(t, s, "A")
	assign(t, s, a, "alpha")
	if _, err := s.DeleteCategory(userCtx(), &dictionaryv1.DeleteCategoryRequest{Id: a}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.rows["alpha"]; !ok {
		t.Error("the word was deleted with its category")
	}
	if _, err := s.DeleteCategory(userCtx(), &dictionaryv1.DeleteCategoryRequest{Id: a}); code(err) != codes.NotFound {
		t.Errorf("second delete: %v", err)
	}
}

func TestListCategoriesCounts(t *testing.T) {
	s, _ := categoryService(t, "alpha", "beta", "gamma")
	a := mustCategory(t, s, "A")
	assign(t, s, a, "alpha", "beta")
	r, err := s.ListCategories(userCtx(), &dictionaryv1.ListCategoriesRequest{})
	if err != nil || len(r.Categories) != 1 || r.Categories[0].Total != 2 || r.UncategorizedTotal != 1 {
		t.Errorf("%v %v", r, err)
	}
}

func exportWords(t *testing.T, s *Service, req *dictionaryv1.ExportAnkiRequest) (*dictionaryv1.ExportAnkiResponse, error) {
	t.Helper()
	return s.ExportAnki(userCtx(), req)
}

func TestExportByCategoryMarksOnlyThoseWords(t *testing.T) {
	s, st := categoryService(t, "alpha", "beta", "gamma")
	a := mustCategory(t, s, "Travel")
	assign(t, s, a, "alpha", "beta")
	r, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{CategoryId: a})
	if err != nil || r.WordCount != 2 || r.Filename != "Travel.apkg" {
		t.Fatalf("export: %v %v", r, err)
	}
	if _, ok := st.rows["gamma"]; !ok || st.rows["gamma"].Exported {
		t.Error("gamma must stay new")
	}
	if _, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{CategoryId: "00000000-0000-4000-8000-999999999999"}); code(err) != codes.NotFound {
		t.Errorf("unknown category: %v", err)
	}
	r, err = exportWords(t, s, &dictionaryv1.ExportAnkiRequest{Uncategorized: true})
	if err != nil || r.WordCount != 1 {
		t.Errorf("uncategorized: %v %v", r, err)
	}
}

func TestExportSelectedWords(t *testing.T) {
	s, st := categoryService(t, "alpha", "beta", "gamma")
	st.rows["alpha"] = func() store.SavedWord { w := st.rows["alpha"]; w.Exported = true; return w }()
	r, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{Words: []string{"alpha", "gamma", "ghost", "alpha"}, Scope: dictionaryv1.ExportScope_EXPORT_SCOPE_NEW})
	if err != nil || r.WordCount != 2 || r.Filename != "My Words.apkg" {
		t.Fatalf("selected: %v %v", r, err)
	}
	if _, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{Words: []string{"ghost"}}); code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), CodeNoWords) {
		t.Errorf("only unknown words: %v", err)
	}
}

func TestExportAllIsUnchanged(t *testing.T) {
	s, _ := categoryService(t, "alpha", "beta")
	a := mustCategory(t, s, "A")
	assign(t, s, a, "alpha")
	r, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{})
	if err != nil || r.WordCount != 2 {
		t.Errorf("all: %v %v", r, err)
	}
}

func TestReviewQueueByCategory(t *testing.T) {
	st := newFakeStore()
	s := reviewService(st)
	for _, w := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.SaveWord(userCtx(), &dictionaryv1.SaveWordRequest{Word: w, Content: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	a := mustCategory(t, s, "A")
	assign(t, s, a, "alpha", "beta")
	r, err := s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{CategoryId: a})
	if err != nil || r.TotalCount != 2 || r.NewCount != 2 || len(r.Cards) != 2 {
		t.Fatalf("A: %v %v", r, err)
	}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Uncategorized: true})
	if r.TotalCount != 1 || len(r.Cards) != 1 || r.Cards[0].Word != "gamma" {
		t.Errorf("uncategorized: %v", r)
	}
}

func TestCategoryLimitsFollowSettings(t *testing.T) {
	s, st := categoryService(t)
	st.settings = map[string]json.RawMessage{"max_categories": json.RawMessage(`2`), "max_category_name_length": json.RawMessage(`5`)}
	if _, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: "toolong"}); code(err) != codes.InvalidArgument {
		t.Errorf("long name: %v", err)
	}
	a := mustCategory(t, s, "short")
	mustCategory(t, s, "b")
	if _, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: "c"}); code(err) != codes.ResourceExhausted {
		t.Errorf("limit: %v", err)
	}
	if _, err := s.RenameCategory(userCtx(), &dictionaryv1.RenameCategoryRequest{Id: a, Name: "sixsix"}); code(err) != codes.InvalidArgument {
		t.Errorf("rename: %v", err)
	}
	// the administrator relaxes both
	st.settings = map[string]json.RawMessage{"max_categories": json.RawMessage(`3`), "max_category_name_length": json.RawMessage(`10`)}
	if _, err := s.RenameCategory(userCtx(), &dictionaryv1.RenameCategoryRequest{Id: a, Name: "sixsix"}); err != nil {
		t.Errorf("rename after relax: %v", err)
	}
	if _, err := s.CreateCategory(userCtx(), &dictionaryv1.CreateCategoryRequest{Name: "c"}); err != nil {
		t.Errorf("create after relax: %v", err)
	}
}

func TestListCategoriesCacheIsDroppedByWrites(t *testing.T) {
	s, st := categoryService(t, "alpha", "beta")
	s.WithCategoryCache(time.Minute, 10)
	a := mustCategory(t, s, "A")
	list := func() *dictionaryv1.ListCategoriesResponse {
		t.Helper()
		r, err := s.ListCategories(userCtx(), &dictionaryv1.ListCategoriesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	list()
	list()
	if st.listCalls != 1 {
		t.Fatalf("the second list must come from the cache: %d store calls", st.listCalls)
	}
	assign(t, s, a, "alpha") // a write on this instance drops it
	if r := list(); st.listCalls != 2 || r.Categories[0].Total != 1 || r.UncategorizedTotal != 1 {
		t.Errorf("after assign: calls=%d %v", st.listCalls, r)
	}
	if _, err := s.RenameCategory(userCtx(), &dictionaryv1.RenameCategoryRequest{Id: a, Name: "B"}); err != nil {
		t.Fatal(err)
	}
	if r := list(); st.listCalls != 3 || r.Categories[0].Name != "B" {
		t.Errorf("after rename: calls=%d %v", st.listCalls, r)
	}
}

func TestSyncExportRefusesALongList(t *testing.T) {
	s, st := categoryService(t, "alpha", "beta", "gamma")
	s.cfg.ExportSyncMax = 2
	_, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{})
	if code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeExportUseJob) || !strings.Contains(err.Error(), "3 words") {
		t.Fatalf("3 words, limit 2: %v", err)
	}
	for _, w := range st.rows {
		if w.Exported {
			t.Error("a refused export marked a word exported")
		}
	}
	s.cfg.ExportSyncMax = 3
	if r, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{}); err != nil || r.WordCount != 3 {
		t.Errorf("at the limit: %v %v", r, err)
	}
}

func TestBatchOfWordsIsBounded(t *testing.T) {
	s, _ := categoryService(t, "alpha")
	a := mustCategory(t, s, "A")
	many := make([]string, maxBatchWords+1)
	for i := range many {
		many[i] = fmt.Sprintf("w%d", i)
	}
	if _, err := s.UpdateWordCategories(userCtx(), &dictionaryv1.UpdateWordCategoriesRequest{Words: many, AddCategoryIds: []string{a}}); code(err) != codes.InvalidArgument {
		t.Errorf("UpdateWordCategories with %d words: %v", len(many), err)
	}
	if _, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{Words: many}); code(err) != codes.InvalidArgument {
		t.Errorf("export of %d selected words: %v", len(many), err)
	}
}

func TestCheckSavedWords(t *testing.T) {
	s, _ := categoryService(t, "alpha", "beta")
	a := mustCategory(t, s, "A")
	assign(t, s, a, "alpha")
	r, err := s.CheckSavedWords(userCtx(), &dictionaryv1.CheckSavedWordsRequest{Words: []string{" Beta ", "ALPHA", "ghost", "alpha", "not a word!!"}})
	if err != nil || len(r.Words) != 2 {
		t.Fatalf("check: %v %v", r, err)
	}
	if r.Words[0].Word != "beta" || len(r.Words[0].CategoryIds) != 0 || r.Words[1].Word != "alpha" || len(r.Words[1].CategoryIds) != 1 || r.Words[1].CategoryIds[0] != a {
		t.Errorf("words: %v", r.Words)
	}
	many := make([]string, maxCheckWords+1)
	for i := range many {
		many[i] = fmt.Sprintf("w%d", i)
	}
	if _, err := s.CheckSavedWords(userCtx(), &dictionaryv1.CheckSavedWordsRequest{Words: many}); code(err) != codes.InvalidArgument {
		t.Errorf("too many words: %v", err)
	}
}

// the system setting of the administrators wins over the default of the configuration
func TestSyncExportLimitIsASystemSetting(t *testing.T) {
	s, st := categoryService(t, "alpha", "beta", "gamma")
	s.cfg.ExportSyncMax = 5
	st.settings = map[string]json.RawMessage{"export_sync_max_words": json.RawMessage(`2`)}
	_, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{})
	if code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeExportUseJob) || !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("3 words, setting 2, configuration 5: %v", err)
	}
	st.settings["export_sync_max_words"] = json.RawMessage(`3`)
	if r, err := exportWords(t, s, &dictionaryv1.ExportAnkiRequest{}); err != nil || r.WordCount != 3 {
		t.Errorf("at the setting: %v %v", r, err)
	}
}
