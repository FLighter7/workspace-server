package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	txA = "tx-aaaaaaaaaaaaaaaa"
	txB = "tx-bbbbbbbbbbbbbbbb"
)

func (e *env) begin(app, device, tx string) []Staged {
	e.t.Helper()
	staged, err := e.s.Begin(app, device, tx)
	if err != nil {
		e.t.Fatalf("Begin(%s, %s): %v", device, tx, err)
	}
	return staged
}

func TestTransaction(t *testing.T) {
	e := newEnv(t)
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "data.json", nil, `{"v":1}`)
	e.put("todo", "phone", txA, "photos/p1.jpg", nil, "jpeg-1")
	e.put("todo", "phone", txA, "empty.txt", nil, "")
	// Nothing shows before the commit.
	if rev, _ := e.s.Head("todo"); rev != nil {
		t.Fatal("staged bodies show before the commit")
	}
	if _, ok := e.readFile("todo", "data.json"); ok {
		t.Fatal("a staged body is in place before the commit")
	}
	c, err := e.s.Commit("todo", "phone", txA)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := e.s.Head("todo")
	if c.Revision == nil || *c.Revision != *head {
		t.Errorf("committed revision %v, head %v", c.Revision, head)
	}
	if len(c.Docs) != 3 || c.Docs[0].Key.String() != "data.json" || *c.Docs[0].Etag != etagOf(`{"v":1}`) || *c.Docs[2].Etag != etagOf("") {
		t.Errorf("committed docs = %+v", c.Docs)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"data.json": `{"v":1}`, "photos/p1.jpg": "jpeg-1", "empty.txt": ""})
	// The transaction is gone, and so is its staging folder.
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("a second Commit = %v, want ErrNoTransaction", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.root, "todo", ".wss", "staging")); len(entries) != 0 {
		t.Errorf("staging holds %d entries after the commit", len(entries))
	}

	// Replace one, delete another, add a third, in one go.
	e.begin("todo", "phone", txB)
	e.put("todo", "phone", txB, "data.json", ptr(etagOf(`{"v":1}`)), `{"v":2}`)
	if err := e.s.Delete("todo", "phone", txB, key(t, "photos/p1.jpg"), ptr(etagOf("jpeg-1"))); err != nil {
		t.Fatal(err)
	}
	e.put("todo", "phone", txB, "photos/p2.jpg", nil, "jpeg-2")
	c, err = e.s.Commit("todo", "phone", txB)
	if err != nil {
		t.Fatal(err)
	}
	if c.Docs[1].Etag != nil {
		t.Errorf("a deleted document's etag %v, want nil", *c.Docs[1].Etag)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"data.json": `{"v":2}`, "photos/p2.jpg": "jpeg-2", "empty.txt": ""})
}

func TestEmptyTransaction(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "1"})
	before, _ := e.s.Head("todo")
	e.begin("todo", "phone", txA)
	c, err := e.s.Commit("todo", "phone", txA)
	if err != nil || c.Revision == nil || *c.Revision != *before || len(c.Docs) != 0 {
		t.Fatalf("Commit of nothing = %+v, %v", c, err)
	}
}

