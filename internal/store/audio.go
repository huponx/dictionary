package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Audio is a row of dictionary_audio; the bytes are a file (the filestore package).
type Audio struct {
	ID          string
	UserID      string
	ContentType string
	Size        int
	SHA256      []byte
}

// AudioBytes is the total size of the audio files of the user.
func (s *Store) AudioBytes(ctx context.Context, userID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT audio_bytes FROM dictionary_user_stats WHERE user_id = $1`, userID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, mapErr(err)
}

// AudioBySHA finds the audio file of the user with the hash; ErrNotFound when there is none.
func (s *Store) AudioBySHA(ctx context.Context, userID string, sha []byte) (*Audio, error) {
	return s.audio(ctx, `SELECT id, user_id, content_type, size, sha256 FROM dictionary_audio WHERE user_id = $1 AND sha256 = $2`, userID, sha)
}

// GetAudio returns the audio file of the user; ErrNotFound for an unknown id and for the file of another user.
func (s *Store) GetAudio(ctx context.Context, userID, id string) (*Audio, error) {
	return s.audio(ctx, `SELECT id, user_id, content_type, size, sha256 FROM dictionary_audio WHERE user_id = $1 AND id = $2::uuid`, userID, id)
}

func (s *Store) audio(ctx context.Context, q string, args ...any) (*Audio, error) {
	var a Audio
	err := s.pool.QueryRow(ctx, q, args...).Scan(&a.ID, &a.UserID, &a.ContentType, &a.Size, &a.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

// AudioByIDs returns the audio files of the user among the ids (the unknown ones are missing from the result).
func (s *Store) AudioByIDs(ctx context.Context, userID string, ids []string) ([]Audio, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, user_id, content_type, size, sha256 FROM dictionary_audio
		WHERE user_id = $1 AND id = ANY($2::uuid[])`, userID, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanAudio(rows)
}

func scanAudio(rows pgx.Rows) ([]Audio, error) {
	defer rows.Close()
	var out []Audio
	for rows.Next() {
		var a Audio
		if err := rows.Scan(&a.ID, &a.UserID, &a.ContentType, &a.Size, &a.SHA256); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertAudio records an audio file. When the user already has a file with the hash (a parallel upload of the
// same file) nothing is inserted and the existing one is returned with inserted false.
func (s *Store) InsertAudio(ctx context.Context, a Audio) (existing *Audio, inserted bool, err error) {
	var n int
	err = s.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO dictionary_audio (id, user_id, content_type, size, sha256)
			VALUES ($1::uuid, $2, $3, $4, $5) ON CONFLICT (user_id, sha256) DO NOTHING RETURNING user_id, size
		), u AS (
			INSERT INTO dictionary_user_stats AS st (user_id, audio_bytes) SELECT user_id, size FROM ins
			ON CONFLICT (user_id) DO UPDATE SET audio_bytes = st.audio_bytes + EXCLUDED.audio_bytes
		) SELECT count(*) FROM ins`,
		a.ID, a.UserID, a.ContentType, a.Size, a.SHA256).Scan(&n)
	if err != nil {
		return nil, false, mapErr(err)
	}
	if n == 1 {
		return &a, true, nil
	}
	ex, err := s.AudioBySHA(ctx, a.UserID, a.SHA256)
	return ex, false, err
}

// DeleteUnusedAudio deletes up to limit audio files that no word refers to and that were uploaded before the
// cutoff, and returns them so the caller can delete their files. Several replicas can run it at once.
func (s *Store) DeleteUnusedAudio(ctx context.Context, cutoff time.Time, limit int) ([]Audio, error) {
	rows, err := s.pool.Query(ctx, `
		WITH del AS (
		DELETE FROM dictionary_audio a USING (
			SELECT id FROM dictionary_audio x
			WHERE x.created_at < $1 AND NOT EXISTS (SELECT 1 FROM dictionary_audio_refs r WHERE r.audio_id = x.id)
			ORDER BY x.created_at LIMIT $2 FOR UPDATE SKIP LOCKED
		) d WHERE a.id = d.id
		RETURNING a.id, a.user_id, a.content_type, a.size, a.sha256, a.released
		), u AS (
			UPDATE dictionary_user_stats st SET audio_bytes = greatest(st.audio_bytes - x.n, 0)
			FROM (SELECT user_id, sum(size) FILTER (WHERE NOT released) AS n FROM del GROUP BY user_id) x WHERE st.user_id = x.user_id AND x.n > 0
		) SELECT id, user_id, content_type, size, sha256 FROM del`, cutoff, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanAudio(rows)
}

// ExistingAudioIDs returns which of the ids have a row.
func (s *Store) ExistingAudioIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM dictionary_audio WHERE id = ANY($1::uuid[])`, ids)
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
