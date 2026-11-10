package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// putData writes an app's document and n history versions under dataDir.
func putData(t *testing.T, dataDir, id, doc string, versions int) {
	t.Helper()
	app := filepath.Join(dataDir, "apps", id)
	if err := os.MkdirAll(filepath.Join(app, "history"), 0o700); err != nil {
		t.Fatal(err)
	}
	if doc != "" {
		writeFile(t, filepath.Join(app, "data.json"), doc)
	}
	for i := range versions {
		writeFile(t, filepath.Join(app, "history", "2026010"+string(rune('1'+i))+"T000000Z-0000000"+string(rune('0'+i))+".json"), `{"v":1}`)
	}
}

func TestCheckDataDirVerdicts(t *testing.T) {
	type setup func(t *testing.T, current, target string) string // returns the path to check
	cases := []struct {
		name  string
		setup setup
		want  verdict
	}{
		{"same folder", func(t *testing.T, cur, _ string) string { return cur }, verdictSame},
		{"same folder, not clean", func(t *testing.T, cur, _ string) string { return cur + string(filepath.Separator) + "." }, verdictSame},
		{"missing, parent exists, no current data", func(t *testing.T, _, tgt string) string {
			return filepath.Join(tgt, "new")
		}, verdictCreate},
		{"missing, parent exists, current data", func(t *testing.T, cur, tgt string) string {
			putData(t, cur, "todo", `{"a":1}`, 1)
			return filepath.Join(tgt, "new")
		}, verdictCreate},
		{"missing parent", func(t *testing.T, _, tgt string) string {
			return filepath.Join(tgt, "gone", "new")
		}, verdictUnavailable},
		{"relative path", func(t *testing.T, _, _ string) string { return "rel/data" }, verdictUnavailable},
		{"a file, not a folder", func(t *testing.T, _, tgt string) string {
			p := filepath.Join(tgt, "file")
			writeFile(t, p, "x")
			return p
		}, verdictUnavailable},
		{"inside the current apps folder", func(t *testing.T, cur, _ string) string {
			putData(t, cur, "todo", `{"a":1}`, 0)
			return filepath.Join(cur, "apps", "todo")
		}, verdictUnavailable},
		{"empty target, current data", func(t *testing.T, cur, tgt string) string {
			putData(t, cur, "todo", `{"a":1}`, 1)
			return tgt
		}, verdictMove},
		{"neither has data", func(t *testing.T, _, tgt string) string { return tgt }, verdictSwitch},
		{"empty app folders are not data", func(t *testing.T, cur, tgt string) string {
			putData(t, cur, "todo", "", 0)
			putData(t, tgt, "todo", "", 0)
			return tgt
		}, verdictSwitch},
		{"target data only", func(t *testing.T, _, tgt string) string {
			putData(t, tgt, "notes", `{"n":1}`, 0)
			return tgt
		}, verdictAdopt},
		{"a history version alone is data", func(t *testing.T, _, tgt string) string {
			putData(t, tgt, "notes", "", 1)
			return tgt
		}, verdictAdopt},
		{"both have data", func(t *testing.T, cur, tgt string) string {
			putData(t, cur, "todo", `{"a":1}`, 0)
			putData(t, tgt, "todo", `{"b":1}`, 0)
			return tgt
		}, verdictConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cur, tgt := t.TempDir(), t.TempDir()
			path := c.setup(t, cur, tgt)
			got, err := checkDataDir(cur, path)
			if err != nil {
				t.Fatalf("checkDataDir = %v", err)
			}
			if got.Verdict != c.want {
				t.Errorf("verdict %q (%s), want %q", got.Verdict, got.Message, c.want)
			}
		})
	}
}

