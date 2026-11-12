package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/store"
)

// obj is a decoded text frame; JSON numbers decode as float64.
type obj = map[string]any

// fakeChannel records what a session sends. It never buffers, and it is safe
// for concurrent use. What is sent after Close is counted in late, not kept:
// a closed channel delivers nothing.
type fakeChannel struct {
	mu       sync.Mutex
	texts    []obj
	binaries [][]byte
	closed   bool
	late     int
}

var errChannelClosed = errors.New("fake channel closed")

func (c *fakeChannel) SendText(s string) error {
	var f obj
	if err := json.Unmarshal([]byte(s), &f); err != nil || f == nil {
		panic(fmt.Sprintf("session sent a text frame that is not a JSON object: %q", s))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.late++
		return errChannelClosed
	}
	c.texts = append(c.texts, f)
	return nil
}

func (c *fakeChannel) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.late++
		return errChannelClosed
	}
	c.binaries = append(c.binaries, append([]byte(nil), data...))
	return nil
}

func (c *fakeChannel) lateSends() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.late
}

func (c *fakeChannel) BufferedAmount() uint64               { return 0 }
func (c *fakeChannel) SetBufferedAmountLowThreshold(uint64) {}
func (c *fakeChannel) OnBufferedAmountLow(func())           {}

func (c *fakeChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeChannel) frames() []obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]obj(nil), c.texts...)
}

func (c *fakeChannel) body() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Join(c.binaries, nil)
}

func (c *fakeChannel) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// clock is a fake clock that moves only when told.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// logLines records what the hub logs.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// env is a hub over real registries in a temporary directory. apps.json
// starts with the default "todo" app only.
type env struct {
	dir      string // the data directory; documents are in dir/apps
	logs     *logLines
	hub      *Hub
	clock    *clock
	apps     *apps.Registry
	devices  *devices.Registry
	store    *store.Store
	secrets  *pairing.Secrets
	refusals *pairing.Refusals

	hookMu  sync.Mutex
	onName  func()           // runs inside every welcome, after the session is registered
	onStore func(app string) // Deps.Stored
}

// setOnName sets the function that runs inside every welcome: the hub reads
// the server's name there, after it has registered the session and released
// the app's lock, before it answers.
func (e *env) setOnName(f func()) {
	e.hookMu.Lock()
	defer e.hookMu.Unlock()
	e.onName = f
}

func (e *env) name() string {
	e.hookMu.Lock()
	f := e.onName
	e.hookMu.Unlock()
	if f != nil {
		f()
	}
	return "Desk"
}

