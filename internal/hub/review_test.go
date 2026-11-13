package hub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/store"
)

// handEdit changes data.json of app by hand, as a person would.
func (e *env) handEdit(t *testing.T, app, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, "apps", app, "data.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A hand edit that the store takes in inside a get (its open reconciles)
// moves the revision: every session of the app, the reader's included, and
// Deps.Changed hear of it at once.
func TestHandEditTakenInByAGetIsTold(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	e.writeDoc(t, "todo", "data.json", `{}`)
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)

	e.handEdit(t, "todo", `{"by":"hand"}`)
	a.HandleText(`{"t":"get","path":"","id":"data","type":"json"}`)
	if d := waitFrames(t, fa, "doc", 1)[0]; d["etag"] != store.Etag([]byte(`{"by":"hand"}`)) {
		t.Fatalf("doc %v, want the hand edit", d)
	}
	rev := e.revisionOf(t, "todo")
	for name, c := range map[string]struct {
		s  *protocol.Session
		fc *fakeChannel
	}{"the reader": {a, fa}, "the other session": {b, fb}} {
		if got := framesOf(&fakeChannel{texts: settle(t, c.s, c.fc)}, "changed"); len(got) != 1 || got[0]["revision"] != rev {
			t.Errorf("%s got %v, want one changed to %v", name, got, rev)
		}
	}
	if got := e.changesHeard(); len(got) != 1 || got[0] != "todo "+rev.(string) {
		t.Errorf("Changed heard %v", got)
	}
}

// So does one that a commit's own reconcile takes in, whether the commit
// then conflicts or goes through.
func TestHandEditTakenInByACommitIsTold(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		e := newEnv(t)
		idA, tokA := e.device(t, "todo")
		idB, tokB := e.device(t, "todo")
		e.writeDoc(t, "todo", "data.json", `{}`)
		etag := store.Etag([]byte(`{}`))
		a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
		b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
		e.handEdit(t, "todo", `{"by":"hand"}`)
		write(a, "data.json", &etag, []byte(`{"v":2}`))
		f := waitFrames(t, fa, "conflict", 1, "committed")[0]
		rev := e.revisionOf(t, "todo")
		if f["t"] != "conflict" || f["revision"] != rev {
			t.Fatalf("A got %v, want a conflict at %v", f, rev)
		}
		if got := framesOf(&fakeChannel{texts: settle(t, b, fb)}, "changed"); len(got) != 1 || got[0]["revision"] != rev {
			t.Errorf("B got %v, want one changed to %v", got, rev)
		}
		if n := countType(settle(t, a, fa), "changed"); n != 0 {
			t.Errorf("A got %d changed frames: its conflict names the revision", n)
		}
		if got := e.changesHeard(); len(got) != 1 {
			t.Errorf("Changed heard %v", got)
		}
	})
	t.Run("the link's conflict", func(t *testing.T) {
		e := newEnv(t)
		idB, tokB := e.device(t, "todo")
		e.writeDoc(t, "todo", "data.json", `{}`)
		b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
		e.handEdit(t, "todo", `{"by":"hand"}`)
		tx, _, err := e.hub.BeginLink("todo", newTx())
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Delete(keyOf("data.json"), ptr(store.Etag([]byte(`{}`)))); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(); err == nil {
			t.Fatal("the link's commit went through over a hand edit")
		}
		rev := e.revisionOf(t, "todo")
		if got := framesOf(&fakeChannel{texts: settle(t, b, fb)}, "changed"); len(got) != 1 || got[0]["revision"] != rev {
			t.Errorf("B got %v, want one changed to %v", got, rev)
		}
		if got := e.changesHeard(); len(got) != 1 {
			t.Errorf("Changed heard %v", got)
		}
	})
}

