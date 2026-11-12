package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/protocol"
)

// Handing subordinates out to devices: a subordinate announces itself over
// its link session, a main lists its subordinates in every welcome and tells
// them its device registry, and a subordinate pairs nothing.

// laptopInfo is what a subordinate named Laptop announces.
func laptopInfo() obj {
	return obj{
		"serverId":    "Lp7tOp0123ab",
		"name":        "Laptop",
		"password":    "Zx9yW8vU7tS6rQ5pO4nM3lK2",
		"fingerprint": "10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F:20:21:22:23:24:25:26:27:28:29:2A:2B:2C:2D:2E:2F",
		"port":        float64(47801),
		"hosts":       []any{"5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f.local", "192.168.1.30"},
	}
}

func announceFrame(server obj) string { return frame(obj{"t": "announce", "server": server}) }

// linkSession opens a link session of the server-kind device srv.
func (e *env) linkSession(t *testing.T, remote, srv, token string) (*protocol.Session, *fakeChannel) {
	t.Helper()
	s, fc := e.connect(t, remote)
	s.HandleText(linkHelloFrame(srv, token))
	waitFrames(t, fc, "welcome", 1)
	return s, fc
}

// settleLink sends apps on a link session and waits for its answer, so that
// everything queued before it has been sent.
func settleLink(t *testing.T, s *protocol.Session, fc *fakeChannel) []obj {
	t.Helper()
	n := len(framesOf(fc, "apps"))
	s.HandleText(`{"t":"apps"}`)
	waitFrames(t, fc, "apps", n+1)
	return fc.frames()
}

