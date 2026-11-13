package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/hub"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// Divergence (diverge.go): the subordinate keeps its own set, the app stops
// syncing with the main, every device of the app on either server is asked,
// and the first choice makes one side's set the other's, whole.

// What the link logs when a choice is carried out.
const (
	logKeptMain = "the main's set replaced this server's, as a device chose"
	logKeptSub  = "this server's set replaced the main's, as a device chose"
	logSettled  = "the divergence is settled"
)

// divergenceFile reads n's diverged.json of app; ok is false when there is
// none.
func (n *node) divergenceFile(app string) (divergence, bool) {
	n.t.Helper()
	raw, err := n.store.ReadSide(app, divergedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return divergence{}, false
	}
	if err != nil {
		n.t.Fatal(err)
	}
	var d divergence
	if err := json.Unmarshal(raw, &d); err != nil {
		n.t.Fatal(err)
	}
	return d, true
}

// lastDiverged is the list of the last diverged push d heard, and whether it
// heard one.
func (d *device) lastDiverged() ([]wsclient.DivergedServer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.diverged) == 0 {
		return nil, false
	}
	return d.diverged[len(d.diverged)-1], true
}

// hearsDiverged reports whether the last diverged push d heard lists the
// server id alone.
func (d *device) hearsDiverged(id string) bool {
	list, ok := d.lastDiverged()
	return ok && len(list) == 1 && list[0].ServerID == id
}

// hearsNoDivergence reports whether d heard a diverged push, and the last
// one lists none.
func (d *device) hearsNoDivergence() bool {
	list, ok := d.lastDiverged()
	return ok && len(list) == 0
}

func (d *device) heardResolved(server string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.resolved {
		if s == server {
			n++
		}
	}
	return n
}

// resolve has d choose keep for the divergence of server.
func (d *device) resolve(t *testing.T, server, keep string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	if err := d.cl.Resolve(ctx, server, keep); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

// diverge links sub to main, both holding data.json at v1, then has a
// device on each write data.json differently while the link is down, and
// waits for the divergence. It returns the devices, on the main and here,
// and the two documents: the main's and this server's.
func diverge(t *testing.T, main, sub *node) (phone, tab *device, onMain, here string) {
	t.Helper()
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	phone, tab = main.device("todo"), sub.device("todo")
	sub.link.Close()
	onMain = phone.put(t, &v1, `{"v":2}`)
	here = tab.put(t, &v1, `{"v":1,"laptop":true}`)
	sub.openLink()
	waitFor(t, "the divergence", func() bool { _, ok := sub.divergenceFile("todo"); return ok })
	return phone, tab, onMain, here
}

// Both changed the same document: the subordinate keeps its own, records
// the divergence, and stops syncing that app with the main; other apps go
// on. Every device of the app on both servers hears it, a welcome lists it,
// and nothing is written either way.
func TestDivergenceOnTheSameKey(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes"}); err != nil {
		t.Fatal(err)
	}
	phone, tab, onMain, here := diverge(t, main, sub)

	d, _ := sub.divergenceFile("todo")
	if d.Reason != reasonChanged || !slices.Equal(d.Keys, []string{dataKey}) || d.Main.ServerID != main.id.ServerID || d.Main.Name != "Desk" || d.Keep != "" {
		t.Errorf("diverged.json %+v, want data.json changed on both sides, with the main named", d)
	}
	waitFor(t, "the main's devices to hear of it", func() bool { return phone.hearsDiverged(sub.id.ServerID) })
	waitFor(t, "the devices here to hear of it", func() bool { return tab.hearsDiverged(sub.id.ServerID) })
	if w := main.device("todo").welcome; len(w.Diverged) != 1 || w.Diverged[0].ServerID != sub.id.ServerID || w.Diverged[0].Name != "Laptop" {
		t.Errorf("the main's welcome lists %+v, want the laptop", w.Diverged)
	}
	if w := sub.device("todo").welcome; len(w.Diverged) != 1 || w.Diverged[0].ServerID != sub.id.ServerID {
		t.Errorf("the subordinate's welcome lists %+v, want itself", w.Diverged)
	}
	if st := sub.link.Status(); !slices.Equal(st.Diverged, []string{"todo"}) {
		t.Errorf("status lists %v diverged, want todo", st.Diverged)
	}
	if !sub.logs.has("this server keeps its own") {
		t.Error("the divergence was not logged")
	}

	// Paused: nothing moves either way, while notes goes on.
	dev := sub.link.snapshot().Device
	waitFor(t, "the subordinate's session of todo to the main to end", func() bool {
		return !slices.ContainsFunc(main.hub.Sessions(), func(si hub.SessionInfo) bool { return si.App == "todo" && si.Device == dev })
	})
	l2 := tab.put(t, &here, `{"v":1,"laptop":2}`)
	v3 := phone.put(t, &onMain, `{"v":3}`)
	notes := main.write("notes", `{"n":1}`, nil)
	waitFor(t, "notes to go on syncing", func() bool { return sub.etag("notes") == notes })
	time.Sleep(300 * time.Millisecond)
	if main.etag("todo") != v3 || sub.etag("todo") != l2 {
		t.Errorf("todo moved while it waited for a choice: %s on the main, %s here", main.etag("todo"), sub.etag("todo"))
	}
}

