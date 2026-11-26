package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The purge of the data of a deleted account (identity DeleteUser sets identity_users.deleted_at). A direct DELETE of
// the user would cascade over every table of the user in one statement; here each call deletes at most one batch, in
// its own short transaction, and the user row goes last, when nothing refers to it.

// DeletedUser is an account to purge or to finish deleting.
type DeletedUser struct {
	ID        string
	DeletedAt time.Time
	// Purged: the data was found deleted (purged_at is set); the user only waits for the grace period.
	Purged bool
}

// DeletedUsers returns up to limit deleted users that need work, the oldest first: the ones whose data is not purged,
// and the purged ones whose grace period has passed. Users waiting for the end of the grace period are not returned,
// so they cannot crowd out the new ones.
func (s *Store) DeletedUsers(ctx context.Context, grace time.Duration, limit int) ([]DeletedUser, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, deleted_at, purged_at IS NOT NULL FROM identity_users
		WHERE deleted_at IS NOT NULL AND (purged_at IS NULL OR deleted_at < now() - make_interval(secs => $1))
		ORDER BY deleted_at LIMIT $2`, grace.Seconds(), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	us, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeletedUser, error) {
		var u DeletedUser
		return u, r.Scan(&u.ID, &u.DeletedAt, &u.Purged)
	})
	return us, mapErr(err)
}

// purgeLockTimeout bounds the wait for a row lock of one batch (a replica or a request holding it gives up the
// batch, which is retried on the next tick).
const purgeLockTimeout = "5s"

// advisoryLock is the lock of SaveWord: "dictionary:"+user. The purge only tries it, and skips a user whose lock is
// taken (a save, or another replica purging the user).
const advisoryLock = `pg_try_advisory_xact_lock(hashtextextended($1, 0))`

// purgeSteps are the deletes of a user, in the order they can run: the saved words first (the references to the
// images and the audio, the review state and log and the category links go with them), then the images and the audio
// (the rows; the files are deleted by directory), the export jobs (their parts go with them), the categories, and the small
// tables last. A step with LIMIT deletes one batch ($2 rows).
var purgeSteps = []struct{ table, sql string }{
	{"saved_words", batchDelete("dictionary_saved_words")},
	{"images", batchDelete("dictionary_images")},
	{"audio", batchDelete("dictionary_audio")},
	{"export_jobs", batchDelete("dictionary_export_jobs")},
	{"categories", batchDelete("dictionary_categories")},
	{"deck_links", `DELETE FROM dictionary_deck_links WHERE user_id = $1 AND $2 > 0`},
	// no foreign key: the cascade would miss it
	{"review_bonus", `DELETE FROM dictionary_review_bonus WHERE user_id = $1 AND $2 > 0`},
	{"fsrs_params", `DELETE FROM dictionary_fsrs_params WHERE user_id = $1 AND $2 > 0`},
	{"user_stats", `DELETE FROM dictionary_user_stats WHERE user_id = $1 AND $2 > 0`},
}

// batchDelete deletes $2 rows of the user $1 of the table. The rows are picked once, by ctid (the ARRAY is evaluated
// before the delete): a join with a LIMIT subquery can be planned as a nested loop that reads the whole list of the
// user again for every row it deletes.
func batchDelete(table string) string {
	return `DELETE FROM ` + table + ` WHERE ctid = ANY (ARRAY(SELECT ctid FROM ` + table + ` WHERE user_id = $1 LIMIT $2))`
}

// PurgeStep deletes the next batch (at most batch rows) of the data of a deleted user in one transaction and
// returns the table and the number of rows it deleted. clean is true (and n 0) when the user has no data left. busy is
// true when somebody else holds the lock of the user (a save, another replica); nothing was deleted then. The user must
// be deleted (deleted_at set), or nothing happens and the error is ErrNotFound.
func (s *Store) PurgeStep(ctx context.Context, userID string, batch int) (table string, n int64, clean, busy bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, purgeLockTimeout); err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT `+advisoryLock, "dictionary:"+userID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			busy = true
			return nil
		}
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM identity_users WHERE id = $1`, userID).Scan(&deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !deleted {
			return ErrNotFound
		}
		for _, st := range purgeSteps {
			tag, err := tx.Exec(ctx, st.sql, userID, batch)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				table, n = st.table, tag.RowsAffected()
				return nil
			}
		}
		clean = true
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return "", 0, false, false, err
	}
	return table, n, clean, busy, mapErr(err)
}

// MarkPurged records that the data of the deleted user is gone (the user then waits for the grace period).
func (s *Store) MarkPurged(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE identity_users SET purged_at = now() WHERE id = $1 AND deleted_at IS NOT NULL AND purged_at IS NULL`, userID)
	return mapErr(err)
}

// dictionaryEmpty is true when no table of the dictionary has a row of the user ($1).
const dictionaryEmpty = `NOT EXISTS (SELECT 1 FROM dictionary_saved_words WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_images WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_audio WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_export_jobs WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_categories WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_deck_links WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_review_bonus WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_fsrs_params WHERE user_id = $1)
	AND NOT EXISTS (SELECT 1 FROM dictionary_user_stats WHERE user_id = $1)`

// FinishUserDelete deletes the row of a deleted, purged user whose grace period has passed, when the dictionary holds
// nothing of it any more (the cascade then has nothing to do). It holds the lock of SaveWord, so a save cannot slip a row
// in between. When rows were written after the purge it clears purged_at, so the purge runs again, and returns
// false. It also returns false when the grace period is not over or the lock is taken.
func (s *Store) FinishUserDelete(ctx context.Context, userID string, grace time.Duration) (deleted bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, purgeLockTimeout); err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT `+advisoryLock, "dictionary:"+userID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return nil
		}
		tag, err := tx.Exec(ctx, `DELETE FROM identity_users WHERE id = $1 AND deleted_at IS NOT NULL AND purged_at IS NOT NULL
			AND deleted_at < now() - make_interval(secs => $2) AND `+dictionaryEmpty, userID, grace.Seconds())
		if err != nil {
			return err
		}
		if deleted = tag.RowsAffected() > 0; deleted {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE identity_users SET purged_at = NULL WHERE id = $1 AND deleted_at IS NOT NULL
			AND purged_at IS NOT NULL AND NOT (`+dictionaryEmpty+`)`, userID)
		return err
	})
	return deleted, mapErr(err)
}
