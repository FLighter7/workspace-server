//go:build !windows

package main

import (
	"os"
	"testing"
)

// makeUnreadable makes the file at path fail to open for reading until the
// test ends.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root reads every file")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}
