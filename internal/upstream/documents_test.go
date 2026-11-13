package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// The sync rule document by document, on real servers linked over WebRTC
// (see link_test.go): the three-way comparison with the base in link.json,
// deletes and their tombstones, the base across restarts and mains, and big
// bodies, streamed and resumed.

// tombstoned reports whether n's set of app holds a tombstone of ks.
func (n *node) tombstoned(app, ks string) bool {
	n.t.Helper()
	l, err := n.store.List(app)
	if err != nil {
		n.t.Fatal(err)
	}
	for _, r := range l.Removed {
		if r.Key.String() == ks {
			return true
		}
	}
	return false
}

// While the two cannot reach each other, the main changes one document and
// a device here another: the link pulls the one and pushes the other, and
// both end with both, agreed.
func TestBothSidesChangeDifferentDocuments(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	sub.link.Close()
	a := main.writeKey("todo", "notes/a.md", "from the desk", nil)
	b := tab.putKey(t, "notes/b.md", nil, "from the laptop")
	sub.openLink()

	want := map[string]string{dataKey: v1, "notes/a.md": a, "notes/b.md": b}
	waitFor(t, "both sets to hold both documents", func() bool {
		return maps.Equal(main.set("todo"), want) && maps.Equal(sub.set("todo"), want)
	})
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
	if sub.logs.count(logPulled) != 2 || sub.logs.count(logPushed) != 1 || sub.logs.has(logReplaced) {
		t.Errorf("pulls %d, pushes %d, replaced %v; want the first link's pull, then one pull and one push",
			sub.logs.count(logPulled), sub.logs.count(logPushed), sub.logs.has(logReplaced))
	}
}

// A delete goes both ways, as a delete: the side that takes it keeps a
// tombstone of the document, as for a device's delete.
func TestDeletesBothWays(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	x := main.writeKey("todo", "x.md", "x", nil)
	y := main.writeKey("todo", "y.md", "y", nil)
	sub.linkTo(main)
	waitFor(t, "the agreed set", func() bool {
		return sub.agreedOn("todo", map[string]string{dataKey: v1, "x.md": x, "y.md": y})
	})
	tab := sub.device("todo")

	tab.removeKey(t, "x.md", &x)
	waitFor(t, "the delete to reach the main", func() bool { return main.etagOf("todo", "x.md") == "" })
	if !main.tombstoned("todo", "x.md") {
		t.Error("the main keeps no tombstone of the document deleted on the subordinate")
	}

	main.removeKey("todo", "y.md", &y)
	waitFor(t, "the delete to reach the subordinate", func() bool { return sub.etagOf("todo", "y.md") == "" })
	if !sub.tombstoned("todo", "y.md") {
		t.Error("the subordinate keeps no tombstone of the document deleted on the main")
	}
	want := map[string]string{dataKey: v1}
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
	if !maps.Equal(main.set("todo"), want) || !maps.Equal(sub.set("todo"), want) {
		t.Errorf("sets %v and %v, want %v on both", main.set("todo"), sub.set("todo"), want)
	}
	if sub.logs.has(logReplaced) {
		t.Error("a delete was taken for a divergence")
	}
}

// A first link: two empty sets agree on an empty set, which link.json keeps;
// an empty side takes the other's set, whole; two different sets that are
// not empty diverge, and the main's set replaces this server's, documents
// only this server held included.
func TestFirstLinks(t *testing.T) {
	t.Run("both empty", func(t *testing.T) {
		main := startNode(t, "Desk")
		sub := startNode(t, "Laptop")
		sub.linkTo(main)
		waitFor(t, "an agreed empty set", func() bool { b, ok := sub.link.baseOf("todo"); return ok && len(b) == 0 })
		if lk := sub.linkFileOf("todo"); lk.Base == nil || len(lk.Base) != 0 {
			t.Errorf("link.json %+v, want an empty base", lk)
		}
	})
	t.Run("the subordinate empty", func(t *testing.T) {
		main := startNode(t, "Desk")
		sub := startNode(t, "Laptop")
		want := map[string]string{
			dataKey:             main.write("todo", `{"v":1}`, nil),
			"photos/2026/a.jpg": main.writeKey("todo", "photos/2026/a.jpg", "jpeg", nil),
		}
		sub.linkTo(main)
		waitFor(t, "the main's set here", func() bool { return maps.Equal(sub.set("todo"), want) && sub.agreedOn("todo", want) })
	})
	t.Run("the main empty", func(t *testing.T) {
		main := startNode(t, "Desk")
		sub := startNode(t, "Laptop")
		want := map[string]string{
			dataKey: sub.write("todo", `{"l":1}`, nil),
			"b.md":  sub.writeKey("todo", "b.md", "b", nil),
		}
		sub.linkTo(main)
		waitFor(t, "this server's set on the main", func() bool { return maps.Equal(main.set("todo"), want) && sub.agreedOn("todo", want) })
	})
	t.Run("both hold documents", func(t *testing.T) {
		main := startNode(t, "Desk")
		sub := startNode(t, "Laptop")
		want := map[string]string{dataKey: main.write("todo", `{"main":1}`, nil)}
		sub.write("todo", `{"laptop":1}`, nil)
		sub.writeKey("todo", "only-here.md", "mine", nil)
		sub.linkTo(main)
		waitFor(t, "the main's set to replace this server's", func() bool { return maps.Equal(sub.set("todo"), want) })
		waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
		if !maps.Equal(main.set("todo"), want) {
			t.Errorf("the main's set %v changed", main.set("todo"))
		}
		if !sub.logs.has("never agreed") || !sub.logs.has(logReplaced) {
			t.Error("the divergence was not logged")
		}
	})
}

