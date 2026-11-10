package rtc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/hub"
	"github.com/FLighter7/workspace-server/internal/identity"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
)

// testDeadline bounds every test that waits for a connection.
const testDeadline = 20 * time.Second

// obj is a decoded text frame; JSON numbers decode as float64.
type obj = map[string]any

func TestStandInOffer(t *testing.T) {
	want := strings.Join([]string{
		"v=0",
		"o=- 0 2 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"a=group:BUNDLE 0",
		"a=msid-semantic: WMS",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"c=IN IP4 0.0.0.0",
		"a=ice-ufrag:Ab3/",
		"a=ice-pwd:placeholderplaceholder00",
		"a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
		"a=setup:actpass",
		"a=mid:0",
		"a=sctp-port:5000",
		"a=max-message-size:262144",
	}, "\r\n") + "\r\n"
	if got := standInOffer("Ab3/"); got != want {
		t.Fatalf("standInOffer =\n%q\nwant\n%q", got, want)
	}
}

func TestValidUfrag(t *testing.T) {
	for s, want := range map[string]bool{
		"Ab3/":                   true,
		"aB3+xY9z":               true,
		strings.Repeat("a", 256): true,
		"abc":                    false,
		strings.Repeat("a", 257): false,
		"abc\r\na=x":             false,
		"ab cd":                  false,
		"abcd:":                  false,
		"":                       false,
	} {
		if got := validUfrag(s); got != want {
			t.Errorf("validUfrag(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestPairPutAndChangedOverLoopback(t *testing.T) {
	s := startServer(t, nil)
	deadline := time.Now().Add(testDeadline)

	a := dial(t, s, s.cfg.ICEPwd)
	b := dial(t, s, s.cfg.ICEPwd)
	a.waitOpen(deadline)
	b.waitOpen(deadline)

	for _, c := range []struct {
		cl   *client
		name string
	}{{a, "Phone"}, {b, "Laptop"}} {
		welcome := c.cl.pair(deadline, s.secrets, c.name)
		if etag, ok := welcome["etag"]; !ok || etag != nil {
			t.Fatalf("%s: welcome = %v, want etag null", c.name, welcome)
		}
		if welcome["serverId"] != s.serverID {
			t.Fatalf("%s: welcome serverId = %v, want %q", c.name, welcome["serverId"], s.serverID)
		}
	}

	body := jsonBody(t, 40000)
	b.send(obj{"t": "put", "ifEtag": nil, "bytes": len(body)})
	for off := 0; off < len(body); off += protocol.ChunkBytes {
		if err := b.dc.Send(body[off:min(off+protocol.ChunkBytes, len(body))]); err != nil {
			t.Fatalf("send body: %v", err)
		}
	}
	stored := b.frame(deadline, "stored")
	etag, _ := stored["etag"].(string)
	if etag != store.Etag(body) {
		t.Fatalf("stored = %v, want etag %q", stored, store.Etag(body))
	}

	if changed := a.frame(deadline, "changed"); changed["etag"] != etag {
		t.Fatalf("changed = %v, want etag %q", changed, etag)
	}

	a.send(obj{"t": "get"})
	doc := a.frame(deadline, "doc")
	if doc["etag"] != etag || doc["bytes"] != float64(len(body)) {
		t.Fatalf("doc = %v, want etag %q and bytes %d", doc, etag, len(body))
	}
	if got := a.body(deadline, len(body)); !bytes.Equal(got, body) {
		t.Fatalf("get returned %d bytes that differ from the %d bytes put", len(got), len(body))
	}

	if n := s.l.ActiveSessions(); n != 2 {
		t.Errorf("ActiveSessions() = %d, want 2", n)
	}
}

// TestBadTokenErrorReachesClient checks that the server lets its final error
// frame reach the client before tearing the connection down, so the client
// can tell "pair again" from a dropped network.
//
// On loopback the frame arrives before the peer connection closes even
// without the drain, so TestTeardownDrainsTheDataChannel pins the drain.
func TestBadTokenErrorReachesClient(t *testing.T) {
	s := startServer(t, nil)
	deadline := time.Now().Add(testDeadline)

	c := dial(t, s, s.cfg.ICEPwd)
	c.waitOpen(deadline)
	c.send(obj{"t": "hello", "proto": 1, "app": "todo", "device": "nosuchdevice0000", "token": "wrong"})
	if f := c.frame(deadline, "error"); f["code"] != protocol.CodeBadToken {
		t.Fatalf("error = %v, want code %q", f, protocol.CodeBadToken)
	}
	c.waitClosed(deadline)
}

func TestWrongPasswordIsIgnored(t *testing.T) {
	s := startServer(t, nil)
	from := netip.MustParseAddrPort("127.0.0.1:5000")

	// Admission happens inside onUnhandled, so its effect shows at once.
	s.l.onUnhandled(bindingRequest(t, "wrongpw1", identity.RandomString(24)), from)
	if n := s.l.ActiveSessions(); n != 0 {
		t.Fatalf("ActiveSessions() = %d after a wrong-password check, want 0", n)
	}
	s.l.onUnhandled(bindingRequest(t, "rightpw1", s.cfg.ICEPwd), from)
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d after a right-password check, want 1", n)
	}
	endConn(t, s.l, "rightpw1")

	// A real client with the wrong password does not connect; one with the
	// right password does.
	wrong := dial(t, s, identity.RandomString(24))
	right := dial(t, s, s.cfg.ICEPwd)
	right.waitOpen(time.Now().Add(testDeadline))
	if state := wrong.pc.ConnectionState(); state == webrtc.PeerConnectionStateConnected {
		t.Fatalf("wrong-password client is %s", state)
	}
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d, want only the right-password client", n)
	}
}

// TestLateCheckForAnEndedConnection checks that a connectivity check for the
// ufrag of a connection that ended lately starts no new connection.
func TestLateCheckForAnEndedConnection(t *testing.T) {
	s := startServer(t, nil)
	now := time.Now()
	s.l.mu.Lock()
	s.l.now = func() time.Time { return now }
	s.l.mu.Unlock()
	from := netip.MustParseAddrPort("127.0.0.1:5000")
	check := bindingRequest(t, "ghost123", s.cfg.ICEPwd)

	s.l.onUnhandled(check, from)
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d, want 1", n)
	}
	endConn(t, s.l, "ghost123")

	s.l.onUnhandled(check, from)
	if n := s.l.ActiveSessions(); n != 0 {
		t.Fatalf("ActiveSessions() = %d after a late check, want 0", n)
	}
	s.l.onUnhandled(bindingRequest(t, "other123", s.cfg.ICEPwd), from)
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d for a new ufrag, want 1", n)
	}
	endConn(t, s.l, "other123")

	s.l.mu.Lock()
	now = now.Add(recentTTL)
	s.l.mu.Unlock()
	s.l.onUnhandled(check, from)
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d once the ufrag aged out, want 1", n)
	}
}

