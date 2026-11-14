package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/upstream"
	"github.com/FLighter7/workspace-server/internal/web"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// linkWait bounds every wait for the link between two servers.
const linkWait = 20 * time.Second

// Two whole servers in this process: the subordinate links to the main
// through its page's API and pulls the main's document; it announces itself,
// and a device paired with the main learns it from the welcome; the
// subordinate refuses to pair the device itself, admits it on the main's
// word, and pushes the device's write on to the main; the roles show in the
// status; a second link is refused; a reset forgets the link. The
// subordinate's page issues no pairing code of its own.
func TestTwoServersLinkPullAndPush(t *testing.T) {
	mainDir, subDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(mainDir, "apps.json"), `{"apps":[{"id":"todo","name":"Todo"},{"id":"notes","name":"Notes"}]}`)
	if err := os.MkdirAll(filepath.Join(mainDir, "WSS-Data", "apps", "todo"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mainDir, "WSS-Data", "apps", "todo", "data.json"), `{"v":1}`)

	mu, mp := freePorts(t)
	su := freeUDPPort(t, mu, mp)
	sp := freeTCPPort(t, mu, mp, su)
	main := startServer(t, mainDir, mu, mp)
	sub := startServer(t, subDir, su, sp)
	if st := sub.status(t); st.Role != "main" {
		t.Fatalf("a server with no upstream has role %q, want main", st.Role)
	}

	// The main's code with 127.0.0.1 as its only host, so the test does
	// not depend on this machine's LAN.
	c := main.pairingCode(t)
	c.H = []string{"127.0.0.1"}
	body, _ := json.Marshal(map[string]string{"code": pairing.EncodeCode(c)})
	code, resp := sub.send(t, http.MethodPost, "/api/upstream", string(body))
	if code != http.StatusOK {
		t.Fatalf("POST /api/upstream = %d %s\n%s", code, resp, sub.logs)
	}
	var st struct {
		Linked   bool
		ServerID string
	}
	if err := json.Unmarshal([]byte(resp), &st); err != nil || !st.Linked || st.ServerID != c.ID {
		t.Errorf("POST /api/upstream answered %s, want the link to %s", resp, c.ID)
	}
	if role := sub.status(t).Role; role != "subordinate" {
		t.Errorf("the linked server's role is %q, want subordinate", role)
	}

	waitUntil(t, "the pull", func() bool {
		doc, ok := sub.document(t, "todo")
		return ok && doc == `{"v":1}`
	})
	waitUntil(t, "notes on the subordinate", func() bool {
		_, list := sub.send(t, http.MethodGet, "/api/apps", "")
		var apps []struct{ ID string }
		_ = json.Unmarshal([]byte(list), &apps)
		for _, a := range apps {
			if a.ID == "notes" {
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(subDir, "upstream.json")); err != nil {
		t.Errorf("upstream.json: %v", err)
	}

	// The quota of an app the main lists is the main's: the subordinate's
	// page refuses to change it, and takes the one set on the main's page.
	// An app only the subordinate serves is its own.
	if code, resp := sub.send(t, http.MethodPut, "/api/apps/todo", `{"quota":3145728}`); code != http.StatusConflict || !strings.Contains(resp, `"followsMain"`) {
		t.Errorf("PUT a quota on the subordinate = %d %s, want 409 followsMain", code, resp)
	}
	if code, resp := sub.send(t, http.MethodPost, "/api/apps", `{"id":"photos"}`); code != http.StatusCreated {
		t.Fatalf("POST an app on the subordinate = %d %s", code, resp)
	}
	if code, resp := sub.send(t, http.MethodPut, "/api/apps/photos", `{"quota":4194304}`); code != http.StatusNoContent {
		t.Errorf("PUT the quota of the subordinate's own app = %d %s, want 204", code, resp)
	}
	_, subList := sub.send(t, http.MethodGet, "/api/apps", "")
	var subApps []web.AppInfo
	_ = json.Unmarshal([]byte(subList), &subApps)
	for _, a := range subApps {
		if want := a.ID != "photos"; a.FollowsMain != want {
			t.Errorf("the subordinate's %s: followsMain %v, want %v", a.ID, a.FollowsMain, want)
		}
		if a.ID == "photos" && a.Quota != 4<<20 {
			t.Errorf("photos' quota %d, want 4 MiB", a.Quota)
		}
	}
	if code, resp := main.send(t, http.MethodPut, "/api/apps/todo", `{"quota":3145728}`); code != http.StatusNoContent {
		t.Fatalf("PUT a quota on the main = %d %s", code, resp)
	}
	waitUntil(t, "the main's new quota on the subordinate", func() bool {
		_, list := sub.send(t, http.MethodGet, "/api/apps", "")
		var apps []web.AppInfo
		_ = json.Unmarshal([]byte(list), &apps)
		for _, a := range apps {
			if a.ID == "todo" {
				return a.Quota == 3<<20
			}
		}
		return false
	})

	ctx, cancel := context.WithTimeout(context.Background(), linkWait)
	defer cancel()
	// The subordinate pairs nothing, so its page issues no code that would
	// fail with notMain.
	if code, resp := sub.send(t, http.MethodPost, "/api/pairing", ""); code != http.StatusConflict || !strings.Contains(resp, `"notMain"`) {
		t.Errorf("POST /api/pairing on the subordinate = %d %s, want 409 notMain", code, resp)
	}

	// A device pairs with the main, whose welcome lists the subordinate as
	// it announced itself (connected means announced).
	waitUntil(t, "the link", func() bool { return sub.upstream(t).Connected })
	mc := main.pairingCode(t)
	mc.H = []string{"127.0.0.1"}
	paired, w, err := pairWith(ctx, mc)
	if err != nil {
		t.Fatalf("pair with the main: %v", err)
	}
	subStatus := sub.status(t)
	if len(w.Subordinates) != 1 {
		t.Fatalf("the main's welcome lists %+v, want the subordinate", w.Subordinates)
	}
	si := w.Subordinates[0]
	if si.ServerID != subStatus.ServerID || si.Name != subStatus.Name || si.Port != su || si.Hosts[0] != subStatus.MDNSName {
		t.Errorf("the subordinate is listed as %+v; its status is %+v", si, subStatus)
	}

	// The subordinate admits it on the main's word; its write there goes
	// on to the main: the hub's hook reaches the link. The announced
	// password and fingerprint are what the connection is built from; only
	// the hosts are 127.0.0.1, so the test does not depend on the LAN.
	waitUntil(t, "the subordinate to admit the device", func() bool { return sub.upstream(t).Devices == 1 })
	phone, err := wsclient.Dial(ctx, wsclient.Target{Password: si.Password, Fingerprint: si.Fingerprint, Port: si.Port, Hosts: []string{"127.0.0.1"}}, "todo", wsclient.DialOptions{})
	if err != nil {
		t.Fatalf("dial the subordinate: %v", err)
	}
	defer phone.Close()
	sw, err := phone.Hello(ctx, paired.Device, paired.Token)
	if err != nil || sw.Role != "subordinate" || sw.ServerID != si.ServerID {
		t.Fatalf("hello on the subordinate: %+v, %v", sw, err)
	}
	if err := writeDataJSON(ctx, phone, `{"v":2}`); err != nil {
		t.Fatalf("put on the subordinate: %v", err)
	}
	waitUntil(t, "the push to the main", func() bool {
		doc, ok := main.document(t, "todo")
		return ok && doc == `{"v":2}`
	})

	again, _ := json.Marshal(map[string]string{"code": pairing.EncodeCode(c)})
	if code, resp := sub.send(t, http.MethodPost, "/api/upstream", string(again)); code != http.StatusConflict {
		t.Errorf("a second link = %d %s, want 409", code, resp)
	}

	// The main knows the subordinate as a device with no app, beside the
	// phone.
	_, devs := main.send(t, http.MethodGet, "/api/devices", "")
	var list []struct{ App, Name string }
	if err := json.Unmarshal([]byte(devs), &list); err != nil || len(list) != 2 || list[0].App != "" || list[1].App != "todo" {
		t.Errorf("the main's devices %s, want one server-kind device and the phone", devs)
	}

	if code, resp := sub.send(t, http.MethodDelete, "/api/data", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE /api/data = %d %s", code, resp)
	}
	waitUntil(t, "the reset", func() bool {
		_, err := os.Stat(filepath.Join(subDir, "upstream.json"))
		return errors.Is(err, os.ErrNotExist)
	})
	waitUntil(t, "the server to come back as a main", func() bool {
		code, resp, err := sub.request(http.MethodGet, "/api/status", "")
		return err == nil && code == http.StatusOK && json.Valid(resp) && sub.status(t).Role == "main"
	})
}

// Through the subordinate's page: a pairing code of the main for a browser
// on its computer (invite), which pairs a device with the main; the main's
// page shows the subordinate as a server and refuses to link while it has
// one; unlinking makes the subordinate a main again, the main forgets it,
// and the data stays.
func TestTwoServersInviteAndUnlink(t *testing.T) {
	mainDir, subDir := t.TempDir(), t.TempDir()
	mu, mp := freePorts(t)
	su := freeUDPPort(t, mu, mp)
	sp := freeTCPPort(t, mu, mp, su)
	main := startServer(t, mainDir, mu, mp)
	sub := startServer(t, subDir, su, sp)

	if code, resp := sub.send(t, http.MethodPost, "/api/upstream/invite", ""); code != http.StatusConflict || !strings.Contains(resp, `"notLinked"`) {
		t.Errorf("invite before linking = %d %s, want 409 notLinked", code, resp)
	}
	c := main.pairingCode(t)
	c.H = []string{"127.0.0.1"}
	body, _ := json.Marshal(map[string]string{"code": pairing.EncodeCode(c)})
	if code, resp := sub.send(t, http.MethodPost, "/api/upstream", string(body)); code != http.StatusOK {
		t.Fatalf("POST /api/upstream = %d %s\n%s", code, resp, sub.logs)
	}
	waitUntil(t, "the link", func() bool { return sub.upstream(t).Connected })
	subStatus := sub.status(t)

	// The main's page: the subordinate is a server, with what it
	// announced, and the main does not link to another main while it has
	// one.
	_, devs := main.send(t, http.MethodGet, "/api/devices", "")
	var list []web.DeviceInfo
	if err := json.Unmarshal([]byte(devs), &list); err != nil || len(list) != 1 {
		t.Fatalf("the main's devices %s", devs)
	}
	if d := list[0]; d.Kind != "server" || d.App != "" || d.Server == nil || d.Server.ServerID != subStatus.ServerID ||
		d.Server.Name != subStatus.Name || d.Server.Port != su || d.Server.Hosts[0] != subStatus.MDNSName {
		t.Errorf("the main lists the subordinate as %+v (announced %+v); its status is %+v", d, d.Server, subStatus)
	}
	var up web.UpstreamStatus
	if _, resp := main.send(t, http.MethodGet, "/api/upstream", ""); json.Unmarshal([]byte(resp), &up) != nil || up.Subordinates != 1 || up.Linked {
		t.Errorf("the main's GET /api/upstream = %s, want one subordinate", resp)
	}
	if code, resp := main.send(t, http.MethodPost, "/api/upstream", `{"code":"wsrv1.x"}`); code != http.StatusConflict || !strings.Contains(resp, `"hasSubordinates"`) {
		t.Errorf("linking the main = %d %s, want 409 hasSubordinates", code, resp)
	}

	// invite: the main's code, which pairs a device with the main.
	code, resp := sub.send(t, http.MethodPost, "/api/upstream/invite", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/upstream/invite = %d %s\n%s", code, resp, sub.logs)
	}
	var inv struct{ Code, QR string }
	if err := json.Unmarshal([]byte(resp), &inv); err != nil || !strings.HasPrefix(inv.QR, "data:image/png;base64,") {
		t.Fatalf("the invitation %s", resp)
	}
	ic, err := pairing.DecodeCode(inv.Code)
	if err != nil || ic.ID != c.ID {
		t.Fatalf("the invited code %q (%v), want the main's", inv.Code, err)
	}
	ic.H = []string{"127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), linkWait)
	defer cancel()
	browser, w, err := pairWith(ctx, ic)
	if err != nil {
		t.Fatalf("pair with the invited code: %v", err)
	}
	if len(w.Subordinates) != 1 || w.Subordinates[0].ServerID != subStatus.ServerID {
		t.Errorf("the main's welcome lists %+v, want the subordinate", w.Subordinates)
	}
	waitUntil(t, "the subordinate to admit the browser", func() bool { return sub.upstream(t).Devices == 1 })

	// Unlinking: the main is told, the link and its file go, and the data
	// stays.
	code, resp = sub.send(t, http.MethodDelete, "/api/upstream", "")
	if code != http.StatusOK || strings.TrimSpace(resp) != `{"forgotten":true}` {
		t.Fatalf("DELETE /api/upstream = %d %s\n%s", code, resp, sub.logs)
	}
	if _, err := os.Stat(filepath.Join(subDir, "upstream.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upstream.json after unlinking: %v", err)
	}
	if st := sub.status(t); st.Role != "main" {
		t.Errorf("the unlinked server's role %q", st.Role)
	}
	if st := sub.upstream(t); st.Linked || st.Devices != 0 {
		t.Errorf("the unlinked server's link %+v", st)
	}
	_, devs = main.send(t, http.MethodGet, "/api/devices", "")
	if err := json.Unmarshal([]byte(devs), &list); err != nil || len(list) != 1 || list[0].ID != browser.Device {
		t.Errorf("the main's devices after the unlink %s, want the browser only", devs)
	}
	_, apps := sub.send(t, http.MethodGet, "/api/apps", "")
	if !strings.Contains(apps, `"id":"todo"`) {
		t.Errorf("the unlinked server's apps %s", apps)
	}
	if code, resp := sub.send(t, http.MethodDelete, "/api/upstream", ""); code != http.StatusConflict || !strings.Contains(resp, `"notLinked"`) {
		t.Errorf("a second unlink = %d %s, want 409 notLinked", code, resp)
	}
	if code, _ := sub.send(t, http.MethodPost, "/api/pairing", ""); code != http.StatusOK {
		t.Errorf("POST /api/pairing on the unlinked server = %d, want a code", code)
	}
}

// Two whole servers whose todo sets differ at their first link: the
// subordinate keeps its own, takes the main's quota, and its page lists the
// app as diverged; a planned switch of the main is refused (409 diverged);
// the main's welcome asks a device paired with it, whose choice (keep the
// subordinate's) reaches the subordinate and makes the main's set its own.
func TestTwoServersDivergeAndADeviceChooses(t *testing.T) {
	mainDir, subDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(mainDir, "apps.json"), `{"apps":[{"id":"todo","name":"Todo","quota":2097152}]}`)
	for dir, body := range map[string]string{mainDir: `{"main":1}`, subDir: `{"laptop":1}`} {
		writeFileAll(t, filepath.Join(dir, "WSS-Data", "apps", "todo", "data.json"), body)
	}
	writeFileAll(t, filepath.Join(mainDir, "WSS-Data", "apps", "todo", "only-main.md"), "theirs")
	mu, mp := freePorts(t)
	su := freeUDPPort(t, mu, mp)
	sp := freeTCPPort(t, mu, mp, su)
	main := startServer(t, mainDir, mu, mp)
	sub := startServer(t, subDir, su, sp)

	c := main.pairingCode(t)
	c.H = []string{"127.0.0.1"}
	body, _ := json.Marshal(map[string]string{"code": pairing.EncodeCode(c)})
	if code, resp := sub.send(t, http.MethodPost, "/api/upstream", string(body)); code != http.StatusOK {
		t.Fatalf("POST /api/upstream = %d %s\n%s", code, resp, sub.logs)
	}
	waitUntil(t, "the divergence", func() bool {
		st := sub.upstream(t)
		return len(st.Diverged) == 1 && st.Diverged[0] == "todo"
	})
	waitUntil(t, "the main's quota", func() bool {
		_, list := sub.send(t, http.MethodGet, "/api/apps", "")
		return strings.Contains(list, `"quota":2097152`)
	})
	if code, resp := sub.send(t, http.MethodPost, "/api/upstream/promote", `{}`); code != http.StatusConflict || !strings.Contains(resp, `"diverged"`) {
		t.Errorf("a planned switch while diverged = %d %s, want 409 diverged", code, resp)
	}
	// Each page marks the app, naming the other server: the subordinate's
	// the main, the main's the subordinate, once it has told the main.
	mainStatus, subStatus := main.status(t), sub.status(t)
	if got := appDivergence(t, sub, "todo"); len(got) != 1 || got[0].ServerID != mainStatus.ServerID || got[0].Name != mainStatus.Name || got[0].Since == "" {
		t.Errorf("the subordinate's page lists todo's divergence as %+v, want the main %s", got, mainStatus.ServerID)
	}
	waitUntil(t, "the main's page to mark todo", func() bool {
		got := appDivergence(t, main, "todo")
		return len(got) == 1 && got[0].ServerID == subStatus.ServerID && got[0].Name == subStatus.Name
	})

	ctx, cancel := context.WithTimeout(context.Background(), linkWait)
	defer cancel()
	subID := sub.status(t).ServerID
	var paired wsclient.Paired
	var target wsclient.Target
	waitUntil(t, "the main to ask its devices", func() bool {
		mc := main.pairingCode(t)
		mc.H = []string{"127.0.0.1"}
		p, w, err := pairWith(ctx, mc)
		if err != nil {
			t.Fatalf("pair with the main: %v", err)
		}
		pc, _ := wsclient.ParseCode(pairing.EncodeCode(mc))
		paired, target = p, pc.Target
		return len(w.Diverged) == 1 && w.Diverged[0].ServerID == subID
	})
	phone, err := wsclient.Dial(ctx, target, "todo", wsclient.DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	if _, err := phone.Hello(ctx, paired.Device, paired.Token); err != nil {
		t.Fatal(err)
	}
	if err := phone.Resolve(ctx, subID, wsclient.KeepSubordinate); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	waitUntil(t, "the subordinate's set on the main", func() bool {
		_, errMain := os.Stat(filepath.Join(mainDir, "WSS-Data", "apps", "todo", "only-main.md"))
		data, _ := os.ReadFile(filepath.Join(mainDir, "WSS-Data", "apps", "todo", "data.json"))
		return errors.Is(errMain, os.ErrNotExist) && string(data) == `{"laptop":1}`
	})
	waitUntil(t, "the divergence to end", func() bool { return len(sub.upstream(t).Diverged) == 0 })
	waitUntil(t, "the pages to drop the mark", func() bool {
		return len(appDivergence(t, sub, "todo")) == 0 && len(appDivergence(t, main, "todo")) == 0
	})
}

// appDivergence is what the server's GET /api/apps says of app's
// divergences.
func appDivergence(t *testing.T, ts *testServer, app string) []web.Divergence {
	t.Helper()
	code, list := ts.send(t, http.MethodGet, "/api/apps", "")
	var apps []web.AppInfo
	if code != http.StatusOK || json.Unmarshal([]byte(list), &apps) != nil {
		t.Fatalf("GET /api/apps = %d %s", code, list)
	}
	for _, a := range apps {
		if a.ID == app {
			if a.Diverged == nil {
				t.Fatalf("GET /api/apps gives %s no diverged list: %s", app, list)
			}
			return a.Diverged
		}
	}
	t.Fatalf("GET /api/apps lists no %s: %s", app, list)
	return nil
}

// pairWith pairs a device of todo with the server whose code c is, and
// confirms the pairing.
func pairWith(ctx context.Context, c pairing.Code) (wsclient.Paired, wsclient.Welcome, error) {
	pc, err := wsclient.ParseCode(pairing.EncodeCode(c))
	if err != nil {
		return wsclient.Paired{}, wsclient.Welcome{}, err
	}
	cl, err := wsclient.Dial(ctx, pc.Target, "todo", wsclient.DialOptions{})
	if err != nil {
		return wsclient.Paired{}, wsclient.Welcome{}, err
	}
	defer cl.Close()
	p, w, err := cl.Pair(ctx, "", pc.Secret, "Phone")
	if err != nil {
		return p, w, err
	}
	_, err = cl.Head(ctx) // confirms the pairing
	return p, w, err
}

// upstream reads the link's status from the page's API.
func (ts *testServer) upstream(t *testing.T) upstream.Status {
	t.Helper()
	code, body := ts.send(t, http.MethodGet, "/api/upstream", "")
	var st upstream.Status
	if code != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil {
		t.Fatalf("GET /api/upstream = %d %s", code, body)
	}
	return st
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(linkWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A link session has no app: reloading apps.json leaves it open whatever
// the file drops.
func TestReloadAppsKeepsLinkSessions(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	secret, _ := s.secrets.Issue()
	ch := &fakeChannel{}
	link := protocol.NewSession(ch, "192.0.2.1:50000", s.hub, protocol.Options{})
	t.Cleanup(link.Close)
	link.HandleText(`{"t":"pair","proto":1,"kind":"server","secret":"` + secret + `","name":"Laptop"}`)
	if !link.Authenticated() || link.App() != "" {
		t.Fatal("the server-kind pairing did not open a link session")
	}
	writeFile(t, filepath.Join(dir, "apps.json"), `{"apps":[]}`)
	s.reloadApps()
	if ch.isClosed() {
		t.Error("reloading apps.json closed the link session")
	}
}

// writeDataJSON writes body as the app's data.json over cl, in a transaction of
// its own, over whatever data.json the set holds.
func writeDataJSON(ctx context.Context, cl *wsclient.Client, body string) error {
	data := wsclient.Key{ID: "data", Type: "json"}
	l, err := cl.ListAll(ctx)
	if err != nil {
		return err
	}
	var ifEtag *string
	for _, d := range l.Docs {
		if d.Key == data {
			ifEtag = &d.Etag
		}
	}
	const tx = "upstream-test-0001"
	if _, err := cl.Begin(ctx, tx); err != nil {
		return err
	}
	if _, err := cl.Put(ctx, tx, data, ifEtag, int64(len(body)), 0, strings.NewReader(body)); err != nil {
		return err
	}
	r, err := cl.Commit(ctx, tx)
	if err == nil && !r.Committed {
		err = errors.New("the commit met a conflict")
	}
	return err
}