// The base survives a restart: writes made on either side while the
// subordinate was down are pulled or pushed by it, not taken for a
// divergence.
func TestBaseAfterARestart(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	sub.link.Close()
	l2 := tab.put(t, &v1, `{"v":1,"laptop":true}`)
	a := main.writeKey("todo", "a.md", "a", nil)
	sub.restart()

	want := map[string]string{dataKey: l2, "a.md": a}
	waitFor(t, "both sides to take the other's write", func() bool {
		return maps.Equal(main.set("todo"), want) && maps.Equal(sub.set("todo"), want)
	})
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
	if sub.logs.has(logReplaced) {
		t.Error("the writes were taken for a divergence: the base did not survive the restart")
	}
}

// A base agreed with another main is no base: the files plan's decision 6.
// Here, a link.json that names another server: the two sets, both changed,
// are a first link's, and the main's set wins, where the base would have
// pushed this server's.
func TestBaseOfAnotherMain(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	sub.link.Close()
	lk := sub.linkFileOf("todo")
	lk.ServerID = "Zz0123456789"
	raw, _ := json.Marshal(lk)
	if err := sub.store.WriteSide("todo", linkFile, raw); err != nil {
		t.Fatal(err)
	}
	tab.put(t, &v1, `{"v":1,"laptop":true}`)
	sub.openLink()

	waitFor(t, "the main's set to win here", func() bool { return sub.etag("todo") == v1 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	if !sub.logs.has("agreed with another main") || !sub.logs.has(logReplaced) {
		t.Error("the base of another main was used, or not logged")
	}
	if lk := sub.linkFileOf("todo"); lk.ServerID != main.id.ServerID {
		t.Errorf("link.json names %s, want the main", lk.ServerID)
	}
}

// bigBody is n bytes that are not all alike.
func bigBody(n int) string {
	var b strings.Builder
	b.Grow(n)
	for i := 0; b.Len() < n; i++ {
		b.WriteString(time.Duration(i).String())
	}
	return b.String()[:n]
}

// A big document goes both ways, its body streamed: 20 MiB from the main,
// and another from the subordinate.
func TestBigDocuments(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	body := bigBody(20 << 20)
	big := main.writeKey("todo", "big/a.bin", body, nil)
	sub.linkTo(main)
	waitFor(t, "the pull of the big document", func() bool { return sub.etagOf("todo", "big/a.bin") == big })

	tab := sub.device("todo")
	other := tab.putKey(t, "big/b.bin", nil, body[1:]+"!")
	waitFor(t, "the push of the big document", func() bool { return main.etagOf("todo", "big/b.bin") == other })
	waitFor(t, "the agreed set", func() bool {
		return sub.agreedOn("todo", map[string]string{"big/a.bin": big, "big/b.bin": other})
	})
}

// cutWriter passes writes on, and cuts the connection off once after bytes
// have gone through.
type cutWriter struct {
	w     io.Writer
	n     int64
	after int64
	cut   func()
}

func (c *cutWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.cut != nil && c.n >= c.after {
		c.cut()
		c.cut = nil
	}
	return n, err
}

// cutReader passes reads on, and cuts the connection off once after bytes
// have gone through.
type cutReader struct {
	r     io.Reader
	n     int64
	after int64
	cut   func()
}

func (c *cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.cut != nil && c.n >= c.after {
		c.cut()
		c.cut = nil
	}
	return n, err
}

// A pull cut off in the middle of a body resumes from what the
// subordinate's transaction staged, not from the start.
func TestPullResumesAfterADrop(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	big := main.writeKey("todo", "big.bin", bigBody(6<<20), nil)
	var once sync.Once
	sub.link.setWrapPull(func(w io.Writer, cl *wsclient.Client) io.Writer {
		cw := io.Writer(w)
		once.Do(func() { cw = &cutWriter{w: w, after: 2 << 20, cut: cl.Close} })
		return cw
	})
	sub.linkTo(main)
	waitFor(t, "the pull", func() bool { return sub.etagOf("todo", "big.bin") == big })
	waitFor(t, "the pull to be logged", func() bool { return sub.logs.has(logPulled) })
	if !sub.logs.has("resumed, ") {
		t.Error("the pull started over instead of resuming")
	}
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", map[string]string{"big.bin": big}) })
	if lk := sub.linkFileOf("todo"); lk.Pull != nil {
		t.Errorf("link.json still records the pull: %+v", lk.Pull)
	}
}

// A push cut off in the middle of a body resumes from what the main's
// transaction staged, not from the start.
func TestPushResumesAfterADrop(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "an agreed empty set", func() bool { b, ok := sub.link.baseOf("todo"); return ok && len(b) == 0 })
	tab := sub.device("todo")
	cut := make(chan struct{})
	var once sync.Once
	sub.link.setWrapPush(func(r io.Reader, cl *wsclient.Client) io.Reader {
		cr := io.Reader(r)
		once.Do(func() {
			cr = &cutReader{r: r, after: 2 << 20, cut: func() {
				sub.cutOff(main, true)
				cl.Close()
				close(cut)
			}}
		})
		return cr
	})
	big := tab.putKey(t, "big.bin", nil, bigBody(6<<20))
	<-cut
	// What arrived is staged on the main before the subordinate comes back.
	device := sub.link.snapshot().Device
	waitFor(t, "the main to stage what arrived", func() bool {
		lk := sub.linkFileOf("todo")
		if lk.Push == nil {
			return false
		}
		staged, err := main.store.Begin("todo", device, lk.Push.Tx)
		return err == nil && len(staged) == 1 && staged[0].Received > 0
	})
	sub.cutOff(main, false)
	sub.link.NetworkChanged()
	waitFor(t, "the push", func() bool { return main.etagOf("todo", "big.bin") == big })
	waitFor(t, "the push to be logged", func() bool { return sub.logs.has(logPushed) })
	if !sub.logs.has("resumed, ") {
		t.Error("the push started over instead of resuming")
	}
}

// inStep holds while both sides are at the base, and not while a write here
// waits to reach the main.
func TestInStep(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the link to be in step", func() bool { return sub.agreed("todo", v1) && sub.link.inStep() })
	tab := sub.device("todo")

	release := make(chan struct{})
	pushing := make(chan struct{})
	var once sync.Once
	sub.link.setWrapPush(func(r io.Reader, _ *wsclient.Client) io.Reader {
		once.Do(func() { close(pushing); <-release })
		return r
	})
	v2 := tab.put(t, &v1, `{"v":2}`)
	<-pushing
	if sub.link.inStep() {
		t.Error("in step while a write here has not reached the main")
	}
	close(release)
	waitFor(t, "the push", func() bool { return main.etag("todo") == v2 })
	waitFor(t, "the link to be in step again", func() bool { return sub.agreed("todo", v2) && sub.link.inStep() })
	if !bytes.Equal([]byte(sub.etag("todo")), []byte(v2)) {
		t.Errorf("the subordinate holds %s, want %s", sub.etag("todo"), v2)
	}
}

// The rule itself, key by key, and the plan of a set.
func TestDecideAndCompare(t *testing.T) {
	e := func(s string) *string { return &s }
	for _, c := range []struct {
		base, here, main *string
		want             action
	}{
		{e("a"), e("a"), e("a"), agreed},
		{e("a"), e("b"), e("b"), agreed},
		{e("a"), e("a"), e("b"), pull},
		{e("a"), e("a"), nil, pull},
		{nil, nil, e("b"), pull},
		{e("a"), e("b"), e("a"), push},
		{e("a"), nil, e("a"), push},
		{nil, e("b"), nil, push},
		{e("a"), e("b"), e("c"), conflict},
		{e("a"), nil, e("c"), conflict},
		{nil, e("b"), e("c"), conflict},
	} {
		if got := decide(c.base, c.here, c.main); got != c.want {
			t.Errorf("decide(%v, %v, %v) = %v, want %v", deref(c.base), deref(c.here), deref(c.main), got, c.want)
		}
	}

	p := compare(map[string]string{"a": "1", "b": "1", "gone": "1"},
		map[string]string{"a": "2", "b": "1", "c": "1"},
		map[string]string{"a": "1", "b": "2", "d": "1"})
	if len(p.diverged) != 0 || len(p.pulls) != 2 || len(p.pushes) != 2 {
		t.Fatalf("plan %+v, want pulls of b and d, pushes of a and c", p)
	}
	if p.pulls[0].key != "b" || p.pulls[1].key != "d" || p.pushes[0].key != "a" || p.pushes[1].key != "c" {
		t.Errorf("plan %+v", p)
	}
	if next := nextBase(map[string]string{"a": "1", "gone": "1"}, p.agree, union(map[string]string{"a": "2"}, nil)); !maps.Equal(next, map[string]string{"a": "1"}) {
		t.Errorf("nextBase = %v, want a kept and gone dropped", next)
	}
	if p := compare(nil, map[string]string{"a": "1"}, map[string]string{"a": "2"}); !p.firstLink || len(p.diverged) != 1 {
		t.Errorf("a first link with different sets: %+v, want a divergence", p)
	}
	if p := compare(nil, map[string]string{"a": "1"}, map[string]string{"a": "1"}); len(p.diverged) != 0 || len(p.agree) != 1 {
		t.Errorf("a first link with the same sets: %+v, want them agreed", p)
	}
	if got := store.RevisionOf(map[string]string{}); got != nil {
		t.Errorf("the revision of an empty set is %v, want nil", *got)
	}
}

func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}
