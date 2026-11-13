// Package wsclient is a Go client of the workspace-server protocol
// (docs/protocol.md): what a subordinate server uses to reach its main. It
// mirrors the client package in client/ (serverAnswer.ts, PairingCode.ts,
// ChannelFraming.ts, ServerClient.ts), and plays the same vectors in
// testdata/conformance. Dial opens the connection with Pion as the offerer;
// a Client speaks the messages over it, one request at a time.
package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sync"
	"time"
)

// Version is the protocol version this client speaks.
const Version = 1

// Framing and flow control, as the server's: bodies go in binary messages of
// at most ChunkBytes; a sender pauses while more than BufferHigh bytes wait
// in the channel and resumes at BufferLow.
const (
	ChunkBytes = 16384
	BufferHigh = 1 << 20
	BufferLow  = 256 << 10
)

// forgetWait is how long Forget waits for the server to close the channel.
const forgetWait = 2 * time.Second

// The kinds Pair may name.
const (
	KindDevice = "device"
	KindServer = "server"
)

// The roles a welcome names.
const (
	RoleMain        = "main"
	RoleSubordinate = "subordinate"
)

// Codes of the errors this client reports itself, beside the server's.
const (
	// CodeClosed: the channel closed, or the client was closed.
	CodeClosed = "closed"
	// CodeConnectTimeout: the connection did not open in time.
	CodeConnectTimeout = "connectTimeout"
	// CodeProtocol: the server sent what the protocol does not allow; also
	// a server code.
	CodeProtocol = "protocol"
	// CodeInternal: a server failure, and any server code this client does
	// not know.
	CodeInternal = "internal"
)

// ServerCodes are the codes a server sends in an error frame
// (docs/protocol.md, "Errors"). Any other reads as CodeInternal.
var ServerCodes = []string{
	"protocol", "unknownApp", "badSecret", "badToken", "blocked",
	"badPath", "quota", "noTransaction", "incomplete",
	"notAuthenticated", "internal", "notMain",
}

// Error is a failure this client reports: a server's error frame, or one of
// its own codes. Message is English, for a developer.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message + " (" + e.Code + ")" }

// HasCode reports whether err is an *Error with code.
func HasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func protocolError(format string, args ...any) *Error {
	return &Error{Code: CodeProtocol, Message: fmt.Sprintf(format, args...)}
}

// Channel is the data channel a Client speaks over. Incoming messages reach
// the Client through Receive, and the channel's end through ChannelClosed.
type Channel interface {
	SendText(s string) error
	Send(data []byte) error
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(th uint64)
	OnBufferedAmountLow(f func())
	// Close must not wait: the Client may call it from inside Receive.
	Close() error
}

// Welcome is what a welcome tells the client. Revision is the revision of
// the app's set, nil for an empty set, and always on a link session. Role is
// empty when the server named none: a server from before roles. Term is the
// family's term as the server knows it, 0 when it names none. Subordinates is
// what a main lists: nil when the welcome carries no list (a subordinate, or a
// main from before subordinates were handed out, which takes no announce
// either), and empty but not nil for a main without any. An entry this client
// cannot read is left out. Diverged lists the servers whose copy of the app
// diverged from their main's and waits for a device's choice; nil when the
// welcome names none.
type Welcome struct {
	ServerID     string           `json:"serverId"`
	Name         string           `json:"name"`
	Revision     *string          `json:"revision"`
	Hosts        []string         `json:"hosts"`
	Role         string           `json:"role,omitempty"`
	Term         int              `json:"term,omitempty"`
	Subordinates []Server         `json:"subordinates,omitempty"`
	Diverged     []DivergedServer `json:"diverged,omitempty"`
}

// DivergedServer is a server whose copy of an app diverged from its main's,
// and since when (RFC 3339 UTC).
type DivergedServer struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Since    string `json:"since"`
}

