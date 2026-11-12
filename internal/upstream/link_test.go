package upstream

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/hub"
	"github.com/FLighter7/workspace-server/internal/identity"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/rtc"
	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// These tests run real servers in this process, each with its own UDP
// listener on 127.0.0.1, hub, registries, store and link, and link them over
// real WebRTC.

// testDeadline bounds every wait for the network.
const testDeadline = 20 * time.Second

// logBuf keeps a server's log lines and passes them to the test's log.
type logBuf struct {
	t     *testing.T
	name  string
	mu    sync.Mutex
	lines []string
}

func (b *logBuf) printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	b.mu.Lock()
	b.lines = append(b.lines, line)
	b.mu.Unlock()
	b.t.Logf("%s: %s", b.name, line)
}

func (b *logBuf) has(sub string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.ContainsFunc(b.lines, func(l string) bool { return strings.Contains(l, sub) })
}

// node is one server.
type node struct {
	t        *testing.T
	name     string
	dir      string
	id       *identity.Identity
	fp       string // base64url of the certificate's SHA-256
	fpHex    string // uppercase colon hex
	cfg      rtc.Config
	l        *rtc.Listener
	apps     *apps.Registry
	devices  *devices.Registry
	store    *store.Store
	secrets  *pairing.Secrets
	hub      *hub.Hub
	link     *Link
	logs     *logBuf
	linkDeps Deps
}

func startNode(t *testing.T, name string) *node {
	t.Helper()
	n := &node{t: t, name: name, dir: t.TempDir(), logs: &logBuf{t: t, name: name}}
	var err error
	if n.id, err = identity.LoadOrCreate(filepath.Join(n.dir, "identity.json")); err != nil {
		t.Fatal(err)
	}
	cert, err := n.id.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	fp, err := n.id.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	n.fp = base64.RawURLEncoding.EncodeToString(fp[:])
	if n.fpHex, err = n.id.FingerprintHex(); err != nil {
		t.Fatal(err)
	}
	if n.apps, err = apps.Open(filepath.Join(n.dir, "apps.json")); err != nil { // lists "todo"
		t.Fatal(err)
	}
	if n.devices, err = devices.Open(filepath.Join(n.dir, "devices.json"), time.Now); err != nil {
		t.Fatal(err)
	}
	n.store = store.New(filepath.Join(n.dir, "apps"), time.Now)
	n.secrets = pairing.NewSecrets(time.Now)
	n.hub = hub.New(hub.Deps{
		ServerID: n.id.ServerID,
		Name:     func() string { return name },
		Hosts:    func() []string { return []string{"127.0.0.1"} },
		Apps:     n.apps,
		Devices:  n.devices,
		Store:    n.store,
		Secrets:  n.secrets,
		Refusals: pairing.NewRefusals(time.Now),
		Now:      time.Now,
		Role:     func() string { return n.link.Role() },
		Logf:     n.logs.printf,
	})
	n.linkDeps = Deps{
		Path:     filepath.Join(n.dir, "upstream.json"),
		ServerID: n.id.ServerID,
		Name:     func() string { return name },
		Apps:     n.apps,
		Store:    n.store,
		Writer:   n.hub,
		Dial: func(ctx context.Context, t wsclient.Target, app string) (*wsclient.Client, error) {
			return wsclient.Dial(ctx, t, app, wsclient.DialOptions{})
		},
		Logf:       n.logs.printf,
		MinBackoff: 50 * time.Millisecond,
		MaxBackoff: 400 * time.Millisecond,
	}
	n.openLink()
	n.cfg = rtc.Config{
		Addresses:   []net.IP{net.IPv4(127, 0, 0, 1)},
		Port:        freePort(t),
		ICEPwd:      n.id.ICEPwd,
		Certificate: cert,
		Backend:     n.hub,
		Logf:        func(format string, args ...any) { t.Logf(name+": rtc: "+format, args...) },
	}
	n.listen()
	return n
}

// openLink opens the link from upstream.json and starts it, as a server
// start does.
func (n *node) openLink() {
	n.t.Helper()
	l, err := Open(n.linkDeps)
	if err != nil {
		n.t.Fatal(err)
	}
	n.link = l
	l.Start()
	n.t.Cleanup(l.Close)
}

