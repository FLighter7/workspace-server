package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/FLighter7/workspace-server/internal/protocol"
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
	up       *node // the main, once linkTo linked n to it

	// selfName and selfPort, when set, are what n announces instead of its
	// own name and port.
	selfName, selfPort atomic.Value

	cutMu sync.Mutex
	cut   map[int]bool // UDP ports n's link cannot reach: a split
}

// cutOff makes n unable to reach other (or able again), as a split of the
// network would.
func (n *node) cutOff(other *node, on bool) {
	n.cutMu.Lock()
	defer n.cutMu.Unlock()
	if n.cut == nil {
		n.cut = map[int]bool{}
	}
	n.cut[other.cfg.Port] = on
}

func (n *node) isCut(port int) bool {
	n.cutMu.Lock()
	defer n.cutMu.Unlock()
	return n.cut[port]
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
	n.cfg = rtc.Config{
		Addresses:   []net.IP{net.IPv4(127, 0, 0, 1)},
		Port:        freePort(t),
		ICEPwd:      n.id.ICEPwd,
		Certificate: cert,
		Logf:        func(format string, args ...any) { t.Logf(name+": rtc: "+format, args...) },
	}
	n.open()
	n.listen()
	return n
}

// open reads the server's files into new registries, a hub and a link, as a
// server start does, and starts the link.
func (n *node) open() {
	t, name := n.t, n.name
	t.Helper()
	var err error
	if n.apps, err = apps.Open(filepath.Join(n.dir, "apps.json")); err != nil { // lists "todo"
		t.Fatal(err)
	}
	if n.devices, err = devices.Open(filepath.Join(n.dir, "devices.json"), time.Now); err != nil {
		t.Fatal(err)
	}
	n.store = store.New(filepath.Join(n.dir, "apps"), time.Now, func(app string) int64 {
		a, _ := n.apps.Get(app)
		return a.Quota
	})
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
		Changed:  func(app string, _ *string) { n.cur.Load().LocalWrite(app) },
		Invite: func() (string, time.Time, error) {
			secret, expires := n.secrets.Issue()
			return n.codeWith(secret), expires, nil
		},
		Term: func() int { return n.cur.Load().Term() },
		Follow: func(sender string, term int, si protocol.ServerInfo, device, token string) (protocol.TermAnswer, error) {
			t, main, err := n.cur.Load().Follow(sender, term, wsclient.Server(si), device, token)
			return protocol.TermAnswer{Term: t, Main: main}, err
		},
	})
	n.cfg.Backend = n.hub
	n.linkDeps = Deps{
		Path:       filepath.Join(n.dir, "upstream.json"),
		FamilyPath: filepath.Join(n.dir, "family.json"),
		Devices:    n.devices,
		ServerID:   n.id.ServerID,
		Name:       func() string { return name },
		Apps:       n.apps,
		Store:      n.store,
		Writer:     n.hub,
		Dial: func(ctx context.Context, t wsclient.Target, app string) (*wsclient.Client, error) {
			if n.isCut(t.Port) {
				return nil, &wsclient.Error{Code: wsclient.CodeConnectTimeout, Message: "cut off by the test"}
			}
			return wsclient.Dial(ctx, t, app, wsclient.DialOptions{})
		},
		Self:       n.self,
		Admit:      n.hub.Replicate,
		Logf:       n.logs.printf,
		MinBackoff: 50 * time.Millisecond,
		MaxBackoff: 400 * time.Millisecond,
	}
	n.openLink()
}

// self is how devices reach n, as it announces it.
func (n *node) self() wsclient.Server {
	s := wsclient.Server{
		ServerID: n.id.ServerID, Name: n.name, Password: n.id.ICEPwd, Fingerprint: n.fpHex,
		Port: n.cfg.Port, Hosts: []string{"127.0.0.1"},
	}
	if v, ok := n.selfName.Load().(string); ok {
		s.Name = v
	}
	if v, ok := n.selfPort.Load().(int); ok {
		s.Port = v
	}
	return s
}

