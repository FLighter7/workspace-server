// Package fsutil holds file helpers shared by the packages that keep the
// server's JSON files.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

// syncDir is SyncDir; a test replaces it.
var syncDir = SyncDir

// WriteFileAtomic writes data to a temporary file next to path, syncs it and
// renames it into place, so a reader never sees a partial file, and then
// syncs the directory, so that the rename survives a power loss (best
// effort: see SyncDir). The directory is created (mode 0700) if missing. The
// temporary file is removed on every failure path. perm is applied to the new
// file; a filesystem that refuses chmod (os.ErrPermission) is tolerated.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	_, err := WriteFileAtomicStat(path, data, perm)
	return err
}

// WriteFileAtomicStat is WriteFileAtomic that also returns the new file's
// information, read before the rename, which keeps it: no other writer can
// slip in between the write and the stat.
func WriteFileAtomicStat(path string, data []byte, perm os.FileMode) (os.FileInfo, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	fi, err := os.Stat(tmp.Name())
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	// The file is in place: a failed directory sync must not make the
	// caller undo anything.
	_ = syncDir(dir)
	return fi, nil
}
