package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The document frames of an app session: list, get, begin, put, delete,
// commit, abort, resolve, and the pushes of the set.

// listingOf is a listing of n documents and r tombstones, sorted by key.
func listingOf(rev *string, n, r int) Listing {
	l := Listing{Revision: rev, Docs: []DocInfo{}, Removed: []Removed{}}
	for i := range n {
		l.Docs = append(l.Docs, DocInfo{Key: Key{Path: "d", ID: fmt.Sprintf("x%04d", i), Type: "bin"}, Etag: "e", Bytes: 1})
	}
	for i := range r {
		l.Removed = append(l.Removed, Removed{Key: Key{Path: "r", ID: fmt.Sprintf("y%04d", i), Type: "bin"}, At: "2026-11-13T10:00:00Z"})
	}
	return l
}

// lastText waits until everything queued so far has been sent, and returns
// the last text frame before the barrier.
func lastText(t *testing.T, s *Session, fc *fakeChannel) obj {
	t.Helper()
	msgs := barrier(t, s, fc)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].text != nil {
			return msgs[i].text
		}
	}
	t.Fatal("no text frame")
	return nil
}

func TestListPages(t *testing.T) {
	b := newFakeBackend()
	b.listing = listingOf(ptr("r1"), 700, 400)
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)

	var docs, removed int
	var pages int
	var cursor any
	for {
		frame := `{"t":"list"}`
		if cursor != nil {
			frame = fmt.Sprintf(`{"t":"list","revision":"r1","cursor":%q}`, cursor)
		}
		s.HandleText(frame)
		f := lastText(t, s, fc)
		if f["t"] != "list" || f["revision"] != "r1" {
			t.Fatalf("page %d: %v", pages, f["t"])
		}
		d, r := len(f["docs"].([]any)), len(f["removed"].([]any))
		if d+r > ListPage || d+r == 0 {
			t.Fatalf("page %d holds %d entries", pages, d+r)
		}
		docs, removed, pages = docs+d, removed+r, pages+1
		cursor = f["cursor"]
		if cursor == nil {
			break
		}
	}
	if docs != 700 || removed != 400 || pages != 3 {
		t.Errorf("%d documents, %d tombstones in %d pages; want 700, 400 in 3", docs, removed, pages)
	}
}

func TestListStaleAcrossPages(t *testing.T) {
	b := newFakeBackend()
	b.listing = listingOf(ptr("r1"), 600, 0)
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"list"}`)
	cursor := lastText(t, s, fc)["cursor"].(string)

	b.mu.Lock()
	b.listing = listingOf(ptr("r2"), 601, 0)
	b.mu.Unlock()
	s.HandleText(fmt.Sprintf(`{"t":"list","revision":"r1","cursor":%q}`, cursor))
	if f := lastText(t, s, fc); f["t"] != "stale" || f["revision"] != "r2" {
		t.Errorf("sent %v, want stale at r2", f)
	}

	// Pinned to the empty set: null is a revision too.
	b.mu.Lock()
	b.listing = listingOf(nil, 0, 2)
	b.mu.Unlock()
	s.HandleText(`{"t":"list","revision":null}`)
	if f := lastText(t, s, fc); f["t"] != "list" || f["revision"] != nil || len(f["removed"].([]any)) != 2 {
		t.Errorf("sent %v, want the empty set's list", f)
	}
	s.HandleText(`{"t":"list","revision":"r1"}`)
	if f := lastText(t, s, fc); f["t"] != "stale" || f["revision"] != nil {
		t.Errorf("sent %v, want stale at null", f)
	}
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

func TestListRefusals(t *testing.T) {
	for _, frame := range []string{
		`{"t":"list","cursor":"d:a.bin"}`,
		`{"t":"list","revision":"r1","cursor":"x:a.bin"}`,
		`{"t":"list","revision":5}`,
	} {
		t.Run(frame, func(t *testing.T) {
			b := newFakeBackend()
			b.listing = listingOf(ptr("r1"), 2, 0)
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
		})
	}
}

func TestGetFromAnOffset(t *testing.T) {
	b := newFakeBackend()
	doc := body(40000)
	b.setDoc(docKey, "e1", doc)
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"get","path":"notes","id":"n1","type":"json","etag":"e1","offset":30000}`)
	expectMessages(t, barrier(t, s, fc)[1:], docObj("e1", 40000, 30000), 10000)
	if got := bytes.Join(fc.binaries(), nil); !bytes.Equal(got, doc[30000:]) {
		t.Error("the body is not the document past the offset")
	}
	if f := b.openFiles()[0]; f.closeCount() != 1 {
		t.Errorf("the file closed %d times", f.closeCount())
	}
	if g := b.gets[0]; g.key != docKey || g.etag == nil || *g.etag != "e1" {
		t.Errorf("backend Get got %+v", g)
	}
}

