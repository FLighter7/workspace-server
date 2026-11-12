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
	"sync/atomic"
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
	cur      atomic.Pointer[Link] // link, for the hub's goroutines
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
		Role:     func() string { return n.cur.Load().Role() },
		Logf:     n.logs.printf,
		Stored:   func(app string) { n.cur.Load().LocalWrite(app) },
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
	n.cur.Store(l)
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

// historyHolds reports whether a version in n's history of app holds body.
func (n *node) historyHolds(app, body string) bool {
	dir := filepath.Join(n.dir, "apps", app, "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && string(b) == body {
			return true
		}
	}
	return false
}

// versions is the number of versions in n's history of app.
func (n *node) versions(app string) int {
	n.t.Helper()
	st, err := n.store.Stat(app)
	if err != nil {
		n.t.Fatal(err)
	}
	return st.Versions
}

// agreed reports whether n's link has agreed with its main on etag for app.
func (n *node) agreed(app, etag string) bool {
	b, ok := n.link.base(app)
	return ok && b != nil && *b == etag
}

func (b *logBuf) count(sub string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, l := range b.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func (d *device) heardCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.heard)
}

// What the link logs for each way a document moves.
const (
	logPushed   = "pushed this server's document"
	logPulled   = "pulled the main's document"
	logReplaced = "replaced this server's"
)

// The row "the same etag on both sides": two servers holding the same bytes
// before their first link agree on them, and neither writes anything.
func TestSameDocumentsAgree(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	if got := sub.write("todo", `{"v":1}`, nil); got != v1 {
		t.Fatalf("the same bytes have etags %s and %s", got, v1)
	}
	phone, tab := main.device("todo"), sub.device("todo")

	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	time.Sleep(300 * time.Millisecond)
	if phone.heardCount() != 0 || tab.heardCount() != 0 {
		t.Errorf("devices heard %v on the main and %v here, want nothing written", phone.heard, tab.heard)
	}
	if main.versions("todo") != 0 || sub.versions("todo") != 0 {
		t.Error("a document was rewritten")
	}
	if n := sub.logs.count(logPushed) + sub.logs.count(logPulled) + sub.logs.count(logReplaced); n != 0 {
		t.Errorf("%d documents moved, want none", n)
	}
}

// The row "only the subordinate moved": a device's write on the subordinate
// is pushed to the main at once, conditional on the agreed etag, and becomes
// the new agreed etag; the main's devices hear of it, and nothing comes
// back.
func TestPushWhenOnlyTheSubordinateMoved(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	phone, tab := main.device("todo"), sub.device("todo")

	v2 := tab.put(t, &v1, `{"v":2}`)
	waitFor(t, "the push", func() bool { return main.etag("todo") == v2 })
	waitFor(t, "the agreed etag to follow", func() bool { return sub.agreed("todo", v2) })
	waitFor(t, "the main's device to hear of it", func() bool { return phone.heardEtag(v2) })
	if !main.historyHolds("todo", `{"v":1}`) {
		t.Error("the main's replaced version is not in its history")
	}
	if st := sub.link.Status(); st.LastSync == nil {
		t.Error("no last sync after a push")
	}
	time.Sleep(300 * time.Millisecond)
	if tab.heardCount() != 0 {
		t.Errorf("the writing device heard %v, want nothing: the push writes nothing here", tab.heard)
	}

	// Again, at once: the hook after every write, not a reconnect.
	v3 := tab.put(t, &v2, `{"v":3}`)
	waitFor(t, "the second push", func() bool { return main.etag("todo") == v3 })
	if n := sub.logs.count(logPushed); n != 2 {
		t.Errorf("%d pushes, want 2", n)
	}
}