func TestCheckDataDirListsTheApps(t *testing.T) {
	cur, tgt := t.TempDir(), t.TempDir()
	putData(t, cur, "todo", `{"a":1}`, 2)
	putData(t, tgt, "notes", `{"nn":12}`, 1)
	if err := os.MkdirAll(filepath.Join(tgt, "apps", "Bad_ID"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := checkDataDir(cur, tgt)
	if err != nil {
		t.Fatal(err)
	}
	want := []appData{{ID: "todo", Size: 7, Versions: 2}}
	if !slices.Equal(c.Current, want) {
		t.Errorf("current %+v, want %+v", c.Current, want)
	}
	if want := []appData{{ID: "notes", Size: 9, Versions: 1}}; !slices.Equal(c.Target, want) {
		t.Errorf("target %+v, want %+v", c.Target, want)
	}
	if c.Path != tgt {
		t.Errorf("path %q, want %q", c.Path, tgt)
	}
}

func TestMoveDataCopiesEverything(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 2)
	putData(t, from, "notes", `{"n":2}`, 0)
	if err := moveData(from, to); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	for _, rel := range []string{
		"apps/todo/data.json", "apps/notes/data.json",
		"apps/todo/history/20260101T000000Z-00000000.json", "apps/todo/history/20260102T000000Z-00000001.json",
	} {
		a, err1 := os.ReadFile(filepath.Join(from, rel))
		b, err2 := os.ReadFile(filepath.Join(to, rel))
		if err1 != nil || err2 != nil || string(a) != string(b) {
			t.Errorf("%s: source %q (%v), copy %q (%v)", rel, a, err1, b, err2)
		}
	}
	if _, err := os.Stat(filepath.Join(to, "apps.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("apps.partial is left: %v", err)
	}
}

func TestMoveDataReplacesEmptyAppFolders(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 0)
	putData(t, to, "todo", "", 0) // an empty leftover
	if err := moveData(from, to); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("copy %q", b)
	}
}

func TestMoveDataFailureLeavesNoPartialCopy(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 0)
	putData(t, from, "notes", `{"n":2}`, 0)
	makeUnreadable(t, filepath.Join(from, "apps", "notes", "data.json"))
	if err := moveData(from, to); err == nil {
		t.Fatal("moveData = nil, want the read error")
	}
	for _, name := range []string{"apps", "apps.partial"} {
		if _, err := os.Stat(filepath.Join(to, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after a failed move: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(from, "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("the source changed: %q", b)
	}
}

func TestAdoptableApps(t *testing.T) {
	dir := t.TempDir()
	putData(t, dir, "todo", `{}`, 0)
	putData(t, dir, "notes", "", 1)
	if err := os.MkdirAll(filepath.Join(dir, "apps", "Bad_ID"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "apps", "stray.json"), "{}")
	if got, want := adoptableApps(dir), []string{"notes", "todo"}; !slices.Equal(got, want) {
		t.Errorf("adoptableApps = %v, want %v", got, want)
	}
	if got := adoptableApps(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("a missing folder gives %v", got)
	}
}

// ourServerDir makes a server folder with every file a server writes.
func ourServerDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workspace-server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity.json", "config.json", "apps.json", "devices.json", "server.log", "server.lock"} {
		writeFile(t, filepath.Join(dir, name), "{}")
	}
	putData(t, dir, "todo", `{"a":1}`, 1)
	return dir
}

func TestResetServerDirKeepsTheSettings(t *testing.T) {
	dir := ourServerDir(t)
	dataDir := t.TempDir()
	putData(t, dataDir, "notes", `{"n":1}`, 1)
	writeFile(t, filepath.Join(dataDir, "mine.txt"), "the user's")
	if err := resetServerDir(dir, dataDir); err != nil {
		t.Fatalf("resetServerDir = %v", err)
	}
	for _, name := range []string{"config.json", "server.log", "server.lock", "apps/todo/data.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err) // apps/ of the server folder is not the data folder here
		}
	}
	for _, name := range []string{"identity.json", "apps.json", "devices.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "apps")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data folder's apps is still there")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "mine.txt")); err != nil {
		t.Errorf("an unrelated file in the data folder went: %v", err)
	}
}

func TestRemoveServerDir(t *testing.T) {
	dir := ourServerDir(t)
	if err := removeServerDir(dir); err != nil {
		t.Fatalf("removeServerDir = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the folder is still there: %v", err)
	}
}

func TestRemoveServerDirRefusesAForeignFolder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.txt"), "the user's")
	err := removeServerDir(dir)
	if err == nil || !strings.Contains(err.Error(), "not a workspace-server folder") {
		t.Errorf("removeServerDir = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("a file went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refusal created server.lock")
	}
}