// One file that cannot be read (another program holds it unshared) keeps
// nobody out: hellos are welcomed, commits of other documents go through,
// and the log says why that file is left as it was.
func TestAnUnreadableFileKeepsTheAppWorking(t *testing.T) {
	e := newEnv(t)
	e.hub.d.Store.SetLogf(e.logs.printf)
	id, tok := e.device(t, "todo")
	rev := e.writeDoc(t, "todo", "data.json", `{}`)
	path := filepath.Join(e.dir, "apps", "todo", "notes.md")
	if err := os.WriteFile(path, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := holdUnshared(t, path)

	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(helloFrame("todo", id, tok))
	if w := waitFrames(t, fc, "welcome", 1, "error")[0]; w["t"] != "welcome" || w["revision"] != rev {
		t.Fatalf("hello answered %v, want a welcome at %s", w, rev)
	}
	write(s, "other.json", nil, []byte(`{}`))
	if f := waitFrames(t, fc, "committed", 1, "error")[0]; f["t"] != "committed" {
		t.Fatalf("commit answered %v", f)
	}
	if e.logs.String() == "" {
		t.Error("nothing logged about the file")
	}

	// Once released, the next hello takes it in.
	release()
	e.login(t, "192.168.1.9:5001", "todo", id, tok)
	if e.etagOf(t, "todo", "notes.md") == nil {
		t.Error("notes.md not taken in once readable")
	}
}

// A commit's answer leaves under the app's lock: a commit that another
// device makes right after it (here, from inside Deps.Changed) reaches the
// committing session as a changed that follows its committed, so its last
// frame names the newest revision.
func TestCommittedNeverFollowsANewerChanged(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	var fired atomic.Bool
	e.hookMu.Lock()
	e.onChange = func(app string) {
		if fired.CompareAndSwap(false, true) {
			write(b, "other.json", nil, []byte(`{}`))
		}
	}
	e.hookMu.Unlock()
	write(a, "data.json", nil, []byte(`{"v":1}`))
	waitFrames(t, fb, "committed", 1)
	waitFrames(t, fa, "committed", 1)
	var last any
	for _, f := range settle(t, a, fa) {
		switch f["t"] {
		case "changed", "committed", "welcome":
			last = f["revision"]
		}
	}
	if newest := e.revisionOf(t, "todo"); last != newest {
		t.Errorf("A's last revision frame names %v, want the newest %v", last, newest)
	}
}

// RemoveApp and a device's begin, put or delete never interleave: once the
// app is gone, nothing of it is left on disk, its staging included.
func TestRemoveAppLeavesNoStagingBehind(t *testing.T) {
	for round := range 30 {
		e := newEnv(t)
		id, _ := e.device(t, "todo")
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			tx := newTx()
			e.hub.Begin("todo", id, tx)
			if sink, perr := e.hub.Put("todo", id, tx, protocol.Key{ID: "a", Type: "md"}, nil, 1, 0); perr == nil {
				_, _ = sink.Write([]byte("a"))
				_ = sink.Close()
			}
			e.hub.Delete("todo", id, tx, protocol.Key{ID: "b", Type: "md"}, nil)
		})
		wg.Go(func() {
			<-start
			if err := e.hub.RemoveApp("todo"); err != nil {
				t.Errorf("RemoveApp: %v", err)
			}
		})
		close(start)
		wg.Wait()
		if _, err := os.Stat(filepath.Join(e.dir, "apps", "todo")); !os.IsNotExist(err) {
			t.Fatalf("round %d: the removed app's folder is back: %v", round, err)
		}
	}
}

// Removing a device, a server-kind one included, discards the transactions
// it had open, on every app.
func TestRemoveDeviceDiscardsItsTransactions(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	srv, srvToken := e.serverDevice(t)
	for _, app := range []string{"todo", "notes"} {
		s, fc := e.login(t, "192.168.1.9:5000", app, srv, srvToken)
		tx := newTx()
		s.HandleText(frame(obj{"t": "begin", "tx": tx}))
		s.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "a", "type": "md", "ifEtag": nil, "bytes": 2, "offset": 0}))
		s.HandleBinary([]byte("a"))
		waitFrames(t, fc, "began", 1)
	}
	if !e.hub.RemoveDevice(srv) {
		t.Fatal("RemoveDevice = false")
	}
	for _, app := range []string{"todo", "notes"} {
		waitFor(t, "the staging to go", func() bool {
			entries, _ := os.ReadDir(filepath.Join(e.dir, "apps", app, ".wss", "staging"))
			return len(entries) == 0
		})
	}
}

