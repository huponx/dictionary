// Package store is the PostgreSQL access of the dictionary service. The schema lives in the
// db-migration repository (dictionary_* tables).
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/huponx/dictionary/internal/cardref"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrLimit: the user already has the maximum number of pending words.
	ErrLimit = errors.New("store: too many saved words")
	// ErrBytesLimit: the content of the words of the user would pass the quota of the user.
	ErrBytesLimit = errors.New("store: content quota exceeded")
	// ErrNotFound: no such pending word.
	ErrNotFound = errors.New("store: not found")
	// ErrUnknownImage: a card refers to an image that does not exist or belongs to another user.
	ErrUnknownImage = errors.New("store: unknown image")
	// ErrUnknownAudio: a card refers to an audio file that does not exist or belongs to another user.
	ErrUnknownAudio = errors.New("store: unknown audio")
	// ErrUnknownUser: the user does not exist (foreign key of dictionary_saved_words).
	ErrUnknownUser = errors.New("store: unknown user")
)

// SavedWord is a row of dictionary_saved_words.
type SavedWord struct {
	// Group of the dictionaries the word belongs to; with the word it is the key of the row (per user).
	Group     string
	Word      string
	Content   string
	AudioUK   string
	AudioUS   string
	CreatedAt time.Time
	// Exported: the word was packed in a deck (it may still be in the list, see MarkExported).
	Exported bool
	// CategoryIDs: the categories of the word (only ListPending fills it).
	CategoryIDs []string
	// NextReview: when the word is due for its next review; the zero time when it was never reviewed (only
	// ListPending fills it).
	NextReview time.Time
	// Learning: the word is in a learning step (only ListPending fills it).
	Learning bool
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

// SaveWord adds the word to the list of the user in the group w.Group (the words with removed_at NULL), or overwrites it (an upsert:
// the card, the audio and the save time, so it is the newest of the list). A word that was already exported is
// new again. It returns the number of words in the list after the save, or ErrLimit when the word is not in the
// list yet and the user already has max words. The content of the list of the user may not pass the quota of the
// user (the own quota or, when the user has none, defaultQuota): a save that would is refused with ErrBytesLimit.
// A word that is new in the list adds its content, one that is in the list the difference to its old content, so
// a save that makes the content smaller is never refused. The word is added to the categories in categoryIDs
// (ErrNotFound for one that is not the user's); the categories it has stay, so saving a word again never loses
// them.
func (s *Store) SaveWord(ctx context.Context, userID string, w SavedWord, max int, defaultQuota int64, categoryIDs []string) (pending int, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// serialize the saves of one user so two parallel saves cannot both pass the limit
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "dictionary:"+userID); err != nil {
			return err
		}
		// a deleted account saves nothing: its data is being purged (the purge takes the same lock)
		var live bool
		if err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM identity_users WHERE id = $1`, userID).Scan(&live); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		} else if err != nil || !live {
			return ErrUnknownUser
		}
		// FOR UPDATE: a parallel delete of the word waits, so the difference below is what the counter gets
		var old int64
		var already bool
		err := tx.QueryRow(ctx, `SELECT true, octet_length(content) FROM dictionary_saved_words
			WHERE user_id = $1 AND grp = $3 AND word = $2 AND removed_at IS NULL FOR UPDATE`, userID, w.Word, w.Group).Scan(&already, &old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var words int64
		if !already {
			words = 1
		}
		delta := int64(len(w.Content)) - old
		// the counter row exists for every user that saved a word (and is the foreign key check of the user)
		if _, err := tx.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE dictionary_user_stats SET listed = listed + $2, content_bytes = content_bytes + $3
			WHERE user_id = $1 AND ($2 = 0 OR listed < $4) AND ($3 <= 0 OR content_bytes + $3 <= COALESCE(content_quota, $5))
			RETURNING listed`, userID, words, delta, max, defaultQuota).Scan(&pending)
		if errors.Is(err, pgx.ErrNoRows) {
			// the counters do not tell which of the two limits stopped the save: the words come first
			var listed int64
			if err := tx.QueryRow(ctx, `SELECT listed FROM dictionary_user_stats WHERE user_id = $1`, userID).Scan(&listed); err != nil {
				return err
			}
			if words > 0 && listed >= int64(max) {
				return ErrLimit
			}
			return ErrBytesLimit
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dictionary_saved_words (user_id, grp, word, content, audio_uk, audio_us)
			VALUES ($1, $6, $2, $3, $4, $5)
			ON CONFLICT (user_id, grp, word) DO UPDATE SET
				content = EXCLUDED.content, audio_uk = EXCLUDED.audio_uk, audio_us = EXCLUDED.audio_us,
				created_at = now(),
				exported_at = NULL, removed_at = NULL`,
			userID, w.Word, w.Content, w.AudioUK, w.AudioUS, w.Group); err != nil {
			return err
		}
		// the links of the word carry its created_at (a saved word is the newest of the list), see wordSource
		if _, err := tx.Exec(ctx, `UPDATE dictionary_word_categories wc SET created_at = w.created_at FROM dictionary_saved_words w
			WHERE wc.user_id = $1 AND wc.grp = $3 AND wc.word = $2 AND w.user_id = wc.user_id AND w.grp = wc.grp AND w.word = wc.word AND wc.created_at <> w.created_at`,
			userID, w.Word, w.Group); err != nil {
			return err
		}
		if err := setImageRefs(ctx, tx, userID, w.Group, w.Word, cardref.IDs(w.Content)); err != nil {
			return err
		}
		if err := setAudioRefs(ctx, tx, userID, w.Group, w.Word, cardref.AudioIDs(w.AudioUK, w.AudioUS)); err != nil {
			return err
		}
		return addCategories(ctx, tx, userID, w.Group, w.Word, categoryIDs)
	})
	return pending, mapErr(err)
}

// setImageRefs replaces the set of images the word uses. Every id must be an image of the user (ErrUnknownImage
// otherwise), so a card cannot point at the image of somebody else.
func setImageRefs(ctx context.Context, tx pgx.Tx, userID, group, word string, ids []string) error {
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
	if _, err := tx.Exec(ctx, `DELETE FROM dictionary_image_refs WHERE user_id = $1 AND grp = $3 AND word = $2`, userID, word, group); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO dictionary_image_refs (user_id, grp, word, image_id)
		SELECT $1, $4, $2, unnest($3::uuid[])`, userID, word, ids, group)
	return err
}

