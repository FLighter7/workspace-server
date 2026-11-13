package fsutil

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// These pin down what the files stage 0 spike found on Windows (NTFS): what
// a commit can do to a document while a get is still streaming it.

func randomBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// openPartlyRead opens name through OpenShared and reads n bytes of it, as a
// get that is part way through.
func openPartlyRead(t *testing.T, name string, n int) (*os.File, []byte) {
	t.Helper()
	f, err := OpenShared(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	head := make([]byte, n)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatal(err)
	}
	return f, head
}

func readRest(t *testing.T, f *os.File, head []byte) []byte {
	t.Helper()
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reader after the change: %v", err)
	}
	return append(head, rest...)
}

func TestRenameOverSharedReader(t *testing.T) {
	dir := t.TempDir()
	live, staged := filepath.Join(dir, "live"), filepath.Join(dir, "staged")
	oldBody, newBody := randomBody(t, 2<<20), randomBody(t, 1<<20)
	writeFile(t, live, oldBody)
	writeFile(t, staged, newBody)

	r, head := openPartlyRead(t, live, 512<<10)
	if err := Rename(staged, live); err != nil {
		t.Fatalf("rename over a file open through OpenShared: %v", err)
	}
	if got, err := ReadFileShared(live); err != nil || !bytes.Equal(got, newBody) {
		t.Errorf("a fresh read got %d bytes, %v; want the new body", len(got), err)
	}
	if got := readRest(t, r, head); !bytes.Equal(got, oldBody) {
		t.Errorf("the open reader got %d bytes, not the old body", len(got))
	}
	if got := entries(t, dir); len(got) != 1 || got[0] != "live" {
		t.Errorf("directory holds %v, want only live", got)
	}
}

func TestWriteFileAtomicOverSharedReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	writeFile(t, path, []byte("old"))
	r, head := openPartlyRead(t, path, 1)
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("atomic write over a file open through OpenShared: %v", err)
	}
	if got, _ := ReadFileShared(path); string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
	if got := readRest(t, r, head); string(got) != "old" {
		t.Errorf("the open reader got %q, want old", got)
	}
}

func TestRemoveUnderSharedReader(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live")
	oldBody := randomBody(t, 1<<20)
	writeFile(t, live, oldBody)

	r, head := openPartlyRead(t, live, 256<<10)
	if err := os.Remove(live); err != nil {
		t.Fatalf("remove of a file open through OpenShared: %v", err)
	}
	if _, err := os.Stat(live); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the name is still there after the remove: %v", err)
	}
	f, err := os.OpenFile(live, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("a new file at the removed name: %v", err)
	}
	_, _ = f.Write([]byte("new"))
	_ = f.Close()
	if got := readRest(t, r, head); !bytes.Equal(got, oldBody) {
		t.Errorf("the open reader got %d bytes, not the old body", len(got))
	}
	if got, _ := ReadFileShared(live); string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
}

// One plain os.Open on a document (it shares no delete) blocks the commit
// that would replace it, which is why every read goes through OpenShared.
func TestPlainOpenBlocksRename(t *testing.T) {
	dir := t.TempDir()
	live, staged := filepath.Join(dir, "live"), filepath.Join(dir, "staged")
	writeFile(t, live, []byte("old"))
	writeFile(t, staged, []byte("new"))

	f, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	err = Rename(staged, live)
	_ = f.Close()
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("rename over a file open by os.Open: %v, want a sharing violation", err)
	}
	if got, _ := os.ReadFile(live); string(got) != "old" {
		t.Errorf("content = %q, want old: a failed rename changed the file", got)
	}
	if err := Rename(staged, live); err != nil {
		t.Errorf("rename once the reader closed: %v", err)
	}
}

func TestPosixUnsupported(t *testing.T) {
	wrap := func(errno windows.Errno) error {
		return &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errno}
	}
	for _, errno := range []windows.Errno{
		windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION,
	} {
		if !posixUnsupported(wrap(errno)) {
			t.Errorf("%v does not fall back", errno)
		}
	}
	for _, err := range []error{
		nil, wrap(windows.ERROR_ACCESS_DENIED), wrap(windows.ERROR_SHARING_VIOLATION),
		wrap(windows.ERROR_FILE_NOT_FOUND), wrap(windows.ERROR_NOT_SAME_DEVICE),
	} {
		if posixUnsupported(err) {
			t.Errorf("%v falls back to os.Rename", err)
		}
	}
}