// A first link with two different sets that are both non-empty diverges too;
// nothing is written on either side.
func TestFirstLinkWithTwoSets(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	u := main.write("todo", `{"main":1}`, nil)
	l := sub.write("todo", `{"laptop":1}`, nil)
	only := sub.writeKey("todo", "only-here.md", "mine", nil)
	tab := sub.device("todo")
	mainWritten := main.written("todo")

	sub.linkTo(main)
	waitFor(t, "the divergence", func() bool { _, ok := sub.divergenceFile("todo"); return ok })
	if d, _ := sub.divergenceFile("todo"); d.Reason != reasonFirstLink {
		t.Errorf("diverged.json %+v, want a first link", d)
	}
	waitFor(t, "the device here to hear of it", func() bool { return tab.hearsDiverged(sub.id.ServerID) })
	if !maps.Equal(sub.set("todo"), map[string]string{dataKey: l, "only-here.md": only}) || main.etag("todo") != u || !main.written("todo").Equal(mainWritten) {
		t.Error("a first link that diverged wrote something")
	}
	if !sub.logs.has("never agreed") {
		t.Error("the log does not say that the two were never agreed on")
	}
}

// A device of the subordinate keeps the main's set: the subordinate's set
// becomes the main's, documents only it held deleted; the divergence ends
// on both servers, and the app syncs again.
func TestChoiceOnTheSubordinateKeepsTheMain(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, tab, onMain, _ := diverge(t, main, sub)
	tab.putKey(t, "only-here.md", nil, "mine")

	tab.resolve(t, sub.id.ServerID, protocol.KeepMain)
	want := map[string]string{dataKey: onMain}
	waitFor(t, "the main's set here", func() bool { return maps.Equal(sub.set("todo"), want) })
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	waitFor(t, "the main's device to hear resolved", func() bool { return phone.heardResolved(sub.id.ServerID) == 1 && phone.hearsNoDivergence() })
	waitFor(t, "the main to forget it", func() bool { return len(main.hub.Divergences("todo")) == 0 })
	if !sub.logs.has(logKeptMain) || !sub.logs.has(logSettled) {
		t.Error("the choice was not logged")
	}
	if n := tab.heardResolved(sub.id.ServerID); n != 1 {
		t.Errorf("the choosing device heard %d resolved, want its answer only", n)
	}

	// Syncing again: the main's next write comes here.
	v3 := phone.put(t, &onMain, `{"v":3}`)
	waitFor(t, "the pull after the divergence", func() bool { return sub.etag("todo") == v3 })
}

// A device of the subordinate keeps its own set: the main's becomes the
// subordinate's, documents only the main held deleted; the main's devices
// hear of it, and the push counts against the write guard.
func TestChoiceOnTheSubordinateKeepsItsOwn(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, tab, _, here := diverge(t, main, sub)
	main.writeKey("todo", "only-there.md", "theirs", nil)

	tab.resolve(t, sub.id.ServerID, protocol.KeepSubordinate)
	want := map[string]string{dataKey: here}
	waitFor(t, "this server's set on the main", func() bool { return maps.Equal(main.set("todo"), want) })
	waitFor(t, "the agreed set", func() bool { return sub.agreedOn("todo", want) })
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	waitFor(t, "the main's device to hear of it", func() bool {
		return phone.heardResolved(sub.id.ServerID) == 1 && phone.heardCount() > 0
	})
	if !sub.logs.has(logKeptSub) {
		t.Error("the choice was not logged")
	}
	if n := sub.link.pushCount("todo"); n != 1 {
		t.Errorf("the write guard counts %d pushes, want the one that carried the choice out", n)
	}
	if !maps.Equal(sub.set("todo"), want) {
		t.Errorf("the subordinate's set changed: %v", sub.set("todo"))
	}
}

