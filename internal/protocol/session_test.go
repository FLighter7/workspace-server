package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// obj is a decoded text frame; JSON numbers decode as float64.
type obj = map[string]any

const (
	helloFrame = `{"t":"hello","proto":1,"app":"todo","device":"d1","token":"x"}`
	pairFrame  = `{"t":"pair","proto":1,"app":"todo","secret":"sec","name":"Phone"}`
)

func newSession(t *testing.T, b *fakeBackend, opt Options) (*Session, *fakeChannel) {
	t.Helper()
	fc := &fakeChannel{}
	s := NewSession(fc, "192.168.1.9:5000", b, opt)
	t.Cleanup(s.Close)
	return s, fc
}

// authenticate sends hello and waits for the welcome.
func authenticate(t *testing.T, s *Session, fc *fakeChannel) {
	t.Helper()
	s.HandleText(helloFrame)
	waitFor(t, "the welcome", func() bool {
		for _, f := range fc.texts() {
			if f["t"] == "welcome" {
				return true
			}
		}
		return false
	})
}

var barrierSeq atomic.Int64

func isBarrier(m message) bool {
	etag, _ := m.text["etag"].(string)
	return m.text["t"] == "changed" && strings.HasPrefix(etag, "barrier-")
}

// barrier pushes a changed frame and waits until it is sent. The session sends
// in order, so everything queued before it has been sent by then. It returns
// the messages sent before the barrier, leaving out earlier barriers.
func barrier(t *testing.T, s *Session, fc *fakeChannel) []message {
	t.Helper()
	etag := fmt.Sprintf("barrier-%d", barrierSeq.Add(1))
	s.PushChanged(etag)
	var before []message
	waitFor(t, "the barrier frame", func() bool {
		msgs := fc.messages()
		for i, m := range msgs {
			if m.text != nil && m.text["t"] == "changed" && m.text["etag"] == etag {
				before = msgs[:i]
				return true
			}
		}
		return false
	})
	var out []message
	for _, m := range before {
		if !isBarrier(m) {
			out = append(out, m)
		}
	}
	return out
}

func describe(msgs []message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		if m.text == nil {
			parts[i] = fmt.Sprintf("binary(%d)", len(m.binary))
			continue
		}
		b, _ := json.Marshal(m.text)
		parts[i] = string(b)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// expectMessages checks the messages one by one: an obj is a text frame that
// must equal it exactly, an int a binary message of that length.
func expectMessages(t *testing.T, got []message, want ...any) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		switch w := want[i].(type) {
		case obj:
			ok = got[i].text != nil && reflect.DeepEqual(got[i].text, w)
		case int:
			ok = got[i].text == nil && len(got[i].binary) == w
		default:
			t.Fatalf("bad expectation %T", w)
		}
	}
	if !ok {
		wantMsgs := make([]message, len(want))
		for i, w := range want {
			if f, isObj := w.(obj); isObj {
				wantMsgs[i] = message{text: f}
			} else {
				wantMsgs[i] = message{binary: make([]byte, w.(int))}
			}
		}
		t.Errorf("sent %s\nwant %s", describe(got), describe(wantMsgs))
	}
}

// expectError checks that f is an error frame with code and a message.
func expectError(t *testing.T, f obj, code string) {
	t.Helper()
	msg, _ := f["message"].(string)
	if f["t"] != "error" || f["code"] != code || msg == "" || len(f) != 3 {
		t.Errorf("frame %v, want an error frame with code %q and a message", f, code)
	}
}

// expectClosedWithError waits for the session to send one error frame with
// code as its last frame and close the channel.
func expectClosedWithError(t *testing.T, fc *fakeChannel, code string) {
	t.Helper()
	waitFor(t, "the channel to close", fc.isClosed)
	texts := fc.texts()
	if len(texts) == 0 {
		t.Fatalf("channel closed with no error frame")
	}
	expectError(t, texts[len(texts)-1], code)
	expectNoLateSends(t, fc)
}

// expectNoLateSends checks that nothing was sent after the channel closed: a
// frame sent then never reaches the client.
func expectNoLateSends(t *testing.T, fc *fakeChannel) {
	t.Helper()
	if late := fc.lateSends(); len(late) != 0 {
		t.Errorf("sent %s after the channel closed", describe(late))
	}
}

