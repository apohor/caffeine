// Package profileimages persists AI-generated profile artwork on the local
// filesystem. Images are keyed by the machine profile id; there is no
// versioning — a new generation replaces the previous image.
package profileimages

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Store is a thin filesystem-backed image store. It is safe for
// concurrent use.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// Image is a stored image with its mime-type and modification time.
type Image struct {
	Data     []byte
	MimeType string
	Modified time.Time
}

// ErrNotFound is returned when no image exists for the requested id.
var ErrNotFound = errors.New("profile image not found")

// Open creates the directory if needed and returns a ready Store.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("profileimages: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Matches UUID-ish ids the machine uses: [A-Za-z0-9._-]{1,128}. We reject
// anything else to keep the id safe as a filename component.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (s *Store) pathFor(id, ext string) (string, error) {
	if !safeID.MatchString(id) {
		return "", fmt.Errorf("invalid id %q", id)
	}
	return filepath.Join(s.dir, id+ext), nil
}

func extForMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// Put replaces any existing image for id with the supplied bytes. It uses
// an atomic rename so concurrent Get calls always see a complete file.
func (s *Store) Put(id, mime string, data []byte) error {
	ext := extForMime(mime)
	dst, err := s.pathFor(id, ext)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove any existing image in a different extension so Get can find
	// the new one deterministically.
	s.removeAll(id)

	tmp, err := os.CreateTemp(s.dir, ".tmp-"+id+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// Get returns the stored image for id, or ErrNotFound.
func (s *Store) Get(id string) (*Image, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		p, err := s.pathFor(id, ext)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return &Image{Data: data, MimeType: mimeForExt(ext), Modified: info.ModTime()}, nil
	}
	return nil, ErrNotFound
}

// Has reports whether an image exists for id without loading it.
func (s *Store) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		p, err := s.pathFor(id, ext)
		if err != nil {
			return false
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Delete removes any stored image for id. Returns nil if none existed.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeAll(id)
	return nil
}

// removeAll is the lock-free helper used while s.mu is already held.
func (s *Store) removeAll(id string) {
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		if p, err := s.pathFor(id, ext); err == nil {
			_ = os.Remove(p)
		}
	}
}

// List returns the set of profile ids that currently have a stored image.
// Used by the frontend to decide whether to show the AI badge per card.
func (s *Store) List() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			continue
		}
		ext := filepath.Ext(name)
		id := strings.TrimSuffix(name, ext)
		if !safeID.MatchString(id) {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
