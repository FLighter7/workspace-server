// Package store keeps one JSON document per app on disk. The bytes are stored
// exactly as the last writer sent them; the document's version is the hash of
// those bytes, and every replaced version is kept in a bounded history.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/fsutil"
)

const (
	dataFile   = "data.json"
	historyDir = "history"
	// historyStamp is the UTC layout of the time prefix of a history file
	// name. Names written before 2026-11-10 have no milliseconds; parsing
	// with historyParse reads both.
	historyStamp = "20060102T150405.000Z"
	historyParse = "20060102T150405Z"
)

var (
	// ErrNotJSON is returned by Write when the body is not valid JSON.
	ErrNotJSON = errors.New("store: body is not valid JSON")
	// ErrBadApp is returned when an app id is not a valid id.
	ErrBadApp = errors.New("store: invalid app id")
)

// link makes a history entry as a hard link to the document; a test replaces
// it to stand in for a file system without hard links.
var link = os.Link

// ConflictError is returned by Write when ifEtag does not match the stored
// document. Current is the stored document's etag, or nil if there is none.
type ConflictError struct {
	Current *string
}

func (e *ConflictError) Error() string {
	if e.Current == nil {
		return "store: conflict: no document exists"
	}
	return "store: conflict: current etag is " + *e.Current
}

// Stats describes an app's stored document.
type Stats struct {
	Exists   bool
	Size     int64
	Modified time.Time
	Versions int // number of files in history
}