func body(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func ptr(s string) *string { return &s }

func TestHelloWelcome(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	if s.Authenticated() || s.App() != "" || s.Device() != "" {
		t.Error("session looks authenticated before hello")
	}
	if s.Remote() != "192.168.1.9:5000" {
		t.Errorf("Remote() = %q", s.Remote())
	}

	s.HandleText(helloFrame)
	fc.waitTexts(t, 1)
	expectMessages(t, barrier(t, s, fc), obj{
		"t": "welcome", "proto": float64(1), "serverId": "S", "name": "Desk",
		"etag": nil, "hosts": []any{"u.local", "192.168.1.5"},
	})
	if want := [][3]string{{"todo", "d1", "x"}}; !reflect.DeepEqual(b.hellos, want) {
		t.Errorf("backend Hello got %v, want %v", b.hellos, want)
	}
	if !s.Authenticated() || s.App() != "todo" || s.Device() != "d1" {
		t.Errorf("Authenticated %v, App %q, Device %q", s.Authenticated(), s.App(), s.Device())
	}

	t.Run("etag and no hosts", func(t *testing.T) {
		b := newFakeBackend()
		b.welcome = Welcome{ServerID: "S", Name: "Desk", Etag: ptr("e1")}
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		expectMessages(t, barrier(t, s, fc), obj{
			"t": "welcome", "proto": float64(1), "serverId": "S", "name": "Desk",
			"etag": "e1", "hosts": []any{},
		})
	})
}

func TestWrongProtoClosesWithProtocolError(t *testing.T) {
	for _, frame := range []string{
		`{"t":"hello","proto":2,"app":"todo","device":"d1","token":"x"}`,
		`{"t":"hello","app":"todo","device":"d1","token":"x"}`,
		`{"t":"hello","proto":"1","app":"todo","device":"d1","token":"x"}`,
		`{"t":"pair","proto":0,"app":"todo","secret":"sec","name":"Phone"}`,
	} {
		t.Run(frame, func(t *testing.T) {
			b := newFakeBackend()
			s, fc := newSession(t, b, Options{})
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
			if len(fc.texts()) != 1 {
				t.Errorf("sent %s, want one error frame", describe(fc.messages()))
			}
			if b.count("hello")+b.count("pair") != 0 {
				t.Errorf("backend was asked: %v", b.callLog())
			}
			if s.Authenticated() {
				t.Error("Authenticated() = true")
			}
		})
	}
}

func TestHelloErrorCloses(t *testing.T) {
	for _, code := range []string{CodeBadToken, CodeUnknownApp} {
		t.Run(code, func(t *testing.T) {
			b := newFakeBackend()
			b.helloErr = &Error{Code: code, Message: "refused for the test"}
			s, fc := newSession(t, b, Options{})
			s.HandleText(helloFrame)
			waitFor(t, "the channel to close", fc.isClosed)
			expectMessages(t, fc.messages(), obj{"t": "error", "code": code, "message": "refused for the test"})
			if s.Authenticated() {
				t.Error("Authenticated() = true")
			}
		})
	}
}

func TestPairUnknownAppKeepsChannelOpen(t *testing.T) {
	b := newFakeBackend()
	b.pairErr = &Error{Code: CodeUnknownApp, Message: "app notes is not served"}
	s, fc := newSession(t, b, Options{})
	s.HandleText(`{"t":"pair","proto":1,"app":"notes","secret":"sec","name":"Phone"}`)
	expectError(t, fc.waitTexts(t, 1)[0], CodeUnknownApp)
	if s.Authenticated() {
		t.Error("Authenticated() = true after unknownApp")
	}

	// The device tries again on the same channel once the app is added.
	b.pairErr = nil
	s.HandleText(`{"t":"pair","proto":1,"app":"notes","secret":"sec","name":"Phone"}`)
	msgs := barrier(t, s, fc)
	if len(msgs) != 3 || msgs[1].text["t"] != "paired" || msgs[2].text["t"] != "welcome" {
		t.Errorf("sent %s, want error, paired, welcome", describe(msgs))
	}
	if fc.isClosed() {
		t.Error("channel closed")
	}

	t.Run("badSecret closes", func(t *testing.T) {
		b := newFakeBackend()
		b.pairErr = &Error{Code: CodeBadSecret, Message: "wrong or expired secret"}
		s, fc := newSession(t, b, Options{})
		s.HandleText(pairFrame)
		expectClosedWithError(t, fc, CodeBadSecret)
		if s.Authenticated() {
			t.Error("Authenticated() = true")
		}
	})
}

func TestPairSendsPairedThenWelcome(t *testing.T) {
	b := newFakeBackend()
	b.welcome.Etag = ptr("e1")
	s, fc := newSession(t, b, Options{})
	s.HandleText(pairFrame)
	fc.waitTexts(t, 2)
	expectMessages(t, barrier(t, s, fc),
		obj{"t": "paired", "device": "d1", "token": "tok"},
		obj{
			"t": "welcome", "proto": float64(1), "serverId": "S", "name": "Desk",
			"etag": "e1", "hosts": []any{"u.local", "192.168.1.5"},
		},
	)
	if want := [][4]string{{"device", "todo", "sec", "Phone"}}; !reflect.DeepEqual(b.pairs, want) {
		t.Errorf("backend Pair got %v, want %v", b.pairs, want)
	}
	if !s.Authenticated() || s.App() != "todo" || s.Device() != "d1" {
		t.Errorf("Authenticated %v, App %q, Device %q", s.Authenticated(), s.App(), s.Device())
	}
}

func TestNotAuthenticated(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	frames := []string{`{"t":"head"}`, `{"t":"get"}`, `{"t":"put","ifEtag":null,"bytes":3}`, `{"t":"forget"}`}
	for i, frame := range frames {
		s.HandleText(frame)
		expectError(t, fc.waitTexts(t, i+1)[i], CodeNotAuthenticated)
	}
	if calls := b.callLog(); len(calls) != 0 {
		t.Errorf("backend was asked: %v", calls)
	}

	// The channel stayed open: hello still works.
	authenticate(t, s, fc)
	if msgs := barrier(t, s, fc); len(msgs) != len(frames)+1 || fc.isClosed() {
		t.Errorf("sent %s, closed %v", describe(msgs), fc.isClosed())
	}
}

func TestGetEmpty(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"get"}`)
	expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "doc", "etag": nil, "bytes": float64(0)})
}

func TestGetChunks(t *testing.T) {
	b := newFakeBackend()
	b.doc, b.docEtag = body(40000), ptr("e1")
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"get"}`)
	expectMessages(t, barrier(t, s, fc)[1:],
		obj{"t": "doc", "etag": "e1", "bytes": float64(40000)}, 16384, 16384, 7232)
	if got := bytes.Join(fc.binaries(), nil); !bytes.Equal(got, b.doc) {
		t.Error("chunks do not concatenate to the document")
	}
}

