package cleanup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Needs the schema of db-migration; set DICTIONARY_TEST_POSTGRES_URL (see internal/store).
func TestPurgeDeleted(t *testing.T) {
	url := os.Getenv("DICTIONARY_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("DICTIONARY_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	st, err := store.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	user, other := uuid.NewString(), uuid.NewString()
	for _, u := range []string{user, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO identity_users (id, email) VALUES ($1, $2)`, u, u+"@dictionary.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO dictionary_saved_words (user_id, word) SELECT $1, 'w' || g FROM generate_series(1, 7) g`, u); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO dictionary_review_bonus (user_id, expires_at) VALUES ($1, now() + interval '1 day')`, u); err != nil {
			t.Fatal(err)
		}
	}
	defer pool.Exec(context.Background(), `DELETE FROM identity_users WHERE id = ANY($1)`, []string{user, other}) //nolint:errcheck

	root := t.TempDir()
	stores := map[string]*filestore.Store{}
	for _, k := range []string{"images", "audio", "exports"} {
		stores[k] = &filestore.Store{Dir: filepath.Join(root, k)}
		for _, u := range []string{user, other} {
			if err := stores[k].Write(u, uuid.NewString(), "png", []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_users SET deleted_at = now() - interval '1 minute', disabled = true WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}

	c := &Cleaner{Store: st, Files: stores["images"], Audio: stores["audio"], Exports: stores["exports"], Log: zap.NewNop(),
		Config: Config{PurgeGrace: time.Hour, PurgeBatch: 2}}
	count := func(q, id string) (n int) {
		if err := pool.QueryRow(ctx, q, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// the data goes at once, the row stays during the grace period
	c.PurgeDeleted(ctx)
	if n := count(`SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1`, user); n != 0 {
		t.Errorf("%d words left", n)
	}
	if n := count(`SELECT count(*) FROM dictionary_review_bonus WHERE user_id = $1`, user); n != 0 {
		t.Errorf("review bonus left")
	}
	for k, s := range stores {
		if _, err := os.Stat(filepath.Join(s.Dir, user)); !os.IsNotExist(err) {
			t.Errorf("%s: files of the deleted user left (%v)", k, err)
		}
		if _, err := os.Stat(filepath.Join(s.Dir, other)); err != nil {
			t.Errorf("%s: files of another user: %v", k, err)
		}
	}
	if n := count(`SELECT count(*) FROM identity_users WHERE id = $1`, user); n != 1 {
		t.Fatal("the row went before the grace period")
	}
	// a live user is untouched
	if n := count(`SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1`, other); n != 7 {
		t.Errorf("a live user has %d words", n)
	}
	// after the grace period the row goes
	c.Config.PurgeGrace = 0
	c.PurgeDeleted(ctx)
	if n := count(`SELECT count(*) FROM identity_users WHERE id = $1`, user); n != 0 {
		t.Error("the row stayed after the grace period")
	}
	if n := count(`SELECT count(*) FROM identity_users WHERE id = $1`, other); n != 1 {
		t.Error("a live user was deleted")
	}
}
