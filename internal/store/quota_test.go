package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func contentBytesOf(t *testing.T, s *Store, user string) (counter, actual int64) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `SELECT coalesce((SELECT content_bytes FROM dictionary_user_stats WHERE user_id = $1), 0),
		coalesce((SELECT sum(octet_length(content)) FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL), 0)`, user).
		Scan(&counter, &actual); err != nil {
		t.Fatal(err)
	}
	return counter, actual
}

func wantBytes(t *testing.T, s *Store, user string, want int64) {
	t.Helper()
	if c, a := contentBytesOf(t, s, user); c != want || a != want {
		t.Fatalf("content bytes: counter %d, rows %d, want %d", c, a, want)
	}
}

func ptr(n int64) *int64 { return &n }

func TestContentBytesFollowTheList(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	save := func(word, content string) {
		t.Helper()
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: word, Content: content}, 100, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	save("a", strings.Repeat("x", 100))
	save("b", strings.Repeat("é", 50)) // 100 bytes, 50 characters
	wantBytes(t, s, user, 200)
	// an overwrite adds the difference
	save("a", strings.Repeat("x", 30))
	wantBytes(t, s, user, 130)
	save("a", strings.Repeat("x", 30))
	wantBytes(t, s, user, 130)

	// marking exported without remove keeps the words in the list; with remove they leave it
	if _, err := s.MarkExported(ctx, user, []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 130)
	if _, err := s.MarkExported(ctx, user, []string{"a"}, true); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 100)
	// a word saved again after it left the list counts as new
	save("a", strings.Repeat("x", 10))
	wantBytes(t, s, user, 110)

	if err := s.DeletePending(ctx, user, "b"); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 10)
	if err := s.DeletePending(ctx, user, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 10)

	// RemoveExported takes the exported words that are still in the list
	save("c", strings.Repeat("x", 20))
	if _, err := s.MarkExported(ctx, user, []string{"c"}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RemoveExported(ctx, user); err != nil || n != 1 {
		t.Fatalf("remove exported: %d, %v", n, err)
	}
	wantBytes(t, s, user, 10)
}

func TestContentQuota(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	save := func(word string, n int, quota int64) error {
		_, err := s.SaveWord(ctx, user, SavedWord{Word: word, Content: strings.Repeat("x", n)}, 100, quota, nil)
		return err
	}
	if err := save("a", 60, 100); err != nil {
		t.Fatal(err)
	}
	if err := save("b", 41, 100); !errors.Is(err, ErrBytesLimit) {
		t.Fatalf("over the quota: %v", err)
	}
	wantBytes(t, s, user, 60)
	if err := save("b", 40, 100); err != nil { // exactly the quota
		t.Fatal(err)
	}
	// growing an existing word counts the difference only
	if err := save("a", 61, 100); !errors.Is(err, ErrBytesLimit) {
		t.Fatalf("growing over the quota: %v", err)
	}
	// the words come first when both limits stop the save
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "c", Content: "xx"}, 2, 100, nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("both limits: %v", err)
	}

	// an own quota replaces the default, larger or smaller
	if err := s.SetStorageQuota(ctx, user, Quotas{Content: ptr(500)}); err != nil {
		t.Fatal(err)
	}
	if err := save("a", 200, 100); err != nil {
		t.Fatalf("own quota larger than the default: %v", err)
	}
	if err := s.SetStorageQuota(ctx, user, Quotas{Content: ptr(50)}); err != nil {
		t.Fatal(err)
	}
	// over the quota now: it can shrink and delete, not grow or add
	if err := save("d", 1, 100); !errors.Is(err, ErrBytesLimit) {
		t.Fatalf("add over the own quota: %v", err)
	}
	if err := save("a", 210, 1000); !errors.Is(err, ErrBytesLimit) {
		t.Fatalf("grow over the own quota: %v", err)
	}
	if err := save("a", 150, 1000); err != nil {
		t.Fatalf("shrink while over the quota: %v", err)
	}
	if err := save("a", 150, 1000); err != nil {
		t.Fatalf("same size while over the quota: %v", err)
	}
	if err := s.DeletePending(ctx, user, "a"); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 40)
	// removing the own quota brings the default back
	if err := s.SetStorageQuota(ctx, user, Quotas{}); err != nil {
		t.Fatal(err)
	}
	if err := save("e", 61, 100); !errors.Is(err, ErrBytesLimit) {
		t.Fatalf("default again: %v", err)
	}
	if err := save("e", 60, 100); err != nil {
		t.Fatal(err)
	}
}

func TestStorageUsageAndQuota(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	// a user that never stored anything has no row: zeros and no own limits
	if u, err := s.GetStorageUsage(ctx, user); err != nil || u != (StorageUsage{}) {
		t.Fatalf("new user: %+v, %v", u, err)
	}
	if err := s.SetStorageQuota(ctx, user, Quotas{Content: ptr(10), Audio: ptr(30)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "a", Content: "12345"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetStorageUsage(ctx, user)
	if err != nil || u.ContentBytes != 5 || u.ContentQuota == nil || *u.ContentQuota != 10 || u.ImageQuota != nil || *u.AudioQuota != 30 {
		t.Fatalf("usage: %+v, %v", u, err)
	}
	if _, err := s.GetStorageUsage(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("unknown user: %v", err)
	}
	if err := s.SetStorageQuota(ctx, "00000000-0000-4000-8000-000000000000", Quotas{}); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("set for an unknown user: %v", err)
	}
}

func TestReconcileFixesContentBytes(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "a", Content: "12345"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_user_stats SET content_bytes = 99 WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	var was Counters
	after := ""
	for {
		next, f, err := s.ReconcileListed(ctx, after, 50)
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := f[user]; ok {
			was = c
		}
		if next == "" {
			break
		}
		after = next
	}
	if was.ContentBytes != 99 || was.Listed != 1 {
		t.Errorf("drift not reported: %+v", was)
	}
	wantBytes(t, s, user, 5)
}
