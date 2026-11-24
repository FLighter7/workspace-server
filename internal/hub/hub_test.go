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
	"sync/atomic"
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

	// deps, set by a tweak, changes the hub's Deps before the hub is made.
	deps func(*Deps)

	hookMu   sync.Mutex
	onName   func()           // runs inside every welcome, after the session is registered
	onChange func(app string) // runs in every Deps.Changed
	heard    []string         // every Deps.Changed, "app revision"
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
		dir:     dir,
		logs:    &logLines{},
		clock:   clk,
		apps:    ar,
		devices: dr,
		store: store.New(filepath.Join(dir, "apps"), clk.Now, func(app string) int64 {
			a, _ := ar.Get(app)
			return a.Quota
		}),
		secrets:  pairing.NewSecrets(clk.Now),
		refusals: pairing.NewRefusals(clk.Now),
	}
	for _, tweak := range tweaks {
		tweak(e)
	}
	d := Deps{
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
		Changed:  e.changed,
	}
	if e.deps != nil {
		e.deps(&d)
	}
	e.hub = New(d)
	return e
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
		rev := e.writeDoc(t, "todo", "data.json", `{"items":[]}`)
		e.clock.Advance(time.Hour)

		s, fc := e.connect(t, "192.168.1.9:5000")
		expectSessions(t, e.hub) // connected but not authenticated
		s.HandleText(helloFrame("todo", id, token))
		w := waitFrames(t, fc, "welcome", 1)[0]
		want := obj{
			"t": "welcome", "proto": float64(1), "role": "main", "serverId": "srv-1", "name": "Desk",
			"revision": rev, "hosts": []any{"u.local", "192.168.1.5"}, "subordinates": []any{},
		}
		if !reflect.DeepEqual(w, want) {
			t.Errorf("welcome %v, want %v", w, want)
		}
		expectSessions(t, e.hub, SessionInfo{App: "todo", Device: id, Remote: "192.168.1.9:5000"})
		if seen := e.devices.List()[0].LastSeen; !seen.Equal(e.clock.Now()) {
			t.Errorf("LastSeen %v, want %v (Touch on hello)", seen, e.clock.Now())
		}
	})

	t.Run("no documents", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		_, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
		if w := framesOf(fc, "welcome")[0]; w["revision"] != nil {
			t.Errorf("welcome revision %v, want null", w["revision"])
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
	if w := frames[1]; w["serverId"] != "srv-1" || w["revision"] != nil {
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

// A code issued for a machine puts the device that pairs with it on that
// machine: another app on a phone the user already has.
func TestPairJoinsTheMachineOfTheCode(t *testing.T) {
	e := newEnv(t)
	first, _, err := e.devices.Pair("notes", "iPhone Safari")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := e.secrets.IssueFor(first.Machine)
	s, fc := e.connect(t, "192.168.1.9:5000")
	s.HandleText(pairFrame("todo", secret, "iPhone Safari (PWA)"))
	waitFrames(t, fc, "welcome", 1)
	device, _ := fc.frames()[0]["device"].(string)
	if d, _ := e.devices.Get(device); d.Machine != first.Machine {
		t.Errorf("the device is on machine %q, want the code's %q", d.Machine, first.Machine)
	}
	if n := len(e.devices.Machines()); n != 1 {
		t.Errorf("%d machines, want the one both devices share", n)
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

func TestRemoveAppClosesSessionsAndDeletesData(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	id, token := e.device(t, "todo")
	idN, tokN := e.device(t, "notes")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	n, fn := e.login(t, "192.168.1.9:5001", "notes", idN, tokN)
	write(s, "data.json", nil, []byte(`{"x":1}`))
	waitFrames(t, fc, "committed", 1)

	if err := e.hub.RemoveApp("todo"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the todo session to close", fc.isClosed)
	if rev := e.revisionOf(t, "todo"); rev != nil {
		t.Errorf("revision of todo %v, want no documents", rev)
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

// An internal error reaches the device without its text, which can name
// files on the server's computer; the server's log gets the text.
func TestInternalErrorKeepsTheDetailsInTheLog(t *testing.T) {
	e := newEnv(t)
	e.breakDocument(t, "todo")
	_, perr := e.hub.Head("todo")
	if perr == nil || perr.Code != protocol.CodeInternal {
		t.Fatalf("Head = %v, want error %q", perr, protocol.CodeInternal)
	}
	if perr.Message == "" || strings.Contains(perr.Message, "journal") || strings.Contains(perr.Message, e.dir) {
		t.Errorf("message %q, want one that names no file", perr.Message)
	}
	if !strings.Contains(e.logs.String(), filepath.Join("todo", ".wss", "journal.json")) {
		t.Errorf("log %q, want the error naming the file", e.logs)
	}
}

func TestCloseAppKeepsDevicesAndData(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	write(s, "data.json", nil, []byte(`{}`))
	waitFrames(t, fc, "committed", 1)

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
	if e.etagOf(t, "todo", "data.json") == nil {
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

func TestClosedSessionWasSeenUntilNow(t *testing.T) {
	lastSeen := func(e *env, id string) time.Time {
		t.Helper()
		for _, d := range e.devices.List() {
			if d.ID == id {
				return d.LastSeen
			}
		}
		t.Fatalf("no device %s", id)
		return time.Time{}
	}

	t.Run("a device of an app", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		s, _ := e.login(t, "192.168.1.9:5000", "todo", id, token)
		e.clock.Advance(3 * time.Hour)
		s.Close()
		want := e.clock.Now()
		if seen := lastSeen(e, id); !seen.Equal(want) {
			t.Errorf("LastSeen %v, want %v, when the session closed", seen, want)
		}
		// It is on disk too, for a server that starts again.
		again, err := devices.Open(filepath.Join(e.dir, "devices.json"), e.clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		if seen := again.List()[0].LastSeen; !seen.Equal(want) {
			t.Errorf("LastSeen on disk %v, want %v", seen, want)
		}
		// A session closed already says nothing more of the device.
		e.clock.Advance(time.Hour)
		e.hub.Closed(s)
		if seen := lastSeen(e, id); !seen.Equal(want) {
			t.Errorf("LastSeen %v after a second close, want %v", seen, want)
		}
	})

	t.Run("a session that never authenticated", func(t *testing.T) {
		e := newEnv(t)
		id, _ := e.device(t, "todo")
		want := lastSeen(e, id)
		e.clock.Advance(time.Hour)
		s, _ := e.connect(t, "192.168.1.9:5000")
		s.HandleText(helloFrame("todo", id, "wrong"))
		s.Close()
		if seen := lastSeen(e, id); !seen.Equal(want) {
			t.Errorf("LastSeen %v, want %v: a wrong token is not the device", seen, want)
		}
	})

	t.Run("a removed device", func(t *testing.T) {
		e := newEnv(t)
		id, token := e.device(t, "todo")
		_, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
		e.clock.Advance(time.Hour)
		e.hub.RemoveDevice(id)
		waitFor(t, "the removed device's session to close", fc.isClosed)
		if ids := e.deviceIDs(); len(ids) != 0 {
			t.Errorf("devices %v, want none", ids)
		}
	})
}

// breakDocument puts a directory where a commit leaves app's journal, so that
// every read and write of it fails, and returns the function that repairs it.
func (e *env) breakDocument(t *testing.T, app string) func() {
	t.Helper()
	// A folder where a commit's journal goes: the store cannot read it, and
	// fails every operation on the app until it is gone.
	path := filepath.Join(e.dir, "apps", app, ".wss", "journal.json")
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

// changesHeard returns what Deps.Changed was told so far, as "app rev"
// ("app null" for an empty set).
func (e *env) changesHeard() []string {
	e.hookMu.Lock()
	defer e.hookMu.Unlock()
	return append([]string(nil), e.heard...)
}

// changed is Deps.Changed: it records the call and runs onChange.
func (e *env) changed(app string, rev *string) {
	e.hookMu.Lock()
	r := "null"
	if rev != nil {
		r = *rev
	}
	e.heard = append(e.heard, app+" "+r)
	f := e.onChange
	e.hookMu.Unlock()
	if f != nil {
		f(app)
	}
}

// keyOf reads "path/id.type" or "id.type" into a key.
func keyOf(ks string) store.Key {
	k, ok := store.ParseKey(ks)
	if !ok {
		panic("bad key " + ks)
	}
	return k
}

var txSeq atomic.Int64

// newTx returns a transaction id never used before.
func newTx() string { return fmt.Sprintf("tx-%016d", txSeq.Add(1)) }

// write sends one transaction that puts body as the document ks, expecting
// it to have ifEtag: begin, put and commit. Its answers are began, staged,
// and committed or conflict (or errors).
func write(s *protocol.Session, ks string, ifEtag *string, body []byte) {
	k := keyOf(ks)
	tx := newTx()
	s.HandleText(frame(obj{"t": "begin", "tx": tx}))
	s.HandleText(frame(obj{
		"t": "put", "tx": tx, "path": k.Path, "id": k.ID, "type": k.Type,
		"ifEtag": ifEtag, "bytes": len(body), "offset": 0,
	}))
	for off := 0; off < len(body); off += protocol.ChunkBytes {
		s.HandleBinary(body[off:min(off+protocol.ChunkBytes, len(body))])
	}
	s.HandleText(frame(obj{"t": "commit", "tx": tx}))
}

// writeDoc writes body as the document ks of app straight into the store,
// and returns the set's revision after it.
//
// The hub announces the write as it does any move of the store's (sessions
// get changed, Deps.Changed hears it); writeDoc waits for that, and leaves
// it out of what Deps.Changed heard, so that a test counts only its own.
func (e *env) writeDoc(t *testing.T, app, ks, body string) string {
	t.Helper()
	prev := e.revisionOf(t, app)
	tx := newTx()
	if _, err := e.store.Begin(app, "test-writer", tx); err != nil {
		t.Fatal(err)
	}
	sink, err := e.store.Put(app, "test-writer", tx, keyOf(ks), e.etagOf(t, app, ks), int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	_ = sink.Close()
	c, err := e.store.Commit(app, "test-writer", tx)
	if err != nil {
		t.Fatal(err)
	}
	if prev != *c.Revision {
		heard := app + " " + *c.Revision
		waitFor(t, "the write's announcement", func() bool { return slices.Contains(e.changesHeard(), heard) })
		e.hookMu.Lock()
		e.heard = slices.DeleteFunc(e.heard, func(h string) bool { return h == heard })
		e.hookMu.Unlock()
	}
	return *c.Revision
}

// etagOf returns the etag of the document ks of app, nil when there is
// none.
func (e *env) etagOf(t *testing.T, app, ks string) *string {
	t.Helper()
	l, err := e.store.List(app)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range l.Docs {
		if d.Key.String() == ks {
			return &d.Etag
		}
	}
	return nil
}

// revisionOf returns app's revision, "null" for an empty set.
func (e *env) revisionOf(t *testing.T, app string) any {
	t.Helper()
	rev, err := e.store.Head(app)
	if err != nil {
		t.Fatal(err)
	}
	if rev == nil {
		return nil
	}
	return *rev
}

// committedEtag returns the etag a committed or conflict frame names for its
// first document.
func committedEtag(f obj) any {
	docs, _ := f["docs"].([]any)
	if len(docs) == 0 {
		return "none"
	}
	return docs[0].(obj)["etag"]
}

// Every hello reconciles the app's set with its files: a file changed by
// hand is in the welcome's revision, and the app's other sessions and
// Deps.Changed hear of it.
func TestHelloReconciles(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	rev := e.writeDoc(t, "todo", "data.json", `{}`)
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	if w := framesOf(fa, "welcome")[0]; w["revision"] != rev {
		t.Fatalf("welcome %v, want revision %s", w, rev)
	}
	if got := e.changesHeard(); len(got) != 0 {
		t.Fatalf("Changed heard %v with nothing changed", got)
	}

	path := filepath.Join(e.dir, "apps", "todo", "notes", "hand.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	now := e.revisionOf(t, "todo")
	if now == rev {
		t.Fatal("the hand-made file did not change the revision")
	}
	if w := framesOf(fb, "welcome")[0]; w["revision"] != now {
		t.Errorf("welcome %v, want revision %v", w, now)
	}
	if c := waitFrames(t, fa, "changed", 1); c[0]["revision"] != now {
		t.Errorf("the other session got %v, want changed %v", c, now)
	}
	if got := e.changesHeard(); !slices.Equal(got, []string{"todo " + now.(string)}) {
		t.Errorf("Changed heard %v", got)
	}
	if n := countType(settle(t, a, fa), "changed"); n != 1 {
		t.Errorf("%d changed frames, want 1", n)
	}
}

func TestDocumentReads(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)

	s.HandleText(`{"t":"head"}`)
	s.HandleText(`{"t":"list"}`)
	s.HandleText(`{"t":"get","path":"","id":"data","type":"json"}`)
	got := waitFrames(t, fc, "head", 3, "list", "stale")
	want := []obj{
		{"t": "head", "revision": nil},
		{"t": "list", "revision": nil, "docs": []any{}, "removed": []any{}},
		{"t": "stale", "revision": nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}

	doc := `{"items":["milk"]}`
	e.writeDoc(t, "todo", "data.json", doc)
	rev := e.writeDoc(t, "todo", "photos/p1.jpg", "jpeg")
	etag := store.Etag([]byte(doc))
	s.HandleText(`{"t":"head"}`)
	s.HandleText(`{"t":"list"}`)
	s.HandleText(frame(obj{"t": "get", "path": "", "id": "data", "type": "json", "etag": etag, "offset": 3}))
	got = waitFrames(t, fc, "head", 6, "list", "stale", "doc")[3:]
	want = []obj{
		{"t": "head", "revision": rev},
		{"t": "list", "revision": rev, "docs": []any{
			obj{"path": "", "id": "data", "type": "json", "etag": etag, "bytes": float64(len(doc))},
			obj{"path": "photos", "id": "p1", "type": "jpg", "etag": store.Etag([]byte("jpeg")), "bytes": float64(4)},
		}, "removed": []any{}},
		{"t": "doc", "path": "", "id": "data", "type": "json", "etag": etag, "bytes": float64(len(doc)), "offset": float64(3)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	waitFor(t, "the document body", func() bool { return len(fc.body()) >= len(doc)-3 })
	if string(fc.body()) != doc[3:] {
		t.Errorf("body %q, want %q", fc.body(), doc[3:])
	}

	// At an etag the document no longer has: stale, with the revision now.
	s.HandleText(frame(obj{"t": "get", "path": "", "id": "data", "type": "json", "etag": store.Etag([]byte("old"))}))
	if f := waitFrames(t, fc, "stale", 2)[1]; f["revision"] != rev {
		t.Errorf("stale %v, want revision %s", f, rev)
	}
	// A key that breaks the rules.
	s.HandleText(`{"t":"get","path":"","id":"Data","type":"json"}`)
	expectError(t, waitFrames(t, fc, "error", 1)[0], protocol.CodeBadPath)
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

func TestCommitBroadcast(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "notes"})
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	idC, tokC := e.device(t, "notes")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	_, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)
	c, fcC := e.login(t, "192.168.1.9:5002", "notes", idC, tokC)

	doc := []byte(`{"items":[]}`)
	write(a, "data.json", nil, doc)
	etag := store.Etag(doc)
	f := waitFrames(t, fa, "committed", 1)[0]
	rev := e.revisionOf(t, "todo")
	if f["revision"] != rev || committedEtag(f) != etag {
		t.Errorf("committed %v, want revision %v and etag %s", f, rev, etag)
	}
	if f := waitFrames(t, fb, "changed", 1)[0]; f["revision"] != rev {
		t.Errorf("B got %v, want changed %v", f, rev)
	}
	if n := countType(settle(t, a, fa), "changed"); n != 0 {
		t.Errorf("the writer got %d changed frames, want 0", n)
	}
	if n := countType(settle(t, c, fcC), "changed"); n != 0 {
		t.Errorf("a session of another app got %d changed frames, want 0", n)
	}
	if got := e.changesHeard(); !slices.Equal(got, []string{"todo " + rev.(string)}) {
		t.Errorf("Changed heard %v", got)
	}

	// A commit that changes nothing (the same bytes again) tells nobody.
	write(a, "data.json", &etag, doc)
	waitFrames(t, fa, "committed", 2)
	if n := countType(settle(t, a, fa), "changed"); n != 0 {
		t.Errorf("the writer got %d changed frames", n)
	}
	if got := e.changesHeard(); len(got) != 1 {
		t.Errorf("Changed heard %v after a commit that changed nothing", got)
	}
}

// A conflict leaves the transaction open: the device puts again with the
// etag the conflict named, and commits the same transaction.
func TestCommitConflictLeavesTheTransactionOpen(t *testing.T) {
	e := newEnv(t)
	idA, tokA := e.device(t, "todo")
	idB, tokB := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", idA, tokA)
	b, fb := e.login(t, "192.168.1.9:5001", "todo", idB, tokB)

	write(b, "data.json", nil, []byte(`{"v":1}`))
	waitFrames(t, fb, "committed", 1)
	etag := store.Etag([]byte(`{"v":1}`))

	tx := newTx()
	a.HandleText(frame(obj{"t": "begin", "tx": tx}))
	a.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "data", "type": "json", "ifEtag": nil, "bytes": 7, "offset": 0}))
	a.HandleBinary([]byte(`{"v":2}`))
	a.HandleText(frame(obj{"t": "commit", "tx": tx}))
	f := waitFrames(t, fa, "conflict", 1)[0]
	if f["revision"] != e.revisionOf(t, "todo") || committedEtag(f) != etag {
		t.Errorf("conflict %v, want the current etag %s", f, etag)
	}
	// Only the etag changes: no body follows.
	a.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "data", "type": "json", "ifEtag": etag, "bytes": 7, "offset": 7}))
	a.HandleText(frame(obj{"t": "commit", "tx": tx}))
	f = waitFrames(t, fa, "committed", 1)[0]
	if committedEtag(f) != store.Etag([]byte(`{"v":2}`)) {
		t.Errorf("committed %v", f)
	}
	if n := countType(settle(t, b, fb), "changed"); n != 1 {
		t.Errorf("B got %d changed frames, want 1 (the conflict told nobody)", n)
	}
}

// A body cut off by the session's end resumes in a new session from what
// arrived, which began reports.
func TestPutResumesInANewSession(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	body := bytes.Repeat([]byte("0123456789"), 5000) // 50000 bytes
	tx := newTx()
	s.HandleText(frame(obj{"t": "begin", "tx": tx}))
	s.HandleText(frame(obj{"t": "put", "tx": tx, "path": "big", "id": "b", "type": "bin", "ifEtag": nil, "bytes": len(body), "offset": 0}))
	s.HandleBinary(body[:16384])
	s.HandleBinary(body[16384:30000])
	waitFrames(t, fc, "began", 1)
	s.Close()

	s2, fc2 := e.login(t, "192.168.1.9:5001", "todo", id, token)
	s2.HandleText(frame(obj{"t": "begin", "tx": tx}))
	began := waitFrames(t, fc2, "began", 1)[0]
	staged := began["staged"].([]any)
	if len(staged) != 1 || staged[0].(obj)["received"] != float64(30000) || staged[0].(obj)["bytes"] != float64(len(body)) {
		t.Fatalf("began %v, want the body at 30000 bytes", began)
	}
	s2.HandleText(frame(obj{"t": "put", "tx": tx, "path": "big", "id": "b", "type": "bin", "ifEtag": nil, "bytes": len(body), "offset": 30000}))
	s2.HandleBinary(body[30000:])
	s2.HandleText(frame(obj{"t": "commit", "tx": tx}))
	if f := waitFrames(t, fc2, "committed", 1)[0]; committedEtag(f) != store.Etag(body) {
		t.Errorf("committed %v, want the whole body's etag", f)
	}
	if st := framesOf(fc2, "staged"); len(st) != 1 || st[0]["received"] != float64(len(body)) {
		t.Errorf("staged %v", st)
	}
}

// The store's refusals reach the device as their codes, and keep the session
// open; a bad transaction id closes it.
func TestTransactionRefusals(t *testing.T) {
	e := newEnv(t)
	e.addApp(t, apps.App{ID: "tiny", Quota: apps.MinQuota})
	id, token := e.device(t, "tiny")
	s, fc := e.login(t, "192.168.1.9:5000", "tiny", id, token)

	// Past the quota: refused at the put, before any byte is staged; the
	// body is read and dropped.
	write(s, "big.bin", nil, bytes.Repeat([]byte("x"), int(apps.MinQuota)+1))
	errs := waitFrames(t, fc, "error", 1)
	expectError(t, errs[0], protocol.CodeQuota)
	if f := waitFrames(t, fc, "committed", 1)[0]; f["revision"] != nil {
		t.Errorf("committed %v, want an empty transaction", f) // the put was refused
	}

	tx := newTx()
	s.HandleText(frame(obj{"t": "begin", "tx": tx}))
	// A key that breaks the rules.
	s.HandleText(frame(obj{"t": "delete", "tx": tx, "path": "", "id": "CON", "type": "md", "ifEtag": nil}))
	expectError(t, waitFrames(t, fc, "error", 2)[1], protocol.CodeBadPath)
	// A body that has not arrived.
	s.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "a", "type": "md", "ifEtag": nil, "bytes": 10, "offset": 0}))
	s.HandleBinary([]byte("12345"))
	s.Close()
	s, fc = e.login(t, "192.168.1.9:5001", "tiny", id, token)
	s.HandleText(frame(obj{"t": "commit", "tx": tx}))
	expectError(t, waitFrames(t, fc, "error", 1)[0], protocol.CodeIncomplete)
	// Another device's transaction, or none.
	s.HandleText(frame(obj{"t": "commit", "tx": newTx()}))
	expectError(t, waitFrames(t, fc, "error", 2)[1], protocol.CodeNoTransaction)
	// An abort of it is idempotent.
	s.HandleText(frame(obj{"t": "abort", "tx": tx}))
	s.HandleText(frame(obj{"t": "abort", "tx": tx}))
	waitFrames(t, fc, "aborted", 2)
	// Resolve finds nothing to settle until divergence exists.
	s.HandleText(`{"t":"resolve","server":"Lp7tOp0123ab","keep":"main"}`)
	if f := waitFrames(t, fc, "resolved", 1)[0]; f["server"] != "Lp7tOp0123ab" {
		t.Errorf("resolved %v", f)
	}
	if fc.isClosed() {
		t.Fatal("a refusal closed the channel")
	}
	s.HandleText(`{"t":"begin","tx":"short"}`)
	expectClosedWithError(t, fc, protocol.CodeProtocol)
}

// Two devices committing at the same instant with the same ifEtag: exactly
// one commits, the other gets conflict with the winner's etag.
func TestConcurrentCommitsWithSameEtag(t *testing.T) {
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
				write(s, "data.json", current, body)
			})
		}
		close(start)
		wg.Wait()

		ra := waitFrames(t, fa, "committed", round+1, "conflict")[round]
		rb := waitFrames(t, fb, "committed", round+1, "conflict")[round]
		var winner, loser obj
		switch {
		case ra["t"] == "committed" && rb["t"] == "conflict":
			winner, loser = ra, rb
		case rb["t"] == "committed" && ra["t"] == "conflict":
			winner, loser = rb, ra
		default:
			t.Fatalf("round %d: answers %v and %v, want one committed and one conflict", round, ra, rb)
		}
		etag, _ := committedEtag(winner).(string)
		if committedEtag(loser) != etag {
			t.Fatalf("round %d: conflict %v, want the winner's etag %s", round, loser, etag)
		}
		if cur := e.etagOf(t, "todo", "data.json"); cur == nil || *cur != etag {
			t.Fatalf("round %d: stored etag %v, want the winner's %s", round, cur, etag)
		}
		current = &etag
	}
}

