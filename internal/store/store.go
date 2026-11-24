// Package store is the PostgreSQL access of the dictionary service. The schema lives in the
// db-migration repository (dictionary_* tables).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/huponx/dictionary/internal/cardref"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrLimit: the user already has the maximum number of pending words.
	ErrLimit = errors.New("store: too many saved words")
	// ErrNotFound: no such pending word.
	ErrNotFound = errors.New("store: not found")
	// ErrUnknownImage: a card refers to an image that does not exist or belongs to another user.
	ErrUnknownImage = errors.New("store: unknown image")
	// ErrUnknownUser: the user does not exist (foreign key of dictionary_saved_words).
	ErrUnknownUser = errors.New("store: unknown user")
)

// SavedWord is a row of dictionary_saved_words.
type SavedWord struct {
	Word      string
	Content   string
	AudioUK   string
	AudioUS   string
	CreatedAt time.Time
	// Exported: the word was packed in a deck (it may still be in the list, see MarkExported).
	Exported bool
}

// Scope tells which words of the list an export packs.
type Scope int

const (
	// ScopeNew: the words that were never exported (the default).
	ScopeNew Scope = iota
	// ScopeAll: every word of the list, the exported ones too.
	ScopeAll
)

// Store wraps the pool.
type Store struct {
	pool *pgxpool.Pool
}

// New connects and pings.
func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Ping is the readiness check.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// SaveWord adds the word to the list of the user (the words with removed_at NULL), or overwrites it (an upsert:
// the card, the audio and the save time, so it is the newest of the list). A word that was already exported is
// new again. It returns the number of words in the list after the save, or ErrLimit when the word is not in the
// list yet and the user already has max words.
func (s *Store) SaveWord(ctx context.Context, userID string, w SavedWord, max int) (pending int, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// serialize the saves of one user so two parallel saves cannot both pass the limit
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "dictionary:"+userID); err != nil {
			return err
		}
		var already bool
		err := tx.QueryRow(ctx, `SELECT true FROM dictionary_saved_words WHERE user_id = $1 AND word = $2 AND removed_at IS NULL`,
			userID, w.Word).Scan(&already)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !already {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL`,
				userID).Scan(&count); err != nil {
				return err
			}
			if count >= max {
				return ErrLimit
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dictionary_saved_words (user_id, word, content, audio_uk, audio_us)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, word) DO UPDATE SET
				content = EXCLUDED.content, audio_uk = EXCLUDED.audio_uk, audio_us = EXCLUDED.audio_us,
				created_at = now(),
				exported_at = NULL, removed_at = NULL`,
			userID, w.Word, w.Content, w.AudioUK, w.AudioUS); err != nil {
			return err
		}
		if err := setImageRefs(ctx, tx, userID, w.Word, cardref.IDs(w.Content)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL`, userID).Scan(&pending)
	})
	return pending, mapErr(err)
}

// setImageRefs replaces the set of images the word uses. Every id must be an image of the user (ErrUnknownImage
// otherwise), so a card cannot point at the image of somebody else.
func setImageRefs(ctx context.Context, tx pgx.Tx, userID, word string, ids []string) error {
	if len(ids) > 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dictionary_images WHERE user_id = $1 AND id = ANY($2::uuid[])`,
			userID, ids).Scan(&n); err != nil {
			return err
		}
		if n != len(ids) {
			return ErrUnknownImage
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dictionary_image_refs WHERE user_id = $1 AND word = $2`, userID, word); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO dictionary_image_refs (user_id, word, image_id)
		SELECT $1, $2, unnest($3::uuid[])`, userID, word, ids)
	return err
}

// CountPending is the number of words in the list of the user, and how many of them are new (never exported).
func (s *Store) CountPending(ctx context.Context, userID string) (total, fresh int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE exported_at IS NULL)
		FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL`, userID).Scan(&total, &fresh)
	return total, fresh, mapErr(err)
}

// ListPending returns up to limit words of the list, newest first (created_at, then word, both descending),
// starting after the key (afterCreated, afterWord) when afterWord is not empty.
func (s *Store) ListPending(ctx context.Context, userID string, afterCreated time.Time, afterWord string, limit int) ([]SavedWord, error) {
	var (
		rows pgx.Rows
		err  error
	)
	const cols = `SELECT word, content, audio_uk, audio_us, created_at, exported_at IS NOT NULL FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL`
	if afterWord == "" {
		rows, err = s.pool.Query(ctx, cols+` ORDER BY created_at DESC, word DESC LIMIT $2`, userID, limit)
	} else {
		rows, err = s.pool.Query(ctx, cols+` AND (created_at, word) < ($2, $3) ORDER BY created_at DESC, word DESC LIMIT $4`,
			userID, afterCreated, afterWord, limit)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return scanWords(rows)
}

// Pending returns the words an export packs, oldest first (the order of the cards of a deck): the words of the
// list that were never exported (ScopeNew), or every word of the list (ScopeAll).
func (s *Store) Pending(ctx context.Context, userID string, scope Scope) ([]SavedWord, error) {
	q := `SELECT word, content, audio_uk, audio_us, created_at, exported_at IS NOT NULL FROM dictionary_saved_words
		WHERE user_id = $1 AND removed_at IS NULL`
	if scope != ScopeAll {
		q += ` AND exported_at IS NULL`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at, word`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanWords(rows)
}