func TestGetStale(t *testing.T) {
	b := newFakeBackend()
	b.setDoc(docKey, "e1", body(10))
	b.revision = ptr("r9")
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"get","path":"notes","id":"n1","type":"json","etag":"e0"}`)
	s.HandleText(`{"t":"get","path":"notes","id":"gone","type":"json"}`)
	expectMessages(t, barrier(t, s, fc)[1:],
		obj{"t": "stale", "revision": "r9"}, obj{"t": "stale", "revision": "r9"})
	if fc.isClosed() || len(b.openFiles()) != 0 {
		t.Error("channel closed, or a file opened")
	}
}

func TestGetOffsetRefusals(t *testing.T) {
	for name, frame := range map[string]string{
		"offset without etag": `{"t":"get","path":"notes","id":"n1","type":"json","offset":5}`,
		"negative offset":     `{"t":"get","path":"notes","id":"n1","type":"json","etag":"e1","offset":-1}`,
		"offset past the end": `{"t":"get","path":"notes","id":"n1","type":"json","etag":"e1","offset":11}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := newFakeBackend()
			b.setDoc(docKey, "e1", body(10))
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
			for _, f := range b.openFiles() {
				if f.closeCount() != 1 {
					t.Errorf("a file closed %d times", f.closeCount())
				}
			}
		})
	}
}

func TestGetAtTheEndSendsNoBody(t *testing.T) {
	b := newFakeBackend()
	b.setDoc(docKey, "e1", body(10))
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"get","path":"notes","id":"n1","type":"json","etag":"e1","offset":10}`)
	expectMessages(t, barrier(t, s, fc)[1:], docObj("e1", 10, 10))
	if f := b.openFiles()[0]; f.closeCount() != 1 {
		t.Errorf("the file closed %d times", f.closeCount())
	}
}

// A file that cannot be read to the size announced closes the session: the
// client cannot tell what follows the body from the body.
func TestGetReadErrorClosesSession(t *testing.T) {
	b := newFakeBackend()
	b.setDoc(docKey, "e1", body(40000))
	b.file = func(f *fakeFile) { f.readErr, f.failAt = errors.New("disk gone"), 20000 }
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(getFrame)
	waitFor(t, "the channel to close", fc.isClosed)
	if n := len(fc.binaries()); n != 1 {
		t.Errorf("%d chunks sent, want the one before the failure", n)
	}
	waitFor(t, "the file to close", func() bool { return b.openFiles()[0].closeCount() == 1 })
}

func TestBegin(t *testing.T) {
	b := newFakeBackend()
	b.began = []Staged{{Key: docKey, IfEtag: ptr("e1"), Bytes: 100, Received: 40}}
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"begin","tx":"tx-0123456789abcdef"}`)
	expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "began", "tx": "tx-0123456789abcdef", "staged": []any{
		obj{"path": "notes", "id": "n1", "type": "json", "ifEtag": "e1", "bytes": float64(100), "received": float64(40)},
	}})
	if want := [3]string{"todo", "d1", "tx-0123456789abcdef"}; b.begins[0] != want {
		t.Errorf("backend Begin got %v", b.begins)
	}

	b.beginErr = &Error{CodeProtocol, "invalid transaction id"}
	s.HandleText(`{"t":"begin","tx":"short"}`)
	expectClosedWithError(t, fc, CodeProtocol)
}