// setAudioRefs replaces the set of audio files the word uses; every id must be a file of the user
// (ErrUnknownAudio otherwise).
func setAudioRefs(ctx context.Context, tx pgx.Tx, userID, group, word string, ids []string) error {
	if len(ids) > 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dictionary_audio WHERE user_id = $1 AND id = ANY($2::uuid[])`,
			userID, ids).Scan(&n); err != nil {
			return err
		}
		if n != len(ids) {
			return ErrUnknownAudio
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dictionary_audio_refs WHERE user_id = $1 AND grp = $3 AND word = $2`, userID, word, group); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO dictionary_audio_refs (user_id, grp, word, audio_id)
		SELECT $1, $4, $2, unnest($3::uuid[])`, userID, word, ids, group)
	return err
}

// countCap bounds every count of the list: a count never reads more than this many rows, however long the list
// is. It is the cap of the review counts too.
const countCap = reviewCountCap

// CountCap is the cap of the counts of CountPending for the filter: categoryCountCap for the words of one category (a
// lookup per word), countCap for the others.
func CountCap(f WordFilter) int {
	if f.byCategory() {
		return categoryCountCap
	}
	return countCap
}

// CountPending is the number of words in the list of the user that pass the filter (a group, and a category), and how
// many of them are new (never exported). Every count stops at CountCap(f) rows, and capped says one did (it is a lower
// bound). Without a category filter the total is the counter of the user when the user keeps words in this group only
// (exact, however long the list); the counter spans the groups, so with words in another group the total is capped
// like the others.
func (s *Store) CountPending(ctx context.Context, userID string, f WordFilter) (total, fresh int, capped bool, err error) {
	limit := CountCap(f)
	args := []any{userID, limit}
	src := f.source(&args)
	count := func(extra string) string {
		return `(SELECT count(*) FROM (SELECT 1 FROM ` + src.from + ` WHERE ` + src.where + extra + ` LIMIT $2) t)`
	}
	if f.byCategory() {
		// the word of a link is looked up with a subplan, not joined: for a category of a few thousand words the planner
		// hashes the whole list against the links (140 ms), a subplan stops at the cap
		count = func(extra string) string {
			return `(SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
					WHERE w.user_id = wc.user_id AND w.grp = wc.grp AND w.word = wc.word AND w.removed_at IS NULL` + extra + `) AS listed
				FROM dictionary_word_categories wc WHERE wc.user_id = $1 AND wc.category_id = ` + src.category + `::uuid AND wc.grp = ` + src.group + `) x WHERE listed LIMIT $2) t)`
		}
	}
	filtered := f.narrowed()
	q := `SELECT ` + count(" AND w.exported_at IS NULL")
	// the words of the filter, capped; without a category filter the total is made exact below when it can be
	q += `, ` + count("")
	row := s.pool.QueryRow(ctx, q, args...)
	if err = row.Scan(&fresh, &total); err != nil {
		return 0, 0, false, mapErr(err)
	}
	capped = total >= limit || fresh >= limit
	if !filtered && total >= limit {
		// a long list: the counter of the user is exact when no word of another group is in the list
		var other bool
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dictionary_saved_words WHERE user_id = $1 AND grp <> $2 AND removed_at IS NULL)`,
			userID, f.Group).Scan(&other); err != nil {
			return 0, 0, false, mapErr(err)
		}
		if !other {
			if total, err = s.ListedCount(ctx, userID); err != nil {
				return 0, 0, false, err
			}
			capped = fresh >= limit
		}
	}
	return total, fresh, capped, nil
}