// A commit over a document changed by hand that the server cannot read (held
// unshared) is refused before the store's commit point, as internal: the
// transaction stays open, and once the file can be read the same commit
// conflicts with the edit, which nothing ever wrote over.
func TestCommitOverAnUnreadableHandEdit(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	e.writeDoc(t, "todo", "data.json", `{}`)
	etag := store.Etag([]byte(`{}`))
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	path := filepath.Join(e.dir, "apps", "todo", "data.json")
	e.handEdit(t, "todo", `{"by":"hand!"}`)
	release := holdUnshared(t, path)

	tx := newTx()
	a.HandleText(frame(obj{"t": "begin", "tx": tx}))
	a.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "data", "type": "json", "ifEtag": etag, "bytes": 7, "offset": 0}))
	a.HandleBinary([]byte(`{"v":2}`))
	a.HandleText(frame(obj{"t": "commit", "tx": tx}))
	f := waitFrames(t, fa, "committed", 1, "conflict", "error")[0]
	expectError(t, f, protocol.CodeInternal)
	if logs := e.logs.String(); !strings.Contains(logs, "cannot read now") || !strings.Contains(logs, "the transaction stays open") {
		t.Errorf("the log does not say that the commit waits for an unreadable file:\n%s", logs)
	}

	release()
	a.HandleText(frame(obj{"t": "commit", "tx": tx}))
	f = waitFrames(t, fa, "committed", 1, "conflict")[0]
	if f["t"] != "conflict" || committedEtag(f) != store.Etag([]byte(`{"by":"hand!"}`)) {
		t.Errorf("the commit once readable answered %v, want a conflict naming the hand edit", f)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"by":"hand!"}` {
		t.Errorf("data.json holds %q, want the hand edit", b)
	}
}

// A revision the store moves outside the hub's own calls (here a commit
// straight into the store; a journal applied again by any operation is
// reported the same way) is announced: the app's sessions get changed, and
// Deps.Changed hears it.
func TestStoreMovesAreAnnounced(t *testing.T) {
	e := newEnv(t)
	id, tok := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, tok)
	tx := newTx()
	if _, err := e.store.Begin("todo", "elsewhere", tx); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete("todo", "elsewhere", tx, keyOf("gone.md"), nil); err != nil {
		t.Fatal(err)
	}
	sink, err := e.store.Put("todo", "elsewhere", tx, keyOf("new.md"), nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sink.Write([]byte("n"))
	if _, err := e.store.Commit("todo", "elsewhere", tx); err != nil {
		t.Fatal(err)
	}
	rev := e.revisionOf(t, "todo")
	if c := waitFrames(t, fc, "changed", 1); c[0]["revision"] != rev {
		t.Errorf("changed %v, want %v", c, rev)
	}
	waitFor(t, "Deps.Changed", func() bool { return slices.Contains(e.changesHeard(), "todo "+rev.(string)) })
	if n := countType(settle(t, s, fc), "changed"); n != 1 {
		t.Errorf("%d changed frames, want 1", n)
	}
}

// A device the main's registry drops (Replicate), one a merged registry
// drops (Adopt), and an own device handed over that the main does not admit
// (ClearDevices) lose their open transactions with their sessions.
func TestDroppedDevicesLoseTheirTransactions(t *testing.T) {
	staging := func(e *env) int {
		entries, _ := os.ReadDir(filepath.Join(e.dir, "apps", "todo", ".wss", "staging"))
		return len(entries)
	}
	open := func(t *testing.T, e *env, id, token string) {
		t.Helper()
		s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
		tx := newTx()
		s.HandleText(frame(obj{"t": "begin", "tx": tx}))
		waitFrames(t, fc, "began", 1)
		if staging(e) != 1 {
			t.Fatalf("staging holds %d, want the transaction", staging(e))
		}
	}
	t.Run("Replicate", func(t *testing.T) {
		e := newEnv(t)
		e.hub.d.Role = func() string { return protocol.RoleSubordinate }
		e.hub.Replicate([]devices.Entry{{ID: "Dev0123456789ab", App: "todo", TokenHash: hashOf("tok-main")}})
		open(t, e, "Dev0123456789ab", "tok-main")
		e.hub.Replicate(nil)
		if staging(e) != 0 {
			t.Errorf("staging holds %d after the main dropped the device", staging(e))
		}
	})
	t.Run("Adopt", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		open(t, e, id, token)
		if _, err := e.hub.Adopt(nil, []devices.Tombstone{{ID: id, At: e.clock.Now()}}, devices.Self{ServerID: "srv-1"}); err != nil {
			t.Fatal(err)
		}
		if staging(e) != 0 {
			t.Errorf("staging holds %d after the merged registry removed the device", staging(e))
		}
	})
	t.Run("ClearDevices", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		open(t, e, id, token)
		if err := e.hub.ClearDevices(); err != nil {
			t.Fatal(err)
		}
		if staging(e) != 0 {
			t.Errorf("staging holds %d after the device was handed over and not admitted", staging(e))
		}
	})
}