// A commit that reaches the hub after its device was removed, as one in
// flight when the device is removed does, commits nothing.
func TestCommitFromARemovedDevice(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	tx := newTx()
	s.HandleText(frame(obj{"t": "begin", "tx": tx}))
	s.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "data", "type": "json", "ifEtag": nil, "bytes": 2, "offset": 0}))
	s.HandleBinary([]byte("{}"))
	waitFrames(t, fc, "staged", 1)

	if !e.hub.RemoveDevice(id) {
		t.Fatal("RemoveDevice = false")
	}
	if perr := e.hub.Commit(s, "todo", id, tx); perr == nil || perr.Code != protocol.CodeBadToken {
		t.Errorf("Commit after RemoveDevice = %v; want error %q", perr, protocol.CodeBadToken)
	}
	// Its transaction is discarded with it, staging and all.
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "apps", "todo", ".wss", "staging")); len(entries) != 0 {
		t.Errorf("staging holds %d entries after the device's removal", len(entries))
	}
	if rev := e.revisionOf(t, "todo"); rev != nil {
		t.Errorf("revision %v, want nothing committed", rev)
	}
	if _, perr := e.hub.Begin("todo", id, newTx()); perr == nil || perr.Code != protocol.CodeBadToken {
		t.Errorf("Begin after RemoveDevice = %v, want %q", perr, protocol.CodeBadToken)
	}
}

