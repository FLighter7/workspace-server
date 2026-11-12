package wsclient

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Defaults for DialOptions left zero.
const (
	defaultOpenTimeout    = 10 * time.Second
	defaultResolveTimeout = 1500 * time.Millisecond
)

// channelLabel is the data channel's label, as the client package's.
const channelLabel = "workspace"

// DialOptions configure Dial.
type DialOptions struct {
	// Resolve looks up a .local host name. Nil leaves such hosts out.
	Resolve func(ctx context.Context, name string) ([]net.IP, error)
	// ResolveTimeout bounds the lookups; 0 means 1.5 s.
	ResolveTimeout time.Duration
	// OpenTimeout bounds the wait for the data channel to open; 0 means
	// 10 s.
	OpenTimeout time.Duration
}

// Dial connects to the server t describes with no signaling, as the client
// package's openServerChannel does: Pion makes the offer, and the answer is
// built here from t (BuildAnswer). Its hosts are the candidates: a .local
// name is resolved first, through opts.Resolve, and an IPv4 address is taken
// as it is. The returned Client speaks for app ("" for a link session); its
// Close closes the connection, and whatever ends the connection ends the
// Client.
func Dial(ctx context.Context, t Target, app string, opts DialOptions) (*Client, error) {
	if opts.OpenTimeout <= 0 {
		opts.OpenTimeout = defaultOpenTimeout
	}
	if opts.ResolveTimeout <= 0 {
		opts.ResolveTimeout = defaultResolveTimeout
	}
	hosts := candidates(ctx, t.Hosts, opts)
	if len(hosts) == 0 {
		return nil, &Error{Code: CodeConnectTimeout, Message: fmt.Sprintf("no address of the server could be used: %s", strings.Join(t.Hosts, ", "))}
	}
	t.Hosts = hosts

	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	// Our own resolver handles .local names; Pion's would take port 5353
	// beside the server's responder.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// The server listens on 127.0.0.1 too, so a server on this machine can
	// be reached there.
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	dc, err := pc.CreateDataChannel(channelLabel, nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	conn := &pionChannel{DataChannel: dc, pc: pc}
	c := NewClient(conn, app)

	opened := make(chan struct{})
	closed := make(chan struct{})
	var openOnce, closeOnce sync.Once
	dc.OnOpen(func() { openOnce.Do(func() { close(opened) }) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) { c.Receive(msg.IsString, msg.Data) })
	gone := func() {
		closeOnce.Do(func() { close(closed) })
		c.ChannelClosed()
		// Never close Pion objects inside a Pion callback.
		_ = conn.Close()
	}
	dc.OnClose(gone)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed:
			gone()
		default:
		}
	})

	fail := func(err error) (*Client, error) {
		c.Close()
		return nil, err
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fail(err)
	}
	// No waiting for ICE gathering: the server learns this side's address
	// from its first binding request.
	if err := pc.SetLocalDescription(offer); err != nil {
		return fail(err)
	}
	answer := BuildAnswer(offer.SDP, t, RandomUfrag(), time.Now().UnixMilli())
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return fail(fmt.Errorf("the server's answer: %w", err))
	}

	timer := time.NewTimer(opts.OpenTimeout)
	defer timer.Stop()
	select {
	case <-opened:
		return c, nil
	case <-closed:
		return fail(&Error{Code: CodeClosed, Message: "the connection to the server closed before it opened"})
	case <-timer.C:
		return fail(&Error{Code: CodeConnectTimeout, Message: "the server did not answer in time"})
	case <-ctx.Done():
		return fail(ctx.Err())
	}
}

// candidates returns the hosts as IPv4 addresses in their order, a .local
// name replaced by what it resolves to; a name that does not resolve, and
// anything that is neither, is left out.
func candidates(ctx context.Context, hosts []string, opts DialOptions) []string {
	var out []string
	add := func(ip net.IP) {
		s := ip.String()
		for _, have := range out {
			if have == s {
				return
			}
		}
		out = append(out, s)
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h).To4(); ip != nil {
			add(ip)
			continue
		}
		if !strings.HasSuffix(strings.ToLower(h), ".local") || opts.Resolve == nil {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, opts.ResolveTimeout)
		ips, err := opts.Resolve(rctx, h)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if ip4 := ip.To4(); ip4 != nil {
				add(ip4)
			}
		}
	}
	return out
}

// pionChannel is the Client's view of a Pion data channel. Close closes the
// channel and its peer connection on a goroutine of its own, since the
// Client may close from inside a Pion callback, where closing a Pion object
// can deadlock.
type pionChannel struct {
	*webrtc.DataChannel
	pc   *webrtc.PeerConnection
	once sync.Once
}

func (p *pionChannel) Close() error {
	p.once.Do(func() {
		go func() {
			_ = p.DataChannel.Close()
			_ = p.pc.Close()
		}()
	})
	return nil
}