// newEnv builds the env; each tweak runs before the hub is made, so it can
// replace the env's registries.
func newEnv(t *testing.T, tweaks ...func(*env)) *env {
	t.Helper()
	dir := t.TempDir()
	clk := &clock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	ar, err := apps.Open(filepath.Join(dir, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	dr, err := devices.Open(filepath.Join(dir, "devices.json"), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		dir:      dir,
		logs:     &logLines{},
		clock:    clk,
		apps:     ar,
		devices:  dr,
		store:    store.New(filepath.Join(dir, "apps"), clk.Now),
		secrets:  pairing.NewSecrets(clk.Now),
		refusals: pairing.NewRefusals(clk.Now),
	}
	for _, tweak := range tweaks {
		tweak(e)
	}
	e.hub = New(Deps{
		ServerID: "srv-1",
		Name:     e.name,
		Hosts:    func() []string { return []string{"u.local", "192.168.1.5"} },
		Apps:     e.apps,
		Devices:  e.devices,
		Store:    e.store,
		Secrets:  e.secrets,
		Refusals: e.refusals,
		Now:      clk.Now,
		Logf:     e.logs.printf,
		Stored:   e.stored,
	})
	return e
}

func (e *env) stored(app string) {
	e.hookMu.Lock()
	f := e.onStore
	e.hookMu.Unlock()
	if f != nil {
		f(app)
	}
}

func (e *env) addApp(t *testing.T, a apps.App) {
	t.Helper()
	if err := e.apps.Add(a); err != nil {
		t.Fatal(err)
	}
}

// device pairs a device of app straight in the registry and returns its id
// and token.
func (e *env) device(t *testing.T, app string) (id, token string) {
	t.Helper()
	d, token, err := e.devices.Pair(app, "Phone")
	if err != nil {
		t.Fatal(err)
	}
	return d.ID, token
}

// connect opens a session from remote; it is closed when the test ends.
func (e *env) connect(t *testing.T, remote string) (*protocol.Session, *fakeChannel) {
	t.Helper()
	fc := &fakeChannel{}
	s := protocol.NewSession(fc, remote, e.hub, protocol.Options{})
	t.Cleanup(s.Close)
	return s, fc
}

// login connects from remote and says hello as device, waiting for the
// welcome.
func (e *env) login(t *testing.T, remote, app, id, token string) (*protocol.Session, *fakeChannel) {
	t.Helper()
	s, fc := e.connect(t, remote)
	s.HandleText(helloFrame(app, id, token))
	waitFrames(t, fc, "welcome", 1)
	return s, fc
}

func (e *env) deviceIDs() []string {
	var ids []string
	for _, d := range e.devices.List() {
		ids = append(ids, d.ID)
	}
	return ids
}

func frame(fields obj) string {
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func helloFrame(app, device, token string) string {
	return frame(obj{"t": "hello", "proto": 1, "app": app, "device": device, "token": token})
}

func pairFrame(app, secret, name string) string {
	return frame(obj{"t": "pair", "proto": 1, "app": app, "secret": secret, "name": name})
}

// put sends a put of body that expects the document to have ifEtag.
func put(s *protocol.Session, ifEtag *string, body []byte) {
	s.HandleText(frame(obj{"t": "put", "ifEtag": ifEtag, "bytes": len(body)}))
	if len(body) > 0 {
		s.HandleBinary(body)
	}
}

func ptr(s string) *string { return &s }

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

// framesOf returns the frames sent so far whose type is one of types.
func framesOf(fc *fakeChannel, types ...string) []obj {
	var out []obj
	for _, f := range fc.frames() {
		for _, typ := range types {
			if f["t"] == typ {
				out = append(out, f)
			}
		}
	}
	return out
}

// waitFrames waits until at least n frames of one of types have been sent and
// returns them.
func waitFrames(t *testing.T, fc *fakeChannel, typ string, n int, more ...string) []obj {
	t.Helper()
	types := append([]string{typ}, more...)
	waitFor(t, fmt.Sprintf("%d %v frames", n, types), func() bool { return len(framesOf(fc, types...)) >= n })
	return framesOf(fc, types...)
}

// settle sends head on an authenticated session and waits for the answer.
// The session sends in order, so every frame queued before the head has been
// sent by then; it returns all frames sent.
func settle(t *testing.T, s *protocol.Session, fc *fakeChannel) []obj {
	t.Helper()
	n := len(framesOf(fc, "head"))
	s.HandleText(`{"t":"head"}`)
	waitFrames(t, fc, "head", n+1)
	return fc.frames()
}

func countType(frames []obj, typ string) int {
	n := 0
	for _, f := range frames {
		if f["t"] == typ {
			n++
		}
	}
	return n
}

// expectError checks that f is an error frame with code and a message.
func expectError(t *testing.T, f obj, code string) {
	t.Helper()
	msg, _ := f["message"].(string)
	if f["t"] != "error" || f["code"] != code || msg == "" {
		t.Errorf("frame %v, want an error frame with code %q and a message", f, code)
	}
}

// expectClosedWithError waits for the channel to close and checks that the
// last frame sent was an error with code.
func expectClosedWithError(t *testing.T, fc *fakeChannel, code string) {
	t.Helper()
	waitFor(t, "the channel to close", fc.isClosed)
	frames := fc.frames()
	if len(frames) == 0 {
		t.Fatalf("channel closed with no error frame")
	}
	expectError(t, frames[len(frames)-1], code)
	if n := fc.lateSends(); n != 0 {
		t.Errorf("%d messages sent after the channel closed", n)
	}
}

// expectOpenAfterError checks that an unauthenticated session's last answer
// was an error with code and that the channel stayed open: a later frame is
// still answered, which a closing session never does.
func expectOpenAfterError(t *testing.T, s *protocol.Session, fc *fakeChannel, code string) {
	t.Helper()
	errs := waitFrames(t, fc, "error", 1)
	expectError(t, errs[len(errs)-1], code)
	s.HandleText(`{"t":"head"}`)
	errs = waitFrames(t, fc, "error", len(errs)+1)
	expectError(t, errs[len(errs)-1], protocol.CodeNotAuthenticated)
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

func expectSessions(t *testing.T, h *Hub, want ...SessionInfo) {
	t.Helper()
	got := h.Sessions()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sessions() = %v, want %v", got, want)
	}
}

func TestHelloOutcomes(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		doc := []byte(`{"items":[]}`)
		if _, err := e.store.Write("todo", doc, nil, 10); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Hour)

		s, fc := e.connect(t, "192.168.1.9:5000")
		expectSessions(t, e.hub) // connected but not authenticated
		s.HandleText(helloFrame("todo", id, token))
		w := waitFrames(t, fc, "welcome", 1)[0]
		want := obj{
			"t": "welcome", "proto": float64(1), "role": "main", "serverId": "srv-1", "name": "Desk",
			"etag": store.Etag(doc), "hosts": []any{"u.local", "192.168.1.5"}, "subordinates": []any{},
		}
		if !reflect.DeepEqual(w, want) {
			t.Errorf("welcome %v, want %v", w, want)
		}
		expectSessions(t, e.hub, SessionInfo{App: "todo", Device: id, Remote: "192.168.1.9:5000"})
		if seen := e.devices.List()[0].LastSeen; !seen.Equal(e.clock.Now()) {
			t.Errorf("LastSeen %v, want %v (Touch on hello)", seen, e.clock.Now())
		}
	})

	t.Run("no document", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		_, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
		if w := framesOf(fc, "welcome")[0]; w["etag"] != nil {
			t.Errorf("welcome etag %v, want null", w["etag"])
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		e := newEnv(t)
		id, _ := e.device(t, "todo")
		s, fc := e.connect(t, "192.168.1.9:5000")
		s.HandleText(helloFrame("todo", id, "wrong"))
		expectClosedWithError(t, fc, protocol.CodeBadToken)
		expectSessions(t, e.hub)
	})

	t.Run("unknown device", func(t *testing.T) {
		e := newEnv(t)
		_, token := e.device(t, "todo")
		s, fc := e.connect(t, "192.168.1.9:5000")
		s.HandleText(helloFrame("todo", "nosuchdevice0000", token))
		expectClosedWithError(t, fc, protocol.CodeBadToken)
	})

	t.Run("unlisted app", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		s, fc := e.connect(t, "192.168.1.9:5000")
		s.HandleText(helloFrame("notes", id, token))
		expectClosedWithError(t, fc, protocol.CodeUnknownApp)
		expectSessions(t, e.hub)
	})
}

