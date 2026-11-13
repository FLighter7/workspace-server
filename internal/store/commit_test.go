package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errInjected stands in for a crash: it is transient, so the journal stays
// and the next operation or start carries the commit through, as after a
// real crash.
var errInjected = fmt.Errorf("injected failure: %w", errTransient)

// inject makes the store's file operations fail where fail says, until the
// test ends. fail gets the operation's name and its path (a rename's target).
func inject(t *testing.T, fail func(op, path string) bool) {
	t.Helper()
	w, r, rm, ra := writeAtomic, renameFile, removeFile, removeAll
	t.Cleanup(func() { writeAtomic, renameFile, removeFile, removeAll = w, r, rm, ra })
	writeAtomic = func(path string, data []byte, perm os.FileMode) error {
		if fail("write", path) {
			return errInjected
		}
		return w(path, data, perm)
	}
	renameFile = func(from, to string) error {
		if fail("rename", to) {
			return errInjected
		}
		return r(from, to)
	}
	removeFile = func(path string) error {
		if fail("remove", path) {
			return errInjected
		}
		return rm(path)
	}
	removeAll = func(path string) error {
		if fail("removeAll", path) {
			return errInjected
		}
		return ra(path)
	}
}

// crashScenario stages, in txA of "phone", a replacement of old.json, a new
// new/n.json and the deletion of dir/sub/del.json, over a set that also
// holds keep.json.
func crashScenario(t *testing.T) *env {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"keep.json": "keep", "old.json": "old", "dir/sub/del.json": "del"})
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "old.json", ptr(etagOf("old")), "old2")
	e.put("todo", "phone", txA, "new/n.json", nil, "n")
	if err := e.s.Delete("todo", "phone", txA, key(t, "dir/sub/del.json"), ptr(etagOf("del"))); err != nil {
		t.Fatal(err)
	}
	return e
}

var (
	before = map[string]string{"keep.json": "keep", "old.json": "old", "dir/sub/del.json": "del"}
	after  = map[string]string{"keep.json": "keep", "old.json": "old2", "new/n.json": "n"}
)

// checkApplied checks that the commit of crashScenario is fully in place.
func (e *env) checkApplied() {
	e.t.Helper()
	wantDocs(e.t, e.docs("todo"), after)
	wss := filepath.Join(e.root, "todo", ".wss")
	if _, err := os.Stat(filepath.Join(wss, journalName)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("the journal stays: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(wss, stagingName)); len(entries) != 0 {
		e.t.Errorf("staging holds %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(e.root, "todo", "dir")); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("the emptied folders stay: %v", err)
	}
	l, _ := e.s.List("todo")
	if len(l.Removed) != 1 || l.Removed[0].Key.String() != "dir/sub/del.json" {
		e.t.Errorf("tombstones %+v", l.Removed)
	}
	docs := map[string]docEntry{}
	for _, d := range l.Docs {
		docs[d.Key.String()] = docEntry{Etag: d.Etag}
	}
	if !sameEtag(l.Revision, revisionOf(docs)) {
		e.t.Error("the revision does not match the documents")
	}
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrNoTransaction) {
		e.t.Errorf("the committed transaction is still open: %v", err)
	}
	// The index on disk agrees with a fresh look at the files.
	rev := l.Revision
	e.restart()
	if got, _ := e.s.Head("todo"); !sameEtag(got, rev) {
		e.t.Error("the revision changed across a restart")
	}
}

func TestCommitCrashAtEveryStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(op, path string) bool
		// applied: the failure came after the commit point.
		applied bool
	}{
		{"writing the journal", func(op, path string) bool {
			return op == "write" && filepath.Base(path) == journalName
		}, false},
		{"the first rename", func(op, path string) bool {
			return op == "rename" && strings.HasSuffix(filepath.ToSlash(path), "/old.json")
		}, true},
		{"the second rename", func(op, path string) bool {
			return op == "rename" && strings.HasSuffix(filepath.ToSlash(path), "/new/n.json")
		}, true},
		{"removing a deleted document", func(op, path string) bool {
			return op == "remove" && filepath.Base(path) == "del.json"
		}, true},
		// The index is a cache: a failure to write it fails nothing.
		{"writing the index", func(op, path string) bool {
			return op == "write" && filepath.Base(path) == indexName
		}, true},
		{"removing the staging folder", func(op, path string) bool {
			return op == "removeAll" && filepath.Base(path) == stagingDir(txA)
		}, true},
		{"removing the journal", func(op, path string) bool {
			return op == "remove" && filepath.Base(path) == journalName
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, restart := range []bool{true, false} {
				e := crashScenario(t)
				failing := true
				inject(t, func(op, path string) bool { return failing && tc.fail(op, path) })
				_, err := e.s.Commit("todo", "phone", txA)
				if tc.name == "writing the index" {
					if err != nil {
						t.Fatalf("Commit = %v, want it through with the index unsaved", err)
					}
				} else if !errors.Is(err, errInjected) {
					t.Fatalf("Commit = %v, want the injected failure", err)
				}
				failing = false
				if !tc.applied {
					// Before the commit point: nothing changed, and the
					// transaction is still open.
					if restart {
						e.restart()
					}
					wantDocs(t, e.docs("todo"), before)
					if _, err := e.s.Commit("todo", "phone", txA); err != nil {
						t.Fatalf("Commit again: %v", err)
					}
					e.checkApplied()
					continue
				}
				// After the commit point: a restart, or the next operation,
				// carries the commit through.
				if restart {
					e.restart()
				} else if _, err := e.s.Head("todo"); err != nil {
					t.Fatalf("the next operation: %v", err)
				}
				e.checkApplied()
			}
		})
	}
}