// Etag returns the lowercase hex SHA-256 of body.
func Etag(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Store is a set of per-app documents under one root directory.
type Store struct {
	root string
	now  func() time.Time

	mu    sync.Mutex // guards locks
	locks map[string]*sync.Mutex
}

// New returns a Store whose apps live in <root>/<id>/. now supplies the time
// used to name history files.
func New(root string, now func() time.Time) *Store {
	return &Store{root: root, now: now, locks: make(map[string]*sync.Mutex)}
}

// lock validates app and takes its lock; the caller must call the returned
// function to release it. The lock covers every read and write of the app's
// files, so a reader never sees a replacement half done.
func (s *Store) lock(app string) (unlock func(), err error) {
	if !apps.ValidID(app) {
		return nil, fmt.Errorf("%w: %q", ErrBadApp, app)
	}
	s.mu.Lock()
	m, ok := s.locks[app]
	if !ok {
		m = new(sync.Mutex)
		s.locks[app] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock, nil
}

func (s *Store) dir(app string) string { return filepath.Join(s.root, app) }

// readCurrent returns the stored bytes and their etag, or nil, nil if there is
// no document. The etag is always computed from the bytes on disk.
func (s *Store) readCurrent(app string) ([]byte, *string, error) {
	body, err := os.ReadFile(filepath.Join(s.dir(app), dataFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	etag := Etag(body)
	return body, &etag, nil
}

// Read returns the app's document and its etag. With no document it returns
// nil, nil, nil.
func (s *Store) Read(app string) ([]byte, *string, error) {
	unlock, err := s.lock(app)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	return s.readCurrent(app)
}

// Write replaces the app's document with body and returns the new etag. body
// must be valid JSON and is stored byte for byte. ifEtag must be the etag of
// the stored document, or nil when no document exists; otherwise Write returns
// a *ConflictError and changes nothing. The replaced document is kept in
// history, of which the newest `history` versions are retained.
//
// data.json is never missing or partial: the old file is first hard-linked
// into history (so its bytes survive), or copied there on a file system
// without hard links, then replaced by an atomic rename. If the write fails,
// the history entry made for it is removed again.
func (s *Store) Write(app string, body []byte, ifEtag *string, history int) (string, error) {
	if !apps.ValidID(app) {
		return "", fmt.Errorf("%w: %q", ErrBadApp, app)
	}
	if !json.Valid(body) {
		return "", ErrNotJSON
	}
	unlock, err := s.lock(app)
	if err != nil {
		return "", err
	}
	defer unlock()

	old, current, err := s.readCurrent(app)
	if err != nil {
		return "", err
	}
	if (current == nil) != (ifEtag == nil) || (current != nil && *current != *ifEtag) {
		return "", &ConflictError{Current: current}
	}

	dataPath := filepath.Join(s.dir(app), dataFile)
	var kept string // history file made for this write, removed if it fails
	if current != nil && history > 0 {
		hdir := filepath.Join(s.dir(app), historyDir)
		if err := os.MkdirAll(hdir, 0o700); err != nil {
			return "", err
		}
		names, err := s.historyNames(app)
		if err != nil {
			return "", err
		}
		// The newest entry may already hold the current document: a write
		// of the same bytes, or a crash between the link and the rename.
		if !newestHolds(hdir, names, *current, old) {
			name := filepath.Join(hdir, nextStamp(s.now(), names)+"-"+(*current)[:8]+".json")
			switch err := link(dataPath, name); {
			case err == nil:
				kept = name
				_ = fsutil.SyncDir(hdir)
			case errors.Is(err, os.ErrExist):
				// Not possible: the name is newer than every entry.
			default:
				// No hard links on this file system (exFAT, FAT, most SMB
				// and CIFS mounts): the entry is a copy of the bytes read
				// above.
				if err := fsutil.WriteFileAtomic(name, old, 0o600); err != nil {
					return "", err
				}
				kept = name
			}
		}
	}
	if err := fsutil.WriteFileAtomic(dataPath, body, 0o600); err != nil {
		if kept != "" {
			_ = os.Remove(kept)
		}
		return "", err
	}
	s.prune(app, history)
	return Etag(body), nil
}

// newestHolds reports whether the newest history entry holds the document
// with etag and bytes doc. The bytes are compared only when the name carries
// the etag's prefix, so a prefix collision never drops a version.
func newestHolds(hdir string, names []string, etag string, doc []byte) bool {
	if len(names) == 0 || !strings.HasSuffix(names[len(names)-1], "-"+etag[:8]+".json") {
		return false
	}
	b, err := os.ReadFile(filepath.Join(hdir, names[len(names)-1]))
	return err == nil && bytes.Equal(b, doc)
}

// nextStamp returns the time prefix for a new history entry: now in UTC, to
// the millisecond, or a millisecond after the newest entry when now is not
// after it (several writes within one millisecond, a coarse clock, or one
// stepped back), so that the names keep the order of the writes.
func nextStamp(now time.Time, names []string) string {
	stamp := now.UTC().Truncate(time.Millisecond)
	if len(names) > 0 {
		if last, ok := historyTime(names[len(names)-1]); ok && !stamp.After(last) {
			stamp = last.Add(time.Millisecond)
		}
	}
	return stamp.Format(historyStamp)
}

// historyTime parses the time prefix of a history file name, with or without
// milliseconds.
func historyTime(name string) (time.Time, bool) {
	stamp, _, ok := strings.Cut(name, "-")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(historyParse, stamp)
	return t, err == nil
}

// historyNames returns the app's history file names, oldest first: by the
// time in the name, a name without one first.
func (s *Store) historyNames(app string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir(app), historyDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		ti, oki := historyTime(names[i])
		tj, okj := historyTime(names[j])
		if oki != okj {
			return !oki
		}
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return names[i] < names[j]
	})
	return names, nil
}

// prune deletes the oldest history files beyond keep. It is best effort: the
// new document is already stored, so a file that cannot be removed is left for
// the next write to retry.
func (s *Store) prune(app string, keep int) {
	names, err := s.historyNames(app)
	if err != nil || len(names) <= keep {
		return
	}
	for _, name := range names[:len(names)-max(keep, 0)] {
		_ = os.Remove(filepath.Join(s.dir(app), historyDir, name))
	}
}

// Stat reports on the app's document and its history.
func (s *Store) Stat(app string) (Stats, error) {
	unlock, err := s.lock(app)
	if err != nil {
		return Stats{}, err
	}
	defer unlock()

	var st Stats
	names, err := s.historyNames(app)
	if err != nil {
		return Stats{}, err
	}
	st.Versions = len(names)
	info, err := os.Stat(filepath.Join(s.dir(app), dataFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return Stats{}, err
	}
	st.Exists, st.Size, st.Modified = true, info.Size(), info.ModTime()
	return st, nil
}

// DeleteApp removes the app's document and history. It is not an error if the
// app has no files.
func (s *Store) DeleteApp(app string) error {
	unlock, err := s.lock(app)
	if err != nil {
		return err
	}
	defer unlock()
	return os.RemoveAll(s.dir(app))
}
