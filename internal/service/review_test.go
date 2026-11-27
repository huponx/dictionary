package service

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/huponx/dictionary/internal/srs"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/authn"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var reviewNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type fakeReview struct {
	state srsState
	first time.Time
	last  time.Time
}

type srsState = srs.State

// The fake store knows one user (userUUID): the rows of another one are empty.
func (f *fakeStore) reviewQueue(_ context.Context, user string, now time.Time, flt store.WordFilter, limit, dueAllowed, newAllowed int) ([]store.ReviewCard, error) {
	if user != userUUID {
		return nil, nil
	}
	var due, fresh []store.ReviewCard
	for _, w := range f.sorted() {
		if !f.match(w.Word, flt) {
			continue
		}
		r, ok := f.review[w.Word]
		c := store.ReviewCard{Group: w.Group, Word: w.Word, Content: w.Content, AudioUK: w.AudioUK, AudioUS: w.AudioUS, State: r.state}
		switch {
		case !ok:
			c.IsNew, c.State = true, srs.New()
			fresh = append(fresh, c)
		case !r.state.Due.After(now):
			due = append(due, c)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].State.Due.Before(due[j].State.Due) })
	out := due[:min(min(limit, dueAllowed), len(due))]
	if n := min(limit-len(out), newAllowed); n > 0 {
		out = append(out, fresh[:min(n, len(fresh))]...)
	}
	return out, nil
}

func (f *fakeStore) reviewCounts(_ context.Context, user string, now time.Time, flt store.WordFilter) (store.ReviewCounts, error) {
	var c store.ReviewCounts
	if user != userUUID {
		return c, nil
	}
	for w := range f.rows {
		if !f.match(w, flt) {
			continue
		}
		c.Total++
		r, ok := f.review[w]
		switch {
		case !ok:
			c.New++
		case !r.state.Due.After(now) && r.state.Learning:
			c.Learning++
		case !r.state.Due.After(now):
			c.Due++
		case c.Next.IsZero() || r.state.Due.Before(c.Next):
			c.Next = r.state.Due
		}
	}
	return c, nil
}

func (f *fakeStore) reviewToday(_ context.Context, _ string, now time.Time) (int, int, error) {
	since, newWords, again := now.Add(-24*time.Hour), 0, 0
	for w, r := range f.review {
		if _, listed := f.rows[w]; !listed {
			continue
		}
		switch {
		case r.first.After(since):
			newWords++
		case r.last.After(since):
			again++
		}
	}
	return newWords, again, nil
}

func (f *fakeStore) ReviewBonus(_ context.Context, _ string, now time.Time) (store.ReviewBonus, error) {
	if !f.bonus.Expires.After(now) {
		return store.ReviewBonus{}, nil
	}
	return f.bonus, nil
}

func (f *fakeStore) AddReviewBonus(_ context.Context, _ string, now time.Time, newWords, reviews int) (store.ReviewBonus, error) {
	if !f.bonus.Expires.After(now) {
		f.bonus = store.ReviewBonus{}
	}
	f.bonus = store.ReviewBonus{New: min(f.bonus.New+newWords, 9999), Reviews: min(f.bonus.Reviews+reviews, 9999), Expires: now.Add(24 * time.Hour)}
	return f.bonus, nil
}

func (f *fakeStore) putReview(_ context.Context, user, word string, now time.Time, g srs.Grade, next func(srs.State) srs.State) (srs.State, error) {
	if _, ok := f.rows[word]; !ok || user != userUUID {
		return srs.State{}, store.ErrNotFound
	}
	if f.review == nil {
		f.review = map[string]fakeReview{}
	}
	r, ok := f.review[word]
	if !ok {
		r = fakeReview{state: srs.New(), first: now}
	}
	var elapsed int64
	if !r.last.IsZero() {
		elapsed = int64(now.Sub(r.last).Seconds())
	}
	r.state, r.last = next(r.state), now
	r.state.Last = now
	f.review[word] = r
	f.log = append(f.log, loggedGrade{word: word, grade: g, elapsed: elapsed})
	return r.state, nil
}

type loggedGrade struct {
	word    string
	grade   srs.Grade
	elapsed int64
}

func (f *fakeStore) FSRSParams(context.Context, string) (*store.FSRSParams, error) {
	return f.params, nil
}

