//go:build windows

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestDetachedHelper is the program TestStartDetachedHasNoConsole starts: it
// reports what it finds about its console and exits. Run by itself it does
// nothing.
func TestDetachedHelper(t *testing.T) {
	out := os.Getenv("WS_SPAWN_OUT")
	if out == "" {
		t.Skip("started by TestStartDetachedHasNoConsole")
	}
	window, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	wd, _ := os.Getwd()
	report := fmt.Sprintf("window=%d\nwd=%s\n", window, wd)
	// Written under another name and renamed, so that the parent never reads
	// half of it.
	if err := os.WriteFile(out+".tmp", []byte(report), 0o600); err == nil {
		_ = os.Rename(out+".tmp", out)
	}
}

func TestStartDetachedHasNoConsole(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.txt")
	t.Setenv("WS_SPAWN_OUT", out)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := startDetached(exe, "-test.run=^TestDetachedHelper$"); err != nil {
		t.Fatalf("startDetached = %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if data, err := os.ReadFile(out); err == nil {
			sysDir, _ := windows.GetSystemDirectory()
			want := fmt.Sprintf("window=0\nwd=%s\n", sysDir)
			if string(data) != want {
				t.Errorf("the detached process reports\n%s\nwant\n%s", data, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached process wrote no report in 15 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStartDetachedReportsAMissingProgram(t *testing.T) {
	err := startDetached(filepath.Join(t.TempDir(), "missing.exe"), "run")
	if err == nil {
		t.Fatal("startDetached = nil for a program that does not exist")
	}
}
