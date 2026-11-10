package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// message is one message the session sent: a text frame (decoded) or a
// binary message.
type message struct {
	text   map[string]any // nil for a binary message
	binary []byte
}

// fakeChannel records what a session sends. Its buffered amount grows by the
// size of every message, like a channel whose peer reads nothing until the
// test calls drain. It is safe for concurrent use.
type fakeChannel struct {
	mu        sync.Mutex
	sent      []message
	late      []message // sent after Close: never delivered
	buffered  uint64
	threshold uint64
	onLow     func()
	closed    bool
	closes    int
	failSends bool // every send fails, as on a broken transport
	// polled is set by BufferedAmount and cleared by every send: true means
	// the sender looked at the buffer after its last message.
	polled bool
}

var errChannelClosed = errors.New("fake channel closed")

func (c *fakeChannel) SendText(s string) error {
	var frame map[string]any
	if err := json.Unmarshal([]byte(s), &frame); err != nil || frame == nil {
		panic(fmt.Sprintf("session sent a text frame that is not a JSON object: %q", s))
	}
	return c.record(message{text: frame}, len(s))
}

var errSendFailed = errors.New("fake channel: send failed")

// record keeps one sent message. A message sent after Close goes to late,
// not sent: a closed channel delivers nothing.
func (c *fakeChannel) record(m message, size int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polled = false
	switch {
	case c.closed:
		c.late = append(c.late, m)
		return errChannelClosed
	case c.failSends:
		return errSendFailed
	}
	c.sent = append(c.sent, m)
	c.buffered += uint64(size)
	return nil
}

func (c *fakeChannel) Send(data []byte) error {
	return c.record(message{binary: append([]byte(nil), data...)}, len(data))
}

func (c *fakeChannel) BufferedAmount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polled = true
	return c.buffered
}

func (c *fakeChannel) SetBufferedAmountLowThreshold(th uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threshold = th
}

func (c *fakeChannel) OnBufferedAmountLow(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onLow = f
}

func (c *fakeChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closes++
	return nil
}

// setBuffered sets the buffered amount without firing any callback.
func (c *fakeChannel) setBuffered(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffered = n
}

// drain empties the buffer and, as a real channel does, fires the
// OnBufferedAmountLow callback when that crosses the threshold.
func (c *fakeChannel) drain() {
	c.mu.Lock()
	crossed := c.buffered > c.threshold
	c.buffered = 0
	f := c.onLow
	c.mu.Unlock()
	if crossed && f != nil {
		f()
	}
}

func (c *fakeChannel) messages() []message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]message(nil), c.sent...)
}

// lateSends returns the messages the session tried to send after the channel
// closed.
func (c *fakeChannel) lateSends() []message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]message(nil), c.late...)
}

func (c *fakeChannel) texts() []map[string]any {
	var out []map[string]any
	for _, m := range c.messages() {
		if m.text != nil {
			out = append(out, m.text)
		}
	}
	return out
}

func (c *fakeChannel) binaries() [][]byte {
	var out [][]byte
	for _, m := range c.messages() {
		if m.text == nil {
			out = append(out, m.binary)
		}
	}
	return out
}

func (c *fakeChannel) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeChannel) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *fakeChannel) wasPolled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.polled
}

// waitTexts waits until the session has sent at least n text frames and
// returns all of them.
func (c *fakeChannel) waitTexts(t *testing.T, n int) []map[string]any {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d text frames", n), func() bool { return len(c.texts()) >= n })
	return c.texts()
}

// waitFor polls cond until it holds, failing the test after two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

type putCall struct {
	app, device string
	ifEtag      *string
	body        []byte
}

// fakeBackend returns configured results and records its calls. Tests set the
// result fields before the frame that uses them; its methods are safe for
// concurrent use.
type fakeBackend struct {
	mu sync.Mutex

	welcome  Welcome
	helloErr *Error
	onHello  func(s *Session) // runs inside Hello, before it returns

	pairDevice, pairToken string
	pairErr               *Error
	onPair                func(s *Session)

	headEtag *string
	headErr  *Error

	doc     []byte
	docEtag *string
	getErr  *Error

	maxBytes int64

	putEtag     string
	putConflict bool
	putCurrent  *string
	putErr      *Error

	// calls logs every call in order: "hello", "hello returned", "pair",
	// "pair returned", "head", "get", "maxBytes", "put", "forget", "closed".
	calls   []string
	hellos  [][3]string // app, device, token
	pairs   [][3]string // app, secret, name
	puts    []putCall
	forgets [][2]string // app, device
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		welcome:    Welcome{ServerID: "S", Name: "Desk", Hosts: []string{"u.local", "192.168.1.5"}},
		pairDevice: "d1",
		pairToken:  "tok",
		maxBytes:   1 << 20,
		putEtag:    "e2",
	}
}

func (b *fakeBackend) log(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
}

func (b *fakeBackend) Hello(s *Session, app, device, token string) (Welcome, *Error) {
	b.mu.Lock()
	b.calls = append(b.calls, "hello")
	b.hellos = append(b.hellos, [3]string{app, device, token})
	w, err, hook := b.welcome, b.helloErr, b.onHello
	b.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	b.log("hello returned")
	return w, err
}

func (b *fakeBackend) Pair(s *Session, app, secret, name string) (string, string, Welcome, *Error) {
	b.mu.Lock()
	b.calls = append(b.calls, "pair")
	b.pairs = append(b.pairs, [3]string{app, secret, name})
	device, token, w, err, hook := b.pairDevice, b.pairToken, b.welcome, b.pairErr, b.onPair
	b.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	b.log("pair returned")
	if err != nil {
		return "", "", Welcome{}, err
	}
	return device, token, w, nil
}

func (b *fakeBackend) Head(app string) (*string, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "head")
	return b.headEtag, b.headErr
}

func (b *fakeBackend) Get(app string) ([]byte, *string, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "get")
	return b.doc, b.docEtag, b.getErr
}

func (b *fakeBackend) MaxBytes(app string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "maxBytes")
	return b.maxBytes
}

func (b *fakeBackend) Put(from *Session, app, device string, ifEtag *string, body []byte) (string, bool, *string, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "put")
	b.puts = append(b.puts, putCall{app, device, ifEtag, append([]byte(nil), body...)})
	return b.putEtag, b.putConflict, b.putCurrent, b.putErr
}

func (b *fakeBackend) Forget(app, device string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "forget")
	b.forgets = append(b.forgets, [2]string{app, device})
}

func (b *fakeBackend) Closed(s *Session) { b.log("closed") }

func (b *fakeBackend) callLog() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *fakeBackend) putCalls() []putCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]putCall(nil), b.puts...)
}

func (b *fakeBackend) count(call string) int {
	n := 0
	for _, c := range b.callLog() {
		if c == call {
			n++
		}
	}
	return n
}