func (f *fakeStore) PutFSRSParams(_ context.Context, _ string, p store.FSRSParams) error {
	f.params = &p
	return nil
}

func (f *fakeStore) DeleteFSRSParams(context.Context, string) error {
	f.params = nil
	return nil
}

func (f *fakeStore) ClaimOptimize(_ context.Context, _ string, now time.Time, gap time.Duration) (bool, error) {
	if !f.lastClaim.IsZero() && now.Sub(f.lastClaim) < gap {
		return false, nil
	}
	f.lastClaim = now
	return true, nil
}

func (f *fakeStore) LockOptimize(context.Context, string) (func(), bool, error) {
	f.lockMu.Lock()
	defer f.lockMu.Unlock()
	if f.locked {
		return nil, false, nil
	}
	f.locked = true
	return func() { f.lockMu.Lock(); f.locked = false; f.lockMu.Unlock() }, true, nil
}

func (f *fakeStore) ReviewLogs(context.Context, string, int) ([][]srs.Review, error) {
	return f.logs, nil
}

func reviewService(st *fakeStore) *Service {
	s := newService(st, &fakeDecks{})
	s.now = func() time.Time { return reviewNow }
	return s
}

func listWords(t *testing.T, st *fakeStore, words ...string) {
	t.Helper()
	for _, w := range words {
		if _, err := st.SaveWord(context.Background(), userUUID, store.SavedWord{Word: w, Content: "<p>" + w + "</p>"}, 100, 1<<30, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func cardWords(r *dictionaryv1.GetReviewQueueResponse) []string {
	var out []string
	for _, c := range r.Cards {
		out = append(out, c.Word)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReviewQueueOrderDueBeforeNew(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "a", "b", "c", "d", "e")
	st.review = map[string]fakeReview{
		"b": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-time.Hour)}, first: reviewNow.Add(-72 * time.Hour)},
		"d": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-2 * time.Hour)}, first: reviewNow.Add(-72 * time.Hour)},
		"e": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(5 * time.Hour)}, first: reviewNow.Add(-72 * time.Hour)},
	}
	r, err := reviewService(st).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cardWords(r); !equal(got, []string{"d", "b", "a", "c"}) {
		t.Fatalf("order %v", got)
	}
	if r.DueCount != 2 || r.LearningCount != 0 || r.NewCount != 2 || r.TotalCount != 5 || r.NextDueAt != reviewNow.Add(5*time.Hour).Format(time.RFC3339) {
		t.Fatalf("%+v", r)
	}
	if r.Cards[0].IsNew || !r.Cards[2].IsNew || len(r.Cards[0].GradeIntervalsSeconds) != 4 {
		t.Fatalf("%+v", r.Cards)
	}
	// a new card: Again 1 minute, Hard 10 minutes, Good 1 day, Easy 4 days
	if got := r.Cards[2].GradeIntervalsSeconds; got[0] != 60 || got[1] != 600 || got[2] != 86400 || got[3] != 4*86400 {
		t.Fatalf("%v", got)
	}
}

func TestReviewQueueLimit(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "a", "b", "c")
	st.review = map[string]fakeReview{"c": {state: srs.State{Ease: 2.5, IntervalDays: 1, Due: reviewNow}, first: reviewNow.Add(-48 * time.Hour)}}
	r, err := reviewService(st).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en", Limit: 2})
	if err != nil || !equal(cardWords(r), []string{"c", "a"}) || r.NewCount != 2 || r.DueCount != 1 {
		t.Fatalf("%v %+v %v", cardWords(r), r, err)
	}
	if _, err := reviewService(st).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en", Limit: -1}); code(err) != codes.InvalidArgument {
		t.Fatalf("negative limit: %v", err)
	}
}