func TestPutStreamsIntoTheSink(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	doc := body(40000)
	s.HandleText(`{"t":"put","tx":"tx-0123456789abcdef","path":"notes","id":"n1","type":"json","ifEtag":"e1","bytes":40000,"offset":0}`)
	p := b.putCalls()[0]
	if p.tx != "tx-0123456789abcdef" || p.key != docKey || p.ifEtag == nil || *p.ifEtag != "e1" || p.size != 40000 || p.offset != 0 {
		t.Fatalf("backend Put got %+v", p)
	}
	// Each message is in the sink before the next one is handled: the
	// session holds no body.
	for off := 0; off < len(doc); off += ChunkBytes {
		chunk := doc[off:min(off+ChunkBytes, len(doc))]
		s.HandleBinary(chunk)
		data, _, _ := p.sink.state()
		if len(data) != off+len(chunk) {
			t.Fatalf("after %d bytes the sink holds %d", off+len(chunk), len(data))
		}
	}
	expectMessages(t, barrier(t, s, fc)[1:], obj{"t": "staged", "path": "notes", "id": "n1", "type": "json", "received": float64(40000)})
	data, writes, closes := p.sink.state()
	if !bytes.Equal(data, doc) || len(writes) != 3 || closes != 1 {
		t.Errorf("the sink got %d bytes in %d writes, closed %d times", len(data), len(writes), closes)
	}
}

func TestPutResumes(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":null,"bytes":10,"offset":6}`)
	s.HandleBinary(body(4))
	// Only the etag the complete body expects changes: no body follows.
	s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":"e2","bytes":10,"offset":10}`)
	staged := obj{"t": "staged", "path": "notes", "id": "n1", "type": "json", "received": float64(10)}
	expectMessages(t, barrier(t, s, fc)[1:], staged, staged)
	puts := b.putCalls()
	if puts[0].offset != 6 || puts[1].offset != 10 || *puts[1].ifEtag != "e2" {
		t.Errorf("puts %+v", puts)
	}
	if data, _, _ := puts[0].sink.state(); len(data) != 4 {
		t.Errorf("the sink got %d bytes, want the 4 past the offset", len(data))
	}
}

// A refused put is answered after its body, which the session discards; a
// put the client sends next works.
func TestPutRefusedDiscardsTheBody(t *testing.T) {
	for _, code := range []string{CodeQuota, CodeBadPath, CodeNoTransaction} {
		t.Run(code, func(t *testing.T) {
			b := newFakeBackend()
			b.putErr = &Error{code, "refused"}
			b.head = ptr("r1")
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":null,"bytes":5000}`)
			s.HandleBinary(body(4999))
			if msgs := barrier(t, s, fc); len(msgs) != 1 {
				t.Fatalf("sent %s before the body was consumed", describe(msgs))
			}
			s.HandleBinary(body(1))
			s.HandleText(`{"t":"head"}`)
			expectMessages(t, barrier(t, s, fc)[1:],
				obj{"t": "error", "code": code, "message": "refused"}, obj{"t": "head", "revision": "r1"})
			if fc.isClosed() {
				t.Error("channel closed")
			}
		})
	}
	t.Run("protocol closes at once", func(t *testing.T) {
		b := newFakeBackend()
		b.putErr = &Error{CodeProtocol, "the offset does not fit"}
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":null,"bytes":10,"offset":5}`)
		expectClosedWithError(t, fc, CodeProtocol)
	})
}