func TestRecentUfrags(t *testing.T) {
	var r recentUfrags
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r.add("a", t0)
	r.add("b", t0.Add(30*time.Second))
	if !r.has("a", t0.Add(59*time.Second)) || !r.has("b", t0.Add(59*time.Second)) {
		t.Fatal("a fresh ufrag is not remembered")
	}
	if r.has("a", t0.Add(time.Minute)) {
		t.Error("a is remembered after recentTTL")
	}
	if !r.has("b", t0.Add(time.Minute)) {
		t.Error("b is forgotten before recentTTL")
	}
	if r.has("c", t0) {
		t.Error("an unknown ufrag is remembered")
	}

	for i := range recentMax + 10 {
		r.add(fmt.Sprint("u", i), t0.Add(2*time.Minute))
	}
	if len(r.at) != recentMax || len(r.order) != recentMax {
		t.Errorf("%d ufrags kept (%d in order), want %d", len(r.at), len(r.order), recentMax)
	}
	if r.has("u0", t0.Add(2*time.Minute)) || !r.has(fmt.Sprint("u", recentMax+9), t0.Add(2*time.Minute)) {
		t.Error("the cap did not drop the oldest ufrags")
	}
}

// fakePeer stands in for a peer connection in teardown.
type fakePeer struct {
	dcClosed <-chan struct{}
	closed   chan struct{}
	dcFirst  bool // the data channel had closed when Close was called
}

func (p *fakePeer) Close() error {
	select {
	case <-p.dcClosed:
		p.dcFirst = true
	default:
	}
	close(p.closed)
	return nil
}

