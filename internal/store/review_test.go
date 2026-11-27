package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/huponx/dictionary/internal/srs"
)

func saveAll(t *testing.T, s *Store, user string, words ...string) {
	t.Helper()
	for _, w := range words {
		if _, err := s.SaveWord(context.Background(), user, SavedWord{Group: "en", Word: w, Content: "<p>" + w + "</p>"}, 100, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func grade(g srs.Grade, now time.Time) func(srs.State) srs.State {
	return func(cur srs.State) srs.State { return srs.Next(cur, g, now) }
}

func words(cards []ReviewCard) []string {
	var out []string
	for _, c := range cards {
		out = append(out, c.Word)
	}
	return out
}

func TestReviewQueueDueThenNewAndExcludesRemoved(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "a", "b", "c", "d")

	// c and d are reviewed: c comes due before d, then both are in the future after one more grade of d
	if _, err := s.PutReview(ctx, user, "en", "c", now.Add(-48*time.Hour), srs.Good, grade(srs.Good, now.Add(-48*time.Hour))); err != nil { // due -24h
		t.Fatal(err)
	}
	if _, err := s.PutReview(ctx, user, "en", "d", now.Add(-48*time.Hour), srs.Again, grade(srs.Again, now.Add(-48*time.Hour))); err != nil { // due -47h50m
		t.Fatal(err)
	}
	q, err := s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 10, 10)
	if err != nil || len(q) != 4 || words(q)[0] != "d" || words(q)[1] != "c" || !q[2].IsNew || !q[3].IsNew || q[0].IsNew {
		t.Fatalf("%v %v", words(q), err)
	}
	if q[0].State.Ease != 2.5 || !q[0].State.Learning || q[1].State.IntervalDays != 1 {
		t.Fatalf("%+v %+v", q[0].State, q[1].State)
	}
	// limit and the allowance of new words
	if q, _ = s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 3, 3, 10); len(q) != 3 {
		t.Fatalf("%v", words(q))
	}
	if q, _ = s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 10, 1); len(q) != 3 || words(q)[2] != "a" {
		t.Fatalf("%v", words(q))
	}
	if q, _ = s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 10, 0); len(q) != 2 {
		t.Fatalf("%v", words(q))
	}
	// the allowance of due cards does not take the new words' place
	if q, _ = s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 1, 10); len(q) != 3 || words(q)[0] != "d" || !q[1].IsNew {
		t.Fatalf("%v", words(q))
	}
	if q, _ = s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 0, 10); len(q) != 2 || !q[0].IsNew || !q[1].IsNew {
		t.Fatalf("%v", words(q))
	}
	cnt, err := s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"})
	if err != nil || cnt.Total != 4 || cnt.Due != 1 || cnt.Learning != 1 || cnt.New != 2 || !cnt.Next.IsZero() {
		t.Fatalf("%+v %v", cnt, err)
	}

	// a word that left the list is not reviewed, counted or scheduled
	if _, err := s.MarkExported(ctx, user, "en", []string{"a", "c"}, true); err != nil {
		t.Fatal(err)
	}
	q, _ = s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 10, 10)
	if got := words(q); len(got) != 2 || got[0] != "d" || got[1] != "b" {
		t.Fatalf("%v", got)
	}
	if cnt, _ = s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"}); cnt.Total != 2 || cnt.Due != 0 || cnt.Learning != 1 || cnt.New != 1 {
		t.Fatalf("%+v", cnt)
	}
	if _, err := s.PutReview(ctx, user, "en", "a", now, srs.Good, grade(srs.Good, now)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a removed word: %v", err)
	}
	if _, err := s.PutReview(ctx, user, "en", "ghost", now, srs.Good, grade(srs.Good, now)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown word: %v", err)
	}

	// next due
	if _, err := s.PutReview(ctx, user, "en", "b", now, srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	cnt, _ = s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"})
	if want := now.Add(24 * time.Hour); !cnt.Next.Equal(want) {
		t.Fatalf("next %v, want %v", cnt.Next, want)
	}

	// a card graded Again again is learning: counted apart from the review cards once its step is due
	if _, err := s.PutReview(ctx, user, "en", "b", now, srs.Again, grade(srs.Again, now)); err != nil {
		t.Fatal(err)
	}
	soon := now.Add(11 * time.Minute)
	if cnt, _ = s.ReviewCounts(ctx, user, soon, WordFilter{Group: "en"}); cnt.Due != 0 || cnt.Learning != 2 || cnt.New != 0 {
		t.Fatalf("%+v", cnt)
	}
	if cnt, _ = s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"}); cnt.Learning != 1 {
		t.Fatalf("b is not due yet: %+v", cnt)
	}
}