// ListPending returns up to limit words of the list, newest first (created_at, then word, both descending),
// starting after the key (afterCreated, afterWord) when afterWord is not empty. A non-empty query keeps only the
// words that contain it (case-insensitive), and f the ones that pass the category filter. The words come with
// their category ids and review state.
func (s *Store) ListPending(ctx context.Context, userID string, afterCreated time.Time, afterWord, query string, f WordFilter, limit int) ([]SavedWord, error) {
	args := []any{userID}
	src := f.source(&args)
	q := `SELECT w.grp, w.word, w.content, w.audio_uk, w.audio_us, w.created_at, w.exported_at IS NOT NULL FROM ` + src.from + ` WHERE ` + src.where
	if query != "" {
		args = append(args, escapeLike(query))
		q += ` AND w.word ILIKE '%' || $` + strconv.Itoa(len(args)) + ` || '%' ESCAPE '\'`
	}
	if afterWord != "" {
		args = append(args, afterCreated, afterWord)
		q += ` AND (` + src.created + `, ` + src.word + `) < ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, q+` ORDER BY `+src.created+` DESC, `+src.word+` DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, mapErr(err)
	}
	words, err := scanWords(rows)
	if err != nil || len(words) == 0 {
		return words, err
	}
	names := make([]string, len(words))
	for i, w := range words {
		names[i] = w.Word
	}
	ids, err := s.categoryIDs(ctx, userID, f.Group, names)
	if err != nil {
		return nil, err
	}
	states, err := s.reviewStates(ctx, userID, f.Group, names)
	if err != nil {
		return nil, err
	}
	for i := range words {
		words[i].CategoryIDs = ids[words[i].Word]
		st := states[words[i].Word]
		words[i].NextReview, words[i].Learning = st.due, st.learning
	}
	return words, nil
}

