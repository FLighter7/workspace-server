//go:build !windows

package fsutil

import "os"

// SyncDir flushes dir's entries to disk, so that a file created or renamed
// in it survives a power loss. Some file systems (network mounts, FUSE)
// refuse it; callers treat a failure as best effort.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
