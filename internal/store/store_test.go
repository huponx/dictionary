package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests need the schema of db-migration (V15). Set DICTIONARY_TEST_POSTGRES_URL, for example
// postgres://postgres:postgres@localhost:5432/app?sslmode=disable (the database of `make up` in db-migration).
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	url := os.Getenv("DICTIONARY_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("DICTIONARY_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	s, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	id := uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO identity_users (id, email) VALUES ($1, $2)`, id, id+"@dictionary.test"); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	// the foreign key cascades: deleting the user removes its words
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), `DELETE FROM identity_users WHERE id = $1`, id) })
	return s, id
}

func TestSaveListExportCycle(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)

	for i, w := range []string{"alpha", "beta", "gamma"} {
		n, err := s.SaveWord(ctx, user, SavedWord{Word: w, Content: "c-" + w, AudioUS: "https://a/" + w}, 3)
		if err != nil || n != i+1 {
			t.Fatalf("save %s: n=%d err=%v", w, n, err)
		}
	}
	// the limit counts pending words; a pending word can be updated at the limit
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "delta"}, 3); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
	if n, err := s.SaveWord(ctx, user, SavedWord{Word: "beta", Content: "updated"}, 3); err != nil || n != 3 {
		t.Fatalf("update at the limit: n=%d err=%v", n, err)
	}

	// newest first, in pages of 2, with the keyset; the overwritten beta is the newest now
	page1, err := s.ListPending(ctx, user, time.Time{}, "", 2)
	if err != nil || len(page1) != 2 || page1[0].Word != "beta" || page1[1].Word != "gamma" {
		t.Fatalf("page 1: %+v, %v", page1, err)
	}
	last := page1[1]
	page2, err := s.ListPending(ctx, user, last.CreatedAt, last.Word, 2)
	if err != nil || len(page2) != 1 || page2[0].Word != "alpha" {
		t.Fatalf("page 2: %+v, %v", page2, err)
	}
	if page1[0].Content != "updated" {
		t.Errorf("content of beta = %q", page1[0].Content)
	}

	all, err := s.Pending(ctx, user)
	if err != nil || len(all) != 3 || all[0].Word != "alpha" || all[2].Word != "beta" {
		t.Fatalf("Pending (oldest first): %+v, %v", all, err)
	}

	if err := s.DeletePending(ctx, user, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePending(ctx, user, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}

	// export: only the packed words are marked; a word saved meanwhile stays pending
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "late"}, 3); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkExported(ctx, user, []string{"beta", "gamma"}); err != nil || n != 2 {
		t.Fatalf("MarkExported: n=%d err=%v", n, err)
	}
	if n, _ := s.CountPending(ctx, user); n != 1 {
		t.Errorf("pending after export = %d, want 1 (late)", n)
	}
	// exported words are not deletable as pending and do not count against the limit
	if err := s.DeletePending(ctx, user, "beta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an exported word: %v", err)
	}
	for _, w := range []string{"one", "two"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 3); err != nil {
			t.Fatalf("a new batch has room: %v", err)
		}
	}
	// saving an exported word again makes it pending again (and counts)
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "beta"}, 4); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountPending(ctx, user); n != 4 {
		t.Errorf("pending = %d, want 4", n)
	}

	// retention: gamma was exported "now", so only a cutoff in the future deletes it, and beta is pending again
	if n, err := s.DeleteExportedBefore(ctx, time.Now().Add(-time.Hour), 100); err != nil || n != 0 {
		t.Errorf("nothing is that old: n=%d err=%v", n, err)
	}
	if n, err := s.DeleteExportedBefore(ctx, time.Now().Add(time.Minute), 100); err != nil || n < 1 {
		t.Errorf("cleanup: n=%d err=%v", n, err)
	}
	if n, _ := s.CountPending(ctx, user); n != 4 {
		t.Errorf("the cleanup must not touch pending words: %d", n)
	}
}

func TestUnknownUser(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.SaveWord(context.Background(), uuid.NewString(), SavedWord{Word: "x"}, 3); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("err = %v, want ErrUnknownUser", err)
	}
}

func TestLimitIsRaceFree(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	const max = 3
	errs := make(chan error, 10)
	for i := range 10 {
		go func() {
			_, err := s.SaveWord(ctx, user, SavedWord{Word: "w" + string(rune('a'+i))}, max)
			errs <- err
		}()
	}
	ok := 0
	for range 10 {
		if err := <-errs; err == nil {
			ok++
		} else if !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
	}
	if n, _ := s.CountPending(ctx, user); ok != max || n != max {
		t.Errorf("saved %d, pending %d, want %d", ok, n, max)
	}
}
