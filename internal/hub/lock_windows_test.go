package hub

import (
	"syscall"
	"testing"
)

// holdUnshared opens the file at path as another program can: with no
// sharing at all, so that no one else opens it until release.
func holdUnshared(t *testing.T, path string) (release func()) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = syscall.CloseHandle(h)
		}
	}
	t.Cleanup(release)
	return release
}
