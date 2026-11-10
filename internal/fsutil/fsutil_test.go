package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteAndReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "data.json")
	if err := WriteFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "one" {
		t.Errorf("content = %q, want one", got)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Errorf("content = %q, want two", got)
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 1 || got[0] != "data.json" {
		t.Errorf("directory holds %v, want only data.json", got)
	}
}

func TestAppliesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	for _, perm := range []os.FileMode{0o600, 0o644} {
		path := filepath.Join(t.TempDir(), "f")
		if err := WriteFileAtomic(path, []byte("x"), perm); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != perm {
			t.Errorf("mode = %v, want %v", fi.Mode().Perm(), perm)
		}
	}
}

func TestSyncsTheDirectory(t *testing.T) {
	var synced []string
	syncDir = func(dir string) error {
		synced = append(synced, dir)
		return errors.New("sync not supported here")
	}
	t.Cleanup(func() { syncDir = SyncDir })

	path := filepath.Join(t.TempDir(), "data.json")
	fi, err := WriteFileAtomicStat(path, []byte("one"), 0o600)
	if err != nil {
		t.Fatalf("a failed directory sync failed the write: %v", err)
	}
	if len(synced) != 1 || synced[0] != filepath.Dir(path) {
		t.Errorf("synced %v, want [%s]", synced, filepath.Dir(path))
	}
	if got, _ := os.ReadFile(path); string(got) != "one" {
		t.Errorf("content = %q, want one", got)
	}
	now, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != now.Size() || !fi.ModTime().Equal(now.ModTime()) {
		t.Errorf("returned size %d, time %v; the file has %d, %v", fi.Size(), fi.ModTime(), now.Size(), now.ModTime())
	}
}

func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir: %v", err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil && runtime.GOOS != "windows" {
		t.Error("SyncDir of a missing folder succeeded")
	}
}

func TestFailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the directory should be: MkdirAll fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filepath.Join(blocker, "data.json"), []byte("y"), 0o600); err == nil {
		t.Error("write under a regular file succeeded")
	}

	// The target is a directory: the final rename fails after the temp file
	// was written, which must still be cleaned up.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("y"), 0o600); err == nil {
		t.Error("replacing a non-empty directory succeeded")
	}
	if got := entries(t, dir); len(got) != 2 {
		t.Errorf("directory holds %v, want only blocker and target", got)
	}
}
