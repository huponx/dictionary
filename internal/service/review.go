package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/srs"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/logging"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

// Cards one GetReviewQueue returns when the request asks for none, and at most.
const (
	defaultReviewLimit = 50
	maxReviewLimit     = 200
)

// learnAhead is how far ahead a card counts as due: a card in a learning step (Again, Hard) comes due in minutes
// and must not vanish from the review while the user steps away for a moment.
const learnAhead = 20 * time.Minute

// GetReviewQueue returns the cards to review: the due ones first as far as the daily limit of the user allows
// (reviewMaxPerDay, minus the cards reviewed again in the last 24 hours), then new words as far as the other daily
// limit (reviewNewPerDay, counted over the last 24 hours) allows. The bonus of AddReviewBonus raises both limits while
// it lasts; NewHeld and DueHeld say how many cards the limits still hold back.
func (s *Service) GetReviewQueue(ctx context.Context, req *dictionaryv1.GetReviewQueueRequest) (*dictionaryv1.GetReviewQueueResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	switch {
	case limit < 0:
		return nil, apperror.InvalidArgument("limit must not be negative").WithReason("limit_negative")
	case limit == 0:
		limit = defaultReviewLimit
	case limit > maxReviewLimit:
		limit = maxReviewLimit
	}
	now := s.now()
	horizon := now.Add(learnAhead)
	filter := wordFilter(req.GetCategoryId(), req.GetUncategorized())
	// the queries of a request do not depend on the number of words of the user: they read what they return, or a
	// counter with a cap. The ones that do not wait for each other run at once.
	var (
		sch           schedule
		done, again   int
		bonus         store.ReviewBonus
		counts        store.ReviewCounts
		cards         []store.ReviewCard
		failedStep    string
		firstFailure  error
		failureLocker sync.Mutex
	)
	run := func(step string, fns ...func() error) error {
		var wg sync.WaitGroup
		for _, fn := range fns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := fn(); err != nil {
					failureLocker.Lock()
					if firstFailure == nil {
						failedStep, firstFailure = step, err
					}
					failureLocker.Unlock()
				}
			}()
		}
		wg.Wait()
		if firstFailure != nil {
			return internal(ctx, failedStep, firstFailure)
		}
		return nil
	}
	err = run("read review state",
		func() (err error) { sch = s.loadSchedule(ctx, user, now); return nil },
		func() (err error) { done, again, err = s.store.ReviewToday(ctx, user, now); return err },
		func() (err error) { bonus, err = s.store.ReviewBonus(ctx, user, now); return err },
	)
	if err != nil {
		return nil, err
	}
	prefs := sch.prefs
	newAllowed := max(0, prefs.ReviewNewPerDay+bonus.New-done)
	dueAllowed := max(0, prefs.ReviewMaxPerDay+bonus.Reviews-again)
	err = run("read review queue",
		func() (err error) { counts, err = s.store.ReviewCounts(ctx, user, horizon, filter); return err },
		func() (err error) {
			cards, err = s.store.ReviewQueue(ctx, user, horizon, filter, limit, dueAllowed, newAllowed)
			return err
		},
	)
	if err != nil {
		return nil, err
	}
	newCount := min(counts.New, newAllowed)
	learning := min(counts.Learning, dueAllowed)
	due := min(counts.Due, dueAllowed-learning)
	out := &dictionaryv1.GetReviewQueueResponse{
		DueCount: int32(due), LearningCount: int32(learning), NewCount: int32(newCount), TotalCount: int32(counts.Total),
		NewHeld: int32(counts.New - newCount), DueHeld: int32(counts.Due + counts.Learning - due - learning),
		CountsCapped: counts.Capped,
		Cards:        make([]*dictionaryv1.ReviewCard, 0, len(cards)),
	}
	if !counts.Next.IsZero() {
		out.NextDueAt = counts.Next.UTC().Format(time.RFC3339)
	}
	for _, c := range cards {
		out.Cards = append(out.Cards, &dictionaryv1.ReviewCard{
			Word: c.Word, Content: c.Content, AudioUk: c.AudioUK, AudioUs: c.AudioUS, IsNew: c.IsNew, Learning: c.State.Learning && !c.IsNew,
			GradeIntervalsSeconds: intervalSeconds(sch.scheduler.Preview(c.State, now)),
		})
	}
	return out, nil
}

