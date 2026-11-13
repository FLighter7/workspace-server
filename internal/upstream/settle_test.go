package upstream

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// What a resumed transaction may reuse, what a commit settles in the base,
// and what a lost base record, a refused pull, a dropped app and a key not
// valid here do to the link (see sync.go).

// A push cut off, then the documents change before it resumes: the bodies
// staged for the old etags are never resumed as the new ones. Before, the
// record took the new etags before those bodies were staged again, and a
// second cut left an old body under a new etag, which the next attempt
// resumed: the main got a body mixed from both versions, and the base said
// it was the new one.
func TestPushResumeAfterTheBodiesChanged(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "an agreed empty set", func() bool { b, ok := sub.link.baseOf("todo"); return ok && len(b) == 0 })
	tab := sub.device("todo")

	const size = 3 << 20
	a1 := bigBody(size)
	a2 := "X" + a1[1:]
	b1 := strings.Repeat("1", size)
	b2 := strings.Repeat("2", size)

	var calls atomic.Int32
	cut1 := make(chan struct{})
	cut2 := make(chan struct{})
	sub.link.setWrapPush(func(r io.Reader, cl *wsclient.Client) io.Reader {
		switch calls.Add(1) {
		case 2: // b.bin in the first push
			return &cutReader{r: r, after: 1 << 20, cut: func() { sub.cutOff(main, true); cl.Close(); close(cut1) }}
		case 3: // a.bin in the second push
			return &cutReader{r: r, after: 64 << 10, cut: func() { sub.cutOff(main, true); cl.Close(); close(cut2) }}
		}
		return r
	})
	put2 := func(ifA, ifB *string, a, b string) {
		tab.commit(t, func(ctx context.Context, tx string) error {
			if _, err := tab.cl.Put(ctx, tx, wireKey("a.bin"), ifA, int64(len(a)), 0, strings.NewReader(a)); err != nil {
				return err
			}
			_, err := tab.cl.Put(ctx, tx, wireKey("b.bin"), ifB, int64(len(b)), 0, strings.NewReader(b))
			return err
		})
	}
	put2(nil, nil, a1, b1)
	<-cut1
	device := sub.link.snapshot().Device
	waitFor(t, "the main to stage a whole and b in part", func() bool {
		lk := sub.linkFileOf("todo")
		if lk.Push == nil {
			return false
		}
		staged, err := main.store.Begin("todo", device, lk.Push.Tx)
		return err == nil && len(staged) == 2 && staged[1].Received > 0 && staged[1].Received < size
	})
	ea1, eb1 := store.Etag([]byte(a1)), store.Etag([]byte(b1))
	put2(&ea1, &eb1, a2, b2)
	ea2, eb2 := store.Etag([]byte(a2)), store.Etag([]byte(b2))

	sub.cutOff(main, false)
	sub.link.NetworkChanged()
	<-cut2
	sub.cutOff(main, false)
	sub.link.NetworkChanged()

	want := map[string]string{"a.bin": ea2, "b.bin": eb2}
	waitFor(t, "the device's second bodies on the main", func() bool { return maps.Equal(main.set("todo"), want) })
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
	if !maps.Equal(sub.set("todo"), want) {
		t.Errorf("the subordinate holds %v, want %v", sub.set("todo"), want)
	}
	if sub.logs.has("otherwise than meant") {
		t.Error("a commit wrote a body other than the one meant")
	}
}

// reusable keeps a transaction only when every body it staged is recorded at
// the etag wanted now.
func TestReusable(t *testing.T) {
	e := func(s string) *string { return &s }
	k := func(s string) store.Key { k, _ := store.ParseKey(s); return k }
	staged := []store.Staged{
		{Key: k("a.bin"), Bytes: 10, Received: 5},
		{Key: k("b.bin"), Bytes: 10, Received: 10},
		{Key: k("c.bin"), Delete: true},
	}
	cs := []change{{key: "a.bin", to: e("e1")}, {key: "b.bin", to: e("e2")}, {key: "c.bin"}}
	targets, gone := targetsOf(cs)
	if !slices.Equal(gone, []string{"c.bin"}) {
		t.Errorf("gone %v", gone)
	}
	keep := reusable(staged, map[string]string{"a.bin": "e1", "b.bin": "e2"}, cs, targets)
	if len(keep) != 2 || keep["a.bin"].Received != 5 || keep["b.bin"].Received != 10 {
		t.Errorf("keep %v, want a and b", keep)
	}
	moved := []change{{key: "a.bin", to: e("e1")}, {key: "b.bin", to: e("e3")}, {key: "c.bin"}}
	targets, _ = targetsOf(moved)
	if keep := reusable(staged, map[string]string{"a.bin": "e1", "b.bin": "e2"}, moved, targets); keep != nil {
		t.Errorf("a body staged for e2 is kept for e3: %v", keep)
	}
	if keep := reusable(staged, map[string]string{"a.bin": "e1"}, cs, targets); keep != nil {
		t.Errorf("a body with no recorded etag is kept: %v", keep)
	}
	if keep := reusable(staged, map[string]string{"a.bin": "e1", "b.bin": "e2"}, cs[:1], targets); keep != nil {
		t.Errorf("a transaction that staged a change no longer wanted is kept: %v", keep)
	}
}