// restart stops n and starts it again from its files, on the same port.
func (n *node) restart() {
	n.t.Helper()
	n.link.Close()
	if err := n.l.Close(); err != nil {
		n.t.Fatal(err)
	}
	n.open()
	n.listen()
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
	return n.codeWith(secret)
}

// codeWith is a pairing code of n with secret; its only host is 127.0.0.1.
func (n *node) codeWith(secret string) string {
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
	n.up = up
}

// dataKey is the document most tests sync: data.json at the app's root, as
// todo keeps it.
const dataKey = "data.json"

// revOne is the revision of a set that holds data.json at etag alone.
func revOne(etag string) string { return *store.RevisionOf(map[string]string{dataKey: etag}) }

// etag is the etag of n's data.json of app, "" when there is none.
func (n *node) etag(app string) string { return n.etagOf(app, dataKey) }

// etagOf is the etag of n's document ks of app, "" when there is none.
func (n *node) etagOf(app, ks string) string { return n.set(app)[ks] }

// set is n's set of app, as etags by key.
func (n *node) set(app string) map[string]string {
	n.t.Helper()
	l, err := n.store.List(app)
	if err != nil {
		n.t.Fatal(err)
	}
	return setOf(l)
}

// write writes data.json of app on n, as a device would: n's sessions hear
// of it. It goes through the hub's own transaction for the link, which a
// write here would discard on a subordinate, so the tests write so on a
// main, or on a subordinate before it links.
func (n *node) write(app, body string, ifEtag *string) string {
	n.t.Helper()
	return n.writeKey(app, dataKey, body, ifEtag)
}

// writeKey writes the document ks of app on n as write does, and returns
// its etag.
func (n *node) writeKey(app, ks, body string, ifEtag *string) string {
	n.t.Helper()
	n.change(app, map[string]*string{ks: &body}, map[string]*string{ks: ifEtag})
	return store.Etag([]byte(body))
}

// removeKey deletes the document ks of app on n as write writes.
func (n *node) removeKey(app, ks string, ifEtag *string) {
	n.t.Helper()
	n.change(app, map[string]*string{ks: nil}, map[string]*string{ks: ifEtag})
}

// change writes bodies (nil: a delete) in one transaction, each with its
// ifEtag, as write does.
func (n *node) change(app string, bodies, ifEtags map[string]*string) {
	n.t.Helper()
	tx, _, err := n.hub.BeginLink(app, newTx())
	if err != nil {
		n.t.Fatalf("%s: begin: %v", n.name, err)
	}
	for ks, body := range bodies {
		k, _ := store.ParseKey(ks)
		if body == nil {
			if err := tx.Delete(k, ifEtags[ks]); err != nil {
				n.t.Fatalf("%s: delete %s: %v", n.name, ks, err)
			}
			continue
		}
		sink, err := tx.Put(k, ifEtags[ks], int64(len(*body)), 0)
		if err == nil {
			_, err = io.WriteString(sink, *body)
		}
		if err != nil {
			n.t.Fatalf("%s: put %s: %v", n.name, ks, err)
		}
		_ = sink.Close()
	}
	if _, err := tx.Commit(); err != nil {
		n.t.Fatalf("%s: commit: %v", n.name, err)
	}
}

// writeBehind writes data.json of app on n straight into the store, behind
// the hub: no session hears of it.
func (n *node) writeBehind(app, body string, ifEtag *string) string {
	n.t.Helper()
	tx := newTx()
	if _, err := n.store.Begin(app, "behind", tx); err != nil {
		n.t.Fatal(err)
	}
	k, _ := store.ParseKey(dataKey)
	sink, err := n.store.Put(app, "behind", tx, k, ifEtag, int64(len(body)), 0)
	if err != nil {
		n.t.Fatal(err)
	}
	if _, err := io.WriteString(sink, body); err != nil {
		n.t.Fatal(err)
	}
	if _, err := n.store.Commit(app, "behind", tx); err != nil {
		n.t.Fatal(err)
	}
	return store.Etag([]byte(body))
}

