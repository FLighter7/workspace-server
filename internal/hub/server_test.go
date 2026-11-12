package hub

import (
	"errors"
	"reflect"
	"testing"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/store"
)

// The sessions of a server-kind device: a subordinate server linked to this
// one.

func serverPairFrame(secret, name string) string {
	return frame(obj{"t": "pair", "proto": 1, "kind": "server", "secret": secret, "name": name})
}

func linkHelloFrame(device, token string) string {
	return frame(obj{"t": "hello", "proto": 1, "device": device, "token": token})
}

// serverDevice pairs a server-kind device straight in the registry.
func (e *env) serverDevice(t *testing.T) (id, token string) {
	t.Helper()
	d, token, err := e.devices.PairServer("Laptop")
	if err != nil {
		t.Fatal(err)
	}
	return d.ID, token
}

var todoAndNotes = []any{obj{"id": "notes", "name": "Notes"}, obj{"id": "todo", "name": "Todo"}}

// A server-kind pair needs no app: it makes a device with no app, and the
// session is its link session, whose welcome has no etag and names the role,
// and which may ask for the app list.
func TestPairAsAServer(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes", Name: "Notes"})
	if _, err := e.store.Write("todo", []byte(`{}`), nil, 10); err != nil {
		t.Fatal(err)
	}
	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(serverPairFrame(secret, "Laptop"))
	waitFrames(t, fc, "welcome", 1)
	s.HandleText(`{"t":"apps"}`)
	frames := waitFrames(t, fc, "apps", 3, "paired", "welcome")

	list := e.devices.List()
	if len(list) != 1 || !list[0].IsServer() || list[0].App != "" || list[0].Name != "Laptop" {
		t.Fatalf("devices = %+v, want one server-kind device named Laptop", list)
	}
	if frames[0]["t"] != "paired" || frames[0]["device"] != list[0].ID {
		t.Errorf("first frame %v, want paired for %s", frames[0], list[0].ID)
	}
	if w := frames[1]; w["t"] != "welcome" || w["etag"] != nil || w["role"] != "main" {
		t.Errorf("welcome %v, want etag null and role main", w)
	}
	if got := frames[2]; !reflect.DeepEqual(got, obj{"t": "apps", "apps": todoAndNotes}) {
		t.Errorf("apps %v, want notes and todo", got)
	}
	expectSessions(t, e.hub, SessionInfo{App: "", Device: list[0].ID, Remote: "192.168.1.9:5000"})
	if e.secrets.Check(secret) {
		t.Error("the secret is still valid after the pairing")
	}
}

// A server-kind device is admitted to every listed app and to a link
// session; a device of an app is admitted to neither another app nor a link
// session, and an unknown device asking for a link session learns that its
// pairing is gone.
func TestServerKindAdmission(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	srv, srvToken := e.serverDevice(t)
	phone, phoneToken := e.device(t, "todo")

	for _, app := range []string{"todo", "notes", ""} {
		s, fc := e.connect(t, "192.168.1.9:5000")
		if app == "" {
			s.HandleText(linkHelloFrame(srv, srvToken))
		} else {
			s.HandleText(helloFrame(app, srv, srvToken))
		}
		w := waitFrames(t, fc, "welcome", 1)[0]
		if w["role"] != "main" {
			t.Errorf("app %q: welcome %v, want role main", app, w)
		}
		if s.App() != app {
			t.Errorf("session app %q, want %q", s.App(), app)
		}
	}

	for _, c := range []struct {
		name  string
		frame string
		code  string
	}{
		{"a server-kind device with a wrong token", linkHelloFrame(srv, phoneToken), protocol.CodeBadToken},
		{"a server-kind device for an app not listed", helloFrame("chess", srv, srvToken), protocol.CodeUnknownApp},
		{"a device of an app, with no app", linkHelloFrame(phone, phoneToken), protocol.CodeUnknownApp},
		{"a device of an app, for another app", helloFrame("notes", phone, phoneToken), protocol.CodeBadToken},
		{"an unknown device, with no app", linkHelloFrame("nosuchdevice0000", "x"), protocol.CodeBadToken},
	} {
		s, fc := e.connect(t, "192.168.1.10:5000")
		s.HandleText(c.frame)
		if f := waitFrames(t, fc, "error", 1)[0]; f["code"] != c.code {
			t.Errorf("%s: error %v, want %s", c.name, f, c.code)
		}
	}
}

// The app list goes only to sessions of a server-kind device; others are
// closed for asking. Every link session hears each change, and app sessions
// do not.
func TestAppListAndItsChanges(t *testing.T) {
	e := newEnv(t)
	srv, srvToken := e.serverDevice(t)
	phone, phoneToken := e.device(t, "todo")

	link, linkFc := e.connect(t, "192.168.1.9:5000")
	link.HandleText(linkHelloFrame(srv, srvToken))
	waitFrames(t, linkFc, "welcome", 1)
	appSession, appFc := e.login(t, "192.168.1.9:5001", "todo", srv, srvToken)
	appSession.HandleText(`{"t":"apps"}`)
	if f := waitFrames(t, appFc, "apps", 1)[0]; !reflect.DeepEqual(f["apps"], []any{obj{"id": "todo", "name": "Todo"}}) {
		t.Errorf("apps on an app session %v, want todo", f)
	}

	e.addApp(t, apps.App{ID: "notes", Name: "Notes"})
	e.hub.AppsChanged()
	if f := waitFrames(t, linkFc, "apps", 1)[0]; !reflect.DeepEqual(f["apps"], todoAndNotes) {
		t.Errorf("pushed apps %v, want notes and todo", f)
	}
	if err := e.hub.RemoveApp("notes"); err != nil {
		t.Fatal(err)
	}
	e.hub.AppsChanged()
	if f := waitFrames(t, linkFc, "apps", 2)[1]; !reflect.DeepEqual(f["apps"], []any{obj{"id": "todo", "name": "Todo"}}) {
		t.Errorf("pushed apps %v, want todo", f)
	}
	settle(t, appSession, appFc)
	if n := len(framesOf(appFc, "apps")); n != 1 {
		t.Errorf("the app session got %d apps frames, want only its answer", n)
	}
	if _, ok := e.devices.Get(srv); !ok {
		t.Error("removing an app removed the server-kind device")
	}

	s, fc := e.login(t, "192.168.1.10:5000", "todo", phone, phoneToken)
	s.HandleText(`{"t":"apps"}`)
	waitFor(t, "the device's session to close", fc.isClosed)
	if f := framesOf(fc, "error"); len(f) != 1 || f[0]["code"] != protocol.CodeProtocol {
		t.Errorf("frames %v, want one protocol error", fc.frames())
	}
}

