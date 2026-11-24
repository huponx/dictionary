package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests need the schema of db-migration (V15, V20, V21 and V22). Set DICTIONARY_TEST_POSTGRES_URL, for example
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
	page1, err := s.ListPending(ctx, user, time.Time{}, "", "", 2)
	if err != nil || len(page1) != 2 || page1[0].Word != "beta" || page1[1].Word != "gamma" {
		t.Fatalf("page 1: %+v, %v", page1, err)
	}
	last := page1[1]
	page2, err := s.ListPending(ctx, user, last.CreatedAt, last.Word, "", 2)
	if err != nil || len(page2) != 1 || page2[0].Word != "alpha" {
		t.Fatalf("page 2: %+v, %v", page2, err)
	}
	if page1[0].Content != "updated" {
		t.Errorf("content of beta = %q", page1[0].Content)
	}

	all, err := s.Pending(ctx, user, ScopeNew)
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
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 3); err != nil {
			t.Fatalf("a new batch has room: %v", err)
		}
	}
	// saving an exported word again makes it pending again (and counts)
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "beta"}, 4); err != nil {
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
	total, fresh, err := s.CountPending(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return total, fresh
}

func TestExportKeepsWordsInTheList(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	for _, w := range []string{"a", "b", "c"} {
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10); err != nil {
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
	list, err := s.ListPending(ctx, user, time.Time{}, "", "", 10)
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
	if w, _ := s.Pending(ctx, user, ScopeNew); len(w) != 1 || w[0].Word != "c" {
		t.Errorf("new words: %+v", w)
	}
	if w, _ := s.Pending(ctx, user, ScopeAll); len(w) != 3 {
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "d"}, 2); !errors.Is(err, ErrLimit) {
		t.Errorf("the exported word b is in the list, so the limit of 2 is reached: %v", err)
	}
	// saving an exported word again makes it new
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "b"}, 2); err != nil {
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
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveWord(ctx, otherUser, SavedWord{Word: "a"}, 10); err != nil {
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: card(theirs.ID)}, 10); !errors.Is(err, ErrUnknownImage) {
		t.Fatalf("foreign image: %v", err)
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: card(mine.ID)}, 10); err != nil {
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
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: "no image"}, 10); err != nil {
		t.Fatal(err)
	}
	if refs() != 0 {
		t.Fatalf("refs after edit %d", refs())
	}
	if _, err := s.SaveWord(ctx, user, SavedWord{Word: "alpha", Content: card(mine.ID, fresh.ID)}, 10); err != nil || refs() != 2 {
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
		if _, err := s.SaveWord(ctx, user, SavedWord{Word: w}, 10); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]int{"APP": 2, "ana": 1, "%": 1, "_": 1, `\`: 1, "zzz": 0, "": 7} {
		got, err := s.ListPending(ctx, user, time.Time{}, "", q, 10)
		if err != nil || len(got) != want {
			t.Errorf("query %q: %d words (%v), want %d", q, len(got), err, want)
		}
	}
	page1, err := s.ListPending(ctx, user, time.Time{}, "", "app", 1)
	if err != nil || len(page1) != 1 {
		t.Fatalf("page 1: %v %v", page1, err)
	}
	page2, err := s.ListPending(ctx, user, page1[0].CreatedAt, page1[0].Word, "app", 1)
	if err != nil || len(page2) != 1 || page2[0].Word == page1[0].Word {
		t.Fatalf("page 2: %v %v", page2, err)
	}
}
