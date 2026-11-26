package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests need the schema of db-migration (V15, V20, V21, V22 to V25). Set DICTIONARY_TEST_POSTGRES_URL, for example
// postgres://postgres:postgres@localhost:5432/app?sslmode=disable (the database of `make up` in db-migration).
// testQuota is a content quota no test reaches by accident.
const testQuota int64 = 1 << 30

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
		n, err := s.SaveWord(ctx, user, SavedWord{Word: w, Content: "c-" + w, AudioUS: "https://a/" + w}, 3, testQuota, nil)
		if err != nil || n != i+1 {
			t.Fatalf("save %s: n=%d err=%v", w, n, err)
		}
	}
	// the limit counts pending words; a pending word can be updated at the limit
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "delta"}, 3, testQuota, nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
	if n, err := s.SaveWord(ctx, user, SavedWord{Word: "beta", Content: "updated"}, 3, testQuota, nil); err != nil || n != 3 {
		t.Fatalf("update at the limit: n=%d err=%v", n, err)
	}

	// newest first, in pages of 2, with the keyset; the overwritten beta is the newest now
	page1, err := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{}, 2)
	if err != nil || len(page1) != 2 || page1[0].Word != "beta" || page1[1].Word != "gamma" {
		t.Fatalf("page 1: %+v, %v", page1, err)
	}
	last := page1[1]
	page2, err := s.ListPending(ctx, user, last.CreatedAt, last.Word, "", WordFilter{}, 2)
	if err != nil || len(page2) != 1 || page2[0].Word != "alpha" {
		t.Fatalf("page 2: %+v, %v", page2, err)
	}
	if page1[0].Content != "updated" {
		t.Errorf("content of beta = %q", page1[0].Content)
	}

	all, err := s.Pending(ctx, user, ScopeNew, WordFilter{}, nil, 0)
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "late"}, 3, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkExported(ctx, user, []string{"beta", "gamma"}, true); err != nil || n != 2 {
		t.Fatalf("MarkExported: n=%d err=%v", n, err)
	}
	if n, _ := count(t, s, user); n != 1 {
		t.Errorf("pending after export = %d, want 1 (late)", n)
	}
	// exported words are not deletable as pending and do not count against the limit
	if err := s.DeletePending(ctx, user, "beta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an exported word: %v", err)
	}
	for _, w := range []string{"one", "two"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 3, testQuota, nil); err != nil {
			t.Fatalf("a new batch has room: %v", err)
		}
	}
	// saving an exported word again makes it pending again (and counts)
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "beta"}, 4, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := count(t, s, user); n != 4 {
		t.Errorf("pending = %d, want 4", n)
	}

	// retention: gamma was exported "now", so only a cutoff in the future deletes it, and beta is pending again
	if n, err := s.DeleteRemovedBefore(ctx, time.Now().Add(-time.Hour), 100); err != nil || n != 0 {
		t.Errorf("nothing is that old: n=%d err=%v", n, err)
	}
	if n, err := s.DeleteRemovedBefore(ctx, time.Now().Add(time.Minute), 100); err != nil || n < 1 {
		t.Errorf("cleanup: n=%d err=%v", n, err)
	}
	if n, _ := count(t, s, user); n != 4 {
		t.Errorf("the cleanup must not touch pending words: %d", n)
	}
}

