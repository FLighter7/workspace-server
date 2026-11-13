// Package fsutil holds file helpers shared by the packages that keep the
// server's files: atomic writes, and reads and renames that do not get in
// each other's way on Windows.
package fsutil

import (
	"bytes"
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
// slip in between the write and the stat. The rename is Rename, so it
// replaces a file that a reader holds open through OpenShared.
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
	if err := Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	// The file is in place: a failed directory sync must not make the
	// caller undo anything.
	_ = syncDir(dir)
	return fi, nil
}

// ReadFileShared is os.ReadFile through OpenShared: the read does not stop a
// Rename over the file or its removal (see OpenShared).
func ReadFileShared(name string) ([]byte, error) {
	f, err := OpenShared(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 && int64(int(fi.Size())) == fi.Size() {
		buf.Grow(int(fi.Size()) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