// A device of the main chooses, with the subordinate connected: the choice
// reaches it over its link session at once.
func TestChoiceOnTheMain(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, tab, _, here := diverge(t, main, sub)
	waitFor(t, "the main to know the divergence", func() bool { return phone.hearsDiverged(sub.id.ServerID) })

	phone.resolve(t, sub.id.ServerID, protocol.KeepSubordinate)
	want := map[string]string{dataKey: here}
	waitFor(t, "this server's set on the main", func() bool { return maps.Equal(main.set("todo"), want) })
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	waitFor(t, "the devices here to hear resolved", func() bool { return tab.heardResolved(sub.id.ServerID) == 1 && tab.hearsNoDivergence() })
	if d := sub.logs.has("a device of the main chose"); !d {
		t.Error("the choice from the main was not logged")
	}
	waitFor(t, "the main to forget it", func() bool { return len(main.hub.Divergences("todo")) == 0 })
	if _, err := main.store.ReadSide("todo", "divergences.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the main's divergences.json: %v, want it gone", err)
	}
}

// A choice made on the main while the subordinate is away waits there, a
// restart of the main included, and is carried out once the subordinate is
// back.
func TestChoiceOnTheMainWhileTheSubordinateIsAway(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, _, onMain, _ := diverge(t, main, sub)
	waitFor(t, "the main to know the divergence", func() bool { return phone.hearsDiverged(sub.id.ServerID) })

	sub.link.Close()
	phone.resolve(t, sub.id.ServerID, protocol.KeepMain)
	main.restart()
	if list := main.hub.Divergences("todo"); len(list) != 0 {
		t.Errorf("the main lists %+v after the choice, want none waiting", list)
	}
	if d, _ := sub.divergenceFile("todo"); d.Keep != "" {
		t.Fatal("the subordinate took a choice while away")
	}

	sub.openLink()
	waitFor(t, "the main's set here", func() bool { return sub.etag("todo") == onMain })
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	waitFor(t, "the main to forget it", func() bool {
		_, err := main.store.ReadSide("todo", "divergences.json")
		return errors.Is(err, fs.ErrNotExist)
	})
}

// Two choices: one on the main while the subordinate is away, and then one
// on the subordinate, which comes first there. The subordinate's wins; the
// main's, when it arrives, is ignored, and every device hears resolved.
func TestTheFirstChoiceWins(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, tab, _, here := diverge(t, main, sub)
	waitFor(t, "the main to know the divergence", func() bool { return phone.hearsDiverged(sub.id.ServerID) })

	other := sub.device("todo")
	sub.link.Close()
	phone.resolve(t, sub.id.ServerID, protocol.KeepMain)
	tab.resolve(t, sub.id.ServerID, protocol.KeepSubordinate)
	// A third choice, after the first here: answered, and ignored.
	other.resolve(t, sub.id.ServerID, protocol.KeepMain)
	waitFor(t, "the other device here to hear the first choice", func() bool { return other.heardResolved(sub.id.ServerID) == 2 })
	sub.openLink()

	want := map[string]string{dataKey: here}
	waitFor(t, "this server's set on the main", func() bool { return maps.Equal(main.set("todo"), want) })
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	waitFor(t, "the main to forget it", func() bool {
		_, err := main.store.ReadSide("todo", "divergences.json")
		return errors.Is(err, fs.ErrNotExist)
	})
	if !maps.Equal(sub.set("todo"), want) {
		t.Errorf("the subordinate's set %v, want its own kept", sub.set("todo"))
	}
}

// Two devices choose at the same moment, one on each server: one choice
// wins, the two servers end with the same set, and both devices are
// answered.
func TestTwoChoicesAtOnce(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, tab, onMain, here := diverge(t, main, sub)
	waitFor(t, "the main to know the divergence", func() bool { return phone.hearsDiverged(sub.id.ServerID) })

	var wg sync.WaitGroup
	wg.Go(func() { phone.resolve(t, sub.id.ServerID, protocol.KeepMain) })
	wg.Go(func() { tab.resolve(t, sub.id.ServerID, protocol.KeepSubordinate) })
	wg.Wait()
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	waitFor(t, "the two to agree", func() bool {
		m, s := main.set("todo"), sub.set("todo")
		return maps.Equal(m, s) && (s[dataKey] == onMain || s[dataKey] == here) && sub.agreedOn("todo", s)
	})
	if sub.logs.count(logKeptMain)+sub.logs.count(logKeptSub) != 1 {
		t.Errorf("the choice was carried out %d times, want once", sub.logs.count(logKeptMain)+sub.logs.count(logKeptSub))
	}
}

