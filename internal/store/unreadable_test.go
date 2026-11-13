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