func TestResumeAtOffset(t *testing.T) {
	e := newEnv(t)
	body := strings.Repeat("0123456789", 100)
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "big.bin"), nil, int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(sink, body[:300]); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil { // the stream broke
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Commit of a partial body = %v, want ErrIncomplete", err)
	}
	staged := e.begin("todo", "phone", txA) // the device comes back
	if len(staged) != 1 || staged[0].Received != 300 || staged[0].Bytes != 1000 || staged[0].Key.String() != "big.bin" || staged[0].IfEtag != nil {
		t.Fatalf("Begin to resume = %+v", staged)
	}
	// Past what arrived, another size, or a key that has nothing: refused.
	for _, tc := range []struct {
		key           string
		bytes, offset int64
	}{{"big.bin", 1000, 301}, {"big.bin", 999, 200}, {"other.bin", 1000, 100}, {"big.bin", 1000, 1001}, {"big.bin", 1000, -1}} {
		if _, err := e.s.Put("todo", "phone", txA, key(t, tc.key), nil, tc.bytes, tc.offset); !errors.Is(err, ErrBadBody) {
			t.Errorf("Put(%s, %d, at %d) = %v, want ErrBadBody", tc.key, tc.bytes, tc.offset, err)
		}
	}
	// Resuming before what arrived rewrites from there; the hash covers the
	// kept part read back from disk.
	sink, err = e.s.Put("todo", "phone", txA, key(t, "big.bin"), nil, 1000, 250)
	if err != nil {
		t.Fatal(err)
	}
	if sink.Received() != 250 || sink.Complete() {
		t.Fatalf("resumed sink at %d", sink.Received())
	}
	if _, err := io.WriteString(sink, body[250:]); err != nil {
		t.Fatal(err)
	}
	if !sink.Complete() {
		t.Fatal("the sink is not complete")
	}
	if _, err := sink.Write([]byte("x")); !errors.Is(err, ErrBadBody) {
		t.Errorf("a byte past the size = %v, want ErrBadBody", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := e.s.Commit("todo", "phone", txA)
	if err != nil {
		t.Fatal(err)
	}
	if *c.Docs[0].Etag != etagOf(body) {
		t.Error("the resumed body's etag is wrong")
	}
	wantDocs(t, e.docs("todo"), map[string]string{"big.bin": body})
}

func TestOverflowAndRestart(t *testing.T) {
	e := newEnv(t)
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := sink.Write([]byte("12345")); n != 0 || !errors.Is(err, ErrBadBody) {
		t.Fatalf("Write past the size = %d, %v; want 0, ErrBadBody", n, err)
	}
	// Putting the key again replaces the sink; the old one is dead.
	sink2, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("1")); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("a write to a replaced sink = %v, want ErrNoTransaction", err)
	}
	if _, err := sink2.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.bin": "ab"})
}

func TestChangeIfEtagOnly(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "old"})
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "a.json", ptr(etagOf("stale")), "new")
	var conflict *ConflictError
	if _, err := e.s.Commit("todo", "phone", txA); !errors.As(err, &conflict) {
		t.Fatalf("Commit with a wrong ifEtag = %v, want a conflict", err)
	}
	head, _ := e.s.Head("todo")
	if conflict.Revision == nil || *conflict.Revision != *head || len(conflict.Docs) != 1 ||
		conflict.Docs[0].Key.String() != "a.json" || *conflict.Docs[0].Etag != etagOf("old") ||
		*conflict.Current(key(t, "a.json")) != etagOf("old") {
		t.Fatalf("conflict = %+v", conflict)
	}
	// The transaction stays: only the ifEtag changes, no byte is sent again.
	staged := e.begin("todo", "phone", txA)
	if len(staged) != 1 || staged[0].Received != 3 {
		t.Fatalf("staged after the conflict: %+v", staged)
	}
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.json"), ptr(etagOf("old")), 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !sink.Complete() || sink.Received() != 3 {
		t.Error("the sink of an ifEtag change is not complete")
	}
	if _, err := sink.Write([]byte("x")); !errors.Is(err, ErrBadBody) {
		t.Errorf("a byte into it = %v, want ErrBadBody", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "new"})
}

func TestConflicts(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "a", "b.json": "b"})
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "a.json", nil, "a2")                                // must not exist: it does
	e.put("todo", "phone", txA, "c.json", ptr(etagOf("c")), "c2")                   // must exist: it does not
	e.put("todo", "phone", txA, "b.json", ptr(etagOf("b")), "b2")                   // fine
	if err := e.s.Delete("todo", "phone", txA, key(t, "d.json"), nil); err != nil { // fine: absent
		t.Fatal(err)
	}
	var conflict *ConflictError
	if _, err := e.s.Commit("todo", "phone", txA); !errors.As(err, &conflict) {
		t.Fatalf("Commit = %v, want a conflict", err)
	}
	if len(conflict.Docs) != 2 || *conflict.Current(key(t, "a.json")) != etagOf("a") ||
		conflict.Docs[1].Key.String() != "c.json" || conflict.Docs[1].Etag != nil {
		t.Fatalf("conflict = %+v", conflict.Docs)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "a", "b.json": "b"})
	// A hand edit counts at commit: the index is reconciled first.
	e.begin("todo", "phone", txB)
	e.put("todo", "phone", txB, "b.json", ptr(etagOf("b")), "b3")
	e.writeFile("todo", "b.json", "edited by hand")
	if _, err := e.s.Commit("todo", "phone", txB); !errors.As(err, &conflict) || *conflict.Docs[0].Etag != etagOf("edited by hand") {
		t.Fatalf("Commit over a hand edit = %v", err)
	}
}