func TestPairConsumesSecret(t *testing.T) {
	e := newEnv(t)
	secret, _ := e.secrets.Issue()
	e.refusals.Record("todo", "Old phone")

	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("todo", secret, "Phone"))
	waitFrames(t, fc, "welcome", 1)
	frames := fc.frames()
	if len(frames) != 2 || frames[0]["t"] != "paired" {
		t.Fatalf("sent %v, want paired then welcome", frames)
	}
	device, _ := frames[0]["device"].(string)
	token, _ := frames[0]["token"].(string)
	if device == "" || token == "" {
		t.Fatalf("paired frame %v lacks a device or token", frames[0])
	}
	if w := frames[1]; w["serverId"] != "srv-1" || w["etag"] != nil {
		t.Errorf("welcome %v", w)
	}
	list := e.devices.List()
	if len(list) != 1 || list[0].ID != device || list[0].App != "todo" || list[0].Name != "Phone" {
		t.Errorf("devices %+v, want the paired Phone of todo", list)
	}
	if r := e.refusals.Recent(); len(r) != 0 {
		t.Errorf("refusals for the paired app not cleared: %v", r)
	}
	if e.secrets.Check(secret) {
		t.Error("secret still valid after pairing")
	}
	expectSessions(t, e.hub, SessionInfo{App: "todo", Device: device, Remote: "192.168.1.9:5000"})

	// The token works for hello.
	e.login(t, "192.168.1.9:5001", "todo", device, token)

	// The secret works once.
	s3, fc3 := e.connect(t, "192.168.1.9:5002")
	s3.HandleText(pairFrame("todo", secret, "Laptop"))
	expectClosedWithError(t, fc3, protocol.CodeBadSecret)
	if n := len(e.devices.List()); n != 1 {
		t.Errorf("%d devices after a reused secret, want 1", n)
	}
}

func TestPairWrongSecret(t *testing.T) {
	e := newEnv(t)
	e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("todo", "not-the-secret", "Phone"))
	expectClosedWithError(t, fc, protocol.CodeBadSecret)
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0", n)
	}
}

func TestPairUnknownAppRecordsRefusal(t *testing.T) {
	e := newEnv(t)
	secret, _ := e.secrets.Issue()

	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("notes", secret, "Phone"))
	expectOpenAfterError(t, s, fc, protocol.CodeUnknownApp)
	r := e.refusals.Recent()
	if len(r) != 1 || r[0].App != "notes" || r[0].Device != "Phone" {
		t.Errorf("refusals %+v, want one for notes from Phone", r)
	}
	if !e.secrets.Check(secret) {
		t.Error("secret consumed by a refused pairing")
	}
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0", n)
	}

	// The user adds the app; the device's next attempt succeeds.
	e.addApp(t, apps.App{ID: "notes"})
	s.HandleText(pairFrame("notes", secret, "Phone"))
	waitFrames(t, fc, "welcome", 1)
	if r := e.refusals.Recent(); len(r) != 0 {
		t.Errorf("refusals %+v after pairing notes, want none", r)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	for i := range 10 {
		s, fc := e.connect(t, fmt.Sprintf("192.168.1.9:%d", 5000+i))
		s.HandleText(helloFrame("todo", id, "wrong"))
		expectClosedWithError(t, fc, protocol.CodeBadToken)
	}

	// The IP is blocked: a correct hello is refused with blocked.
	s, fc := e.connect(t, "192.168.1.9:6000")
	s.HandleText(helloFrame("todo", id, token))
	expectClosedWithError(t, fc, protocol.CodeBlocked)
	expectSessions(t, e.hub)

	// A pair with a valid secret is refused without using the secret.
	secret, _ := e.secrets.Issue()
	s, fc = e.connect(t, "192.168.1.9:6001")
	s.HandleText(pairFrame("todo", secret, "Laptop"))
	expectClosedWithError(t, fc, protocol.CodeBlocked)
	if !e.secrets.Check(secret) || len(e.devices.List()) != 1 {
		t.Error("a pair from a blocked IP was checked")
	}

	// Another IP is not blocked.
	e.login(t, "192.168.1.10:5000", "todo", id, token)

	// Refusals during the block do not extend it.
	e.clock.Advance(9 * time.Minute)
	s, fc = e.connect(t, "192.168.1.9:6002")
	s.HandleText(helloFrame("todo", id, token))
	expectClosedWithError(t, fc, protocol.CodeBlocked)

	// 10 min 1 s after the 10th failure the block is over.
	e.clock.Advance(time.Minute + time.Second)
	e.login(t, "192.168.1.9:6003", "todo", id, token)
}

