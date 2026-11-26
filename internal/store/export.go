package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrJobActive: the user already has a queued or running export job.
	ErrJobActive = errors.New("store: an export job is active")
	// ErrLeaseLost: the job was cancelled, finished, deleted or taken by another replica; the worker must stop.
	ErrLeaseLost = errors.New("store: export lease lost")
)

// Statuses of an export job.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// ExportParams is what a job exports and how, fixed when it starts. The preferences of the user (fresh, remove)
// are read at the start because the worker has no request to read them with.
type ExportParams struct {
	All           bool   `json:"all"` // scope: every word of the list, the exported ones too
	CategoryID    string `json:"categoryId,omitempty"`
	Uncategorized bool   `json:"uncategorized,omitempty"`
	DeckName      string `json:"deckName"`
	Fresh         bool   `json:"fresh"`
	Remove        bool   `json:"remove"`
	// AfterCreated, AfterWord: the key of the last word packed; the job goes on after it.
	AfterCreated time.Time `json:"afterCreated"`
	AfterWord    string    `json:"afterWord"`
}

// Filter is the category filter of the job.
func (p ExportParams) Filter() WordFilter {
	return WordFilter{CategoryID: p.CategoryID, Uncategorized: p.Uncategorized}
}

// Scope is the scope of the job.
func (p ExportParams) Scope() Scope {
	if p.All {
		return ScopeAll
	}
	return ScopeNew
}

// ExportJob is a row of dictionary_export_jobs.
type ExportJob struct {
	ID         string
	UserID     string
	Status     string
	Params     ExportParams
	Total      int
	Built      int
	Building   int // words of the part being built that are done (an estimate)
	Parts      int
	Error      string
	LeaseID    string
	CreatedAt  time.Time
	FinishedAt time.Time // zero while it runs
	ExpiresAt  time.Time
}

// ExportPart is a row of dictionary_export_parts: one deck file of a job.
type ExportPart struct {
	N         int
	Filename  string
	FileID    string
	WordCount int
	Size      int
}

const jobColumns = `id::text, user_id::text, status, params, total, built, building, parts, error, coalesce(lease_id::text, ''),
	created_at, finished_at, expires_at`

func scanJob(row pgx.Row) (ExportJob, error) {
	var j ExportJob
	var params []byte
	var fin *time.Time
	if err := row.Scan(&j.ID, &j.UserID, &j.Status, &params, &j.Total, &j.Built, &j.Building, &j.Parts, &j.Error, &j.LeaseID,
		&j.CreatedAt, &fin, &j.ExpiresAt); err != nil {
		return ExportJob{}, err
	}
	if fin != nil {
		j.FinishedAt = *fin
	}
	return j, json.Unmarshal(params, &j.Params)
}

// PendingPage returns up to limit words of the list, oldest first (the order of the cards of a deck), after the key
// (afterCreated, afterWord) when afterWord is not empty: the words of the list that were never exported (ScopeNew)
// or every word (ScopeAll) that pass the filter. A job reads its words with it, one batch at a time.
func (s *Store) PendingPage(ctx context.Context, userID string, scope Scope, f WordFilter, afterCreated time.Time, afterWord string, limit int) ([]SavedWord, error) {
	args := []any{userID}
	src := f.source(&args)
	q := `SELECT w.word, w.content, w.audio_uk, w.audio_us, w.created_at, w.exported_at IS NOT NULL FROM ` + src.from + ` WHERE ` + src.where
	if scope != ScopeAll {
		q += ` AND w.exported_at IS NULL`
	}
	if afterWord != "" {
		args = append(args, afterCreated, afterWord)
		q += ` AND (` + src.created + `, ` + src.word + `) > ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, q+` ORDER BY `+src.created+`, `+src.word+` LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanWords(rows)
}

// CountExportable is the exact number of words a job with the scope and the filter packs. It reads them all, so
// only a job (not a request) calls it.
func (s *Store) CountExportable(ctx context.Context, userID string, scope Scope, f WordFilter) (int, error) {
	args := []any{userID}
	src := f.source(&args)
	q := `SELECT count(*) FROM ` + src.from + ` WHERE ` + src.where
	if scope != ScopeAll {
		q += ` AND w.exported_at IS NULL`
	}
	var n int
	err := s.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, mapErr(err)
}

// CreateExportJob queues a job that expires at now+ttl; ErrJobActive when the user has one queued or running.
func (s *Store) CreateExportJob(ctx context.Context, userID string, p ExportParams, ttl time.Duration) (ExportJob, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return ExportJob{}, err
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `INSERT INTO dictionary_export_jobs (user_id, params, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3)) RETURNING `+jobColumns, userID, raw, ttl.Seconds()))
	return j, mapErr(err)
}

