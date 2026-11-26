package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/huponx/dictionary/internal/srs"
)

func mustCat(t *testing.T, s *Store, user, name string) string {
	t.Helper()
	c, err := s.CreateCategory(context.Background(), user, name, 50)
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return c.ID
}

func wordsOf(ws []SavedWord) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Word)
	}
	slices.Sort(out)
	return out
}

func TestCategoryCRUDAndLimit(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a := mustCat(t, s, user, "Travel")
	if _, err := s.CreateCategory(ctx, user, "TRAVEL", 50); !errors.Is(err, ErrCategoryExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.CreateCategory(ctx, user, "Other", 1); !errors.Is(err, ErrLimit) {
		t.Errorf("limit: %v", err)
	}
	b := mustCat(t, s, user, "Food")
	if _, err := s.RenameCategory(ctx, user, b, "travel"); !errors.Is(err, ErrCategoryExists) {
		t.Errorf("rename to a used name: %v", err)
	}
	if c, err := s.RenameCategory(ctx, user, b, "Cooking"); err != nil || c.Name != "Cooking" {
		t.Errorf("rename: %v %v", c, err)
	}
	if _, err := s.RenameCategory(ctx, user, "not-a-uuid", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename unknown: %v", err)
	}
	res, err := s.ListCategories(ctx, user)
	cats := res.List
	if err != nil || len(cats) != 2 || cats[0].Name != "Cooking" || cats[1].ID != a {
		t.Errorf("list: %v %v", cats, err)
	}
	if err := s.DeleteCategory(ctx, user, a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCategory(ctx, user, a); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestCategoriesOfAnotherUserAreNotUsable(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	_, other := testStore(t)
	foreign := mustCat(t, s, other, "Theirs")
	saveAll(t, s, user, "alpha")
	if _, err := s.SetWordCategories(ctx, user, []string{"alpha"}, []string{foreign}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("assign foreign: %v", err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "beta"}, 100, testQuota, []string{foreign}); !errors.Is(err, ErrNotFound) {
		t.Errorf("save with foreign: %v", err)
	}
	if err := s.DeleteCategory(ctx, user, foreign); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete foreign: %v", err)
	}
	// the failed save left no word behind
	if total, _, _, _ := s.CountPending(ctx, user, WordFilter{}); total != 1 {
		t.Errorf("total = %d", total)
	}
}

func TestResaveKeepsCategoriesAndFilters(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a, b := mustCat(t, s, user, "A"), mustCat(t, s, user, "B")
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: "1"}, 100, testQuota, []string{a}); err != nil {
		t.Fatal(err)
	}
	saveAll(t, s, user, "beta", "gamma")
	if _, err := s.SetWordCategories(ctx, user, []string{"beta", "ghost"}, []string{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.SetWordCategories(ctx, user, []string{"beta", "ghost"}, nil, []string{b}); n != 1 {
		t.Errorf("updated = %d (ghost is not in the list)", n)
	}
	// re-saving alpha with the card changed and another category adds, it never removes
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: "2"}, 100, testQuota, []string{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: "3"}, 100, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range all {
		if w.Word == "alpha" && len(w.CategoryIDs) != 2 {
			t.Errorf("alpha categories: %v", w.CategoryIDs)
		}
	}
	for name, c := range map[string]struct {
		f    WordFilter
		want []string
	}{
		"A":             {WordFilter{CategoryID: a}, []string{"alpha", "beta"}},
		"B":             {WordFilter{CategoryID: b}, []string{"alpha"}},
		"uncategorized": {WordFilter{Uncategorized: true}, []string{"gamma"}},
		"unknown":       {WordFilter{CategoryID: "nope"}, nil},
		"all":           {WordFilter{}, []string{"alpha", "beta", "gamma"}},
	} {
		got, err := s.ListPending(ctx, user, time.Time{}, "", "", c.f, 10)
		if err != nil || !slices.Equal(wordsOf(got), c.want) {
			t.Errorf("ListPending %s: %v %v", name, wordsOf(got), err)
		}
		p, err := s.Pending(ctx, user, ScopeAll, c.f, nil, 0)
		if err != nil || !slices.Equal(wordsOf(p), c.want) {
			t.Errorf("Pending %s: %v %v", name, wordsOf(p), err)
		}
		if total, _, _, err := s.CountPending(ctx, user, c.f); err != nil || total != len(c.want) {
			t.Errorf("CountPending %s: %d %v", name, total, err)
		}
	}
	// the page keyset works together with the filter
	p1, _ := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{CategoryID: a}, 1)
	p2, _ := s.ListPending(ctx, user, p1[0].CreatedAt, p1[0].Word, "", WordFilter{CategoryID: a}, 5)
	if len(p1) != 1 || len(p2) != 1 || p1[0].Word == p2[0].Word {
		t.Errorf("pages: %v %v", p1, p2)
	}
}

