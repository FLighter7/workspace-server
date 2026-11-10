// Package rtc accepts WebRTC connections with no signaling. The server is
// ICE-lite with a fixed ICE password and certificate. A client builds the
// server's answer itself from the pairing code, choosing the server's ICE
// ufrag; the first connectivity check that carries an unknown ufrag and
// verifies against the password starts a peer connection for it, and the
// client's retransmitted check reaches that connection. Each connection's
// data channel speaks the sync protocol through a protocol.Session.
package rtc

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
)

// Defaults for Config fields left zero.
const (
	defaultMaxSessions    = 64
	defaultConnectTimeout = 20 * time.Second
	defaultAuthTimeout    = 10 * time.Second
)

// closeGrace bounds how long a connection whose session ended waits for its
// data channel to finish closing before the peer connection goes, so that
// the session's last frame (an error, say) can reach the client. A variable
// for tests.
var closeGrace = 2 * time.Second

// A server ufrag whose connection ended is refused for recentTTL: a late
// connectivity check of the client's old connection would otherwise start a
// new one, which holds a slot until the connect deadline. Browsers give up a
// connection after about 30 s without consent; clients pick a new ufrag for
// every connection anyway. At most recentMax ufrags are remembered.
const (
	recentTTL = time.Minute
	recentMax = 1024
)

var _ protocol.Channel = (*webrtc.DataChannel)(nil)

// Config configures a Listener.
type Config struct {
	Addresses   []net.IP // LAN IPv4 + 127.0.0.1; one UDP socket per distinct address
	Port        int
	ICEPwd      string
	Certificate webrtc.Certificate
	Backend     protocol.Backend
	// MaxSessions caps the peer connections being set up or open,
	// authenticated or not; 0 means 64.
	MaxSessions int
	// ConnectTimeout closes a peer connection not connected, with its data
	// channel open, in time; 0 means 20 s.
	ConnectTimeout time.Duration
	// AuthTimeout closes a session not authenticated in time after its
	// channel opens; 0 means 10 s.
	AuthTimeout time.Duration
	// Logf, if set, receives one line per closed connection and errors.
	Logf func(format string, args ...any)
}

// Listener accepts peer connections on one UDP port of every address.
type Listener struct {
	cfg Config
	mux *ice.MultiUDPMuxDefault

	mu     sync.Mutex
	conns  map[string]*conn // by server ufrag, from the first check until torn down
	recent recentUfrags     // ufrags of connections torn down lately
	closed bool
	wg     sync.WaitGroup // one count per entry in conns

	now func() time.Time // time.Now; tests replace it
}

// Listen binds cfg.Port on every address and starts accepting connections.
func Listen(cfg Config) (*Listener, error) {
	switch {
	case len(cfg.Addresses) == 0:
		return nil, errors.New("rtc: no address to listen on")
	case cfg.ICEPwd == "":
		return nil, errors.New("rtc: no ICE password")
	case cfg.Backend == nil:
		return nil, errors.New("rtc: no backend")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	if cfg.AuthTimeout <= 0 {
		cfg.AuthTimeout = defaultAuthTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	// An address on two interfaces is bound once; a second bind would fail.
	unique := make([]net.IP, 0, len(cfg.Addresses))
	for _, ip := range cfg.Addresses {
		if !slices.ContainsFunc(unique, ip.Equal) {
			unique = append(unique, ip)
		}
	}
	cfg.Addresses = unique

	l := &Listener{cfg: cfg, conns: map[string]*conn{}, now: time.Now}
	// A socket delivers packets as soon as its mux exists; onUnhandled waits
	// on the lock until the whole mux is ready.
	l.mu.Lock()
	defer l.mu.Unlock()
	muxes := make([]ice.UDPMux, 0, len(cfg.Addresses))
	for _, ip := range cfg.Addresses {
		sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: cfg.Port})
		if err != nil {
			l.closed = true
			for _, m := range muxes {
				_ = m.Close()
			}
			return nil, fmt.Errorf("rtc: listen on %s: %w", net.JoinHostPort(ip.String(), strconv.Itoa(cfg.Port)), err)
		}
		muxes = append(muxes, ice.NewUDPMuxDefault(ice.UDPMuxParams{
			UDPConn:           sock,
			OnUnhandledPacket: l.onUnhandled,
		}))
	}
	l.mux = ice.NewMultiUDPMuxDefault(muxes...)
	return l, nil
}

// Close stops accepting, closes every peer connection and waits until they
// are torn down, then closes the sockets. Later calls do nothing.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	conns := make([]*conn, 0, len(l.conns))
	for _, c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()

	for _, c := range conns {
		c.end("listener closed", false)
	}
	// The sockets stay open meanwhile, so each client hears its connection
	// close instead of waiting for a timeout.
	l.wg.Wait()
	return l.mux.Close()
}

