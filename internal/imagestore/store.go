package imagestore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ErrNotFound: there is no such file.
var ErrNotFound = errors.New("imagestore: not found")

var (
	idRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	fileRe = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.(jpg|png)$`)
)

// Store keeps the files of the images under Dir.
type Store struct {
	Dir string
}

// Ext is the file extension of a content type of Normalize.
func Ext(contentType string) string {
	if contentType == "image/png" {
		return "png"
	}
	return "jpg"
}

// ContentType is the content type of a file extension.
func ContentType(ext string) string {
	if ext == "png" {
		return "image/png"
	}
	return "image/jpeg"
}

// Hash is the sha256 of the (normalized) bytes.
func Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// path is the file of an image; ids come from the database or from uuid.New, but they are checked again
// because they become a path.
func (s *Store) path(user, id, ext string) (string, error) {
	if !idRe.MatchString(user) || !idRe.MatchString(id) || (ext != "jpg" && ext != "png") {
		return "", fmt.Errorf("imagestore: bad name %q/%q.%q", user, id, ext)
	}
	return filepath.Join(s.Dir, user, id+"."+ext), nil
}

// Write stores the bytes: a temporary file in the same directory, then a rename, so a reader never sees
// half a file.
func (s *Store) Write(user, id, ext string, data []byte) error {
	p, err := s.path(user, id, ext)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Read returns the bytes of an image, ErrNotFound when the file is missing.
func (s *Store) Read(user, id, ext string) ([]byte, error) {
	p, err := s.path(user, id, ext)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// Remove deletes the file of an image; a missing file is not an error.
func (s *Store) Remove(user, id, ext string) error {
	p, err := s.path(user, id, ext)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// File is an image file found on the volume.
type File struct {
	User, ID, Ext string
}

// Orphans calls keep with the files of one directory of a user that are older than minAge, in groups of at most
// batch, so the caller can compare them with the database; keep returns the ids that still have a row (the others are deleted). It stops at the first
// error. Temporary files older than minAge are deleted too.
func (s *Store) Orphans(user string, minAge time.Duration, batch int, keep func([]File) (map[string]bool, error)) (removed int, err error) {
	if !idRe.MatchString(user) {
		return 0, fmt.Errorf("imagestore: bad user %q", user)
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, user))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-minAge)
	var group []File
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		exists, err := keep(group)
		if err != nil {
			return err
		}
		for _, f := range group {
			if exists[f.ID] {
				continue
			}
			if err := s.Remove(f.User, f.ID, f.Ext); err != nil {
				return err
			}
			removed++
		}
		group = group[:0]
		return nil
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			if len(e.Name()) > 5 && e.Name()[:5] == ".tmp-" {
				os.Remove(filepath.Join(s.Dir, user, e.Name()))
			}
			continue
		}
		group = append(group, File{User: user, ID: m[1], Ext: m[2]})
		if len(group) >= batch {
			if err := flush(); err != nil {
				return removed, err
			}
		}
	}
	return removed, flush()
}

// Users lists the user directories of the volume.
func (s *Store) Users() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && idRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
