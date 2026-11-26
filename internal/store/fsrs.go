package store

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/huponx/dictionary/internal/srs"
	"github.com/jackc/pgx/v5"
)

// FSRSParams are the FSRS weights fitted to a user and how good the fit was.
type FSRSParams struct {
	// Weights are the fitted weights; nil when the last try found nothing better than the defaults.
	Weights                  *srs.Weights
	ReviewCount              int
	LossDefault, LossTrained float64
	TrainedAt                time.Time
}

// FSRSParams returns the stored parameters of the user; nil when there is no row or no fitted weights.
func (s *Store) FSRSParams(ctx context.Context, userID string) (*FSRSParams, error) {
	var w []float32
	var p FSRSParams
	var ld, lt *float32
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT weights, review_count, loss_default, loss_trained, trained_at
		FROM dictionary_fsrs_params WHERE user_id = $1`, userID).Scan(&w, &p.ReviewCount, &ld, &lt, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	if len(w) != srs.NumWeights {
		return nil, nil
	}
	var weights srs.Weights
	for i, x := range w {
		weights[i] = float64(x)
	}
	p.Weights = &weights
	if ld != nil {
		p.LossDefault = float64(*ld)
	}
	if lt != nil {
		p.LossTrained = float64(*lt)
	}
	if at != nil {
		p.TrainedAt = *at
	}
	return &p, nil
}

// PutFSRSParams stores fitted weights of the user.
func (s *Store) PutFSRSParams(ctx context.Context, userID string, p FSRSParams) error {
	w := make([]float32, srs.NumWeights)
	for i, x := range p.Weights {
		w[i] = float32(x)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dictionary_fsrs_params (user_id, weights, review_count, loss_default, loss_trained, trained_at, attempted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (user_id) DO UPDATE SET weights = EXCLUDED.weights, review_count = EXCLUDED.review_count,
			loss_default = EXCLUDED.loss_default, loss_trained = EXCLUDED.loss_trained, trained_at = EXCLUDED.trained_at`,
		userID, w, p.ReviewCount, float32(p.LossDefault), float32(p.LossTrained), p.TrainedAt)
	return mapErr(err)
}

// DeleteFSRSParams forgets the fitted weights of the user (the defaults apply again). The row stays so the rate
// limit of ClaimOptimize cannot be skipped by resetting.
func (s *Store) DeleteFSRSParams(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE dictionary_fsrs_params SET weights = NULL, review_count = 0, loss_default = NULL,
		loss_trained = NULL, trained_at = NULL WHERE user_id = $1`, userID)
	return mapErr(err)
}

// ClaimOptimize records an optimization try of the user at now. It reports false when the last try was less than gap
// ago (on any replica): the rate limit of the optimizer lives in the database, not in the memory of a process.
func (s *Store) ClaimOptimize(ctx context.Context, userID string, now time.Time, gap time.Duration) (bool, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO dictionary_fsrs_params AS p (user_id, attempted_at) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET attempted_at = EXCLUDED.attempted_at
			WHERE p.attempted_at <= $2::timestamptz - $3::interval
		RETURNING attempted_at`, userID, now, gap).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, mapErr(err)
}

// LockOptimize takes the optimizer lock of the user, held until the returned function runs; false when another run of
// the user holds it. It uses a dedicated connection, because the lock belongs to the session.
func (s *Store) LockOptimize(ctx context.Context, userID string) (unlock func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, mapErr(err)
	}
	key := "dictionary:fsrs:" + userID
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, mapErr(err)
	}
	return func() {
		// not the request context: the lock must go even when it was cancelled
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			conn.Conn().Close(uctx) // closing the session drops the lock
		}
		conn.Release()
	}, true, nil
}

// maxLogReviews is how many of the latest grades of a user the optimizer reads (enough for srs.MaxSamples samples).
const maxLogReviews = 75000

// ReviewLogs returns the review history the optimizer fits: the cards of the latest grades of the user (about limit
// of them), each with its whole history in time order, the cards ordered by their first grade.
func (s *Store) ReviewLogs(ctx context.Context, userID string, limit int) ([][]srs.Review, error) {
	limit = min(limit, maxLogReviews)
	rows, err := s.pool.Query(ctx, `
		WITH recent AS (
			SELECT DISTINCT word FROM (
				SELECT word FROM dictionary_review_log WHERE user_id = $1 ORDER BY reviewed_at DESC LIMIT $2
			) t
		)
		SELECT l.word, l.reviewed_at, l.grade, l.elapsed_seconds
		FROM recent JOIN dictionary_review_log l ON l.user_id = $1 AND l.word = recent.word
		ORDER BY l.word, l.reviewed_at`, userID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out [][]srs.Review
	var cur string
	for rows.Next() {
		var word string
		var at time.Time
		var grade int16
		var elapsed int64
		if err := rows.Scan(&word, &at, &grade, &elapsed); err != nil {
			return nil, err
		}
		if word != cur || len(out) == 0 {
			out = append(out, nil)
			cur = word
		}
		out[len(out)-1] = append(out[len(out)-1], srs.Review{Grade: srs.Grade(grade), At: at.Unix(), Elapsed: elapsed})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][0].At < out[j][0].At })
	return out, nil
}

// DeleteReviewLogsBefore deletes up to limit log rows older than t (the BRIN index finds them) and returns how many.
func (s *Store) DeleteReviewLogsBefore(ctx context.Context, t time.Time, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dictionary_review_log WHERE ctid IN (
		SELECT ctid FROM dictionary_review_log WHERE reviewed_at < $1 LIMIT $2)`, t, limit)
	return tag.RowsAffected(), mapErr(err)
}