func TestTeardownDrainsTheDataChannel(t *testing.T) {
	newConn := func(drain bool) (*conn, *fakePeer) {
		c := &conn{l: &Listener{cfg: Config{Logf: t.Logf}}, dcClosed: make(chan struct{}), drain: drain}
		return c, &fakePeer{dcClosed: c.dcClosed, closed: make(chan struct{})}
	}
	defer func(g time.Duration) { closeGrace = g }(closeGrace)

	t.Run("waits for the data channel", func(t *testing.T) {
		closeGrace = time.Hour
		c, pc := newConn(true)
		done := make(chan struct{})
		go func() { c.teardown(pc); close(done) }()
		select {
		case <-pc.closed:
			t.Fatal("the peer connection closed before the data channel")
		case <-time.After(100 * time.Millisecond):
		}
		close(c.dcClosed)
		<-done
		if !pc.dcFirst {
			t.Error("the peer connection closed before the data channel")
		}
	})
	t.Run("gives up after the grace", func(t *testing.T) {
		closeGrace = 50 * time.Millisecond
		c, pc := newConn(true)
		start := time.Now()
		c.teardown(pc)
		if d := time.Since(start); d < closeGrace {
			t.Errorf("closed after %v, want at least %v", d, closeGrace)
		}
	})
	t.Run("no drain asked", func(t *testing.T) {
		closeGrace = time.Hour
		c, pc := newConn(false)
		done := make(chan struct{})
		go func() { c.teardown(pc); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("teardown waited although no drain was asked")
		}
	})
}