func TestOneTransactionPerDevice(t *testing.T) {
	e := newEnv(t)
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "a.json", nil, "a")
	e.begin("todo", "laptop", txB) // another device's transaction is its own
	e.put("todo", "laptop", txB, "b.json", nil, "b")

	// Another id from the phone discards its first.
	txC := "tx-cccccccccccccccc"
	if staged := e.begin("todo", "phone", txC); len(staged) != 0 {
		t.Fatalf("a new transaction stages %+v", staged)
	}
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Commit of the discarded one = %v, want ErrNoTransaction", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "todo", ".wss", "staging", stagingDir(txA))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the discarded transaction's folder stays: %v", err)
	}
	// Another device cannot touch one that is not its own.
	if _, err := e.s.Begin("todo", "phone", txB); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Begin of the laptop's id from the phone = %v, want ErrNoTransaction", err)
	}
	if _, err := e.s.Put("todo", "phone", txB, key(t, "x.json"), nil, 1, 0); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Put = %v", err)
	}
	if err := e.s.Delete("todo", "phone", txB, key(t, "x.json"), nil); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Delete = %v", err)
	}
	if _, err := e.s.Commit("todo", "phone", txB); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Commit = %v", err)
	}
	if err := e.s.Abort("todo", "phone", txB); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Abort = %v", err)
	}
	if _, err := e.s.Commit("todo", "laptop", txB); err != nil {
		t.Fatalf("the laptop's own commit: %v", err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"b.json": "b"})
	// The same device on another app has a transaction of its own.
	e.begin("notes", "phone", txA)
	if _, err := e.s.Commit("todo", "phone", txC); err != nil {
		t.Errorf("the phone's transaction on todo after one on notes: %v", err)
	}
}

func TestAbort(t *testing.T) {
	e := newEnv(t)
	e.begin("todo", "phone", txA)
	sink, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("123")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Abort("todo", "phone", txA); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("4")); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("a write after Abort = %v, want ErrNoTransaction", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("Close after Abort: %v", err)
	}
	if err := e.s.Abort("todo", "phone", txA); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("a second Abort = %v, want ErrNoTransaction", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.root, "todo", ".wss", "staging")); len(entries) != 0 {
		t.Errorf("staging holds %d entries after Abort", len(entries))
	}
}

func TestSweep(t *testing.T) {
	e := newEnv(t)
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "a.json", nil, "a")
	e.begin("todo", "laptop", txB)
	e.clock.add(StagingTTL / 2)
	// Activity keeps a transaction: the phone's is touched now.
	e.put("todo", "phone", txA, "b.json", nil, "b")
	// A stray left by a crash (no tx.json) goes at the first sweep.
	stray := filepath.Join(e.root, "todo", ".wss", "staging", "shim-0123456789abcdef")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Sweep(e.clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stray stays: %v", err)
	}
	e.clock.add(StagingTTL/2 + time.Second) // the laptop's is idle for a day and a second
	if err := e.s.Sweep(e.clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "laptop", txB); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("an expired transaction commits: %v", err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); err != nil {
		t.Errorf("an active transaction was swept: %v", err)
	}
	// Recover sweeps too, after a restart.
	e.begin("todo", "phone", txB)
	e.clock.add(StagingTTL)
	e.restart()
	if _, err := e.s.Commit("todo", "phone", txB); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("Recover did not sweep an expired transaction: %v", err)
	}
}