// A first link where only the subordinate has a document pushes it: a main
// with no document never empties the subordinate.
func TestFirstLinkPushesWhenOnlyTheSubordinateHasADocument(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	l1 := sub.write("todo", `{"l":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the push", func() bool { return main.etag("todo") == l1 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", l1) })
}

// The row "both moved": while the two could not reach each other, a device
// wrote on each. When the link comes back, the main's copy wins on the
// subordinate, the subordinate's goes to its history, and its devices hear
// of it; a device's merge written back is pushed.
func TestBothMovedTheMainWins(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	phone, tab := main.device("todo"), sub.device("todo")

	sub.link.Close()
	v2 := phone.put(t, &v1, `{"v":2}`)
	l2 := tab.put(t, &v1, `{"v":1,"laptop":true}`)
	sub.openLink()

	waitFor(t, "the main's copy to win here", func() bool { return sub.etag("todo") == v2 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v2) })
	waitFor(t, "the device here to hear of it", func() bool { return tab.heardEtag(v2) })
	if !sub.historyHolds("todo", `{"v":1,"laptop":true}`) {
		t.Errorf("the subordinate's own version %s is not in its history", l2[:8])
	}
	if main.etag("todo") != v2 {
		t.Error("the main's document changed")
	}
	if !sub.logs.has(logReplaced) {
		t.Error("the replacement was not logged")
	}

	// The device merges, as an app does, and writes the result back.
	merged := tab.put(t, &v2, `{"v":2,"laptop":true}`)
	waitFor(t, "the merge to reach the main", func() bool { return main.etag("todo") == merged })
	waitFor(t, "the main's device to hear of it", func() bool { return phone.heardEtag(merged) })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", merged) })
}

// The first link with different documents on both sides is a divergence
// too: the main's copy wins, and the subordinate's is kept in its history.
func TestFirstLinkWithDifferentDocuments(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	u1 := main.write("todo", `{"main":1}`, nil)
	sub.write("todo", `{"laptop":1}`, nil)
	tab := sub.device("todo")

	sub.linkTo(main)
	waitFor(t, "the main's copy to win here", func() bool { return sub.etag("todo") == u1 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", u1) })
	waitFor(t, "the device here to hear of it", func() bool { return tab.heardEtag(u1) })
	if !sub.historyHolds("todo", `{"laptop":1}`) {
		t.Error("the subordinate's own document is not in its history")
	}
	if main.etag("todo") != u1 || main.versions("todo") != 0 {
		t.Error("the main's document was touched")
	}
	if !sub.logs.has("never agreed") {
		t.Error("the log does not say the two were never agreed on")
	}
}

// The main's document changes while the subordinate pushes: the main
// answers conflict, the subordinate decides again, and both have moved, so
// the main's copy wins here. To make the race certain, the main's document
// changes behind the hub, so the subordinate hears no changed and pushes
// against an etag the main no longer has.
func TestConflictDuringPush(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	a, _ := main.apps.Get("todo")
	v2, err := main.store.Write("todo", []byte(`{"v":2}`), &v1, a.History)
	if err != nil {
		t.Fatal(err)
	}
	tab.put(t, &v1, `{"v":1,"laptop":true}`)

	waitFor(t, "the main's copy to win here", func() bool { return sub.etag("todo") == v2 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v2) })
	waitFor(t, "the device here to hear of it", func() bool { return tab.heardEtag(v2) })
	if !sub.logs.has("changed before this server's reached it") {
		t.Error("the conflict was not logged")
	}
	if !sub.historyHolds("todo", `{"v":1,"laptop":true}`) {
		t.Error("the subordinate's own version is not in its history")
	}
	if main.etag("todo") != v2 {
		t.Error("the push overwrote the main's newer document")
	}

	merged := tab.put(t, &v2, `{"v":2,"laptop":true}`)
	waitFor(t, "the merge to reach the main", func() bool { return main.etag("todo") == merged })
}

// Writes made on the subordinate while the main was out of reach are pushed
// once the link is back, the last of them only.
func TestPushAfterReconnect(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	if err := main.l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to notice", func() bool { return !sub.link.Status().Connected })
	l2 := tab.put(t, &v1, `{"v":"away","n":1}`)
	l3 := tab.put(t, &l2, `{"v":"away","n":2}`)
	main.listen()

	waitFor(t, "the push after the main came back", func() bool { return main.etag("todo") == l3 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", l3) })
	if n := sub.logs.count(logPushed); n != 1 {
		t.Errorf("%d pushes, want 1: the latest document only", n)
	}
	if main.historyHolds("todo", `{"v":"away","n":1}`) {
		t.Error("the main stored a version the subordinate had already replaced")
	}
}

// The write guard: past MaxPushesPerMinute pushes of one app within a
// minute, that app stops syncing (no session to the main, nothing pushed or
// pulled), the status lists it, and the other apps go on.
func TestWriteGuardStopsTheApp(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes"}); err != nil {
		t.Fatal(err)
	}
	v0 := main.write("todo", `{"n":0}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v0) })
	tab := sub.device("todo")

	prev := v0
	for i := 1; i <= MaxPushesPerMinute; i++ {
		prev = tab.put(t, &prev, fmt.Sprintf(`{"n":%d}`, i))
		waitFor(t, fmt.Sprintf("push %d", i), func() bool { return main.etag("todo") == prev })
	}
	pushed := prev
	over := tab.put(t, &prev, `{"n":"over"}`)
	waitFor(t, "the guard to stop todo", func() bool { return slices.Equal(sub.link.Status().Stopped, []string{"todo"}) })
	waitFor(t, "todo's session to the main to end", func() bool {
		return !slices.ContainsFunc(main.hub.Sessions(), func(si hub.SessionInfo) bool { return si.App == "todo" })
	})
	if main.etag("todo") != pushed {
		t.Errorf("the main's todo is %s, want %s: the push past the limit went through", main.etag("todo"), pushed)
	}
	if !sub.logs.has("stopped syncing with the main") {
		t.Error("the stop was not logged")
	}
	if st := sub.link.Status(); !slices.Equal(st.Apps, []string{"notes"}) || st.Halted {
		t.Errorf("status %+v, want notes still synced and the link not halted", st)
	}

	main.write("todo", `{"main":true}`, &pushed)
	notes := main.write("notes", `{"notes":1}`, nil)
	waitFor(t, "notes to go on syncing", func() bool { return sub.etag("notes") == notes })
	if sub.etag("todo") != over {
		t.Error("a stopped app pulled the main's document")
	}
}

