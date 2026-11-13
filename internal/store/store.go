// Package store keeps each app's documents as plain files in the app's
// folder, <root>/<app>/<folder>/<id>.<type> (see Key), and the server's own
// files beside them in <root>/<app>/.wss/:
//
//	.wss/index.json        the set: each document's etag, size and mtime, the
//	                       set's revision and size, and the tombstones of
//	                       deleted documents (the newest MaxTombstones)
//	.wss/journal.json      only while a commit is being applied
//	.wss/staging/<tx>/     an open transaction: tx.json and the bodies
//
// A document's etag is the lowercase hex SHA-256 of its bytes; the set's
// revision is the SHA-256 of the sorted list of its keys and etags (see
// revisionOf), nil for an empty set. The index is a cache: Reconcile compares
// it with the files on disk by size and modification time, so a file changed,
// added or removed by hand is taken in, and a file whose name is not a valid
// key is neither shown nor touched.
//
// Writes arrive as transactions: Begin, then Put and Delete stage changes,
// and Commit applies them all or none, through a journal (see commit.go).
//
// Every open of a document goes through fsutil.OpenShared, never os.Open: on
// Windows one plain handle on a document makes the rename of a commit over
// it fail until the handle closes.
package store

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/fsutil"
)

const (
	wssName     = ".wss"
	indexName   = "index.json"
	journalName = "journal.json"
	stagingName = "staging"
	txName      = "tx.json"

	// MaxTombstones is how many tombstones of deleted documents an app keeps,
	// the newest.
	MaxTombstones = 1000
	// StagingTTL is how long an open transaction lives after its last
	// activity; Sweep removes it after that.
	StagingTTL = 24 * time.Hour
)

// Errors the protocol maps to its codes. They are wrapped with details, so
// test them with errors.Is; a conflict is a *ConflictError.
var (
	// ErrBadApp is returned when an app id is not a valid id.
	ErrBadApp = errors.New("store: invalid app id")
	// ErrQuota: the set with the transaction would exceed the app's quota.
	ErrQuota = errors.New("store: the app's quota would be exceeded")
	// ErrNoTransaction: no open transaction of that id for that device: it
	// was never begun, was committed, aborted or discarded by a newer Begin,
	// expired, or belongs to another device.
	ErrNoTransaction = errors.New("store: no such transaction")
	// ErrBadTx: a transaction id that is not 16 to 64 of [A-Za-z0-9_-].
	ErrBadTx = errors.New("store: invalid transaction id")
	// ErrIncomplete: Commit found a body that has not fully arrived.
	ErrIncomplete = errors.New("store: a body has not fully arrived")
	// ErrStale: the document asked for is not there with that etag (any
	// more).
	ErrStale = errors.New("store: the document has changed")
	// ErrBadBody: a put's size or offset does not fit what is staged, or
	// its body runs past the size it announced.
	ErrBadBody = errors.New("store: the body does not fit its announced size or offset")
)

// ConflictError is returned by Commit when a document's etag is not the one
// the transaction expects. The transaction stays open. Revision is the set's
// revision now, and Docs the current etags of the documents that conflict,
// nil for one that does not exist.
type ConflictError struct {
	Revision *string
	Docs     []DocEtag
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("store: conflict on %d document(s)", len(e.Docs))
}

// Current returns the current etag of k as the conflict reported it, nil if
// k does not exist or did not conflict.
func (e *ConflictError) Current(k Key) *string {
	for _, d := range e.Docs {
		if d.Key == k {
			return d.Etag
		}
	}
	return nil
}

// DocEtag is a document and its etag, nil when it does not exist.
type DocEtag struct {
	Key  Key
	Etag *string
}

// DocInfo describes a document of the set.
type DocInfo struct {
	Key   Key
	Etag  string
	Bytes int64
}

// Removed is the tombstone of a deleted document.
type Removed struct {
	Key Key
	At  time.Time
}

// Listing is an app's set: its revision, its documents sorted by key, and
// the tombstones of the documents deleted from it, sorted by key.
type Listing struct {
	Revision *string
	Docs     []DocInfo
	Removed  []Removed
}

// Stats sums up an app's set for the server's page.
type Stats struct {
	Files    int
	Bytes    int64
	Quota    int64
	Modified time.Time // the last change of the set; zero if it never changed
}