// GetExportJob returns the job of the user and its parts; an empty or "latest" id is the job started last.
// ErrNotFound for none, an unknown id, and the job of another user.
func (s *Store) GetExportJob(ctx context.Context, userID, id string) (ExportJob, []ExportPart, error) {
	var row pgx.Row
	if id == "" || id == "latest" {
		row = s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM dictionary_export_jobs WHERE user_id = $1
			AND expires_at > now() ORDER BY created_at DESC LIMIT 1`, userID)
	} else {
		jid, err := uuid.Parse(id)
		if err != nil {
			return ExportJob{}, nil, ErrNotFound
		}
		row = s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM dictionary_export_jobs WHERE user_id = $1 AND id = $2`, userID, jid)
	}
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExportJob{}, nil, ErrNotFound
	}
	if err != nil {
		return ExportJob{}, nil, mapErr(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT n, filename, file_id::text, word_count, size FROM dictionary_export_parts
		WHERE job_id = $1 ORDER BY n`, j.ID)
	if err != nil {
		return ExportJob{}, nil, mapErr(err)
	}
	defer rows.Close()
	var parts []ExportPart
	for rows.Next() {
		var p ExportPart
		if err := rows.Scan(&p.N, &p.Filename, &p.FileID, &p.WordCount, &p.Size); err != nil {
			return ExportJob{}, nil, err
		}
		parts = append(parts, p)
	}
	return j, parts, rows.Err()
}

// CancelExportJob stops a queued or running job (its files stay until it expires, ttl from now); a finished one is
// returned as it is. ErrNotFound for an unknown job.
func (s *Store) CancelExportJob(ctx context.Context, userID, id string, ttl time.Duration) (ExportJob, error) {
	jid, err := uuid.Parse(id)
	if err != nil {
		return ExportJob{}, ErrNotFound
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `UPDATE dictionary_export_jobs SET status = 'cancelled', finished_at = now(),
		expires_at = now() + make_interval(secs => $3), building = 0, lease_id = NULL, lease_until = NULL
		WHERE user_id = $1 AND id = $2 AND status IN ('queued', 'running') RETURNING `+jobColumns, userID, jid, ttl.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		j, err = scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM dictionary_export_jobs WHERE user_id = $1 AND id = $2`, userID, jid))
		if errors.Is(err, pgx.ErrNoRows) {
			return ExportJob{}, ErrNotFound
		}
	}
	return j, mapErr(err)
}

// ClaimExportJob takes the oldest queued job, or running job whose lease ended (its replica died), for leaseFor;
// nil when there is none. Several replicas can call it at once (SKIP LOCKED).
func (s *Store) ClaimExportJob(ctx context.Context, leaseFor time.Duration) (*ExportJob, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `UPDATE dictionary_export_jobs
		SET status = 'running', building = 0, lease_id = gen_random_uuid(), lease_until = now() + make_interval(secs => $1)
		WHERE id = (SELECT id FROM dictionary_export_jobs
			WHERE (status = 'queued' OR (status = 'running' AND lease_until < now())) AND expires_at > now()
			ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+jobColumns, leaseFor.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &j, nil
}

// RenewExportLease extends the lease; ErrLeaseLost when the job is not running under that lease any more.
func (s *Store) RenewExportLease(ctx context.Context, id, leaseID string, leaseFor time.Duration) error {
	tag, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET lease_until = now() + make_interval(secs => $3)
		WHERE id = $1 AND lease_id = $2::uuid AND status = 'running'`, id, leaseID, leaseFor.Seconds())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// SetExportTotal records how many words the job packs.
func (s *Store) SetExportTotal(ctx context.Context, id, leaseID string, total int) error {
	tag, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET total = $3 WHERE id = $1 AND lease_id = $2::uuid AND status = 'running'`,
		id, leaseID, total)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// SetExportBuilding records how many words of the part being built are done (the page shows built + building, which
// must not go down: AddExportPart keeps what is left of it). A job that lost its lease changes
// nothing (the worker learns it when it renews the lease).
func (s *Store) SetExportBuilding(ctx context.Context, id, leaseID string, n int) error {
	_, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET building = $3 WHERE id = $1 AND lease_id = $2::uuid AND status = 'running'`,
		id, leaseID, n)
	return mapErr(err)
}

