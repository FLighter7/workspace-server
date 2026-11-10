//go:build windows

package program

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// scratchKey is a key of the test's own, outside the Uninstall list, so that
// no test run adds a line to Settings → Apps.
func scratchKey(t *testing.T) string {
	t.Helper()
	key := fmt.Sprintf(`Software\workspace-server-test-%d`, os.Getpid())
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) })
	return key
}

func TestWriteEntryWritesTheValues(t *testing.T) {
	key := scratchKey(t)
	exe := `C:\Users\a b\Downloads\workspace-server.exe`
	if err := writeEntry(key, exe, "v1.2.3"); err != nil {
		t.Fatalf("writeEntry = %v", err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	for _, v := range Entry(exe, "v1.2.3") {
		if v.DWord != nil {
			got, typ, err := k.GetIntegerValue(v.Name)
			if err != nil || typ != registry.DWORD || uint32(got) != *v.DWord {
				t.Errorf("%s = %d (type %d, %v), want DWORD %d", v.Name, got, typ, err, *v.DWord)
			}
			continue
		}
		got, typ, err := k.GetStringValue(v.Name)
		if err != nil || typ != registry.SZ || got != v.String {
			t.Errorf("%s = %q (type %d, %v), want SZ %q", v.Name, got, typ, err, v.String)
		}
	}
}

func TestWriteEntryReplacesTheOldValues(t *testing.T) {
	key := scratchKey(t)
	for _, version := range []string{"v1", "v2"} {
		if err := writeEntry(key, `C:\ws.exe`, version); err != nil {
			t.Fatal(err)
		}
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if got, _, _ := k.GetStringValue("DisplayVersion"); got != "v2" {
		t.Errorf("DisplayVersion = %q, want v2", got)
	}
}

func TestDeleteEntry(t *testing.T) {
	key := scratchKey(t)
	if err := deleteEntry(key); err != nil {
		t.Errorf("deleting a missing key = %v, want nil", err)
	}
	if err := writeEntry(key, `C:\ws.exe`, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := deleteEntry(key); err != nil {
		t.Fatalf("deleteEntry = %v", err)
	}
	if _, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE); !errors.Is(err, registry.ErrNotExist) {
		t.Errorf("the key is still there: %v", err)
	}
}

// Remove hands the file to a hidden PowerShell that deletes it a moment
// later. The names here hold everything the helper's single-quoted string has
// to survive: spaces, quotes (a straight and a typographic one), $ and a
// backtick.
func TestRemoveDeletesAFileWithAHostileName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b'c $x `y\u2019z")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "w's $x `y\u2019z.exe")
	bystander := filepath.Join(dir, "bystander.txt")
	for _, p := range []string{path, bystander} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper did not delete %s in 15 s (%v)", path, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file next to the program is gone: %v", err)
	}
}

// The helper waits for a file that is held open, as the running program's own
// file is, and deletes it when the holder lets go.
func TestRemoveWaitsForAnOpenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		f.Close()
		t.Fatalf("Remove = %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Lstat(path); err != nil {
		f.Close()
		t.Fatalf("the file went while it was open: %v", err)
	}
	f.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper did not delete the file after it was closed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