// Server is how a device reaches a server: a pairing code without the
// secret, as a subordinate announces itself and a main lists its
// subordinates. Fingerprint is uppercase colon hex.
type Server struct {
	ServerID    string   `json:"serverId"`
	Name        string   `json:"name"`
	Password    string   `json:"password"`
	Fingerprint string   `json:"fingerprint"`
	Port        int      `json:"port"`
	Hosts       []string `json:"hosts"`
}

// Device is one device of a main's registry: its id, its app (none for a
// server-kind device, another server of the family), and its token's
// SHA-256 in lowercase hex; its kind, name and pairing time (RFC 3339) when
// the main names them, and what a server-kind device announced.
type Device struct {
	ID        string  `json:"id"`
	App       string  `json:"app"`
	Kind      string  `json:"kind,omitempty"`
	Name      string  `json:"name,omitempty"`
	TokenHash string  `json:"tokenHash"`
	PairedAt  string  `json:"pairedAt,omitempty"`
	Server    *Server `json:"server,omitempty"`
}

// Tombstone is a device removed from the family: its id, and when (RFC
// 3339).
type Tombstone struct {
	ID string `json:"id"`
	At string `json:"at"`
}

// Registry is a device registry as it travels: the devices, and the ids
// removed.
type Registry struct {
	Devices []Device
	Removed []Tombstone
}

// TermAnswer is a server's answer to Term: the term and the main it follows
// once it handled it.
type TermAnswer struct {
	Term int
	Main string
}

// Paired is what a successful pair returns: the new device and its token.
type Paired struct {
	Device string `json:"device"`
	Token  string `json:"token"`
}

// App is one entry of a server's app list: its id, name and quota in bytes
// (0 when the server named none).
type App struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Quota int64  `json:"quota"`
}

// Choice is a main's push to a subordinate's link session: a device chose
// Keep (KeepMain or KeepSubordinate) for the divergence of the
// subordinate's copy of App that began at Since (RFC 3339).
type Choice struct {
	App   string
	Keep  string
	Since string
}

// incoming is one frame as received. A doc's body is not kept: it goes to
// the writer of the get that asked for it as it arrives, and werr is what
// that writer failed with, if it did.
type incoming struct {
	t     string
	frame map[string]json.RawMessage
	werr  error
}

// pending is the request waiting for its reply. sink takes the body of a
// get's doc.
type pending struct {
	kind    string // the request's frame type
	sink    io.Writer
	replies chan incoming
}

// Client speaks the protocol over one data channel for one app, or for no
// app: a link session. One request runs at a time; a later call waits for
// the one before it. Pushes reach the listeners set with OnChanged,
// OnDiverged, OnResolved, OnApps, OnDevices, OnTakeOver and OnChoice, which run on the
// goroutine that calls Receive and must not block or call the Client. A
// request given up through its context ends the Client, whose replies would
// no longer pair up with requests.
type Client struct {
	ch  Channel
	app string

	reqMu sync.Mutex // one request at a time

	mu         sync.Mutex
	pending    *pending
	body       *bodyState // the body arriving, if any
	ended      error
	onChanged  func(revision *string)
	onDiverged func(list []DivergedServer)
	onResolved func(server string)
	onApps     func(list []App)
	onDevices  func(reg Registry)
	onTakeOver func()
	onChoice   func(ch Choice)

	done chan struct{} // closed when the client ends
	low  chan struct{} // the channel's buffer drained to BufferLow
}

// bodyState is a doc's body arriving: left bytes of it still to come, each
// piece written to w as it arrives. Only the goroutine that calls Receive
// touches it; the client's lock guards the pointer to it.
type bodyState struct {
	in   incoming
	left int64
	w    io.Writer
}