func (n *node) listen() {
	n.t.Helper()
	l, err := rtc.Listen(n.cfg)
	if err != nil {
		n.t.Fatal(err)
	}
	n.l = l
	n.t.Cleanup(func() { _ = l.Close() })
}

// code is a pairing code of n with a fresh secret; its only host is
// 127.0.0.1.
func (n *node) code() string {
	secret, _ := n.secrets.Issue()
	return pairing.EncodeCode(pairing.Code{
		V: 1, ID: n.id.ServerID, N: n.name, P: n.id.ICEPwd, F: n.fp, O: n.cfg.Port,
		H: []string{"127.0.0.1"}, S: secret,
	})
}

func (n *node) target() wsclient.Target {
	return wsclient.Target{Password: n.id.ICEPwd, Fingerprint: n.fpHex, Port: n.cfg.Port, Hosts: []string{"127.0.0.1"}}
}

// linkTo links n to up and fails the test if it does not.
func (n *node) linkTo(up *node) {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	if err := n.link.Pair(ctx, up.code()); err != nil {
		n.t.Fatalf("%s: link to %s: %v", n.name, up.name, err)
	}
}

// etag is the etag of n's document of app, "" when there is none.
func (n *node) etag(app string) string {
	_, etag, err := n.store.Read(app)
	if err != nil {
		n.t.Fatal(err)
	}
	if etag == nil {
		return ""
	}
	return *etag
}

func (n *node) write(app, body string, ifEtag *string) string {
	n.t.Helper()
	etag, err := n.hub.Replace(app, []byte(body), ifEtag)
	if err != nil {
		n.t.Fatalf("%s: write %s: %v", n.name, app, err)
	}
	return etag
}

// device is a device paired with a server over WebRTC, which records the
// changed pushes it hears.
type device struct {
	cl      *wsclient.Client
	welcome wsclient.Welcome
	mu      sync.Mutex
	heard   []string
}

func (n *node) device(app string) *device {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	secret, _ := n.secrets.Issue()
	cl, err := wsclient.Dial(ctx, n.target(), app, wsclient.DialOptions{})
	if err != nil {
		n.t.Fatalf("%s: dial: %v", n.name, err)
	}
	n.t.Cleanup(cl.Close)
	d := &device{cl: cl}
	cl.OnChanged(func(etag string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.heard = append(d.heard, etag)
	})
	if _, d.welcome, err = cl.Pair(ctx, "", secret, "Phone"); err != nil {
		n.t.Fatalf("%s: pair a device: %v", n.name, err)
	}
	if _, err := cl.Head(ctx); err != nil { // confirms the pairing
		n.t.Fatal(err)
	}
	return d
}

func (d *device) heardEtag(etag string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.heard, etag)
}

func (d *device) put(t *testing.T, ifEtag *string, body string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	stored, etag, err := d.cl.Put(ctx, ifEtag, []byte(body))
	if err != nil || !stored {
		t.Fatalf("put: stored %v, %v", stored, err)
	}
	return *etag
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func ptr(s string) *string { return &s }

// The row "only the main moved": the subordinate pulls the main's document,
// writes it if its own copy is still the one it read, tells its own
// sessions, and keeps the etag both agree on.
func TestPullWhenOnlyTheMainMoved(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)

	sub.linkTo(main)
	if role := sub.link.Role(); role != RoleSubordinate {
		t.Errorf("the subordinate's role is %q", role)
	}
	waitFor(t, "the first pull", func() bool { return sub.etag("todo") == v1 })
	waitFor(t, "the agreed etag", func() bool { b, ok := sub.link.base("todo"); return ok && same(b, &v1) })

	servers := slices.DeleteFunc(main.devices.List(), func(d devices.Device) bool { return !d.IsServer() })
	if len(servers) != 1 || servers[0].Name != "Laptop" {
		t.Fatalf("the main's server-kind devices: %+v, want the laptop", servers)
	}

	local := sub.device("todo")
	if local.welcome.Etag == nil || *local.welcome.Etag != v1 || local.welcome.Role != RoleSubordinate {
		t.Errorf("the local device's welcome %+v, want etag %s and role subordinate", local.welcome, v1)
	}
	phone := main.device("todo")
	v2 := phone.put(t, &v1, `{"v":2}`)

	waitFor(t, "the second pull", func() bool { return sub.etag("todo") == v2 })
	waitFor(t, "the local device to hear of it", func() bool { return local.heardEtag(v2) })
	waitFor(t, "the agreed etag to follow", func() bool { b, _ := sub.link.base("todo"); return same(b, &v2) })
	if st, err := sub.store.Stat("todo"); err != nil || st.Versions != 1 {
		t.Errorf("the subordinate's history: %+v, %v; want the replaced version kept", st, err)
	}

	saved, err := load(sub.linkDeps.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ServerID != main.id.ServerID || saved.Device != servers[0].ID || !same(saved.Apps["todo"].Base, &v2) {
		t.Errorf("upstream.json %+v, want the main, the device and base %s", saved, v2)
	}
	if fi, err := os.Stat(sub.linkDeps.Path); err == nil && fi.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Errorf("upstream.json mode %v, want readable by its owner only", fi.Mode().Perm())
	}
	st := sub.link.Status()
	if !st.Linked || !st.Connected || st.ServerID != main.id.ServerID || st.LastSync == nil || !slices.Equal(st.Apps, []string{"todo"}) {
		t.Errorf("status %+v, want linked and connected, todo synced, a last sync", st)
	}
	if main.link.Role() != RoleMain {
		t.Errorf("the main's role is %q", main.link.Role())
	}
}