func TestRateLimitCountsEveryRefusal(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	secret, _ := e.secrets.Issue()
	port := 5000
	attempt := func(frame string) {
		t.Helper()
		s, fc := e.connect(t, fmt.Sprintf("[fe80::1]:%d", port))
		port++
		s.HandleText(frame)
		waitFrames(t, fc, "error", 1)
	}
	for range 4 {
		attempt(helloFrame("todo", id, "wrong")) // badToken
	}
	for range 3 {
		attempt(helloFrame("notes", id, token)) // unknownApp
	}
	for range 2 {
		attempt(pairFrame("todo", "wrong", "Phone")) // badSecret
	}
	attempt(pairFrame("notes", secret, "Phone")) // unknownApp

	s, fc := e.connect(t, "[fe80::1]:6000")
	s.HandleText(helloFrame("todo", id, token))
	expectClosedWithError(t, fc, protocol.CodeBlocked)
	e.login(t, "[fe80::2]:5000", "todo", id, token)
}

func TestRateLimitForgetsOldFailures(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	port := 5000
	fail := func(n int) {
		t.Helper()
		for range n {
			s, fc := e.connect(t, fmt.Sprintf("192.168.1.9:%d", port))
			port++
			s.HandleText(helloFrame("todo", id, "wrong"))
			expectClosedWithError(t, fc, protocol.CodeBadToken)
		}
	}
	fail(9)
	e.clock.Advance(time.Minute)
	fail(9)
	e.login(t, "192.168.1.9:6000", "todo", id, token) // 9 within the minute

	fail(1) // the 10th within the minute
	s, fc := e.connect(t, "192.168.1.9:6001")
	s.HandleText(helloFrame("todo", id, token))
	expectClosedWithError(t, fc, protocol.CodeBlocked)
}

func TestHeadAndGet(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)

	s.HandleText(`{"t":"head"}`)
	s.HandleText(`{"t":"get"}`)
	got := waitFrames(t, fc, "head", 2, "doc")
	want := []obj{{"t": "head", "etag": nil}, {"t": "doc", "etag": nil, "bytes": float64(0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}

	doc := []byte(`{"items":["milk"]}`)
	etag, err := e.store.Write("todo", doc, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	s.HandleText(`{"t":"head"}`)
	s.HandleText(`{"t":"get"}`)
	got = waitFrames(t, fc, "head", 4, "doc")[2:]
	want = []obj{{"t": "head", "etag": etag}, {"t": "doc", "etag": etag, "bytes": float64(len(doc))}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	waitFor(t, "the document body", func() bool { return len(fc.body()) >= len(doc) })
	if !bytes.Equal(fc.body(), doc) {
		t.Errorf("body %q, want %q", fc.body(), doc)
	}
}

func TestPutBroadcast(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	idC, tokC := e.device(t, "notes")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	_, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	c, fcC := e.login(t, "192.168.1.9:5002", "notes", idC, tokC)

	doc := []byte(`{"items":[]}`)
	put(a, nil, doc)
	etag := store.Etag(doc)
	if f := waitFrames(t, fa, "stored", 1)[0]; f["etag"] != etag {
		t.Errorf("stored %v, want etag %s", f, etag)
	}
	if f := waitFrames(t, fb, "changed", 1)[0]; f["etag"] != etag {
		t.Errorf("B got %v, want changed %s", f, etag)
	}
	if n := countType(settle(t, a, fa), "changed"); n != 0 {
		t.Errorf("the writer got %d changed frames, want 0", n)
	}
	if n := countType(settle(t, c, fcC), "changed"); n != 0 {
		t.Errorf("a session of another app got %d changed frames, want 0", n)
	}
	if _, cur, _ := e.store.Read("todo"); cur == nil || *cur != etag {
		t.Errorf("stored etag %v, want %s", cur, etag)
	}
}

func TestPutConflictAndNotJSON(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)

	first := []byte(`{"v":1}`)
	put(a, nil, first)
	waitFrames(t, fa, "stored", 1)
	etag := store.Etag(first)

	// null ifEtag although a document exists, then a wrong etag.
	put(a, nil, []byte(`{"v":2}`))
	put(a, ptr("0000"), []byte(`{"v":3}`))
	for _, f := range waitFrames(t, fa, "conflict", 2) {
		if f["etag"] != etag {
			t.Errorf("conflict %v, want the current etag %s", f, etag)
		}
	}

	put(a, &etag, []byte("not json"))
	errs := waitFrames(t, fa, "error", 1)
	expectError(t, errs[0], protocol.CodeNotJSON)
	settle(t, a, fa) // the channel stays open
	if fa.isClosed() {
		t.Error("notJson closed the channel")
	}

	if _, cur, _ := e.store.Read("todo"); cur == nil || *cur != etag {
		t.Errorf("stored etag %v, want %s unchanged", cur, etag)
	}
	if n := countType(settle(t, b, fb), "changed"); n != 1 {
		t.Errorf("B got %d changed frames, want 1 (refused puts are not pushed)", n)
	}
}

// Deps.Stored hears every stored put, once the hub's locks are released,
// and nothing else: not a refused put, and not Replace, which is how the link
// writes (a hook on its own writes would send them back).
func TestStoredHook(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var calls []string
	locked := false
	e.hookMu.Lock()
	e.onStore = func(app string) {
		free := make(chan struct{})
		go func() {
			l := e.hub.appLock(app)
			l.Lock()
			l.Unlock()
			e.hub.servers.Lock()
			e.hub.servers.Unlock()
			close(free)
		}()
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-free:
		case <-time.After(time.Second):
			locked = true
		}
		calls = append(calls, app)
	}
	e.hookMu.Unlock()
	heard := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}

	id, tok := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", id, tok)
	first := []byte(`{"v":1}`)
	put(a, nil, first)
	waitFrames(t, fa, "stored", 1)
	if got := heard(); !slices.Equal(got, []string{"todo"}) {
		t.Fatalf("Stored heard %v after a stored put, want [todo]", got)
	}
	mu.Lock()
	if locked {
		t.Error("Stored ran under the app's lock or the servers lock")
	}
	mu.Unlock()

	etag := store.Etag(first)
	put(a, nil, []byte(`{"v":2}`)) // a conflict
	waitFrames(t, fa, "conflict", 1)
	put(a, &etag, []byte("not json"))
	waitFrames(t, fa, "error", 1)
	if _, err := e.hub.Replace("todo", []byte(`{"v":3}`), &etag); err != nil {
		t.Fatal(err)
	}
	if got := heard(); !slices.Equal(got, []string{"todo"}) {
		t.Errorf("Stored heard %v, want only the one stored put", got)
	}
}