func TestReviewQueueNewPerDay(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "a", "b", "c", "d")
	s := reviewService(st)
	s.WithPrefs(&fakePrefs{raw: `{"reviewNewPerDay":2}`})

	r, _ := s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if !equal(cardWords(r), []string{"a", "b"}) || r.NewCount != 2 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// "a" was reviewed 23 hours ago (inside the window, due again in the future): one new word left
	st.review = map[string]fakeReview{"a": {state: srs.State{Ease: 2.5, IntervalDays: 1, Due: reviewNow.Add(time.Hour)}, first: reviewNow.Add(-23 * time.Hour)}}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if !equal(cardWords(r), []string{"b"}) || r.NewCount != 1 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// 25 hours ago: it left the window
	st.review["a"] = fakeReview{state: st.review["a"].state, first: reviewNow.Add(-25 * time.Hour)}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if !equal(cardWords(r), []string{"b", "c"}) || r.NewCount != 2 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// 0 means no new words at all
	s.WithPrefs(&fakePrefs{raw: `{"reviewNewPerDay":0}`})
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if len(r.Cards) != 0 || r.NewCount != 0 || r.TotalCount != 4 {
		t.Fatalf("%+v", r)
	}
}

func TestReviewQueueMaxPerDay(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "a", "b", "c", "d", "e")
	old := reviewNow.Add(-72 * time.Hour)
	st.review = map[string]fakeReview{
		"a": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-3 * time.Hour)}, first: old},
		"b": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-2 * time.Hour)}, first: old},
		"c": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-time.Hour)}, first: old},
	}
	s := reviewService(st)
	get := func(prefs string) *dictionaryv1.GetReviewQueueResponse {
		s.WithPrefs(&fakePrefs{raw: prefs})
		r, err := s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// the limit cuts the due cards, the new words are not reduced by it
	r := get(`{"reviewMaxPerDay":2,"reviewNewPerDay":2}`)
	if !equal(cardWords(r), []string{"a", "b", "d", "e"}) || r.DueCount != 2 || r.NewCount != 2 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// 0: no review cards, the new words still come
	r = get(`{"reviewMaxPerDay":0,"reviewNewPerDay":2}`)
	if !equal(cardWords(r), []string{"d", "e"}) || r.DueCount != 0 || r.LearningCount != 0 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// "a" was reviewed again 2 hours ago: one of the two is used up
	st.review["a"] = fakeReview{state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-time.Hour)}, first: old, last: reviewNow.Add(-2 * time.Hour)}
	r = get(`{"reviewMaxPerDay":2,"reviewNewPerDay":0}`)
	if len(r.Cards) != 1 || r.DueCount != 1 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// a card seen for the first time today is a new word, not a review
	st.review["a"] = fakeReview{state: st.review["a"].state, first: reviewNow.Add(-2 * time.Hour), last: reviewNow.Add(-time.Hour)}
	if r = get(`{"reviewMaxPerDay":2,"reviewNewPerDay":0}`); r.DueCount != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestReviewQueueDefaultsToTenNewWords(t *testing.T) {
	st := newFakeStore()
	st.clock = st.clock.Add(0)
	for i := 0; i < 12; i++ {
		listWords(t, st, string(rune('a'+i))+"x")
	}
	r, _ := reviewService(st).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if len(r.Cards) != 10 || r.NewCount != 10 || r.TotalCount != 12 {
		t.Fatalf("%d %+v", len(r.Cards), r)
	}
}