func scanWords(rows pgx.Rows) ([]SavedWord, error) {
	defer rows.Close()
	var out []SavedWord
	for rows.Next() {
		var w SavedWord
		if err := rows.Scan(&w.Word, &w.Content, &w.AudioUK, &w.AudioUS, &w.CreatedAt, &w.Exported); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeletePending removes a word of the list (an exported one that is still in the list too); ErrNotFound when
// the user has no such word.
func (s *Store) DeletePending(ctx context.Context, userID, word string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dictionary_saved_words WHERE user_id = $1 AND word = $2 AND removed_at IS NULL`, userID, word)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkExported sets exported_at on the given words of the list (the ones packed in a deck), which makes them
// "exported". With remove they also leave the list (removed_at), as before; without it they stay in the list.
// Words saved or refreshed after the deck was read are not in the given list and are not touched (a refreshed
// word is new again). It returns the number of rows changed.
func (s *Store) MarkExported(ctx context.Context, userID string, words []string, remove bool) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE dictionary_saved_words
		SET exported_at = now(), removed_at = CASE WHEN $3 THEN now() END
		WHERE user_id = $1 AND removed_at IS NULL AND word = ANY($2)`, userID, words, remove)
	return tag.RowsAffected(), mapErr(err)
}

// RemoveExported takes the words of the list that were exported out of it (removed_at); new words stay. It
// returns how many left the list.
func (s *Store) RemoveExported(ctx context.Context, userID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE dictionary_saved_words SET removed_at = now()
		WHERE user_id = $1 AND removed_at IS NULL AND exported_at IS NOT NULL`, userID)
	return tag.RowsAffected(), mapErr(err)
}

// DeleteRemovedBefore deletes up to limit rows that left the list before the cutoff and returns how many it
// deleted. Several replicas can run it at once (SKIP LOCKED). Words still in the list are never deleted.
func (s *Store) DeleteRemovedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM dictionary_saved_words d USING (
			SELECT user_id, word FROM dictionary_saved_words
			WHERE removed_at < $1 ORDER BY removed_at LIMIT $2 FOR UPDATE SKIP LOCKED
		) x WHERE d.user_id = x.user_id AND d.word = x.word`, cutoff, limit)
	return tag.RowsAffected(), mapErr(err)
}

// DeckLink is a deck waiting to be downloaded.
type DeckLink struct {
	Filename  string
	Data      []byte
	WordCount int
	ExpiresAt time.Time
}

// PutDeckLink stores the deck behind the hash of its token. A user has one link: an earlier one is
// replaced.
func (s *Store) PutDeckLink(ctx context.Context, userID string, tokenHash []byte, l DeckLink) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM dictionary_deck_links WHERE user_id = $1`, userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO dictionary_deck_links (token_hash, user_id, filename, data, word_count, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, tokenHash, userID, l.Filename, l.Data, l.WordCount, l.ExpiresAt)
		return err
	})
	return mapErr(err)
}

// TakeDeckLink returns the deck of the token and deletes it (a link works once); ErrNotFound when the
// token is unknown, used or expired at now.
func (s *Store) TakeDeckLink(ctx context.Context, tokenHash []byte, now time.Time) (*DeckLink, error) {
	var l DeckLink
	err := s.pool.QueryRow(ctx, `DELETE FROM dictionary_deck_links WHERE token_hash = $1 AND expires_at > $2
		RETURNING filename, data, word_count, expires_at`, tokenHash, now).Scan(&l.Filename, &l.Data, &l.WordCount, &l.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &l, nil
}

// DeleteExpiredDeckLinks deletes the links that expired before now and returns how many.
func (s *Store) DeleteExpiredDeckLinks(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dictionary_deck_links WHERE expires_at <= $1`, now)
	return tag.RowsAffected(), mapErr(err)
}

func mapErr(err error) error {
	if err == nil || errors.Is(err, ErrLimit) || errors.Is(err, ErrUnknownImage) {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" { // foreign_key_violation: no such user
		return ErrUnknownUser
	}
	return err
}