// startPausedGet authenticates, then sends get for a 40000-byte document with
// the buffer just under 1 MiB, and waits until the sender has sent the first
// chunk, seen the buffer over 1 MiB and paused.
func startPausedGet(t *testing.T, b *fakeBackend, s *Session, fc *fakeChannel) {
	t.Helper()
	b.doc, b.docEtag = body(40000), ptr("e1")
	authenticate(t, s, fc)
	fc.setBuffered(1<<20 - 1000)
	s.HandleText(`{"t":"get"}`)
	waitFor(t, "the sender to pause after the first chunk", func() bool {
		return len(fc.binaries()) == 1 && fc.wasPolled()
	})
}

func TestGetWaitsForBuffer(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	if fc.threshold != 256<<10 {
		t.Errorf("low threshold %d, want 256 KiB", fc.threshold)
	}
	startPausedGet(t, b, s, fc)

	time.Sleep(20 * time.Millisecond)
	if n := len(fc.binaries()); n != 1 {
		t.Fatalf("%d chunks sent with more than 1 MiB buffered, want 1", n)
	}
	fc.drain()
	waitFor(t, "the rest of the body", func() bool { return len(fc.binaries()) == 3 })
	if got := bytes.Join(fc.binaries(), nil); !bytes.Equal(got, b.doc) {
		t.Error("chunks do not concatenate to the document")
	}
}

func TestFramesWaitBehindBodyWithoutBlockingCaller(t *testing.T) {
	b := newFakeBackend()
	b.headEtag = ptr("e1")
	s, fc := newSession(t, b, Options{})
	startPausedGet(t, b, s, fc)

	returned := make(chan struct{})
	go func() {
		s.PushChanged("e9")
		s.HandleText(`{"t":"head"}`)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("PushChanged or HandleText blocked while the body was paused")
	}
	if texts := fc.texts(); len(texts) != 2 { // welcome, doc
		t.Fatalf("sent %s while the body was paused", describe(fc.messages()))
	}

	fc.drain()
	expectMessages(t, barrier(t, s, fc)[1:],
		obj{"t": "doc", "etag": "e1", "bytes": float64(40000)}, 16384, 16384, 7232,
		obj{"t": "changed", "etag": "e9"},
		obj{"t": "head", "etag": "e1"},
	)
}

func TestCloseDropsQueuedFrames(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	startPausedGet(t, b, s, fc)
	s.PushChanged("e9")
	s.Close()
	fc.drain()
	time.Sleep(20 * time.Millisecond)
	if n := len(fc.binaries()); n != 1 {
		t.Errorf("%d chunks sent, want 1 (the rest dropped by Close)", n)
	}
	if n := len(fc.texts()); n != 2 { // welcome, doc
		t.Errorf("sent %s, want nothing after Close", describe(fc.messages()))
	}
	expectNoLateSends(t, fc)
}

func TestPutAssemblesBody(t *testing.T) {
	doc := body(20000)

	t.Run("stored", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":"e1","bytes":20000}`)
		s.HandleBinary(doc[:16384])
		if msgs := barrier(t, s, fc); len(msgs) != 1 || len(b.putCalls()) != 0 {
			t.Fatalf("before the body was complete: sent %s, puts %d", describe(msgs), len(b.putCalls()))
		}
		s.HandleBinary(doc[16384:])
		expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "stored", "etag": "e2"})
		puts := b.putCalls()
		if len(puts) != 1 {
			t.Fatalf("%d puts, want 1", len(puts))
		}
		p := puts[0]
		if p.app != "todo" || p.device != "d1" || p.ifEtag == nil || *p.ifEtag != "e1" || !bytes.Equal(p.body, doc) {
			t.Errorf("put app %q device %q ifEtag %v body %d bytes", p.app, p.device, p.ifEtag, len(p.body))
		}
	})

	t.Run("conflict", func(t *testing.T) {
		b := newFakeBackend()
		b.putConflict, b.putCurrent = true, ptr("cur")
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":"e1","bytes":20000}`)
		s.HandleBinary(doc[:16384])
		s.HandleBinary(doc[16384:])
		expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "conflict", "etag": "cur"})
	})

	t.Run("null ifEtag", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":null,"bytes":20000}`)
		s.HandleBinary(doc[:16384])
		s.HandleBinary(doc[16384:])
		expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "stored", "etag": "e2"})
		if puts := b.putCalls(); len(puts) != 1 || puts[0].ifEtag != nil {
			t.Errorf("puts %+v, want one with a nil ifEtag", puts)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		b := newFakeBackend()
		b.putErr = &Error{Code: CodeNotJSON, Message: "body is not JSON"}
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":null,"bytes":0}`)
		expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "error", "code": CodeNotJSON, "message": "body is not JSON"})
		if puts := b.putCalls(); len(puts) != 1 || len(puts[0].body) != 0 {
			t.Errorf("puts %+v, want one with an empty body", puts)
		}
		if fc.isClosed() {
			t.Error("channel closed")
		}
	})
}