func TestReviewQueueEmpty(t *testing.T) {
	r, err := reviewService(newFakeStore()).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if err != nil || len(r.Cards) != 0 || r.TotalCount != 0 || r.NextDueAt != "" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSubmitReviewSchedules(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "hello")
	s := reviewService(st)
	r, err := s.SubmitReview(userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "Hello", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD})
	if err != nil || r.IntervalSeconds != 86400 || r.DueAt != reviewNow.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("%+v %v", r, err)
	}
	if got := st.review["hello"].state; got.Learning || got.IntervalDays != 1 {
		t.Fatalf("%+v", got)
	}
	// graded again after the day: 1 day * 2.5 ease = 3 days
	s.now = func() time.Time { return reviewNow.Add(24 * time.Hour) }
	r, err = s.SubmitReview(userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD})
	if err != nil || r.IntervalSeconds != 3*86400 {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = s.SubmitReview(userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_AGAIN})
	if err != nil || r.IntervalSeconds != 60 || st.review["hello"].state.Lapses != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSubmitReviewErrors(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "hello")
	s := reviewService(st)
	good := dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD
	for name, c := range map[string]struct {
		ctx  context.Context
		req  *dictionaryv1.SubmitReviewRequest
		want codes.Code
	}{
		"not in the list": {userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "ghost", Grade: good}, codes.NotFound},
		"unspecified":     {userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello"}, codes.InvalidArgument},
		"unknown grade":   {userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello", Grade: 9}, codes.InvalidArgument},
		"blank word":      {userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: " ", Grade: good}, codes.InvalidArgument},
		"another user":    {authn.WithPrincipal(context.Background(), &authn.Principal{Subject: "1c1c1c1c-1111-4222-8333-444444444444", ClientID: "gatekeeper"}), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello", Grade: good}, codes.NotFound},
		"service token":   {authn.WithPrincipal(context.Background(), &authn.Principal{Subject: "gatekeeper", ClientID: "gatekeeper"}), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello", Grade: good}, codes.PermissionDenied},
		"no principal":    {context.Background(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "hello", Grade: good}, codes.Unauthenticated},
	} {
		if _, err := s.SubmitReview(c.ctx, c.req); code(err) != c.want {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if len(st.review) != 0 {
		t.Fatalf("a failed grade stored a schedule: %v", st.review)
	}
	if _, err := s.GetReviewQueue(authn.WithPrincipal(context.Background(), &authn.Principal{Subject: "gatekeeper", ClientID: "gatekeeper"}), &dictionaryv1.GetReviewQueueRequest{Group: "en"}); code(err) != codes.PermissionDenied {
		t.Fatalf("service token on the queue: %v", err)
	}
}

func TestReviewPermissions(t *testing.T) {
	for _, m := range []string{"GetReviewQueue", "SubmitReview"} {
		if r := Permissions["/dictionary.v1.DictionaryService/"+m]; len(r.Any) != 1 || r.Any[0] != "dictionary:word:read" {
			t.Errorf("%s: %+v", m, r)
		}
	}
}

// A card in a learning step (due in minutes) is still in the review when the user comes back.
func TestReviewQueueKeepsCardsInLearning(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "soon", "later")
	st.review = map[string]fakeReview{
		"soon":  {state: srs.State{Ease: 2.5, Learning: true, Due: reviewNow.Add(10 * time.Minute)}, first: reviewNow.Add(-time.Hour)},
		"later": {state: srs.State{Ease: 2.5, IntervalDays: 1, Due: reviewNow.Add(30 * time.Minute)}, first: reviewNow.Add(-time.Hour)},
	}
	r, err := reviewService(st).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if err != nil || !equal(cardWords(r), []string{"soon"}) || r.DueCount != 0 || r.LearningCount != 1 || r.NewCount != 0 {
		t.Fatalf("%v %+v %v", cardWords(r), r, err)
	}
	if r.NextDueAt != reviewNow.Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("next %s", r.NextDueAt)
	}
}

func TestReviewQueueCountsKindsApart(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "fresh", "step", "due", "future")
	st.review = map[string]fakeReview{
		"step":   {state: srs.State{Ease: 2.5, Learning: true, Due: reviewNow.Add(10 * time.Minute)}, first: reviewNow.Add(-time.Hour)},
		"due":    {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-time.Hour)}, first: reviewNow.Add(-72 * time.Hour)},
		"future": {state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(5 * time.Hour)}, first: reviewNow.Add(-72 * time.Hour)},
	}
	r, err := reviewService(st).GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if r.NewCount != 1 || r.LearningCount != 1 || r.DueCount != 1 {
		t.Fatalf("%+v", r)
	}
	want := map[string][2]bool{"due": {false, false}, "step": {false, true}, "fresh": {true, false}} // isNew, learning
	if len(r.Cards) != len(want) {
		t.Fatalf("%v", cardWords(r))
	}
	for _, c := range r.Cards {
		if got := [2]bool{c.IsNew, c.Learning}; got != want[c.Word] {
			t.Fatalf("%s: isNew/learning %v, want %v", c.Word, got, want[c.Word])
		}
	}
}