// NewClient returns a client speaking over ch for app; app "" makes it a
// link session's client. It sets the channel's low-buffer threshold and
// callback.
func NewClient(ch Channel, app string) *Client {
	c := &Client{ch: ch, app: app, done: make(chan struct{}), low: make(chan struct{}, 1)}
	ch.SetBufferedAmountLowThreshold(BufferLow)
	ch.OnBufferedAmountLow(func() {
		select {
		case c.low <- struct{}{}:
		default:
		}
	})
	return c
}

// OnChanged sets the listener of changed pushes: the app's set has a new
// revision, nil for an empty set.
func (c *Client) OnChanged(f func(revision *string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onChanged = f
}

// OnDiverged sets the listener of diverged pushes: the servers whose copy of
// the app diverged from their main's, which wait for a device's choice.
func (c *Client) OnDiverged(f func(list []DivergedServer)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDiverged = f
}

// OnResolved sets the listener of resolved pushes: the divergence of the
// server named has been settled, by a device's resolve.
func (c *Client) OnResolved(f func(server string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onResolved = f
}

// OnApps sets the listener of app lists the server pushes, and of the one
// Apps reads.
func (c *Client) OnApps(f func(list []App)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onApps = f
}

// OnDevices sets the listener of the device registries the main pushes, and
// of the one Announce reads.
func (c *Client) OnDevices(f func(reg Registry)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDevices = f
}

// OnTakeOver sets the listener of takeOver: the main asks this server, over
// its link session, to become the main of the family.
func (c *Client) OnTakeOver(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTakeOver = f
}

// OnChoice sets the listener of resolve pushes on a link session: a device
// of the main chose which copy of an app to keep, this server's or the
// main's.
func (c *Client) OnChoice(f func(ch Choice)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onChoice = f
}

// Done is closed when the client ends: the channel closed, the server broke
// the protocol, or Close was called. Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the client ended, nil while it has not.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended
}

// Close ends the client and closes its channel.
func (c *Client) Close() {
	c.end(&Error{Code: CodeClosed, Message: "the connection was closed"})
	_ = c.ch.Close()
}

// ChannelClosed tells the client that its channel has closed.
func (c *Client) ChannelClosed() {
	c.end(&Error{Code: CodeClosed, Message: "the connection to the server closed"})
}

// end ends the client once with err.
func (c *Client) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended != nil {
		return
	}
	c.ended = err
	c.body = nil
	close(c.done)
}

// fail ends the client with err and closes the channel: the two sides no
// longer agree.
func (c *Client) fail(err error) {
	c.end(err)
	_ = c.ch.Close()
}

// Receive hands the client one incoming message: a text frame, or a piece of
// a body. Messages must arrive in order, one at a time. A piece of a doc's
// body is written to the get's writer here, before Receive returns.
func (c *Client) Receive(text bool, data []byte) {
	var in *incoming
	var err error
	if text {
		in, err = c.frame(data)
	} else {
		in, err = c.piece(data)
	}
	if err != nil {
		c.fail(err)
		return
	}
	if in != nil {
		c.dispatch(*in)
	}
}

