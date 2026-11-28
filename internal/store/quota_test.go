package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
		if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: word, Content: content}, 100, testQuota, nil); err != nil {
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
	if _, err := s.MarkExported(ctx, user, "en", []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 130)
	if _, err := s.MarkExported(ctx, user, "en", []string{"a"}, true); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 100)
	// a word saved again after it left the list counts as new
	save("a", strings.Repeat("x", 10))
	wantBytes(t, s, user, 110)

	if err := s.DeletePending(ctx, user, "en", "b"); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 10)
	if err := s.DeletePending(ctx, user, "en", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	wantBytes(t, s, user, 10)

	// RemoveExported takes the exported words that are still in the list
	save("c", strings.Repeat("x", 20))
	if _, err := s.MarkExported(ctx, user, "en", []string{"c"}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RemoveExported(ctx, user, "en"); err != nil || n != 1 {
		t.Fatalf("remove exported: %d, %v", n, err)
	}
	wantBytes(t, s, user, 10)
}

func TestContentQuota(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	save := func(word string, n int, quota int64) error {
		_, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: word, Content: strings.Repeat("x", n)}, 100, quota, nil)
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: "c", Content: "xx"}, 2, 100, nil); !errors.Is(err, ErrLimit) {
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
	if err := s.DeletePending(ctx, user, "en", "a"); err != nil {
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: "a", Content: "12345"}, 10, testQuota, nil); err != nil {
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: "a", Content: "12345"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_user_stats SET content_bytes = 99 WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	var was Counters
	after := ""
	for {
		next, f, err := s.ReconcileCounters(ctx, after, 50)
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

func countersOf(t *testing.T, s *Store, user string) Counters {
	t.Helper()
	var c Counters
	err := s.pool.QueryRow(context.Background(), `SELECT listed, content_bytes, image_bytes, audio_bytes FROM dictionary_user_stats WHERE user_id = $1`, user).
		Scan(&c.Listed, &c.ContentBytes, &c.ImageBytes, &c.AudioBytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRecomputeUserCounters(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: "a", Content: "12345"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertImage(ctx, Image{ID: uuid.NewString(), UserID: user, ContentType: "image/jpeg", Size: 100, SHA256: []byte(uuid.NewString())}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertAudio(ctx, Audio{ID: uuid.NewString(), UserID: user, ContentType: "audio/mpeg", Size: 40, SHA256: []byte(uuid.NewString())}); err != nil {
		t.Fatal(err)
	}
	want := Counters{Listed: 1, ContentBytes: 5, ImageBytes: 100, AudioBytes: 40}
	for _, col := range []string{"listed", "content_bytes", "image_bytes", "audio_bytes"} {
		if _, err := s.pool.Exec(ctx, `UPDATE dictionary_user_stats SET `+col+` = 999 WHERE user_id = $1`, user); err != nil {
			t.Fatal(err)
		}
		was, now, err := s.RecomputeUserCounters(ctx, user)
		if err != nil || now != want || was == want {
			t.Fatalf("%s: was %+v now %+v, %v", col, was, now, err)
		}
		if got := countersOf(t, s, user); got != want {
			t.Fatalf("%s: stored %+v", col, got)
		}
	}
	if was, now, err := s.RecomputeUserCounters(ctx, user); err != nil || was != want || now != want {
		t.Fatalf("no drift: %+v %+v %v", was, now, err)
	}
}

func TestRecomputeUserCountersCreatesRowAndRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	was, now, err := s.RecomputeUserCounters(ctx, user)
	if err != nil || was != (Counters{}) || now != (Counters{}) {
		t.Fatalf("user without a counter row: %+v %+v %v", was, now, err)
	}
	countersOf(t, s, user) // the row exists now
	if _, _, err := s.RecomputeUserCounters(ctx, uuid.NewString()); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestReconcileCountersFixesImageAndAudioBytes(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	if _, _, err := s.InsertImage(ctx, Image{ID: uuid.NewString(), UserID: user, ContentType: "image/jpeg", Size: 100, SHA256: []byte(uuid.NewString())}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertAudio(ctx, Audio{ID: uuid.NewString(), UserID: user, ContentType: "audio/mpeg", Size: 40, SHA256: []byte(uuid.NewString())}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_user_stats SET image_bytes = 1, audio_bytes = 2 WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	var was Counters
	after := ""
	for {
		next, f, err := s.ReconcileCounters(ctx, after, 50)
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
	if was.ImageBytes != 1 || was.AudioBytes != 2 {
		t.Errorf("drift not reported: %+v", was)
	}
	if got := countersOf(t, s, user); got.ImageBytes != 100 || got.AudioBytes != 40 {
		t.Errorf("not fixed: %+v", got)
	}
}

func TestRecomputeWhileImagesChange(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			id, sha := uuid.NewString(), []byte(uuid.NewString())
			if _, _, err := s.InsertImage(ctx, Image{ID: id, UserID: user, ContentType: "image/jpeg", Size: 10, SHA256: sha}); err != nil {
				t.Error(err)
				return
			}
			if i%2 == 0 { // a delete the way the cleanup does it: the counter moves in the statement of the row
				if _, err := s.pool.Exec(ctx, `WITH d AS (DELETE FROM dictionary_images WHERE id = $1 RETURNING user_id, size)
					UPDATE dictionary_user_stats t SET image_bytes = t.image_bytes - d.size FROM d WHERE t.user_id = d.user_id`, id); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	for i := 0; i < 20; i++ {
		if _, _, err := s.RecomputeUserCounters(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	var rows int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(size), 0) FROM dictionary_images WHERE user_id = $1`, user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if got := countersOf(t, s, user); got.ImageBytes != rows {
		t.Fatalf("counter %d, rows %d", got.ImageBytes, rows)
	}
}

// fileBytes returns the counter of the images and of the audio, and what the rows that are not released add up to.
func fileBytes(t *testing.T, s *Store, user string) (img, aud, imgRows, audRows int64) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `SELECT
		coalesce((SELECT image_bytes FROM dictionary_user_stats WHERE user_id = $1), 0),
		coalesce((SELECT audio_bytes FROM dictionary_user_stats WHERE user_id = $1), 0),
		coalesce((SELECT sum(size) FROM dictionary_images WHERE user_id = $1 AND NOT released), 0)::bigint,
		coalesce((SELECT sum(size) FROM dictionary_audio WHERE user_id = $1 AND NOT released), 0)::bigint`, user).
		Scan(&img, &aud, &imgRows, &audRows); err != nil {
		t.Fatal(err)
	}
	return
}

func wantFiles(t *testing.T, s *Store, user string, img, aud int64) {
	t.Helper()
	if i, a, ir, ar := fileBytes(t, s, user); i != img || a != aud || ir != img || ar != aud {
		t.Fatalf("image bytes: counter %d, rows %d, want %d; audio bytes: counter %d, rows %d, want %d", i, ir, img, a, ar, aud)
	}
}

// Deleting a word takes the files only it used off the counters at once, and the cleanup that deletes them later
// takes nothing off a second time; a word that uses a released file again makes it count again.
func TestDeleteWordReleasesFileBytes(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	img1, img2, aud1 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, im := range []Image{{ID: img1, Size: 100, SHA256: []byte("i1")}, {ID: img2, Size: 50, SHA256: []byte("i2")}} {
		im.UserID, im.ContentType = user, "image/png"
		if _, ok, err := s.InsertImage(ctx, im); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if _, ok, err := s.InsertAudio(ctx, Audio{ID: aud1, UserID: user, ContentType: "audio/mpeg", Size: 30, SHA256: []byte("a1")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	wantFiles(t, s, user, 150, 30)
	save := func(word, content, audio string) {
		t.Helper()
		if _, err := s.SaveWord(ctx, user, SavedWord{Group: "en", Word: word, Content: content, AudioUK: audio}, 100, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	// a uses img1 and the audio, b uses img1 and img2
	save("a", "![](dictimg:"+img1+")", "dictaud:"+aud1)
	save("b", "![](dictimg:"+img1+") ![](dictimg:"+img2+")", "")

	// img1 is still used by b; the audio only by a
	if err := s.DeletePending(ctx, user, "en", "a"); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, s, user, 150, 0)
	// img2 only by b; img1 goes with it
	if err := s.DeletePending(ctx, user, "en", "b"); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, s, user, 0, 0)

	// the rows are still there, for the cleanup
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM dictionary_images WHERE user_id = $1) + (SELECT count(*) FROM dictionary_audio WHERE user_id = $1)`, user).Scan(&rows); err != nil || rows != 3 {
		t.Fatal(rows, err)
	}

	// a word that uses a released file makes it count again
	save("c", "![](dictimg:"+img2+")", "dictaud:"+aud1)
	wantFiles(t, s, user, 50, 30)

	// the cleanup deletes what no word uses (img1) and must not take its size off a second time
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_images SET created_at = now() - interval '2 days' WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	gone, err := s.DeleteUnusedImages(ctx, time.Now().Add(-time.Hour), 100)
	if err != nil || len(gone) != 1 || gone[0].ID != img1 {
		t.Fatal(gone, err)
	}
	wantFiles(t, s, user, 50, 30)

	// the counters are the sum of the rows that count: a recalculation changes nothing
	was, now, err := s.RecomputeUserCounters(ctx, user)
	if err != nil || was != now {
		t.Fatal(was, now, err)
	}
}