// ActiveSessions returns the number of peer connections being set up or
// open, authenticated or not.
func (l *Listener) ActiveSessions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

// onUnhandled sees every packet that no peer connection claims. It must not
// block. A binding request for an unknown server ufrag whose integrity
// verifies against the ICE password starts a peer connection in the
// background; anything else, a wrong password or a ufrag whose connection
// ended lately included, is dropped silently.
func (l *Listener) onUnhandled(data []byte, addr netip.AddrPort) {
	if !stun.IsMessage(data) {
		return
	}
	msg := &stun.Message{Raw: append([]byte(nil), data...)}
	if err := msg.Decode(); err != nil || msg.Type != stun.BindingRequest {
		return
	}
	var user stun.Username
	if err := user.GetFrom(msg); err != nil {
		return
	}
	ufrag, clientUfrag, ok := strings.Cut(string(user), ":")
	if !ok || !validUfrag(ufrag) || !validUfrag(clientUfrag) {
		return
	}
	if err := stun.NewShortTermIntegrity(l.cfg.ICEPwd).Check(msg); err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.conns[ufrag] != nil || len(l.conns) >= l.cfg.MaxSessions ||
		l.recent.has(ufrag, l.now()) {
		return
	}
	c := &conn{
		l:        l,
		ufrag:    ufrag,
		remote:   addr.String(),
		since:    time.Now(),
		stopped:  make(chan struct{}),
		opened:   make(chan struct{}),
		dcClosed: make(chan struct{}),
	}
	l.conns[ufrag] = c
	l.wg.Add(1)
	go l.serve(c, clientUfrag)
}

// serve owns one peer connection for its whole life: it sets it up, waits
// until something ends it or it fails to connect in time, and tears it down.
func (l *Listener) serve(c *conn, clientUfrag string) {
	defer l.release(c)

	pc, err := l.newPeerConnection(c.ufrag)
	if err != nil {
		c.end("peer connection: "+err.Error(), false)
		return
	}
	c.watch(pc)
	if err := answer(pc, clientUfrag); err != nil {
		c.end(err.Error(), false)
	}

	// Connected means with the session's channel open: a connection that
	// never opens one would otherwise escape the auth deadline and hold its
	// slot for as long as the client keeps it alive.
	timer := time.NewTimer(l.cfg.ConnectTimeout)
	defer timer.Stop()
	select {
	case <-c.stopped:
	case <-timer.C:
		select {
		case <-c.opened:
		default:
			c.end(fmt.Sprintf("not connected within %s", l.cfg.ConnectTimeout), false)
		}
		<-c.stopped
	}
	c.teardown(pc)
}

func (l *Listener) newPeerConnection(ufrag string) (*webrtc.PeerConnection, error) {
	var se webrtc.SettingEngine
	se.SetLite(true)
	se.SetICECredentials(ufrag, l.cfg.ICEPwd)
	se.DisableCertificateFingerprintVerification(true)
	if err := se.SetAnsweringDTLSRole(webrtc.DTLSRoleServer); err != nil {
		return nil, err
	}
	se.SetICEUDPMux(l.mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	return api.NewPeerConnection(webrtc.Configuration{Certificates: []webrtc.Certificate{l.cfg.Certificate}})
}

// answer answers the stand-in offer of the client with clientUfrag.
func answer(pc *webrtc.PeerConnection, clientUfrag string) error {
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: standInOffer(clientUfrag)}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("remote description: %w", err)
	}
	desc, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("answer: %w", err)
	}
	if err := pc.SetLocalDescription(desc); err != nil {
		return fmt.Errorf("local description: %w", err)
	}
	return nil
}

// release forgets a torn-down connection, freeing its slot. Its ufrag stays
// refused for a while.
func (l *Listener) release(c *conn) {
	c.mu.Lock()
	why := c.why
	c.mu.Unlock()
	l.mu.Lock()
	delete(l.conns, c.ufrag)
	l.recent.add(c.ufrag, l.now())
	l.mu.Unlock()
	l.cfg.Logf("connection from %s (ufrag %s) closed after %s: %s",
		c.remote, c.ufrag, time.Since(c.since).Round(time.Millisecond), why)
	l.wg.Done()
}

// conn is one peer connection, from its first connectivity check until it is
// torn down. Its serve goroutine owns the peer connection; Pion callbacks and
// the session only ask it to end. The only Pion objects closed elsewhere are
// data channels, each on a goroutine of its own.
type conn struct {
	l      *Listener
	ufrag  string // the server's, chosen by the client
	remote string // the client's ip:port, from the first check
	since  time.Time

	stopped  chan struct{} // closed by the first end
	opened   chan struct{} // closed once session is set
	dcClosed chan struct{} // closed when the data channel has closed
	dcOnce   sync.Once

	mu      sync.Mutex
	ending  bool
	why     string
	drain   bool // wait for the data channel to close before closing pc
	dc      *webrtc.DataChannel
	session *protocol.Session
}