// The base takes what a commit reports, not what the link meant to write.
func TestSettleFromTheCommit(t *testing.T) {
	e := func(s string) *string { return &s }
	base := map[string]string{"b.bin": "old"}
	cs := []change{{key: "a.bin", to: e("e1")}, {key: "b.bin"}, {key: "c.bin", to: e("e3")}}
	odd := settle(base, cs, map[string]*string{"a.bin": e("e9"), "b.bin": nil, "c.bin": e("e3")})
	if !maps.Equal(base, map[string]string{"a.bin": "e9", "c.bin": "e3"}) {
		t.Errorf("base %v", base)
	}
	if !slices.Equal(odd, []string{"a.bin"}) {
		t.Errorf("odd %v, want a.bin", odd)
	}
}

// A push that committed while its base was not saved: link.json still holds
// the base before it, and the push's record. A device then changes the
// document here. The record shows the push landed, so the change is pushed,
// not taken for a divergence that the main's set would win.
func TestLostBaseRecord(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	sub.link.Close()
	v2 := tab.put(t, &v1, `{"v":2}`)
	main.write("todo", `{"v":2}`, &v1) // the push, landed
	v3 := tab.put(t, &v2, `{"v":3}`)   // the device's next change
	lk := sub.linkFileOf("todo")
	lk.Push = &pendingTx{Tx: newTx(), Etags: map[string]string{dataKey: v2}}
	if err := sub.link.saveLink("todo", &lk); err != nil {
		t.Fatal(err)
	}
	sub.openLink()

	waitFor(t, "the change to reach the main", func() bool { return main.etag("todo") == v3 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v3) })
	if sub.logs.has(logReplaced) || sub.etag("todo") != v3 {
		t.Error("the lost base record was taken for a divergence")
	}
}

// A pull this server's quota refuses does not hold its own changes back:
// they reach the main all the same.
func TestPushesGoOnWhenAPullIsRefused(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "an agreed empty set", func() bool { b, ok := sub.link.baseOf("todo"); return ok && len(b) == 0 })
	tab := sub.device("todo")

	sub.link.Close()
	main.writeKey("todo", "big.bin", bigBody(2<<20), nil)
	a := tab.putKey(t, "a.md", nil, "mine")
	raw := `{"apps":[{"id":"todo","name":"Todo","quota":1048576}]}`
	if err := os.WriteFile(filepath.Join(sub.dir, "apps.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.apps.ReloadIfChanged(); err != nil {
		t.Fatal(err)
	}
	if q, _ := sub.apps.Get("todo"); q.Quota != apps.MinQuota {
		t.Fatalf("the subordinate's quota is %d", q.Quota)
	}
	sub.openLink()

	waitFor(t, "this server's change on the main", func() bool { return main.etagOf("todo", "a.md") == a })
	waitFor(t, "the refused pull to be logged", func() bool { return sub.logs.has("quota") })
	if sub.etagOf("todo", "big.bin") != "" {
		t.Error("the pull past the quota went through")
	}
}

// An app the main stops serving forgets its base only once its worker has
// stopped: a sync in flight never saves link.json again after it.
func TestDroppedAppForgetsItsBase(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes"}); err != nil {
		t.Fatal(err)
	}
	n1 := main.writeKey("notes", "a.md", "a", nil)
	sub.linkTo(main)
	waitFor(t, "notes agreed", func() bool { return sub.agreedOn("notes", map[string]string{"a.md": n1}) })

	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sub.link.setWrapPull(func(w io.Writer, _ *wsclient.Client) io.Writer {
		once.Do(func() { close(blocked); <-release })
		return w
	})
	main.writeKey("notes", "b.md", "b", nil)
	<-blocked // notes' worker is in the middle of a pull
	if err := main.hub.RemoveApp("notes"); err != nil {
		t.Fatal(err)
	}
	main.hub.AppsChanged()
	time.Sleep(500 * time.Millisecond)
	if _, err := sub.store.ReadSide("notes", linkFile); err != nil {
		t.Errorf("notes' base went while its worker still ran: %v", err)
	}
	close(release)
	waitFor(t, "notes to stop syncing", func() bool { return sub.logs.has(`app "notes": the main no longer serves it`) })
	waitFor(t, "notes' base to go", func() bool {
		_, err := sub.store.ReadSide("notes", linkFile)
		return errors.Is(err, fs.ErrNotExist)
	})
	time.Sleep(300 * time.Millisecond)
	if _, err := sub.store.ReadSide("notes", linkFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("notes' base came back: %v", err)
	}
}

// The main's revision as the link compares it with its base: without the
// keys not valid here, for the revision it listed them at.
func TestComparableRevision(t *testing.T) {
	l, err := Open(Deps{Path: filepath.Join(t.TempDir(), "upstream.json")})
	if err != nil {
		t.Fatal(err)
	}
	full, other := "full", "other"
	kept := map[string]string{"a.md": strings.Repeat("a", 64)}
	l.setComparable("todo", &full, kept, 2)
	if got := l.comparable("todo", &full); !same(got, store.RevisionOf(kept)) {
		t.Errorf("comparable(full) = %v, want the revision of the keys valid here", got)
	}
	if got := l.comparable("todo", &other); got != &other {
		t.Error("a revision not listed is not taken as it is")
	}
	l.setComparable("todo", &full, kept, 1)
	if got := l.comparable("todo", &full); got != &full {
		t.Error("a listing with every key valid here still has a comparable revision")
	}
}