// A sink that fails mid-body is closed; the rest of the body is discarded,
// and the failure answered after it.
func TestPutSinkFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"ended transaction": {&Error{CodeNoTransaction, "ended"}, CodeNoTransaction},
		"disk error":        {errors.New("disk full"), CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			b := newFakeBackend()
			b.sink = func() *fakeSink { return &fakeSink{failAt: 20000, failErr: tc.err} }
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":null,"bytes":40000}`)
			doc := body(40000)
			for off := 0; off < len(doc); off += ChunkBytes {
				s.HandleBinary(doc[off:min(off+ChunkBytes, len(doc))])
			}
			msgs := barrier(t, s, fc)[1:]
			if len(msgs) != 1 {
				t.Fatalf("sent %s, want one error", describe(msgs))
			}
			expectError(t, msgs[0].text, tc.code)
			data, writes, closes := b.putCalls()[0].sink.state()
			if len(data) != 20000 || len(writes) != 2 || closes != 1 {
				t.Errorf("the sink got %d bytes in %d writes, closed %d times", len(data), len(writes), closes)
			}
		})
	}
	t.Run("close error", func(t *testing.T) {
		b := newFakeBackend()
		b.sink = func() *fakeSink { return &fakeSink{closeErr: errors.New("sync failed")} }
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":null,"bytes":3}`)
		s.HandleBinary([]byte("abc"))
		expectError(t, lastText(t, s, fc), CodeInternal)
	})
}

