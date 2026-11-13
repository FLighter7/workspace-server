package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/fsutil"
)

// The file operations of the store, in package variables so that a test can
// make any of them fail: a commit interrupted at each of its steps, a disk
// that refuses a write, a file another program holds open.
var (
	writeAtomic = fsutil.WriteFileAtomic
	renameFile  = fsutil.Rename
	removeFile  = os.Remove
	removeAll   = os.RemoveAll
	readFile    = os.ReadFile // the server's own files only, never a document
	hashFile    = hashShared

	// racyWindow is how close to its mtime a file may be hashed and still
	// be trusted by size and mtime afterwards: more than the coarsest mtime
	// tick of a common file system (FAT's 2 s).
	racyWindow = 3 * time.Second

	// busyRetries and busyDelay are how often and how far apart a rename or
	// a removal that another program's handle blocks is tried again.
	busyRetries = 10
	busyDelay   = 50 * time.Millisecond
)

// hashShared returns the etag and the size of the file at path, read
// through fsutil.OpenShared, and its modification time, taken before the
// read: if the file changes during the read, the next Reconcile sees a newer
// time and hashes it again.
func hashShared(path string) (etag string, size int64, mtime int64, err error) {
	f, err := fsutil.OpenShared(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, fi.ModTime().UnixNano(), nil
}

// errTransient marks a failure that may pass, as isBusy does for the
// system's own; the tests wrap it to stand in for a crash in the middle of
// a commit, which a later operation carries through.
var errTransient = errors.New("store: transient failure")

// isTransient reports whether err may pass if tried again later: another
// program's handle on the file (isBusy), or errTransient.
func isTransient(err error) bool { return isBusy(err) || errors.Is(err, errTransient) }

// retryBusy runs op, and again while it fails because another handle has the
// file open (isBusy), up to busyRetries more times.
func retryBusy(op func() error) error {
	for i := 0; ; i++ {
		err := op()
		if err == nil || !isBusy(err) || i >= busyRetries {
			return err
		}
		time.Sleep(busyDelay)
	}
}

// removeEmptyDirs removes dir and then each parent that is left empty, up to
// but not including root. It stops at the first that will not go: not empty,
// or held.
func removeEmptyDirs(dir, root string) {
	for {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return
		}
		// Never a link or a junction, which os.Remove would remove too.
		if fi, err := os.Lstat(dir); err != nil || fi.Mode().Type() != fs.ModeDir {
			return
		}
		if err := removeFile(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// reconcile compares the index of app with the files under its folder (see
// the package comment), saves the index if anything in it changed, and
// reports whether the revision changed. A file whose path is not a valid key,
// and anything that is not a regular file, is left out and left alone, and so
// is the .wss folder. A file that vanishes during the walk counts as removed.
func (s *Store) reconcile(app string, a *appState) (bool, error) {
	root := s.dir(app)
	seen := make(map[string]bool, len(a.idx.Docs))
	dirty := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // the folder or an entry vanished meanwhile
			}
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// No key can lie below a folder that is not a valid one,
			// .wss included, nor below anything but a plain folder.
			if !ValidFolder(rel) || d.Type() != fs.ModeDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		k, ok := ParseKey(rel)
		if !ok {
			return nil
		}
		ks := k.String()
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		e, have := a.idx.Docs[ks]
		if have && !e.Racy && e.Bytes == fi.Size() && e.Mtime == fi.ModTime().UnixNano() {
			seen[ks] = true
			return nil
		}
		etag, size, mtime, err := hashFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		seen[ks] = true
		// A file hashed within racyWindow of its mtime may change again
		// within the same tick of the clock, keeping its size and mtime:
		// it is hashed again at the next reconcile, until it is older.
		age := time.Since(time.Unix(0, mtime))
		next := docEntry{Etag: etag, Bytes: size, Mtime: mtime, Racy: age < racyWindow && age > -racyWindow}
		if !have || e != next {
			a.idx.put(ks, next)
			dirty = true
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	now := s.now()
	for ks := range a.idx.Docs {
		if !seen[ks] {
			a.idx.remove(ks, now)
			dirty = true
		}
	}
	if !dirty {
		return false, nil
	}
	changed := a.idx.finish(now)
	if !a.onDisk && a.idx.empty() {
		return changed, nil
	}
	return changed, s.saveOrMark(app, a)
}

// saveOrMark saves the index, and when that fails marks it unsaved, so that
// the next operation on the app tries again.
func (s *Store) saveOrMark(app string, a *appState) error {
	if err := s.save(app, a); err != nil {
		a.unsaved = true
		return err
	}
	a.unsaved = false
	return nil
}