func TestGetAndCommitInternalErrors(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	idB, tokenB := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	other, fo := e.login(t, "192.168.1.9:5001", "todo", idB, tokenB)
	repair := e.breakDocument(t, "todo")

	s.HandleText(`{"t":"get","path":"","id":"data","type":"json"}`)
	expectError(t, waitFrames(t, fc, "error", 1)[0], protocol.CodeInternal)
	write(s, "data.json", nil, []byte(`{"a":1}`)) // begin, put and commit fail
	for _, f := range waitFrames(t, fc, "error", 4)[1:] {
		expectError(t, f, protocol.CodeInternal)
	}
	expectNoFileInFrames(t, e, fc)
	if fc.isClosed() {
		t.Fatal("an internal error closed the channel")
	}

	// The failures told nobody: the next commit is the only change.
	repair()
	write(s, "data.json", nil, []byte(`{"a":2}`))
	waitFrames(t, fc, "committed", 1)
	frames := settle(t, other, fo)
	if changed := framesOf(&fakeChannel{texts: frames}, "changed"); len(changed) != 1 || changed[0]["revision"] != e.revisionOf(t, "todo") {
		t.Errorf("the other session got %v, want one changed for the commit", changed)
	}
}

// A commit that fails before the store's commit point leaves its
// transaction open: nothing was applied, and the device, which lists again,
// finds what it staged with a begin of the same id, and commits it.
func TestCommitFailureBeforeTheCommitPoint(t *testing.T) {
	e := newEnv(t)
	id, token := e.device(t, "todo")
	s, fc := e.login(t, "192.168.1.9:5000", "todo", id, token)
	tx := newTx()
	s.HandleText(frame(obj{"t": "begin", "tx": tx}))
	s.HandleText(frame(obj{"t": "put", "tx": tx, "path": "", "id": "a", "type": "md", "ifEtag": nil, "bytes": 1, "offset": 0}))
	s.HandleBinary([]byte("a"))
	waitFrames(t, fc, "staged", 1)

	repair := e.breakDocument(t, "todo")
	s.HandleText(frame(obj{"t": "commit", "tx": tx}))
	expectError(t, waitFrames(t, fc, "error", 1)[0], protocol.CodeInternal)
	repair()
	s.HandleText(frame(obj{"t": "begin", "tx": tx}))
	if staged := waitFrames(t, fc, "began", 2)[1]["staged"].([]any); len(staged) != 1 {
		t.Fatalf("began %v, want the body still staged", staged)
	}
	s.HandleText(frame(obj{"t": "commit", "tx": tx}))
	if f := waitFrames(t, fc, "committed", 1)[0]; committedEtag(f) != store.Etag([]byte("a")) {
		t.Errorf("committed %v", f)
	}
}