func TestPutTooLarge(t *testing.T) {
	b := newFakeBackend()
	b.maxBytes = 1000
	b.headEtag = ptr("e1")
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)

	s.HandleText(`{"t":"put","ifEtag":null,"bytes":5000}`)
	s.HandleBinary(body(4000))
	s.HandleBinary(body(999))
	if msgs := barrier(t, s, fc); len(msgs) != 1 {
		t.Fatalf("sent %s before the announced body was consumed", describe(msgs))
	}
	s.HandleBinary(body(1))
	msgs := barrier(t, s, fc)[1:]
	if len(msgs) != 1 {
		t.Fatalf("sent %s, want one error frame", describe(msgs))
	}
	expectError(t, msgs[0].text, CodeTooLarge)
	if n := len(b.putCalls()); n != 0 {
		t.Errorf("backend Put called %d times", n)
	}

	s.HandleText(`{"t":"head"}`)
	expectMessages(t, barrier(t, s, fc)[2:], obj{"t": "head", "etag": "e1"})
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

func TestPutWithoutValidBytesIsProtocolError(t *testing.T) {
	for _, frame := range []string{
		`{"t":"put","ifEtag":null}`,
		`{"t":"put","ifEtag":null,"bytes":-1}`,
		`{"t":"put","ifEtag":null,"bytes":1.5}`,
		`{"t":"put","ifEtag":null,"bytes":"10"}`,
	} {
		t.Run(frame, func(t *testing.T) {
			b := newFakeBackend()
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
			if n := len(b.putCalls()); n != 0 {
				t.Errorf("backend Put called %d times", n)
			}
		})
	}
}

func TestBinaryWithoutPutIsProtocolError(t *testing.T) {
	t.Run("authenticated", func(t *testing.T) {
		s, fc := newSession(t, newFakeBackend(), Options{})
		authenticate(t, s, fc)
		s.HandleBinary([]byte("{}"))
		expectClosedWithError(t, fc, CodeProtocol)
	})
	t.Run("after a complete put", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":null,"bytes":2}`)
		s.HandleBinary([]byte("{}"))
		s.HandleBinary([]byte("{}"))
		expectClosedWithError(t, fc, CodeProtocol)
		if n := len(b.putCalls()); n != 1 {
			t.Errorf("backend Put called %d times, want 1", n)
		}
	})
	t.Run("after a refused put before hello", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		s.HandleText(`{"t":"put","ifEtag":null,"bytes":2}`)
		expectError(t, fc.waitTexts(t, 1)[0], CodeNotAuthenticated)
		s.HandleBinary([]byte("{}"))
		expectClosedWithError(t, fc, CodeProtocol)
		waitFor(t, "backend Closed", func() bool { return b.count("closed") == 1 })
		if calls := b.callLog(); len(calls) != 1 || calls[0] != "closed" {
			t.Errorf("backend calls %v, want only closed", calls)
		}
	})
}

func TestOverrunIsProtocolError(t *testing.T) {
	t.Run("second message", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":null,"bytes":10}`)
		s.HandleBinary(body(6))
		s.HandleBinary(body(6))
		expectClosedWithError(t, fc, CodeProtocol)
		if n := len(b.putCalls()); n != 0 {
			t.Errorf("backend Put called %d times", n)
		}
	})
	t.Run("too large and overrun", func(t *testing.T) {
		b := newFakeBackend()
		b.maxBytes = 5
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","ifEtag":null,"bytes":10}`)
		s.HandleBinary(body(11))
		expectClosedWithError(t, fc, CodeProtocol)
	})
}

func TestTextDuringPutBodyIsProtocolError(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"put","ifEtag":null,"bytes":100}`)
	s.HandleBinary(body(10))
	s.HandleText(`{"t":"head"}`)
	expectClosedWithError(t, fc, CodeProtocol)
	if b.count("head") != 0 || len(b.putCalls()) != 0 {
		t.Errorf("backend calls %v", b.callLog())
	}
}

func TestForget(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"forget"}`)
	waitFor(t, "the channel to close", fc.isClosed)
	if want := [][2]string{{"todo", "d1"}}; !reflect.DeepEqual(b.forgets, want) {
		t.Errorf("backend Forget got %v, want %v", b.forgets, want)
	}
	waitFor(t, "backend Closed", func() bool { return b.count("closed") == 1 })
}

func TestPushChanged(t *testing.T) {
	s, fc := newSession(t, newFakeBackend(), Options{})
	authenticate(t, s, fc)
	s.PushChanged("e9")
	expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "changed", "etag": "e9"})
}