// The guard counts one app's pushes over a sliding minute.
func TestWriteGuardWindow(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	start := now
	l, err := Open(Deps{Path: filepath.Join(t.TempDir(), "upstream.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxPushesPerMinute {
		if !l.allowPush("todo") {
			t.Fatalf("push %d refused", i+1)
		}
		now = now.Add(time.Second)
	}
	if l.allowPush("todo") {
		t.Error("a push past the limit within a minute was allowed")
	}
	if !l.allowPush("notes") {
		t.Error("another app's push was refused")
	}
	now = start.Add(pushWindow)
	if !l.allowPush("todo") {
		t.Error("a push was refused once the first one left the window")
	}
	if l.allowPush("todo") {
		t.Error("a second push was allowed with ten still in the window")
	}
}

// No ping-pong: once a push, a pull and a divergence have settled, both
// servers stay quiet. A document copied from the main is not pushed back,
// and a push brings nothing back.
func TestNoWritePingPong(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	phone, tab := main.device("todo"), sub.device("todo")

	pushed := tab.put(t, &v1, `{"v":2}`)
	waitFor(t, "the push", func() bool { return main.etag("todo") == pushed })
	pulled := phone.put(t, &pushed, `{"v":3}`)
	waitFor(t, "the pull", func() bool { return sub.etag("todo") == pulled })

	sub.link.Close()
	v4 := phone.put(t, &pulled, `{"v":4}`)
	tab.put(t, &pulled, `{"v":3,"laptop":true}`)
	sub.openLink()
	waitFor(t, "the main's copy to win here", func() bool { return sub.etag("todo") == v4 })
	merged := tab.put(t, &v4, `{"v":4,"laptop":true}`)
	waitFor(t, "convergence", func() bool {
		return main.etag("todo") == merged && sub.etag("todo") == merged && sub.agreed("todo", merged)
	})

	type counts struct{ phone, tab, mainVersions, subVersions, moves int }
	snap := func() counts {
		return counts{
			phone.heardCount(), tab.heardCount(), main.versions("todo"), sub.versions("todo"),
			sub.logs.count(logPushed) + sub.logs.count(logPulled) + sub.logs.count(logReplaced),
		}
	}
	before := snap()
	time.Sleep(1500 * time.Millisecond)
	if after := snap(); after != before {
		t.Errorf("writes after convergence: before %+v, after %+v", before, after)
	}
	if n := sub.logs.count(logPushed); n != 2 {
		t.Errorf("%d pushes, want 2 (the first write here and the merge)", n)
	}
	if n := sub.logs.count(logPulled); n != 2 {
		t.Errorf("%d pulls, want 2 (the first link and the main's write)", n)
	}
	if n := sub.logs.count(logReplaced); n != 1 {
		t.Errorf("%d replacements, want 1", n)
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