func TestReviewBonusNewWords(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "a", "b", "c", "d", "e")
	prefs := &fakePrefs{raw: `{"reviewNewPerDay":2}`}
	s := reviewService(st)
	s.WithPrefs(prefs)
	// the limit is used up: "a" and "b" were first reviewed an hour ago
	old := srs.State{Ease: 2.5, IntervalDays: 1, Due: reviewNow.Add(time.Hour)}
	st.review = map[string]fakeReview{"a": {state: old, first: reviewNow.Add(-time.Hour)}, "b": {state: old, first: reviewNow.Add(-time.Hour)}}
	r, _ := s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if len(r.Cards) != 0 || r.NewHeld != 3 {
		t.Fatalf("%+v", r)
	}
	b, err := s.AddReviewBonus(userCtx(), &dictionaryv1.AddReviewBonusRequest{NewWords: 2})
	if err != nil || b.ExtraNew != 2 || b.ExtraReviews != 0 || b.ExpiresAt != reviewNow.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("%+v %v", b, err)
	}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if !equal(cardWords(r), []string{"c", "d"}) || r.NewCount != 2 || r.NewHeld != 1 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
	// the bonus adds up
	if b, _ = s.AddReviewBonus(userCtx(), &dictionaryv1.AddReviewBonusRequest{NewWords: 5}); b.ExtraNew != 7 {
		t.Fatalf("%+v", b)
	}
	// the settings are not touched
	if prefs.raw != `{"reviewNewPerDay":2}` {
		t.Fatalf("prefs: %s", prefs.raw)
	}
	// 24 hours later the bonus is gone and the original limit is back
	old.Due = reviewNow.Add(48 * time.Hour)
	s.now = func() time.Time { return reviewNow.Add(24*time.Hour + time.Minute) }
	st.review["a"] = fakeReview{state: old, first: reviewNow.Add(23 * time.Hour)}
	st.review["b"] = fakeReview{state: old, first: reviewNow.Add(23 * time.Hour)}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if len(r.Cards) != 0 || r.NewHeld != 3 {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
}

func TestReviewBonusReviews(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "a", "b", "c")
	old := reviewNow.Add(-72 * time.Hour)
	st.review = map[string]fakeReview{}
	for i, w := range []string{"a", "b", "c"} {
		st.review[w] = fakeReview{state: srs.State{Ease: 2.5, IntervalDays: 3, Due: reviewNow.Add(-time.Duration(3-i) * time.Hour)}, first: old}
	}
	s := reviewService(st)
	s.WithPrefs(&fakePrefs{raw: `{"reviewMaxPerDay":1,"reviewNewPerDay":0}`})
	r, _ := s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if r.DueCount != 1 || r.DueHeld != 2 {
		t.Fatalf("%+v", r)
	}
	if _, err := s.AddReviewBonus(userCtx(), &dictionaryv1.AddReviewBonusRequest{Reviews: 1}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if r.DueCount != 2 || r.DueHeld != 1 || !equal(cardWords(r), []string{"a", "b"}) {
		t.Fatalf("%v %+v", cardWords(r), r)
	}
}

