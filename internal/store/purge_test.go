package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// userTables are the tables with a user_id that the purge must empty.
var userTables = []string{
	"dictionary_saved_words", "dictionary_review_state", "dictionary_review_log", "dictionary_image_refs", "dictionary_audio_refs",
	"dictionary_word_categories", "dictionary_images", "dictionary_audio", "dictionary_export_jobs", "dictionary_categories",
	"dictionary_deck_links", "dictionary_review_bonus", "dictionary_fsrs_params", "dictionary_user_stats",
}

func countRows(t *testing.T, s *Store, table, user string) (n int) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedUser fills every table of a user: words (with review state, log, categories, image and audio refs), images,
// audio, an export job with a part, a deck link, a bonus, FSRS params and the counters.
func seedUser(t *testing.T, s *Store, user string, words int) {
	t.Helper()
	ctx := context.Background()
	img, aud, cat, job := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO dictionary_user_stats (user_id, listed) VALUES ($1, $2)`, []any{user, words}},
		{`INSERT INTO dictionary_saved_words (user_id, word, content) SELECT $1, 'w' || g, 'c' FROM generate_series(1, $2) g`, []any{user, words}},
		{`INSERT INTO dictionary_images (id, user_id, content_type, size, sha256) VALUES ($2, $1, 'image/png', 10, '\x01')`, []any{user, img}},
		{`INSERT INTO dictionary_audio (id, user_id, content_type, size, sha256) VALUES ($2, $1, 'audio/mpeg', 10, '\x02')`, []any{user, aud}},
		{`INSERT INTO dictionary_categories (id, user_id, name) VALUES ($2, $1, 'cat')`, []any{user, cat}},
		{`INSERT INTO dictionary_image_refs (user_id, word, image_id) SELECT $1, 'w' || g, $2 FROM generate_series(1, least($3, 50)) g`, []any{user, img, words}},
		{`INSERT INTO dictionary_audio_refs (user_id, word, audio_id) SELECT $1, 'w' || g, $2 FROM generate_series(1, least($3, 50)) g`, []any{user, aud, words}},
		{`INSERT INTO dictionary_word_categories (user_id, word, category_id) SELECT $1, 'w' || g, $2 FROM generate_series(1, $3) g`, []any{user, cat, words}},
		{`INSERT INTO dictionary_review_state (user_id, word, due_at, first_reviewed_at, last_reviewed_at)
			SELECT $1, 'w' || g, now(), now(), now() FROM generate_series(1, $2) g`, []any{user, words}},
		{`INSERT INTO dictionary_review_log (user_id, word, reviewed_at, grade) SELECT $1, 'w' || g, now(), 3 FROM generate_series(1, $2) g`, []any{user, words}},
		{`INSERT INTO dictionary_export_jobs (id, user_id, status, params, expires_at) VALUES ($2, $1, 'done', '{}', now() + interval '1 day')`, []any{user, job}},
		{`INSERT INTO dictionary_export_parts (job_id, n, filename, file_id, word_count, size) VALUES ($1, 1, 'a.apkg', $2, 1, 1)`, []any{job, uuid.NewString()}},
		{`INSERT INTO dictionary_deck_links (token_hash, user_id, filename, data, word_count, expires_at) VALUES ($2, $1, 'd.apkg', '\x00', 1, now() + interval '1 day')`, []any{user, []byte(uuid.NewString())}},
		{`INSERT INTO dictionary_review_bonus (user_id, expires_at) VALUES ($1, now() + interval '1 day')`, []any{user}},
		{`INSERT INTO dictionary_fsrs_params (user_id, attempted_at) VALUES ($1, now())`, []any{user}},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed %.40s: %v", q.sql, err)
		}
	}
}

func softDelete(t *testing.T, s *Store, user string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE identity_users SET deleted_at = now(), disabled = true WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
}

// purgeAll runs PurgeStep until the user is clean, checking that no call deletes more than one batch.
func purgeAll(t *testing.T, s *Store, user string, batch int) (calls int, perTable map[string]int64) {
	t.Helper()
	perTable = map[string]int64{}
	for i := 0; i < 100000; i++ {
		table, n, clean, busy, err := s.PurgeStep(context.Background(), user, batch)
		if err != nil {
			t.Fatalf("purge step: %v", err)
		}
		if busy {
			time.Sleep(time.Millisecond)
			continue
		}
		if clean {
			return calls, perTable
		}
		if n > int64(batch) {
			t.Fatalf("%s: one step deleted %d rows, the batch is %d", table, n, batch)
		}
		calls++
		perTable[table] += n
	}
	t.Fatal("purge does not end")
	return
}

func TestPurgeDeletedUser(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	const words, batch = 20000, 100
	seedUser(t, s, user, words)

	// a live user is never purged
	if _, _, _, _, err := s.PurgeStep(ctx, user, batch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purge of a live user: %v", err)
	}
	// and a deleted one saves nothing
	softDelete(t, s, user)
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "late"}, 1<<30, testQuota, nil); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("save by a deleted user: %v", err)
	}

	calls, per := purgeAll(t, s, user, batch)
	if per["saved_words"] != words {
		t.Errorf("deleted words = %d, want %d (%v)", per["saved_words"], words, per)
	}
	if calls < words/batch {
		t.Errorf("%d calls for %d words in batches of %d", calls, words, batch)
	}
	for _, tb := range userTables {
		if n := countRows(t, s, tb, user); n != 0 {
			t.Errorf("%s still has %d rows", tb, n)
		}
	}

	// the row stays until the grace period is over, whatever the data
	if err := s.MarkPurged(ctx, user); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishUserDelete(ctx, user, time.Hour); err != nil || ok {
		t.Fatalf("finish inside the grace period: %v %v", ok, err)
	}
	if got, err := s.DeletedUsers(ctx, time.Hour, 10); err != nil || len(got) != 0 {
		t.Errorf("a user waiting for the grace period is listed: %v %v", got, err)
	}
	if ok, err := s.FinishUserDelete(ctx, user, 0); err != nil || !ok {
		t.Fatalf("finish after the grace period: %v %v", ok, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM identity_users WHERE id = $1`, user).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user row: %d %v", n, err)
	}
}