// frame reads a text frame: ready at once, unless it is a doc whose body is
// still to come. It returns nil then, or after the client has ended.
func (c *Client) frame(data []byte) (*incoming, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended != nil {
		return nil, nil
	}
	if c.body != nil {
		return nil, protocolError("a text frame arrived inside a body")
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(data, &frame); err != nil || frame == nil {
		return nil, protocolError("the server sent a broken frame")
	}
	var t string
	if json.Unmarshal(frame["t"], &t) != nil || frame["t"] == nil {
		return nil, protocolError("a frame without t")
	}
	in := incoming{t: t, frame: frame}
	if t != "doc" {
		return &in, nil
	}
	// Only a doc carries a body: the document's bytes past the offset.
	size, ok1 := readCount(frame["bytes"])
	offset, ok2 := int64(0), true
	if raw, present := frame["offset"]; present {
		offset, ok2 = readCount(raw)
	}
	if !ok1 || !ok2 || offset > size {
		return nil, protocolError("a doc with an invalid size or offset")
	}
	if size == offset {
		return &in, nil
	}
	if c.pending == nil || c.pending.kind != "get" {
		return nil, protocolError("a doc nobody asked for")
	}
	c.body = &bodyState{in: in, left: size - offset, w: c.pending.sink}
	return nil, nil
}

// piece takes a piece of the body arriving and writes it to the get's
// writer, outside the client's lock. A writer that fails is not written to
// again; the rest of the body is read and dropped, and the get fails with
// the writer's error. It returns the doc once its body is all in.
func (c *Client) piece(data []byte) (*incoming, error) {
	c.mu.Lock()
	b := c.body
	ended := c.ended != nil
	c.mu.Unlock()
	switch {
	case ended:
		return nil, nil
	case b == nil:
		return nil, protocolError("a binary message arrived when no body was expected")
	case int64(len(data)) > b.left:
		return nil, protocolError("a body overran its announced size")
	}
	b.left -= int64(len(data))
	if b.in.werr == nil && len(data) > 0 {
		if _, err := b.w.Write(data); err != nil {
			b.in.werr = err
		}
	}
	if b.left > 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended != nil {
		return nil, nil
	}
	c.body = nil
	return &b.in, nil
}

// dispatch hands a complete frame to the listeners or to the request
// waiting for it.
func (c *Client) dispatch(in incoming) {
	c.mu.Lock()
	onChanged, onDiverged, onResolved := c.onChanged, c.onDiverged, c.onResolved
	onApps, onDevices, onTakeOver, onChoice, p := c.onApps, c.onDevices, c.onTakeOver, c.onChoice, c.pending
	c.mu.Unlock()

	switch in.t {
	case "changed":
		rev, ok := readRevision(in.frame)
		if !ok {
			c.fail(protocolError("a malformed changed"))
			return
		}
		if onChanged != nil {
			onChanged(rev)
		}
		return
	case "diverged":
		list, ok := readDiverged(in.frame["servers"])
		if !ok {
			c.fail(protocolError("a malformed diverged"))
			return
		}
		if onDiverged != nil {
			onDiverged(list)
		}
		return
	case "resolved":
		server, ok := readString(in.frame["server"])
		if !ok {
			c.fail(protocolError("a malformed resolved"))
			return
		}
		if onResolved != nil {
			onResolved(server)
		}
		if p == nil || p.kind != "resolve" {
			return // a push
		}
	case "apps":
		list, err := readApps(in.frame)
		if err != nil {
			c.fail(err)
			return
		}
		if onApps != nil {
			onApps(list)
		}
		if p == nil || p.kind != "apps" {
			return // a push
		}
	case "devices":
		reg, err := readRegistry(in.frame)
		if err != nil {
			c.fail(err)
			return
		}
		if onDevices != nil {
			onDevices(reg)
		}
		if p == nil || p.kind != "announce" {
			return // a push
		}
	case "takeOver":
		if onTakeOver != nil {
			onTakeOver()
		}
		return
	case "resolve":
		app, ok1 := readString(in.frame["app"])
		keep, ok2 := readString(in.frame["keep"])
		since, ok3 := readString(in.frame["since"])
		if !ok1 || app == "" || !ok2 || (keep != KeepMain && keep != KeepSubordinate) || !ok3 {
			c.fail(protocolError("a malformed resolve"))
			return
		}
		if onChoice != nil {
			onChoice(Choice{App: app, Keep: keep, Since: since})
		}
		return
	case "error":
		if p == nil {
			return
		}
	}
	if p == nil {
		c.fail(protocolError("unexpected %s", in.t))
		return
	}
	select {
	case p.replies <- in:
	default:
		c.fail(protocolError("unexpected %s", in.t))
	}
}

// outBody is a body a request sends after its frame: n bytes read from r.
type outBody struct {
	r io.Reader
	n int64
}

// request sends frame (and body, when not nil) once every earlier request
// has finished, and hands each reply to read until it reports the request
// done. sink, when not nil, takes the body of a doc that answers. An error
// frame fails the request with its code; an error read returns ends the
// client, as the two sides no longer agree.
func (c *Client) request(ctx context.Context, frame map[string]any, body *outBody, sink io.Writer, read func(incoming) (bool, error)) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	p := &pending{kind: frame["t"].(string), sink: sink, replies: make(chan incoming, 4)}
	c.mu.Lock()
	if c.ended != nil {
		err := c.ended
		c.mu.Unlock()
		return err
	}
	c.pending = p
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.pending = nil
		c.mu.Unlock()
	}()

	if err := c.send(ctx, frame, body); err != nil {
		return err
	}
	for {
		select {
		case in := <-p.replies:
			if in.t == "error" {
				return readError(in.frame)
			}
			done, err := read(in)
			if err != nil {
				c.fail(err)
				return err
			}
			if done {
				return nil
			}
		case <-c.done:
			return c.Err()
		case <-ctx.Done():
			c.Close()
			return ctx.Err()
		}
	}
}