// SavedByNames returns which of the words are in the list of the user, with the ids of their categories (a word in no
// category maps to nil). The words are looked up by primary key, so the cost follows the number of words asked for.
func (s *Store) SavedByNames(ctx context.Context, userID, group string, words []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(words) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT word FROM dictionary_saved_words WHERE user_id = $1 AND grp = $3 AND removed_at IS NULL AND word = ANY($2)`, userID, words, group)
	if err != nil {
		return nil, mapErr(err)
	}
	listed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(listed) == 0 {
		return out, err
	}
	ids, err := s.categoryIDs(ctx, userID, group, listed)
	if err != nil {
		return nil, err
	}
	for _, w := range listed {
		out[w] = ids[w]
	}
	return out, nil
}

// escapeLike makes s match literally in a LIKE pattern (ESCAPE '\').
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Pending returns the words an export packs, oldest first (the order of the cards of a deck): the words of the
// list that were never exported (ScopeNew), or every word of the list (ScopeAll), that pass the category filter f.
// A non-nil words keeps only those words and ignores f and scope: the exported ones are returned too. A limit above
// zero returns at most that many words (the oldest), so a list of 1,000,000 words never goes to memory.
func (s *Store) Pending(ctx context.Context, userID string, scope Scope, f WordFilter, words []string, limit int) ([]SavedWord, error) {
	args := []any{userID}
	var src wordSource
	extra := ""
	if words != nil {
		args = append(args, words, f.Group)
		src = wordSource{from: `dictionary_saved_words w`, where: `w.user_id = $1 AND w.removed_at IS NULL AND w.word = ANY($2) AND w.grp = $3`, created: "w.created_at", word: "w.word"}
	} else {
		src = f.source(&args)
		if scope != ScopeAll {
			extra = ` AND w.exported_at IS NULL`
		}
	}
	q := `SELECT w.grp, w.word, w.content, w.audio_uk, w.audio_us, w.created_at, w.exported_at IS NOT NULL FROM ` + src.from + ` WHERE ` + src.where + extra +
		` ORDER BY ` + src.created + `, ` + src.word
	if limit > 0 {
		args = append(args, limit)
		q += ` LIMIT $` + strconv.Itoa(len(args))
	}
	rows, err := s.pool.Query(ctx, q, args...)
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
		if err := rows.Scan(&w.Group, &w.Word, &w.Content, &w.AudioUK, &w.AudioUS, &w.CreatedAt, &w.Exported); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeletePending removes a word of the list (an exported one that is still in the list too); ErrNotFound when
// the user has no such word.
func (s *Store) DeletePending(ctx context.Context, userID, group, word string) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		n, bytes, err := changedRows(ctx, tx, `DELETE FROM dictionary_saved_words WHERE user_id = $1 AND grp = $3 AND word = $2 AND removed_at IS NULL`, userID, word, group)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return addListed(ctx, tx, userID, -n, -bytes)
	})
	return mapErr(err)
}

// changedRows runs an UPDATE or DELETE of dictionary_saved_words and returns how many rows it changed and the
// total size of their content, to move the counters of addListed.
func changedRows(ctx context.Context, tx pgx.Tx, q string, args ...any) (n, bytes int64, err error) {
	err = tx.QueryRow(ctx, `WITH c AS (`+q+` RETURNING octet_length(content) AS b) SELECT count(*), coalesce(sum(b), 0)::bigint FROM c`,
		args...).Scan(&n, &bytes)
	return n, bytes, err
}

// addListed adds the deltas to the number of words in the list of the user and to the size of their content. Call
// it in the transaction that changed the rows, so the counters never drift from them.
func addListed(ctx context.Context, tx pgx.Tx, userID string, words, bytes int64) error {
	if words == 0 && bytes == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id, listed, content_bytes) VALUES ($1, greatest($2::bigint, 0), greatest($3::bigint, 0))
		ON CONFLICT (user_id) DO UPDATE SET listed = greatest(dictionary_user_stats.listed + $2::bigint, 0),
			content_bytes = greatest(dictionary_user_stats.content_bytes + $3::bigint, 0)`, userID, words, bytes)
	return err
}