// A session that ends under a body closes its sink, so that what arrived
// stays staged for a put that resumes it.
func TestCloseDuringPutClosesTheSink(t *testing.T) {
	b := newFakeBackend()
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"put","tx":"t","path":"notes","id":"n1","type":"json","ifEtag":null,"bytes":100}`)
	s.HandleBinary(body(30))
	s.Close()
	data, _, closes := b.putCalls()[0].sink.state()
	if len(data) != 30 || closes != 1 {
		t.Errorf("the sink holds %d bytes, closed %d times; want 30, once", len(data), closes)
	}
	s.HandleBinary(body(10)) // after the close: ignored
	if data, _, _ := b.putCalls()[0].sink.state(); len(data) != 30 {
		t.Errorf("written after the close: %d bytes", len(data))
	}
	_ = fc
}

func TestDeleteCommitAbortResolve(t *testing.T) {
	b := newFakeBackend()
	b.committed = Committed{Revision: ptr("r2"), Docs: []DocEtag{{Key: docKey, Etag: ptr("e2")}, {Key: Key{ID: "x", Type: "md"}}}}
	s, fc := newSession(t, b, Options{})
	authenticate(t, s, fc)
	s.HandleText(`{"t":"delete","tx":"t","path":"","id":"x","type":"md","ifEtag":"e3"}`)
	s.HandleText(`{"t":"commit","tx":"t"}`)
	s.HandleText(`{"t":"abort","tx":"t"}`)
	s.HandleText(`{"t":"resolve","server":"Lp7tOp0123ab","keep":"subordinate"}`)
	expectMessages(t, barrier(t, s, fc)[1:],
		obj{"t": "staged", "path": "", "id": "x", "type": "md", "received": float64(0)},
		obj{"t": "committed", "revision": "r2", "docs": []any{
			obj{"path": "notes", "id": "n1", "type": "json", "etag": "e2"},
			obj{"path": "", "id": "x", "type": "md", "etag": nil},
		}},
		obj{"t": "aborted", "tx": "t"},
		obj{"t": "resolved", "server": "Lp7tOp0123ab"},
	)
	if d := b.deletes[0]; d.key != (Key{ID: "x", Type: "md"}) || *d.ifEtag != "e3" || d.tx != "t" {
		t.Errorf("backend Delete got %+v", d)
	}
	if b.resolves[0] != [4]string{"todo", "d1", "Lp7tOp0123ab", KeepSubordinate} {
		t.Errorf("backend Resolve got %v", b.resolves)
	}

	b.mu.Lock()
	b.conflict = &Conflict{Revision: ptr("r3"), Docs: []DocEtag{{Key: docKey}}}
	b.mu.Unlock()
	s.HandleText(`{"t":"commit","tx":"t"}`)
	if f := lastText(t, s, fc); f["t"] != "conflict" || f["revision"] != "r3" {
		t.Errorf("sent %v, want a conflict", f)
	}
	b.mu.Lock()
	b.commitErr = &Error{CodeIncomplete, "a body is missing"}
	b.mu.Unlock()
	s.HandleText(`{"t":"commit","tx":"t"}`)
	expectError(t, lastText(t, s, fc), CodeIncomplete)
	if fc.isClosed() {
		t.Error("channel closed")
	}
}

func TestResolveRefusals(t *testing.T) {
	for _, frame := range []string{
		`{"t":"resolve","server":"Lp7tOp0123ab","keep":"both"}`,
		`{"t":"resolve","keep":"main"}`,
		`{"t":"resolve","server":{"serverId":"x"},"keep":"main"}`,
	} {
		t.Run(frame, func(t *testing.T) {
			b := newFakeBackend()
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
			if b.count("resolve") != 0 {
				t.Error("the backend was asked")
			}
		})
	}
}

func TestDocumentFramesBeforeHelloAndOnALinkSession(t *testing.T) {
	frames := []string{
		`{"t":"list"}`, getFrame, `{"t":"begin","tx":"t"}`,
		`{"t":"delete","tx":"t","path":"","id":"x","type":"md","ifEtag":null}`,
		`{"t":"commit","tx":"t"}`, `{"t":"abort","tx":"t"}`, `{"t":"resolve","server":"S","keep":"main"}`,
	}
	t.Run("before hello", func(t *testing.T) {
		b := newFakeBackend()
		s, fc := newSession(t, b, Options{})
		for i, frame := range frames {
			s.HandleText(frame)
			expectError(t, fc.waitTexts(t, i+1)[i], CodeNotAuthenticated)
		}
		if calls := b.callLog(); len(calls) != 0 {
			t.Errorf("backend was asked: %v", calls)
		}
	})
	for _, frame := range frames {
		t.Run("link session "+frame, func(t *testing.T) {
			b := newFakeBackend()
			s, fc := newSession(t, b, Options{})
			s.HandleText(linkHello)
			s.HandleText(frame)
			expectClosedWithError(t, fc, CodeProtocol)
			for _, c := range b.callLog() {
				if c != "hello" && c != "hello returned" && c != "closed" {
					t.Errorf("backend calls %v", b.callLog())
				}
			}
		})
	}
}

func TestChangedToAnEmptySetDuringHello(t *testing.T) {
	b := newFakeBackend()
	b.welcome.Revision = ptr("r1")
	b.onHello = func(s *Session) { s.PushChanged(nil) }
	s, fc := newSession(t, b, Options{})
	s.HandleText(helloFrame)
	msgs := barrier(t, s, fc)
	if len(msgs) != 2 || msgs[1].text["t"] != "changed" || msgs[1].text["revision"] != nil {
		t.Errorf("sent %s, want the welcome and changed to null", describe(msgs))
	}

	b = newFakeBackend()
	b.onHello = func(s *Session) { s.PushChanged(nil) } // the welcome says null too
	s, fc = newSession(t, b, Options{})
	s.HandleText(helloFrame)
	if msgs := barrier(t, s, fc); len(msgs) != 1 {
		t.Errorf("sent %s, want the welcome only", describe(msgs))
	}
}

func TestDivergedAndResolvedPushes(t *testing.T) {
	list := []DivergedServer{{ServerID: "Lp7tOp0123ab", Name: "Laptop", Since: "2026-11-13T09:00:00Z"}}
	b := newFakeBackend()
	b.welcome.Diverged = list
	s, fc := newSession(t, b, Options{})
	s.PushDiverged(list) // before the welcome: the welcome says it
	s.HandleText(helloFrame)
	s.PushDiverged(list)
	s.PushResolved("Lp7tOp0123ab")
	server := obj{"serverId": "Lp7tOp0123ab", "name": "Laptop", "since": "2026-11-13T09:00:00Z"}
	expectMessages(t, barrier(t, s, fc),
		obj{
			"t": "welcome", "proto": float64(1), "serverId": "S", "name": "Desk", "revision": nil,
			"hosts": []any{"u.local", "192.168.1.5"}, "diverged": []any{server},
		},
		obj{"t": "diverged", "servers": []any{server}},
		obj{"t": "resolved", "server": "Lp7tOp0123ab"},
	)
	s.Close()
	s.PushDiverged(list)
	s.PushResolved("x")
	time.Sleep(10 * time.Millisecond)
	expectNoLateSends(t, fc)
}