// A server-kind device writes to any app; Replace writes without a session
// and tells every session of the app, and refuses an app that is not listed.
func TestServerKindWritesAndReplace(t *testing.T) {
	e := newEnv(t)
	srv, srvToken := e.serverDevice(t)
	phone, phoneToken := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", srv, srvToken)
	other, otherFc := e.login(t, "192.168.1.10:5000", "todo", phone, phoneToken)

	put(s, nil, []byte(`{"a":1}`))
	stored := waitFrames(t, fc, "stored", 1)[0]
	if changed := waitFrames(t, otherFc, "changed", 1)[0]; changed["etag"] != stored["etag"] {
		t.Errorf("changed %v, want %v", changed, stored["etag"])
	}

	body := []byte(`{"a":2}`)
	etag, err := e.hub.Replace("todo", body, ptr(stored["etag"].(string)))
	if err != nil || etag != store.Etag(body) {
		t.Fatalf("Replace = %q, %v; want %q", etag, err, store.Etag(body))
	}
	for _, f := range []*fakeChannel{fc, otherFc} {
		if c := waitFrames(t, f, "changed", 1); c[len(c)-1]["etag"] != etag {
			t.Errorf("last changed %v, want %s", c[len(c)-1], etag)
		}
	}
	settle(t, other, otherFc)

	var conflict *store.ConflictError
	if _, err := e.hub.Replace("todo", body, nil); !errors.As(err, &conflict) || *conflict.Current != etag {
		t.Errorf("Replace with a stale etag = %v, want a conflict naming %s", err, etag)
	}
	if _, err := e.hub.Replace("chess", body, nil); !errors.Is(err, apps.ErrNotFound) {
		t.Errorf("Replace for an app not listed = %v, want apps.ErrNotFound", err)
	}
}

// Removing a server-kind device closes its sessions of every app and its link
// session; forget from any of them removes it.
func TestRemoveServerKindDevice(t *testing.T) {
	t.Run("from the page", func(t *testing.T) {
		e := newEnv(t)
		e.addApp(t, apps.App{ID: "notes"})
		srv, token := e.serverDevice(t)
		_, todoFc := e.login(t, "192.168.1.9:5000", "todo", srv, token)
		_, notesFc := e.login(t, "192.168.1.9:5001", "notes", srv, token)
		link, linkFc := e.connect(t, "192.168.1.9:5002")
		link.HandleText(linkHelloFrame(srv, token))
		waitFrames(t, linkFc, "welcome", 1)

		if !e.hub.RemoveDevice(srv) {
			t.Fatal("RemoveDevice = false")
		}
		for _, fc := range []*fakeChannel{todoFc, notesFc, linkFc} {
			waitFor(t, "every session to close", fc.isClosed)
		}
		expectSessions(t, e.hub)
		s, fc := e.connect(t, "192.168.1.9:5003")
		s.HandleText(linkHelloFrame(srv, token))
		if f := waitFrames(t, fc, "error", 1)[0]; f["code"] != protocol.CodeBadToken {
			t.Errorf("a link hello after the removal: %v, want badToken", f)
		}
	})
	t.Run("forget on an app session", func(t *testing.T) {
		e := newEnv(t)
		srv, token := e.serverDevice(t)
		s, fc := e.login(t, "192.168.1.9:5000", "todo", srv, token)
		link, linkFc := e.connect(t, "192.168.1.9:5001")
		link.HandleText(linkHelloFrame(srv, token))
		waitFrames(t, linkFc, "welcome", 1)
		s.HandleText(`{"t":"forget"}`)
		waitFor(t, "the app session to close", fc.isClosed)
		waitFor(t, "the link session to close", linkFc.isClosed)
		if len(e.devices.List()) != 0 {
			t.Errorf("devices %+v after forget, want none", e.devices.List())
		}
	})
}

// The welcome names the role the server has now.
func TestWelcomeRole(t *testing.T) {
	role := protocol.RoleSubordinate
	e := newEnv(t)
	e.hub.d.Role = func() string { return role }
	id, token := e.device(t, "todo")
	_, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	if w := framesOf(fc, "welcome")[0]; w["role"] != "subordinate" {
		t.Errorf("welcome %v, want role subordinate", w)
	}
}

// CloseServers ends the sessions of server-kind devices only.
func TestCloseServers(t *testing.T) {
	e := newEnv(t)
	srv, srvToken := e.serverDevice(t)
	phone, phoneToken := e.device(t, "todo")
	_, srvFc := e.login(t, "192.168.1.9:5000", "todo", srv, srvToken)
	_, phoneFc := e.login(t, "192.168.1.10:5000", "todo", phone, phoneToken)
	e.hub.CloseServers()
	waitFor(t, "the server's session to close", srvFc.isClosed)
	if phoneFc.isClosed() {
		t.Error("the phone's session closed too")
	}
}