func TestPushChangedAfterCloseIsNoop(t *testing.T) {
	for _, auth := range []bool{true, false} {
		t.Run(fmt.Sprintf("authenticated %v", auth), func(t *testing.T) {
			s, fc := newSession(t, newFakeBackend(), Options{})
			if auth {
				authenticate(t, s, fc)
			}
			s.Close()
			n := len(fc.messages())
			s.PushChanged("e9")
			time.Sleep(20 * time.Millisecond)
			if got := fc.messages(); len(got) != n {
				t.Errorf("sent %s after Close", describe(got[n:]))
			}
			expectNoLateSends(t, fc)
		})
	}
}

func TestSenderExitsAfterClose(t *testing.T) {
	waitSender := func(t *testing.T, s *Session) {
		t.Helper()
		select {
		case <-s.senderDone:
		case <-time.After(2 * time.Second):
			t.Fatal("the sender goroutine is still running after the session closed")
		}
	}
	t.Run("idle", func(t *testing.T) {
		s, fc := newSession(t, newFakeBackend(), Options{})
		authenticate(t, s, fc)
		barrier(t, s, fc)
		s.Close()
		waitSender(t, s)
	})
	t.Run("paused on a full buffer", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		startPausedGet(t, b, s, fc)
		s.Close()
		waitSender(t, s)
		expectNoLateSends(t, fc)
	})
	t.Run("after a protocol error", func(t *testing.T) {
		s, fc := newSession(t, newFakeBackend(), Options{})
		s.HandleText(`{"t":"bogus"}`)
		expectClosedWithError(t, fc, CodeProtocol)
		waitSender(t, s)
	})
	t.Run("after the auth timeout", func(t *testing.T) {
		s, fc := newSession(t, newFakeBackend(), Options{AuthTimeout: 10 * time.Millisecond})
		waitFor(t, "the auth timeout", fc.isClosed)
		waitSender(t, s)
	})
	t.Run("after a failed send", func(t *testing.T) {
		s, fc := newSession(t, newFakeBackend(), Options{})
		fc.mu.Lock()
		fc.failSends = true
		fc.mu.Unlock()
		s.HandleText(helloFrame)
		waitFor(t, "the channel to close", fc.isClosed)
		waitSender(t, s)
	})
}

func TestChangedDuringHelloFollowsWelcome(t *testing.T) {
	welcome := func(etag any) obj {
		return obj{
			"t": "welcome", "proto": float64(1), "serverId": "S", "name": "Desk",
			"etag": etag, "hosts": []any{"u.local", "192.168.1.5"},
		}
	}
	push := func(s *Session) {
		s.PushChanged("e3")
		s.PushChanged("e5")
	}

	t.Run("hello, older welcome etag", func(t *testing.T) {
		b := newFakeBackend()
		b.welcome.Etag, b.onHello = ptr("e1"), push
		s, fc := newSession(t, b, Options{})
		s.HandleText(helloFrame)
		expectMessages(t, barrier(t, s, fc), welcome("e1"), obj{"t": "changed", "etag": "e5"})
	})
	t.Run("hello, no document in the welcome", func(t *testing.T) {
		b := newFakeBackend()
		b.onHello = push
		s, fc := newSession(t, b, Options{})
		s.HandleText(helloFrame)
		expectMessages(t, barrier(t, s, fc), welcome(nil), obj{"t": "changed", "etag": "e5"})
	})
	t.Run("hello, welcome carries the pushed etag", func(t *testing.T) {
		b := newFakeBackend()
		b.welcome.Etag, b.onHello = ptr("e5"), push
		s, fc := newSession(t, b, Options{})
		s.HandleText(helloFrame)
		expectMessages(t, barrier(t, s, fc), welcome("e5"))
	})
	t.Run("pair", func(t *testing.T) {
		b := newFakeBackend()
		b.welcome.Etag, b.onPair = ptr("e1"), push
		s, fc := newSession(t, b, Options{})
		s.HandleText(pairFrame)
		expectMessages(t, barrier(t, s, fc),
			obj{"t": "paired", "device": "d1", "token": "tok"},
			welcome("e1"),
			obj{"t": "changed", "etag": "e5"})
	})
	t.Run("refused hello drops the push", func(t *testing.T) {
		b := newFakeBackend()
		b.helloErr, b.onHello = &Error{Code: CodeBadToken, Message: "no"}, push
		s, fc := newSession(t, b, Options{})
		s.HandleText(helloFrame)
		waitFor(t, "the channel to close", fc.isClosed)
		expectMessages(t, fc.messages(), obj{"t": "error", "code": CodeBadToken, "message": "no"})
	})
}

