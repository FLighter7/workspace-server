package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/FLighter7/workspace-server/internal/config"
)

// installedServer makes def the default server folder, with its data in data
// when data is not empty, and makes this a build of version v. Both are
// restored after the test.
func installedServer(t *testing.T, def, data, v string) {
	t.Helper()
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	if data != "" {
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, def, config.Config{Name: "x", UDPPort: 1, HTTPPort: 2, DataDir: data})
	}
	savedDir, savedVersion := defaultServerDir, version
	defaultServerDir = func() (string, error) { return def, nil }
	version = v
	t.Cleanup(func() { defaultServerDir, version = savedDir, savedVersion })
}

func TestCheckDevDirs(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "installed")
	data := filepath.Join(root, "data")
	installedServer(t, def, data, devVersion)
	other := filepath.Join(root, "tmp", "dev")

	refused := []string{def, filepath.Join(def, "apps"), data, filepath.Join(data, "apps", "todo")}
	if runtime.GOOS == "windows" {
		refused = append(refused, strings.ToUpper(def), strings.ToUpper(data))
	}
	for _, dir := range refused {
		var e devBuildError
		if err := checkDevDirs(devVersion, def, other, dir); !errors.As(err, &e) || e.dir != dir {
			t.Errorf("checkDevDirs(dev, %s) = %v, want a refusal naming it", dir, err)
		}
		if err := checkDevDirs("1.0.0", def, dir); err != nil {
			t.Errorf("a release build refuses %s: %v", dir, err)
		}
	}
	for _, dir := range []string{other, root, filepath.Join(root, "installed-2"), ""} {
		if err := checkDevDirs(devVersion, def, dir); err != nil {
			t.Errorf("checkDevDirs(dev, %q) = %v, want nil", dir, err)
		}
	}
	if err := checkDevDirs(devVersion, "", def); err != nil {
		t.Errorf("no default folder: %v, want nil", err)
	}
}

// A development build refuses the default server folder before anything is
// created, locked or logged in it.
func TestDevBuildDoesNotRunOnTheDefaultFolder(t *testing.T) {
	def := filepath.Join(t.TempDir(), "installed")
	installedServer(t, def, "", devVersion)
	err := runServer(context.Background(), serverOptions{Dir: def}, nil)
	var e devBuildError
	if !errors.As(err, &e) {
		t.Fatalf("runServer = %v, want a development build's refusal", err)
	}
	entries, _ := os.ReadDir(def)
	if len(entries) != 0 {
		t.Errorf("the default folder holds %v after the refusal", entries)
	}
}

// Nor a side folder whose config names the installed server's data folder.
func TestDevBuildDoesNotRunOnTheInstalledData(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	installedServer(t, filepath.Join(root, "installed"), data, devVersion)
	side := filepath.Join(root, "side")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, side, config.Config{Name: "x", UDPPort: 1, HTTPPort: 2, DataDir: data})
	err := runServer(context.Background(), serverOptions{Dir: side}, nil)
	if e := (devBuildError{}); !errors.As(err, &e) || e.dir != data {
		t.Fatalf("runServer = %v, want a refusal naming %s", err, data)
	}
	if _, err := os.Stat(filepath.Join(side, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the side folder was locked: %v", err)
	}
}

// The page of a development build cannot move its data onto the installed
// server's.
func TestDevBuildDoesNotMoveOntoTheInstalledData(t *testing.T) {
	root := t.TempDir()
	def, data := filepath.Join(root, "installed"), filepath.Join(root, "data")
	installedServer(t, def, data, devVersion)
	putData(t, data, "todo", `{"a":1}`, 0)
	cur := filepath.Join(root, "dev")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{data, def} {
		c, err := checkDataDir(cur, target)
		if err != nil {
			t.Fatal(err)
		}
		if c.Verdict != verdictUnavailable || !strings.Contains(c.Message, "development build") {
			t.Errorf("checkDataDir(%s) = %s %q, want a development build's refusal", target, c.Verdict, c.Message)
		}
	}
	version = "1.0.0"
	if c, _ := checkDataDir(cur, data); c.Verdict != verdictAdopt {
		t.Errorf("a release build: verdict %s, want %s", c.Verdict, verdictAdopt)
	}
}

func TestDevBuildDoesNotStart(t *testing.T) {
	f := newFakeDeps(t)
	f.version = devVersion
	f.running, f.registered = true, true
	if code := f.run("start"); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if got := f.got(); len(got) != 0 {
		t.Errorf("calls %v, want none: the installed server keeps running", got)
	}
	if !strings.Contains(f.errOut.String(), "development build") {
		t.Errorf("stderr %q", f.errOut.String())
	}
}

func TestDevBuildDoesNotDeleteTheInstalledServer(t *testing.T) {
	f := newFakeDeps(t)
	f.version = devVersion
	data := filepath.Join(t.TempDir(), "data")
	ourFolder(t, f, data)
	if code := f.run("delete", "--yes"); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if got := f.got(); len(got) != 0 {
		t.Errorf("calls %v, want none", got)
	}
	// Nor through a side folder that names its data.
	side := filepath.Join(t.TempDir(), "side")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, side, config.Config{Name: "x", UDPPort: 1, HTTPPort: 2, DataDir: data})
	if code := f.run("delete", "--yes", "-dir", side); code != 1 {
		t.Fatalf("side folder: exit code %d, want 1", code)
	}
	if got := f.got(); len(got) != 0 {
		t.Errorf("calls %v, want none", got)
	}
}

func TestDevBuildDeletesASideServerAndStops(t *testing.T) {
	f := newFakeDeps(t)
	f.version = devVersion
	side := filepath.Join(t.TempDir(), "side")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(side, "identity.json"), "{}")
	if code := f.run("delete", "--yes", "-dir", side); code != 0 {
		t.Fatalf("delete: exit code %d, stderr %q", code, f.errOut.String())
	}
	f.running, f.registered = true, true
	if code := f.run("stop"); code != 0 {
		t.Fatalf("stop: exit code %d, stderr %q", code, f.errOut.String())
	}
}
