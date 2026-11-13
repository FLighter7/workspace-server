package main

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// putData makes an app folder in the data folder dataDir, in its WSS-Data
// folder: the store's .wss folder with an index, data.json holding doc
// unless it is empty, and n more documents f<i>.json of 7 bytes each.
func putData(t *testing.T, dataDir, id, doc string, n int) {
	t.Helper()
	app := filepath.Join(wssDir(dataDir), "apps", id)
	writeFileAll(t, filepath.Join(app, ".wss", "index.json"), "{}")
	if doc != "" {
		writeFile(t, filepath.Join(app, "data.json"), doc)
	}
	for i := range n {
		writeFileAll(t, filepath.Join(app, "f", "f"+string(rune('0'+i))+".json"), `{"v":1}`)
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
			return filepath.Join(cur, "WSS-Data", "apps", "todo")
		}, verdictUnavailable},
		{"inside the current WSS-Data", func(t *testing.T, cur, _ string) string {
			putData(t, cur, "todo", `{"a":1}`, 0)
			return filepath.Join(cur, "WSS-Data", "more")
		}, verdictUnavailable},
		{"the current WSS-Data itself", func(t *testing.T, cur, _ string) string {
			putData(t, cur, "todo", `{"a":1}`, 0)
			writeFile(t, filepath.Join(cur, "WSS-Data", "README.txt"), readmeText)
			return filepath.Join(cur, "WSS-Data")
		}, verdictSame},
		{"WSS-Data is a file there", func(t *testing.T, _, tgt string) string {
			writeFile(t, filepath.Join(tgt, "WSS-Data"), "x")
			return tgt
		}, verdictUnavailable},
		{"data outside WSS-Data is not data", func(t *testing.T, _, tgt string) string {
			writeFileAll(t, filepath.Join(tgt, "apps", "notes", "data.json"), `{"n":1}`)
			return tgt
		}, verdictSwitch},
		{"a WSS-Data folder with data, chosen itself", func(t *testing.T, _, tgt string) string {
			wss := filepath.Join(tgt, "WSS-Data")
			writeFileAll(t, filepath.Join(wss, "apps", "notes", "data.json"), `{"n":1}`)
			writeFileAll(t, filepath.Join(wss, "apps", "notes", ".wss", "index.json"), "{}")
			return wss
		}, verdictAdopt},
		{"an app folder without .wss is not the server's", func(t *testing.T, _, tgt string) string {
			writeFileAll(t, filepath.Join(tgt, "WSS-Data", "apps", "notes", "data.json"), `{"n":1}`)
			return tgt
		}, verdictSwitch},
		{"the server's own files alone are not data", func(t *testing.T, _, tgt string) string {
			putData(t, tgt, "notes", "", 0)
			writeFileAll(t, filepath.Join(wssDir(tgt), "apps", "notes", ".wss", "staging", "tx-0123456789abcdef", "b0"), "body")
			return tgt
		}, verdictSwitch},
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
		{"another document alone is data", func(t *testing.T, _, tgt string) string {
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
	if err := os.MkdirAll(filepath.Join(tgt, "WSS-Data", "apps", "Bad_ID"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := checkDataDir(cur, tgt)
	if err != nil {
		t.Fatal(err)
	}
	want := []appData{{ID: "todo", Size: 7 + 2*7}}
	if !slices.Equal(c.Current, want) {
		t.Errorf("current %+v, want %+v", c.Current, want)
	}
	if want := []appData{{ID: "notes", Size: 9 + 7}}; !slices.Equal(c.Target, want) {
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
	if err := moveData(wssDir(from), wssDir(to)); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	for _, rel := range []string{
		"WSS-Data/apps/todo/data.json", "WSS-Data/apps/notes/data.json",
		"WSS-Data/apps/todo/f/f0.json", "WSS-Data/apps/todo/f/f1.json",
		"WSS-Data/apps/todo/.wss/index.json", "WSS-Data/apps/notes/.wss/index.json",
	} {
		a, err1 := os.ReadFile(filepath.Join(from, rel))
		b, err2 := os.ReadFile(filepath.Join(to, rel))
		if err1 != nil || err2 != nil || string(a) != string(b) {
			t.Errorf("%s: source %q (%v), copy %q (%v)", rel, a, err1, b, err2)
		}
	}
	if _, err := os.Stat(filepath.Join(to, "WSS-Data", "apps.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("apps.partial is left: %v", err)
	}
}

func TestMoveDataReplacesEmptyAppFolders(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 0)
	putData(t, to, "todo", "", 0) // an empty leftover
	if err := moveData(wssDir(from), wssDir(to)); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "WSS-Data", "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("copy %q", b)
	}
}

func TestMoveDataFailureLeavesNoPartialCopy(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 0)
	putData(t, from, "notes", `{"n":2}`, 0)
	makeUnreadable(t, filepath.Join(from, "WSS-Data", "apps", "notes", "data.json"))
	if err := moveData(wssDir(from), wssDir(to)); err == nil {
		t.Fatal("moveData = nil, want the read error")
	}
	if _, err := os.Stat(filepath.Join(to, "WSS-Data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("WSS-Data exists after a failed move: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(from, "WSS-Data", "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("the source changed: %q", b)
	}
}

func TestAdoptableApps(t *testing.T) {
	dir := t.TempDir()
	putData(t, dir, "todo", `{}`, 0)
	putData(t, dir, "notes", "", 1)
	if err := os.MkdirAll(filepath.Join(dir, "WSS-Data", "apps", "Bad_ID"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "WSS-Data", "apps", "stray.json"), "{}")
	if got, want := adoptableApps(wssDir(dir)), []string{"notes", "todo"}; !slices.Equal(got, want) {
		t.Errorf("adoptableApps = %v, want %v", got, want)
	}
	if got := adoptableApps(wssDir(filepath.Join(dir, "missing"))); len(got) != 0 {
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
	for _, name := range []string{"config.json", "server.log", "server.lock", "WSS-Data/apps/todo/data.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err) // the server folder is not the data folder here
		}
	}
	for _, name := range []string{"identity.json", "apps.json", "devices.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "WSS-Data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data folder's WSS-Data is still there")
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
	if _, err := os.Stat(filepath.Join(dataDir, "WSS-Data")); !errors.Is(err, os.ErrNotExist) {
		t.Error("WSS-Data is still there")
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
// deleted: only app folders with a valid id that hold the store's .wss
// folder are ours.
func TestForeignAppsFolderIsNotData(t *testing.T) {
	dir := t.TempDir()
	writeFileAll(t, filepath.Join(dir, "WSS-Data", "apps", "MyTool", "notes.txt"), "the user's")
	writeFileAll(t, filepath.Join(dir, "WSS-Data", "apps", "notes", "random.txt"), "the user's too")
	if has, err := hasData(wssDir(dir)); err != nil || has {
		t.Errorf("hasData = %v, %v; a foreign apps folder is not data", has, err)
	}
	putData(t, dir, "todo", `{"a":1}`, 0)
	if has, _ := hasData(wssDir(dir)); !has {
		t.Error("hasData = false with a document")
	}
}

func TestResetAndRemoveKeepAForeignAppsFolder(t *testing.T) {
	for name, remove := range map[string]func(string) error{
		"reset":      func(d string) error { return resetServerDir(t.TempDir(), d) },
		"removeData": removeData,
	} {
		dir := t.TempDir()
		writeFileAll(t, filepath.Join(dir, "WSS-Data", "apps", "MyTool", "notes.txt"), "the user's")
		writeFileAll(t, filepath.Join(dir, "WSS-Data", "apps", "notes", "random.txt"), "the user's too")
		writeFile(t, filepath.Join(dir, "WSS-Data", "README.txt"), readmeText)
		putData(t, dir, "todo", `{"a":1}`, 1)
		if err := remove(dir); err != nil {
			t.Fatalf("%s = %v", name, err)
		}
		for _, keep := range []string{"WSS-Data/apps/MyTool/notes.txt", "WSS-Data/apps/notes/random.txt", "WSS-Data/README.txt"} {
			if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
				t.Errorf("%s deleted %s", name, keep)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "WSS-Data", "apps", "todo")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s kept the server's todo folder", name)
		}
	}
}

func TestMoveDataKeepsForeignFilesInTheTarget(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 1)
	writeFileAll(t, filepath.Join(to, "WSS-Data", "apps", "MyTool", "notes.txt"), "the user's")
	if err := moveData(wssDir(from), wssDir(to)); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "WSS-Data", "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("the moved document: %q", b)
	}
	if _, err := os.Stat(filepath.Join(to, "WSS-Data", "apps", "MyTool", "notes.txt")); err != nil {
		t.Errorf("the move deleted the user's file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "WSS-Data", "apps.partial")); !errors.Is(err, os.ErrNotExist) {
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

func TestWSSDir(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, files ...string) string {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			writeFileAll(t, filepath.Join(dir, f), "x")
		}
		return dir
	}
	nested := func(dir string) string { return filepath.Join(dir, "WSS-Data") }
	cases := []struct {
		name, dir, want string
	}{
		{"a plain folder", mk("plain"), nested(filepath.Join(root, "plain"))},
		{"a plain folder holding apps", mk("withapps", "apps/todo/data.json"), nested(filepath.Join(root, "withapps"))},
		{"WSS-Data with README.txt", mk("a/WSS-Data", "README.txt"), filepath.Join(root, "a", "WSS-Data")},
		{"WSS-Data with apps", mk("b/WSS-Data", "apps/todo/data.json"), filepath.Join(root, "b", "WSS-Data")},
		{"an empty WSS-Data", mk("c/WSS-Data"), filepath.Join(root, "c", "WSS-Data")},
		{"a missing WSS-Data", filepath.Join(root, "d", "WSS-Data"), filepath.Join(root, "d", "WSS-Data")},
		{"WSS-Data holding other files only", mk("e/WSS-Data", "photo.jpg"), nested(filepath.Join(root, "e", "WSS-Data"))},
		{"WSS-Data holding only the system's files", mk("g/WSS-Data", ".DS_Store", "desktop.ini", "Thumbs.db"), filepath.Join(root, "g", "WSS-Data")},
	}
	// The name in another case is WSS-Data where the file system ignores
	// case.
	otherCase := mk("f/wss-data", "README.txt")
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		cases = append(cases, struct{ name, dir, want string }{"another case", otherCase, otherCase})
	} else {
		cases = append(cases, struct{ name, dir, want string }{"another case", otherCase, nested(otherCase)})
	}
	for _, c := range cases {
		if got := wssDir(c.dir); got != c.want {
			t.Errorf("%s: wssDir = %s, want %s", c.name, got, c.want)
		}
	}
	if got, want := appsRoot(filepath.Join(root, "plain")), filepath.Join(root, "plain", "WSS-Data", "apps"); got != want {
		t.Errorf("appsRoot = %s, want %s", got, want)
	}
}

// Choosing a WSS-Data folder as the data folder uses it as is: the data is
// moved into it, not into a WSS-Data inside it.
func TestMoveDataIntoAWSSDataFolderChosenItself(t *testing.T) {
	from := t.TempDir()
	putData(t, from, "todo", `{"a":1}`, 0)
	to := filepath.Join(t.TempDir(), "WSS-Data")
	c, err := checkDataDir(from, to)
	if err != nil || c.Verdict != verdictCreate {
		t.Fatalf("checkDataDir = %s %q, %v; want create", c.Verdict, c.Message, err)
	}
	if err := ensureDataDir(to); err != nil {
		t.Fatal(err)
	}
	if err := moveData(wssDir(from), wssDir(to)); err != nil {
		t.Fatalf("moveData = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("the moved document: %q", b)
	}
	if _, err := os.Stat(filepath.Join(to, "README.txt")); err != nil {
		t.Errorf("README.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "WSS-Data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a WSS-Data inside the chosen WSS-Data: %v", err)
	}
	// Moving back to the folder holding it is the same data.
	if c, _ := checkDataDir(to, filepath.Dir(to)); c.Verdict != verdictSame {
		t.Errorf("its parent: verdict %s, want same", c.Verdict)
	}
}

// README.txt is written with WSS-Data, written again when it is gone, and
// never written over.
func TestEnsureDataDirWritesTheReadme(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	readme := filepath.Join(dataDir, "WSS-Data", "README.txt")
	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(readme); err != nil || string(b) != readmeText {
		t.Fatalf("README.txt %q, %v", b, err)
	}
	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(readme); err != nil || string(b) != readmeText {
		t.Errorf("README.txt after it was deleted: %q, %v", b, err)
	}
	writeFile(t, readme, "the user's words")
	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(readme); string(b) != "the user's words" {
		t.Errorf("README.txt was written over: %q", b)
	}
}

func TestEnsureDataDirRefusesAWSSDataFile(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, filepath.Join(dataDir, "WSS-Data"), "x")
	if err := ensureDataDir(dataDir); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("ensureDataDir = %v, want the folder unavailable", err)
	}
}

// Deleting the data removes README.txt and WSS-Data only when nothing else
// is left in WSS-Data.
func TestRemoveDataKeepsAWSSDataFolderWithOtherFiles(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	putData(t, dataDir, "todo", `{"a":1}`, 1)
	writeFile(t, filepath.Join(dataDir, "WSS-Data", "notes.txt"), "the user's")
	if err := removeData(dataDir); err != nil {
		t.Fatalf("removeData = %v", err)
	}
	for _, keep := range []string{"WSS-Data/notes.txt", "WSS-Data/README.txt"} {
		if _, err := os.Stat(filepath.Join(dataDir, keep)); err != nil {
			t.Errorf("%s went: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "WSS-Data", "apps")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("apps is still there: %v", err)
	}

	// Without the other file, all of it goes.
	if err := os.Remove(filepath.Join(dataDir, "WSS-Data", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	putData(t, dataDir, "todo", `{"a":1}`, 0)
	if err := removeData(dataDir); err != nil {
		t.Fatalf("removeData = %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data folder is still there: %v", err)
	}
}

// A WSS-Data folder chosen itself goes with its data; the reset, which
// serves again from it, finds it again rather than a WSS-Data inside it.
func TestResetOfAWSSDataFolderChosenItself(t *testing.T) {
	wss := filepath.Join(t.TempDir(), "WSS-Data")
	if err := ensureDataDir(wss); err != nil {
		t.Fatal(err)
	}
	putData(t, wss, "todo", `{"a":1}`, 0)
	if err := resetServerDir(t.TempDir(), wss); err != nil {
		t.Fatalf("resetServerDir = %v", err)
	}
	if _, err := os.Stat(wss); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied WSS-Data is still there: %v", err)
	}
	if err := ensureDataDir(wss); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wss, "README.txt")); err != nil {
		t.Errorf("README.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wss, "WSS-Data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a WSS-Data inside the chosen WSS-Data: %v", err)
	}
}

// nestedWSSData makes a data folder x/WSS-Data that the user chose and that
// holds a file of theirs, so that the server keeps the data in
// x/WSS-Data/WSS-Data, with a document there. It returns x and x/WSS-Data.
func nestedWSSData(t *testing.T) (x, chosen string) {
	t.Helper()
	x = filepath.Join(t.TempDir(), "x")
	chosen = filepath.Join(x, "WSS-Data")
	writeFileAll(t, filepath.Join(chosen, "notes.txt"), "the user's")
	if err := ensureDataDir(chosen); err != nil {
		t.Fatal(err)
	}
	if got, want := wssDir(chosen), filepath.Join(chosen, "WSS-Data"); got != want {
		t.Fatalf("wssDir = %s, want %s", got, want)
	}
	putData(t, chosen, "todo", `{"a":1}`, 0)
	return x, chosen
}

// Moving to the parent of a chosen WSS-Data that holds the data in a
// WSS-Data of its own would put the data into the folder that holds it: the
// check refuses, and so does the move, which leaves everything as it was.
func TestCheckDataDirRefusesOverlappingWSSData(t *testing.T) {
	x, chosen := nestedWSSData(t)
	c, err := checkDataDir(chosen, x)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verdict != verdictUnavailable || !strings.Contains(c.Message, "overlaps") {
		t.Errorf("checkDataDir(%s) = %s %q, want unavailable: it overlaps", x, c.Verdict, c.Message)
	}
	serverDir := t.TempDir()
	for _, v := range []verdict{verdictMove, verdictSwitch} {
		_, err := moveDataDir(serverDir, chosen, action{kind: actMoveData, dataDir: x, verdict: v}, log.New(io.Discard, "", 0))
		if err == nil {
			t.Errorf("moveDataDir (%s) = nil, want a refusal", v)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(chosen, "WSS-Data", "apps", "todo", "data.json")); string(b) != `{"a":1}` {
		t.Errorf("the data: %q", b)
	}
	if _, err := os.Stat(filepath.Join(serverDir, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config.json was written (%v)", err)
	}

	// The current data inside the target's WSS-Data, the other way round.
	a := t.TempDir()
	inner := filepath.Join(a, "WSS-Data", "inner")
	putData(t, inner, "todo", `{"a":1}`, 0)
	if c, _ := checkDataDir(inner, a); c.Verdict != verdictUnavailable {
		t.Errorf("checkDataDir(%s) from inside its WSS-Data = %s, want unavailable", a, c.Verdict)
	}
}

// moveData refuses a source with no data, and two folders one inside the
// other, rather than reporting a move that moved nothing.
func TestMoveDataRefusesNothingToMove(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := moveData(wssDir(from), wssDir(to)); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("moveData from a folder with no data = %v, want an error", err)
	}
	putData(t, from, "todo", `{"a":1}`, 0)
	if err := moveData(wssDir(from), filepath.Join(wssDir(from), "WSS-Data")); err == nil {
		t.Error("moveData into its own WSS-Data = nil, want an error")
	}
}

// A failed move adds nothing to the target: not the folder a create made,
// not WSS-Data, not README.txt.
func TestMoveDataDirFailureAddsNothing(t *testing.T) {
	saved := moveDataFn
	moveDataFn = func(from, to string) error { return errors.New("disk full") }
	t.Cleanup(func() { moveDataFn = saved })
	logger := log.New(io.Discard, "", 0)
	serverDir := t.TempDir()
	putData(t, serverDir, "todo", `{"a":1}`, 0)

	existing := t.TempDir()
	if _, err := moveDataDir(serverDir, serverDir, action{kind: actMoveData, dataDir: existing, verdict: verdictMove}, logger); err == nil {
		t.Fatal("moveDataDir = nil, want the copy's error")
	}
	if entries, _ := os.ReadDir(existing); len(entries) != 0 {
		t.Errorf("the target holds %v after a failed move", entries)
	}
	created := filepath.Join(t.TempDir(), "new")
	if _, err := moveDataDir(serverDir, serverDir, action{kind: actMoveData, dataDir: created, verdict: verdictCreate}, logger); err == nil {
		t.Fatal("moveDataDir = nil, want the copy's error")
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the created folder stays after a failed move: %v", err)
	}
}

// Where the file system ignores case, the current WSS-Data named in another
// case is the same folder, not one inside the current data.
func TestCheckDataDirSameWSSDataInAnotherCase(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("the file system tells case apart")
	}
	cur := t.TempDir()
	if err := ensureDataDir(cur); err != nil {
		t.Fatal(err)
	}
	putData(t, cur, "todo", `{"a":1}`, 0)
	for _, target := range []string{filepath.Join(cur, "wss-data"), strings.ToUpper(cur)} {
		if c, _ := checkDataDir(cur, target); c.Verdict != verdictSame {
			t.Errorf("checkDataDir(%s) = %s %q, want same", target, c.Verdict, c.Message)
		}
	}
	chosen := filepath.Join(t.TempDir(), "WSS-Data")
	if err := ensureDataDir(chosen); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(chosen), "wss-data")
	if c, _ := checkDataDir(chosen, other); c.Verdict != verdictSame {
		t.Errorf("checkDataDir(%s) = %s %q, want same", other, c.Verdict, c.Message)
	}
}

// linkDir makes link a symbolic link to the folder target, or a junction on
// Windows, where a symbolic link needs a privilege.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("mklink /J: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
}

// A WSS-Data that is a link to the user's folder keeps the link when the
// data goes; only README.txt goes from it.
func TestRemoveAppsKeepsALinkedWSSData(t *testing.T) {
	real := t.TempDir()
	dataDir := t.TempDir()
	link := filepath.Join(dataDir, "WSS-Data")
	linkDir(t, real, link)
	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	putData(t, dataDir, "todo", `{"a":1}`, 0)
	if _, err := os.Stat(filepath.Join(real, "apps", "todo", "data.json")); err != nil {
		t.Fatalf("the data is not behind the link: %v", err)
	}
	if err := resetServerDir(t.TempDir(), dataDir); err != nil {
		t.Fatalf("resetServerDir = %v", err)
	}
	if !isLink(link) {
		t.Errorf("the link is gone or is a folder now")
	}
	if entries, err := os.ReadDir(real); err != nil || len(entries) != 0 {
		t.Errorf("the linked folder holds %v (%v), want nothing", entries, err)
	}
}

// A move whose config.json cannot be saved takes back only what it added:
// a WSS-Data the user made in the target, empty, stays.
func TestMoveDataDirSaveFailureKeepsTheUsersWSSData(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	for _, made := range []bool{true, false} {
		serverDir := t.TempDir()
		putData(t, serverDir, "todo", `{"a":1}`, 0)
		// config.json a folder: it can be neither read nor saved.
		if err := os.Mkdir(filepath.Join(serverDir, configFile), 0o700); err != nil {
			t.Fatal(err)
		}
		mine := t.TempDir()
		if made {
			if err := os.MkdirAll(filepath.Join(mine, "WSS-Data"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		c, err := checkDataDir(serverDir, mine)
		if err != nil || c.Verdict != verdictMove {
			t.Fatalf("checkDataDir = %s %q, %v; want move", c.Verdict, c.Message, err)
		}
		if _, err := moveDataDir(serverDir, serverDir, action{kind: actMoveData, dataDir: mine, verdict: verdictMove}, logger); err == nil {
			t.Fatal("moveDataDir = nil, want the configuration's error")
		}
		if made {
			entries, err := os.ReadDir(filepath.Join(mine, "WSS-Data"))
			if err != nil || len(entries) != 0 {
				t.Errorf("the user's WSS-Data: %v, %v; want it there and empty", entries, err)
			}
		} else if entries, _ := os.ReadDir(mine); len(entries) != 0 {
			t.Errorf("the target holds %v after a failed move", entries)
		}
		if b, _ := os.ReadFile(filepath.Join(serverDir, "WSS-Data", "apps", "todo", "data.json")); string(b) != `{"a":1}` {
			t.Errorf("the data: %q", b)
		}
	}
}

// The systems' own files do not keep README.txt and WSS-Data when the data
// goes; anything else keeps all of them.
func TestRemoveDataTakesTheSystemsFilesAlong(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	putData(t, dataDir, "todo", `{"a":1}`, 0)
	for _, name := range []string{".DS_Store", "desktop.ini", "Thumbs.db"} {
		writeFile(t, filepath.Join(dataDir, "WSS-Data", name), "x")
	}
	if err := removeData(dataDir); err != nil {
		t.Fatalf("removeData = %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		entries, _ := os.ReadDir(filepath.Join(dataDir, "WSS-Data"))
		t.Errorf("the data folder stays (%v), WSS-Data holding %v", err, entries)
	}

	if err := ensureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dataDir, "WSS-Data", "desktop.ini"), "x")
	writeFile(t, filepath.Join(dataDir, "WSS-Data", "notes.txt"), "the user's")
	if err := removeData(dataDir); err != nil {
		t.Fatalf("removeData = %v", err)
	}
	for _, keep := range []string{"README.txt", "desktop.ini", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dataDir, "WSS-Data", keep)); err != nil {
			t.Errorf("%s went: %v", keep, err)
		}
	}
}

// Where the file system ignores case (Windows, and macOS, whose path is
// forced here), a target inside the current WSS-Data named in another case
// is refused as overlapping.
func TestOverlapIgnoresCaseWhereTheSystemDoes(t *testing.T) {
	if want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"; foldCase != want {
		t.Errorf("foldCase = %v on %s, want %v", foldCase, runtime.GOOS, want)
	}
	saved := foldCase
	foldCase = true
	t.Cleanup(func() { foldCase = saved })
	root := string(filepath.Separator) + "x"
	if !within(filepath.Join(root, "WSS-Data"), filepath.Join(root, "wss-data", "foo")) {
		t.Error("within ignores no case")
	}
	if !samePath(filepath.Join(root, "WSS-Data"), filepath.Join(root, "wss-data")) {
		t.Error("samePath ignores no case")
	}
	cur := t.TempDir()
	if err := ensureDataDir(cur); err != nil {
		t.Fatal(err)
	}
	putData(t, cur, "todo", `{"a":1}`, 0)
	target := filepath.Join(cur, "wss-data", "foo")
	if c, _ := checkDataDir(cur, target); c.Verdict != verdictUnavailable {
		t.Errorf("checkDataDir(%s) = %s %q, want unavailable", target, c.Verdict, c.Message)
	}
}