func TestCloseDuringAuthLeavesNoRegistration(t *testing.T) {
	// closedAfterReturn reports whether the backend saw Closed after the
	// given call returned.
	closedAfterReturn := func(b *fakeBackend, call string) bool {
		returned := false
		for _, c := range b.callLog() {
			if c == call+" returned" {
				returned = true
			}
			if returned && c == "closed" {
				return true
			}
		}
		return false
	}

	t.Run("hello closes the session", func(t *testing.T) {
		b := newFakeBackend()
		b.onHello = func(s *Session) { s.Close() }
		s, fc := newSession(t, b, Options{})
		s.HandleText(helloFrame)
		if !closedAfterReturn(b, "hello") {
			t.Errorf("backend calls %v, want closed after hello returned", b.callLog())
		}
		if msgs := fc.messages(); len(msgs) != 0 || s.Authenticated() {
			t.Errorf("sent %s, Authenticated %v", describe(msgs), s.Authenticated())
		}
	})
	t.Run("auth timer fires during hello", func(t *testing.T) {
		b := newFakeBackend()
		var fc *fakeChannel
		b.onHello = func(s *Session) { waitFor(t, "the auth timer", fc.isClosed) }
		var s *Session
		s, fc = newSession(t, b, Options{AuthTimeout: 20 * time.Millisecond})
		s.HandleText(helloFrame)
		if !closedAfterReturn(b, "hello") {
			t.Errorf("backend calls %v, want closed after hello returned", b.callLog())
		}
		if msgs := fc.messages(); len(msgs) != 0 || s.Authenticated() {
			t.Errorf("sent %s, Authenticated %v", describe(msgs), s.Authenticated())
		}
	})
	t.Run("pair closes the session", func(t *testing.T) {
		b := newFakeBackend()
		b.onPair = func(s *Session) { s.Close() }
		s, fc := newSession(t, b, Options{})
		s.HandleText(pairFrame)
		if !closedAfterReturn(b, "pair") {
			t.Errorf("backend calls %v, want closed after pair returned", b.callLog())
		}
		if msgs := fc.messages(); len(msgs) != 0 || s.Authenticated() {
			t.Errorf("sent %s, Authenticated %v", describe(msgs), s.Authenticated())
		}
		// The token never went out, so the new device entry is undone.
		if want := [][2]string{{"todo", "d1"}}; !reflect.DeepEqual(b.forgets, want) {
			t.Errorf("backend Forget got %v, want %v", b.forgets, want)
		}
	})
	t.Run("hello closes the session: nothing to undo", func(t *testing.T) {
		b := newFakeBackend()
		b.onHello = func(s *Session) { s.Close() }
		s, _ := newSession(t, b, Options{})
		s.HandleText(helloFrame)
		if b.count("forget") != 0 {
			t.Errorf("backend calls %v, want no forget", b.callLog())
		}
	})
}

func TestPairingIsUndoneUnlessConfirmed(t *testing.T) {
	forgets := func(b *fakeBackend) [][2]string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return append([][2]string(nil), b.forgets...)
	}
	paired := func(t *testing.T, b *fakeBackend) (*Session, *fakeChannel) {
		t.Helper()
		s, fc := newSession(t, b, Options{})
		s.HandleText(pairFrame)
		fc.waitTexts(t, 2)
		return s, fc
	}

	t.Run("closed before the client's next frame", func(t *testing.T) {
		b := newFakeBackend()
		s, _ := paired(t, b)
		s.Close()
		if want := [][2]string{{"todo", "d1"}}; !reflect.DeepEqual(forgets(b), want) {
			t.Errorf("backend Forget got %v, want %v", forgets(b), want)
		}
		calls := b.callLog()
		if i := slices.Index(calls, "forget"); i < 0 || i > slices.Index(calls, "closed") {
			t.Errorf("backend calls %v, want forget before closed", calls)
		}
	})
	t.Run("paired dropped from the queue by the close", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		fc.setBuffered(2 << 20) // the sender waits before the first frame
		s.HandleText(pairFrame)
		waitFor(t, "the sender to wait on the buffer", fc.wasPolled)
		s.Close()
		if len(fc.texts()) != 0 {
			t.Fatalf("sent %s", describe(fc.messages()))
		}
		if want := [][2]string{{"todo", "d1"}}; !reflect.DeepEqual(forgets(b), want) {
			t.Errorf("backend Forget got %v, want %v", forgets(b), want)
		}
	})
	t.Run("sending paired fails", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		fc.mu.Lock()
		fc.failSends = true
		fc.mu.Unlock()
		s.HandleText(pairFrame)
		waitFor(t, "the undo", func() bool { return len(forgets(b)) == 1 })
	})
	for _, frame := range []string{`{"t":"head"}`, `{"t":"get"}`, `{"t":"bogus"}`} {
		t.Run("confirmed by "+frame, func(t *testing.T) {
			b := newFakeBackend()
			s, _ := paired(t, b)
			s.HandleText(frame) // even a frame that ends the session
			s.Close()
			if got := forgets(b); len(got) != 0 {
				t.Errorf("backend Forget got %v, want none", got)
			}
		})
	}
	t.Run("confirmed by a body piece", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := paired(t, b)
		s.HandleBinary([]byte("{}")) // a protocol error, but the client got the token
		waitFor(t, "the channel to close", fc.isClosed)
		if got := forgets(b); len(got) != 0 {
			t.Errorf("backend Forget got %v, want none", got)
		}
	})
	t.Run("hello is never undone", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.Close()
		if got := forgets(b); len(got) != 0 {
			t.Errorf("backend Forget got %v, want none", got)
		}
	})
}