// entryOf is a device of the registry as the main sends it.
func entryOf(e *env, t *testing.T, id string) obj {
	t.Helper()
	d, ok := e.devices.Get(id)
	if !ok {
		t.Fatalf("no device %s", id)
	}
	return obj{"id": d.ID, "app": d.App, "tokenHash": d.TokenHash}
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// A subordinate answers pair with notMain, of either kind, before checking
// the secret: it stays good for the main's page, nothing is created, and the
// attempts count no failures.
func TestPairRefusedOnASubordinate(t *testing.T) {
	e := newEnv(t)
	e.hub.d.Role = func() string { return protocol.RoleSubordinate }
	secret, _ := e.secrets.Issue()

	for i := range 12 { // past the rate limit, were these failures
		f := pairFrame("todo", secret, "Phone")
		if i%2 == 1 {
			f = serverPairFrame(secret, "Tablet")
		}
		s, fc := e.connect(t, "192.168.1.9:5000")
		s.HandleText(f)
		waitFor(t, "the session to close", fc.isClosed)
		if errs := framesOf(fc, "error"); len(errs) != 1 || errs[0]["code"] != protocol.CodeNotMain {
			t.Fatalf("attempt %d: frames %v, want one notMain error", i, fc.frames())
		}
	}
	if !e.secrets.Check(secret) {
		t.Error("the secret was used up")
	}
	if list := e.devices.List(); len(list) != 0 {
		t.Errorf("devices %+v, want none", list)
	}
	id, token := e.device(t, "todo")
	if _, fc := e.login(t, "192.168.1.9:5000", "todo", id, token); fc.isClosed() {
		t.Error("the address was blocked")
	}
}

// A link session announces how devices reach its subordinate; the main keeps
// it with the server-kind device, lists it in every welcome, and answers with
// its registry. A subordinate's welcome lists nothing.
func TestAnnounceAndTheSubordinatesList(t *testing.T) {
	e := newEnv(t)
	phone, phoneToken := e.device(t, "todo")
	srv, srvToken := e.serverDevice(t)

	_, fc := e.login(t, "192.168.1.10:5000", "todo", phone, phoneToken)
	if w := framesOf(fc, "welcome")[0]; !reflect.DeepEqual(w["subordinates"], []any{}) {
		t.Errorf("a main's welcome before any announce: %v, want subordinates []", w)
	}

	link, linkFc := e.linkSession(t, "192.168.1.9:5000", srv, srvToken)
	link.HandleText(announceFrame(laptopInfo()))
	got := waitFrames(t, linkFc, "devices", 1)[0]
	if want := []any{entryOf(e, t, phone)}; !reflect.DeepEqual(got["devices"], want) {
		t.Errorf("the answer %v, want the phone", got)
	}
	d, _ := e.devices.Get(srv)
	if d.Server == nil || d.Server.Name != "Laptop" || d.Server.Port != 47801 {
		t.Errorf("the server-kind device %+v, want what it announced", d)
	}
	reopened, err := devices.Open(e.dir+"/devices.json", e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := reopened.Get(srv); d.Server == nil || d.Server.ServerID != "Lp7tOp0123ab" {
		t.Errorf("devices.json keeps %+v, want what it announced", d)
	}

	_, fc = e.login(t, "192.168.1.10:5001", "todo", phone, phoneToken)
	if w := framesOf(fc, "welcome")[0]; !reflect.DeepEqual(w["subordinates"], []any{laptopInfo()}) {
		t.Errorf("a main's welcome %v, want the laptop listed", w)
	}

	moved := laptopInfo()
	moved["port"], moved["hosts"] = float64(47900), []any{"5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f.local"}
	link.HandleText(announceFrame(moved))
	waitFrames(t, linkFc, "devices", 2)
	_, fc = e.login(t, "192.168.1.10:5002", "todo", phone, phoneToken)
	if w := framesOf(fc, "welcome")[0]; !reflect.DeepEqual(w["subordinates"], []any{moved}) {
		t.Errorf("after a second announce: %v, want the new port and hosts", w)
	}

	e.hub.d.Role = func() string { return protocol.RoleSubordinate }
	_, fc = e.login(t, "192.168.1.10:5003", "todo", phone, phoneToken)
	if w := framesOf(fc, "welcome")[0]; w["subordinates"] != nil || w["role"] != "subordinate" {
		t.Errorf("a subordinate's welcome %v, want no subordinates", w)
	}
}

// What a main refuses to take from announce: anything but a link session, a
// server it cannot keep, and its own id. Each closes the session.
func TestAnnounceRefusals(t *testing.T) {
	e := newEnv(t)
	phone, phoneToken := e.device(t, "todo")
	srv, srvToken := e.serverDevice(t)

	own := laptopInfo()
	own["serverId"] = "srv-1" // the env's own id; not even a valid one
	bad := laptopInfo()
	bad["hosts"] = []any{"evil\r\na=candidate"}
	for _, c := range []struct {
		name  string
		login func() (*protocol.Session, *fakeChannel)
		frame string
	}{
		{"a device's session", func() (*protocol.Session, *fakeChannel) {
			return e.login(t, "192.168.1.10:5000", "todo", phone, phoneToken)
		}, announceFrame(laptopInfo())},
		{"a server-kind device's app session", func() (*protocol.Session, *fakeChannel) {
			return e.login(t, "192.168.1.9:5000", "todo", srv, srvToken)
		}, announceFrame(laptopInfo())},
		{"a host that would end the SDP line", func() (*protocol.Session, *fakeChannel) {
			return e.linkSession(t, "192.168.1.9:5001", srv, srvToken)
		}, announceFrame(bad)},
		{"this server's own id", func() (*protocol.Session, *fakeChannel) {
			return e.linkSession(t, "192.168.1.9:5002", srv, srvToken)
		}, announceFrame(own)},
		{"no server", func() (*protocol.Session, *fakeChannel) {
			return e.linkSession(t, "192.168.1.9:5003", srv, srvToken)
		}, `{"t":"announce"}`},
	} {
		s, fc := c.login()
		s.HandleText(c.frame)
		waitFor(t, c.name+": the session to close", fc.isClosed)
		if errs := framesOf(fc, "error"); len(errs) != 1 || errs[0]["code"] != protocol.CodeProtocol {
			t.Errorf("%s: frames %v, want one protocol error", c.name, fc.frames())
		}
	}
	if d, _ := e.devices.Get(srv); d.Server != nil {
		t.Errorf("the server-kind device kept %+v from a refused announce", d.Server)
	}
}

// Every link session that announced hears the registry after each change:
// a pairing, its undoing, a removal and an app's removal. One that did not
// announce hears none. Server-kind devices are never in it.
func TestRegistryPushes(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	srv, srvToken := e.serverDevice(t)
	link, linkFc := e.linkSession(t, "192.168.1.9:5000", srv, srvToken)
	link.HandleText(announceFrame(laptopInfo()))
	if f := waitFrames(t, linkFc, "devices", 1)[0]; !reflect.DeepEqual(f["devices"], []any{}) {
		t.Errorf("the first registry %v, want empty", f)
	}
	quiet, quietFc := e.linkSession(t, "192.168.1.9:5001", srv, srvToken)

	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.10:5000")
	s.HandleText(pairFrame("todo", secret, "Phone"))
	paired := waitFrames(t, fc, "paired", 1)[0]
	phone := paired["device"].(string)
	waitFrames(t, linkFc, "devices", 2)
	s.HandleText(`{"t":"head"}`) // confirms
	if f := framesOf(linkFc, "devices")[1]; !reflect.DeepEqual(f["devices"], []any{entryOf(e, t, phone)}) {
		t.Errorf("after the pairing %v, want the phone", f)
	}

	notes, _ := e.device(t, "notes")
	e.hub.DevicesChanged() // a change the hub did not make itself
	waitFrames(t, linkFc, "devices", 3)
	if f := framesOf(linkFc, "devices")[2]; !reflect.DeepEqual(f["devices"], []any{entryOf(e, t, notes), entryOf(e, t, phone)}) {
		t.Errorf("registry %v, want notes first, sorted by app", f)
	}

	if !e.hub.RemoveDevice(phone) {
		t.Fatal("RemoveDevice = false")
	}
	if f := waitFrames(t, linkFc, "devices", 4)[3]; !reflect.DeepEqual(f["devices"], []any{entryOf(e, t, notes)}) {
		t.Errorf("after the removal %v, want notes only", f)
	}
	if err := e.hub.RemoveApp("notes"); err != nil {
		t.Fatal(err)
	}
	if f := waitFrames(t, linkFc, "devices", 5)[4]; !reflect.DeepEqual(f["devices"], []any{}) {
		t.Errorf("after the app's removal %v, want none", f)
	}

	// An unconfirmed pairing is pushed, and pushed away again when undone.
	secret, _ = e.secrets.Issue()
	s, fc = e.connect(t, "192.168.1.10:5001")
	s.HandleText(pairFrame("todo", secret, "Tablet"))
	waitFrames(t, fc, "welcome", 1)
	waitFrames(t, linkFc, "devices", 6)
	s.Close()
	if f := waitFrames(t, linkFc, "devices", 7)[6]; !reflect.DeepEqual(f["devices"], []any{}) {
		t.Errorf("after the pairing was undone %v, want none", f)
	}

	settleLink(t, quiet, quietFc)
	if n := len(framesOf(quietFc, "devices")); n != 0 {
		t.Errorf("a link session that did not announce got %d registries", n)
	}
	for _, f := range framesOf(linkFc, "devices") {
		for _, d := range f["devices"].([]any) {
			if d.(obj)["id"] == srv {
				t.Errorf("the registry %v holds the server-kind device", f)
			}
		}
	}
}

// A subordinate admits the devices of its main's registry by their token
// hashes, each to its own app only; its own devices win an id they share.
// Forget from one closes its sessions but cannot remove it: it is the main's.
func TestReplicaAdmission(t *testing.T) {
	e := newEnv(t)
	e.hub.d.Role = func() string { return protocol.RoleSubordinate }
	e.addApp(t, apps.App{ID: "notes"})
	own, ownToken := e.device(t, "todo")
	e.devices.SetReplica([]devices.Entry{
		{ID: "Dev0123456789ab", App: "todo", TokenHash: hashOf("tok-main")},
		{ID: own, App: "todo", TokenHash: hashOf("not-the-own-token")},
		{ID: "Bad", App: "No App!", TokenHash: hashOf("x")},
	})
	if n := len(e.devices.Replica()); n != 2 {
		t.Errorf("replica holds %d devices, want the 2 valid ones", n)
	}

	s, fc := e.login(t, "192.168.1.9:5000", "todo", "Dev0123456789ab", "tok-main")
	put(s, nil, []byte(`{"a":1}`))
	waitFrames(t, fc, "stored", 1)
	e.login(t, "192.168.1.9:5001", "todo", own, ownToken) // the own device wins its id

	for _, c := range []struct{ name, app, device, token string }{
		{"a wrong token", "todo", "Dev0123456789ab", "tok-other"},
		{"another app", "notes", "Dev0123456789ab", "tok-main"},
		{"the replica's hash for an own device", "todo", own, "not-the-own-token"},
	} {
		s, fc := e.connect(t, "192.168.1.10:5000")
		s.HandleText(helloFrame(c.app, c.device, c.token))
		if f := waitFrames(t, fc, "error", 1)[0]; f["code"] != protocol.CodeBadToken {
			t.Errorf("%s: %v, want badToken", c.name, f)
		}
	}
	if list := e.devices.List(); len(list) != 1 {
		t.Errorf("listed devices %+v, want the own one only", list)
	}

	s.HandleText(`{"t":"forget"}`)
	waitFor(t, "the session to close", fc.isClosed)
	if _, ok := e.devices.Get("Dev0123456789ab"); !ok {
		t.Error("forget removed the main's device from the replica")
	}

	// The main drops the device: its live session closes, and so does no
	// session of the own device that shares an id with a dropped entry.
	_, fc = e.login(t, "192.168.1.9:5002", "todo", "Dev0123456789ab", "tok-main")
	ownSession, ownFc := e.login(t, "192.168.1.9:5003", "todo", own, ownToken)
	e.hub.Replicate([]devices.Entry{{ID: "Dev0123456789ab", App: "todo", TokenHash: hashOf("tok-main")}})
	settle(t, ownSession, ownFc)
	if fc.isClosed() || ownFc.isClosed() {
		t.Error("a session closed although its device stays")
	}
	e.hub.Replicate(nil)
	waitFor(t, "the dropped device's session to close", fc.isClosed)
	settle(t, ownSession, ownFc)
	if ownFc.isClosed() {
		t.Error("the own device's session closed with the replica's entry")
	}
	s, fc = e.connect(t, "192.168.1.9:5004")
	s.HandleText(helloFrame("todo", "Dev0123456789ab", "tok-main"))
	if f := waitFrames(t, fc, "error", 1)[0]; f["code"] != protocol.CodeBadToken {
		t.Errorf("after the main dropped it: %v, want badToken", f)
	}
}