func intervalSeconds(p [4]time.Duration) []int64 {
	return []int64{int64(p[0].Seconds()), int64(p[1].Seconds()), int64(p[2].Seconds()), int64(p[3].Seconds())}
}

// loadSchedule returns the preferences of the user and the scheduler they choose: SM-2, or FSRS with the weights fitted
// to the user (the defaults when there are none or they cannot be read).
func (s *Service) loadSchedule(ctx context.Context, user string, now time.Time) schedule {
	if v, ok := s.sched.get(user, now); ok {
		return v
	}
	prefs := s.userPrefs(ctx)
	v := schedule{prefs: prefs}
	if prefs.ReviewAlgorithm == settings.AlgorithmFSRS {
		v.scheduler = srs.Scheduler{Algo: srs.FSRS, Retention: float64(prefs.DesiredRetention) / 100}
		if p, err := s.store.FSRSParams(ctx, user); err != nil {
			logging.FromCtx(ctx).Warn("fsrs parameters unavailable, using the defaults", zap.Error(err))
			return v // not cached: the next request tries again
		} else if p != nil && p.Weights != nil {
			v.scheduler.Weights = *p.Weights
		}
	}
	s.sched.put(user, now, v)
	return v
}

// maxReviewBonus bounds each number of AddReviewBonus.
const maxReviewBonus = 9999

// AddReviewBonus raises today's limits of the review by the given numbers for 24 hours; the settings stay as they are.
func (s *Service) AddReviewBonus(ctx context.Context, req *dictionaryv1.AddReviewBonusRequest) (*dictionaryv1.AddReviewBonusResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	n, r := req.GetNewWords(), req.GetReviews()
	switch {
	case n < 0 || n > maxReviewBonus || r < 0 || r > maxReviewBonus:
		return nil, apperror.InvalidArgument("new_words and reviews must be between 0 and 9999").WithReason("review_limits_range")
	case n == 0 && r == 0:
		return nil, apperror.InvalidArgument("new_words or reviews must be above 0").WithReason("review_limits_zero")
	}
	b, err := s.store.AddReviewBonus(ctx, user, s.now(), int(n), int(r))
	if err != nil {
		return nil, internal(ctx, "add review bonus", err)
	}
	return &dictionaryv1.AddReviewBonusResponse{ExtraNew: int32(b.New), ExtraReviews: int32(b.Reviews), ExpiresAt: b.Expires.UTC().Format(time.RFC3339)}, nil
}

// SubmitReview grades a card and stores its next review.
func (s *Service) SubmitReview(ctx context.Context, req *dictionaryv1.SubmitReviewRequest) (*dictionaryv1.SubmitReviewResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	grade, ok := map[dictionaryv1.ReviewGrade]srs.Grade{
		dictionaryv1.ReviewGrade_REVIEW_GRADE_AGAIN: srs.Again, dictionaryv1.ReviewGrade_REVIEW_GRADE_HARD: srs.Hard,
		dictionaryv1.ReviewGrade_REVIEW_GRADE_GOOD: srs.Good, dictionaryv1.ReviewGrade_REVIEW_GRADE_EASY: srs.Easy,
	}[req.GetGrade()]
	if !ok {
		return nil, apperror.InvalidArgument("grade must be again, hard, good or easy").WithReason("invalid_grade")
	}
	word, err := lookup.NormalizeWord(req.GetWord())
	if err != nil {
		return nil, lookupErr(ctx, err)
	}
	now := s.now()
	sch := s.loadSchedule(ctx, user, now)
	next, err := s.store.PutReview(ctx, user, word, now, grade, func(cur srs.State) srs.State { return sch.scheduler.Next(cur, grade, now) })
	switch {
	case err == nil:
		return &dictionaryv1.SubmitReviewResponse{
			DueAt: next.Due.UTC().Format(time.RFC3339), IntervalSeconds: int64(next.Due.Sub(now).Seconds()),
			GradeIntervalsSeconds: intervalSeconds(sch.scheduler.Preview(next, now)),
		}, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("saved word not found").WithReason("saved_word_not_found")
	default:
		return nil, internal(ctx, "submit review", err)
	}
}

