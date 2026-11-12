package hub

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/protocol"
)

// A welcome names the family's term, and leaves it out at 0.
func TestWelcomeNamesTheTerm(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	_, fc := e.login(t, "192.168.1.10:5000", "todo", id, token)
	if w := framesOf(fc, "welcome")[0]; w["term"] != nil {
		t.Errorf("welcome at term 0: %v, want no term", w)
	}
	e.hub.d.Term = func() int { return 3 }
	_, fc = e.login(t, "192.168.1.10:5001", "todo", id, token)
	if w := framesOf(fc, "welcome")[0]; w["term"] != float64(3) {
		t.Errorf("welcome at term 3: %v", w)
	}
}

// A term frame on a link session goes to Follow with the sender's device,
// and is answered with what Follow returns. Any other session may not tell
// one, and a server that is not valid is refused.
func TestTermGoesToFollow(t *testing.T) {
	type call struct {
		sender        string
		term          int
		server        protocol.ServerInfo
		device, token string
	}
	var calls []call
	e := newEnv(t)
	e.hub.d.Follow = func(sender string, term int, si protocol.ServerInfo, device, token string) (protocol.TermAnswer, error) {
		calls = append(calls, call{sender, term, si, device, token})
		if term == 9 {
			return protocol.TermAnswer{}, errors.New("cannot")
		}
		return protocol.TermAnswer{Term: term, Main: si.ServerID}, nil
	}
	srv, srvToken := e.serverDevice(t)
	link, linkFc := e.linkSession(t, "192.168.1.9:5000", srv, srvToken)
	link.HandleText(frame(obj{"t": "term", "term": 2, "server": laptopInfo(), "device": "Dev0123456789ab", "token": "tok"}))
	if got := waitFrames(t, linkFc, "term", 1)[0]; !reflect.DeepEqual(got, obj{"t": "term", "term": float64(2), "main": "Lp7tOp0123ab"}) {
		t.Errorf("the answer %v", got)
	}
	if len(calls) != 1 || calls[0].sender != srv || calls[0].term != 2 || calls[0].server.ServerID != "Lp7tOp0123ab" || calls[0].device != "Dev0123456789ab" || calls[0].token != "tok" {
		t.Errorf("Follow got %+v", calls)
	}
	link.HandleText(frame(obj{"t": "term", "term": 9, "server": laptopInfo()}))
	expectClosedWithError(t, linkFc, protocol.CodeProtocol)

	phone, phoneToken := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.10:5000", "todo", phone, phoneToken)
	s.HandleText(frame(obj{"t": "term", "term": 2, "server": laptopInfo()}))
	expectClosedWithError(t, fc, protocol.CodeProtocol)

	own := laptopInfo()
	own["serverId"] = "srv-1"
	for _, bad := range []obj{own, {"serverId": "x"}} {
		link, linkFc := e.linkSession(t, "192.168.1.9:5001", srv, srvToken)
		link.HandleText(frame(obj{"t": "term", "term": 2, "server": bad}))
		expectClosedWithError(t, linkFc, protocol.CodeProtocol)
	}
	if len(calls) != 2 {
		t.Errorf("Follow was called %d times, want 2", len(calls))
	}
}

// A server-kind device of the replica, another server of the family, opens
// a link session on a subordinate to tell a term, and nothing else: no app
// session, no app list, no announce, no code.
func TestAFamilyServerMayOnlyTellATerm(t *testing.T) {
	e := newEnv(t)
	e.hub.d.Role = func() string { return protocol.RoleSubordinate }
	followed := 0
	e.hub.d.Follow = func(string, int, protocol.ServerInfo, string, string) (protocol.TermAnswer, error) {
		followed++
		return protocol.TermAnswer{Term: 1, Main: "Lp7tOp0123ab"}, nil
	}
	token := "family-token"
	e.hub.Replicate([]devices.Entry{{ID: "Laptop00000001", Kind: devices.KindServer, TokenHash: hashOf(token)}})

	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(helloFrame("todo", "Laptop00000001", token))
	expectClosedWithError(t, fc, protocol.CodeBadToken)

	for _, f := range []string{`{"t":"apps"}`, announceFrame(laptopInfo()), `{"t":"invite"}`} {
		link, linkFc := e.linkSession(t, "192.168.1.9:5001", "Laptop00000001", token)
		link.HandleText(f)
		expectClosedWithError(t, linkFc, protocol.CodeProtocol)
	}
	link, linkFc := e.linkSession(t, "192.168.1.9:5002", "Laptop00000001", token)
	link.HandleText(frame(obj{"t": "term", "term": 1, "server": laptopInfo()}))
	waitFrames(t, linkFc, "term", 1)
	if followed != 1 {
		t.Errorf("Follow called %d times", followed)
	}

	// A registry without it closes its sessions.
	e.hub.Replicate(nil)
	waitFor(t, "the family session to close", linkFc.isClosed)
}