func TestPendingOfSelectedWords(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	saveAll(t, s, user, "alpha", "beta", "gamma")
	if _, err := s.MarkExported(ctx, user, []string{"alpha"}, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Pending(ctx, user, ScopeNew, WordFilter{}, []string{"alpha", "gamma", "ghost"}, 0)
	if err != nil || !slices.Equal(wordsOf(got), []string{"alpha", "gamma"}) {
		t.Errorf("selected: %v %v", wordsOf(got), err)
	}
}

func TestDeleteCategoryKeepsWordsAndDeleteWordCleansLinks(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a := mustCat(t, s, user, "A")
	saveAll(t, s, user, "alpha", "beta")
	if _, err := s.SetWordCategories(ctx, user, []string{"alpha", "beta"}, []string{a}, nil); err != nil {
		t.Fatal(err)
	}
	res, _ := s.ListCategories(ctx, user)
	cats, uncat := res.List, res.UncatTotal
	if cats[0].Total != 2 || uncat != 0 {
		t.Errorf("counts: %v %d", cats, uncat)
	}
	if err := s.DeletePending(ctx, user, "alpha"); err != nil {
		t.Fatal(err)
	}
	var links int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_word_categories WHERE user_id = $1`, user).Scan(&links); err != nil || links != 1 {
		t.Errorf("links after the word was deleted: %d %v", links, err)
	}
	if err := s.DeleteCategory(ctx, user, a); err != nil {
		t.Fatal(err)
	}
	if total, _, _, _ := s.CountPending(ctx, user, WordFilter{Uncategorized: true}); total != 1 {
		t.Errorf("beta must stay, uncategorized now: %d", total)
	}
}

func TestExportOfACategoryMarksOnlyItsWords(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a := mustCat(t, s, user, "A")
	saveAll(t, s, user, "alpha", "beta", "gamma")
	if _, err := s.SetWordCategories(ctx, user, []string{"alpha", "beta"}, []string{a}, nil); err != nil {
		t.Fatal(err)
	}
	pend, err := s.Pending(ctx, user, ScopeNew, WordFilter{CategoryID: a}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExported(ctx, user, []string{pend[0].Word, pend[1].Word}, false); err != nil {
		t.Fatal(err)
	}
	if total, fresh, _, _ := s.CountPending(ctx, user, WordFilter{}); total != 3 || fresh != 1 {
		t.Errorf("total=%d fresh=%d", total, fresh)
	}
	res, _ := s.ListCategories(ctx, user)
	cats, uncatNew := res.List, res.UncatNew
	if cats[0].Total != 2 || cats[0].New != 0 || uncatNew != 1 {
		t.Errorf("counts: %+v uncatNew=%d", cats, uncatNew)
	}
}

func TestReviewByCategoryKeepsDailyLimitGlobal(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := mustCat(t, s, user, "A")
	saveAll(t, s, user, "alpha", "beta", "gamma")
	if _, err := s.SetWordCategories(ctx, user, []string{"alpha", "beta"}, []string{a}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutReview(ctx, user, "gamma", now, srs.Good, grade(srs.Good, now)); err != nil { // uncategorized, reviewed
		t.Fatal(err)
	}
	f := WordFilter{CategoryID: a}
	cnt, err := s.ReviewCounts(ctx, user, now, f)
	if err != nil || cnt.Total != 2 || cnt.New != 2 || cnt.Due != 0 {
		t.Errorf("counts A: %+v %v", cnt, err)
	}
	q, err := s.ReviewQueue(ctx, user, now, f, 10, 10, 10)
	if err != nil || !slices.Equal(words(q), []string{"alpha", "beta"}) {
		t.Errorf("queue A: %v %v", words(q), err)
	}
	cnt, _ = s.ReviewCounts(ctx, user, now, WordFilter{Uncategorized: true})
	if cnt.Total != 1 || cnt.New != 0 {
		t.Errorf("counts uncategorized: %+v", cnt)
	}
	q, _ = s.ReviewQueue(ctx, user, now.Add(48*time.Hour), WordFilter{Uncategorized: true}, 10, 10, 10)
	if !slices.Equal(words(q), []string{"gamma"}) {
		t.Errorf("queue uncategorized: %v", words(q))
	}
	// the card reviewed in no category still counts against the limit of new words of the whole list
	if n, _, err := s.ReviewToday(ctx, user, now); err != nil || n != 1 {
		t.Errorf("ReviewToday = %d %v", n, err)
	}
}

// categorizedOK checks the flag of every word of the user against its links.
func categorizedOK(t *testing.T, s *Store, user string) {
	t.Helper()
	var bad int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM dictionary_saved_words w WHERE w.user_id = $1 AND
		w.categorized <> EXISTS (SELECT 1 FROM dictionary_word_categories wc WHERE wc.user_id = w.user_id AND wc.word = w.word)`, user).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("%d words have a categorized flag that does not match their links", bad)
	}
}

