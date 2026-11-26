package filestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	user = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	id1  = "11111111-1111-4111-8111-111111111111"
	id2  = "22222222-2222-4222-8222-222222222222"
)

func TestWriteReadRemove(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if err := s.Write(user, id1, "jpg", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	b, err := s.Read(user, id1, "jpg")
	if err != nil || !bytes.Equal(b, []byte("abc")) {
		t.Fatalf("%q %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Dir, user))
	if len(entries) != 1 {
		t.Fatalf("left a temporary file: %v", entries)
	}
	if err := s.Remove(user, id1, "jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(user, id1, "jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := s.Remove(user, id1, "jpg"); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
}

func TestPathsAreChecked(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, c := range [][3]string{{"../x", id1, "jpg"}, {user, "../../etc/passwd", "jpg"}, {user, id1, "svg"}} {
		if err := s.Write(c[0], c[1], c[2], nil); err == nil {
			t.Fatalf("%v accepted", c)
		}
	}
}

func TestOrphans(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, id := range []string{id1, id2} {
		if err := s.Write(user, id, "png", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(s.Dir, user, id1+".png"), old, old)
	// id2 is new: kept whatever the database says
	n, err := s.Orphans(user, time.Hour, 10, func(fs []File) (map[string]bool, error) {
		if len(fs) != 1 || fs[0].ID != id1 {
			t.Fatalf("got %v", fs)
		}
		return map[string]bool{}, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := s.Read(user, id1, "png"); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan kept")
	}
	if _, err := s.Read(user, id2, "png"); err != nil {
		t.Fatal("new file removed")
	}
	users, _ := s.Users()
	if len(users) != 1 || users[0] != user {
		t.Fatalf("users %v", users)
	}
}

func TestRemoveAll(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: filepath.Join(dir, "images")}
	user, other := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	img := "33333333-3333-3333-3333-333333333333"
	for _, u := range []string{user, other} {
		if err := s.Write(u, img, "png", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveAll(user); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, user)); !os.IsNotExist(err) {
		t.Errorf("the directory of the user is still there: %v", err)
	}
	if _, err := s.Read(other, img, "png"); err != nil {
		t.Errorf("another user lost a file: %v", err)
	}
	if err := s.RemoveAll(user); err != nil { // a missing directory is fine
		t.Errorf("second RemoveAll: %v", err)
	}
	// only a user id: nothing can leave Dir
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "../", "../keep.txt", "../images", other + "/..", "/", dir, "*"} {
		if err := s.RemoveAll(bad); err == nil {
			t.Errorf("RemoveAll(%q) did not fail", bad)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file outside Dir was touched: %v", err)
	}
	if _, err := s.Read(other, img, "png"); err != nil {
		t.Errorf("another user lost a file after the bad names: %v", err)
	}
}