// send sends frame and then body in pieces of at most ChunkBytes, pausing
// while the channel holds more than BufferHigh bytes. A body that cannot be
// read to its end ends the client: the frame announced its size, and the
// server would take the next frame for the rest of it.
func (c *Client) send(ctx context.Context, frame map[string]any, body *outBody) error {
	text, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if err := c.ch.SendText(string(text)); err != nil {
		return c.sendFailed(err)
	}
	if body == nil || body.n == 0 {
		return nil
	}
	buf := make([]byte, min(int64(ChunkBytes), body.n))
	for left := body.n; left > 0; {
		for c.ch.BufferedAmount() > BufferHigh {
			select {
			case <-c.low:
			case <-c.done:
				return c.Err()
			case <-ctx.Done():
				c.Close()
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				// The low mark is a callback; poll too, so a missed one
				// costs a moment, not the request.
			}
		}
		chunk := buf[:min(int64(len(buf)), left)]
		if _, err := io.ReadFull(body.r, chunk); err != nil {
			c.fail(&Error{Code: CodeClosed, Message: "the body could not be read to its announced size: " + err.Error()})
			return fmt.Errorf("read the body: %w", err)
		}
		if err := c.ch.Send(chunk); err != nil {
			return c.sendFailed(err)
		}
		left -= int64(len(chunk))
	}
	return nil
}

func (c *Client) sendFailed(err error) error {
	c.fail(&Error{Code: CodeClosed, Message: "the connection to the server closed while sending: " + err.Error()})
	return c.Err()
}

// Hello authenticates as device with token: for the client's app, or, for a
// client with no app, a link session, which only a server-kind device may
// open.
func (c *Client) Hello(ctx context.Context, device, token string) (Welcome, error) {
	frame := map[string]any{"t": "hello", "proto": Version, "device": device, "token": token}
	if c.app != "" {
		frame["app"] = c.app
	}
	var w Welcome
	err := c.request(ctx, frame, nil, nil, func(in incoming) (bool, error) {
		if in.t != "welcome" {
			return false, protocolError("unexpected %s", in.t)
		}
		var err error
		w, err = readWelcome(in.frame)
		return true, err
	})
	return w, err
}