// device is a device paired with a server over WebRTC, which records the
// revisions of the changed pushes it hears ("" for an empty set).
type device struct {
	id      string
	cl      *wsclient.Client
	welcome wsclient.Welcome
	mu      sync.Mutex
	heard   []string
}

// device pairs a device of app and returns it connected to n. A
// subordinate pairs nothing: the device pairs with n's main and connects to
// n once n admits it on the main's word, as a phone does.
func (n *node) device(app string) *device {
	n.t.Helper()
	if n.up != nil && n.link.Role() == RoleSubordinate {
		id, token := n.up.pair(app)
		waitFor(n.t, "the subordinate to admit the device", func() bool { _, ok := n.devices.Get(id); return ok })
		return n.connect(app, id, token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	secret, _ := n.secrets.Issue()
	d := n.dial(ctx, app)
	p, w, err := d.cl.Pair(ctx, "", secret, "Phone")
	if err != nil {
		n.t.Fatalf("%s: pair a device: %v", n.name, err)
	}
	d.id, d.welcome = p.Device, w
	if _, err := d.cl.Head(ctx); err != nil { // confirms the pairing
		n.t.Fatal(err)
	}
	return d
}

// pair pairs a device of app with n and returns its id and token, without
// keeping a session.
func (n *node) pair(app string) (id, token string) {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	secret, _ := n.secrets.Issue()
	d := n.dial(ctx, app)
	defer d.cl.Close()
	p, _, err := d.cl.Pair(ctx, "", secret, "Phone")
	if err != nil {
		n.t.Fatalf("%s: pair a device: %v", n.name, err)
	}
	if _, err := d.cl.Head(ctx); err != nil { // confirms the pairing
		n.t.Fatal(err)
	}
	return p.Device, p.Token
}

// connect connects a paired device to n with hello.
func (n *node) connect(app, id, token string) *device {
	n.t.Helper()
	d, err := n.tryConnect(app, id, token)
	if err != nil {
		n.t.Fatalf("%s: hello: %v", n.name, err)
	}
	return d
}

// tryConnect connects a paired device to n with hello, and returns the
// error hello failed with.
func (n *node) tryConnect(app, id, token string) (*device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	d := n.dial(ctx, app)
	d.id = id
	var err error
	d.welcome, err = d.cl.Hello(ctx, id, token)
	return d, err
}

// dial opens a session to n for app that records the changed pushes it
// hears.
func (n *node) dial(ctx context.Context, app string) *device {
	n.t.Helper()
	cl, err := wsclient.Dial(ctx, n.target(), app, wsclient.DialOptions{})
	if err != nil {
		n.t.Fatalf("%s: dial: %v", n.name, err)
	}
	n.t.Cleanup(cl.Close)
	d := &device{cl: cl}
	cl.OnChanged(func(rev *string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if rev == nil {
			d.heard = append(d.heard, "")
			return
		}
		d.heard = append(d.heard, *rev)
	})
	return d
}

// heardEtag reports whether d heard that the set holds data.json at etag
// alone.
func (d *device) heardEtag(etag string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.heard, revOne(etag))
}

// put writes data.json, which must have ifEtag, in a transaction of its
// own, and returns its new etag.
func (d *device) put(t *testing.T, ifEtag *string, body string) string {
	t.Helper()
	return d.putKey(t, dataKey, ifEtag, body)
}

// putKey writes the document ks as put does.
func (d *device) putKey(t *testing.T, ks string, ifEtag *string, body string) string {
	t.Helper()
	d.commit(t, func(ctx context.Context, tx string) error {
		_, err := d.cl.Put(ctx, tx, wireKey(ks), ifEtag, int64(len(body)), 0, strings.NewReader(body))
		return err
	})
	return store.Etag([]byte(body))
}

// removeKey deletes the document ks, which must have ifEtag, in a
// transaction of its own.
func (d *device) removeKey(t *testing.T, ks string, ifEtag *string) {
	t.Helper()
	d.commit(t, func(ctx context.Context, tx string) error {
		_, err := d.cl.Delete(ctx, tx, wireKey(ks), ifEtag)
		return err
	})
}

// commit runs stage in a new transaction and commits it.
func (d *device) commit(t *testing.T, stage func(ctx context.Context, tx string) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	tx := newTx()
	if _, err := d.cl.Begin(ctx, tx); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := stage(ctx, tx); err != nil {
		t.Fatalf("stage: %v", err)
	}
	r, err := d.cl.Commit(ctx, tx)
	if err != nil || !r.Committed {
		t.Fatalf("commit: %+v, %v", r, err)
	}
}

func wireKey(ks string) wsclient.Key {
	k, _ := store.ParseKey(ks)
	return wsclient.Key{Path: k.Path, ID: k.ID, Type: k.Type}
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
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })

	servers := slices.DeleteFunc(main.devices.List(), func(d devices.Device) bool { return !d.IsServer() })
	if len(servers) != 1 || servers[0].Name != "Laptop" {
		t.Fatalf("the main's server-kind devices: %+v, want the laptop", servers)
	}

	local := sub.device("todo")
	if local.welcome.Revision == nil || *local.welcome.Revision != revOne(v1) || local.welcome.Role != RoleSubordinate {
		t.Errorf("the local device's welcome %+v, want the revision of %s and role subordinate", local.welcome, v1)
	}
	phone := main.device("todo")
	v2 := phone.put(t, &v1, `{"v":2}`)

	waitFor(t, "the second pull", func() bool { return sub.etag("todo") == v2 })
	waitFor(t, "the local device to hear of it", func() bool { return local.heardEtag(v2) })
	waitFor(t, "the agreed etag to follow", func() bool { return sub.agreed("todo", v2) })

	saved, err := load(sub.linkDeps.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ServerID != main.id.ServerID || saved.Device != servers[0].ID {
		t.Errorf("upstream.json %+v, want the main and the device", saved)
	}
	if lk := sub.linkFileOf("todo"); lk.ServerID != main.id.ServerID || !reflect.DeepEqual(lk.Base, map[string]string{dataKey: v2}) || lk.Pull != nil || lk.Push != nil {
		t.Errorf("link.json %+v, want the main and base {data.json: %s}", lk, v2)
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
	waitFor(t, "notes' base to go", func() bool { _, ok := sub.link.baseOf("notes"); return !ok })
	if !slices.Equal(sub.link.Status().Apps, []string{"chess", "todo"}) {
		t.Errorf("synced apps %v, want chess and todo", sub.link.Status().Apps)
	}
}

// Only a main can be an upstream: a subordinate refuses a server's pair as
// it refuses a device's (notMain), so the server keeps no pairing and leaves
// none behind there.
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

// written is the time of the last write to n's app, zero if it was never
// written: a write that changes nothing in the app's files moves it not.
func (n *node) written(app string) time.Time {
	n.t.Helper()
	st, err := n.store.Stats(app)
	if err != nil {
		n.t.Fatal(err)
	}
	return st.Modified
}

// agreed reports whether n's link has agreed with its main on a set of app
// that holds data.json at etag alone.
func (n *node) agreed(app, etag string) bool {
	return n.agreedOn(app, map[string]string{dataKey: etag})
}

// agreedOn reports whether n's link has agreed with its main on set for app.
func (n *node) agreedOn(app string, set map[string]string) bool {
	b, ok := n.link.baseOf(app)
	return ok && maps.Equal(b, set)
}

// baseOf is app's base with the main the link follows now, and whether
// there is one.
func (l *Link) baseOf(app string) (map[string]string, bool) {
	st := l.snapshot()
	if st == nil {
		return nil, false
	}
	lk := l.linkOf(app, st.ServerID)
	return lk.Base, lk.Base != nil
}

// linkFileOf reads n's link.json of app as it is on disk.
func (n *node) linkFileOf(app string) appLink {
	n.t.Helper()
	raw, err := n.store.ReadSide(app, linkFile)
	if err != nil {
		n.t.Fatal(err)
	}
	var lk appLink
	if err := json.Unmarshal(raw, &lk); err != nil {
		n.t.Fatal(err)
	}
	return lk
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

// What the link logs for each way documents move.
const (
	logPushed   = "pushed to the main"
	logPulled   = "pulled from the main"
	logReplaced = "the main's set replaced this server's"
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
	mainWritten, subWritten := main.written("todo"), sub.written("todo")

	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	time.Sleep(300 * time.Millisecond)
	if phone.heardCount() != 0 || tab.heardCount() != 0 {
		t.Errorf("devices heard %v on the main and %v here, want nothing written", phone.heard, tab.heard)
	}
	if !main.written("todo").Equal(mainWritten) || !sub.written("todo").Equal(subWritten) {
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
	waitFor(t, "the agreed etag to follow", func() bool { return sub.agreed("todo", v3) })
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
// subordinate, replacing the subordinate's, and its devices hear of it; a
// device's merge written back is pushed.
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
	if l2 == v2 {
		t.Fatal("the two writes have one etag")
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
// too: the main's copy wins, and replaces the subordinate's.
func TestFirstLinkWithDifferentDocuments(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	u1 := main.write("todo", `{"main":1}`, nil)
	sub.write("todo", `{"laptop":1}`, nil)
	tab := sub.device("todo")
	mainWritten := main.written("todo")

	sub.linkTo(main)
	waitFor(t, "the main's copy to win here", func() bool { return sub.etag("todo") == u1 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", u1) })
	waitFor(t, "the device here to hear of it", func() bool { return tab.heardEtag(u1) })
	if main.etag("todo") != u1 || !main.written("todo").Equal(mainWritten) {
		t.Error("the main's document was touched")
	}
	if !sub.logs.has("never agreed") {
		t.Error("the log does not say the two were never agreed on")
	}
}

// The main's document changes while the subordinate pushes: the main
// answers conflict, the subordinate decides again, and both have moved, so
// the main's copy wins here. To make the race certain, the main's document
// changes behind the hub while the push's body is on its way, after the
// subordinate listed the main's set.
func TestConflictDuringPush(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v1) })
	tab := sub.device("todo")

	v2 := store.Etag([]byte(`{"v":2}`))
	var once sync.Once
	sub.link.setWrapPush(func(r io.Reader, _ *wsclient.Client) io.Reader {
		once.Do(func() { main.writeBehind("todo", `{"v":2}`, &v1) })
		return r
	})
	tab.put(t, &v1, `{"v":1,"laptop":true}`)

	waitFor(t, "the main's copy to win here", func() bool { return sub.etag("todo") == v2 })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v2) })
	waitFor(t, "the device here to hear of it", func() bool { return tab.heardEtag(v2) })
	if !sub.logs.has("changed before this server's reached it") {
		t.Error("the conflict was not logged")
	}
	if main.etag("todo") != v2 {
		t.Error("the push overwrote the main's newer document")
	}

	merged := tab.put(t, &v2, `{"v":2,"laptop":true}`)
	waitFor(t, "the merge to reach the main", func() bool { return main.etag("todo") == merged })
	// The guard counts what the main committed: the merge, not the push
	// the conflict refused.
	waitFor(t, "the push to be logged", func() bool { return sub.logs.count(logPushed) == 1 })
	if n := sub.link.pushCount("todo"); n != 1 {
		t.Errorf("the guard counts %d pushes, want 1", n)
	}
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

	pushed, over := tripGuard(t, main, sub, tab, v0)
	if st := sub.link.Status(); !slices.Equal(st.Stopped, []string{"todo"}) {
		t.Errorf("stopped %v, want todo", st.Stopped)
	}
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

// tripGuard has tab, a device of todo on sub, write until the write guard
// stops todo: MaxPushesPerMinute documents, each pushed to main, then one
// more. It returns the last document pushed and the one the guard held
// back, which stays on sub.
func tripGuard(t *testing.T, main, sub *node, tab *device, from string) (pushed, over string) {
	t.Helper()
	prev := from
	for i := 1; i <= MaxPushesPerMinute; i++ {
		prev = tab.put(t, &prev, fmt.Sprintf(`{"n":%d}`, i))
		waitFor(t, fmt.Sprintf("push %d", i), func() bool { return main.etag("todo") == prev })
	}
	over = tab.put(t, &prev, `{"n":"over"}`)
	waitFor(t, "the guard to stop todo", func() bool { return slices.Contains(sub.link.Status().Stopped, "todo") })
	return prev, over
}

// setWrapPush and setWrapPull set what wraps the bodies the link pushes and
// pulls.
func (l *Link) setWrapPush(f func(r io.Reader, cl *wsclient.Client) io.Reader) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.wrapPush = f
}

func (l *Link) setWrapPull(f func(w io.Writer, cl *wsclient.Client) io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.wrapPull = f
}

// pushCount is how many pushes of app the guard counts in its minute now.
func (l *Link) pushCount(app string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pushes[app])
}