func TestRestartInTheMiddleOfAPut(t *testing.T) {
	e := newEnv(t)
	persistEvery = 100
	t.Cleanup(func() { persistEvery = 8 << 20 })
	body := strings.Repeat("abcdefghij", 50) // 500 bytes
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "small.json", nil, "{}")
	sink, err := e.s.Put("todo", "phone", txA, key(t, "big.bin"), ptr("x"), 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 350; i += 50 {
		if _, err := io.WriteString(sink, body[i:i+50]); err != nil {
			t.Fatal(err)
		}
	}
	// The process dies: the sink is never closed, and its handle goes with
	// the process. tx.json last recorded 300.
	sink.kill()
	e.s = e.open()
	staged := e.begin("todo", "phone", txA)
	if len(staged) != 2 || staged[0].Received != 2 || staged[1].Received != 300 || *staged[1].IfEtag != "x" {
		t.Fatalf("staged after a restart: %+v", staged)
	}
	sink2, err := e.s.Put("todo", "phone", txA, key(t, "big.bin"), nil, 500, 300)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(sink2, body[300:]); err != nil {
		t.Fatal(err)
	}
	if err := sink2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); err != nil {
		t.Fatal(err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"small.json": "{}", "big.bin": body})

	// A body complete before a restart commits after it, its etag taken
	// from the file.
	e.begin("todo", "phone", txB)
	e.put("todo", "phone", txB, "c.txt", nil, "complete")
	e.restart()
	if _, err := e.s.Commit("todo", "phone", txB); err != nil {
		t.Fatalf("Commit after a restart: %v", err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"small.json": "{}", "big.bin": body, "c.txt": "complete"})

	// A body file that lost its end in a crash counts only what it holds.
	e.begin("todo", "phone", txA)
	e.put("todo", "phone", txA, "d.txt", nil, "0123456789")
	bodyFile := filepath.Join(e.root, "todo", ".wss", "staging", stagingDir(txA), "b0")
	if err := os.Truncate(bodyFile, 4); err != nil {
		t.Fatal(err)
	}
	e.s = e.open()
	if staged := e.begin("todo", "phone", txA); staged[0].Received != 4 {
		t.Fatalf("received after losing the end: %+v", staged)
	}
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Commit = %v, want ErrIncomplete", err)
	}
}

func TestQuota(t *testing.T) {
	e := newEnv(t)
	e.quota = 1 << 20
	mib := int64(1 << 20)
	e.commitAll("todo", map[string]string{"a.bin": strings.Repeat("a", 600<<10)})

	e.begin("todo", "phone", txA)
	// A new 500 KiB document takes the set past 1 MiB: refused before a byte.
	if _, err := e.s.Put("todo", "phone", txA, key(t, "b.bin"), nil, 500<<10, 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("Put past the quota = %v, want ErrQuota", err)
	}
	// Replacing a.bin frees its size: a 1 MiB a.bin fits exactly.
	if _, err := e.s.Put("todo", "phone", txA, key(t, "a.bin"), ptr(etagOf(strings.Repeat("a", 600<<10))), mib, 0); err != nil {
		t.Fatalf("Put of a replacement that fits: %v", err)
	}
	// Deleting a.bin frees all of it for b.bin.
	if err := e.s.Delete("todo", "phone", txA, key(t, "a.bin"), ptr(etagOf(strings.Repeat("a", 600<<10)))); err != nil {
		t.Fatal(err)
	}
	e.put("todo", "phone", txA, "b.bin", nil, strings.Repeat("b", 900<<10))
	// Other changes count together.
	if _, err := e.s.Put("todo", "phone", txA, key(t, "c.bin"), nil, 200<<10, 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("Put of a third = %v, want ErrQuota", err)
	}

	// Commit checks again: another device fills the set meanwhile.
	e.begin("todo", "laptop", txB)
	e.put("todo", "laptop", txB, "z.bin", nil, strings.Repeat("z", 300<<10))
	if _, err := e.s.Commit("todo", "laptop", txB); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); !errors.Is(err, ErrQuota) {
		t.Fatalf("Commit past the quota = %v, want ErrQuota", err)
	}
	// The transaction stays open: deleting z.bin too makes it fit.
	if err := e.s.Delete("todo", "phone", txA, key(t, "z.bin"), ptr(etagOf(strings.Repeat("z", 300<<10)))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Commit("todo", "phone", txA); err != nil {
		t.Fatalf("Commit that fits: %v", err)
	}
	if st, _ := e.s.Stats("todo"); st.Bytes != 900<<10 || st.Files != 1 {
		t.Errorf("Stats = %+v", st)
	}
}
