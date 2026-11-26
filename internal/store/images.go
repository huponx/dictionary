package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Image is a row of dictionary_images; the bytes are a file (the filestore package).
type Image struct {
	ID          string
	UserID      string
	ContentType string
	Size        int
	SHA256      []byte
}

// ImageBytes is the total size of the images of the user.
func (s *Store) ImageBytes(ctx context.Context, userID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT image_bytes FROM dictionary_user_stats WHERE user_id = $1`, userID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, mapErr(err)
}

// ImageBySHA finds the image of the user with the hash; ErrNotFound when there is none.
func (s *Store) ImageBySHA(ctx context.Context, userID string, sha []byte) (*Image, error) {
	return s.image(ctx, `SELECT id, user_id, content_type, size, sha256 FROM dictionary_images WHERE user_id = $1 AND sha256 = $2`, userID, sha)
}

// GetImage returns the image of the user; ErrNotFound for an unknown id and for the image of another user.
func (s *Store) GetImage(ctx context.Context, userID, id string) (*Image, error) {
	return s.image(ctx, `SELECT id, user_id, content_type, size, sha256 FROM dictionary_images WHERE user_id = $1 AND id = $2::uuid`, userID, id)
}

func (s *Store) image(ctx context.Context, q string, args ...any) (*Image, error) {
	var im Image
	err := s.pool.QueryRow(ctx, q, args...).Scan(&im.ID, &im.UserID, &im.ContentType, &im.Size, &im.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &im, nil
}

// ImagesByIDs returns the images of the user among the ids (the unknown ones are missing from the result).
func (s *Store) ImagesByIDs(ctx context.Context, userID string, ids []string) ([]Image, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, user_id, content_type, size, sha256 FROM dictionary_images
		WHERE user_id = $1 AND id = ANY($2::uuid[])`, userID, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var im Image
		if err := rows.Scan(&im.ID, &im.UserID, &im.ContentType, &im.Size, &im.SHA256); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// InsertImage records an image. When the user already has an image with the hash (a parallel upload of the same
// picture) nothing is inserted and the existing one is returned with inserted false.
func (s *Store) InsertImage(ctx context.Context, im Image) (existing *Image, inserted bool, err error) {
	var n int
	err = s.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO dictionary_images (id, user_id, content_type, size, sha256)
			VALUES ($1::uuid, $2, $3, $4, $5) ON CONFLICT (user_id, sha256) DO NOTHING RETURNING user_id, size
		), u AS (
			INSERT INTO dictionary_user_stats AS st (user_id, image_bytes) SELECT user_id, size FROM ins
			ON CONFLICT (user_id) DO UPDATE SET image_bytes = st.image_bytes + EXCLUDED.image_bytes
		) SELECT count(*) FROM ins`,
		im.ID, im.UserID, im.ContentType, im.Size, im.SHA256).Scan(&n)
	if err != nil {
		return nil, false, mapErr(err)
	}
	if n == 1 {
		return &im, true, nil
	}
	ex, err := s.ImageBySHA(ctx, im.UserID, im.SHA256)
	return ex, false, err
}

// DeleteUnusedImages deletes up to limit images that no word refers to and that were uploaded before the
// cutoff, and returns them so the caller can delete their files. Several replicas can run it at once.
func (s *Store) DeleteUnusedImages(ctx context.Context, cutoff time.Time, limit int) ([]Image, error) {
	rows, err := s.pool.Query(ctx, `
		WITH del AS (
		DELETE FROM dictionary_images i USING (
			SELECT id FROM dictionary_images x
			WHERE x.created_at < $1 AND NOT EXISTS (SELECT 1 FROM dictionary_image_refs r WHERE r.image_id = x.id)
			ORDER BY x.created_at LIMIT $2 FOR UPDATE SKIP LOCKED
		) d WHERE i.id = d.id
		RETURNING i.id, i.user_id, i.content_type, i.size, i.sha256
		), u AS (
			UPDATE dictionary_user_stats st SET image_bytes = greatest(st.image_bytes - x.n, 0)
			FROM (SELECT user_id, sum(size) AS n FROM del GROUP BY user_id) x WHERE st.user_id = x.user_id
		) SELECT id, user_id, content_type, size, sha256 FROM del`, cutoff, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var im Image
		if err := rows.Scan(&im.ID, &im.UserID, &im.ContentType, &im.Size, &im.SHA256); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// ExistingImageIDs returns which of the ids have a row.
func (s *Store) ExistingImageIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM dictionary_images WHERE id = ANY($1::uuid[])`, ids)
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
