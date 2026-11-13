package store

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These cover what the review of 2026-11-13 found: paths a commit must not
// take, a journal that must never stop an app for good, the quota's edges,
// and the durability of a body that is resumed.

// dirLink makes link a link to the folder target: a junction on Windows,
// which needs no privilege, a symbolic link elsewhere.
func dirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
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

// outsideFolder returns a folder outside the app's, holding secret.json.
func outsideFolder(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// commitOne stages one change in a transaction of "dev" and commits it: a
// body, or "-" for a delete.
func (e *env) commitOne(ks string, ifEtag *string, body string) error {
	e.t.Helper()
	e.begin("todo", "dev", txA)
	if body == "-" {
		if err := e.s.Delete("todo", "dev", txA, key(e.t, ks), ifEtag); err != nil {
			e.t.Fatal(err)
		}
	} else {
		e.put("todo", "dev", txA, ks, ifEtag, body)
	}
	_, err := e.s.Commit("todo", "dev", txA)
	return err
}

// healthy checks that the app still answers, with no journal left.
func (e *env) healthy() {
	e.t.Helper()
	if _, err := e.s.Head("todo"); err != nil {
		e.t.Fatalf("the app is stopped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "todo", ".wss", journalName)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("a journal is left: %v", err)
	}
}

// I2: a link or junction in the app's folder never lets a commit write or
// delete outside it, and is never removed.
func TestCommitDoesNotFollowLinks(t *testing.T) {
	e := newEnv(t)
	e.writeFile("todo", "a.json", "a")
	outside := outsideFolder(t)
	dirLink(t, e.file("todo", "dir"), outside)
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "a"}) // not followed

	if err := e.commitOne("dir/new.json", nil, "x"); !errors.Is(err, ErrBadPath) {
		t.Fatalf("a put below a link = %v, want ErrBadPath", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a put wrote through the link")
	}
	if err := e.s.Abort("todo", "dev", txA); err != nil {
		t.Fatal(err)
	}
	// A delete of what is not a document deletes nothing.
	if err := e.commitOne("dir/secret.json", nil, "-"); err != nil {
		t.Fatalf("a delete of nothing: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret.json")); err != nil || string(b) != "secret" {
		t.Errorf("the file outside: %q, %v", b, err)
	}
	if _, err := os.Lstat(e.file("todo", "dir")); err != nil {
		t.Errorf("the link went: %v", err)
	}
	e.healthy()

	// removeEmptyDirs stops at a link even when asked to start there.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	dirLink(t, e.file("todo", "x/link"), empty)
	removeEmptyDirs(e.file("todo", "x/link"), filepath.Join(e.root, "todo"))
	if _, err := os.Lstat(e.file("todo", "x/link")); err != nil {
		t.Errorf("removeEmptyDirs removed a link: %v", err)
	}
	if _, err := os.Stat(empty); err != nil {
		t.Errorf("the link's target went: %v", err)
	}
}

// I1: what the disk would not take is refused before the commit point; the
// app goes on and the transaction stays open.
func TestCommitRefusesObstacles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env)
		key   string
	}{
		{"a folder where the document goes", func(e *env) { e.writeFile("todo", "a.json/inner.txt", "user") }, "a.json"},
		{"a file where a folder goes", func(e *env) { e.writeFile("todo", "x", "user file") }, "x/y.json"},
		{"a file where a deeper folder goes", func(e *env) { e.writeFile("todo", "p/q", "user file") }, "p/q/r/s.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeFile("todo", "keep.json", "k")
			tc.setup(e)
			if err := e.commitOne(tc.key, nil, "body"); !errors.Is(err, ErrBadPath) {
				t.Fatalf("Commit = %v, want ErrBadPath", err)
			}
			e.healthy()
			if staged := e.begin("todo", "dev", txA); len(staged) != 1 {
				t.Errorf("the transaction did not stay open: %+v", staged)
			}
		})
	}
}

