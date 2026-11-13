//go:build !windows

package hub

import (
	"os"
	"testing"
)

// holdUnshared makes the file at path unreadable to the server until
// release, as Windows' unshared handle of another program does there.
func holdUnshared(t *testing.T, path string) (release func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = os.Chmod(path, 0o600)
		}
	}
	t.Cleanup(release)
	return release
}
