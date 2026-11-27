package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/huponx/dictionary/internal/srs"
)

// The same word in two groups is two cards: saving, listing, deleting, exporting, the categories and the review of
// one never touch the other.
func TestGroupsAreSeparate(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)

	for _, g := range []string{"en", "x"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Group: g, Word: "hello", Content: "card of " + g}, 10, testQuota, nil); err != nil {
			t.Fatalf("save in %s: %v", g, err)
		}
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "x", Word: "only-x"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	// saving again overwrites inside the group only
	if n, err := s.SaveWord(ctx, user, SavedWord{Group: "x", Word: "hello", Content: "again"}, 10, testQuota, nil); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v: the quota counts the words of the user across the groups", n, err)
	}

	for g, want := range map[string][]string{"en": {"hello"}, "x": {"hello", "only-x"}, "none": nil} {
		got, err := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{Group: g}, 10)
		if err != nil || len(got) != len(want) {
			t.Fatalf("list %s: %+v %v", g, got, err)
		}
		for _, w := range got {
			if w.Group != g {
				t.Errorf("list %s returned a word of group %s", g, w.Group)
			}
		}
		total, fresh, _, err := s.CountPending(ctx, user, WordFilter{Group: g})
		if err != nil || total != len(want) || fresh != len(want) {
			t.Errorf("count %s = %d/%d, %v, want %d", g, total, fresh, err, len(want))
		}
	}
	en, _ := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{Group: "en"}, 10)
	if en[0].Content != "card of en" {
		t.Errorf("the card of en = %q, the save in x overwrote it", en[0].Content)
	}

	// the search and the check by names stay in the group
	if got, err := s.ListPending(ctx, user, time.Time{}, "", "only", WordFilter{Group: "en"}, 10); err != nil || len(got) != 0 {
		t.Errorf("search in en found %+v %v", got, err)
	}
	if got, err := s.SavedByNames(ctx, user, "en", []string{"hello", "only-x"}); err != nil || len(got) != 1 {
		t.Errorf("check in en: %v %v", got, err)
	}

	// categories: the links of one group, the counts of each group
	c, err := s.CreateCategory(ctx, user, "both", 10)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.SetWordCategories(ctx, user, "x", []string{"hello"}, []string{c.ID}, nil); err != nil || n != 1 {
		t.Fatalf("categorize: %d %v", n, err)
	}
	for g, want := range map[string]int{"en": 0, "x": 1} {
		res, err := s.ListCategories(ctx, user, g)
		if err != nil || len(res.List) != 1 || res.List[0].Total != want {
			t.Errorf("categories of %s: %+v %v, want %d", g, res, err, want)
		}
		got, err := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{Group: g, CategoryID: c.ID}, 10)
		if err != nil || len(got) != want {
			t.Errorf("category list %s: %+v %v", g, got, err)
		}
	}
	if res, _ := s.ListCategories(ctx, user, "en"); res.UncatTotal != 1 {
		t.Errorf("uncategorized in en = %d, want 1", res.UncatTotal)
	}

	// review state is per group
	now := time.Now()
	if _, err := s.PutReview(ctx, user, "x", "hello", now, srs.Good, func(cur srs.State) srs.State { cur.Reps++; cur.Due = now.Add(time.Hour); return cur }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutReview(ctx, user, "none", "hello", now, srs.Good, func(cur srs.State) srs.State { return cur }); !errors.Is(err, ErrNotFound) {
		t.Errorf("review in a group without the word: %v", err)
	}
	cx, err := s.ReviewCounts(ctx, user, now, WordFilter{Group: "x"})
	if err != nil || cx.Total != 2 || cx.New != 1 {
		t.Errorf("review counts x: %+v %v", cx, err)
	}
	cen, err := s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"})
	if err != nil || cen.Total != 1 || cen.New != 1 {
		t.Errorf("review counts en: %+v %v", cen, err)
	}
	if n, _, err := s.ReviewToday(ctx, user, "en", now); err != nil || n != 0 {
		t.Errorf("reviewed today in en = %d %v", n, err)
	}
	if n, _, err := s.ReviewToday(ctx, user, "x", now); err != nil || n != 1 {
		t.Errorf("reviewed today in x = %d %v", n, err)
	}
	q, err := s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 10, 10)
	if err != nil || len(q) != 1 || q[0].Group != "en" || !q[0].IsNew {
		t.Errorf("queue en: %+v %v", q, err)
	}

	// export and remove: one group
	if n, err := s.MarkExported(ctx, user, "x", []string{"hello", "only-x"}, true); err != nil || n != 2 {
		t.Fatalf("mark exported: %d %v", n, err)
	}
	if got, err := s.Pending(ctx, user, ScopeNew, WordFilter{Group: "en"}, nil, 0); err != nil || len(got) != 1 {
		t.Errorf("en must keep its word: %+v %v", got, err)
	}
	if n, err := s.ListedCount(ctx, user); err != nil || n != 1 {
		t.Errorf("listed = %d %v, the counter follows the removed words of the group", n, err)
	}

	// deleting one group's word leaves the other's
	if err := s.DeletePending(ctx, user, "none", "hello"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete in a group without the word: %v", err)
	}
	if err := s.DeletePending(ctx, user, "en", "hello"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RemoveExported(ctx, user, "x"); err != nil || n != 0 {
		t.Errorf("remove exported: %d %v", n, err)
	}
}

// With words in two groups the unfiltered total of a long list is capped like the other counts; with one group it
// is the exact counter of the user.
func TestCountPendingTotalAcrossGroups(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	n := reviewCountCap + 5
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_saved_words (user_id, grp, word, content)
		SELECT $1, 'en', 'w' || g, '' FROM generate_series(1, $2) g`, user, n); err != nil {
		t.Skipf("bulk insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id, listed) VALUES ($1, $2) ON CONFLICT (user_id) DO UPDATE SET listed = $2`, user, n); err != nil {
		t.Fatal(err)
	}
	total, _, capped, err := s.CountPending(ctx, user, WordFilter{Group: "en"})
	if err != nil || total != n || !capped {
		// the new words are capped, the total is exact (one group)
		if err != nil || total != n {
			t.Fatalf("one group: total=%d capped=%v err=%v, want %d", total, capped, err, n)
		}
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "x", Word: "other"}, n+10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	total, _, capped, err = s.CountPending(ctx, user, WordFilter{Group: "en"})
	if err != nil || total != reviewCountCap || !capped {
		t.Fatalf("two groups: total=%d capped=%v err=%v, want the cap", total, capped, err)
	}
	if total, _, _, err := s.CountPending(ctx, user, WordFilter{Group: "x"}); err != nil || total != 1 {
		t.Fatalf("group x: %d %v", total, err)
	}
}