// M2: a file whose name differs only in case is no document; a put of the
// lower-case key never replaces it.
func TestCommitLeavesAnotherCaseAlone(t *testing.T) {
	e := newEnv(t)
	e.writeFile("todo", "Notes.json", "user's own")
	err := e.commitOne("notes.json", nil, "from device")
	if _, statErr := os.Stat(e.file("todo", "notes.json")); statErr == nil && err == nil {
		// A file system that tells case: two files side by side.
		if b, _ := e.readFile("todo", "notes.json"); b != "from device" {
			t.Errorf("notes.json holds %q", b)
		}
	} else if !errors.Is(err, ErrBadPath) {
		t.Fatalf("Commit = %v, want ErrBadPath where case is ignored", err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.root, "todo"))
	found := false
	for _, x := range entries {
		if x.Name() == "Notes.json" {
			found = true
		}
	}
	if b, err := os.ReadFile(e.file("todo", "Notes.json")); !found || err != nil || string(b) != "user's own" {
		t.Errorf("Notes.json (listed %v) holds %q, %v", found, b, err)
	}
}

// I1: a document someone marked read-only is replaced and deleted like any
// other.
func TestReadOnlyDocuments(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "1", "b.json": "b"})
	for _, ks := range []string{"a.json", "b.json"} {
		if err := os.Chmod(e.file("todo", ks), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.commitOne("a.json", ptr(etagOf("1")), "2"); err != nil {
		t.Fatalf("replacing a read-only document: %v", err)
	}
	if err := e.commitOne("b.json", ptr(etagOf("b")), "-"); err != nil {
		t.Fatalf("deleting a read-only document: %v", err)
	}
	e.healthy()
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "2"})
}

// I1: a change that cannot be applied after the commit point is dropped
// rather than stop the app: at once for a lasting failure, after
// journalGiveUp for one that might pass. The index then reflects the disk.
func TestJournalNeverStopsTheAppForGood(t *testing.T) {
	for _, transient := range []bool{false, true} {
		t.Run(fmt.Sprintf("transient=%v", transient), func(t *testing.T) {
			e := newEnv(t)
			var logs []string
			e.s.SetLogf(func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
			e.commitAll("todo", map[string]string{"old.json": "old", "keep.json": "keep"})
			fail := errors.New("lasting failure")
			if transient {
				fail = fmt.Errorf("busy: %w", errTransient)
			}
			orig := renameFile
			t.Cleanup(func() { renameFile = orig })
			renameFile = func(from, to string) error {
				if filepath.Base(to) == "old.json" {
					return fail
				}
				return orig(from, to)
			}
			e.begin("todo", "dev", txA)
			e.put("todo", "dev", txA, "old.json", ptr(etagOf("old")), "new")
			e.put("todo", "dev", txA, "added.json", nil, "added")
			_, err := e.s.Commit("todo", "dev", txA)
			if err == nil {
				t.Fatal("Commit went through")
			}
			if transient {
				// Kept for later: the app waits, then gives up.
				if _, err := e.s.Head("todo"); err == nil {
					t.Fatal("the app answers while a busy change waits")
				}
				e.clock.add(journalGiveUp)
			}
			e.healthy()
			wantDocs(t, e.docs("todo"), map[string]string{"old.json": "old", "keep.json": "keep", "added.json": "added"})
			if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "old.json") {
				t.Errorf("the dropped change was not logged: %q", logs)
			}
		})
	}
}

// I3: a size past the quota is refused before anything else; the sum of a
// transaction never wraps around.
func TestQuotaOverflow(t *testing.T) {
	e := newEnv(t)
	e.quota = 1 << 20
	e.commitAll("todo", map[string]string{"a.bin": "hello"})
	e.begin("todo", "phone", txA)
	for _, n := range []int64{math.MaxInt64, math.MaxInt64 - 3, 1<<20 + 1} {
		if _, err := e.s.Put("todo", "phone", txA, key(t, "b.bin"), nil, n, 0); !errors.Is(err, ErrQuota) {
			t.Errorf("Put of %d bytes = %v, want ErrQuota", n, err)
		}
	}
	if satAdd(math.MaxInt64-1, 5) != math.MaxInt64 || satAdd(2, 3) != 5 {
		t.Error("satAdd does not saturate")
	}
}

// I5: a set over its quota (the quota lowered, files added by hand) can
// shrink, but not grow.
func TestOverQuotaShrinks(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("a", 700<<10)
	e.commitAll("todo", map[string]string{"a.bin": big, "b.bin": big, "c.bin": big})
	e.quota = 1 << 20 // 2.1 MiB held
	if err := e.commitOne("a.bin", ptr(etagOf(big)), "-"); err != nil {
		t.Fatalf("a delete from an over-quota set: %v", err)
	}
	if err := e.commitOne("b.bin", ptr(etagOf(big)), "small"); err != nil {
		t.Fatalf("a smaller body in an over-quota set: %v", err)
	}
	e.begin("todo", "dev", txB)
	if _, err := e.s.Put("todo", "dev", txB, key(t, "d.bin"), nil, 400<<10, 0); !errors.Is(err, ErrQuota) {
		t.Errorf("growing an over-quota set = %v, want ErrQuota", err)
	}
	// The shim follows the same rule.
	e.writeFile("todo", "data.json", `"`+strings.Repeat("d", 500<<10)+`"`)
	cur := etagOf(`"` + strings.Repeat("d", 500<<10) + `"`)
	if _, err := e.s.Write("todo", []byte(`{}`), &cur); err != nil {
		t.Errorf("the shim shrinking an over-quota set: %v", err)
	}
}