// The main's app list: the subordinate adds the apps it lacks, syncs an app
// added on the main, and stops syncing one removed there while its data
// stays.
func TestAppList(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes"}); err != nil {
		t.Fatal(err)
	}
	notes := main.write("notes", `{"n":1}`, nil)

	sub.linkTo(main)
	waitFor(t, "notes to be added and pulled", func() bool { return sub.etag("notes") == notes })
	if a, ok := sub.apps.Get("notes"); !ok || a.Name != "Notes" {
		t.Errorf("the subordinate's notes app: %+v, %v; want it named Notes", a, ok)
	}

	if err := main.apps.Add(apps.App{ID: "chess", Name: "Chess"}); err != nil {
		t.Fatal(err)
	}
	main.hub.AppsChanged()
	waitFor(t, "chess to be added", func() bool { _, ok := sub.apps.Get("chess"); return ok })
	waitFor(t, "chess to sync", func() bool { return slices.Contains(sub.link.Status().Apps, "chess") })
	chess := main.write("chess", `{"c":1}`, nil)
	waitFor(t, "chess to be pulled", func() bool { return sub.etag("chess") == chess })

	if err := main.hub.RemoveApp("notes"); err != nil {
		t.Fatal(err)
	}
	main.hub.AppsChanged()
	waitFor(t, "notes to stop syncing", func() bool { return !slices.Contains(sub.link.Status().Apps, "notes") })
	if _, ok := sub.apps.Get("notes"); !ok || sub.etag("notes") != notes {
		t.Errorf("notes on the subordinate after its removal on the main: listed %v, etag %q; want both kept", ok, sub.etag("notes"))
	}
	waitFor(t, "notes' agreed etag to go", func() bool { _, ok := sub.link.base("notes"); return !ok })
	if !slices.Equal(sub.link.Status().Apps, []string{"chess", "todo"}) {
		t.Errorf("synced apps %v, want chess and todo", sub.link.Status().Apps)
	}
}

// Only a main can be an upstream: a server refuses to link to a subordinate,
// keeps no pairing, and leaves none behind there.
func TestASubordinateIsNoUpstream(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	other := startNode(t, "Tablet")
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	err := other.link.Pair(ctx, sub.code())
	var re *RefusedError
	if !errors.As(err, &re) || re.Reason != ReasonNotMain {
		t.Fatalf("link to a subordinate = %v, want refused as not a main", err)
	}
	if other.link.Role() != RoleMain || other.link.Status().Linked {
		t.Error("the refused server counts as linked")
	}
	if _, err := os.Stat(other.linkDeps.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upstream.json after a refusal: %v, want none", err)
	}
	waitFor(t, "the unconfirmed pairing to be undone on the subordinate", func() bool {
		return !slices.ContainsFunc(sub.devices.List(), devices.Device.IsServer)
	})
}