func TestRemoveServerDirMissingIsNothing(t *testing.T) {
	if err := removeServerDir(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("removeServerDir(missing) = %v", err)
	}
}

func TestRemoveServerDirRefusesALockedFolder(t *testing.T) {
	dir := ourServerDir(t)
	unlock, err := lockServerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// The lock is per process on some systems, so a second lock in this
	// process may succeed; the test needs one held by another process.
	if !lockHeldElsewhere(t, dir) {
		t.Skip("this system's locks do not exclude the same process")
	}
	err = removeServerDir(dir)
	if !errors.Is(err, errLocked) {
		t.Errorf("removeServerDir = %v, want errLocked", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "identity.json")); statErr != nil {
		t.Errorf("a file went while the folder was locked: %v", statErr)
	}
}

// lockHeldElsewhere reports whether a second lockServerDir on dir fails with
// errLocked while the test holds the first.
func lockHeldElsewhere(t *testing.T, dir string) bool {
	t.Helper()
	unlock, err := lockServerDir(dir)
	if err == nil {
		unlock()
		return false
	}
	return errors.Is(err, errLocked)
}

func TestRemoveData(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	putData(t, dataDir, "todo", `{"a":1}`, 1)
	if err := removeData(dataDir); err != nil {
		t.Fatalf("removeData = %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied data folder is still there")
	}
}

func TestRemoveDataKeepsAFolderWithOtherFiles(t *testing.T) {
	dataDir := t.TempDir()
	putData(t, dataDir, "todo", `{"a":1}`, 1)
	writeFile(t, filepath.Join(dataDir, "mine.txt"), "the user's")
	if err := removeData(dataDir); err != nil {
		t.Fatalf("removeData = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "apps")); !errors.Is(err, os.ErrNotExist) {
		t.Error("apps is still there")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "mine.txt")); err != nil {
		t.Errorf("the user's file went: %v", err)
	}
}

func TestRemoveDataMissingIsNothing(t *testing.T) {
	if err := removeData(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("removeData(missing) = %v", err)
	}
}

// A folder named apps that is not ours (a common name) is neither data nor
// deleted: only app folders with a valid id that hold a document or history
// are ours.
func TestForeignAppsFolderIsNotData(t *testing.T) {
	dir := t.TempDir()
	writeFileAll(t, filepath.Join(dir, "apps", "MyTool", "notes.txt"), "the user's")
	writeFileAll(t, filepath.Join(dir, "apps", "notes", "random.txt"), "the user's too")
	if has, err := hasData(dir); err != nil || has {
		t.Errorf("hasData = %v, %v; a foreign apps folder is not data", has, err)
	}
	putData(t, dir, "todo", `{"a":1}`, 0)
	if has, _ := hasData(dir); !has {
		t.Error("hasData = false with a document")
	}
}

func TestResetAndRemoveKeepAForeignAppsFolder(t *testing.T) {
	for name, remove := range map[string]func(string) error{
		"reset":      func(d string) error { return resetServerDir(t.TempDir(), d) },
		"removeData": removeData,
	} {
		dir := t.TempDir()
		writeFileAll(t, filepath.Join(dir, "apps", "MyTool", "notes.txt"), "the user's")
		writeFileAll(t, filepath.Join(dir, "apps", "notes", "random.txt"), "the user's too")
		putData(t, dir, "todo", `{"a":1}`, 1)
		if err := remove(dir); err != nil {
			t.Fatalf("%s = %v", name, err)
		}
		for _, keep := range []string{"apps/MyTool/notes.txt", "apps/notes/random.txt"} {
			if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
				t.Errorf("%s deleted the user's %s", name, keep)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "apps", "todo")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s kept the server's todo folder", name)
		}
	}
}

func TestMoveDataKeepsForeignFilesInTheTarget(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 1)
	writeFileAll(t, filepath.Join(to, "apps", "MyTool", "notes.txt"), "the user's")
	if err := moveData(from, to); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("the moved document: %q", b)
	}
	if _, err := os.Stat(filepath.Join(to, "apps", "MyTool", "notes.txt")); err != nil {
		t.Errorf("the move deleted the user's file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "apps.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Error("apps.partial is left")
	}
}

func writeFileAll(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, content)
}