func TestPutLimits(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "tiny", MaxBytes: 1024})
	if got := e.hub.MaxBytes("tiny"); got != 1024 {
		t.Errorf("MaxBytes(tiny) = %d, want 1024", got)
	}
	if got := e.hub.MaxBytes("todo"); got != apps.DefaultMaxBytes {
		t.Errorf("MaxBytes(todo) = %d, want %d", got, apps.DefaultMaxBytes)
	}
	if got := e.hub.MaxBytes("nope"); got != 0 {
		t.Errorf("MaxBytes(nope) = %d, want 0", got)
	}

	id, token := e.device(t, "tiny")
	s, fc := e.login(t, "192.168.1.9:5000", "tiny", id, token)
	put(s, nil, bytes.Repeat([]byte(" "), 2000))
	expectError(t, waitFrames(t, fc, "error", 1)[0], protocol.CodeTooLarge)

	// The app disappears from apps.json; its session is still open. Its limit
	// is now 0, so any body is too large, and an empty one reaches Put.
	if err := e.apps.Remove("tiny"); err != nil {
		t.Fatal(err)
	}
	put(s, nil, []byte(`{}`))
	expectError(t, waitFrames(t, fc, "error", 2)[1], protocol.CodeTooLarge)
	put(s, nil, nil)
	expectError(t, waitFrames(t, fc, "error", 3)[2], protocol.CodeUnknownApp)
}

// Two devices writing at the same instant with the same ifEtag: exactly one is
// stored, the other gets conflict with the winner's etag.
func TestConcurrentPutsWithSameEtag(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)

	var current *string
	for round := range 20 {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, s := range []*protocol.Session{a, b} {
			body := []byte(fmt.Sprintf(`{"round":%d,"writer":%d}`, round, i))
			wg.Go(func() {
				<-start
				put(s, current, body)
			})
		}
		close(start)
		wg.Wait()

		ra := waitFrames(t, fa, "stored", round+1, "conflict")[round]
		rb := waitFrames(t, fb, "stored", round+1, "conflict")[round]
		var winner, loser obj
		switch {
		case ra["t"] == "stored" && rb["t"] == "conflict":
			winner, loser = ra, rb
		case rb["t"] == "stored" && ra["t"] == "conflict":
			winner, loser = rb, ra
		default:
			t.Fatalf("round %d: answers %v and %v, want one stored and one conflict", round, ra, rb)
		}
		etag, _ := winner["etag"].(string)
		if loser["etag"] != etag {
			t.Fatalf("round %d: conflict %v, want the winner's etag %s", round, loser, etag)
		}
		if _, cur, _ := e.store.Read("todo"); cur == nil || *cur != etag {
			t.Fatalf("round %d: stored etag %v, want the winner's %s", round, cur, etag)
		}
		current = &etag
	}
}