// A restart of either server while the app waits for a choice keeps it
// waiting: the subordinate's diverged.json, and the main's record of it.
func TestRestartWhileDiverged(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, _, onMain, here := diverge(t, main, sub)
	waitFor(t, "the main to know the divergence", func() bool { return phone.hearsDiverged(sub.id.ServerID) })
	pulls, pushes := sub.logs.count(logPulled), sub.logs.count(logPushed)

	sub.restart()
	main.restart()
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })
	if w := sub.device("todo").welcome; len(w.Diverged) != 1 {
		t.Errorf("the subordinate's welcome after a restart lists %+v", w.Diverged)
	}
	if w := main.device("todo").welcome; len(w.Diverged) != 1 || w.Diverged[0].ServerID != sub.id.ServerID {
		t.Errorf("the main's welcome after a restart lists %+v", w.Diverged)
	}
	if main.etag("todo") != onMain || sub.etag("todo") != here || sub.logs.count(logPulled) != pulls || sub.logs.count(logPushed) != pushes {
		t.Error("something moved after the restarts")
	}

	main.device("todo").resolve(t, sub.id.ServerID, protocol.KeepMain)
	waitFor(t, "the main's set here", func() bool { return sub.etag("todo") == onMain })
}

// A choice cut off while it is carried out (the pull of a big document,
// here) goes on after a restart, from where it stopped.
func TestRestartWhileTheChoiceIsCarriedOut(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, tab, _, _ := diverge(t, main, sub)
	big := phone.putKey(t, "big.bin", nil, bigBody(3<<20))

	cut := make(chan struct{})
	var once sync.Once
	sub.link.setWrapPull(func(w io.Writer, cl *wsclient.Client) io.Writer {
		return &cutWriter{w: w, after: 1 << 20, cut: func() { once.Do(func() { cl.Close(); close(cut) }) }}
	})
	tab.resolve(t, sub.id.ServerID, protocol.KeepMain)
	<-cut
	sub.restart()

	waitFor(t, "the big document here", func() bool { return sub.etagOf("todo", "big.bin") == big })
	waitFor(t, "the divergence to end", func() bool { _, ok := sub.divergenceFile("todo"); return !ok })
	if !sub.logs.has("resumed") {
		t.Error("the pull started again from 0")
	}
}

// The quota is the main's: the subordinate takes it for the apps it adds
// and for those it has, and follows a change.
func TestQuotaFromTheMain(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes", Quota: 3 << 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := main.apps.SetQuota("todo", 2<<20); err != nil {
		t.Fatal(err)
	}
	sub.linkTo(main)
	waitFor(t, "the main's quotas", func() bool {
		n, _ := sub.apps.Get("notes")
		td, _ := sub.apps.Get("todo")
		return n.Quota == 3<<20 && td.Quota == 2<<20
	})
	if _, err := main.apps.SetQuota("notes", 5<<20); err != nil {
		t.Fatal(err)
	}
	main.hub.AppsChanged()
	waitFor(t, "the changed quota", func() bool { n, _ := sub.apps.Get("notes"); return n.Quota == 5<<20 })
	if !sub.logs.has("its quota is the main's now") {
		t.Error("the quota taken was not logged")
	}
}

// A planned switch of the main is refused while an app is diverged; a
// forced one goes through, and forgets the divergence from the main this
// server no longer follows.
func TestPlannedSwitchRefusedWhileDiverged(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	diverge(t, main, sub)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })
	if _, err := sub.link.Promote(ctx(t), false); !errors.Is(err, ErrDiverged) {
		t.Fatalf("a planned switch while diverged: %v, want ErrDiverged", err)
	}
	if sub.link.Role() != RoleSubordinate {
		t.Fatal("the refused switch changed the role")
	}
	if _, err := sub.link.Promote(ctx(t), true); err != nil {
		t.Fatalf("a forced switch: %v", err)
	}
	if _, ok := sub.divergenceFile("todo"); ok {
		t.Error("the new main kept its divergence from the main it no longer follows")
	}
	if list := sub.hub.Divergences("todo"); len(list) != 0 {
		t.Errorf("the new main lists %+v", list)
	}
}

