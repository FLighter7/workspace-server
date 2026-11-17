//go:build !windows

package fsutil

import "golang.org/x/sys/unix"

// FreeSpace returns the bytes a program without special rights can still
// write on the volume that holds dir.
func FreeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