func TestReplayIsIdempotent(t *testing.T) {
	// The journal applied fully, then applied again (a crash right before
	// it was removed, which the failure above leaves): nothing changes, and
	// the same tombstone is not added twice.
	e := crashScenario(t)
	var saved []byte
	failing := true
	inject(t, func(op, path string) bool {
		if failing && op == "remove" && filepath.Base(path) == journalName {
			saved, _ = os.ReadFile(path)
			return true
		}
		return false
	})
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	failing = false
	for range 3 {
		if err := os.WriteFile(filepath.Join(e.root, "todo", ".wss", journalName), saved, 0o600); err != nil {
			t.Fatal(err)
		}
		e.s = e.open()
		e.checkApplied()
	}
}

func TestEmptyFoldersGo(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a/b/c/x.json": "x", "a/y.json": "y", "z.json": "z"})
	e.commitAll("todo", map[string]string{"a/b/c/x.json": "-"})
	if _, err := os.Stat(e.file("todo", "a/b")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a/b stays: %v", err)
	}
	if _, err := os.Stat(e.file("todo", "a")); err != nil {
		t.Errorf("a went with a/y.json in it: %v", err)
	}
	e.commitAll("todo", map[string]string{"a/y.json": "-", "z.json": "-"})
	if _, err := os.Stat(e.file("todo", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "todo", ".wss", indexName)); err != nil {
		t.Errorf("the app's own folder went: %v", err)
	}
	// A folder that holds a file that is no document stays.
	e.commitAll("todo", map[string]string{"k/d.json": "d"})
	e.writeFile("todo", "k/README", "mine")
	e.commitAll("todo", map[string]string{"k/d.json": "-"})
	if b, ok := e.readFile("todo", "k/README"); !ok || b != "mine" {
		t.Error("a file that is no document went")
	}
}

func TestCommitUnderAReader(t *testing.T) {
	e := newEnv(t)
	oldBody, newBody := strings.Repeat("o", 1<<20), strings.Repeat("n", 1<<19)
	e.commitAll("todo", map[string]string{"doc.bin": oldBody, "gone.bin": oldBody})
	f, _, err := e.s.Open("todo", key(t, "doc.bin"), etagOf(oldBody))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, _, err := e.s.Open("todo", key(t, "gone.bin"), etagOf(oldBody))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	head := make([]byte, 1000)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatal(err)
	}
	// A get is half way through both while a commit replaces one and
	// deletes the other.
	busyRetries = 0
	t.Cleanup(func() { busyRetries = 10 })
	e.commitAll("todo", map[string]string{"doc.bin": newBody, "gone.bin": "-"})
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(head)+string(rest) != oldBody {
		t.Error("the reader did not read the old body to the end")
	}
	if b, err := io.ReadAll(g); err != nil || string(b) != oldBody {
		t.Errorf("the reader of the deleted document: %d bytes, %v", len(b), err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"doc.bin": newBody})
	if _, _, err := e.s.Open("todo", key(t, "doc.bin"), etagOf(oldBody)); !errors.Is(err, ErrStale) {
		t.Errorf("Open of the old etag = %v, want ErrStale", err)
	}
}