// An app removed on the subordinate's page while the main lists it: its
// sync stops first, and what the link held of it goes with its folder. A
// change on the main meanwhile moves nothing (the deletion of everything the
// main holds was once pushed, from the old base); the app comes back with
// the main's next list as a first link, the empty set here taking the
// main's.
func TestRemovedAppOnTheSubordinate(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	other := main.writeKey("todo", "other.json", `{"o":1}`, nil)
	sub.linkTo(main)
	want := map[string]string{dataKey: v1, "other.json": other}
	waitFor(t, "the agreed set", func() bool { return maps.Equal(sub.set("todo"), want) && sub.agreedOn("todo", want) })

	if err := sub.hub.RemoveApp("todo"); err != nil {
		t.Fatal(err)
	}
	want["third.json"] = main.writeKey("todo", "third.json", `{"t":1}`, nil)
	time.Sleep(500 * time.Millisecond)
	if !maps.Equal(main.set("todo"), want) {
		t.Fatalf("the main's set %v after the app was removed on the subordinate, want %v", main.set("todo"), want)
	}
	if _, ok := sub.apps.Get("todo"); ok {
		t.Fatal("the removed app is listed again before the main's next list")
	}

	main.hub.AppsChanged()
	waitFor(t, "the app back, with the main's set", func() bool { return maps.Equal(sub.set("todo"), want) && sub.agreedOn("todo", want) })
	if !maps.Equal(main.set("todo"), want) {
		t.Errorf("the main's set %v, want %v", main.set("todo"), want)
	}
}

// An app removed on the subordinate while it waits for a choice: the
// divergence goes with it, here and on the main, and when the app comes
// back nothing is asked; the main's set is taken as at a first link.
func TestRemovedAppWhileDiverged(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	phone, _, onMain, _ := diverge(t, main, sub)
	waitFor(t, "the main to know the divergence", func() bool { return phone.hearsDiverged(sub.id.ServerID) })

	if err := sub.hub.RemoveApp("todo"); err != nil {
		t.Fatal(err)
	}
	if d := sub.link.OwnDiverged("todo"); d != nil {
		t.Errorf("the subordinate still lists its divergence: %+v", d)
	}
	waitFor(t, "the main to forget it", func() bool { return len(main.hub.Divergences("todo")) == 0 })
	waitFor(t, "the main's devices to hear it settled", func() bool { return phone.hearsNoDivergence() })

	main.hub.AppsChanged()
	waitFor(t, "the app back", func() bool { _, ok := sub.apps.Get("todo"); return ok })
	if w := sub.device("todo").welcome; len(w.Diverged) != 0 {
		t.Errorf("the subordinate asks %+v after the app came back", w.Diverged)
	}
	waitFor(t, "the main's set here", func() bool { return sub.etag("todo") == onMain })
	if main.etag("todo") != onMain || len(main.set("todo")) != 1 {
		t.Errorf("the main's set %v changed", main.set("todo"))
	}
}

// A planned switch waits for the whole family: a subordinate whose own
// copy is in step is refused while the main says that another subordinate
// diverged, and the main refuses to hand its role over meanwhile; once a
// device chose and it was carried out, neither refuses for that.
func TestPlannedSwitchWaitsForTheFamily(t *testing.T) {
	desk := startNode(t, "Desk")
	laptop := startNode(t, "Laptop")
	tablet := startNode(t, "Tablet")
	v1 := desk.write("todo", `{"v":1}`, nil)
	makeFamily(t, desk, laptop, tablet)
	waitFor(t, "both to hold the desk's document", func() bool { return laptop.agreed("todo", v1) && tablet.agreed("todo", v1) })
	phone, tab := desk.device("todo"), laptop.device("todo")

	laptop.link.Close()
	phone.put(t, &v1, `{"v":2}`)
	tab.put(t, &v1, `{"v":1,"laptop":true}`)
	laptop.openLink()
	waitFor(t, "the desk to know the laptop's divergence", func() bool { return phone.hearsDiverged(laptop.id.ServerID) })
	waitFor(t, "the tablet to hear of it", func() bool { return tablet.link.anyDivergence() })
	if _, err := tablet.link.Promote(ctx(t), false); !errors.Is(err, ErrDiverged) {
		t.Errorf("the tablet's planned switch: %v, want ErrDiverged", err)
	}
	if err := desk.hub.TakeOver(tablet.id.ServerID); !errors.Is(err, hub.ErrDiverged) {
		t.Errorf("the desk's hand-over: %v, want hub.ErrDiverged", err)
	}
	if tablet.link.Role() != RoleSubordinate || desk.link.Role() != RoleMain {
		t.Fatal("a refused switch changed a role")
	}

	phone.resolve(t, laptop.id.ServerID, protocol.KeepMain)
	waitFor(t, "the divergence to end", func() bool { _, ok := laptop.divergenceFile("todo"); return !ok })
	waitFor(t, "the tablet to hear it ended", func() bool { return !tablet.link.anyDivergence() })
	waitFor(t, "the desk to forget it", func() bool { return len(desk.hub.Divergences("todo")) == 0 })
}