// ListedCount is the number of words in the list of the user, from the counter (no scan of the words).
func (s *Store) ListedCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT listed FROM dictionary_user_stats WHERE user_id = $1`, userID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, mapErr(err)
}

// MarkExported sets exported_at on the given words of the list (the ones packed in a deck), which makes them
// "exported". With remove they also leave the list (removed_at), as before; without it they stay in the list.
// Words saved or refreshed after the deck was read are not in the given list and are not touched (a refreshed
// word is new again). It returns the number of rows changed.
func (s *Store) MarkExported(ctx context.Context, userID, group string, words []string, remove bool) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var bytes int64
		var err error
		n, bytes, err = changedRows(ctx, tx, `UPDATE dictionary_saved_words
			SET exported_at = now(), removed_at = CASE WHEN $3 THEN now() END
			WHERE user_id = $1 AND grp = $4 AND removed_at IS NULL AND word = ANY($2)`, userID, words, remove, group)
		if err != nil {
			return err
		}
		if remove {
			return addListed(ctx, tx, userID, -n, -bytes)
		}
		return nil
	})
	return n, mapErr(err)
}

// removeBatch is how many words RemoveExported takes out of the list in one transaction, so a list of 1,000,000
// words never holds one lock or writes one huge transaction.
const removeBatch = 5000

// RemoveExported takes the words of the list that were exported out of it (removed_at); new words stay. It
// returns how many left the list. It works in batches of removeBatch words, one transaction each.
func (s *Store) RemoveExported(ctx context.Context, userID, group string) (int64, error) {
	var total int64
	for {
		var n int64
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			// MATERIALIZED: the batch is found first (dictionary_saved_words_exported_idx) and its rows are looked up by
			// primary key; without it the planner hashes the batch against a Seq Scan of the whole table (190 ms against 40)
			var bytes int64
			err := tx.QueryRow(ctx, `WITH b AS MATERIALIZED (
					SELECT word FROM dictionary_saved_words WHERE user_id = $1 AND grp = $3 AND removed_at IS NULL AND exported_at IS NOT NULL LIMIT $2),
				u AS (UPDATE dictionary_saved_words w SET removed_at = now() FROM b
					WHERE w.user_id = $1 AND w.grp = $3 AND w.word = b.word AND w.removed_at IS NULL RETURNING octet_length(w.content) AS n)
				SELECT count(*), coalesce(sum(n), 0)::bigint FROM u`, userID, removeBatch, group).Scan(&n, &bytes)
			if err != nil {
				return err
			}
			return addListed(ctx, tx, userID, -n, -bytes)
		})
		if err != nil {
			return total, mapErr(err)
		}
		total += n
		if n < removeBatch {
			return total, nil
		}
	}
}

// Counters are the per-user counters of the list and of what the user stores that ReconcileCounters checks.
type Counters struct {
	Listed, ContentBytes, ImageBytes, AudioBytes int64
}

// recomputeCounters recomputes the four counters of the user from the rows, in tx. The advisory lock of SaveWord
// comes first; then the row lock of the counters, which makes the image and audio statements (they change the
// counter in the statement of the row but take no advisory lock) wait before they commit; the sums are a second
// statement so they get a fresh snapshot, without a half-done change, and the delta of a waiting change lands on
// the corrected value afterwards. It returns the counters before and after.
func recomputeCounters(ctx context.Context, tx pgx.Tx, user string) (was, now Counters, err error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "dictionary:"+user); err != nil {
		return was, now, err
	}
	if err := tx.QueryRow(ctx, `SELECT listed, content_bytes, image_bytes, audio_bytes FROM dictionary_user_stats
		WHERE user_id = $1 FOR UPDATE`, user).Scan(&was.Listed, &was.ContentBytes, &was.ImageBytes, &was.AudioBytes); err != nil {
		return was, now, err
	}
	if err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL),
			(SELECT coalesce(sum(octet_length(content)), 0)::bigint FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL),
			(SELECT coalesce(sum(size), 0)::bigint FROM dictionary_images WHERE user_id = $1),
			(SELECT coalesce(sum(size), 0)::bigint FROM dictionary_audio WHERE user_id = $1)`, user).
		Scan(&now.Listed, &now.ContentBytes, &now.ImageBytes, &now.AudioBytes); err != nil {
		return was, now, err
	}
	if now != was {
		if _, err := tx.Exec(ctx, `UPDATE dictionary_user_stats SET listed = $2, content_bytes = $3, image_bytes = $4, audio_bytes = $5
			WHERE user_id = $1`, user, now.Listed, now.ContentBytes, now.ImageBytes, now.AudioBytes); err != nil {
			return was, now, err
		}
	}
	return was, now, nil
}

