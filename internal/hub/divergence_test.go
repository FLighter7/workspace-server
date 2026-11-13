package hub

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/protocol"
)

// Divergences: a main keeps what its subordinates tell it over their link
// sessions, lists the divergences that wait for a choice to the app's
// sessions, takes the first device's choice and pushes it to the
// subordinate; a subordinate lists its own, and hands a choice to its link.

const laptopID = "Lp7tOp0123ab"

const since1 = "2026-11-13T09:30:00Z"

// linkedLaptop pairs the server-kind device Laptop, opens its link session
// and announces it, as a subordinate does.
func linkedLaptop(t *testing.T, e *env) (id, token string, s *protocol.Session, fc *fakeChannel) {
	t.Helper()
	id, token = e.serverDevice(t)
	s, fc = e.linkSession(t, "192.168.1.30:5000", id, token)
	s.HandleText(announceFrame(laptopInfo()))
	waitFrames(t, fc, "devices", 1)
	return id, token, s, fc
}

func divergedFrame(app string, since any) string {
	return frame(obj{"t": "diverged", "app": app, "since": since})
}

// laptopEntry is the subordinate Laptop in a diverged list.
func laptopEntry(since string) obj {
	return obj{"serverId": laptopID, "name": "Laptop", "since": since}
}

// lastDiverged is the servers of the last diverged frame fc got.
func lastDiverged(t *testing.T, fc *fakeChannel, n int) []any {
	t.Helper()
	return waitFrames(t, fc, "diverged", n)[n-1]["servers"].([]any)
}

