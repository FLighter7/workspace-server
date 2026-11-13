package fsutil

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRenameReplaces(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeFile(t, a, []byte("one"))
	if err := Rename(a, b); err != nil {
		t.Fatalf("rename into a free name: %v", err)
	}
	writeFile(t, a, []byte("two"))
	if err := Rename(a, b); err != nil {
		t.Fatalf("rename over a file: %v", err)
	}
	if got, _ := os.ReadFile(b); string(got) != "two" {
		t.Errorf("content = %q, want two", got)
	}
	if got := entries(t, dir); len(got) != 1 || got[0] != "b" {
		t.Errorf("directory holds %v, want only b", got)
	}
	if err := Rename(a, b); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rename of a missing file: %v, want not exist", err)
	}
}

func TestReadFileShared(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "doc")
	want := bytes.Repeat([]byte("0123456789"), 100_000)
	writeFile(t, name, want)
	got, err := ReadFileShared(name)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, %v; want %d bytes", len(got), err, len(want))
	}

	empty := filepath.Join(dir, "empty")
	writeFile(t, empty, nil)
	if got, err := ReadFileShared(empty); err != nil || len(got) != 0 {
		t.Errorf("empty file: %q, %v", got, err)
	}
	if _, err := ReadFileShared(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v, want not exist", err)
	}
}

func TestOpenSharedSeeksAndStats(t *testing.T) {
	name := filepath.Join(t.TempDir(), "doc")
	writeFile(t, name, []byte("0123456789"))
	f, err := OpenShared(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != 10 {
		t.Errorf("stat: %v, %v", fi, err)
	}
	if _, err := f.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(f); err != nil || string(rest) != "6789" {
		t.Errorf("after seek: %q, %v", rest, err)
	}
}

// A document's key may be 255 characters on its own, so its full path can
// pass Windows' MAX_PATH (260).
func TestLongPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("f", 100), strings.Repeat("g", 100))
	a := filepath.Join(dir, strings.Repeat("a", 120)+".json")
	b := filepath.Join(dir, strings.Repeat("b", 120)+".json")
	if len(a) < 300 {
		t.Fatalf("path is only %d bytes", len(a))
	}
	if err := WriteFileAtomic(a, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(b, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenShared(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFileShared(b); err != nil || string(got) != "one" {
		t.Errorf("read %q, %v; want one", got, err)
	}
}