func TestFinishUserDeleteWithLateRows(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	softDelete(t, s, user)
	purgeAll(t, s, user, 100)
	if err := s.MarkPurged(ctx, user); err != nil {
		t.Fatal(err)
	}
	// a row written after the purge (a request that still held a token): the user row must stay, the purge run again
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_categories (user_id, name) VALUES ($1, 'late')`, user); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishUserDelete(ctx, user, 0); err != nil || ok {
		t.Fatalf("finish with a late row: %v %v", ok, err)
	}
	got, err := s.DeletedUsers(ctx, time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range got {
		found = found || (u.ID == user && !u.Purged)
	}
	if !found {
		t.Fatal("the user is not up for a new purge")
	}
	purgeAll(t, s, user, 100)
	if err := s.MarkPurged(ctx, user); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishUserDelete(ctx, user, 0); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
}

func TestPurgeParallelWithSaves(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	seedUser(t, s, user, 3000)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	saveErr := make(chan error, 1)
	wg.Add(1)
	go func() { // saves go on while the account is deleted and purged
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, err := s.SaveWord(ctx, user, SavedWord{Word: "s" + uuid.NewString(), Content: "x"}, 1<<30, testQuota, nil)
			if err != nil && !errors.Is(err, ErrUnknownUser) {
				select {
				case saveErr <- err:
				default:
				}
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	softDelete(t, s, user)
	var purgers sync.WaitGroup
	for i := 0; i < 2; i++ { // two replicas at once
		purgers.Add(1)
		go func() { defer purgers.Done(); purgeAll(t, s, user, 100) }()
	}
	purgers.Wait()
	close(stop)
	wg.Wait()
	select {
	case err := <-saveErr:
		t.Fatalf("save: %v", err)
	default:
	}
	for _, tb := range userTables {
		if n := countRows(t, s, tb, user); n != 0 {
			t.Errorf("%s still has %d rows", tb, n)
		}
	}
}