func TestQueueLimitClosesSession(t *testing.T) {
	b := newFakeBackend()
	b.maxBytes = 50000
	var onClose atomic.Int32
	s, fc := newSession(t, b, Options{OnClose: func() { onClose.Add(1) }})
	startPausedGet(t, b, s, fc) // the first document is out of the queue, in the sender

	limit := int(b.maxBytes) + queueSlack
	most := limit/len(b.doc) + 2
	n := 0
	for ; n < most && !fc.isClosed(); n++ {
		s.HandleText(`{"t":"get"}`)
	}
	waitFor(t, "the channel to close", fc.isClosed)
	if n < limit/(len(b.doc)+100) {
		t.Errorf("closed after %d queued documents, want about %d", n, limit/len(b.doc))
	}
	waitFor(t, "OnClose", func() bool { return onClose.Load() == 1 })
	if c := b.count("closed"); c != 1 {
		t.Errorf("backend Closed %d times, want 1", c)
	}
	fc.drain()
	time.Sleep(20 * time.Millisecond)
	if bins := len(fc.binaries()); bins != 1 {
		t.Errorf("%d chunks sent, want only the first one before the close", bins)
	}
	expectNoLateSends(t, fc)
}

func TestOneQueuedDocIsAllowed(t *testing.T) {
	b := newFakeBackend()
	b.maxBytes = 50000
	s, fc := newSession(t, b, Options{})
	startPausedGet(t, b, s, fc)
	s.HandleText(`{"t":"get"}`)
	s.HandleText(`{"t":"head"}`)
	fc.drain()
	msgs := barrier(t, s, fc)
	if fc.isClosed() {
		t.Fatal("channel closed")
	}
	if got := len(fc.binaries()); got != 6 {
		t.Errorf("sent %s, want two whole documents", describe(msgs))
	}
}

func TestADocumentOverTheLimitCanStillBeRead(t *testing.T) {
	b := newFakeBackend()
	b.maxBytes = 1000 // lowered after the document was stored
	b.doc, b.docEtag = body(queueSlack+5000), ptr("e1")
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"get"}`)
	waitFor(t, "the whole document", func() bool {
		fc.drain() // the client reads
		return len(bytes.Join(fc.binaries(), nil)) == len(b.doc)
	})
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

func TestPushFloodClosesSession(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	startPausedGet(t, b, s, fc)
	for i := 0; i < 200000 && !fc.isClosed(); i++ {
		s.PushChanged("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	}
	waitFor(t, "the channel to close", fc.isClosed)
	waitFor(t, "backend Closed", func() bool { return b.count("closed") == 1 })
	expectNoLateSends(t, fc)
}

func TestAuthTimeout(t *testing.T) {
	t.Run("no hello", func(t *testing.T) {
		var onClose atomic.Int32
		start := time.Now()
		s, fc := newSession(t, newFakeBackend(), Options{
			AuthTimeout: 50 * time.Millisecond,
			OnClose:     func() { onClose.Add(1) },
		})
		waitFor(t, "OnClose", func() bool { return onClose.Load() == 1 })
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 200*time.Millisecond {
			t.Errorf("closed after %v, want between 50 and 200 ms", elapsed)
		}
		if !fc.isClosed() {
			t.Error("channel not closed")
		}
		s.Close()
		if n := onClose.Load(); n != 1 {
			t.Errorf("OnClose ran %d times, want 1", n)
		}
	})
	t.Run("hello in time", func(t *testing.T) {
		var onClose atomic.Int32
		s, fc := newSession(t, newFakeBackend(), Options{
			AuthTimeout: 50 * time.Millisecond,
			OnClose:     func() { onClose.Add(1) },
		})
		authenticate(t, s, fc)
		time.Sleep(100 * time.Millisecond)
		if fc.isClosed() || onClose.Load() != 0 {
			t.Errorf("authenticated session closed by the auth timer (OnClose ran %d times)", onClose.Load())
		}
	})
}

func TestCloseRunsOnce(t *testing.T) {
	b := newFakeBackend()
	var onClose atomic.Int32
	var fc *fakeChannel
	var orderOK atomic.Bool
	var s *Session
	s, fc = newSession(t, b, Options{OnClose: func() {
		onClose.Add(1)
		orderOK.Store(fc.isClosed() && b.count("closed") == 1)
	}})
	authenticate(t, s, fc)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Close()
		}()
	}
	wg.Wait()
	s.Close()
	if fc.closeCount() != 1 || b.count("closed") != 1 || onClose.Load() != 1 {
		t.Errorf("channel Close %d, backend Closed %d, OnClose %d; want 1 each",
			fc.closeCount(), b.count("closed"), onClose.Load())
	}
	if !orderOK.Load() {
		t.Error("OnClose ran before the channel and the backend were told")
	}
}

func TestUnknownFrame(t *testing.T) {
	for _, auth := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated %v", auth), func(t *testing.T) {
			s, fc := newSession(t, newFakeBackend(), Options{})
			if auth {
				authenticate(t, s, fc)
			}
			s.HandleText(`{"t":"bogus"}`)
			expectClosedWithError(t, fc, CodeProtocol)
		})
	}
}

func TestMalformedTextIsProtocolError(t *testing.T) {
	for _, frame := range []string{`not json`, `[1]`, `null`, `"hello"`, `{"t":5}`, `{}`, `{"t":"hello","proto":1,"app":7}`} {
		t.Run(frame, func(t *testing.T) {
			s, fc := newSession(t, newFakeBackend(), Options{})
			authenticate(t, s, fc)
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
		})
	}
}

func TestSecondHelloIsProtocolError(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(helloFrame)
	expectClosedWithError(t, fc, CodeProtocol)
	if n := b.count("hello"); n != 1 {
		t.Errorf("backend Hello called %d times, want 1", n)
	}
}

func TestBackendErrorsKeepChannelOpen(t *testing.T) {
	b := newFakeBackend()
	b.headErr = &Error{Code: CodeInternal, Message: "disk on fire"}
	b.getErr = &Error{Code: CodeInternal, Message: "disk on fire"}
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"head"}`)
	s.HandleText(`{"t":"get"}`)
	errFrame := obj{"t": "error", "code": CodeInternal, "message": "disk on fire"}
	expectMessages(t, barrier(t, s, fc)[1:], errFrame, errFrame)
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