func TestCategorizedFlagFollowsLinks(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a, _ := s.CreateCategory(ctx, user, "A", 10)
	b, _ := s.CreateCategory(ctx, user, "B", 10)
	for _, w := range []string{"x", "y", "z"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 100, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "w"}, 100, testQuota, []string{a.ID}); err != nil { // linked when saved
		t.Fatal(err)
	}
	categorizedOK(t, s, user)
	if total, _, _, _ := s.CountPending(ctx, user, WordFilter{Uncategorized: true}); total != 3 {
		t.Errorf("uncategorized = %d, want 3", total)
	}
	if _, err := s.SetWordCategories(ctx, user, []string{"x", "y"}, []string{a.ID, b.ID}, nil); err != nil {
		t.Fatal(err)
	}
	categorizedOK(t, s, user)
	// y leaves A but stays in B; x leaves both
	if _, err := s.SetWordCategories(ctx, user, []string{"y"}, nil, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWordCategories(ctx, user, []string{"x"}, nil, []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	categorizedOK(t, s, user)
	if total, _, _, _ := s.CountPending(ctx, user, WordFilter{Uncategorized: true}); total != 2 { // x, z
		t.Errorf("uncategorized = %d, want 2", total)
	}
	// saving a word again keeps its categories
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "w", Content: "again"}, 100, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	categorizedOK(t, s, user)
	if total, _, _, _ := s.CountPending(ctx, user, WordFilter{Uncategorized: true}); total != 2 {
		t.Errorf("uncategorized after re-save = %d, want 2", total)
	}
	// deleting a category frees the words that were only in it, in batches
	if err := s.DeleteCategory(ctx, user, b.ID); err != nil { // y was only in B
		t.Fatal(err)
	}
	categorizedOK(t, s, user)
	if total, _, _, _ := s.CountPending(ctx, user, WordFilter{Uncategorized: true}); total != 3 { // x, y, z
		t.Errorf("uncategorized after delete = %d, want 3", total)
	}
}

func TestDeleteCategoryInBatches(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	c, _ := s.CreateCategory(ctx, user, "Big", 10)
	n := unlinkBatch + 3
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_saved_words (user_id, word, content, categorized)
		SELECT $1, 'w' || g, '', true FROM generate_series(1, $2) g`, user, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_word_categories (user_id, word, category_id)
		SELECT $1, 'w' || g, $3::uuid FROM generate_series(1, $2) g`, user, n, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCategory(ctx, user, c.ID); err != nil {
		t.Fatal(err)
	}
	categorizedOK(t, s, user)
	var uncat int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1 AND NOT categorized`, user).Scan(&uncat); err != nil || uncat != n {
		t.Errorf("uncategorized = %d (%v), want %d", uncat, err, n)
	}
	if err := s.DeleteCategory(ctx, user, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestCountsAreCapped(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	c, _ := s.CreateCategory(ctx, user, "Big", 10)
	n := countCap + 5
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_saved_words (user_id, word, content, categorized)
		SELECT $1, 'w' || g, '', true FROM generate_series(1, $2) g`, user, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_word_categories (user_id, word, category_id)
		SELECT $1, 'w' || g, $3::uuid FROM generate_series(1, $2) g`, user, n, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id, listed) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET listed = EXCLUDED.listed`, user, n); err != nil {
		t.Fatal(err)
	}
	// no filter: the total is exact (the counter), the new count is capped
	total, fresh, capped, err := s.CountPending(ctx, user, WordFilter{})
	if err != nil || total != n || fresh != countCap || !capped {
		t.Errorf("unfiltered: total=%d fresh=%d capped=%v err=%v", total, fresh, capped, err)
	}
	total, fresh, capped, err = s.CountPending(ctx, user, WordFilter{CategoryID: c.ID})
	if err != nil || total != categoryCountCap || fresh != categoryCountCap || !capped {
		t.Errorf("category: total=%d fresh=%d capped=%v err=%v", total, fresh, capped, err)
	}
	res, err := s.ListCategories(ctx, user)
	if err != nil || !res.Capped || res.List[0].Total != categoryCountCap || res.List[0].New != categoryCountCap {
		t.Errorf("ListCategories: %+v %v", res, err)
	}
	total, _, capped, _ = s.CountPending(ctx, user, WordFilter{Uncategorized: true})
	if total != 0 || capped {
		t.Errorf("uncategorized: total=%d capped=%v", total, capped)
	}
}

