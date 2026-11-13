package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// A commit runs under the app's lock:
//
//  1. check: every body complete, every ifEtag matching the index after a
//     reconcile, the quota;
//  2. write .wss/journal.json and sync it: the commit point. Before it no
//     file of the set has been touched, so a failure is a rollback by doing
//     nothing; the transaction stays open;
//  3. apply the journal: rename each body into place (fsutil.Rename, which
//     replaces a document that a get still reads through OpenShared),
//     remove each deleted document and the folders that leaves empty;
//  4. write the index;
//  5. remove the transaction's staging folder, then the journal.
//
// A failure after the commit point leaves the journal, and the next
// operation on the app, or the next start, applies it again from the top:
// every step can run twice. A rename or a removal that another program's
// handle blocks is tried again for a moment (retryBusy), then fails.

// journal is .wss/journal.json: the changes of one commit, ready to apply.
type journal struct {
	Tx  string      `json:"tx"`
	At  time.Time   `json:"at"`
	Ops []journalOp `json:"ops"`
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
// one staged is a *ConflictError, and a set that would exceed the app's
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
		if s.projected(a, t, nil) > s.quotaOf(app) {
			_ = s.persistTx(t)
			return ErrQuota
		}

		j := journal{Tx: tx, At: s.now().UTC(), Ops: make([]journalOp, 0, len(t.rec.Ops))}
		for _, op := range t.rec.Ops {
			k, _ := ParseKey(op.Key)
			jo := journalOp{Key: op.Key, Delete: op.Delete}
			var etag *string
			if !op.Delete {
				jo.From = stagingName + "/" + tx + "/" + op.File
				jo.Etag, jo.Bytes = op.Etag, op.Bytes
				etag = &jo.Etag
			}
			j.Ops = append(j.Ops, jo)
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

// commitJournal writes j, the commit point, and applies it. t, if not nil,
// is the transaction j commits, which ends at the commit point whatever
// happens next.
func (s *Store) commitJournal(app string, a *appState, j *journal, t *txState) error {
	data, err := json.MarshalIndent(j, "", "\t")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.wss(app), journalName), append(data, '\n'), 0o600); err != nil {
		return err
	}
	// The commit point: from here on the journal is applied, now or later.
	a.journal = true
	if t != nil {
		t.kill()
		delete(a.txs, t.rec.Tx)
	}
	if err := s.apply(app, a, j); err != nil {
		return fmt.Errorf("commit %s, applied later: %w", j.Tx, err)
	}
	a.journal = false
	return nil
}

// replay applies the journal that a commit left.
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
	for _, op := range j.Ops {
		if _, ok := ParseKey(op.Key); !ok {
			return fmt.Errorf("%s: invalid key %q", path, op.Key)
		}
		if !op.Delete && !filepath.IsLocal(filepath.FromSlash(op.From)) {
			return fmt.Errorf("%s: invalid body %q", path, op.From)
		}
	}
	if err := s.apply(app, a, &j); err != nil {
		return fmt.Errorf("apply %s: %w", path, err)
	}
	a.journal = false
	return nil
}

// apply carries out the steps after the commit point (see the top of this
// file). Each can run again after a crash: a body no longer in staging was
// moved already, and a document already gone was deleted already.
func (s *Store) apply(app string, a *appState, j *journal) error {
	root, wss := s.dir(app), s.wss(app)
	for _, op := range j.Ops {
		target := s.docPath(app, op.Key)
		if op.Delete {
			err := retryBusy(func() error { return removeFile(target) })
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			removeEmptyDirs(filepath.Dir(target), root)
			continue
		}
		from := filepath.Join(wss, filepath.FromSlash(op.From))
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue // moved before a crash
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := retryBusy(func() error { return renameFile(from, target) }); err != nil {
			return err
		}
	}
	for _, op := range j.Ops {
		if op.Delete {
			a.idx.remove(op.Key, j.At)
			continue
		}
		fi, err := os.Lstat(s.docPath(app, op.Key))
		if err != nil || !fi.Mode().IsRegular() {
			continue // gone again by hand: the next reconcile settles it
		}
		a.idx.put(op.Key, docEntry{Etag: op.Etag, Bytes: op.Bytes, Mtime: fi.ModTime().UnixNano()})
	}
	a.idx.finish(s.now())
	if err := s.saveOrMark(app, a); err != nil {
		return err
	}
	if ValidTx(j.Tx) {
		dir := filepath.Join(wss, stagingName, j.Tx)
		if err := retryBusy(func() error { return removeAll(dir) }); err != nil {
			return err
		}
	}
	return retryBusy(func() error { return removeFile(filepath.Join(wss, journalName)) })
}