// I4: tx.json never counts a byte that is not synced: every record that
// does not complete a body comes after a sync of it, and so does Close.
func TestReceivedIsSyncedFirst(t *testing.T) {
	e := newEnv(t)
	persistEvery = 100
	t.Cleanup(func() { persistEvery = 8 << 20 })
	var syncs []int64 // what tx.json recorded at each sync
	syncFile = func(f *os.File) error {
		staged := stagedOnDisk(t, e, txA)
		syncs = append(syncs, staged)
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(make([]byte, 150)); err != nil { // a record at 150
		t.Fatal(err)
	}
	if _, err := sink.Write(make([]byte, 30)); err != nil { // none
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil { // a record at 180
		t.Fatal(err)
	}
	if fmt.Sprint(syncs) != "[0 150]" {
		t.Errorf("tx.json held %v at the syncs, want [0 150]: each sync before its record", syncs)
	}
	if got := stagedOnDisk(t, e, txA); got != 180 {
		t.Errorf("tx.json holds %d, want 180", got)
	}
}

// stagedOnDisk reads what tx.json of tx records as received for its first
// change.
func stagedOnDisk(t *testing.T, e *env, tx string) int64 {
	t.Helper()
	txs, err := e.s.loadTxs("todo")
	if err != nil || txs[tx] == nil || len(txs[tx].rec.Ops) == 0 {
		t.Fatalf("tx.json of %s: %v", tx, err)
	}
	// loadTxs bounds received by the body file; read the record itself.
	b, _ := os.ReadFile(filepath.Join(txs[tx].dir, txName))
	m := regexp.MustCompile(`"received": (\d+)`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("tx.json: %s", b)
	}
	var n int64
	fmt.Sscan(string(m[1]), &n)
	return n
}

// M3: ending a transaction never waits for a body's sync.
func TestAbortDoesNotWaitForASync(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	syncing := make(chan struct{})
	var once sync.Once
	syncFile = func(f *os.File) error {
		once.Do(func() { close(syncing) })
		<-release
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() { _, err := sink.Write([]byte("abcd")); wrote <- err }() // completes: syncs
	<-syncing
	aborted := make(chan error, 1)
	go func() { aborted <- e.s.Abort("todo", "phone", txA) }()
	select {
	case err := <-aborted:
		if err != nil && !isBusy(err) {
			t.Errorf("Abort: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Abort waits for the sync")
	}
	close(release)
	if err := <-wrote; !errors.Is(err, ErrNoTransaction) {
		t.Errorf("the write that completed under Abort = %v, want ErrNoTransaction", err)
	}
	_ = e.s.Sweep(e.clock.now())
}

// M8: a put that is refused leaves the body being written for the same key
// alone.
func TestRefusedPutKeepsTheRunningBody(t *testing.T) {
	e := newEnv(t)
	e.quota = 1 << 20
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("01234")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 10, 7); !errors.Is(err, ErrBadBody) {
		t.Errorf("Put past what arrived = %v, want ErrBadBody", err)
	}
	if _, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 2<<20, 0); !errors.Is(err, ErrQuota) {
		t.Errorf("Put past the quota = %v, want ErrQuota", err)
	}
	if _, err := sink.Write([]byte("56789")); err != nil {
		t.Fatalf("the running body was ended: %v", err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.bin": "0123456789"})

	// A resume from what a running body received so far is accepted, and
	// ends that body.
	e.begin("todo", "phone", txB)
	sink, _ = e.s.Put("todo", "phone", txB, key(t, "b.bin"), nil, 6, 0)
	if _, err := sink.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	sink2, err := e.s.Put("todo", "phone", txB, key(t, "b.bin"), nil, 6, 3)
	if err != nil {
		t.Fatalf("resume at what the running body received: %v", err)
	}
	if _, err := sink.Write([]byte("x")); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("the replaced body still writes: %v", err)
	}
	if _, err := sink2.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txB); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.bin": "0123456789", "b.bin": "abcdef"})
}

// M1: two transaction ids that differ only in case keep their own bodies,
// on any file system.
func TestTxIdsDifferingInCase(t *testing.T) {
	e := newEnv(t)
	t1, t2 := "tx-AAAAAAAAAAAAAAAA", "tx-aaaaaaaaaaaaaaaa"
	e.begin("todo", "phone", t1)
	e.put("todo", "phone", t1, "p.json", nil, "phone-body")
	e.begin("todo", "laptop", t2)
	e.put("todo", "laptop", t2, "l.json", nil, "laptop-xx")
	if _, err := e.s.Commit("todo", "phone", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "laptop", t2); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"p.json": "phone-body", "l.json": "laptop-xx"})
	if _, ok := txOfDir(stagingDir(t1)); !ok || stagingDir(t1) == stagingDir(t2) {
		t.Error("staging folder names")
	}
	for _, name := range []string{"zz", "7478", strings.ToUpper(stagingDir(t1)), t1} {
		if _, ok := txOfDir(name); ok {
			t.Errorf("txOfDir(%q) is a transaction", name)
		}
	}
}