// Pair pairs with a one-time secret, as a device of the client's app when
// kind is "" or as kind. A server-kind pair names no app: give it a client
// with no app. It returns once the welcome follows paired; the server
// confirms the pairing only when the next frame arrives, so send one at once.
func (c *Client) Pair(ctx context.Context, kind, secret, name string) (Paired, Welcome, error) {
	frame := map[string]any{"t": "pair", "proto": Version, "secret": secret, "name": name}
	if kind != "" {
		frame["kind"] = kind
	}
	if c.app != "" {
		frame["app"] = c.app
	}
	var p Paired
	var w Welcome
	got := false
	err := c.request(ctx, frame, nil, nil, func(in incoming) (bool, error) {
		switch {
		case in.t == "paired" && !got:
			device, ok1 := readString(in.frame["device"])
			token, ok2 := readString(in.frame["token"])
			if !ok1 || !ok2 {
				return false, protocolError("a malformed paired")
			}
			p, got = Paired{Device: device, Token: token}, true
			return false, nil
		case in.t == "welcome" && got:
			var err error
			w, err = readWelcome(in.frame)
			return true, err
		}
		return false, protocolError("unexpected %s", in.t)
	})
	return p, w, err
}

// Apps asks for the server's app list, which a server tells only a
// server-kind device. The listener set with OnApps hears it too.
func (c *Client) Apps(ctx context.Context) ([]App, error) {
	var list []App
	err := c.request(ctx, map[string]any{"t": "apps"}, nil, nil, func(in incoming) (bool, error) {
		var err error
		list, err = readApps(in.frame)
		return true, err
	})
	return list, err
}

// Announce tells the main, over a link session, how devices reach this
// server, and returns the main's device registry, which the main pushes
// again on every change. The listener set with OnDevices hears it too.
// handover, when not nil, is this server's own registry, which it hands
// over to the main it follows after stepping down as a main itself.
func (c *Client) Announce(ctx context.Context, self Server, handover *Registry) (Registry, error) {
	frame := map[string]any{"t": "announce", "server": self}
	if handover != nil {
		list := handover.Devices
		if list == nil {
			list = []Device{}
		}
		frame["devices"] = list
		if len(handover.Removed) > 0 {
			frame["removed"] = handover.Removed
		}
	}
	var reg Registry
	err := c.request(ctx, frame, nil, nil, func(in incoming) (bool, error) {
		if in.t != "devices" {
			return false, protocolError("unexpected %s", in.t)
		}
		var err error
		reg, err = readRegistry(in.frame)
		return true, err
	})
	return reg, err
}

// Term tells the server, over a link session, that this server is the main
// of the family at term, reached at self; device and token, when not empty,
// are a server-kind credential for the receiver here, to link with. The
// receiver follows this server when that is higher than its own main, and
// answers with the term and main it follows then.
func (c *Client) Term(ctx context.Context, term int, self Server, device, token string) (TermAnswer, error) {
	frame := map[string]any{"t": "term", "term": term, "server": self}
	if device != "" {
		frame["device"], frame["token"] = device, token
	}
	var a TermAnswer
	err := c.request(ctx, frame, nil, nil, func(in incoming) (bool, error) {
		var n float64
		main, ok := readString(in.frame["main"])
		if in.t != "term" || json.Unmarshal(in.frame["term"], &n) != nil || n < 0 || n != math.Trunc(n) || !ok {
			return false, protocolError("a malformed term")
		}
		a = TermAnswer{Term: int(n), Main: main}
		return true, nil
	})
	return a, err
}

// Invitation is a pairing code of the main, which it issued for a browser on
// the subordinate's computer, and when it expires.
type Invitation struct {
	Code      string
	ExpiresAt time.Time
}

// Invite asks the main, over a link session, for a pairing code of its own:
// the code its page's "Pair a device" shows, for a browser on this server's
// computer to pair with the main.
func (c *Client) Invite(ctx context.Context) (Invitation, error) {
	var inv Invitation
	err := c.request(ctx, map[string]any{"t": "invite"}, nil, nil, func(in incoming) (bool, error) {
		code, ok1 := readString(in.frame["code"])
		at, ok2 := readString(in.frame["expiresAt"])
		expires, err := time.Parse(time.RFC3339, at)
		if in.t != "invited" || !ok1 || code == "" || !ok2 || err != nil {
			return false, protocolError("a malformed invited")
		}
		inv = Invitation{Code: code, ExpiresAt: expires}
		return true, nil
	})
	return inv, err
}