// Pair refuses what it cannot link to before it reaches anything.
func TestPairRefusals(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()

	if err := sub.link.Pair(ctx, "hello"); !errors.Is(err, ErrBadCode) {
		t.Errorf("a bad code: %v, want ErrBadCode", err)
	}
	if err := sub.link.Pair(ctx, sub.code()); !errors.Is(err, ErrOwnCode) {
		t.Errorf("its own code: %v, want ErrOwnCode", err)
	}
	used := main.code()
	main.code() // a new secret revokes the one in used
	var re *RefusedError
	if err := sub.link.Pair(ctx, used); !errors.As(err, &re) || re.Reason != "badSecret" {
		t.Errorf("a revoked code: %v, want refused with badSecret", err)
	}
	sub.linkTo(main)
	if err := sub.link.Pair(ctx, main.code()); !errors.Is(err, ErrLinked) {
		t.Errorf("a second link: %v, want ErrLinked", err)
	}
}

// This version only pulls: when the subordinate's own copy moved since the
// two agreed, the app waits, and the main's later write is not pulled over
// it.
func TestLocalChangesWait(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the first pull", func() bool { return sub.etag("todo") == v1 })
	waitFor(t, "the agreed etag", func() bool { b, _ := sub.link.base("todo"); return same(b, &v1) })

	local := sub.device("todo").put(t, &v1, `{"v":"laptop"}`)
	v2 := main.write("todo", `{"v":2}`, &v1)
	waitFor(t, "the wait to be logged", func() bool { return sub.logs.has("so it waits") })
	if got := sub.etag("todo"); got != local {
		t.Errorf("the subordinate's document is %s, want its own %s kept (the main's is %s)", got, local, v2)
	}
	if b, _ := sub.link.base("todo"); !same(b, &v1) {
		t.Errorf("agreed etag %v, want %s still", b, v1)
	}
}

// A server-kind device removed on the main halts the link: the main no
// longer knows this server.
func TestRemovedOnTheMainHalts(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })
	for _, d := range main.devices.List() {
		main.hub.RemoveDevice(d.ID)
	}
	waitFor(t, "the link to halt", func() bool { return sub.link.Status().Halted })
	if st := sub.link.Status(); st.Reason != "badToken" || st.Connected || !st.Linked {
		t.Errorf("status %+v, want halted for badToken, still linked", st)
	}
}

// The link comes back after the main's listener restarts, after a restart
// of the subordinate (from upstream.json), and at once after a network
// change; every time it pulls what it missed.
func TestReconnects(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })

	if err := main.l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to notice", func() bool { return !sub.link.Status().Connected })
	v1 := main.write("todo", `{"v":1}`, nil)
	main.listen()
	waitFor(t, "the pull after the main came back", func() bool { return sub.etag("todo") == v1 })

	sub.link.Close()
	v2 := main.write("todo", `{"v":2}`, &v1)
	sub.openLink()
	if sub.link.Role() != RoleSubordinate {
		t.Fatal("the link reopened from upstream.json is not a subordinate's")
	}
	waitFor(t, "the pull after the subordinate came back", func() bool { return sub.etag("todo") == v2 })

	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })
	before := sub.link.connectCount()
	sub.link.NetworkChanged()
	waitFor(t, "the reconnect", func() bool { return sub.link.connectCount() > before })
	v3 := main.write("todo", `{"v":3}`, &v2)
	waitFor(t, "the pull after the network changed", func() bool { return sub.etag("todo") == v3 })
}

// An invalid upstream.json stops the link from opening, and is left as it
// was.
func TestOpenRefusesADamagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upstream.json")
	for _, raw := range []string{
		`{`,
		`{"serverId":"short","password":"abcdefghijklmnopqrstuvwx","fingerprint":"00","port":1,"hosts":["a"],"device":"d","token":"t"}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(Deps{Path: path}); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("Open(%s) = %v, want an error naming the file", raw, err)
		}
		if got, _ := os.ReadFile(path); string(got) != raw {
			t.Error("the damaged file was changed")
		}
	}
	if l, err := Open(Deps{Path: filepath.Join(dir, "missing.json")}); err != nil || l.Role() != RoleMain || l.Status().Linked {
		t.Errorf("Open without a file = %v, want a main", err)
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }

func (l *Link) connectCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connects
}