func TestCloseAndReopenSamePort(t *testing.T) {
	s := startServer(t, nil)
	deadline := time.Now().Add(testDeadline)

	first := dial(t, s, s.cfg.ICEPwd)
	first.waitOpen(deadline)
	if err := s.l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := s.l.ActiveSessions(); n != 0 {
		t.Fatalf("ActiveSessions() after Close = %d, want 0", n)
	}
	first.waitClosed(deadline) // Close closed its peer connection
	// Pion already closes the client's peer connection in the background on
	// the server's DTLS close_notify; wait until that is done, so that no
	// late connectivity check of the old connection reaches the new listener.
	_ = first.pc.GracefulClose()

	l, err := Listen(s.cfg)
	if err != nil {
		t.Fatalf("Listen again on port %d: %v", s.cfg.Port, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s.l = l

	second := dial(t, s, s.cfg.ICEPwd)
	second.waitOpen(deadline)
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d, want 1", n)
	}
}

// The same address on two interfaces is bound once.
func TestListenOnADuplicatedAddress(t *testing.T) {
	startServer(t, func(cfg *Config) {
		cfg.Addresses = []net.IP{net.IPv4(127, 0, 0, 1), net.ParseIP("127.0.0.1")}
	})
}

func TestSessionCap(t *testing.T) {
	s := startServer(t, func(cfg *Config) { cfg.MaxSessions = 1 })
	deadline := time.Now().Add(testDeadline)
	from := netip.MustParseAddrPort("127.0.0.1:5000")

	// The cap is checked inside onUnhandled, so its effect shows at once.
	s.l.onUnhandled(bindingRequest(t, "first123", s.cfg.ICEPwd), from)
	s.l.onUnhandled(bindingRequest(t, "second12", s.cfg.ICEPwd), from)
	s.l.mu.Lock()
	n, second := len(s.l.conns), s.l.conns["second12"]
	s.l.mu.Unlock()
	if n != 1 || second != nil {
		t.Fatalf("%d connections (second one started: %v), want only the first", n, second != nil)
	}

	// The slot frees once the first connection ends.
	endConn(t, s.l, "first123")
	s.l.onUnhandled(bindingRequest(t, "second12", s.cfg.ICEPwd), from)
	if n := s.l.ActiveSessions(); n != 1 {
		t.Fatalf("ActiveSessions() = %d after the slot freed, want 1", n)
	}
	endConn(t, s.l, "second12")

	// End to end: a real client takes the free slot.
	c := dial(t, s, s.cfg.ICEPwd)
	c.waitOpen(deadline)
}

// TestConnectDeadline checks that a connection which never opens a data
// channel is closed at the connect deadline. The client's channel is
// pre-negotiated, so the server never hears of it, although DTLS and SCTP
// come up.
func TestConnectDeadline(t *testing.T) {
	s := startServer(t, func(cfg *Config) { cfg.ConnectTimeout = time.Second })
	deadline := time.Now().Add(testDeadline)

	negotiated, id := true, uint16(1)
	c := dialWith(t, s, s.cfg.ICEPwd, &webrtc.DataChannelInit{Negotiated: &negotiated, ID: &id})
	select {
	case <-c.connected:
	case <-time.After(time.Until(deadline)):
		t.Fatal("the client did not connect")
	}
	select {
	case <-c.pcClosed: // Pion closes on the server's DTLS close_notify
	case <-time.After(time.Until(deadline)):
		t.Fatal("the server did not close a connection that opened no data channel")
	}
	waitFor(t, deadline, "the connection's slot to free", func() bool { return s.l.ActiveSessions() == 0 })
}

// testServer is a listener on loopback with the real registries, store and
// hub behind it.
type testServer struct {
	l        *Listener
	cfg      Config
	serverID string
	fp       string // certificate fingerprint, uppercase colon hex
	secrets  *pairing.Secrets
}

// startServer starts a listener with the defaults the server uses, changed by
// tweak when it is not nil.
func startServer(t *testing.T, tweak func(*Config)) *testServer {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := id.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	fp, err := id.FingerprintHex()
	if err != nil {
		t.Fatal(err)
	}
	appList, err := apps.Open(filepath.Join(dir, "apps.json")) // lists "todo"
	if err != nil {
		t.Fatal(err)
	}
	devs, err := devices.Open(filepath.Join(dir, "devices.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	secrets := pairing.NewSecrets(time.Now)
	h := hub.New(hub.Deps{
		ServerID: id.ServerID,
		Name:     func() string { return "Test server" },
		Hosts:    func() []string { return []string{"127.0.0.1"} },
		Apps:     appList,
		Devices:  devs,
		Store:    store.New(filepath.Join(dir, "apps"), time.Now),
		Secrets:  secrets,
		Refusals: pairing.NewRefusals(time.Now),
		Now:      time.Now,
	})

	cfg := Config{
		Addresses:      []net.IP{net.IPv4(127, 0, 0, 1)},
		Port:           freePort(t),
		ICEPwd:         id.ICEPwd,
		Certificate:    cert,
		Backend:        h,
		MaxSessions:    64,
		ConnectTimeout: 20 * time.Second,
		AuthTimeout:    10 * time.Second,
		Logf:           t.Logf,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	l, err := Listen(cfg)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	return &testServer{l: l, cfg: cfg, serverID: id.ServerID, fp: fp, secrets: secrets}
}

// freePort returns a UDP port on loopback that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	return port
}

// client is a Pion peer connection playing the browser: it builds the
// server's answer itself and never talks to the server outside WebRTC.
type client struct {
	t         *testing.T
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	connected chan struct{} // the peer connection reached connected
	pcClosed  chan struct{} // the peer connection reached closed
	open      chan struct{}
	closed    chan struct{}
	msgs      chan webrtc.DataChannelMessage
}

// dial starts connecting to s with icePwd as the server's ICE password.
func dial(t *testing.T, s *testServer, icePwd string) *client {
	t.Helper()
	return dialWith(t, s, icePwd, nil)
}

// dialWith is dial with init for the client's data channel.
func dialWith(t *testing.T, s *testServer, icePwd string, init *webrtc.DataChannelInit) *client {
	t.Helper()
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	c := &client{
		t:         t,
		pc:        pc,
		connected: make(chan struct{}),
		pcClosed:  make(chan struct{}),
		open:      make(chan struct{}),
		closed:    make(chan struct{}),
		msgs:      make(chan webrtc.DataChannelMessage, 256),
	}
	var connectedOnce, closedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(c.connected) })
		case webrtc.PeerConnectionStateClosed:
			closedOnce.Do(func() { close(c.pcClosed) })
		default:
		}
	})
	c.dc, err = pc.CreateDataChannel("sync", init)
	if err != nil {
		t.Fatal(err)
	}
	c.dc.OnOpen(func() { close(c.open) })
	c.dc.OnClose(func() { close(c.closed) })
	c.dc.OnMessage(func(msg webrtc.DataChannelMessage) { c.msgs <- msg })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer := buildAnswer(offer.SDP, icePwd, s.fp, s.cfg.Port, []string{"127.0.0.1"})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("remote description: %v\n%s", err, answer)
	}

	return c
}

func (c *client) waitOpen(deadline time.Time) {
	c.t.Helper()
	select {
	case <-c.open:
	case <-c.closed:
		c.t.Fatal("the data channel closed before it opened")
	case <-time.After(time.Until(deadline)):
		c.t.Fatal("the data channel did not open in time")
	}
}

func (c *client) waitClosed(deadline time.Time) {
	c.t.Helper()
	select {
	case <-c.closed:
	case <-time.After(time.Until(deadline)):
		c.t.Fatal("the data channel did not close in time")
	}
}

