package store

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// A file that cannot be read (another program holds it without sharing, or
// its permissions refuse the server) never stops the app: the reconcile logs
// and skips it, keeps what the index holds of it, and adds a new one once it
// can be read.
func TestReconcileSkipsAnUnreadableFile(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var logs []string
	e.s.SetLogf(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	})
	e.commitAll("todo", map[string]string{"held.md": "held", "other.md": "other"})
	rev, _ := e.s.Head("todo")

	locked := map[string]bool{}
	h := hashFile
	t.Cleanup(func() { hashFile = h })
	hashFile = func(path string) (string, int64, int64, error) {
		for name := range locked {
			if strings.HasSuffix(path, name) {
				return "", 0, 0, errors.New("the process cannot access the file")
			}
		}
		return h(path)
	}
	locked["held.md"], locked["new.md"] = true, true
	e.writeFile("todo", "held.md", "changed while held") // hashed again: and fails
	e.writeFile("todo", "new.md", "new")

	changed, now, err := e.s.Reconcile("todo")
	if err != nil || changed || !sameEtag(now, rev) {
		t.Fatalf("Reconcile = %v, %v, %v; want nothing changed and no error", changed, now, err)
	}
	l, _ := e.s.List("todo")
	if len(l.Docs) != 2 || l.Docs[0].Key.String() != "held.md" || l.Docs[0].Etag != etagOf("held") {
		t.Errorf("listing %+v, want held.md as the index held it", l.Docs)
	}
	mu.Lock()
	if len(logs) < 2 {
		t.Errorf("logged %v, want each unreadable file", logs)
	}
	mu.Unlock()
	// A commit, which reconciles first, still goes through.
	e.commitAll("todo", map[string]string{"third.md": "3"})

	// Once readable, both are taken in.
	delete(locked, "held.md")
	delete(locked, "new.md")
	if changed, _, err := e.s.Reconcile("todo"); err != nil || !changed {
		t.Fatalf("Reconcile = %v, %v; want the two files taken in", changed, err)
	}
	l, _ = e.s.List("todo")
	if len(l.Docs) != 4 || l.Docs[0].Etag != etagOf("changed while held") {
		t.Errorf("listing %+v", l.Docs)
	}
}

// A folder that cannot be read keeps the documents the index holds in it.
func TestReconcileKeepsAnUnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("folder permissions: Unix only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a/x.md": "x", "b.md": "b"})
	dir := e.file("todo", "a")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	changed, _, err := e.s.Reconcile("todo")
	if err != nil || changed {
		t.Fatalf("Reconcile = %v, %v; want nothing changed and no error", changed, err)
	}
	if l, _ := e.s.List("todo"); len(l.Docs) != 2 || len(l.Removed) != 0 {
		t.Errorf("listing %+v, want a/x.md kept", l)
	}
}

// A commit never touches a document the reconcile could not read: it may
// have been changed by hand, and a rename over it, blocked past the commit
// point, would write over that edit later. The commit is refused before its
// commit point, the transaction stays open, and once the file can be read
// the same commit conflicts with the edit.
func TestCommitRefusesAnUnreadableDocument(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"data.json": "{}", "dir/x.md": "x"})
	locked := true
	h := hashFile
	t.Cleanup(func() { hashFile = h })
	hashFile = func(path string) (string, int64, int64, error) {
		if locked && strings.HasSuffix(path, "data.json") {
			return "", 0, 0, errors.New("the process cannot access the file")
		}
		return h(path)
	}
	e.writeFile("todo", "data.json", `{"by":"hand"}`)

	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "data.json", ptr(etagOf("{}")), `{"v":2}`)
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("Commit = %v, want ErrUnreadable", err)
	}
	if _, err := os.Stat(e.file("todo", ".wss/journal.json")); !os.IsNotExist(err) {
		t.Errorf("a journal was written: %v", err)
	}
	// A transaction that does not touch it goes through.
	e.begin("todo", "tablet", txB)
	e.put("todo", "tablet", txB, "dir/y.md", nil, "y")
	if _, err := e.s.Commit("todo", "tablet", txB); err != nil {
		t.Errorf("Commit of another document = %v", err)
	}

	locked = false
	var conflict *ConflictError
	if _, err := e.s.Commit("todo", "phone", txA); !errors.As(err, &conflict) || *conflict.Current(key(t, "data.json")) != etagOf(`{"by":"hand"}`) {
		t.Fatalf("Commit once readable = %v, want a conflict naming the hand edit", err)
	}
	if b, _ := os.ReadFile(e.file("todo", "data.json")); string(b) != `{"by":"hand"}` {
		t.Errorf("data.json holds %q, want the hand edit", b)
	}
}

// Every operation that moves an app's revision is reported, the journal a
// failed commit left included, whichever operation applies it.
func TestOnMoveReportsEveryMove(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.md": "a"}) // loads the app
	var mu sync.Mutex
	var moves []string
	e.s.SetOnMove(func(app string) {
		mu.Lock()
		defer mu.Unlock()
		moves = append(moves, app)
	})
	heard := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(moves)
	}
	e.commitAll("todo", map[string]string{"b.md": "b"})
	if heard() != 1 {
		t.Fatalf("%d moves reported after a commit, want 1", heard())
	}
	if _, err := e.s.Head("todo"); err != nil || heard() != 1 {
		t.Fatalf("a read reported a move (%d) or failed: %v", heard(), err)
	}

	// A commit whose rename fails past the commit point leaves its journal.
	failing := true
	inject(t, func(op, path string) bool { return failing && op == "rename" })
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "c.md", nil, "c")
	if _, err := e.s.Commit("todo", "phone", txA); err == nil {
		t.Fatal("the commit went through")
	}
	n := heard()
	failing = false
	// The next operation, a read, applies it: the move is reported.
	if _, err := e.s.Head("todo"); err != nil {
		t.Fatal(err)
	}
	if heard() != n+1 {
		t.Errorf("%d moves reported after the journal was applied, want %d", heard(), n+1)
	}
}
