package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// StorageUsage is what a user stores and the limits an administrator set for the user, in bytes. A limit that is
// nil is not set: the system setting applies.
type StorageUsage struct {
	ContentBytes, ImageBytes, AudioBytes int64
	ContentQuota, ImageQuota, AudioQuota *int64
}

// Quotas are the own limits of a user; nil leaves the setting of the system in effect.
type Quotas struct {
	Content, Image, Audio *int64
}

// GetStorageUsage returns what the user stores and the own limits of the user (a user without a counter row
// stores nothing and has no limits); ErrUnknownUser when there is no such user. It reads the counters, not the rows.
func (s *Store) GetStorageUsage(ctx context.Context, userID string) (StorageUsage, error) {
	var u StorageUsage
	err := s.pool.QueryRow(ctx, `SELECT coalesce(t.content_bytes, 0), coalesce(t.image_bytes, 0), coalesce(t.audio_bytes, 0),
			t.content_quota, t.image_quota, t.audio_quota
		FROM identity_users i LEFT JOIN dictionary_user_stats t ON t.user_id = i.id WHERE i.id = $1`, userID).
		Scan(&u.ContentBytes, &u.ImageBytes, &u.AudioBytes, &u.ContentQuota, &u.ImageQuota, &u.AudioQuota)
	if errors.Is(err, pgx.ErrNoRows) {
		return StorageUsage{}, ErrUnknownUser
	}
	return u, mapErr(err)
}

// SetStorageQuota sets the own limits of the user, all three at once: a nil one removes the limit of that kind.
// The counter row is created when the user has none; ErrUnknownUser when there is no such user.
func (s *Store) SetStorageQuota(ctx context.Context, userID string, q Quotas) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id, content_quota, image_quota, audio_quota)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET content_quota = EXCLUDED.content_quota, image_quota = EXCLUDED.image_quota,
			audio_quota = EXCLUDED.audio_quota`, userID, q.Content, q.Image, q.Audio)
	return mapErr(err)
}
