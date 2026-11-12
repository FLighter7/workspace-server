package upstream

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// What the subordinate's page asks of the link, on real servers linked over
// WebRTC: a pairing code of the main for a browser on the subordinate's
// computer (invite), and unlinking.

// The main issues an ordinary pairing code of its own for the subordinate:
// a browser pairs with the main with it, hears of the subordinate in the
// main's welcome, and is admitted there on the main's word. A server that
// is not linked, or whose main is out of reach, gets no code.
func TestInviteIssuesAWorkingCode(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if _, err := sub.link.Invite(ctx(t)); !errors.Is(err, ErrNotLinked) {
		t.Errorf("invite before linking: %v, want ErrNotLinked", err)
	}
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })

	before := time.Now()
	inv, err := sub.link.Invite(ctx(t))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	c, err := wsclient.ParseCode(inv.Code)
	if err != nil || c.ServerID != main.id.ServerID {
		t.Fatalf("the code %q (%v) is not the main's", inv.Code, err)
	}
	if !inv.ExpiresAt.After(before.Add(9*time.Minute)) || inv.ExpiresAt.After(time.Now().Add(10*time.Minute)) {
		t.Errorf("the code expires at %s, want in 10 minutes", inv.ExpiresAt)
	}
	if !main.logs.has(`issued a pairing code for a browser on the subordinate "Laptop"`) {
		t.Error("the main did not log the code it issued")
	}

	browser, err := wsclient.Dial(ctx(t), c.Target, "todo", wsclient.DialOptions{})
	if err != nil {
		t.Fatalf("dial the main: %v", err)
	}
	t.Cleanup(browser.Close)
	p, w, err := browser.Pair(ctx(t), "", c.Secret, "Browser")
	if err != nil {
		t.Fatalf("pair with the invited code: %v", err)
	}
	if _, err := browser.Head(ctx(t)); err != nil { // confirms the pairing
		t.Fatal(err)
	}
	if got, want := w.Subordinates, []wsclient.Server{sub.self()}; !reflect.DeepEqual(got, want) {
		t.Errorf("the main's welcome lists %+v, want the subordinate", got)
	}
	waitFor(t, "the subordinate to admit the browser", func() bool { _, ok := sub.devices.Get(p.Device); return ok })
	sub.connect("todo", p.Device, p.Token)

	if err := main.l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to notice", func() bool { return !sub.link.Status().Connected })
	var re *RefusedError
	if _, err := sub.link.Invite(ctx(t)); !errors.As(err, &re) || re.Reason != ReasonUnreachable {
		t.Errorf("invite with the main away: %v, want %s", err, ReasonUnreachable)
	}
}

// Unlinking stops the link, deletes upstream.json, stops admitting the
// main's devices (closing their sessions) and makes the server a main
// again; the main forgets the server and stops handing it out. The data
// stays on both sides, and the server can link again.
func TestUnlink(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes"}); err != nil {
		t.Fatal(err)
	}
	main.hub.AppsChanged()
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the pull", func() bool { return sub.etag("todo") == v1 })
	id, token := main.pair("todo")
	waitFor(t, "the subordinate to admit the device", func() bool { _, ok := sub.devices.Get(id); return ok })
	tab := sub.connect("todo", id, token)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })

	forgotten, err := sub.link.Unlink(ctx(t))
	if err != nil || !forgotten {
		t.Fatalf("unlink: forgotten %v, %v; want the main told", forgotten, err)
	}
	if _, err := os.Stat(sub.linkDeps.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upstream.json after unlinking: %v", err)
	}
	if role := sub.link.Role(); role != RoleMain {
		t.Errorf("the role after unlinking is %q", role)
	}
	if st := sub.link.Status(); st.Linked || st.Connected || st.Halted || st.Devices != 0 || len(st.Apps) != 0 {
		t.Errorf("the status after unlinking %+v", st)
	}
	if list := sub.devices.Replica(); len(list) != 0 {
		t.Errorf("the replica after unlinking %+v", list)
	}
	select {
	case <-tab.cl.Done():
	case <-ctx(t).Done():
		t.Fatal("the main's device kept its session on the unlinked server")
	}
	if _, err := sub.tryConnect("todo", id, token); !wsclient.HasCode(err, "badToken") {
		t.Errorf("the main's device on the unlinked server: %v, want badToken", err)
	}
	if slices.ContainsFunc(main.devices.List(), devices.Device.IsServer) {
		t.Errorf("the main still knows the server: %+v", main.devices.List())
	}
	if phone := main.device("todo"); phone.welcome.Subordinates == nil || len(phone.welcome.Subordinates) != 0 {
		t.Errorf("the main still hands out %+v", phone.welcome.Subordinates)
	}

	// The data stays, and nothing moves any more.
	if sub.etag("todo") != v1 {
		t.Errorf("the unlinked server's document %q, want %q", sub.etag("todo"), v1)
	}
	if _, ok := sub.apps.Get("notes"); !ok {
		t.Error("the app taken from the main left the unlinked server's list")
	}
	connects := sub.link.connectCount()
	v2 := main.write("todo", `{"v":2}`, &v1)
	time.Sleep(300 * time.Millisecond)
	if sub.etag("todo") != v1 || sub.link.connectCount() != connects {
		t.Errorf("the unlinked server still syncs: etag %q, connects %d → %d", sub.etag("todo"), connects, sub.link.connectCount())
	}
	if _, err := sub.link.Unlink(ctx(t)); !errors.Is(err, ErrNotLinked) {
		t.Errorf("a second unlink: %v, want ErrNotLinked", err)
	}

	sub.linkTo(main)
	waitFor(t, "the pull after linking again", func() bool { return sub.etag("todo") == v2 })
}

// A main out of reach is not told: the server unlinks all the same, and the
// main keeps the server-kind device until it is removed on its page. A
// server the main had removed already counts as forgotten.
func TestUnlinkWhileTheMainIsAway(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })
	if err := main.l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to notice", func() bool { return !sub.link.Status().Connected })

	forgotten, err := sub.link.Unlink(ctx(t))
	if err != nil || forgotten {
		t.Fatalf("unlink: forgotten %v, %v; want unlinked, the main not told", forgotten, err)
	}
	if _, err := os.Stat(sub.linkDeps.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upstream.json after unlinking: %v", err)
	}
	if !slices.ContainsFunc(main.devices.List(), devices.Device.IsServer) {
		t.Error("the main lost the server-kind device it was never told about")
	}

	main.listen()
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })
	for _, d := range main.devices.List() {
		if d.IsServer() {
			main.hub.RemoveDevice(d.ID)
		}
	}
	waitFor(t, "the link to halt", func() bool { return sub.link.Status().Halted })
	if forgotten, err := sub.link.Unlink(ctx(t)); err != nil || !forgotten {
		t.Errorf("unlinking a removed server: forgotten %v, %v; want forgotten", forgotten, err)
	}
}