// Diverged tells the main, over a link session, the state of this server's
// copy of app: since is when it diverged from the main's and began to wait
// for a device's choice, nil when no divergence of it waits for one. Nothing
// answers it: it returns once the frame is sent.
func (c *Client) Diverged(ctx context.Context, app string, since *time.Time) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	if err := c.Err(); err != nil {
		return err
	}
	frame := map[string]any{"t": "diverged", "app": app, "since": nil}
	if since != nil {
		frame["since"] = since.UTC().Format(time.RFC3339Nano)
	}
	return c.send(ctx, frame, nil)
}

// Forget asks the server to delete this device. It returns once the server
// has closed the channel, or after a short wait.
func (c *Client) Forget(ctx context.Context) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	if err := c.Err(); err != nil {
		return nil
	}
	if err := c.send(ctx, map[string]any{"t": "forget"}, nil); err != nil {
		return nil
	}
	select {
	case <-c.done:
	case <-time.After(forgetWait):
	case <-ctx.Done():
	}
	return nil
}

func readString(raw json.RawMessage) (string, bool) {
	var s string
	if raw == nil || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// readRevision reads the frame's revision, a string or null; ok is false
// when it is missing or neither.
func readRevision(frame map[string]json.RawMessage) (rev *string, ok bool) {
	return readNullable(frame, "revision")
}

// readNullable reads the field name of frame, a string or null; ok is false
// when it is missing or neither.
func readNullable(frame map[string]json.RawMessage, name string) (*string, bool) {
	raw, present := frame[name]
	if !present {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	s, ok := readString(raw)
	if !ok {
		return nil, false
	}
	return &s, true
}

// readCount reads a whole number of 0 or more, at most 2^53.
func readCount(raw json.RawMessage) (int64, bool) {
	var n float64
	if raw == nil || json.Unmarshal(raw, &n) != nil || n < 0 || n != math.Trunc(n) || n > 1<<53 {
		return 0, false
	}
	return int64(n), true
}

func readWelcome(frame map[string]json.RawMessage) (Welcome, error) {
	serverID, ok1 := readString(frame["serverId"])
	name, ok2 := readString(frame["name"])
	rev, ok3 := readRevision(frame)
	var hosts []string
	ok4 := json.Unmarshal(frame["hosts"], &hosts) == nil && hosts != nil
	for _, h := range hosts {
		ok4 = ok4 && h != ""
	}
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Welcome{}, protocolError("a malformed welcome")
	}
	role, _ := readString(frame["role"])
	w := Welcome{ServerID: serverID, Name: name, Revision: rev, Hosts: hosts, Role: role}
	if term, ok := readCount(frame["term"]); ok {
		w.Term = int(term)
	}
	if raw, ok := frame["subordinates"]; ok {
		w.Subordinates = readServers(raw)
	}
	if raw, ok := frame["diverged"]; ok {
		list, ok := readDiverged(raw)
		if !ok {
			return Welcome{}, protocolError("a malformed welcome")
		}
		if len(list) > 0 {
			w.Diverged = list
		}
	}
	return w, nil
}

// readDiverged reads a list of diverged servers, never nil.
func readDiverged(raw json.RawMessage) ([]DivergedServer, bool) {
	var entries []struct {
		ServerID *string `json:"serverId"`
		Name     *string `json:"name"`
		Since    *string `json:"since"`
	}
	if raw == nil || json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil, false
	}
	out := make([]DivergedServer, 0, len(entries))
	for _, e := range entries {
		if e.ServerID == nil || *e.ServerID == "" || e.Name == nil || e.Since == nil {
			return nil, false
		}
		out = append(out, DivergedServer{ServerID: *e.ServerID, Name: *e.Name, Since: *e.Since})
	}
	return out, true
}