func TestRemoveAppClosesSessionsAndDeletesData(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	id, token := e.device(t, "todo")
	idN, tokN := e.device(t, "notes")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	n, fn := e.login(t, "192.168.1.9:5001", "notes", idN, tokN)
	put(s, nil, []byte(`{"x":1}`))
	waitFrames(t, fc, "stored", 1)

	if err := e.hub.RemoveApp("todo"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the todo session to close", fc.isClosed)
	if doc, etag, err := e.store.Read("todo"); doc != nil || etag != nil || err != nil {
		t.Errorf("Store.Read(todo) = %q, %v, %v; want no document", doc, etag, err)
	}
	if _, ok := e.apps.Get("todo"); ok {
		t.Error("todo still listed")
	}
	if ids := e.deviceIDs(); !reflect.DeepEqual(ids, []string{idN}) {
		t.Errorf("devices %v, want only notes' %s", ids, idN)
	}
	expectSessions(t, e.hub, SessionInfo{App: "notes", Device: idN, Remote: "192.168.1.9:5001"})
	settle(t, n, fn)
	if fn.isClosed() {
		t.Error("the notes session was closed")
	}

	if err := e.hub.RemoveApp("todo"); !errors.Is(err, apps.ErrNotFound) {
		t.Errorf("second RemoveApp = %v, want ErrNotFound", err)
	}
}

func TestRemoveDeviceClosesItsSession(t *testing.T) {
	e := newEnv(t)
	id1, tok1 := e.device(t, "todo")
	id2, tok2 := e.device(t, "todo")
	_, f1 := e.login(t, "192.168.1.9:5000", "todo", id1, tok1)
	s2, f2 := e.login(t, "192.168.1.9:5001", "todo", id2, tok2)

	if !e.hub.RemoveDevice(id1) {
		t.Fatal("RemoveDevice = false")
	}
	waitFor(t, "the removed device's session to close", f1.isClosed)
	settle(t, s2, f2)
	if f2.isClosed() {
		t.Error("the other device's session was closed")
	}
	expectSessions(t, e.hub, SessionInfo{App: "todo", Device: id2, Remote: "192.168.1.9:5001"})

	// Its token stops working.
	s3, f3 := e.connect(t, "192.168.1.9:5002")
	s3.HandleText(helloFrame("todo", id1, tok1))
	expectClosedWithError(t, f3, protocol.CodeBadToken)

	if e.hub.RemoveDevice(id1) {
		t.Error("second RemoveDevice = true")
	}
}

// A put that reaches the hub after its device was removed, as one in flight
// when the device is removed does, stores nothing.
func TestPutFromARemovedDevice(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	first := []byte(`{"v":1}`)
	put(s, nil, first)
	waitFrames(t, fc, "stored", 1)
	etag := store.Etag(first)

	if !e.hub.RemoveDevice(id) {
		t.Fatal("RemoveDevice = false")
	}
	_, conflict, _, perr := e.hub.Put(s, "todo", id, &etag, []byte(`{"v":2}`))
	if perr == nil || perr.Code != protocol.CodeBadToken || conflict {
		t.Errorf("Put after RemoveDevice = conflict %v, error %v; want error %q", conflict, perr, protocol.CodeBadToken)
	}
	if doc, _, _ := e.store.Read("todo"); !bytes.Equal(doc, first) {
		t.Errorf("document %q, want %q unchanged", doc, first)
	}
}

// An internal error reaches the device without its text, which can name
// files on the server's computer; the server's log gets the text.
func TestInternalErrorKeepsTheDetailsInTheLog(t *testing.T) {
	e := newEnv(t)
	// A directory where the document should be: reading it fails.
	if err := os.MkdirAll(filepath.Join(e.dir, "apps", "todo", "data.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, perr := e.hub.Head("todo")
	if perr == nil || perr.Code != protocol.CodeInternal {
		t.Fatalf("Head = %v, want error %q", perr, protocol.CodeInternal)
	}
	if perr.Message == "" || strings.Contains(perr.Message, "data.json") || strings.Contains(perr.Message, e.dir) {
		t.Errorf("message %q, want one that names no file", perr.Message)
	}
	if !strings.Contains(e.logs.String(), filepath.Join("todo", "data.json")) {
		t.Errorf("log %q, want the error naming the file", e.logs)
	}
}

func TestCloseAppKeepsDevicesAndData(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	put(s, nil, []byte(`{}`))
	waitFrames(t, fc, "stored", 1)

	// As when the entry is deleted from apps.json by hand.
	if err := e.apps.Remove("todo"); err != nil {
		t.Fatal(err)
	}
	e.hub.CloseApp("todo")
	waitFor(t, "the session to close", fc.isClosed)
	expectSessions(t, e.hub)
	if ids := e.deviceIDs(); !reflect.DeepEqual(ids, []string{id}) {
		t.Errorf("devices %v, want %s kept", ids, id)
	}
	if doc, _, _ := e.store.Read("todo"); doc == nil {
		t.Error("document deleted")
	}
}

func TestForgetClosesTheDevicesSessions(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	other, otherToken := e.device(t, "todo")
	tab1, f1 := e.login(t, "192.168.1.9:5000", "todo", id, token)
	_, f2 := e.login(t, "192.168.1.9:5001", "todo", id, token)
	o, fo := e.login(t, "192.168.1.9:5002", "todo", other, otherToken)

	tab1.HandleText(`{"t":"forget"}`)
	waitFor(t, "the forgetting session to close", f1.isClosed)
	waitFor(t, "the device's other session to close", f2.isClosed)
	if ids := e.deviceIDs(); !reflect.DeepEqual(ids, []string{other}) {
		t.Errorf("devices %v, want only %s", ids, other)
	}
	settle(t, o, fo)
	if fo.isClosed() {
		t.Error("another device's session was closed")
	}
	expectSessions(t, e.hub, SessionInfo{App: "todo", Device: other, Remote: "192.168.1.9:5002"})
}

func TestClosedSessionLeavesTheList(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, _ := e.login(t, "192.168.1.9:5000", "todo", id, token)
	idB, tokB := e.device(t, "todo")
	e.login(t, "192.168.1.9:5001", "todo", idB, tokB)

	s.Close()
	expectSessions(t, e.hub, SessionInfo{App: "todo", Device: idB, Remote: "192.168.1.9:5001"})
	e.hub.Closed(s) // again: nothing happens
	unregistered, _ := e.connect(t, "192.168.1.9:5002")
	e.hub.Closed(unregistered) // never registered: nothing happens
	expectSessions(t, e.hub, SessionInfo{App: "todo", Device: idB, Remote: "192.168.1.9:5001"})
}

// breakDocument puts a directory where app's document should be, so that
// every read and write of it fails, and returns the function that repairs it.
func (e *env) breakDocument(t *testing.T, app string) func() {
	t.Helper()
	path := filepath.Join(e.dir, "apps", app, "data.json")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

// expectNoFileInFrames checks that no frame names a file of the server.
func expectNoFileInFrames(t *testing.T, e *env, fc *fakeChannel) {
	t.Helper()
	for _, f := range fc.frames() {
		msg, _ := f["message"].(string)
		if strings.Contains(msg, ".json") || strings.Contains(msg, e.dir) {
			t.Errorf("frame %v names a file", f)
		}
	}
}

// The secret passes the check but is gone by the time it is consumed, as
// when another pairing takes it in between.
func TestPairLosesTheSecretRace(t *testing.T) {
	var calls int
	e := newEnv(t, func(e *env) {
		var mu sync.Mutex
		e.secrets = pairing.NewSecrets(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls >= 3 { // Issue, Check, then Consume finds it expired
				return e.clock.Now().Add(11 * time.Minute)
			}
			return e.clock.Now()
		})
	})
	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("todo", secret, "Phone"))
	expectClosedWithError(t, fc, protocol.CodeBadSecret)
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0", n)
	}
	expectSessions(t, e.hub)

	// The app's lock was released: a hello for the app goes through.
	id, token := e.device(t, "todo")
	e.login(t, "192.168.1.10:5000", "todo", id, token)
}

func TestPairWhenDevicesCannotBeSaved(t *testing.T) {
	e := newEnv(t)
	// A directory where devices.json goes: writing it fails.
	if err := os.MkdirAll(filepath.Join(e.dir, "devices.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("todo", secret, "Phone"))
	expectClosedWithError(t, fc, protocol.CodeInternal)
	expectNoFileInFrames(t, e, fc)
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0", n)
	}
	expectSessions(t, e.hub)
	if !strings.Contains(e.logs.String(), "devices.json") {
		t.Errorf("log %q, want the error naming the file", e.logs)
	}
	// The secret was used before the write; the user issues a new code.
	if e.secrets.Check(secret) {
		t.Error("the secret is still valid")
	}
}

func TestPairWhenTheWelcomeFails(t *testing.T) {
	e := newEnv(t)
	e.breakDocument(t, "todo")
	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("todo", secret, "Phone"))
	expectClosedWithError(t, fc, protocol.CodeInternal)
	expectNoFileInFrames(t, e, fc)
	if n := len(framesOf(fc, "paired")); n != 0 {
		t.Errorf("%d paired frames sent, want none", n)
	}
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0: the token never went out", n)
	}
	expectSessions(t, e.hub)
}