func (c *client) send(f obj) {
	c.t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.dc.SendText(string(b)); err != nil {
		c.t.Fatalf("send %s: %v", b, err)
	}
}

// next returns the next message. It fails if the channel closes with no
// message left or the deadline passes.
func (c *client) next(deadline time.Time) webrtc.DataChannelMessage {
	c.t.Helper()
	select {
	case msg := <-c.msgs:
		return msg
	case <-c.closed:
		// Messages are queued before the close is reported.
		select {
		case msg := <-c.msgs:
			return msg
		default:
		}
		c.t.Fatal("the data channel closed while a message was awaited")
	case <-time.After(time.Until(deadline)):
		c.t.Fatal("no message before the deadline")
	}

	return webrtc.DataChannelMessage{}
}

// frame returns the next message, which must be a text frame of type want.
func (c *client) frame(deadline time.Time, want string) obj {
	c.t.Helper()
	msg := c.next(deadline)
	if !msg.IsString {
		c.t.Fatalf("got a binary message of %d bytes, want a %q frame", len(msg.Data), want)
	}
	var f obj
	if err := json.Unmarshal(msg.Data, &f); err != nil {
		c.t.Fatalf("frame %q: %v", msg.Data, err)
	}
	if f["t"] != want {
		c.t.Fatalf("got frame %s, want a %q frame", msg.Data, want)
	}

	return f
}

// body reads a body of n bytes sent as binary messages.
func (c *client) body(deadline time.Time, n int) []byte {
	c.t.Helper()
	var out []byte
	for len(out) < n {
		msg := c.next(deadline)
		if msg.IsString {
			c.t.Fatalf("text frame %s after %d of %d body bytes", msg.Data, len(out), n)
		}
		if len(msg.Data) > protocol.ChunkBytes {
			c.t.Fatalf("binary message of %d bytes exceeds %d", len(msg.Data), protocol.ChunkBytes)
		}
		out = append(out, msg.Data...)
	}
	if len(out) != n {
		c.t.Fatalf("body of %d bytes overruns the announced %d", len(out), n)
	}

	return out
}

// pair pairs the client with a fresh one-time secret for the app "todo" and
// returns the welcome.
func (c *client) pair(deadline time.Time, secrets *pairing.Secrets, name string) obj {
	c.t.Helper()
	secret, _ := secrets.Issue()
	c.send(obj{"t": "pair", "proto": 1, "app": "todo", "secret": secret, "name": name})
	paired := c.frame(deadline, "paired")
	if device, _ := paired["device"].(string); device == "" {
		c.t.Fatalf("paired = %v, want a device id", paired)
	}
	if token, _ := paired["token"].(string); token == "" {
		c.t.Fatalf("paired = %v, want a token", paired)
	}

	return c.frame(deadline, "welcome")
}

// waitFor polls cond until it holds or the deadline passes.
// bindingRequest builds the first connectivity check of a client that chose
// serverUfrag, signed with icePwd.
func bindingRequest(t *testing.T, serverUfrag, icePwd string) []byte {
	t.Helper()
	m, err := stun.Build(stun.TransactionID, stun.BindingRequest,
		stun.NewUsername(serverUfrag+":abcd"),
		stun.NewShortTermIntegrity(icePwd),
		stun.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	return m.Raw
}

// endConn ends the connection of ufrag and waits until it is released.
func endConn(t *testing.T, l *Listener, ufrag string) {
	t.Helper()
	l.mu.Lock()
	c := l.conns[ufrag]
	l.mu.Unlock()
	if c == nil {
		t.Fatalf("no connection for ufrag %s", ufrag)
	}
	c.end("ended by the test", false)
	waitFor(t, time.Now().Add(testDeadline), "the connection to be released", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.conns[ufrag] == nil
	})
}

func waitFor(t *testing.T, deadline time.Time, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// jsonBody returns a JSON document {"items":[...]} of exactly size bytes.
func jsonBody(t *testing.T, size int) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < size-100; i++ {
		fmt.Fprintf(&b, `{"id":%d,"title":"Task %d"},`, i, i)
	}
	b.WriteString(`"` + strings.Repeat("x", size-b.Len()-len(`""]}`)) + `"]}`)
	body := []byte(b.String())
	if len(body) != size || !json.Valid(body) {
		t.Fatalf("test body has %d bytes (want %d) or is not JSON", len(body), size)
	}

	return body
}