func TestReviewStateFollowsTheWord(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "keep", "gone")
	for _, w := range []string{"keep", "gone"} {
		if _, err := s.PutReview(ctx, user, "en", w, now, srs.Good, grade(srs.Good, now)); err != nil {
			t.Fatal(err)
		}
	}
	// editing the card keeps the schedule
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: "keep", Content: "<p>edited</p>"}, 100, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_review_state WHERE user_id = $1 AND word = 'keep' AND interval_days = 1`, user).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schedule after an edit: %d %v", n, err)
	}
	// deleting the word (by hand, or by the cleanup after the retention) takes the schedule along
	if err := s.DeletePending(ctx, user, "en", "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExported(ctx, user, "en", []string{"keep"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRemovedBefore(ctx, time.Now().Add(time.Minute), 100); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_review_state WHERE user_id = $1`, user).Scan(&n); err != nil || n != 0 {
		t.Fatalf("schedules left: %d %v", n, err)
	}
}

func TestNewToday(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "a", "b", "c")
	if _, err := s.PutReview(ctx, user, "en", "a", now.Add(-25*time.Hour), srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutReview(ctx, user, "en", "b", now.Add(-23*time.Hour), srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	// grading b again is not a first review
	if _, err := s.PutReview(ctx, user, "en", "b", now, srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.ReviewToday(ctx, user, "en", now); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if n, _, _ := s.ReviewToday(ctx, user, "en", now.Add(2*time.Hour)); n != 0 {
		t.Fatalf("after the window: %d", n)
	}
}

func TestReviewsToday(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "a", "b", "c", "d")
	// a: first review 25h ago, again now (counts); b: first review 23h ago, again now (a new word, not counted)
	// c: first and last review 30h ago (outside the window)
	for _, p := range []struct {
		word string
		at   time.Time
	}{{"a", now.Add(-25 * time.Hour)}, {"a", now}, {"b", now.Add(-23 * time.Hour)}, {"b", now}, {"c", now.Add(-30 * time.Hour)}} {
		if _, err := s.PutReview(ctx, user, "en", p.word, p.at, srs.Good, grade(srs.Good, p.at)); err != nil {
			t.Fatal(err)
		}
	}
	if _, n, err := s.ReviewToday(ctx, user, "en", now); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	// a left the list
	if _, err := s.MarkExported(ctx, user, "en", []string{"a"}, true); err != nil {
		t.Fatal(err)
	}
	if _, n, _ := s.ReviewToday(ctx, user, "en", now); n != 0 {
		t.Fatalf("removed word counted: %d", n)
	}
}

func TestPutReviewParallelGradesApplyInTurn(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "w")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, g := range []srs.Grade{srs.Again, srs.Again} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutReview(ctx, user, "en", "w", now, g, grade(g, now))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	var learning bool
	if err := s.pool.QueryRow(ctx, `SELECT count(*), bool_and(learning) FROM dictionary_review_state WHERE user_id = $1`, user).Scan(&rows, &learning); err != nil || rows != 1 || !learning {
		t.Fatalf("rows=%d learning=%v err=%v", rows, learning, err)
	}
	// both applied one after the other on the same row: after Good then Good the second sees the first
	if _, err := s.PutReview(ctx, user, "en", "w", now, srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	st, err := s.PutReview(ctx, user, "en", "w", now, srs.Good, grade(srs.Good, now))
	if err != nil || st.IntervalDays != 3 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestReviewBonusAddsUpAndExpires(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if b, err := s.ReviewBonus(ctx, user, now); err != nil || b != (ReviewBonus{}) {
		t.Fatalf("no bonus: %+v %v", b, err)
	}
	b, err := s.AddReviewBonus(ctx, user, now, 3, 0)
	if err != nil || b.New != 3 || b.Reviews != 0 || !b.Expires.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("first: %+v %v", b, err)
	}
	// a grant while the bonus runs adds up and renews the 24 hours
	later := now.Add(2 * time.Hour)
	b, err = s.AddReviewBonus(ctx, user, later, 2, 5)
	if err != nil || b.New != 5 || b.Reviews != 5 || !b.Expires.Equal(later.Add(24*time.Hour)) {
		t.Fatalf("second: %+v %v", b, err)
	}
	if got, err := s.ReviewBonus(ctx, user, later); err != nil || got != b {
		t.Fatalf("read: %+v %v", got, err)
	}
	// each kind stops at 9999
	if b, err = s.AddReviewBonus(ctx, user, later, 9999, 0); err != nil || b.New != 9999 || b.Reviews != 5 {
		t.Fatalf("cap: %+v %v", b, err)
	}
	// expired: read as none, a new grant starts from 0
	after := later.Add(25 * time.Hour)
	if got, err := s.ReviewBonus(ctx, user, after); err != nil || got != (ReviewBonus{}) {
		t.Fatalf("expired: %+v %v", got, err)
	}
	if b, err = s.AddReviewBonus(ctx, user, after, 1, 1); err != nil || b.New != 1 || b.Reviews != 1 {
		t.Fatalf("restart: %+v %v", b, err)
	}
}

func TestListPendingCarriesReviewState(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "fresh", "learn", "graded")
	if _, err := s.PutReview(ctx, user, "en", "learn", now, srs.Again, grade(srs.Again, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutReview(ctx, user, "en", "graded", now, srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{Group: "en"}, 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("%v %v", list, err)
	}
	got := map[string]SavedWord{}
	for _, w := range list {
		got[w.Word] = w
	}
	if w := got["fresh"]; !w.NextReview.IsZero() || w.Learning {
		t.Fatalf("new: %+v", w)
	}
	if w := got["learn"]; !w.Learning || !w.NextReview.After(now) {
		t.Fatalf("learning: %+v", w)
	}
	if w := got["graded"]; w.Learning || !w.NextReview.After(now.Add(12*time.Hour)) {
		t.Fatalf("review: %+v", w)
	}
}

func TestReviewStartedFlagAndNewOrder(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "b", "a", "c")
	started := func(w string) (b bool) {
		if err := s.pool.QueryRow(ctx, `SELECT review_started FROM dictionary_saved_words WHERE user_id = $1 AND word = $2`, user, w).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if started("a") {
		t.Fatal("a new word is started")
	}
	if _, err := s.PutReview(ctx, user, "en", "a", now, srs.Good, grade(srs.Good, now)); err != nil {
		t.Fatal(err)
	}
	if !started("a") || started("b") {
		t.Fatal("flag not set on the graded word only")
	}
	q, err := s.ReviewQueue(ctx, user, now, WordFilter{Group: "en"}, 10, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := words(q); len(got) != 2 || got[0] != "b" || got[1] != "c" { // oldest first (saved b, a, c)
		t.Fatalf("new cards: %v", got)
	}
	// export and save again: the review state and the flag stay with the word
	if _, err := s.MarkExported(ctx, user, "en", []string{"a"}, true); err != nil {
		t.Fatal(err)
	}
	saveAll(t, s, user, "a")
	if !started("a") {
		t.Fatal("flag lost on save again")
	}
	if cnt, _ := s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"}); cnt.New != 2 || cnt.Total != 3 || cnt.Capped {
		t.Fatalf("%+v", cnt)
	}
}

func TestPutReviewLogsEachGradeAndStoresFSRSMemory(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	saveAll(t, s, user, "w")
	sc := srs.Scheduler{Algo: srs.FSRS}
	t1, t2 := now.Add(-48*time.Hour), now
	for _, p := range []struct {
		at time.Time
		g  srs.Grade
	}{{t1, srs.Good}, {t2, srs.Again}} {
		if _, err := s.PutReview(ctx, user, "en", "w", p.at, p.g, func(c srs.State) srs.State { return sc.Next(c, p.g, p.at) }); err != nil {
			t.Fatal(err)
		}
	}
	cards, err := s.ReviewLogs(ctx, user, 100)
	if err != nil || len(cards) != 1 || len(cards[0]) != 2 {
		t.Fatalf("%v %v", cards, err)
	}
	if r := cards[0][0]; r.Grade != srs.Good || r.Elapsed != 0 {
		t.Fatalf("first: %+v", r)
	}
	if r := cards[0][1]; r.Grade != srs.Again || r.Elapsed != 48*3600 {
		t.Fatalf("second: %+v", r)
	}
	var stab, diff float32
	if err := s.pool.QueryRow(ctx, `SELECT stability, difficulty FROM dictionary_review_state WHERE user_id = $1 AND word = 'w'`, user).Scan(&stab, &diff); err != nil || stab <= 0 || diff < 1 {
		t.Fatalf("%v %v %v", stab, diff, err)
	}
	// the log goes with the word
	if _, err := s.pool.Exec(ctx, `DELETE FROM dictionary_saved_words WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if cards, _ := s.ReviewLogs(ctx, user, 100); len(cards) != 0 {
		t.Fatalf("log left: %v", cards)
	}
}

func TestReviewCountsAreCapped(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_saved_words (user_id, grp, word, content)
		SELECT $1, 'en', 'w' || g, '' FROM generate_series(1, $2) g`, user, reviewCountCap+5); err != nil {
		t.Skipf("bulk insert: %v", err)
	}
	cnt, err := s.ReviewCounts(ctx, user, now, WordFilter{Group: "en"})
	if err != nil || !cnt.Capped || cnt.Total != reviewCountCap || cnt.New != reviewCountCap {
		t.Fatalf("%+v %v", cnt, err)
	}
}

func TestFSRSParamsRoundTripAndClaim(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if p, err := s.FSRSParams(ctx, user); p != nil || err != nil {
		t.Fatalf("%v %v", p, err)
	}
	if ok, err := s.ClaimOptimize(ctx, user, now, time.Minute); !ok || err != nil {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, _ := s.ClaimOptimize(ctx, user, now.Add(30*time.Second), time.Minute); ok {
		t.Fatal("claimed within the minute")
	}
	if ok, _ := s.ClaimOptimize(ctx, user, now.Add(61*time.Second), time.Minute); !ok {
		t.Fatal("not claimed after the minute")
	}
	// a claim alone stores no weights
	if p, _ := s.FSRSParams(ctx, user); p != nil {
		t.Fatalf("%+v", p)
	}
	w := srs.DefaultWeights
	w[2] = 3.5
	if err := s.PutFSRSParams(ctx, user, FSRSParams{Weights: &w, ReviewCount: 321, LossDefault: 0.5, LossTrained: 0.4, TrainedAt: now}); err != nil {
		t.Fatal(err)
	}
	p, err := s.FSRSParams(ctx, user)
	if err != nil || p == nil || p.ReviewCount != 321 || p.Weights[2] != 3.5 || !p.TrainedAt.Equal(now) {
		t.Fatalf("%+v %v", p, err)
	}
	if err := s.DeleteFSRSParams(ctx, user); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.FSRSParams(ctx, user); p != nil {
		t.Fatal("not deleted")
	}
}

func TestLockOptimizeIsExclusivePerUser(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	unlock, ok, err := s.LockOptimize(ctx, user)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, ok, _ := s.LockOptimize(ctx, user); ok {
		t.Fatal("second lock")
	}
	unlock()
	unlock2, ok, _ := s.LockOptimize(ctx, user)
	if !ok {
		t.Fatal("lock not released")
	}
	unlock2()
}
