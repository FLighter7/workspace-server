package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Side files: what others keep about an app beside its set, in the app's
// .wss folder, such as the link's base (link.json). They go with the app's
// folder (a data-folder move copies them, DeleteApp removes them), and the
// store never reads them itself.

// sideName is a side file's name: a plain lowercase JSON file name.
var sideName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}\.json$`)

// validSide reports whether name may be a side file: not one of the store's
// own files.
func validSide(name string) bool {
	return sideName.MatchString(name) && name != indexName && name != journalName && name != txName
}

func (s *Store) sidePath(app, name string) (string, error) {
	if !validSide(name) {
		return "", fmt.Errorf("store: %q cannot be a side file", name)
	}
	return filepath.Join(s.wss(app), name), nil
}

// ReadSide returns the side file name of app; fs.ErrNotExist when there is
// none.
func (s *Store) ReadSide(app, name string) ([]byte, error) {
	path, err := s.sidePath(app, name)
	if err != nil {
		return nil, err
	}
	_, unlock, err := s.lock(app)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readFile(path)
}

// WriteSide writes the side file name of app atomically, creating the app's
// .wss folder if need be.
func (s *Store) WriteSide(app, name string, data []byte) error {
	path, err := s.sidePath(app, name)
	if err != nil {
		return err
	}
	_, unlock, err := s.lock(app)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.MkdirAll(s.wss(app), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, data, 0o600)
}

// RemoveSide removes the side file name of app; a missing one is no error.
func (s *Store) RemoveSide(app, name string) error {
	path, err := s.sidePath(app, name)
	if err != nil {
		return err
	}
	_, unlock, err := s.lock(app)
	if err != nil {
		return err
	}
	defer unlock()
	if err := removeFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