// AddExportPart records a part (its file is already written) and marks its words exported (and out of the list
// with remove), in one transaction, so a job that dies between parts loses no word and marks none that is not in a
// file. after is the key of the last word of the part: the job goes on after it. p.N must be the next part number;
// ErrLeaseLost when the job is not running under the lease (or the part number is not the next one).
func (s *Store) AddExportPart(ctx context.Context, job ExportJob, leaseID string, p ExportPart, words []string, after SavedWord) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var parts int
		err := tx.QueryRow(ctx, `SELECT parts FROM dictionary_export_jobs WHERE id = $1 AND lease_id = $2::uuid AND status = 'running' FOR UPDATE`,
			job.ID, leaseID).Scan(&parts)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && parts != p.N-1) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dictionary_export_parts (job_id, n, filename, file_id, word_count, size)
			VALUES ($1, $2, $3, $4::uuid, $5, $6)`, job.ID, p.N, p.Filename, p.FileID, p.WordCount, p.Size); err != nil {
			return err
		}
		// advisory lock of the user, like SaveWord: the counter of the list moves with the rows
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "dictionary:"+job.UserID); err != nil {
			return err
		}
		n, bytes, err := changedRows(ctx, tx, `UPDATE dictionary_saved_words
			SET exported_at = now(), removed_at = CASE WHEN $3 THEN now() END
			WHERE user_id = $1 AND removed_at IS NULL AND word = ANY($2)`, job.UserID, words, job.Params.Remove)
		if err != nil {
			return err
		}
		if job.Params.Remove {
			if err := addListed(ctx, tx, job.UserID, -n, -bytes); err != nil {
				return err
			}
		}
		key, err := json.Marshal(map[string]any{"afterCreated": after.CreatedAt, "afterWord": after.Word})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE dictionary_export_jobs SET parts = parts + 1, built = built + $2, building = GREATEST(building - $2, 0), params = params || $3::jsonb
			WHERE id = $1`, job.ID, len(words), key)
		return err
	})
	return mapErr(err)
}

// FinishExportJob ends a running job with status done or failed (message in errMsg); its files stay until ttl from
// now. ErrLeaseLost when it is not running under the lease.
func (s *Store) FinishExportJob(ctx context.Context, id, leaseID, status, errMsg string, ttl time.Duration) error {
	tag, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET status = $3, error = $4, finished_at = now(),
		expires_at = now() + make_interval(secs => $5), building = 0, lease_id = NULL, lease_until = NULL
		WHERE id = $1 AND lease_id = $2::uuid AND status = 'running'`, id, leaseID, status, errMsg, ttl.Seconds())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// SetExportPartToken stores the hash of the token of the download link of a part of the job of the user, and returns
// the part and when the link stops working (the job expires). ErrNotFound for an unknown part or an expired job.
func (s *Store) SetExportPartToken(ctx context.Context, userID, id string, n int, hash []byte) (ExportPart, time.Time, error) {
	jid, err := uuid.Parse(id)
	if err != nil {
		return ExportPart{}, time.Time{}, ErrNotFound
	}
	var p ExportPart
	var expires time.Time
	err = s.pool.QueryRow(ctx, `UPDATE dictionary_export_parts p SET token_hash = $4 FROM dictionary_export_jobs j
		WHERE j.id = p.job_id AND j.user_id = $1 AND p.job_id = $2 AND p.n = $3 AND j.expires_at > now()
		RETURNING p.n, p.filename, p.file_id::text, p.word_count, p.size, j.expires_at`, userID, jid, n, hash).
		Scan(&p.N, &p.Filename, &p.FileID, &p.WordCount, &p.Size, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExportPart{}, time.Time{}, ErrNotFound
	}
	return p, expires, mapErr(err)
}

// ExportPartByToken finds the part behind the hash of a link token and the user it belongs to; ErrNotFound for an
// unknown token and for the part of a job that expired at now.
func (s *Store) ExportPartByToken(ctx context.Context, hash []byte, now time.Time) (userID string, p ExportPart, err error) {
	err = s.pool.QueryRow(ctx, `SELECT j.user_id::text, p.n, p.filename, p.file_id::text, p.word_count, p.size
		FROM dictionary_export_parts p JOIN dictionary_export_jobs j ON j.id = p.job_id
		WHERE p.token_hash = $1 AND j.expires_at > $2`, hash, now).
		Scan(&userID, &p.N, &p.Filename, &p.FileID, &p.WordCount, &p.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ExportPart{}, ErrNotFound
	}
	return userID, p, mapErr(err)
}

// ExpiredFile is a file of an export part whose job was deleted: the caller deletes it from the volume.
type ExpiredFile struct {
	UserID, FileID string
}

// DeleteExpiredExportJobs deletes up to limit jobs that expired before now (their parts go with them) and returns
// the files of the parts. Several replicas can run it at once.
func (s *Store) DeleteExpiredExportJobs(ctx context.Context, now time.Time, limit int) (jobs int, files []ExpiredFile, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM dictionary_export_jobs WHERE expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		jobs = len(ids)
		if jobs == 0 {
			return nil
		}
		prows, err := tx.Query(ctx, `SELECT j.user_id::text, p.file_id::text FROM dictionary_export_parts p
			JOIN dictionary_export_jobs j ON j.id = p.job_id WHERE p.job_id = ANY($1::uuid[])`, ids)
		if err != nil {
			return err
		}
		files, err = pgx.CollectRows(prows, func(r pgx.CollectableRow) (ExpiredFile, error) {
			var f ExpiredFile
			return f, r.Scan(&f.UserID, &f.FileID)
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM dictionary_export_jobs WHERE id = ANY($1::uuid[])`, ids)
		return err
	})
	return jobs, files, mapErr(err)
}

// ExistingExportFileIDs returns which of the ids are the file of a recorded part of an export job.
func (s *Store) ExistingExportFileIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT file_id::text FROM dictionary_export_parts WHERE file_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