// A server-kind pair drops the app it names and opens a link session, which
// gets the welcome's role, answers apps and refuses every document frame.
func TestServerKindPairOpensALinkSession(t *testing.T) {
	for _, frame := range []string{`{"t":"head"}`, `{"t":"get"}`, `{"t":"put","ifEtag":null,"bytes":2}`} {
		t.Run(frame, func(t *testing.T) {
			b := newFakeBackend()
			b.welcome.Role = RoleMain
			b.apps = []AppInfo{{ID: "todo", Name: "Todo"}}
			s, fc := newSession(t, b, Options{})
			s.HandleText(`{"t":"pair","proto":1,"kind":"server","app":"todo","secret":"sec","name":"Laptop"}`)
			if want := [][4]string{{KindServer, "", "sec", "Laptop"}}; !reflect.DeepEqual(b.pairs, want) {
				t.Fatalf("backend Pair got %v, want %v", b.pairs, want)
			}
			if !s.Authenticated() || s.App() != "" || s.Device() != "d1" {
				t.Fatalf("Authenticated %v, App %q, Device %q; want a link session of d1", s.Authenticated(), s.App(), s.Device())
			}
			s.HandleText(`{"t":"apps"}`)
			expectMessages(t, barrier(t, s, fc),
				obj{"t": "paired", "device": "d1", "token": "tok"},
				obj{
					"t": "welcome", "proto": float64(1), "role": "main", "serverId": "S", "name": "Desk",
					"etag": nil, "hosts": []any{"u.local", "192.168.1.5"},
				},
				obj{"t": "apps", "apps": []any{obj{"id": "todo", "name": "Todo"}}},
			)

			s.HandleText(frame)
			waitFor(t, "the channel to close", fc.isClosed)
			texts := fc.texts()
			if last := texts[len(texts)-1]; last["t"] != "error" || last["code"] != CodeProtocol ||
				!strings.Contains(last["message"].(string), "link session") {
				t.Errorf("last frame %v, want a protocol error about the link session", last)
			}
			if b.count("head")+b.count("get")+b.count("put") != 0 {
				t.Errorf("the backend was asked for a document: %v", b.callLog())
			}
		})
	}
}

// An app list pushed before the welcome is held, only the latest kept, and
// sent right after it; one pushed to a closed session goes nowhere.
func TestAppsPushedDuringHelloFollowWelcome(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	b.onHello = func(s *Session) {
		s.PushApps([]AppInfo{{ID: "old", Name: "Old"}})
		s.PushApps(nil)
	}
	s.HandleText(linkHello)
	expectMessages(t, barrier(t, s, fc),
		obj{
			"t": "welcome", "proto": float64(1), "serverId": "S", "name": "Desk",
			"etag": nil, "hosts": []any{"u.local", "192.168.1.5"},
		},
		obj{"t": "apps", "apps": []any{}},
	)
	s.PushApps([]AppInfo{{ID: "todo", Name: "Todo"}})
	expectMessages(t, barrier(t, s, fc)[2:], obj{"t": "apps", "apps": []any{obj{"id": "todo", "name": "Todo"}}})

	s.Close()
	before := len(fc.messages())
	s.PushApps([]AppInfo{{ID: "todo", Name: "Todo"}})
	if n := len(fc.messages()) + len(fc.lateSends()); n != before {
		t.Errorf("a push after Close sent %d messages", n-before)
	}
}

// The backend's refusal of apps closes the session when it is a protocol
// error and only answers otherwise.
func TestAppsRefusals(t *testing.T) {
	b := newFakeBackend()
	b.appsErr = &Error{CodeInternal, "disk"}
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"apps"}`)
	got := barrier(t, s, fc)
	if last := got[len(got)-1].text; last["t"] != "error" || last["code"] != CodeInternal || fc.isClosed() {
		t.Fatalf("sent %s, want an internal error with the channel open", describe(got))
	}

	b.appsErr = &Error{CodeProtocol, "not a server"}
	s.HandleText(`{"t":"apps"}`)
	waitFor(t, "the channel to close", fc.isClosed)
}
