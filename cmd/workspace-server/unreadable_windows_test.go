//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// makeUnreadable makes the file at path fail to open for reading until the
// test ends. Windows ignores a file's mode bits for this, so the test holds
// the file open with no sharing instead: every other open fails with a
// sharing violation.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
}