// M4: a file that changes within the same tick of its mtime, keeping its
// size, is still seen: a file hashed soon after its mtime is hashed again.
func TestRacyMtime(t *testing.T) {
	e := newEnv(t)
	e.writeFile("todo", "a.json", "one")
	mtime := time.Now().Add(-time.Second).Truncate(time.Second)
	if err := os.Chtimes(e.file("todo", "a.json"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "one"})
	e.writeFile("todo", "a.json", "two") // same size, same tick
	if err := os.Chtimes(e.file("todo", "a.json"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if changed, _, err := e.s.Reconcile("todo"); !changed || err != nil {
		t.Fatalf("Reconcile = %v, %v; want the change seen", changed, err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "two"})
}

// M5: a replay that finds a body already moved records what lies at the
// target, hashed, not what the journal expected.
func TestReplayHashesAMovedBody(t *testing.T) {
	e := crashScenario(t)
	failing := true
	inject(t, func(op, path string) bool {
		return failing && op == "rename" && strings.HasSuffix(filepath.ToSlash(path), "/new/n.json")
	})
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	failing = false
	e.writeFile("todo", "old.json", "hand") // after old.json moved, before the replay
	e.restart()
	wantDocs(t, e.docs("todo"), map[string]string{"keep.json": "keep", "old.json": "hand", "new/n.json": "n"})
}

// M6: the journal is flushed through a handle of its own before it is
// applied.
func TestJournalIsFlushed(t *testing.T) {
	e := newEnv(t)
	var flushed []string
	old := flushFile
	flushFile = func(path string) error {
		flushed = append(flushed, filepath.Base(path))
		_, err := os.Stat(path)
		return err
	}
	t.Cleanup(func() { flushFile = old })
	e.commitAll("todo", map[string]string{"a.json": "1"})
	if fmt.Sprint(flushed) != "[journal.json]" {
		t.Errorf("flushed %v", flushed)
	}
}

// M9: a document that became a folder is stale, not an error; a sweep
// removes what a crash left of the store's atomic writes.
func TestOpenOfAFolderAndStrayTemps(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "1"})
	if err := os.Remove(e.file("todo", "a.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.file("todo", "a.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Open("todo", key(t, "a.json"), etagOf("1")); !errors.Is(err, ErrStale) {
		t.Errorf("Open of a folder = %v, want ErrStale", err)
	}

	e.begin("todo", "phone", txA)
	wss := filepath.Join(e.root, "todo", ".wss")
	temps := []string{
		filepath.Join(wss, "index.json.tmp-123"),
		filepath.Join(wss, "journal.json.tmp-456"),
		filepath.Join(wss, stagingName, stagingDir(txA), "tx.json.tmp-789"),
	}
	for _, p := range temps {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.Sweep(e.clock.now()); err != nil {
		t.Fatal(err)
	}
	for _, p := range temps {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stays: %v", filepath.Base(p), err)
		}
	}
	if staged := e.begin("todo", "phone", txA); staged == nil {
		t.Error("the open transaction went with the temps")
	}
}

// R1: a folder on disk whose name differs only in case from a key's folder
// is not that folder: a commit into it is refused, rather than leave the
// document where no key names it.
func TestCommitRefusesAFolderOfAnotherCase(t *testing.T) {
	e := newEnv(t)
	e.writeFile("todo", "Docs/mine.txt", "user's")
	err := e.commitOne("docs/a.json", nil, "device")
	if fi, statErr := os.Stat(e.file("todo", "docs")); statErr == nil && fi.IsDir() && err == nil {
		// A file system that tells case: docs/ is a folder of its own.
		wantDocs(t, e.docs("todo"), map[string]string{"docs/a.json": "device"})
	} else if !errors.Is(err, ErrBadPath) {
		t.Fatalf("Commit into Docs/ = %v, want ErrBadPath where case is ignored", err)
	} else {
		if _, err := os.Stat(e.file("todo", "Docs/a.json")); !errors.Is(err, os.ErrNotExist) {
			t.Error("the document went into the user's Docs/")
		}
	}
	e.healthy()
	if changed, _, _ := e.s.Reconcile("todo"); changed {
		t.Error("a reconcile after the commit changed the set")
	}
	if b, _ := e.readFile("todo", "Docs/mine.txt"); b != "user's" {
		t.Errorf("Docs/mine.txt holds %q", b)
	}
}

// R2: a put that resumes from what a still running body received syncs
// that body before tx.json counts it; so does one that only changes ifEtag
// while the body's own final sync still runs.
func TestResumeFromALiveBodySyncsFirst(t *testing.T) {
	e := newEnv(t)
	var atSync []int64
	syncFile = func(f *os.File) error {
		atSync = append(atSync, stagedOnDisk(t, e, txA))
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("abc")); err != nil { // not recorded yet
		t.Fatal(err)
	}
	sink2, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 6, 3)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(atSync) != "[0]" || stagedOnDisk(t, e, txA) != 3 {
		t.Fatalf("tx.json held %v at the syncs and %d after; want [0] and 3", atSync, stagedOnDisk(t, e, txA))
	}
	if _, err := sink2.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}

	// The ifEtag change while the completing body still syncs.
	atSync = nil
	release, syncing := make(chan struct{}), make(chan struct{})
	first := true
	syncFile = func(f *os.File) error {
		atSync = append(atSync, stagedOnDisk(t, e, txB))
		if first {
			first = false
			close(syncing)
			<-release
		}
		return f.Sync()
	}
	e.begin("todo", "phone", txB)
	sink, err = e.s.Put("todo", "phone", txB, key(t, "b.bin"), nil, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() { _, err := sink.Write([]byte("xyz")); wrote <- err }()
	<-syncing
	if _, err := e.s.Put("todo", "phone", txB, key(t, "b.bin"), ptr("e"), 3, 3); err != nil {
		t.Fatalf("Put of an ifEtag change: %v", err)
	}
	if len(atSync) != 2 || atSync[1] != 0 || stagedOnDisk(t, e, txB) != 3 {
		t.Errorf("tx.json held %v at the syncs and %d after; want a second sync before it counts 3", atSync, stagedOnDisk(t, e, txB))
	}
	close(release)
	<-wrote
	// ifEtag "e" cannot match a new document: a conflict, not
	// ErrIncomplete, shows the body was taken as complete.
	if _, err := e.s.Commit("todo", "phone", txB); !errors.As(err, new(*ConflictError)) {
		t.Errorf("Commit = %v, want a conflict", err)
	}
}

// R3: a body closed while its transaction ends holds no handle in the
// folder that the end removes.
func TestCloseWhileTheTransactionEnds(t *testing.T) {
	e := newEnv(t)
	busyRetries = 0
	t.Cleanup(func() { busyRetries = 10 })
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	a, unlock, err := e.s.lock("todo")
	if err != nil {
		t.Fatal(err)
	}
	synced := make(chan struct{})
	syncFile = func(f *os.File) error {
		defer close(synced)
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	closed := make(chan error, 1)
	go func() { closed <- sink.Close() }() // waits for the app's lock to record
	<-synced
	time.Sleep(100 * time.Millisecond) // the Close closes its file, then waits
	derr := e.s.discard(a, a.txs[txA])
	unlock()
	if derr != nil {
		t.Errorf("ending the transaction under a closing body: %v", derr)
	}
	if err := <-closed; !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Close = %v, want ErrNoTransaction", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "todo", ".wss", stagingName, stagingDir(txA))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the transaction's folder stays: %v", err)
	}
}