func TestHelloWhenTheWelcomeFails(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	repair := e.breakDocument(t, "todo")
	e.clock.Advance(time.Hour)
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(helloFrame("todo", id, token))
	expectClosedWithError(t, fc, protocol.CodeInternal)
	expectNoFileInFrames(t, e, fc)
	expectSessions(t, e.hub)
	ds := e.devices.List()
	if len(ds) != 1 || ds[0].ID != id {
		t.Fatalf("devices %v, want %s kept", ds, id)
	}
	if !ds[0].LastSeen.Equal(e.clock.Now()) {
		t.Errorf("LastSeen %v, want %v", ds[0].LastSeen, e.clock.Now())
	}

	// A server failure is not the device's: it does not count toward a block.
	for i := range 9 {
		s, fc := e.connect(t, fmt.Sprintf("192.168.1.9:%d", 5001+i))
		s.HandleText(helloFrame("todo", id, "wrong"))
		expectClosedWithError(t, fc, protocol.CodeBadToken)
	}
	repair()
	e.login(t, "192.168.1.9:6000", "todo", id, token)
}

func TestGetAndPutInternalErrors(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	idB, tokenB := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	other, fo := e.login(t, "192.168.1.9:5001", "todo", idB, tokenB)
	repair := e.breakDocument(t, "todo")

	s.HandleText(`{"t":"get"}`)
	expectError(t, waitFrames(t, fc, "error", 1)[0], protocol.CodeInternal)
	put(s, nil, []byte(`{"a":1}`))
	expectError(t, waitFrames(t, fc, "error", 2)[1], protocol.CodeInternal)
	expectNoFileInFrames(t, e, fc)
	if fc.isClosed() {
		t.Fatal("an internal error closed the channel")
	}

	// The failed put told nobody: the next stored put is the only change.
	repair()
	body := []byte(`{"a":2}`)
	put(s, nil, body)
	waitFrames(t, fc, "stored", 1)
	frames := settle(t, other, fo)
	if changed := framesOf(&fakeChannel{texts: frames}, "changed"); len(changed) != 1 || changed[0]["etag"] != store.Etag(body) {
		t.Errorf("the other session got %v, want one changed for the stored put", changed)
	}
}

func TestNoRefusalRecordedWhileBlocked(t *testing.T) {
	e := newEnv(t)
	id, _ := e.device(t, "todo")
	for i := range 10 {
		s, fc := e.connect(t, fmt.Sprintf("192.168.1.9:%d", 5000+i))
		s.HandleText(helloFrame("todo", id, "wrong"))
		expectClosedWithError(t, fc, protocol.CodeBadToken)
	}
	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:6000")
	s.HandleText(pairFrame("notes", secret, "Phone"))
	expectClosedWithError(t, fc, protocol.CodeBlocked)
	if r := e.refusals.Recent(); len(r) != 0 {
		t.Errorf("refusals %+v, want none from a blocked IP", r)
	}
	if !e.secrets.Check(secret) {
		t.Error("the secret was used")
	}
}

