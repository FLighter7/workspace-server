package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// fakeFile is a document body the fake backend opens: it counts its closes
// and how much was read from it.
type fakeFile struct {
	*bytes.Reader
	mu      sync.Mutex
	closes  int
	readErr error // returned once read reaches failAt
	failAt  int64
	read    int64
}

func (f *fakeFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil && f.read+int64(len(p)) > f.failAt {
		return 0, f.readErr
	}
	n, err := f.Reader.Read(p)
	f.read += int64(n)
	return n, err
}

func (f *fakeFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeFile) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// fakeSink is a put's body as the fake backend stages it. It keeps the
// bytes (the tests' bodies are small), and records every write.
type fakeSink struct {
	mu       sync.Mutex
	data     []byte
	writes   []int // the size of each write
	closes   int
	failAt   int64 // with failErr: the write that would reach past it fails
	failErr  error
	closeErr error
}

func (k *fakeSink) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closes > 0 {
		return 0, &Error{CodeNoTransaction, "the body is closed"}
	}
	if k.failErr != nil && int64(len(k.data)+len(p)) > k.failAt {
		n := int(k.failAt) - len(k.data)
		k.data = append(k.data, p[:n]...)
		k.writes = append(k.writes, n)
		return n, k.failErr
	}
	k.data = append(k.data, p...)
	k.writes = append(k.writes, len(p))
	return len(p), nil
}

func (k *fakeSink) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closes++
	return k.closeErr
}

func (k *fakeSink) state() (data []byte, writes []int, closes int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]byte(nil), k.data...), append([]int(nil), k.writes...), k.closes
}

type fakeDoc struct {
	etag string
	body []byte
}

type getCall struct {
	app  string
	key  Key
	etag *string
}

type putCall struct {
	app, device, tx string
	key             Key
	ifEtag          *string
	size, offset    int64
	sink            *fakeSink
}

type deleteCall struct {
	app, device, tx string
	key             Key
	ifEtag          *string
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

	head    *string
	headErr *Error

	listing Listing
	listErr *Error

	docs     map[string]fakeDoc // by Key.String()
	revision *string            // what a stale get names
	getErr   *Error
	file     func(*fakeFile) // tweaks each file Get opens
	files    []*fakeFile

	began    []Staged
	beginErr *Error

	putErr *Error
	sink   func() *fakeSink // makes each Sink; nil: a plain one

	deleteErr *Error

	committed Committed
	conflict  *Conflict
	commitErr *Error

	abortErr   *Error
	resolveErr *Error

	apps    []AppInfo
	appsErr *Error

	devices     []DeviceEntry
	removed     []Tombstone
	announceErr *Error
	handovers   []*Registry

	termAnswer TermAnswer
	termErr    *Error
	terms      []termCall

	inviteCode    string
	inviteExpires time.Time
	inviteErr     *Error
	inviteMachine string // what the last invite asked for

	divergedErr *Error
	divergeds   []divergedCall

	// calls logs every call in order: "hello", "hello returned", "pair",
	// "pair returned", "head", "list", "get", "begin", "put", "delete",
	// "commit", "abort", "resolve", "forget", "closed", and the servers'.
	calls     []string
	hellos    [][3]string // app, device, token
	pairs     [][4]string // kind, app, secret, name
	gets      []getCall
	begins    [][3]string // app, device, tx
	puts      []putCall
	deletes   []deleteCall
	commits   [][3]string // app, device, tx
	aborts    [][3]string
	resolves  [][4]string // app, device, server, keep
	forgets   [][2]string // app, device
	announces []ServerInfo
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		welcome:    Welcome{ServerID: "S", Name: "Desk", Hosts: []string{"u.local", "192.168.1.5"}},
		pairDevice: "d1",
		pairToken:  "tok",
		docs:       map[string]fakeDoc{},
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

func (b *fakeBackend) Pair(s *Session, kind, app, secret, name string) (string, string, Welcome, *Error) {
	b.mu.Lock()
	b.calls = append(b.calls, "pair")
	b.pairs = append(b.pairs, [4]string{kind, app, secret, name})
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
	return b.head, b.headErr
}

func (b *fakeBackend) List(app string) (Listing, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "list")
	return b.listing, b.listErr
}

// setDoc makes the backend hold body as the document k, with etag.
func (b *fakeBackend) setDoc(k Key, etag string, body []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.docs[k.String()] = fakeDoc{etag, body}
}