// readServers reads a main's list of subordinates, never nil: an entry this
// client cannot read is left out, and so is a list that is not one.
func readServers(raw json.RawMessage) []Server {
	var entries []json.RawMessage
	_ = json.Unmarshal(raw, &entries)
	out := []Server{}
	for _, e := range entries {
		var s Server
		if json.Unmarshal(e, &s) == nil && s.valid() {
			out = append(out, s)
		}
	}
	return out
}

var (
	fingerprintPattern = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)
	hostPattern        = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)
)

// valid reports whether a device can build a connection to s: the rules of
// the client package's readFallbackServer.
func (s Server) valid() bool {
	if s.ServerID == "" || s.Name == "" || len(s.Password) < minPasswordLength ||
		!fingerprintPattern.MatchString(s.Fingerprint) || s.Port < 1 || s.Port > 65535 || len(s.Hosts) == 0 {
		return false
	}
	for _, h := range s.Hosts {
		if !hostPattern.MatchString(h) {
			return false
		}
	}
	return true
}

// readRegistry reads a devices frame. An entry without id, app or tokenHash
// breaks the frame; the labels are taken when they read, and a server that
// does not read is left out of its entry.
func readRegistry(frame map[string]json.RawMessage) (Registry, error) {
	var raw []struct {
		ID        *string         `json:"id"`
		App       *string         `json:"app"`
		Kind      string          `json:"kind"`
		Name      string          `json:"name"`
		TokenHash *string         `json:"tokenHash"`
		PairedAt  string          `json:"pairedAt"`
		Server    json.RawMessage `json:"server"`
	}
	if json.Unmarshal(frame["devices"], &raw) != nil || raw == nil {
		return Registry{}, protocolError("a malformed devices")
	}
	reg := Registry{Devices: make([]Device, 0, len(raw))}
	for _, d := range raw {
		if d.ID == nil || d.App == nil || d.TokenHash == nil {
			return Registry{}, protocolError("a malformed devices")
		}
		dev := Device{ID: *d.ID, App: *d.App, Kind: d.Kind, Name: d.Name, TokenHash: *d.TokenHash, PairedAt: d.PairedAt}
		var s Server
		if d.Server != nil && json.Unmarshal(d.Server, &s) == nil && s.valid() {
			dev.Server = &s
		}
		reg.Devices = append(reg.Devices, dev)
	}
	if raw, ok := frame["removed"]; ok {
		if json.Unmarshal(raw, &reg.Removed) != nil {
			return Registry{}, protocolError("a malformed devices")
		}
	}
	return reg, nil
}

func readApps(frame map[string]json.RawMessage) ([]App, error) {
	var raw []struct {
		ID    *string         `json:"id"`
		Name  *string         `json:"name"`
		Quota json.RawMessage `json:"quota"`
	}
	if json.Unmarshal(frame["apps"], &raw) != nil || raw == nil {
		return nil, protocolError("a malformed apps")
	}
	list := make([]App, 0, len(raw))
	for _, a := range raw {
		if a.ID == nil || *a.ID == "" {
			return nil, protocolError("a malformed apps")
		}
		app := App{ID: *a.ID}
		if a.Name != nil {
			app.Name = *a.Name
		}
		if a.Quota != nil {
			q, ok := readCount(a.Quota)
			if !ok {
				return nil, protocolError("a malformed apps")
			}
			app.Quota = q
		}
		list = append(list, app)
	}
	return list, nil
}

// readError turns an error frame into an *Error; a code this client does not
// know reads as CodeInternal, a temporary error.
func readError(frame map[string]json.RawMessage) *Error {
	code, _ := readString(frame["code"])
	known := false
	for _, c := range ServerCodes {
		known = known || c == code
	}
	if !known {
		code = CodeInternal
	}
	message, ok := readString(frame["message"])
	if !ok {
		message = "the server reported an error"
	}
	return &Error{Code: code, Message: message}
}
