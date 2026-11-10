//go:build !windows

package program

import "os"

// Register does nothing: only Windows has a list of installed programs.
func Register(exe, version string) error { return nil }

// Unregister does nothing: only Windows has a list of installed programs.
func Unregister() error { return nil }

// Remove deletes the program file path, which must be a regular file; it
// never deletes a folder or follows a symbolic link. Unix lets a running
// program unlink its own file.
func Remove(path string) error {
	if err := checkRegular(path); err != nil {
		return err
	}
	return os.Remove(path)
}
