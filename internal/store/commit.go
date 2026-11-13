package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A commit runs under the app's lock:
//
//  1. check: every body complete, every ifEtag matching the index after a
//     reconcile, every path free (checkTargets), the quota;
//  2. write .wss/journal.json, sync it and flush it: the commit point.
//     Before it no file of the set has been touched, so a failure is a
//     rollback by doing nothing; the transaction stays open;
//  3. apply the journal: rename each body into place (fsutil.Rename, which
//     replaces a document that a get still reads through OpenShared),
//     remove each deleted document and the folders that leaves empty;
//  4. write the index;
//  5. remove the transaction's staging folder, then the journal.
//
// A failure after the commit point that may pass (another program's handle
// on a file, see isTransient) leaves the journal, and the next operation on
// the app, or the next start, applies it again from the top: every step can
// run twice. Meanwhile every operation on the app fails. So that no failure
// stops an app for good, a change that keeps failing (for any reason other
// than a transient one, or a transient one for journalGiveUp) is dropped: it
// is logged, the rest of the journal goes on, and the index is reconciled
// with what the disk holds. A failure to write the index only marks it
// unsaved: it is a cache, which a reconcile rebuilds. Only a journal that
// cannot be removed at all keeps the app stopped.

// journalGiveUp is how long a change that fails only because a file is busy
// is tried again, at every operation on the app, before it is dropped.
var journalGiveUp = time.Minute

// journal is .wss/journal.json: the changes of one commit, ready to apply.
// Staging is the commit's folder in .wss/staging, removed once it is
// applied.
type journal struct {
	Tx      string      `json:"tx"`
	Staging string      `json:"staging"`
	At      time.Time   `json:"at"`
	Ops     []journalOp `json:"ops"`
}

