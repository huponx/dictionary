package filestore

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
var ErrNotFound = errors.New("filestore: not found")

var (
	idRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	fileRe = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.(jpg|png|mp3|ogg|wav|m4a|apkg)$`)
)

// Store keeps the files of the uploads (images or audio) under Dir.
type Store struct {
	Dir string
}

var exts = map[string]string{
	"image/jpeg": "jpg", "image/png": "png",
	"audio/mpeg": "mp3", "audio/ogg": "ogg", "audio/wav": "wav", "audio/mp4": "m4a",
	"application/vnd.anki": "apkg", // the deck files of the export jobs
}

// Ext is the file extension of a content type of NormalizeImage or SniffAudio ("jpg" for an unknown one).
func Ext(contentType string) string {
	if e, ok := exts[contentType]; ok {
		return e
	}
	return "jpg"
}

// ContentType is the content type of a file extension ("image/jpeg" for an unknown one).
func ContentType(ext string) string {
	for ct, e := range exts {
		if e == ext {
			return ct
		}
	}
	return "image/jpeg"
}

// Hash is the sha256 of the (normalized) bytes.
func Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

func validExt(ext string) bool {
	for _, e := range exts {
		if e == ext {
			return true
		}
	}
	return false
}

// path is the file of an upload; ids come from the database or from uuid.New, but they are checked again
// because they become a path.
func (s *Store) path(user, id, ext string) (string, error) {
	if !idRe.MatchString(user) || !idRe.MatchString(id) || !validExt(ext) {
		return "", fmt.Errorf("filestore: bad name %q/%q.%q", user, id, ext)
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

// Open returns the file and its size, ErrNotFound when the file is missing. The caller closes it.
func (s *Store) Open(user, id, ext string) (*os.File, int64, error) {
	p, err := s.path(user, id, ext)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
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

// RemoveAll deletes the directory of a user with every file in it (the account was deleted); a missing directory is
// not an error. The user must be a UUID, so the path cannot leave Dir.
func (s *Store) RemoveAll(user string) error {
	if !idRe.MatchString(user) {
		return fmt.Errorf("filestore: bad user %q", user)
	}
	return os.RemoveAll(filepath.Join(s.Dir, user))
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
		return 0, fmt.Errorf("filestore: bad user %q", user)
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