func (b *fakeBackend) Get(app string, k Key, etag *string) (Doc, *Stale, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "get")
	b.gets = append(b.gets, getCall{app, k, etag})
	if b.getErr != nil {
		return Doc{}, nil, b.getErr
	}
	d, ok := b.docs[k.String()]
	if !ok || etag != nil && *etag != d.etag {
		return Doc{}, &Stale{Revision: b.revision}, nil
	}
	f := &fakeFile{Reader: bytes.NewReader(d.body)}
	if b.file != nil {
		b.file(f)
	}
	b.files = append(b.files, f)
	return Doc{DocInfo: DocInfo{Key: k, Etag: d.etag, Bytes: int64(len(d.body))}, Body: f}, nil, nil
}

// openFiles returns the files Get opened.
func (b *fakeBackend) openFiles() []*fakeFile {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fakeFile(nil), b.files...)
}

func (b *fakeBackend) Begin(app, device, tx string) ([]Staged, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "begin")
	b.begins = append(b.begins, [3]string{app, device, tx})
	return b.began, b.beginErr
}

// Put makes the Sink outside the backend's lock: a test's maker may close
// the session, which tells the backend.
func (b *fakeBackend) Put(app, device, tx string, k Key, ifEtag *string, size, offset int64) (Sink, *Error) {
	b.mu.Lock()
	b.calls = append(b.calls, "put")
	c := putCall{app: app, device: device, tx: tx, key: k, ifEtag: ifEtag, size: size, offset: offset}
	maker, err := b.sink, b.putErr
	b.mu.Unlock()
	if err == nil {
		c.sink = &fakeSink{}
		if maker != nil {
			c.sink = maker()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts = append(b.puts, c)
	if err != nil {
		return nil, err
	}
	return c.sink, nil
}

func (b *fakeBackend) Delete(app, device, tx string, k Key, ifEtag *string) *Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "delete")
	b.deletes = append(b.deletes, deleteCall{app, device, tx, k, ifEtag})
	return b.deleteErr
}

// Commit answers through the session, under the backend's lock, as the hub
// does under the app's lock.
func (b *fakeBackend) Commit(from *Session, app, device, tx string) *Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "commit")
	b.commits = append(b.commits, [3]string{app, device, tx})
	switch {
	case b.commitErr != nil:
		return b.commitErr
	case b.conflict != nil:
		from.AnswerConflict(*b.conflict)
	default:
		from.AnswerCommitted(b.committed)
	}
	return nil
}

func (b *fakeBackend) Abort(app, device, tx string) *Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "abort")
	b.aborts = append(b.aborts, [3]string{app, device, tx})
	return b.abortErr
}

func (b *fakeBackend) Resolve(from *Session, app, device, server, keep string) *Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "resolve")
	b.resolves = append(b.resolves, [4]string{app, device, server, keep})
	return b.resolveErr
}

func (b *fakeBackend) Forget(app, device string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "forget")
	b.forgets = append(b.forgets, [2]string{app, device})
}

func (b *fakeBackend) Apps(s *Session) ([]AppInfo, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "apps")
	return b.apps, b.appsErr
}

// Announce records the server and, unless told to refuse, answers with the
// registry the test set, as the hub does.
func (b *fakeBackend) Announce(s *Session, server ServerInfo, handover *Registry) *Error {
	b.mu.Lock()
	b.calls = append(b.calls, "announce")
	b.announces = append(b.announces, server)
	b.handovers = append(b.handovers, handover)
	list, removed, err := b.devices, b.removed, b.announceErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	s.PushDevices(Registry{Devices: list, Removed: removed})
	return nil
}

// termCall is one call of Term.
type termCall struct {
	term          int
	server        ServerInfo
	device, token string
}

func (b *fakeBackend) Term(s *Session, term int, server ServerInfo, device, token string) (TermAnswer, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "term")
	b.terms = append(b.terms, termCall{term, server, device, token})
	return b.termAnswer, b.termErr
}

func (b *fakeBackend) Invite(s *Session, machine string) (string, time.Time, *Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "invite")
	b.inviteMachine = machine
	return b.inviteCode, b.inviteExpires, b.inviteErr
}

// divergedCall is one call of Diverged.
type divergedCall struct {
	app   string
	since *string
}

func (b *fakeBackend) Diverged(s *Session, app string, since *string) *Error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "diverged")
	b.divergeds = append(b.divergeds, divergedCall{app, since})
	return b.divergedErr
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

var _ io.ReadSeekCloser = (*fakeFile)(nil)