// Resume: an app the write guard stopped syncs with the main at once, by the
// sync rule (only the subordinate moved, so its document is pushed), over a
// new session, with a fresh count: the push at the resume and nine more go
// through, and the next stops it again, since the guard stays. Another app
// goes on all along, its count untouched. A restart still resumes too.
func TestResumeAfterTheWriteGuard(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := main.apps.Add(apps.App{ID: "notes", Name: "Notes"}); err != nil {
		t.Fatal(err)
	}
	v0 := main.write("todo", `{"n":0}`, nil)
	n0 := main.write("notes", `{"notes":0}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etags", func() bool { return sub.agreed("todo", v0) && sub.agreed("notes", n0) })
	tab, pad := sub.device("todo"), sub.device("notes")

	// notes pushes three times: a count of its own, which todo's stop and
	// resume leave alone.
	notes := n0
	for i := 1; i <= 3; i++ {
		notes = pad.put(t, &notes, fmt.Sprintf(`{"notes":%d}`, i))
		waitFor(t, fmt.Sprintf("notes push %d", i), func() bool { return main.etag("notes") == notes })
	}

	pushed, over := tripGuard(t, main, sub, tab, v0)
	if main.etag("todo") != pushed {
		t.Fatalf("the main's todo is %s, want %s", main.etag("todo"), pushed)
	}

	if err := sub.link.Resume("todo"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st := sub.link.Status(); len(st.Stopped) != 0 || !slices.Equal(st.Apps, []string{"notes", "todo"}) {
		t.Errorf("status after resume: apps %v, stopped %v; want both synced, none stopped", st.Apps, st.Stopped)
	}
	waitFor(t, "the held-back document to reach the main at once", func() bool { return main.etag("todo") == over })
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", over) })
	if !sub.logs.has(`app "todo": resumed after the write guard stopped it`) || !sub.logs.has("syncs with the main now") {
		t.Error("the resume was not logged")
	}
	if n := sub.link.pushCount("notes"); n != 3 {
		t.Errorf("notes' count is %d after todo's resume, want 3", n)
	}

	// A fresh count: the push at the resume was the first of ten.
	prev := over
	for i := 2; i <= MaxPushesPerMinute; i++ {
		prev = tab.put(t, &prev, fmt.Sprintf(`{"n":"again %d"}`, i))
		waitFor(t, fmt.Sprintf("push %d after the resume", i), func() bool { return main.etag("todo") == prev })
	}
	if st := sub.link.Status(); len(st.Stopped) != 0 {
		t.Fatalf("stopped %v after ten pushes since the resume", st.Stopped)
	}
	again := tab.put(t, &prev, `{"n":"over again"}`)
	waitFor(t, "the guard to stop todo again", func() bool { return slices.Equal(sub.link.Status().Stopped, []string{"todo"}) })
	if main.etag("todo") != prev {
		t.Errorf("the main's todo is %s, want %s: the push past the limit went through", main.etag("todo"), prev)
	}

	// notes synced all along.
	notes = main.write("notes", `{"notes":"main"}`, &notes)
	waitFor(t, "notes to go on syncing", func() bool { return sub.etag("notes") == notes })

	// A restart resumes, as it always did.
	sub.restart()
	waitFor(t, "the restart to resume todo", func() bool { return main.etag("todo") == again })
	if st := sub.link.Status(); len(st.Stopped) != 0 {
		t.Errorf("stopped %v after a restart", st.Stopped)
	}
}

// A resume while the main is out of reach takes effect at once, and the app
// syncs as soon as the link is back.
func TestResumeWhileTheMainIsAway(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v0 := main.write("todo", `{"n":0}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v0) })
	tab := sub.device("todo")
	pushed, over := tripGuard(t, main, sub, tab, v0)

	if err := main.l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to notice", func() bool { return !sub.link.Status().Connected })
	if err := sub.link.Resume("todo"); err != nil {
		t.Fatalf("resume with the main away: %v", err)
	}
	if st := sub.link.Status(); len(st.Stopped) != 0 {
		t.Errorf("stopped %v after the resume", st.Stopped)
	}
	if !sub.logs.has("syncs with the main once the main is reachable") {
		t.Error("the log does not say the app syncs once the main is reachable")
	}
	if main.etag("todo") != pushed {
		t.Fatal("the main's document changed while it was away")
	}
	main.listen()
	waitFor(t, "the push once the main is back", func() bool { return main.etag("todo") == over })
}