// RecomputeUserCounters recalculates the counters of one user from the rows and returns what they were and what
// they are. The counter row is created when the user has none; ErrUnknownUser when there is no such user.
func (s *Store) RecomputeUserCounters(ctx context.Context, userID string) (was, now Counters, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, userID); err != nil {
			return err
		}
		var err error
		was, now, err = recomputeCounters(ctx, tx, userID)
		return err
	})
	return was, now, mapErr(err)
}

// ReconcileCounters recomputes the counters (see Counters) for up to limit users after the user id afterUser
// (all users from the start when it is empty), under the advisory lock of SaveWord so a parallel save cannot make
// it a false alarm. It returns the last user it checked (empty once the end is reached) and the users whose
// counters were wrong, with what they had; the counters are fixed.
func (s *Store) ReconcileCounters(ctx context.Context, afterUser string, limit int) (next string, fixed map[string]Counters, err error) {
	q := `SELECT user_id::text FROM dictionary_user_stats `
	args := []any{limit}
	if afterUser != "" {
		q += `WHERE user_id > $2::uuid `
		args = append(args, afterUser)
	}
	rows, err := s.pool.Query(ctx, q+`ORDER BY user_id LIMIT $1`, args...)
	if err != nil {
		return "", nil, mapErr(err)
	}
	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return "", nil, err
		}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	fixed = map[string]Counters{}
	for _, u := range users {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			was, now, err := recomputeCounters(ctx, tx, u)
			if err == nil && was != now {
				fixed[u] = was
			}
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) { // the counter row went away since the listing
			continue
		}
		if err != nil {
			return "", fixed, mapErr(err)
		}
	}
	if len(users) < limit {
		return "", fixed, nil
	}
	return users[len(users)-1], fixed, nil
}

// DeleteRemovedBefore deletes up to limit rows that left the list before the cutoff and returns how many it
// deleted. Several replicas can run it at once (SKIP LOCKED). Words still in the list are never deleted.
func (s *Store) DeleteRemovedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM dictionary_saved_words d USING (
			SELECT user_id, grp, word FROM dictionary_saved_words
			WHERE removed_at < $1 ORDER BY removed_at LIMIT $2 FOR UPDATE SKIP LOCKED
		) x WHERE d.user_id = x.user_id AND d.grp = x.grp AND d.word = x.word`, cutoff, limit)
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
	if err == nil || errors.Is(err, ErrLimit) || errors.Is(err, ErrBytesLimit) || errors.Is(err, ErrUnknownImage) || errors.Is(err, ErrUnknownAudio) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrCategoryExists) || errors.Is(err, ErrJobActive) || errors.Is(err, ErrLeaseLost) {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "dictionary_categories_name_idx" {
		return ErrCategoryExists
	}
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "dictionary_export_jobs_active_idx" {
		return ErrJobActive
	}
	if errors.As(err, &pg) && pg.Code == "23503" { // foreign_key_violation: no such user
		return ErrUnknownUser
	}
	return err
}