// count is (words in the list, new words).
func count(t *testing.T, s *Store, user string) (int, int) {
	t.Helper()
	total, fresh, _, err := s.CountPending(context.Background(), user, WordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return total, fresh
}

func TestExportKeepsWordsInTheList(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	for _, w := range []string{"a", "b", "c"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	// remove off: the packed words are exported but stay in the list, flagged
	if n, err := s.MarkExported(ctx, user, []string{"a", "b"}, false); err != nil || n != 2 {
		t.Fatalf("MarkExported: n=%d err=%v", n, err)
	}
	if total, fresh := count(t, s, user); total != 3 || fresh != 1 {
		t.Errorf("total=%d new=%d, want 3 and 1", total, fresh)
	}
	list, err := s.ListPending(ctx, user, time.Time{}, "", "", WordFilter{}, 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %+v %v", list, err)
	}
	flags := map[string]bool{}
	for _, w := range list {
		flags[w.Word] = w.Exported
	}
	if !flags["a"] || !flags["b"] || flags["c"] {
		t.Errorf("exported flags = %v", flags)
	}
	// scope: new is only c, all is the three
	if w, _ := s.Pending(ctx, user, ScopeNew, WordFilter{}, nil, 0); len(w) != 1 || w[0].Word != "c" {
		t.Errorf("new words: %+v", w)
	}
	if w, _ := s.Pending(ctx, user, ScopeAll, WordFilter{}, nil, 0); len(w) != 3 {
		t.Errorf("all words: %+v", w)
	}
	// the cleanup never deletes a word that is in the list, however old the cutoff
	if n, err := s.DeleteRemovedBefore(ctx, time.Now().Add(time.Hour), 100); err != nil || n != 0 {
		t.Errorf("cleanup of the list: n=%d err=%v", n, err)
	}
	// an exported word that is in the list can be deleted, and it counts against the limit
	if err := s.DeletePending(ctx, user, "a"); err != nil {
		t.Errorf("delete an exported word of the list: %v", err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "d"}, 2, testQuota, nil); !errors.Is(err, ErrLimit) {
		t.Errorf("the exported word b is in the list, so the limit of 2 is reached: %v", err)
	}
	// saving an exported word again makes it new
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "b"}, 2, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if total, fresh := count(t, s, user); total != 2 || fresh != 2 {
		t.Errorf("total=%d new=%d, want 2 and 2", total, fresh)
	}
}

func TestRemoveExported(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	other, otherUser := testStore(t)
	_ = other
	for _, w := range []string{"a", "b", "c"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveWord(ctx, otherUser, SavedWord{Word: "a"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExported(ctx, user, []string{"a", "b"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExported(ctx, otherUser, []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RemoveExported(ctx, user); err != nil || n != 2 {
		t.Fatalf("RemoveExported: n=%d err=%v", n, err)
	}
	if total, fresh := count(t, s, user); total != 1 || fresh != 1 {
		t.Errorf("only the new word stays: total=%d new=%d", total, fresh)
	}
	if total, _ := count(t, s, otherUser); total != 1 {
		t.Errorf("the words of another user are untouched: %d", total)
	}
	// they left the list, so the retention cleanup deletes them
	if n, err := s.DeleteRemovedBefore(ctx, time.Now().Add(time.Minute), 100); err != nil || n < 2 {
		t.Errorf("cleanup: n=%d err=%v", n, err)
	}
}

func TestUnknownUser(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.SaveWord(context.Background(), uuid.NewString(), SavedWord{Word: "x"}, 3, testQuota, nil); !errors.Is(err, ErrUnknownUser) {
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
			_, err := s.SaveWord(ctx, user, SavedWord{Word: "w" + string(rune('a'+i))}, max, testQuota, nil)
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
	if n, _ := count(t, s, user); ok != max || n != max {
		t.Errorf("saved %d, pending %d, want %d", ok, n, max)
	}
}

func TestImagesAndRefs(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	_, other := testStore(t)
	img := func(owner string) Image {
		return Image{ID: uuid.NewString(), UserID: owner, ContentType: "image/jpeg", Size: 100, SHA256: []byte(uuid.NewString())}
	}
	mine, theirs := img(user), img(other)
	for _, im := range []Image{mine, theirs} {
		if _, ok, err := s.InsertImage(ctx, im); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
	}
	// the same hash of the same user is not inserted twice
	dup := mine
	dup.ID = uuid.NewString()
	if ex, ok, err := s.InsertImage(ctx, dup); err != nil || ok || ex.ID != mine.ID {
		t.Fatalf("dup: %+v %v %v", ex, ok, err)
	}
	if n, _ := s.ImageBytes(ctx, user); n != 100 {
		t.Fatalf("bytes %d", n)
	}
	if _, err := s.GetImage(ctx, user, theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's image: %v", err)
	}

	card := func(ids ...string) string {
		out := ""
		for _, id := range ids {
			out += `<img src="dictimg:` + id + `">`
		}
		return out
	}
	// a card cannot use the image of another user, and a failed save changes nothing
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: card(theirs.ID)}, 10, testQuota, nil); !errors.Is(err, ErrUnknownImage) {
		t.Fatalf("foreign image: %v", err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: card(mine.ID)}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	refs := func() int {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_image_refs WHERE user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if refs() != 1 {
		t.Fatalf("refs %d", refs())
	}
	// an image in use is kept by the cleanup, however old; an unused old one goes
	spare := img(user)
	if _, _, err := s.InsertImage(ctx, spare); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	gone, err := s.DeleteUnusedImages(ctx, future, 100)
	if err != nil {
		t.Fatal(err)
	}
	var deletedMine, deletedSpare bool
	for _, g := range gone {
		deletedMine = deletedMine || g.ID == mine.ID
		deletedSpare = deletedSpare || g.ID == spare.ID
	}
	if deletedMine || !deletedSpare {
		t.Fatalf("cleanup deleted %+v", gone)
	}
	// a new image (cutoff in the past) is kept
	fresh := img(user)
	if _, _, err := s.InsertImage(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if gone, _ := s.DeleteUnusedImages(ctx, time.Now().Add(-time.Hour), 100); len(gone) != 0 {
		t.Fatalf("a new image was deleted: %+v", gone)
	}
	// editing the card replaces the set; deleting the word drops the refs
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: "no image"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	if refs() != 0 {
		t.Fatalf("refs after edit %d", refs())
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: card(mine.ID, fresh.ID)}, 10, testQuota, nil); err != nil || refs() != 2 {
		t.Fatalf("two refs: %v %d", err, refs())
	}
	if err := s.DeletePending(ctx, user, "alpha"); err != nil || refs() != 0 {
		t.Fatalf("delete: %v %d", err, refs())
	}
	if ids, _ := s.ExistingImageIDs(ctx, []string{mine.ID, spare.ID}); !ids[mine.ID] || ids[spare.ID] {
		t.Fatalf("existing %v", ids)
	}
}

func TestListPendingQuery(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	for _, w := range []string{"Apple", "pineapple", "banana", "100%", "a_b", "axb", `back\slash`} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]int{"APP": 2, "ana": 1, "%": 1, "_": 1, `\`: 1, "zzz": 0, "": 7} {
		got, err := s.ListPending(ctx, user, time.Time{}, "", q, WordFilter{}, 10)
		if err != nil || len(got) != want {
			t.Errorf("query %q: %d words (%v), want %d", q, len(got), err, want)
		}
	}
	page1, err := s.ListPending(ctx, user, time.Time{}, "", "app", WordFilter{}, 1)
	if err != nil || len(page1) != 1 {
		t.Fatalf("page 1: %v %v", page1, err)
	}
	page2, err := s.ListPending(ctx, user, page1[0].CreatedAt, page1[0].Word, "app", WordFilter{}, 1)
	if err != nil || len(page2) != 1 || page2[0].Word == page1[0].Word {
		t.Fatalf("page 2: %v %v", page2, err)
	}
}

func TestAudioAndRefs(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	_, other := testStore(t)
	aud := func(owner string) Audio {
		return Audio{ID: uuid.NewString(), UserID: owner, ContentType: "audio/mpeg", Size: 100, SHA256: []byte(uuid.NewString())}
	}
	mine, theirs := aud(user), aud(other)
	for _, a := range []Audio{mine, theirs} {
		if _, ok, err := s.InsertAudio(ctx, a); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
	}
	dup := mine
	dup.ID = uuid.NewString()
	if ex, ok, err := s.InsertAudio(ctx, dup); err != nil || ok || ex.ID != mine.ID {
		t.Fatalf("dup: %+v %v %v", ex, ok, err)
	}
	if n, _ := s.AudioBytes(ctx, user); n != 100 {
		t.Fatalf("bytes %d", n)
	}
	if _, err := s.GetAudio(ctx, user, theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's audio: %v", err)
	}
	ref := func(a Audio) string { return "dictaud:" + a.ID }
	// a card cannot use the file of another user; a URL is not a reference
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", AudioUK: ref(theirs)}, 10, testQuota, nil); !errors.Is(err, ErrUnknownAudio) {
		t.Fatalf("foreign audio: %v", err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", AudioUK: ref(mine), AudioUS: "https://a/x.mp3"}, 10, testQuota, nil); err != nil {
		t.Fatal(err)
	}
	refs := func() int {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_audio_refs WHERE user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if refs() != 1 {
		t.Fatalf("refs %d", refs())
	}
	// a file in use is kept by the cleanup, however old; an unused old one goes; a new one stays
	spare := aud(user)
	if _, _, err := s.InsertAudio(ctx, spare); err != nil {
		t.Fatal(err)
	}
	gone, err := s.DeleteUnusedAudio(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var deletedMine, deletedSpare bool
	for _, g := range gone {
		deletedMine = deletedMine || g.ID == mine.ID
		deletedSpare = deletedSpare || g.ID == spare.ID
	}
	if deletedMine || !deletedSpare {
		t.Fatalf("cleanup deleted %+v", gone)
	}
	fresh := aud(user)
	if _, _, err := s.InsertAudio(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if gone, _ := s.DeleteUnusedAudio(ctx, time.Now().Add(-time.Hour), 100); len(gone) != 0 {
		t.Fatalf("a new file was deleted: %+v", gone)
	}
	// editing the card replaces the set; deleting the word drops the refs
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha"}, 10, testQuota, nil); err != nil || refs() != 0 {
		t.Fatalf("after edit: %v %d", err, refs())
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", AudioUK: ref(mine), AudioUS: ref(fresh)}, 10, testQuota, nil); err != nil || refs() != 2 {
		t.Fatalf("two refs: %v %d", err, refs())
	}
	if err := s.DeletePending(ctx, user, "alpha"); err != nil || refs() != 0 {
		t.Fatalf("delete: %v %d", err, refs())
	}
	if ids, _ := s.ExistingAudioIDs(ctx, []string{mine.ID, spare.ID}); !ids[mine.ID] || ids[spare.ID] {
		t.Fatalf("existing %v", ids)
	}
}

// listedOf reads the counter and the real number of words in the list.
func listedOf(t *testing.T, s *Store, user string) (counter, real int) {
	t.Helper()
	ctx := context.Background()
	n, err := s.ListedCount(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL`, user).Scan(&real); err != nil {
		t.Fatal(err)
	}
	return n, real
}

// The counter of the list never drifts from count(*) after any mix of the operations that change the list.
func TestListedCounterInvariant(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	words := make([]string, 30)
	for i := range words {
		words[i] = "w" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	for step := range 300 {
		w := words[rnd.IntN(len(words))]
		switch rnd.IntN(6) {
		case 0, 1, 2:
			if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 20, testQuota, nil); err != nil && !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
		case 3:
			if err := s.DeletePending(ctx, user, w); err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
		case 4:
			if _, err := s.MarkExported(ctx, user, []string{w, words[rnd.IntN(len(words))]}, rnd.IntN(2) == 0); err != nil {
				t.Fatal(err)
			}
		case 5:
			if _, err := s.RemoveExported(ctx, user); err != nil {
				t.Fatal(err)
			}
		}
		if c, r := listedOf(t, s, user); c != r {
			t.Fatalf("step %d: counter %d, count(*) %d", step, c, r)
		}
	}
}

func TestReconcileListedFixesDrift(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	for _, w := range []string{"a", "b"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_user_stats SET listed = 7 WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	var fixed map[string]int64
	after := ""
	for { // other users may exist in a shared test database: walk to the end
		next, f, err := s.ReconcileListed(ctx, after, 50)
		if err != nil {
			t.Fatal(err)
		}
		for u, was := range f {
			if u == user {
				fixed = map[string]int64{u: was.Listed}
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if fixed[user] != 7 {
		t.Errorf("drift not reported: %v", fixed)
	}
	if c, r := listedOf(t, s, user); c != 2 || r != 2 {
		t.Errorf("counter %d, count(*) %d, want 2", c, r)
	}
}

func TestRemoveExportedInBatches(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	n := removeBatch + 7
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_saved_words (user_id, word, content, exported_at)
		SELECT $1, 'w' || g, '', now() FROM generate_series(1, $2) g`, user, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_user_stats (user_id, listed) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET listed = EXCLUDED.listed`, user, n); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RemoveExported(ctx, user); err != nil || got != int64(n) {
		t.Fatalf("RemoveExported: %d, %v, want %d", got, err, n)
	}
	if c, r := listedOf(t, s, user); c != 0 || r != 0 {
		t.Errorf("counter %d, count(*) %d, want 0", c, r)
	}
}

func TestPendingLimit(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	for _, w := range []string{"a", "b", "c", "d"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10, testQuota, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Pending(ctx, user, ScopeNew, WordFilter{}, nil, 3)
	if err != nil || len(got) != 3 || got[0].Word != "a" || got[2].Word != "c" { // the oldest first
		t.Errorf("limit 3: %+v %v", got, err)
	}
	if got, _ := s.Pending(ctx, user, ScopeNew, WordFilter{}, nil, 0); len(got) != 4 {
		t.Errorf("no limit: %d words", len(got))
	}
}