// Resume refuses a server that is not linked, an app that is not stopped or
// not served here, and, changing nothing, a halted link.
func TestResumeRefusals(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	if err := sub.link.Resume("todo"); !errors.Is(err, ErrNotLinked) {
		t.Errorf("resume before linking: %v, want ErrNotLinked", err)
	}
	v0 := main.write("todo", `{"n":0}`, nil)
	sub.linkTo(main)
	waitFor(t, "the agreed etag", func() bool { return sub.agreed("todo", v0) })
	if err := sub.link.Resume("todo"); !errors.Is(err, ErrNotStopped) {
		t.Errorf("resume of an app that syncs: %v, want ErrNotStopped", err)
	}
	if err := sub.link.Resume("nope"); !errors.Is(err, apps.ErrNotFound) {
		t.Errorf("resume of an app not served here: %v, want apps.ErrNotFound", err)
	}

	tab := sub.device("todo")
	tripGuard(t, main, sub, tab, v0)
	for _, d := range main.devices.List() {
		if d.IsServer() {
			main.hub.RemoveDevice(d.ID)
		}
	}
	waitFor(t, "the link to halt", func() bool { return sub.link.Status().Halted })
	var re *RefusedError
	if err := sub.link.Resume("todo"); !errors.As(err, &re) || re.Reason != "badToken" {
		t.Errorf("resume on a halted link: %v, want a refusal for badToken", err)
	}
	if st := sub.link.Status(); !slices.Equal(st.Stopped, []string{"todo"}) {
		t.Errorf("stopped %v after a refused resume, want todo still", st.Stopped)
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
	push := func(app string) bool {
		if !l.mayPush(app) {
			return false
		}
		l.countPush(app)
		return true
	}
	if !l.mayPush("todo") || !l.mayPush("todo") {
		t.Fatal("asking counts a push")
	}
	for i := range MaxPushesPerMinute {
		if !push("todo") {
			t.Fatalf("push %d refused", i+1)
		}
		now = now.Add(time.Second)
	}
	if push("todo") {
		t.Error("a push past the limit within a minute was allowed")
	}
	if !push("notes") {
		t.Error("another app's push was refused")
	}
	now = start.Add(pushWindow)
	if !push("todo") {
		t.Error("a push was refused once the first one left the window")
	}
	if push("todo") {
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

	type counts struct {
		phone, tab          int
		mainWrote, subWrote time.Time
		moves               int
	}
	snap := func() counts {
		return counts{
			phone.heardCount(), tab.heardCount(), main.written("todo"), sub.written("todo"),
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