// A subordinate that was a main hands its registry over in its announce:
// the main merges it, less what it removed, and answers with the result.
// Only a main takes a registry over.
func TestAnnounceHandsARegistryOver(t *testing.T) {
	e := newEnv(t)
	kept, _ := e.device(t, "todo")
	removed, _ := e.device(t, "todo")
	e.hub.RemoveDevice(removed)
	srv, srvToken := e.serverDevice(t)
	link, linkFc := e.linkSession(t, "192.168.1.9:5000", srv, srvToken)
	paired := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	link.HandleText(frame(obj{
		"t": "announce", "server": laptopInfo(),
		"devices": []any{
			obj{"id": "Handed00000001", "app": "todo", "name": "Tablet", "tokenHash": hashOf("t1"), "pairedAt": paired},
			obj{"id": removed, "app": "todo", "tokenHash": hashOf("t2")},
		},
		"removed": []any{obj{"id": "GoneOnTheOther", "at": paired}},
	}))
	got := waitFrames(t, linkFc, "devices", 1)
	last := got[len(got)-1]
	wantApps := []any{entryOf(e, t, "Handed00000001"), entryOf(e, t, kept)}
	if apps := appDevices(last); len(apps) != 2 || !reflect.DeepEqual(apps[0], wantApps[0]) && !reflect.DeepEqual(apps[1], wantApps[0]) {
		t.Errorf("the registry after the handover %v, want the handed device and the one kept", last)
	}
	if _, ok := e.devices.Get(removed); ok {
		t.Error("a device removed here came back with the handover")
	}
	if d, ok := e.devices.Get("Handed00000001"); !ok || d.Name != "Tablet" {
		t.Errorf("the handed device %+v", d)
	}

	e.hub.d.Role = func() string { return protocol.RoleSubordinate }
	link2, linkFc2 := e.linkSession(t, "192.168.1.9:5001", srv, srvToken)
	link2.HandleText(frame(obj{"t": "announce", "server": laptopInfo(), "devices": []any{}}))
	expectClosedWithError(t, linkFc2, protocol.CodeProtocol)
}

// TakeOver pushes takeOver to the link session of the server named, and
// Linked tells whether it has one.
func TestTakeOverAndLinked(t *testing.T) {
	e := newEnv(t)
	srv, srvToken := e.serverDevice(t)
	if err := e.hub.TakeOver("Lp7tOp0123ab"); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("TakeOver before announce: %v", err)
	}
	link, linkFc := e.linkSession(t, "192.168.1.9:5000", srv, srvToken)
	link.HandleText(announceFrame(laptopInfo()))
	waitFrames(t, linkFc, "devices", 1)
	if !e.hub.Linked("Lp7tOp0123ab") || e.hub.Linked("Other0123456") {
		t.Error("Linked is wrong")
	}
	if err := e.hub.TakeOver("Lp7tOp0123ab"); err != nil {
		t.Fatal(err)
	}
	waitFrames(t, linkFc, "takeOver", 1)
	link.Close()
	waitFor(t, "the session to close", linkFc.isClosed)
	if err := e.hub.TakeOver("Lp7tOp0123ab"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("TakeOver with no session: %v", err)
	}
	if e.hub.Linked("Lp7tOp0123ab") {
		t.Error("Linked with no session")
	}
}

// MintServer makes a server-kind device for a server and tells the link
// sessions.
func TestMintServer(t *testing.T) {
	e := newEnv(t)
	srv, srvToken := e.serverDevice(t)
	link, linkFc := e.linkSession(t, "192.168.1.9:5000", srv, srvToken)
	link.HandleText(announceFrame(laptopInfo()))
	waitFrames(t, linkFc, "devices", 1)
	desk := devices.Server{ServerID: "Desk01234567", Name: "Desk", Password: "Zx9yW8vU7tS6rQ5pO4nM3lK2",
		Fingerprint: "10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F:20:21:22:23:24:25:26:27:28:29:2A:2B:2C:2D:2E:2F", Port: 47801, Hosts: []string{"192.168.1.20"}}
	id, token, err := e.hub.MintServer(desk)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.devices.Authenticate("", id, token); !ok {
		t.Error("the minted credential is not admitted")
	}
	waitFrames(t, linkFc, "devices", 2)
}