// Limits of the optimizer: runs at once on one instance, the wait between two runs of a user, and the time one run may take.
const (
	maxOptimizing    = 2
	optimizeGap      = time.Minute
	optimizeTimeout  = 15 * time.Second
	optimizeLogLimit = 75000
)

// OptimizeReviewParameters fits the FSRS parameters to the review history of the user and stores them when they
// predict better than the defaults; restore_defaults = true forgets them instead. The CPU is protected by the database and not
// by the memory of the process: one run at a time per user (an advisory lock), one try a minute per user (a row), and
// at most maxOptimizing runs at once on this instance.
func (s *Service) OptimizeReviewParameters(ctx context.Context, req *dictionaryv1.OptimizeReviewParametersRequest) (*dictionaryv1.OptimizeReviewParametersResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetRestoreDefaults() {
		if err := s.store.DeleteFSRSParams(ctx, user); err != nil {
			return nil, internal(ctx, "reset fsrs parameters", err)
		}
		s.sched.forget(user)
		return &dictionaryv1.OptimizeReviewParametersResponse{}, nil
	}
	select {
	case s.optimizing <- struct{}{}:
		defer func() { <-s.optimizing }()
	default:
		return nil, apperror.Wrap(codes.ResourceExhausted, "the server is busy optimizing, try again in a moment", nil).WithReason("optimizer_busy")
	}
	unlock, ok, err := s.store.LockOptimize(ctx, user)
	if err != nil {
		return nil, internal(ctx, "lock optimizer", err)
	}
	if !ok {
		return nil, apperror.Wrap(codes.ResourceExhausted, "the parameters are already being optimized", nil).WithReason("optimizer_running")
	}
	defer unlock()
	now := s.now()
	if ok, err := s.store.ClaimOptimize(ctx, user, now, optimizeGap); err != nil {
		return nil, internal(ctx, "claim optimizer", err)
	} else if !ok {
		return nil, apperror.Wrap(codes.ResourceExhausted, "the parameters were optimized a moment ago, try again in a minute", nil).WithReason("optimizer_recent")
	}
	ctx, cancel := context.WithTimeout(ctx, optimizeTimeout)
	defer cancel()
	cards, err := s.store.ReviewLogs(ctx, user, optimizeLogLimit)
	if err != nil {
		return nil, internal(ctx, "read review log", err)
	}
	res, err := srs.Optimize(ctx, capSamples(cards, srs.MaxSamples))
	switch {
	case errors.Is(err, srs.ErrNotEnoughData):
		return nil, apperror.FailedPrecondition("not enough reviews yet: the optimizer needs at least 200 reviews of cards seen again after a day or more").WithReason("optimizer_not_enough_reviews")
	case err != nil:
		return nil, internal(ctx, "optimize fsrs parameters", err)
	}
	out := &dictionaryv1.OptimizeReviewParametersResponse{
		Applied: res.Applied, ReviewCount: int32(res.Samples), LossDefault: float32(res.LossDefault), LossTrained: float32(res.LossTrained),
		Reason: res.Reason,
	}
	if res.Applied {
		p := store.FSRSParams{Weights: &res.Weights, ReviewCount: res.Samples, LossDefault: res.LossDefault, LossTrained: res.LossTrained, TrainedAt: now}
		if err := s.store.PutFSRSParams(ctx, user, p); err != nil {
			return nil, internal(ctx, "store fsrs parameters", err)
		}
		s.sched.forget(user)
		out.TrainedAt = now.UTC().Format(time.RFC3339)
	} else if p, err := s.store.FSRSParams(ctx, user); err == nil && p != nil {
		out.TrainedAt = p.TrainedAt.UTC().Format(time.RFC3339)
	}
	return out, nil
}

// capSamples drops the oldest cards until the rest has at most max samples (grades after a gap of a day or more).
func capSamples(cards [][]srs.Review, max int) [][]srs.Review {
	counts := make([]int, len(cards))
	total := 0
	for i, c := range cards {
		for j, r := range c {
			if j > 0 && r.Elapsed >= 86400 {
				counts[i]++
			}
		}
		total += counts[i]
	}
	for len(cards) > 0 && total > max { // cards are ordered by their first grade
		total -= counts[0]
		cards, counts = cards[1:], counts[1:]
	}
	return cards
}
