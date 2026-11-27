package store

import (
	"context"
	"errors"
	"time"

	"github.com/huponx/dictionary/internal/srs"
	"github.com/jackc/pgx/v5"
)

// ReviewCard is a word of the list to review. A word without a row in dictionary_review_state is new (IsNew,
// State is srs.New()).
type ReviewCard struct {
	Group   string
	Word    string
	Content string
	AudioUK string
	AudioUS string
	IsNew   bool
	State   srs.State
}

// ReviewCounts says how much there is to review.
type ReviewCounts struct {
	// Total: words in the list.
	Total int
	// Due: review cards (not in learning) that are due now. Learning: cards in a learning step that are due now.
	// New: words never reviewed (before the daily limit).
	Due, Learning, New int
	// Next is when the first card that is not due yet comes due; the zero time when there is none.
	Next time.Time
	// Capped: a count stopped at its cap, so it is a lower bound.
	Capped bool
}

// reviewCountCap bounds every count of ReviewCounts: a count never reads more than this many rows, however long the
// list is. The queue only needs min(count, daily limit), and the limits stay below it.
const reviewCountCap = 10000

// ReviewQueue returns up to limit cards: at most dueAllowed of the ones due at now first (earliest due first), then
// at most newAllowed new words (oldest first) in what is left of limit. Only words of the list (removed_at NULL) that pass f.
func (s *Store) ReviewQueue(ctx context.Context, userID string, now time.Time, f WordFilter, limit, dueAllowed, newAllowed int) ([]ReviewCard, error) {
	args := []any{userID, now, min(limit, dueAllowed)}
	rows, err := s.pool.Query(ctx, `
		SELECT w.grp, w.word, w.content, w.audio_uk, w.audio_us, r.ease, r.interval_days, r.reps, r.lapses, r.learning, r.due_at,
		       r.stability, r.difficulty, r.last_reviewed_at
		FROM dictionary_review_state r JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.word = r.word
		WHERE r.user_id = $1 AND r.due_at <= $2 AND w.removed_at IS NULL`+f.clause("w", &args)+`
		ORDER BY r.due_at, r.word LIMIT $3`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []ReviewCard
	for rows.Next() {
		var c ReviewCard
		var ease, stability, difficulty float32
		if err := rows.Scan(&c.Group, &c.Word, &c.Content, &c.AudioUK, &c.AudioUS, &ease, &c.State.IntervalDays, &c.State.Reps,
			&c.State.Lapses, &c.State.Learning, &c.State.Due, &stability, &difficulty, &c.State.Last); err != nil {
			return nil, err
		}
		c.State.Ease = roundEase(ease)
		c.State.Stability, c.State.Difficulty = float64(stability), float64(difficulty)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if n := min(limit-len(out), newAllowed); n > 0 {
		args := []any{userID, n}
		fresh, err := s.pool.Query(ctx, `
			SELECT w.grp, w.word, w.content, w.audio_uk, w.audio_us
				FROM dictionary_saved_words w
			WHERE w.user_id = $1 AND w.removed_at IS NULL AND NOT w.review_started`+f.clause("w", &args)+`
			ORDER BY w.created_at, w.word LIMIT $2`, args...)
		if err != nil {
			return nil, mapErr(err)
		}
		defer fresh.Close()
		for fresh.Next() {
			c := ReviewCard{IsNew: true, State: srs.New()}
			if err := fresh.Scan(&c.Group, &c.Word, &c.Content, &c.AudioUK, &c.AudioUS); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		if err := fresh.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// roundEase undoes the float32 of the column (2.35 comes back as 2.3499999).
func roundEase(e float32) float64 {
	return float64(int(float64(e)*1000+0.5)) / 1000
}

// ReviewCounts counts the words of the list: all of them, the due ones, the new ones, and when the next one
// comes due. Only the words that pass f. Every count stops at reviewCountCap (Capped says one did), so the cost
// does not grow with the list.
func (s *Store) ReviewCounts(ctx context.Context, userID string, now time.Time, f WordFilter) (ReviewCounts, error) {
	var c ReviewCounts
	var next *time.Time
	args := []any{userID, now, reviewCountCap}
	cl := f.clause("w", &args)
	// The word is looked up with a subplan per card, not joined: a join lets the planner read the whole list of the
	// user, a subplan reads the due cards in order and stops at the cap.
	due := func(learning string) string {
		return `(SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
				WHERE w.user_id = r.user_id AND w.word = r.word AND w.removed_at IS NULL` + cl + `) AS listed
			FROM dictionary_review_state r WHERE r.user_id = $1 AND r.due_at <= $2 AND ` + learning + `) x WHERE listed LIMIT $3) t)`
	}
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = $1 AND w.removed_at IS NULL`+cl+` LIMIT $3) t),
		  (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = $1 AND w.removed_at IS NULL AND NOT w.review_started`+cl+` LIMIT $3) t),
		  `+due("NOT r.learning")+`,
		  `+due("r.learning")+`,
		  (SELECT r.due_at FROM dictionary_review_state r JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.grp = r.grp AND w.word = r.word
				WHERE r.user_id = $1 AND r.due_at > $2 AND w.removed_at IS NULL`+cl+` ORDER BY r.due_at LIMIT 1)`, args...).
		Scan(&c.Total, &c.New, &c.Due, &c.Learning, &next)
	if next != nil {
		c.Next = *next
	}
	c.Capped = c.Total >= reviewCountCap || c.New >= reviewCountCap || c.Due >= reviewCountCap || c.Learning >= reviewCountCap
	return c, mapErr(err)
}

// ReviewToday returns how many words of the list were reviewed for the first time in the 24 hours before now
// (newWords), and how many cards of the list that were reviewed before were reviewed again in that time (again).
// Only the last review of a card is stored, so a card reviewed several times counts once. One range scan on the
// reviews of the last 24 hours.
func (s *Store) ReviewToday(ctx context.Context, userID, group string, now time.Time) (newWords, again int, err error) {
	since := now.Add(-24 * time.Hour)
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE r.first_reviewed_at > $2), count(*) FILTER (WHERE r.first_reviewed_at <= $2)
		FROM dictionary_review_state r
		JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.grp = r.grp AND w.word = r.word AND w.removed_at IS NULL
			WHERE r.user_id = $1 AND r.grp = $3 AND r.last_reviewed_at > $2`, userID, since, group).Scan(&newWords, &again)
	return newWords, again, mapErr(err)
}

// ReviewBonus is the extra daily allowance a user asked for: new words and review cards on top of the daily limits,
// until Expires.
type ReviewBonus struct {
	New, Reviews int
	Expires      time.Time
}

// maxReviewBonus caps each kind of bonus.
const maxReviewBonus = 9999

// ReviewBonus returns the bonus in effect at now: the zero value when there is none or it has expired.
func (s *Store) ReviewBonus(ctx context.Context, userID string, now time.Time) (ReviewBonus, error) {
	var b ReviewBonus
	err := s.pool.QueryRow(ctx, `SELECT extra_new, extra_reviews, expires_at FROM dictionary_review_bonus
		WHERE user_id = $1 AND expires_at > $2`, userID, now).Scan(&b.New, &b.Reviews, &b.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewBonus{}, nil
	}
	return b, mapErr(err)
}

// AddReviewBonus adds newWords and reviews to the bonus and makes it last 24 hours from now. An expired bonus starts
// again from 0. Each kind stops at 9999. It returns the bonus now in effect.
func (s *Store) AddReviewBonus(ctx context.Context, userID string, now time.Time, newWords, reviews int) (ReviewBonus, error) {
	var b ReviewBonus
	err := s.pool.QueryRow(ctx, `
		INSERT INTO dictionary_review_bonus AS b (user_id, extra_new, extra_reviews, expires_at)
		VALUES ($1, LEAST($3::int, $5), LEAST($4::int, $5), $2::timestamptz + interval '24 hours')
		ON CONFLICT (user_id) DO UPDATE SET
			extra_new     = LEAST(CASE WHEN b.expires_at > $2 THEN b.extra_new     ELSE 0 END + $3::int, $5),
			extra_reviews = LEAST(CASE WHEN b.expires_at > $2 THEN b.extra_reviews ELSE 0 END + $4::int, $5),
			expires_at    = $2::timestamptz + interval '24 hours'
		RETURNING extra_new, extra_reviews, expires_at`, userID, now, newWords, reviews, maxReviewBonus).
		Scan(&b.New, &b.Reviews, &b.Expires)
	return b, mapErr(err)
}

// PutReview applies next to the schedule of the word, stores the result and logs the grade. It runs in a
// transaction that locks the word and the schedule, so two grades of one card apply one after the other. ErrNotFound
// when the word is not in the list of the user.
func (s *Store) PutReview(ctx context.Context, userID, group, word string, now time.Time, grade srs.Grade, next func(srs.State) srs.State) (srs.State, error) {
	var out srs.State
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// no row to lock for a new word: serialize the grades of one user instead (the same lock as SaveWord)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "dictionary:"+userID); err != nil {
			return err
		}
		// FOR SHARE keeps a delete of the word from slipping in between the read and the write
		var listed bool
		err := tx.QueryRow(ctx, `SELECT true FROM dictionary_saved_words WHERE user_id = $1 AND grp = $3 AND word = $2 AND removed_at IS NULL FOR SHARE`,
			userID, word, group).Scan(&listed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		cur, first, err := reviewState(ctx, tx, userID, group, word, now)
		if err != nil {
			return err
		}
		var elapsed int64
		if !cur.Last.IsZero() {
			elapsed = max(0, int64(now.Sub(cur.Last).Seconds()))
		}
		out = next(cur)
		out.Last = now
		var b pgx.Batch
		b.Queue(`
			INSERT INTO dictionary_review_state (user_id, grp, word, ease, interval_days, reps, lapses, learning, due_at, first_reviewed_at,
				last_reviewed_at, stability, difficulty)
			VALUES ($1, $13, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (user_id, grp, word) DO UPDATE SET ease = EXCLUDED.ease, interval_days = EXCLUDED.interval_days,
				reps = EXCLUDED.reps, lapses = EXCLUDED.lapses, learning = EXCLUDED.learning, due_at = EXCLUDED.due_at,
				last_reviewed_at = EXCLUDED.last_reviewed_at, stability = EXCLUDED.stability, difficulty = EXCLUDED.difficulty`,
			userID, word, float32(out.Ease), out.IntervalDays, out.Reps, out.Lapses, out.Learning, out.Due, first, now,
			float32(out.Stability), float32(out.Difficulty), group)
		// the word is no longer new (the partial index of the new words skips it)
		b.Queue(`UPDATE dictionary_saved_words SET review_started = true WHERE user_id = $1 AND grp = $3 AND word = $2 AND NOT review_started`, userID, word, group)
		b.Queue(`INSERT INTO dictionary_review_log (user_id, grp, word, reviewed_at, grade, elapsed_seconds, was_learning)
				VALUES ($1, $7, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, userID, word, now, int16(grade), elapsed, cur.Learning, group)
		res := tx.SendBatch(ctx, &b)
		for i := 0; i < b.Len(); i++ {
			if _, err := res.Exec(); err != nil {
				res.Close()
				return err
			}
		}
		return res.Close()
	})
	if err != nil {
		return srs.State{}, mapErr(err)
	}
	return out, nil
}

// reviewState reads the schedule of the word with a lock; a word without one is new (first is now).
func reviewState(ctx context.Context, tx pgx.Tx, userID, group, word string, now time.Time) (srs.State, time.Time, error) {
	s, first := srs.New(), now
	var ease, stability, difficulty float32
	err := tx.QueryRow(ctx, `SELECT ease, interval_days, reps, lapses, learning, due_at, first_reviewed_at, stability, difficulty, last_reviewed_at
		FROM dictionary_review_state WHERE user_id = $1 AND grp = $3 AND word = $2 FOR UPDATE`, userID, word, group).
		Scan(&ease, &s.IntervalDays, &s.Reps, &s.Lapses, &s.Learning, &s.Due, &first, &stability, &difficulty, &s.Last)
	if errors.Is(err, pgx.ErrNoRows) {
		return srs.New(), now, nil
	}
	s.Ease = roundEase(ease)
	s.Stability, s.Difficulty = float64(stability), float64(difficulty)
	return s, first, err
}

type reviewRow struct {
	due      time.Time
	learning bool
}

// reviewStates returns the review state of the words that were reviewed; a new word has no entry.
func (s *Store) reviewStates(ctx context.Context, userID, group string, words []string) (map[string]reviewRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT word, due_at, learning FROM dictionary_review_state
		WHERE user_id = $1 AND grp = $3 AND word = ANY($2)`, userID, words, group)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]reviewRow{}
	for rows.Next() {
		var w string
		var r reviewRow
		if err := rows.Scan(&w, &r.due, &r.learning); err != nil {
			return nil, err
		}
		out[w] = r
	}
	return out, rows.Err()
}