// A pairing whose client never confirms it (no frame after paired) is undone
// when the session ends; a confirmed one stays.
func TestUnconfirmedPairingIsUndone(t *testing.T) {
	e := newEnv(t)
	pairOnce := func(remote string) (*protocol.Session, *fakeChannel) {
		secret, _ := e.secrets.Issue()
		s, fc := e.connect(t, remote)
		s.HandleText(pairFrame("todo", secret, "Phone"))
		waitFrames(t, fc, "welcome", 1)
		return s, fc
	}

	s, _ := pairOnce("192.168.1.9:5000")
	if n := len(e.devices.List()); n != 1 {
		t.Fatalf("%d devices while the session is open, want 1", n)
	}
	s.Close()
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices after an unconfirmed pairing ended, want 0", n)
	}

	s, fc := pairOnce("192.168.1.9:5001")
	settle(t, s, fc) // the client's head confirms it
	s.Close()
	if n := len(e.devices.List()); n != 1 {
		t.Errorf("%d devices after a confirmed pairing ended, want 1", n)
	}
}

// The session closes while Pair is between registering it and answering:
// the device entry, whose token never goes out, is undone.
func TestSessionClosedDuringPair(t *testing.T) {
	e := newEnv(t)
	secret, _ := e.secrets.Issue()
	s, fc := e.connect(t, "192.168.1.9:5000")
	e.setOnName(func() { s.Close() })
	s.HandleText(pairFrame("todo", secret, "Phone"))
	if n := len(fc.frames()); n != 0 {
		t.Errorf("sent %v, want nothing", fc.frames())
	}
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0", n)
	}
	expectSessions(t, e.hub)
}

// The device is removed while its hello is between admission and the
// welcome: the session is closed and gets no welcome.
func TestRemoveDeviceInsideHello(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.connect(t, "192.168.1.9:5000")
	e.setOnName(func() { e.hub.RemoveDevice(id) })
	s.HandleText(helloFrame("todo", id, token))
	waitFor(t, "the session to close", fc.isClosed)
	if w := framesOf(fc, "welcome"); len(w) != 0 {
		t.Errorf("sent a welcome to a removed device: %v", fc.frames())
	}
	if n := len(e.devices.List()); n != 0 {
		t.Errorf("%d devices, want 0", n)
	}
	expectSessions(t, e.hub)
}

// A hello racing the removal of its device is either refused or admitted and
// then closed by the removal; it never stays open with a deleted token. This
// is a smoke test of the race; TestRemoveDeviceInsideHello pins the order.
func TestRemoveDeviceDuringHello(t *testing.T) {
	e := newEnv(t)
	for i := range 30 {
		id, token := e.device(t, "todo")
		s, fc := e.connect(t, fmt.Sprintf("10.0.0.%d:5000", i+1)) // one IP each: no rate limit
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-start; s.HandleText(helloFrame("todo", id, token)) })
		wg.Go(func() { <-start; e.hub.RemoveDevice(id) })
		close(start)
		wg.Wait()
		waitFor(t, fmt.Sprintf("round %d: the session to close", i), fc.isClosed)
		expectSessions(t, e.hub)
	}
}

// A pairing racing the removal of its app either is refused with unknownApp
// or pairs and is then removed with the app; no device of the removed app is
// left behind and no session stays registered.
func TestRemoveAppDuringPair(t *testing.T) {
	e := newEnv(t)
	for i := range 30 {
		e.addApp(t, apps.App{ID: "notes"})
		secret, _ := e.secrets.Issue()
		s, fc := e.connect(t, fmt.Sprintf("10.0.0.%d:5000", i+1)) // one IP each: no rate limit
		start := make(chan struct{})
		var wg sync.WaitGroup
		var removeErr error
		wg.Go(func() { <-start; s.HandleText(pairFrame("notes", secret, "Phone")) })
		wg.Go(func() { <-start; removeErr = e.hub.RemoveApp("notes") })
		close(start)
		wg.Wait()
		if removeErr != nil {
			t.Fatalf("round %d: RemoveApp: %v", i, removeErr)
		}
		waitFor(t, fmt.Sprintf("round %d: the session to close or be refused", i), func() bool {
			return fc.isClosed() || len(framesOf(fc, "error")) > 0
		})
		if errs := framesOf(fc, "error"); len(errs) > 0 {
			expectError(t, errs[0], protocol.CodeUnknownApp)
		}
		if ids := e.deviceIDs(); len(ids) != 0 {
			t.Fatalf("round %d: devices %v left after the app was removed", i, ids)
		}
		expectSessions(t, e.hub)
	}
}

func TestSessionsSorted(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	idT, tokT := e.device(t, "todo")
	idN, tokN := e.device(t, "notes")
	e.login(t, "192.168.1.9:5001", "todo", idT, tokT)
	e.login(t, "192.168.1.9:5000", "todo", idT, tokT)
	e.login(t, "192.168.1.9:5002", "notes", idN, tokN)
	expectSessions(t, e.hub,
		SessionInfo{App: "notes", Device: idN, Remote: "192.168.1.9:5002"},
		SessionInfo{App: "todo", Device: idT, Remote: "192.168.1.9:5000"},
		SessionInfo{App: "todo", Device: idT, Remote: "192.168.1.9:5001"},
	)
}
