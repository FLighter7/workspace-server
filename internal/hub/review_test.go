package hub

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FLighter7/workspace-server/internal/apps"
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