// end asks for the connection to be torn down. With drain, the teardown lets
// the data channel finish closing first (at most closeGrace). Only the first
// call counts. It never blocks, so it is safe from any callback.
func (c *conn) end(why string, drain bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ending {
		return
	}
	c.ending, c.why, c.drain = true, why, drain
	close(c.stopped)
}

// watch hooks the connection's events up.
func (c *conn) watch(pc *webrtc.PeerConnection) {
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			c.end("connection "+state.String(), false)
		default:
		}
	})
	pc.OnDataChannel(c.adopt)
}

// adopt takes the client's data channel. A client opens one; any further
// channel is closed.
func (c *conn) adopt(dc *webrtc.DataChannel) {
	c.mu.Lock()
	first := c.dc == nil && !c.ending
	if first {
		c.dc = dc
	}
	c.mu.Unlock()
	if !first {
		go func() { _ = dc.Close() }()
		return
	}
	dc.OnOpen(func() { c.open(dc) })
	dc.OnMessage(c.receive)
	dc.OnClose(func() {
		c.dcOnce.Do(func() { close(c.dcClosed) })
		c.end("data channel closed", false)
	})
}

// open starts the session once the channel is open.
func (c *conn) open(dc *webrtc.DataChannel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ending {
		return
	}
	c.session = protocol.NewSession(channel{dc}, c.remote, c.l.cfg.Backend, protocol.Options{
		AuthTimeout: c.l.cfg.AuthTimeout,
		OnClose:     func() { c.end("session closed", true) },
	})
	close(c.opened)
}

// receive hands an incoming message to the session.
func (c *conn) receive(msg webrtc.DataChannelMessage) {
	// The open handler runs on a goroutine of its own, so the first message
	// can arrive before the session exists.
	select {
	case <-c.opened:
	case <-c.stopped:
		return
	}
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if msg.IsString {
		s.HandleText(string(msg.Data))
	} else {
		s.HandleBinary(msg.Data)
	}
}

// teardown closes the session, lets the data channel finish closing when the
// session asked for it, and closes the peer connection. It runs on the serve
// goroutine, outside every Pion callback.
func (c *conn) teardown(pc io.Closer) {
	c.mu.Lock()
	s, drain := c.session, c.drain
	c.mu.Unlock()
	if s != nil {
		s.Close()
	}
	if drain {
		grace := time.NewTimer(closeGrace)
		select {
		case <-c.dcClosed:
		case <-grace.C:
		}
		grace.Stop()
	}
	if err := pc.Close(); err != nil {
		c.l.cfg.Logf("connection from %s (ufrag %s): close: %v", c.remote, c.ufrag, err)
	}
}

// recentUfrags remembers server ufrags with the time their connection ended,
// oldest first, for recentTTL and at most recentMax of them. The listener's
// lock guards it.
type recentUfrags struct {
	at    map[string]time.Time
	order []string
}

func (r *recentUfrags) add(ufrag string, now time.Time) {
	if r.at == nil {
		r.at = map[string]time.Time{}
	}
	if _, ok := r.at[ufrag]; !ok {
		r.order = append(r.order, ufrag)
	}
	r.at[ufrag] = now
	r.prune(now)
}

func (r *recentUfrags) has(ufrag string, now time.Time) bool {
	r.prune(now)
	_, ok := r.at[ufrag]
	return ok
}

// prune drops the entries older than recentTTL, and the oldest beyond
// recentMax.
func (r *recentUfrags) prune(now time.Time) {
	drop := 0
	for drop < len(r.order) {
		u := r.order[drop]
		if len(r.order)-drop <= recentMax && now.Sub(r.at[u]) < recentTTL {
			break
		}
		delete(r.at, u)
		drop++
	}
	if drop > 0 {
		r.order = append([]string(nil), r.order[drop:]...)
	}
}

// channel is the session's view of a data channel. Its Close does not wait,
// since the session can close from inside a Pion callback (a message
// handler), where closing a Pion object can deadlock.
type channel struct{ *webrtc.DataChannel }

func (ch channel) Close() error {
	go func() { _ = ch.DataChannel.Close() }()
	return nil
}

// validUfrag reports whether s is a well-formed ICE ufrag, 4 to 256
// characters of [A-Za-z0-9+/] (RFC 8839), so that it can go into an SDP line.
func validUfrag(s string) bool {
	if len(s) < 4 || len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return false
		}
	}
	return true
}