// journalOp is one change: the body at From (relative to .wss, with "/")
// goes to Key, or Key is deleted.
type journalOp struct {
	Key    string `json:"key"`
	Delete bool   `json:"delete,omitempty"`
	From   string `json:"from,omitempty"`
	Etag   string `json:"etag,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// Committed is what a commit changed: the set's new revision, and each
// document it wrote with its etag, or deleted with a nil etag, in the order
// the transaction staged them.
type Committed struct {
	Revision *string
	Docs     []DocEtag
}

// Commit applies device's transaction tx to app's set, all of it or nothing.
// A body still arriving is ErrIncomplete, a document whose etag is not the
// one staged is a *ConflictError, a path that something else holds on disk
// is ErrBadPath (see checkTargets), and a set that would exceed the app's
// quota is ErrQuota; each leaves the transaction open. Once it succeeds the
// transaction is gone.
func (s *Store) Commit(app, device, tx string) (Committed, error) {
	var out Committed
	err := s.do(app, func(a *appState) error {
		t, err := s.txOf(a, device, tx)
		if err != nil {
			return err
		}
		for _, op := range t.rec.Ops {
			if op.Delete {
				continue
			}
			if op.Received < op.Bytes || t.sinks[op.Key] != nil {
				return fmt.Errorf("%w: %s has %d of %d bytes", ErrIncomplete, op.Key, op.Received, op.Bytes)
			}
		}
		for i := range t.rec.Ops {
			op := &t.rec.Ops[i]
			if !op.Delete && op.Etag == "" { // complete before a restart
				etag, size, _, err := hashFile(filepath.Join(t.dir, op.File))
				if err != nil {
					return err
				}
				if size != op.Bytes {
					op.Received = min(size, op.Bytes)
					return fmt.Errorf("%w: %s has %d of %d bytes", ErrIncomplete, op.Key, size, op.Bytes)
				}
				op.Etag = etag
			}
		}
		if _, err := s.reconcile(app, a); err != nil {
			return err
		}
		var conflicts []DocEtag
		for _, op := range t.rec.Ops {
			cur, ok := a.idx.Docs[op.Key]
			if ok && op.IfEtag != nil && cur.Etag == *op.IfEtag || !ok && op.IfEtag == nil {
				continue
			}
			k, _ := ParseKey(op.Key)
			var etag *string
			if ok {
				etag = &cur.Etag
			}
			conflicts = append(conflicts, DocEtag{Key: k, Etag: cloneEtag(etag)})
		}
		t.rec.Active = s.now()
		if conflicts != nil {
			_ = s.persistTx(t)
			return &ConflictError{Revision: cloneEtag(a.idx.Revision), Docs: conflicts}
		}
		keys := make([]string, 0, len(t.rec.Ops))
		for _, op := range t.rec.Ops {
			if _, held := a.idx.Docs[op.Key]; held || !op.Delete {
				keys = append(keys, op.Key)
			}
		}
		if err := s.checkTargets(app, a, keys); err != nil {
			_ = s.persistTx(t)
			return err
		}
		if s.overQuota(app, a, s.projected(a, t, nil)) {
			_ = s.persistTx(t)
			return ErrQuota
		}

		j := journal{Tx: tx, Staging: stagingDir(tx), At: s.now().UTC(), Ops: make([]journalOp, 0, len(t.rec.Ops))}
		for _, op := range t.rec.Ops {
			k, _ := ParseKey(op.Key)
			var etag *string
			if !op.Delete {
				jo := journalOp{Key: op.Key, From: stagingName + "/" + j.Staging + "/" + op.File, Etag: op.Etag, Bytes: op.Bytes}
				j.Ops = append(j.Ops, jo)
				etag = &jo.Etag
			} else if _, ok := a.idx.Docs[op.Key]; ok {
				// A delete of a document that is not there touches nothing.
				j.Ops = append(j.Ops, journalOp{Key: op.Key, Delete: true})
			}
			out.Docs = append(out.Docs, DocEtag{Key: k, Etag: cloneEtag(etag)})
		}
		if err := s.commitJournal(app, a, &j, t); err != nil {
			return err
		}
		out.Revision = cloneEtag(a.idx.Revision)
		return nil
	})
	if err != nil {
		return Committed{}, err
	}
	return out, nil
}

// checkTargets refuses, with ErrBadPath, a commit whose changes the disk
// would not take, so that a journal is never written that cannot be applied
// (step 1 above). Every folder above a document must be missing or a plain
// folder: not a file, nor a link, junction or other reparse point, through
// which a commit would write or delete outside the app's folder. The
// document itself must be missing or a regular file that the index holds: a
// file there that is no document (a folder, a link, or a name that differs
// only in case on a file system that ignores case, such as "Notes.json" for
// "notes.json") belongs to someone else.
func (s *Store) checkTargets(app string, a *appState, keys []string) error {
	for _, ks := range keys {
		fi, err := s.checkPath(app, ks)
		if err != nil {
			return err
		}
		if fi == nil {
			continue
		}
		if _, held := a.idx.Docs[ks]; !held || !fi.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is taken on disk by something that is no document", ErrBadPath, ks)
		}
	}
	return nil
}

// errObstacle: a folder above a document is not a plain folder.
var errObstacle = errors.New("store: a folder on the path is not a plain folder")

// checkPath checks the folders above the document ks, from the app's folder
// down, and returns the information of the document's file, nil if it is
// missing. A folder that is a file, a link, a junction or another reparse
// point is ErrBadPath; one that is missing ends the check, since nothing
// below it exists.
func (s *Store) checkPath(app, ks string) (os.FileInfo, error) {
	dir := s.dir(app)
	parts := strings.Split(ks, "/")
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode().Type() != fs.ModeDir {
			return nil, fmt.Errorf("%w: %w: %s", ErrBadPath, errObstacle, ks)
		}
	}
	fi, err := os.Lstat(filepath.Join(dir, parts[len(parts)-1]))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return fi, err
}

// commitJournal writes j, the commit point, and applies it. t, if not nil,
// is the transaction j commits, which ends at the commit point whatever
// happens next.
func (s *Store) commitJournal(app string, a *appState, j *journal, t *txState) error {
	data, err := json.MarshalIndent(j, "", "\t")
	if err != nil {
		return err
	}
	path := filepath.Join(s.wss(app), journalName)
	if err := writeAtomic(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	// The commit point: from here on the journal is applied, now or later.
	a.journal = true
	if t != nil {
		t.kill()
		delete(a.txs, t.rec.Tx)
	}
	// On Windows the rename into place is not flushed with the folder (there
	// is no folder sync): flushing the journal's own file makes the commit
	// point survive a power loss before the device hears of it.
	if err := flushFile(path); err != nil {
		s.logf("app %q: flush %s: %v", app, journalName, err)
	}
	if err := s.apply(app, a, j); err != nil {
		return fmt.Errorf("commit %s: %w", j.Tx, err)
	}
	return nil
}

// flushFile syncs the file at path through a handle of its own; a test
// replaces it.
var flushFile = func(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// replay applies the journal that a commit left. Changes it had to drop
// are logged, and the app goes on.
func (s *Store) replay(app string, a *appState) error {
	path := filepath.Join(s.wss(app), journalName)
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		a.journal = false
		return nil
	}
	if err != nil {
		return err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if j.Staging != "" && (!filepath.IsLocal(j.Staging) || strings.ContainsAny(j.Staging, `/\`)) {
		return fmt.Errorf("%s: invalid staging folder %q", path, j.Staging)
	}
	for _, op := range j.Ops {
		if _, ok := ParseKey(op.Key); !ok {
			return fmt.Errorf("%s: invalid key %q", path, op.Key)
		}
		if !op.Delete && !filepath.IsLocal(filepath.FromSlash(op.From)) {
			return fmt.Errorf("%s: invalid body %q", path, op.From)
		}
	}
	err = s.apply(app, a, &j)
	var dropped *droppedError
	if errors.As(err, &dropped) {
		return nil // logged; the app goes on
	}
	if err != nil {
		return fmt.Errorf("apply %s: %w", path, err)
	}
	return nil
}

// droppedError reports the changes of a commit that could not be applied
// and were dropped. The rest of the commit is in place.
type droppedError struct {
	keys []string
	errs []error
}

func (e *droppedError) Error() string {
	return fmt.Sprintf("dropped %s: %v", strings.Join(e.keys, ", "), errors.Join(e.errs...))
}

func (e *droppedError) Unwrap() []error { return e.errs }

// apply carries out the steps after the commit point (see the top of this
// file). Each can run again after a crash: a body no longer in staging was
// moved already (the target is hashed again then), and a document already
// gone was deleted already. It returns a *droppedError if it had to drop
// changes, and any other error when the journal stays for later.
func (s *Store) apply(app string, a *appState, j *journal) error {
	root, wss := s.dir(app), s.wss(app)
	var dropped droppedError
	// keep reports whether err, a change's failure, keeps the journal for
	// later; otherwise the change is dropped.
	keep := func(ks string, err error) bool {
		if isTransient(err) && s.now().Sub(s.failingSince(a)) < journalGiveUp {
			return true
		}
		dropped.keys, dropped.errs = append(dropped.keys, ks), append(dropped.errs, err)
		return false
	}
	applied := make([]bool, len(j.Ops))
	rehash := make([]bool, len(j.Ops))
	for i, op := range j.Ops {
		target := s.docPath(app, op.Key)
		fi, err := s.checkPath(app, op.Key)
		if err != nil {
			if keep(op.Key, err) {
				return err
			}
			continue
		}
		if op.Delete {
			if fi != nil && fi.Mode().IsRegular() {
				err := retryBusy(func() error { return removeFile(target) })
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					if keep(op.Key, err) {
						return err
					}
					continue
				}
			}
			applied[i] = true
			removeEmptyDirs(filepath.Dir(target), root)
			continue
		}
		from := filepath.Join(wss, filepath.FromSlash(op.From))
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			applied[i], rehash[i] = true, true // moved before a crash
			continue
		}
		if fi != nil && !fi.Mode().IsRegular() {
			err := fmt.Errorf("%w: %s is taken on disk by something that is no document", ErrBadPath, op.Key)
			if keep(op.Key, err) {
				return err
			}
			continue
		}
		err = os.MkdirAll(filepath.Dir(target), 0o700)
		if err == nil {
			err = retryBusy(func() error { return renameFile(from, target) })
		}
		if err != nil {
			if keep(op.Key, err) {
				return err
			}
			continue
		}
		applied[i] = true
	}
	for i, op := range j.Ops {
		if !applied[i] {
			continue
		}
		if op.Delete {
			a.idx.remove(op.Key, j.At)
			continue
		}
		path := s.docPath(app, op.Key)
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue // gone again: the reconcile below or the next settles it
		}
		e := docEntry{Etag: op.Etag, Bytes: op.Bytes, Mtime: fi.ModTime().UnixNano()}
		if rehash[i] || fi.Size() != op.Bytes {
			// Its body was moved before a crash: what lies there now is the
			// truth, not what the journal expected.
			etag, size, mtime, err := hashFile(path)
			if err != nil {
				continue
			}
			e = docEntry{Etag: etag, Bytes: size, Mtime: mtime}
		}
		a.idx.put(op.Key, e)
	}
	a.idx.finish(s.now())
	if dropped.keys != nil {
		s.logf("app %q: commit %s: %v; the rest is in place", app, j.Tx, &dropped)
		if _, err := s.reconcile(app, a); err != nil {
			s.logf("app %q: reconcile after a dropped change: %v", app, err)
		}
	}
	if err := s.saveOrMark(app, a); err != nil {
		s.logf("app %q: save the index (a reconcile rebuilds it): %v", app, err)
	}
	if j.Staging != "" {
		dir := filepath.Join(wss, stagingName, j.Staging)
		if err := retryBusy(func() error { return removeAll(dir) }); err != nil {
			if isTransient(err) && s.now().Sub(s.failingSince(a)) < journalGiveUp {
				return err
			}
			s.logf("app %q: remove %s: %v; sweep tries again", app, dir, err)
		}
	}
	if err := retryBusy(func() error { return removeFile(filepath.Join(wss, journalName)) }); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // the journal must go, or it would be applied again later
	}
	a.journal, a.failing = false, time.Time{}
	if dropped.keys != nil {
		return &dropped
	}
	return nil
}
