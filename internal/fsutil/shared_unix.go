//go:build !windows

package fsutil

import "os"

// OpenShared opens the file name for reading. It is os.Open here: a file
// open for reading can be renamed over or removed anyway, and the handle
// keeps reading the old file. On Windows it is not, and every read of a
// document goes through OpenShared (see shared_windows.go).
func OpenShared(name string) (*os.File, error) { return os.Open(name) }

// Rename renames oldpath to newpath, replacing newpath atomically if it
// exists, while handles open on the old newpath keep reading the old file.
// It is os.Rename here; Windows needs more (see shared_windows.go).
func Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