// Etag returns the lowercase hex SHA-256 of body.
func Etag(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Store is the apps' sets under one root folder. It is safe for concurrent
// use; each app has its own lock, which every operation on the app takes.
type Store struct {
	root  string
	now   func() time.Time
	quota func(app string) int64

	mu   sync.Mutex // guards apps
	apps map[string]*appState

	logFn func(format string, args ...any) // set by SetLogf before use
}

// SetLogf makes the store log what it does on its own, such as a change of
// a commit it had to drop, through logf. Call it before the store is used.
func (s *Store) SetLogf(logf func(format string, args ...any)) { s.logFn = logf }

func (s *Store) logf(format string, args ...any) {
	if s.logFn != nil {
		s.logFn(format, args...)
	}
}

// appState is what the store holds of an app in memory. mu is the app's
// lock and guards the rest.
type appState struct {
	mu      sync.Mutex
	loaded  bool
	idx     *index
	onDisk  bool                // index.json exists
	txs     map[string]*txState // the open transactions, by id
	journal bool                // a journal waits to be applied
	unsaved bool                // the index changed, but saving it failed
	// failing is when applying the journal first failed for a reason that
	// may pass; zero while nothing failed.
	failing time.Time
}

// failingSince returns when applying a's journal began to fail, starting
// the clock now if it had not; apply calls it only after a failure.
func (s *Store) failingSince(a *appState) time.Time {
	if a.failing.IsZero() {
		a.failing = s.now()
	}
	return a.failing
}

// New returns a Store whose apps live in <root>/<app>/. now gives the time of
// tombstones and transactions; quota gives an app's quota in bytes, and nil,
// or a value of 0 or less, means apps.DefaultQuota. New touches no file: an
// app is loaded at its first operation, and Recover loads them all.
func New(root string, now func() time.Time, quota func(app string) int64) *Store {
	return &Store{root: root, now: now, quota: quota, apps: make(map[string]*appState)}
}

func (s *Store) quotaOf(app string) int64 {
	if s.quota != nil {
		if q := s.quota(app); q > 0 {
			return q
		}
	}
	return apps.DefaultQuota
}

func (s *Store) dir(app string) string { return filepath.Join(s.root, app) }

func (s *Store) wss(app string) string { return filepath.Join(s.root, app, wssName) }

// docPath is the file of key k, a key's String().
func (s *Store) docPath(app, k string) string {
	return filepath.Join(s.dir(app), filepath.FromSlash(k))
}

// lock validates app and takes its lock; the caller calls unlock.
func (s *Store) lock(app string) (a *appState, unlock func(), err error) {
	if !apps.ValidID(app) {
		return nil, nil, fmt.Errorf("%w: %q", ErrBadApp, app)
	}
	s.mu.Lock()
	a, ok := s.apps[app]
	if !ok {
		a = &appState{}
		s.apps[app] = a
	}
	s.mu.Unlock()
	a.mu.Lock()
	return a, a.mu.Unlock, nil
}

// prepare makes a's state current. It applies a journal that a commit left
// (at a crash, or a failure after its commit point), which it looks for on
// every operation: the journal is the truth, whatever the memory says. At the
// first operation on the app it also loads the index and the open
// transactions, after the journal, and reconciles the index with the files.
// The caller holds a.mu.
func (s *Store) prepare(app string, a *appState) error {
	if !a.loaded {
		idx, onDisk, err := loadIndex(filepath.Join(s.wss(app), indexName))
		if err != nil {
			return err
		}
		a.idx, a.onDisk = idx, onDisk
	}
	if _, err := os.Lstat(filepath.Join(s.wss(app), journalName)); err == nil {
		a.journal = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if a.journal {
		if err := s.replay(app, a); err != nil {
			return err
		}
	}
	if a.loaded {
		if a.unsaved {
			return s.saveOrMark(app, a)
		}
		return nil
	}
	// After the journal, which removes its own transaction's staging.
	txs, err := s.loadTxs(app)
	if err != nil {
		return err
	}
	a.txs = txs
	if _, err := s.reconcile(app, a); err != nil {
		return err
	}
	a.loaded = true
	return nil
}

// do runs f on app's state, prepared, under the app's lock.
func (s *Store) do(app string, f func(a *appState) error) error {
	a, unlock, err := s.lock(app)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.prepare(app, a); err != nil {
		return err
	}
	return f(a)
}

// Recover loads every app folder under the root that has a .wss folder:
// it applies a journal a commit left, reconciles the index with the files and
// removes expired transactions. The server calls it once at start; an app
// that fails is loaded again at its next operation. The error joins every
// app's failure.
func (s *Store) Recover() error {
	var errs []error
	for _, app := range s.appFolders() {
		err := s.do(app, func(a *appState) error { return s.sweep(app, a, s.now()) })
		if err != nil {
			errs = append(errs, fmt.Errorf("app %q: %w", app, err))
		}
	}
	return errors.Join(errs...)
}

// Sweep removes the transactions whose last activity is StagingTTL or more
// before now, and whatever else lies in a staging folder that is no open
// transaction, in every app folder that has a .wss folder.
func (s *Store) Sweep(now time.Time) error {
	var errs []error
	for _, app := range s.appFolders() {
		err := s.do(app, func(a *appState) error { return s.sweep(app, a, now) })
		if err != nil {
			errs = append(errs, fmt.Errorf("app %q: %w", app, err))
		}
	}
	return errors.Join(errs...)
}

// appFolders returns the ids of the folders under the root that are app
// folders: a valid app id and a .wss folder inside.
func (s *Store) appFolders() []string {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || !apps.ValidID(e.Name()) {
			continue
		}
		if fi, err := os.Lstat(s.wss(e.Name())); err == nil && fi.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// Reconcile compares app's index with the files on disk (see the package
// comment) and reports whether the revision changed, and the revision now.
func (s *Store) Reconcile(app string) (changed bool, revision *string, err error) {
	err = s.do(app, func(a *appState) error {
		changed, err = s.reconcile(app, a)
		revision = a.idx.Revision
		return err
	})
	return changed, revision, err
}

// Head returns app's revision, nil for an empty set.
func (s *Store) Head(app string) (*string, error) {
	var rev *string
	err := s.do(app, func(a *appState) error {
		rev = a.idx.Revision
		return nil
	})
	return rev, err
}

// List returns app's set as the index holds it.
func (s *Store) List(app string) (Listing, error) {
	var l Listing
	err := s.do(app, func(a *appState) error {
		l.Revision = a.idx.Revision
		l.Docs = make([]DocInfo, 0, len(a.idx.Docs))
		for ks, e := range a.idx.Docs {
			k, _ := ParseKey(ks) // the index holds valid keys only
			l.Docs = append(l.Docs, DocInfo{Key: k, Etag: e.Etag, Bytes: e.Bytes})
		}
		slices.SortFunc(l.Docs, func(x, y DocInfo) int { return cmp.Compare(x.Key.String(), y.Key.String()) })
		l.Removed = make([]Removed, 0, len(a.idx.Removed))
		for _, t := range a.idx.Removed {
			k, _ := ParseKey(t.Key)
			l.Removed = append(l.Removed, Removed{Key: k, At: t.At})
		}
		slices.SortFunc(l.Removed, func(x, y Removed) int { return cmp.Compare(x.Key.String(), y.Key.String()) })
		return nil
	})
	return l, err
}

// Open opens the document k of app for reading if its etag is etag, and
// returns it with its information. The file is opened through
// fsutil.OpenShared under the app's lock; the caller reads it without the
// lock, from any offset (Seek), and closes it. A commit may replace or remove
// the document meanwhile: the open file keeps reading the bytes it had. A
// document that is not there with that etag is ErrStale. A file that differs
// from the index (an edit by hand) is reconciled first.
func (s *Store) Open(app string, k Key, etag string) (*os.File, DocInfo, error) {
	if err := k.Validate(); err != nil {
		return nil, DocInfo{}, err
	}
	var f *os.File
	var info DocInfo
	err := s.do(app, func(a *appState) error {
		ks := k.String()
		for attempt := 0; ; attempt++ {
			e, ok := a.idx.Docs[ks]
			if !ok || e.Etag != etag {
				return ErrStale
			}
			file, err := fsutil.OpenShared(s.docPath(app, ks))
			if err == nil {
				fi, serr := file.Stat()
				if serr == nil && fi.Mode().IsRegular() && fi.Size() == e.Bytes && fi.ModTime().UnixNano() == e.Mtime {
					f, info = file, DocInfo{Key: k, Etag: e.Etag, Bytes: e.Bytes}
					return nil
				}
				_ = file.Close()
				err = serr
			}
			if attempt > 0 {
				if err == nil || errors.Is(err, fs.ErrNotExist) {
					return ErrStale
				}
				return err
			}
			// Changed, removed or replaced by a folder by hand since the
			// index saw it: the reconcile tells.
			if _, err := s.reconcile(app, a); err != nil {
				return err
			}
		}
	})
	return f, info, err
}

// Stats sums up app's set.
func (s *Store) Stats(app string) (Stats, error) {
	var st Stats
	err := s.do(app, func(a *appState) error {
		st = Stats{Files: len(a.idx.Docs), Bytes: a.idx.Bytes, Quota: s.quotaOf(app), Modified: a.idx.Written}
		return nil
	})
	return st, err
}

// DeleteApp removes app's folder with everything in it and forgets its
// state; its open transactions end. It is not an error if the folder is
// missing.
func (s *Store) DeleteApp(app string) error {
	a, unlock, err := s.lock(app)
	if err != nil {
		return err
	}
	defer unlock()
	for _, t := range a.txs {
		t.kill()
	}
	// Loaded again from disk at the next operation.
	a.loaded, a.idx, a.onDisk, a.txs, a.journal, a.unsaved = false, nil, false, nil, false, false
	return os.RemoveAll(s.dir(app))
}