func TestAddReviewBonusValidates(t *testing.T) {
	s := reviewService(newFakeStore())
	for name, req := range map[string]*dictionaryv1.AddReviewBonusRequest{
		"negative new":     {NewWords: -1, Reviews: 1},
		"negative reviews": {NewWords: 1, Reviews: -1},
		"new too big":      {NewWords: 10000},
		"reviews too big":  {Reviews: 10000},
		"both zero":        {},
	} {
		if _, err := s.AddReviewBonus(userCtx(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.AddReviewBonus(userCtx(), &dictionaryv1.AddReviewBonusRequest{NewWords: 9999, Reviews: 9999}); err != nil {
		t.Fatalf("limits: %v", err)
	}
}

func TestListSavedWordsCarriesReviewState(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	saveWords(t, s, "fresh", "learn", "graded")
	if _, err := st.PutReview(context.Background(), userUUID, "en", "learn", reviewNow, srs.Again, func(c srs.State) srs.State { return srs.Next(c, srs.Again, reviewNow) }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutReview(context.Background(), userUUID, "en", "graded", reviewNow, srs.Good, func(c srs.State) srs.State { return srs.Next(c, srs.Good, reviewNow) }); err != nil {
		t.Fatal(err)
	}
	r, err := s.ListSavedWords(userCtx(), &dictionaryv1.ListSavedWordsRequest{Group: "en"})
	if err != nil || len(r.Words) != 3 {
		t.Fatalf("%v %v", r, err)
	}
	got := map[string]*dictionaryv1.SavedWord{}
	for _, w := range r.Words {
		got[w.Word] = w
	}
	if w := got["fresh"]; w.NextReviewAt != "" || w.Learning {
		t.Errorf("new: %v", w)
	}
	if w := got["learn"]; !w.Learning || w.NextReviewAt != reviewNow.Add(time.Minute).Format(time.RFC3339) {
		t.Errorf("learning: %v", w)
	}
	if w := got["graded"]; w.Learning || w.NextReviewAt != reviewNow.Add(24*time.Hour).Format(time.RFC3339) {
		t.Errorf("review: %v", w)
	}
}

func fsrsPrefs(extra string) *fakePrefs {
	return &fakePrefs{raw: `{"reviewAlgorithm":"fsrs"` + extra + `}`}
}

func TestSubmitReviewWithFSRSStoresMemoryAndReturnsFourIntervals(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "apple")
	s := reviewService(st).WithPrefs(fsrsPrefs(""))
	r, err := s.SubmitReview(userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "apple", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD})
	if err != nil {
		t.Fatal(err)
	}
	if st.review["apple"].state.Stability <= 0 || len(r.GradeIntervalsSeconds) != 4 {
		t.Fatalf("%+v %+v", st.review["apple"].state, r)
	}
	if g := r.GradeIntervalsSeconds; !(g[0] < g[1] && g[1] < g[2] && g[2] < g[3]) {
		t.Fatalf("intervals %v", g)
	}
	if len(st.log) != 1 || st.log[0].grade != srs.Good {
		t.Fatalf("log %+v", st.log)
	}
}

func TestSubmitReviewWithoutPreferenceStaysSM2(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "apple")
	r, err := reviewService(st).SubmitReview(userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "apple", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD})
	if err != nil {
		t.Fatal(err)
	}
	if st.review["apple"].state.Stability != 0 || r.IntervalSeconds != 86400 {
		t.Fatalf("%+v %+v", st.review["apple"].state, r)
	}
	// the buttons of the next grade: Again 1m, Hard 10m (SM-2 of a card in review: 1d then more)
	if len(r.GradeIntervalsSeconds) != 4 || r.GradeIntervalsSeconds[0] != 60 {
		t.Fatalf("%v", r.GradeIntervalsSeconds)
	}
}

func TestQueueUsesFSRSIntervalsAndCachesTheSchedule(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "apple")
	s := reviewService(st).WithPrefs(fsrsPrefs(`,"desiredRetention":80`)).WithScheduleCache(time.Minute, 10)
	r, err := s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if err != nil || len(r.Cards) != 1 {
		t.Fatalf("%v %v", r, err)
	}
	g := r.Cards[0].GradeIntervalsSeconds
	if g[0] != 600 || g[2] < 86400 { // Again within 10 minutes, Good at least a day
		t.Fatalf("%v", g)
	}
	// the change of the settings reaches this instance at once
	s.sched.forget(userUUID)
	s.prefs = &fakePrefs{}
	r, _ = s.GetReviewQueue(userCtx(), &dictionaryv1.GetReviewQueueRequest{Group: "en"})
	if r.Cards[0].GradeIntervalsSeconds[0] != 60 {
		t.Fatalf("still FSRS: %v", r.Cards[0].GradeIntervalsSeconds)
	}
}

