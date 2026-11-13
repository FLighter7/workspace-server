package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

// A program that opened a document without FILE_SHARE_DELETE (os.Open here)
// holds a commit back: the rename is tried again for a moment, and goes
// through once the handle closes.
func TestCommitWaitsForABriefHandle(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "1"})
	f, err := os.Open(e.file("todo", "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.Close()
	}()
	e.commitAll("todo", map[string]string{"a.json": "2"})
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "2"})
}

// A handle that stays fails the commit after the commit point; the journal
// stays, and the next operation once the handle is gone carries it through.
func TestCommitBlockedByAHandle(t *testing.T) {
	e := newEnv(t)
	busyRetries, busyDelay = 2, time.Millisecond
	t.Cleanup(func() { busyRetries, busyDelay = 10, 50*time.Millisecond })
	e.commitAll("todo", map[string]string{"a.json": "1"})
	f, err := os.Open(e.file("todo", "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.begin("todo", "dev", txA)
	e.put("todo", "dev", txA, "a.json", ptr(etagOf("1")), "2")
	if _, err := e.s.Commit("todo", "dev", txA); err == nil || !isBusy(err) {
		t.Fatalf("Commit under a handle = %v, want a sharing error", err)
	}
	if b, _ := e.readFile("todo", "a.json"); b != "1" {
		t.Errorf("the live file is %q", b)
	}
	if _, err := e.s.Head("todo"); err == nil {
		t.Error("the next operation went through with the handle still open")
	}
	f.Close()
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "2"})
	if _, err := e.s.Commit("todo", "dev", txA); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("the transaction is still open: %v", err)
	}
}