// Deps.Changed runs once the hub's locks are released: the app's lock and
// the servers lock are both free inside it.
func TestChangedRunsOutsideTheLocks(t *testing.T) {
	e := newEnv(t)
	var locked atomic.Bool
	e.hookMu.Lock()
	e.onChange = func(app string) {
		free := make(chan struct{})
		go func() {
			l := e.hub.appLock(app)
			l.Lock()
			l.Unlock()
			e.hub.servers.Lock()
			e.hub.servers.Unlock()
			close(free)
		}()
		select {
		case <-free:
		case <-time.After(time.Second):
			locked.Store(true)
		}
	}
	e.hookMu.Unlock()

	id, tok := e.device(t, "todo")
	a, fa := e.login(t, "192.168.1.9:5000", "todo", id, tok)
	write(a, "data.json", nil, []byte(`{"v":1}`))
	waitFrames(t, fa, "committed", 1)
	tx, _, err := e.hub.BeginLink("todo", newTx())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Delete(keyOf("data.json"), ptr(store.Etag([]byte(`{"v":1}`)))); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := e.changesHeard(); len(got) != 2 {
		t.Fatalf("Changed heard %v, want the device's commit and the link's", got)
	}
	if locked.Load() {
		t.Error("Changed ran under the app's lock or the servers lock")
	}
}