func TestOptimizeWithEnoughLogStoresParametersAndSubmitUsesThem(t *testing.T) {
	st := newFakeStore()
	listWords(t, st, "apple")
	// a user whose memory is much worse than the defaults: gaps of weeks and many lapses
	for i := 0; i < 400; i++ {
		at := int64(1_700_000_000 + i*100)
		c := []srs.Review{{Grade: srs.Good, At: at}}
		for k := 1; k <= 4; k++ {
			g := srs.Again
			if (i+k)%5 == 0 {
				g = srs.Good
			}
			c = append(c, srs.Review{Grade: g, At: at + int64(k)*30*86400, Elapsed: 30 * 86400})
		}
		st.logs = append(st.logs, c)
	}
	s := reviewService(st).WithPrefs(fsrsPrefs(""))
	r, err := s.OptimizeReviewParameters(userCtx(), &dictionaryv1.OptimizeReviewParametersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Applied || r.LossTrained >= r.LossDefault || st.params == nil || st.params.Weights == nil || r.TrainedAt == "" {
		t.Fatalf("%+v %+v", r, st.params)
	}
	s.sched.forget(userUUID)
	if _, err := s.SubmitReview(userCtx(), &dictionaryv1.SubmitReviewRequest{Group: "en", Word: "apple", Grade: dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD}); err != nil {
		t.Fatal(err)
	}
	want := srs.Scheduler{Algo: srs.FSRS, Weights: *st.params.Weights}.Next(srs.New(), srs.Good, reviewNow)
	if got := st.review["apple"].state; got.Stability != want.Stability {
		t.Fatalf("stability %v, want %v from the fitted weights", got.Stability, want.Stability)
	}
	// the settings show when
	my, err := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if err != nil || my.FsrsTrainedAt == "" || my.FsrsReviewCount != r.ReviewCount {
		t.Fatalf("%+v %v", my, err)
	}
	// reset forgets them
	if _, err := s.OptimizeReviewParameters(userCtx(), &dictionaryv1.OptimizeReviewParametersRequest{RestoreDefaults: true}); err != nil || st.params != nil {
		t.Fatalf("%v %+v", err, st.params)
	}
}

func TestOptimizeWithLittleDataStoresNothing(t *testing.T) {
	st := newFakeStore()
	s := reviewService(st).WithPrefs(fsrsPrefs(""))
	_, err := s.OptimizeReviewParameters(userCtx(), &dictionaryv1.OptimizeReviewParametersRequest{})
	if code(err) != codes.FailedPrecondition || st.params != nil {
		t.Fatalf("%v %+v", err, st.params)
	}
}

func TestOptimizeIsRateLimitedAndOneRunPerUser(t *testing.T) {
	st := newFakeStore()
	s := reviewService(st)
	// the second try within the minute
	_, _ = s.OptimizeReviewParameters(userCtx(), &dictionaryv1.OptimizeReviewParametersRequest{})
	if _, err := s.OptimizeReviewParameters(userCtx(), &dictionaryv1.OptimizeReviewParametersRequest{}); code(err) != codes.ResourceExhausted {
		t.Fatalf("second within a minute: %v", err)
	}
	// while a run holds the lock of the user, another one is refused
	st2 := newFakeStore()
	unlock, ok, _ := st2.LockOptimize(context.Background(), userUUID)
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	if _, err := reviewService(st2).OptimizeReviewParameters(userCtx(), &dictionaryv1.OptimizeReviewParametersRequest{}); code(err) != codes.ResourceExhausted {
		t.Fatalf("while locked: %v", err)
	}
}

func TestUpdateMySettingsAlgorithmAndRetention(t *testing.T) {
	st := newFakeStore()
	prefs := &fakePrefs{}
	s := reviewService(st).WithPrefs(prefs)
	cur, _ := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if cur.User.ReviewAlgorithm != "sm2" || cur.User.DesiredRetention != 90 {
		t.Fatalf("%+v", cur.User)
	}
	u := cur.User
	u.ReviewAlgorithm, u.DesiredRetention = "fsrs", 85
	r, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: u})
	if err != nil || r.User.ReviewAlgorithm != "fsrs" || r.User.DesiredRetention != 85 {
		t.Fatalf("%+v %v", r, err)
	}
	prefs.raw = prefs.written
	again, _ := s.GetMySettings(userCtx(), &dictionaryv1.GetMySettingsRequest{})
	if again.User.ReviewAlgorithm != "fsrs" || again.User.DesiredRetention != 85 {
		t.Fatalf("round trip: %+v", again.User)
	}
	for _, bad := range []func(*dictionaryv1.UserSettings){
		func(u *dictionaryv1.UserSettings) { u.ReviewAlgorithm = "leitner" },
		func(u *dictionaryv1.UserSettings) { u.DesiredRetention = 99 },
		func(u *dictionaryv1.UserSettings) { u.DesiredRetention = 50 },
	} {
		u := proto.Clone(again.User).(*dictionaryv1.UserSettings)
		bad(u)
		if _, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: u}); code(err) != codes.InvalidArgument {
			t.Fatalf("accepted %+v: %v", u, err)
		}
	}
	// an old client sends neither field: SM-2 and 90
	old := proto.Clone(again.User).(*dictionaryv1.UserSettings)
	old.ReviewAlgorithm, old.DesiredRetention = "", 0
	if r, err := s.UpdateMySettings(userCtx(), &dictionaryv1.UpdateMySettingsRequest{User: old}); err != nil || r.User.ReviewAlgorithm != "sm2" || r.User.DesiredRetention != 90 {
		t.Fatalf("%+v %v", r, err)
	}
}