// linksOK checks the links carry the created_at of their word (the category pages order by it).
func linksOK(t *testing.T, s *Store, user string) {
	t.Helper()
	var bad int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM dictionary_word_categories wc
		JOIN dictionary_saved_words w ON w.user_id = wc.user_id AND w.word = wc.word
		WHERE wc.user_id = $1 AND wc.created_at <> w.created_at`, user).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("%d links have a created_at that differs from their word", bad)
	}
}

func TestCategoryPagesFollowTheWords(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a, _ := s.CreateCategory(ctx, user, "A", 10)
	for _, w := range []string{"one", "two", "three"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 100, testQuota, []string{a.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "four"}, 100, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWordCategories(ctx, user, []string{"four"}, []string{a.ID}, nil); err != nil {
		t.Fatal(err)
	}
	linksOK(t, s, user)
	// saving a word again makes it the newest of the list and of its category
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "one", Content: "again"}, 100, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	linksOK(t, s, user)
	f := WordFilter{CategoryID: a.ID}
	page1, err := s.ListPending(ctx, user, time.Time{}, "", "", f, 2)
	if err != nil || len(page1) != 2 || page1[0].Word != "one" || page1[1].Word != "four" {
		t.Fatalf("page 1: %+v %v", page1, err)
	}
	page2, err := s.ListPending(ctx, user, page1[1].CreatedAt, page1[1].Word, "", f, 5)
	if err != nil || len(page2) != 2 || page2[0].Word != "three" || page2[1].Word != "two" {
		t.Fatalf("page 2: %+v %v", page2, err)
	}
	// the query and the export order (oldest first, with a key) work on the links too
	if got, _ := s.ListPending(ctx, user, time.Time{}, "", "T", f, 10); len(got) != 2 {
		t.Errorf("query t in the category: %+v", got)
	}
	oldest, _ := s.PendingPage(ctx, user, ScopeNew, f, time.Time{}, "", 2)
	if len(oldest) != 2 || oldest[0].Word != "two" || oldest[1].Word != "three" {
		t.Fatalf("oldest first: %+v", oldest)
	}
	next, _ := s.PendingPage(ctx, user, ScopeNew, f, oldest[1].CreatedAt, oldest[1].Word, 5)
	if len(next) != 2 || next[0].Word != "four" || next[1].Word != "one" {
		t.Errorf("after the key: %+v", next)
	}
	if n, err := s.CountExportable(ctx, user, ScopeNew, f); err != nil || n != 4 {
		t.Errorf("CountExportable: %d %v", n, err)
	}
	// a word that left the list is not in the page, though its links stay
	if _, err := s.MarkExported(ctx, user, []string{"two"}, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListPending(ctx, user, time.Time{}, "", "", f, 10); len(got) != 3 {
		t.Errorf("after removing one: %+v", got)
	}
}

func TestSavedByNames(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	a, _ := s.CreateCategory(ctx, user, "A", 10)
	for _, w := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 100, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetWordCategories(ctx, user, []string{"alpha"}, []string{a.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExported(ctx, user, []string{"gamma"}, true); err != nil { // left the list
		t.Fatal(err)
	}
	got, err := s.SavedByNames(ctx, user, []string{"alpha", "beta", "gamma", "ghost"})
	if err != nil || len(got) != 2 || len(got["alpha"]) != 1 || got["alpha"][0] != a.ID {
		t.Fatalf("SavedByNames: %v %v", got, err)
	}
	if ids, ok := got["beta"]; !ok || len(ids) != 0 {
		t.Errorf("beta: %v %v", ids, ok)
	}
	if got, err := s.SavedByNames(ctx, user, nil); err != nil || len(got) != 0 {
		t.Errorf("no words: %v %v", got, err)
	}
}