func (e *env) subDivergencesFile(t *testing.T, app string) (divergencesDoc, bool) {
	t.Helper()
	raw, err := e.store.ReadSide(app, divergencesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return divergencesDoc{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc divergencesDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, true
}

// On a main: a subordinate tells its divergence; every session of the app
// hears of it, and a welcome lists it. The first device's choice wins: the
// others hear resolved and the list without it, the subordinate's link
// session gets the choice, and a later choice changes nothing.
func TestDivergenceOnAMain(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	_, _, link, fl := linkedLaptop(t, e)

	link.HandleText(divergedFrame("todo", since1))
	if got := lastDiverged(t, fa, 1); !reflect.DeepEqual(got, []any{laptopEntry(since1)}) {
		t.Fatalf("diverged %v, want the laptop", got)
	}
	lastDiverged(t, fb, 1)
	idC, tokC := e.device(t, "todo")
	_, fc := e.login(t, "192.168.1.9:5002", "todo", idC, tokC)
	if w := waitFrames(t, fc, "welcome", 1)[0]; !reflect.DeepEqual(w["diverged"], []any{laptopEntry(since1)}) {
		t.Errorf("welcome %v, want the laptop's divergence", w)
	}
	// Told again, unchanged: nothing new.
	link.HandleText(divergedFrame("todo", since1))
	settleLink(t, link, fl)
	if n := countType(settle(t, a, fa), "diverged"); n != 1 {
		t.Errorf("%d diverged frames after the same divergence told again, want 1", n)
	}

	a.HandleText(frame(obj{"t": "resolve", "server": laptopID, "keep": "subordinate"}))
	if f := waitFrames(t, fa, "resolved", 1)[0]; f["server"] != laptopID {
		t.Errorf("resolved %v", f)
	}
	if f := waitFrames(t, fb, "resolved", 1)[0]; f["server"] != laptopID {
		t.Errorf("the other device heard resolved %v", f)
	}
	if got := lastDiverged(t, fb, 2); len(got) != 0 {
		t.Errorf("diverged after the choice %v, want none", got)
	}
	if f := waitFrames(t, fl, "resolve", 1)[0]; !reflect.DeepEqual(f, obj{"t": "resolve", "app": "todo", "keep": "subordinate", "since": since1}) {
		t.Errorf("the subordinate got %v", f)
	}
	if doc, ok := e.subDivergencesFile(t, "todo"); !ok || len(doc.Servers) != 1 || doc.Servers[0].Keep != protocol.KeepSubordinate {
		t.Errorf("divergences.json %+v, want the choice kept", doc)
	}

	// A later choice: resolved to it alone, nothing to the others or to the
	// subordinate.
	b.HandleText(frame(obj{"t": "resolve", "server": laptopID, "keep": "main"}))
	waitFrames(t, fb, "resolved", 2)
	settleLink(t, link, fl)
	if n := countType(settle(t, a, fa), "resolved"); n != 1 {
		t.Errorf("the first device heard %d resolved, want 1", n)
	}
	if n := len(framesOf(fl, "resolve")); n != 1 {
		t.Errorf("the subordinate got %d choices, want 1", n)
	}
	if doc, _ := e.subDivergencesFile(t, "todo"); doc.Servers[0].Keep != protocol.KeepSubordinate {
		t.Errorf("the later choice replaced the first: %+v", doc)
	}
}

// A choice made while the subordinate is away waits for it: its next link
// session, telling the same divergence, gets it. Once it tells that none
// waits, the main forgets it.
func TestChoiceWaitsForTheSubordinate(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	id, token, link, _ := linkedLaptop(t, e)
	link.HandleText(divergedFrame("todo", since1))
	lastDiverged(t, fa, 1)
	link.Close()

	a.HandleText(frame(obj{"t": "resolve", "server": laptopID, "keep": "main"}))
	waitFrames(t, fa, "resolved", 1)

	again, fl := e.linkSession(t, "192.168.1.30:5001", id, token)
	again.HandleText(announceFrame(laptopInfo()))
	again.HandleText(divergedFrame("todo", since1))
	if f := waitFrames(t, fl, "resolve", 1)[0]; f["keep"] != "main" || f["since"] != since1 {
		t.Errorf("the subordinate got %v, want the choice made while it was away", f)
	}
	again.HandleText(divergedFrame("todo", nil))
	waitFor(t, "the main to forget the divergence", func() bool { _, ok := e.subDivergencesFile(t, "todo"); return !ok })
	if n := countType(settle(t, a, fa), "resolved"); n != 1 {
		t.Errorf("the device heard %d resolved, want 1: the choice was its own", n)
	}
}

// A device of the subordinate chose first: the subordinate tells that none
// waits, and the main's devices hear resolved and the list without it. A
// new divergence replaces one told before, choice and all.
func TestSettledOnTheSubordinate(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	_, _, link, fl := linkedLaptop(t, e)
	link.HandleText(divergedFrame("todo", since1))
	lastDiverged(t, fa, 1)

	link.HandleText(divergedFrame("todo", nil))
	if f := waitFrames(t, fa, "resolved", 1)[0]; f["server"] != laptopID {
		t.Errorf("resolved %v", f)
	}
	if got := lastDiverged(t, fa, 2); len(got) != 0 {
		t.Errorf("diverged %v, want none", got)
	}

	link.HandleText(divergedFrame("todo", since1))
	lastDiverged(t, fa, 3)
	a.HandleText(frame(obj{"t": "resolve", "server": laptopID, "keep": "main"}))
	waitFrames(t, fl, "resolve", 1)
	const since2 = "2026-11-13T11:00:00Z"
	link.HandleText(divergedFrame("todo", since2))
	if got := lastDiverged(t, fa, 5); !reflect.DeepEqual(got, []any{laptopEntry(since2)}) {
		t.Errorf("diverged %v, want the new divergence, waiting", got)
	}
	settleLink(t, link, fl)
	if n := len(framesOf(fl, "resolve")); n != 1 {
		t.Errorf("the subordinate got %d choices, want the first only: it was for another divergence", n)
	}
}

// Only a subordinate's link session tells a divergence; one of an app, or
// of another server of the family, is closed. An app this server does not
// serve, and a server that never announced itself, are ignored.
func TestDivergedRefusals(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	_ = a
	sid, stok := e.serverDevice(t)
	app, fApp := e.login(t, "192.168.1.30:5000", "todo", sid, stok)
	app.HandleText(divergedFrame("todo", since1))
	expectClosedWithError(t, fApp, protocol.CodeProtocol)

	silent, fs := e.linkSession(t, "192.168.1.30:5001", sid, stok)
	silent.HandleText(divergedFrame("todo", since1)) // never announced
	silent.HandleText(divergedFrame("chess", since1))
	settleLink(t, silent, fs)
	if fs.isClosed() || len(framesOf(fa, "diverged")) != 0 {
		t.Error("a divergence from a server that never announced was taken, or closed the session")
	}

	fam := devices.Entry{ID: "Fam0123456789ab", Kind: devices.KindServer, Name: "Tablet", TokenHash: hashOf("tok-fam")}
	e.hub.Replicate([]devices.Entry{fam})
	famS, famFc := e.linkSession(t, "192.168.1.40:5000", fam.ID, "tok-fam")
	famS.HandleText(divergedFrame("todo", since1))
	expectClosedWithError(t, famFc, protocol.CodeProtocol)
}

// Removing the subordinate drops its divergence: the app's sessions hear the
// list without it.
func TestRemovedSubordinateDropsItsDivergence(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	_, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	id, _, link, _ := linkedLaptop(t, e)
	link.HandleText(divergedFrame("todo", since1))
	lastDiverged(t, fa, 1)
	e.hub.RemoveDevice(id)
	if got := lastDiverged(t, fa, 2); len(got) != 0 {
		t.Errorf("diverged %v after the removal, want none", got)
	}
	if _, ok := e.subDivergencesFile(t, "todo"); ok {
		t.Error("divergences.json still names the removed subordinate")
	}
}

// ownDivergence stands in for a subordinate's link: its own divergence on
// todo, and the choices it takes.
type ownDivergence struct {
	mu      sync.Mutex
	waiting bool
	chosen  []string
}

func (o *ownDivergence) diverged(app string) *protocol.DivergedServer {
	o.mu.Lock()
	defer o.mu.Unlock()
	if app != "todo" || !o.waiting {
		return nil
	}
	return &protocol.DivergedServer{ServerID: "srv-1", Name: "Desk", Since: since1}
}

func (o *ownDivergence) choose(app, keep string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.chosen = append(o.chosen, keep)
	first := o.waiting
	o.waiting = false
	return first
}

// On a subordinate: its own divergence is in every welcome; the first
// choice goes to its link, the other devices hear resolved and the list
// without it, and a later choice is answered resolved alone.
func TestOwnDivergence(t *testing.T) {
	own := &ownDivergence{waiting: true}
	e := newEnv(t, func(e *env) {
		e.deps = func(d *Deps) {
			d.Role = func() string { return protocol.RoleSubordinate }
			d.OwnDiverged = own.diverged
			d.ChooseOwn = own.choose
		}
	})
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	self := obj{"serverId": "srv-1", "name": "Desk", "since": since1}
	if w := waitFrames(t, fa, "welcome", 1)[0]; !reflect.DeepEqual(w["diverged"], []any{self}) {
		t.Errorf("welcome %v, want this server's divergence", w)
	}

	a.HandleText(frame(obj{"t": "resolve", "server": "srv-1", "keep": "main"}))
	waitFrames(t, fa, "resolved", 1)
	if f := waitFrames(t, fb, "resolved", 1)[0]; f["server"] != "srv-1" {
		t.Errorf("the other device heard %v", f)
	}
	if got := lastDiverged(t, fb, 1); len(got) != 0 {
		t.Errorf("diverged %v after the choice, want none", got)
	}
	b.HandleText(frame(obj{"t": "resolve", "server": "srv-1", "keep": "subordinate"}))
	waitFrames(t, fb, "resolved", 2)
	if n := countType(settle(t, a, fa), "resolved"); n != 1 {
		t.Errorf("the first device heard %d resolved, want 1", n)
	}
	own.mu.Lock()
	defer own.mu.Unlock()
	if !reflect.DeepEqual(own.chosen, []string{"main", "subordinate"}) {
		t.Errorf("the link was handed %v", own.chosen)
	}
}

// A choice for a server that has no divergence waiting (here, a main asked
// about a server it does not know) is answered resolved, and nobody else
// hears anything.
func TestResolveWithNothingWaiting(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	a.HandleText(frame(obj{"t": "resolve", "server": laptopID, "keep": "main"}))
	waitFrames(t, fa, "resolved", 1)
	if got := settle(t, b, fb); countType(got, "resolved")+countType(got, "diverged") != 0 {
		t.Errorf("another device heard %v", got)
	}
}

// RemoveApp tells Deps.RemovingApp before anything is removed, and calls the
// function it returns once the app's folder is gone; an app that is not
// listed is not told.
func TestRemoveAppTellsTheLinkFirst(t *testing.T) {
	var calls []string
	e := newEnv(t, func(e0 *env) {
		e0.deps = func(d *Deps) {
			d.RemovingApp = func(app string) func() {
				_, err := os.Stat(filepath.Join(e0.dir, "apps", app, "data.json"))
				calls = append(calls, "removing "+app+" with its folder: "+strconv.FormatBool(err == nil))
				return func() {
					_, err := os.Stat(filepath.Join(e0.dir, "apps", app))
					calls = append(calls, "done, folder gone: "+strconv.FormatBool(os.IsNotExist(err)))
				}
			}
		}
	})
	e.writeDoc(t, "todo", "data.json", `{}`)
	if err := e.hub.RemoveApp("todo"); err != nil {
		t.Fatal(err)
	}
	if err := e.hub.RemoveApp("todo"); err == nil {
		t.Fatal("a second removal succeeded")
	}
	want := []string{"removing todo with its folder: true", "done, folder gone: true"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}
}

// TakeOver refuses while a subordinate's divergence waits for a choice, and
// while a choice made here has not reached it; once the subordinate says
// none waits, it goes through.
func TestTakeOverWaitsForDivergences(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	_, _, link, fl := linkedLaptop(t, e)
	link.HandleText(divergedFrame("todo", since1))
	lastDiverged(t, fa, 1)
	if err := e.hub.TakeOver(laptopID); !errors.Is(err, ErrDiverged) {
		t.Errorf("TakeOver while a divergence waits: %v, want ErrDiverged", err)
	}
	a.HandleText(frame(obj{"t": "resolve", "server": laptopID, "keep": "main"}))
	waitFrames(t, fa, "resolved", 1)
	if err := e.hub.TakeOver(laptopID); !errors.Is(err, ErrDiverged) {
		t.Errorf("TakeOver while a choice has not reached the subordinate: %v, want ErrDiverged", err)
	}
	link.HandleText(divergedFrame("todo", nil))
	settleLink(t, link, fl)
	if err := e.hub.TakeOver(laptopID); err != nil {
		t.Errorf("TakeOver once none waits: %v", err)
	}
}
