package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxBuffered is the buffered amount above which the sender pauses.
	maxBuffered = 1 << 20
	// lowWater is the buffered amount at which a paused sender resumes.
	lowWater = 256 << 10
	// maxQueuedText is how many bytes of text frames may wait to be sent. A
	// session that queues more is not reading what it asks for, and is
	// closed.
	maxQueuedText = 1 << 20
	// maxQueuedStreams is how many document bodies may wait to be sent,
	// besides the one being sent. Each holds an open file, never its bytes.
	maxQueuedStreams = 4
)

// Session is one data channel speaking the sync protocol. Incoming messages
// go to HandleText and HandleBinary; replies and pushes are queued and sent in
// order by the session's own sender goroutine, so no caller ever waits for the
// channel's buffer to drain.
type Session struct {
	ch      Channel
	remote  string
	b       Backend
	onClose func()

	wake       chan struct{} // something was queued
	low        chan struct{} // the channel's buffer drained to lowWater
	done       chan struct{} // closed by Close
	senderDone chan struct{} // closed when the sender goroutine returns

	inMu sync.Mutex // serializes incoming messages
	put  putState   // guarded by inMu

	mu            sync.Mutex
	timer         *time.Timer
	queue         []outgoing
	queuedText    int64 // bytes of text in queue
	queuedStreams int   // bodies in queue
	authenticated bool
	app, device   string // app is empty on a link session
	// holding and held are the latest changed push that arrived before the
	// welcome: held is its revision, which may be nil.
	holding  bool
	held     *string
	heldApps *[]AppInfo // the latest app list pushed before the welcome
	// sink is the put body being received, which the session closes when it
	// ends under it, so that what arrived is kept for a resume.
	sink Sink
	// unconfirmed is set by a successful pair until the client sends its next
	// frame, which shows that it got its token. A session that ends before
	// that undoes the pairing: the client may never have stored the token.
	unconfirmed bool
	closing     bool // a final frame is queued; the sender closes after it
	closed      bool
}

// putState is the put whose body is arriving.
type putState struct {
	active    bool
	key       Key
	remaining int64 // bytes of the body still to arrive
	received  int64 // what staged reports: the offset, plus what was written
	sink      Sink  // nil when the put was refused: the body is discarded
	refusal   *Error
}

// outgoing is one queued item: a text frame and the body that follows it, or
// the order to close once everything before it has been sent. A body is read
// from its file as it is sent, n bytes, and then closed.
type outgoing struct {
	text  string
	body  io.ReadCloser
	n     int64
	close bool
}

// closeBody closes o's body, if it has one: an item that will not be sent.
func (o outgoing) closeBody() {
	if o.body != nil {
		_ = o.body.Close()
	}
}

// NewSession starts a session over ch, the channel from remote. It sets the
// channel's low-buffer threshold and callback, and starts the auth deadline.
func NewSession(ch Channel, remote string, b Backend, opt Options) *Session {
	s := &Session{
		ch:         ch,
		remote:     remote,
		b:          b,
		onClose:    opt.OnClose,
		wake:       make(chan struct{}, 1),
		low:        make(chan struct{}, 1),
		done:       make(chan struct{}),
		senderDone: make(chan struct{}),
	}
	ch.SetBufferedAmountLowThreshold(lowWater)
	ch.OnBufferedAmountLow(func() { signal(s.low) })
	if opt.AuthTimeout > 0 {
		s.mu.Lock()
		s.timer = time.AfterFunc(opt.AuthTimeout, s.authExpired)
		s.mu.Unlock()
	}
	go s.sendLoop()
	return s
}

// textFrame is every field a client frame may carry. server is a server's
// connection data in announce and term, and a server id in resolve, so it is
// decoded by the frame that reads it; revision is kept raw, so that a list
// can tell null from absent.
type textFrame struct {
	T        string          `json:"t"`
	Proto    *int            `json:"proto"`
	App      string          `json:"app"`
	Device   string          `json:"device"`
	Token    string          `json:"token"`
	Secret   string          `json:"secret"`
	Name     string          `json:"name"`
	Kind     *string         `json:"kind"`
	Server   json.RawMessage `json:"server"`
	Term     *int            `json:"term"`
	Devices  *[]DeviceEntry  `json:"devices"`
	Removed  []Tombstone     `json:"removed"`
	Revision json.RawMessage `json:"revision"`
	Cursor   *string         `json:"cursor"`
	Path     string          `json:"path"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Etag     *string         `json:"etag"`
	IfEtag   *string         `json:"ifEtag"`
	Bytes    *int64          `json:"bytes"`
	Offset   *int64          `json:"offset"`
	Tx       string          `json:"tx"`
	Keep     string          `json:"keep"`
}

func (f *textFrame) key() Key { return Key{Path: f.Path, ID: f.ID, Type: f.Type} }

// serverInfo decodes server as a server's connection data; nil when it is
// absent or null.
func (f *textFrame) serverInfo() (*ServerInfo, error) {
	if len(f.Server) == 0 {
		return nil, nil
	}
	var si *ServerInfo
	err := json.Unmarshal(f.Server, &si)
	return si, err
}

// txFrames are the frames that name a transaction, whose id the session
// checks before the backend sees it.
var txFrames = map[string]bool{"begin": true, "put": true, "delete": true, "commit": true, "abort": true}

// ValidTx reports whether tx is a valid transaction id: 16 to 64 characters
// of [A-Za-z0-9_-], chosen by the client (store.ValidTx, which the protocol
// does not import).
func ValidTx(tx string) bool {
	if len(tx) < 16 || len(tx) > 64 {
		return false
	}
	for i := 0; i < len(tx); i++ {
		if c := tx[i]; !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// documentFrames are the frames of an app session's documents, which a link
// session does not have.
var documentFrames = map[string]bool{
	"head": true, "list": true, "get": true, "begin": true, "put": true,
	"delete": true, "commit": true, "abort": true, "resolve": true,
}

// HandleText handles one incoming text message. Incoming messages are handled
// one at a time, in the order the calls take the session's lock; a framing
// violation queues a protocol error and closes the channel after it.
func (s *Session) HandleText(msg string) {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	if !s.confirm() {
		return
	}
	if s.put.active {
		s.fail(CodeProtocol, "text frame before the put body was complete")
		return
	}
	var f textFrame
	if err := json.Unmarshal([]byte(msg), &f); err != nil {
		s.fail(CodeProtocol, "malformed text frame: "+err.Error())
		return
	}
	switch f.T {
	case "hello", "pair", "forget", "apps", "announce", "invite", "term":
	default:
		if !documentFrames[f.T] {
			s.fail(CodeProtocol, fmt.Sprintf("unknown frame type %q", f.T))
			return
		}
	}

	authenticated, app, device := s.identity()
	if f.T == "hello" || f.T == "pair" {
		switch {
		case authenticated:
			s.fail(CodeProtocol, "the session is already authenticated")
		case f.Proto == nil || *f.Proto != Version:
			s.fail(CodeProtocol, fmt.Sprintf("unsupported protocol version; this server speaks proto %d", Version))
		case f.T == "hello":
			s.hello(f.App, f.Device, f.Token)
		default:
			s.pair(f.Kind, f.App, f.Secret, f.Name)
		}
		return
	}
	if !authenticated {
		s.reply(&Error{CodeNotAuthenticated, "send hello or pair first"})
		return
	}
	if app == "" && documentFrames[f.T] {
		s.fail(CodeProtocol, "a link session has no documents; say hello for an app")
		return
	}
	if txFrames[f.T] && !ValidTx(f.Tx) {
		s.fail(CodeProtocol, "a transaction id is 16 to 64 of [A-Za-z0-9_-]")
		return
	}

	switch f.T {
	case "head":
		rev, e := s.b.Head(app)
		if e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(revisionFrame{"head", rev})
	case "list":
		s.list(app, &f)
	case "get":
		s.get(app, &f)
	case "begin":
		staged, e := s.b.Begin(app, device, f.Tx)
		if e != nil {
			s.answer(e)
			return
		}
		if staged == nil {
			staged = []Staged{}
		}
		s.queueFrame(beganFrame{"began", f.Tx, staged})
	case "put":
		s.startPut(app, device, &f)
	case "delete":
		k := f.key()
		if e := s.b.Delete(app, device, f.Tx, k, f.IfEtag); e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(stagedFrame{"staged", k, 0})
	case "commit":
		// The backend answers itself (AnswerCommitted, AnswerConflict).
		if e := s.b.Commit(s, app, device, f.Tx); e != nil {
			s.answer(e)
		}
	case "abort":
		if e := s.b.Abort(app, device, f.Tx); e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(abortedFrame{"aborted", f.Tx})
	case "resolve":
		var server string
		if json.Unmarshal(f.Server, &server) != nil || server == "" || f.Keep != KeepMain && f.Keep != KeepSubordinate {
			s.fail(CodeProtocol, `resolve needs a server id and keep "main" or "subordinate"`)
			return
		}
		if e := s.b.Resolve(s, app, device, server, f.Keep); e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(resolvedFrame{"resolved", server})
	case "forget":
		s.b.Forget(app, device)
		s.closeAfter()
	case "apps":
		list, e := s.b.Apps(s)
		if e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(appsFrame{"apps", appList(list)})
	case "announce":
		si, err := f.serverInfo()
		switch {
		case app != "":
			s.fail(CodeProtocol, "only a link session may announce a server")
		case err != nil || si == nil:
			s.fail(CodeProtocol, "announce needs a server")
		default:
			// The backend answers with PushDevices itself, so that the answer
			// and the pushes leave in the order the registry changed.
			var handover *Registry
			if f.Devices != nil {
				handover = &Registry{Devices: *f.Devices, Removed: f.Removed}
			}
			if e := s.b.Announce(s, *si, handover); e != nil {
				s.answer(e)
			}
		}
	case "invite":
		if app != "" {
			s.fail(CodeProtocol, "only a link session may ask for a pairing code")
			return
		}
		code, expires, e := s.b.Invite(s)
		if e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(invitedFrame{"invited", code, expires.UTC().Format(time.RFC3339)})
	case "term":
		si, err := f.serverInfo()
		switch {
		case app != "":
			s.fail(CodeProtocol, "only a link session may tell a term")
			return
		case f.Term == nil || *f.Term < 0 || err != nil || si == nil:
			s.fail(CodeProtocol, "term needs a term of 0 or more and a server")
			return
		}
		a, e := s.b.Term(s, *f.Term, *si, f.Device, f.Token)
		if e != nil {
			s.answer(e)
			return
		}
		s.queueFrame(termFrame{"term", a.Term, a.Main})
	}
}

// list answers a list frame with one page of the app's set. A list that
// names a revision is answered only at that revision, and with stale
// otherwise; a list with a cursor (the next page) must name one, the
// revision of the pages before it.
func (s *Session) list(app string, f *textFrame) {
	pinned := len(f.Revision) > 0
	var want *string
	if pinned && json.Unmarshal(f.Revision, &want) != nil {
		s.fail(CodeProtocol, "a list's revision is a string or null")
		return
	}
	if f.Cursor != nil && !pinned {
		s.fail(CodeProtocol, "a list with a cursor names the revision of the pages before it")
		return
	}
	l, e := s.b.List(app)
	if e != nil {
		s.answer(e)
		return
	}
	if pinned && !sameRevision(l.Revision, want) {
		s.queueFrame(revisionFrame{"stale", l.Revision})
		return
	}
	docs, removed, next, ok := page(l, f.Cursor)
	if !ok {
		s.fail(CodeProtocol, "a list's cursor is one the server gave")
		return
	}
	s.queueFrame(listFrame{"list", l.Revision, docs, removed, next})
}

// Cursors name the last entry of the page before: "d:<key>" a document,
// "r:<key>" a tombstone.
const (
	cursorDoc     = "d:"
	cursorRemoved = "r:"
)

// page returns the page of l that follows cursor (the first page when it is
// nil): the documents first, then the tombstones, at most listPage entries,
// and the cursor of the next page, empty after the last. ok is false for a
// cursor the server never gave: one that does not name an entry of l, which
// the revision the client names pins.
func page(l Listing, cursor *string) (docs []DocInfo, removed []Removed, next string, ok bool) {
	d, r := 0, 0
	if cursor != nil {
		c := *cursor
		switch {
		case strings.HasPrefix(c, cursorDoc):
			key := c[len(cursorDoc):]
			i := sort.Search(len(l.Docs), func(i int) bool { return l.Docs[i].Key.String() >= key })
			if i == len(l.Docs) || l.Docs[i].Key.String() != key {
				return nil, nil, "", false
			}
			d = i + 1
		case strings.HasPrefix(c, cursorRemoved):
			key := c[len(cursorRemoved):]
			i := sort.Search(len(l.Removed), func(i int) bool { return l.Removed[i].Key.String() >= key })
			if i == len(l.Removed) || l.Removed[i].Key.String() != key {
				return nil, nil, "", false
			}
			d, r = len(l.Docs), i+1
		default:
			return nil, nil, "", false
		}
	}
	docs = l.Docs[d:min(len(l.Docs), d+listPage)]
	room := listPage - len(docs)
	removed = l.Removed[r:min(len(l.Removed), r+room)]
	if d+len(docs) < len(l.Docs) || r+len(removed) < len(l.Removed) {
		if len(removed) > 0 {
			next = cursorRemoved + removed[len(removed)-1].Key.String()
		} else {
			next = cursorDoc + docs[len(docs)-1].Key.String()
		}
	}
	return slicesOr(docs), slicesOr(removed), next, true
}

// slicesOr returns list, never nil, so that an empty list is [] on the wire.
func slicesOr[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// get answers a get frame with the document and its body from the offset
// asked for. An offset past 0 needs the etag of the body it resumes.
func (s *Session) get(app string, f *textFrame) {
	var offset int64
	if f.Offset != nil {
		offset = *f.Offset
	}
	if offset < 0 || offset > 0 && f.Etag == nil {
		s.fail(CodeProtocol, "a get's offset is 0 or more, and past 0 only with the etag it resumes")
		return
	}
	k := f.key()
	doc, stale, e := s.b.Get(app, k, f.Etag)
	switch {
	case e != nil:
		s.answer(e)
		return
	case stale != nil:
		s.queueFrame(revisionFrame{"stale", stale.Revision})
		return
	}
	if offset > doc.Bytes {
		_ = doc.Body.Close()
		s.fail(CodeProtocol, fmt.Sprintf("offset %d is past the document's %d bytes", offset, doc.Bytes))
		return
	}
	if offset > 0 {
		if _, err := doc.Body.Seek(offset, io.SeekStart); err != nil {
			_ = doc.Body.Close()
			s.reply(&Error{CodeInternal, "the document could not be read"})
			return
		}
	}
	frame := encode(docFrame{"doc", doc.Key, doc.Etag, doc.Bytes, offset})
	if doc.Bytes == offset {
		_ = doc.Body.Close()
		s.queue1(outgoing{text: frame})
		return
	}
	s.queue1(outgoing{text: frame, body: doc.Body, n: doc.Bytes - offset})
}

// startPut handles a put frame: the backend stages the body, and the binary
// messages that follow go into the Sink it gives. A put the backend refuses
// is answered once its body has arrived, which the session discards; a
// protocol error closes the session at once.
func (s *Session) startPut(app, device string, f *textFrame) {
	if f.Bytes == nil || *f.Bytes < 0 {
		s.fail(CodeProtocol, "put needs a non-negative bytes field")
		return
	}
	size := *f.Bytes
	var offset int64
	if f.Offset != nil {
		offset = *f.Offset
	}
	if offset < 0 || offset > size {
		s.fail(CodeProtocol, "a put's offset is between 0 and its bytes")
		return
	}
	k := f.key()
	// A missing ifEtag reads as null: a put is always conditional.
	sink, e := s.b.Put(app, device, f.Tx, k, f.IfEtag, size, offset)
	if e != nil && e.Code == CodeProtocol {
		s.fail(e.Code, e.Message)
		return
	}
	if e == nil && sink == nil {
		e = &Error{CodeInternal, "the body could not be staged"}
	}
	s.put = putState{active: true, key: k, remaining: size - offset, received: offset, refusal: e}
	if e == nil {
		s.put.sink = sink
		// A session that closed while the backend was deciding has run
		// finishClose already, which closes only the Sink it finds: this one
		// is closed here, so that what it took stays staged.
		s.mu.Lock()
		closed := s.closed
		if !closed {
			s.sink = sink
		}
		s.mu.Unlock()
		if closed {
			_ = sink.Close()
		}
	}
	if s.put.remaining == 0 {
		s.finishPut()
	}
}

// HandleBinary handles one incoming binary message: a piece of a put body,
// which goes into the put's Sink at once, before the next message is read.
func (s *Session) HandleBinary(data []byte) {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	if !s.confirm() {
		return
	}
	if !s.put.active {
		s.fail(CodeProtocol, "binary message when no body is expected")
		return
	}
	if int64(len(data)) > s.put.remaining {
		s.fail(CodeProtocol, "binary message overruns the announced body")
		return
	}
	s.put.remaining -= int64(len(data))
	if s.put.refusal == nil {
		n, err := s.put.sink.Write(data)
		s.put.received += int64(n)
		if err != nil {
			s.put.refusal = errorOf(err)
			s.endSink()
		}
	}
	if s.put.remaining == 0 {
		s.finishPut()
	}
}

// finishPut answers a put whose announced body has fully arrived.
func (s *Session) finishPut() {
	if s.put.sink != nil {
		if err := s.endSink(); err != nil && s.put.refusal == nil {
			s.put.refusal = errorOf(err)
		}
	}
	p := s.put
	s.put = putState{}
	if p.refusal != nil {
		s.answer(p.refusal)
		return
	}
	s.queueFrame(stagedFrame{"staged", p.key, p.received})
}

// endSink closes the put's Sink, which the session then no longer closes
// when it ends.
func (s *Session) endSink() error {
	sink := s.put.sink
	s.put.sink = nil
	s.mu.Lock()
	if s.sink == sink {
		s.sink = nil
	}
	s.mu.Unlock()
	return sink.Close()
}

// errorOf is the answer to err, a Sink's error.
func errorOf(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{CodeInternal, "the body could not be stored"}
}

func (s *Session) hello(app, device, token string) {
	w, e := s.b.Hello(s, app, device, token)
	if e != nil {
		s.fail(e.Code, e.Message)
		return
	}
	s.authenticate(app, device, w, nil)
}

// pair pairs a device of the kind given, a device when kind is absent. A
// server-kind pair names no app: whatever app it carries is dropped.
func (s *Session) pair(kind *string, app, secret, name string) {
	k := KindDevice
	if kind != nil {
		k = *kind
	}
	switch k {
	case KindDevice:
	case KindServer:
		app = ""
	default:
		s.fail(CodeProtocol, fmt.Sprintf("unknown pairing kind %q", k))
		return
	}
	device, token, w, e := s.b.Pair(s, k, app, secret, name)
	if e != nil {
		if e.Code == CodeUnknownApp {
			s.reply(e)
		} else {
			s.fail(e.Code, e.Message)
		}
		return
	}
	s.authenticate(app, device, w, &pairedFrame{"paired", device, token})
}

// authenticate completes a successful Hello or Pair: it queues paired (when
// given), the welcome, and then the pushes held since before the welcome. If
// the session closed while the backend was deciding, it sends nothing and
// tells the backend again, so that a registration made in the call does not
// linger, and undoes a pairing: the client never got its token.
func (s *Session) authenticate(app, device string, w Welcome, paired *pairedFrame) {
	hosts := w.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	welcome := welcomeFrame{"welcome", Version, w.Role, w.Term, w.ServerID, w.Name, w.Revision, hosts, nil, nil}
	if w.Subordinates != nil {
		list := serverList(w.Subordinates)
		welcome.Subordinates = &list
	}
	if len(w.Diverged) > 0 {
		welcome.Diverged = w.Diverged
	}

	s.mu.Lock()
	if s.closed || s.closing {
		s.mu.Unlock()
		s.b.Closed(s)
		if paired != nil {
			s.b.Forget(app, device)
		}
		return
	}
	s.authenticated, s.app, s.device = true, app, device
	if s.timer != nil {
		s.timer.Stop()
	}
	if paired != nil {
		s.unconfirmed = true
		s.queueLocked(outgoing{text: encode(paired)})
	}
	s.queueLocked(outgoing{text: encode(welcome)})
	if s.holding && !sameRevision(s.held, w.Revision) {
		s.queueLocked(outgoing{text: encode(revisionFrame{"changed", s.held})})
	}
	if list := s.heldApps; list != nil {
		s.queueLocked(outgoing{text: encode(appsFrame{"apps", appList(*list)})})
	}
	s.holding, s.held, s.heldApps = false, nil, nil
	s.mu.Unlock()
}

// sameRevision reports whether two revisions are equal, nil (an empty set)
// included.
func sameRevision(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// PushChanged tells the client that the app's set now has revision, nil for
// an empty set. It never blocks. Before the welcome has been queued (for
// instance when called from inside the backend's Hello or Pair) the push is
// held, only the latest kept, and sent right after the welcome unless the
// welcome carries the same revision. On a closed session it does nothing.
func (s *Session) PushChanged(revision *string) {
	if revision != nil {
		r := *revision
		revision = &r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed || s.closing:
	case !s.authenticated:
		s.holding, s.held = true, revision
	default:
		s.queueLocked(outgoing{text: encode(revisionFrame{"changed", revision})})
	}
}

// AnswerCommitted queues committed, the answer to the session's commit. The
// backend calls it from inside its Commit, under the lock that orders its
// pushes of changed, so that no changed of an older revision follows it. It
// never blocks; on a closed session it does nothing.
func (s *Session) AnswerCommitted(c Committed) {
	s.queueFrame(txResultFrame{"committed", c.Revision, docEtags(c.Docs)})
}

// AnswerConflict queues conflict, the answer to the session's commit, as
// AnswerCommitted does.
func (s *Session) AnswerConflict(c Conflict) {
	s.queueFrame(txResultFrame{"conflict", c.Revision, docEtags(c.Docs)})
}

// PushDiverged tells the client the servers whose copy of the app diverged
// from their main's, which wait for a device's choice. It never blocks.
// Before the welcome it does nothing: the welcome lists them. On a closed
// session it does nothing either.
func (s *Session) PushDiverged(list []DivergedServer) {
	list = slicesOr(list)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || !s.authenticated {
		return
	}
	s.queueLocked(outgoing{text: encode(divergedFrame{"diverged", list})})
}

// PushResolved tells the client that the divergence of the server server on
// the app has been settled. It never blocks. On a session that is closed or
// not authenticated it does nothing.
func (s *Session) PushResolved(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || !s.authenticated {
		return
	}
	s.queueLocked(outgoing{text: encode(resolvedFrame{"resolved", server})})
}

// PushApps tells the client the server's app list. It never blocks. Before
// the welcome has been queued the list is held, only the latest kept, and
// sent right after the welcome. On a closed session it does nothing.
func (s *Session) PushApps(list []AppInfo) {
	list = appList(list)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed || s.closing:
	case !s.authenticated:
		s.heldApps = &list
	default:
		s.queueLocked(outgoing{text: encode(appsFrame{"apps", list})})
	}
}

// PushDevices tells the client the device registry: the answer to its
// announce, and again after every change. It never blocks. On a session
// that is closed or not authenticated it does nothing: only a link session
// that announced is told the registry.
func (s *Session) PushDevices(reg Registry) {
	list := reg.Devices
	if list == nil {
		list = []DeviceEntry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || !s.authenticated {
		return
	}
	s.queueLocked(outgoing{text: encode(devicesFrame{"devices", list, reg.Removed})})
}

// PushTakeOver asks the client, a subordinate on its link session, to
// become the main of the family. It never blocks. On a session that is
// closed or not authenticated it does nothing.
func (s *Session) PushTakeOver() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || !s.authenticated {
		return
	}
	s.queueLocked(outgoing{text: encode(takeOverFrame{"takeOver"})})
}

// App returns the app the session authenticated for; empty until then,
// including during the backend's Hello or Pair, and on a link session.
func (s *Session) App() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.app
}

// Device returns the device the session authenticated as; empty until then,
// including during the backend's Hello or Pair.
func (s *Session) Device() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.device
}

// Remote returns the peer's address, as given to NewSession.
func (s *Session) Remote() string { return s.remote }

// Authenticated reports whether a hello or pair has succeeded.
func (s *Session) Authenticated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authenticated
}

// Close closes the session at once, dropping whatever is still queued. The
// first call closes the channel, then closes a put body still arriving (what
// arrived stays staged), then tells the backend, then runs OnClose; later
// calls do nothing. It is safe from any goroutine, including from inside
// HandleText and the backend's methods.
func (s *Session) Close() {
	s.mu.Lock()
	first := s.markClosedLocked()
	s.mu.Unlock()
	if first {
		s.finishClose()
	}
}

// authExpired closes the session unless it has authenticated.
func (s *Session) authExpired() {
	s.mu.Lock()
	first := !s.authenticated && s.markClosedLocked()
	s.mu.Unlock()
	if first {
		s.finishClose()
	}
}

// markClosedLocked marks the session closed and stops the sender and the
// timer; the bodies still queued are closed. It reports whether this was the
// first close.
func (s *Session) markClosedLocked() bool {
	if s.closed {
		return false
	}
	s.closed = true
	for _, o := range s.queue {
		o.closeBody()
	}
	s.queue = nil
	s.queuedText, s.queuedStreams = 0, 0
	s.holding, s.held, s.heldApps = false, nil, nil
	close(s.done)
	if s.timer != nil {
		s.timer.Stop()
	}
	return true
}

func (s *Session) finishClose() {
	_ = s.ch.Close()
	s.mu.Lock()
	undo, app, device := s.unconfirmed, s.app, s.device
	s.unconfirmed = false
	sink := s.sink
	s.sink = nil
	s.mu.Unlock()
	if sink != nil {
		// What arrived stays staged, synced, for a put that resumes it.
		_ = sink.Close()
	}
	if undo {
		s.b.Forget(app, device)
	}
	s.b.Closed(s)
	if s.onClose != nil {
		s.onClose()
	}
}

// identity returns the authentication state under the lock.
func (s *Session) identity() (authenticated bool, app, device string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authenticated, s.app, s.device
}

// confirm is called for every incoming message. It reports false when the
// session is closed or about to close, in which case the message is ignored.
// Otherwise the message confirms a pairing: the client sent it after paired.
func (s *Session) confirm() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing {
		return false
	}
	s.unconfirmed = false
	return true
}

// answer answers a backend's refusal: a protocol error closes the session,
// any other is a reply.
func (s *Session) answer(e *Error) {
	if e.Code == CodeProtocol {
		s.fail(e.Code, e.Message)
		return
	}
	s.reply(e)
}

// reply queues an error frame; the channel stays open.
func (s *Session) reply(e *Error) {
	s.queueFrame(errorFrame{"error", e.Code, e.Message})
}

// fail queues an error frame and closes the channel after it.
func (s *Session) fail(code, message string) {
	s.closeAfter(errorFrame{"error", code, message})
}

// closeAfter queues the frames given and then the order to close, so the
// channel closes once everything queued so far has been sent. Nothing more is
// queued or handled after it.
func (s *Session) closeAfter(frames ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing {
		return
	}
	for _, f := range frames {
		s.queueLocked(outgoing{text: encode(f)})
	}
	s.queueLocked(outgoing{close: true})
	s.closing = true
}

// queueFrame queues a text frame.
func (s *Session) queueFrame(frame any) {
	s.queue1(outgoing{text: encode(frame)})
}

// queue1 queues o, unless the session is closing; then o's body is closed.
func (s *Session) queue1(o outgoing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing {
		o.closeBody()
		return
	}
	s.queueLocked(o)
}

// queueLocked queues o. A queue that would hold more than maxQueuedText bytes
// of text, or more than maxQueuedStreams bodies, closes the session instead:
// its client asks for more than it reads. A text frame that finds the queue
// empty is always taken.
func (s *Session) queueLocked(o outgoing) {
	if s.closed {
		o.closeBody()
		return
	}
	text := int64(len(o.text))
	if len(s.queue) > 0 && s.queuedText+text > maxQueuedText || o.body != nil && s.queuedStreams >= maxQueuedStreams {
		o.closeBody()
		// The caller may hold the backend's locks (PushChanged does), and
		// closing tells the backend, so the close runs on its own.
		if s.markClosedLocked() {
			go s.finishClose()
		}
		return
	}
	s.queue = append(s.queue, o)
	s.queuedText += text
	if o.body != nil {
		s.queuedStreams++
	}
	signal(s.wake)
}

// sendLoop is the sender goroutine: it sends the queue in order until the
// session closes. A body is read from its file one chunk at a time, into one
// buffer the channel copies from.
func (s *Session) sendLoop() {
	defer close(s.senderDone)
	var buf []byte
	for {
		o, ok := s.next()
		if !ok {
			return
		}
		if o.close {
			s.Close()
			return
		}
		if !s.write(func() error { return s.ch.SendText(o.text) }) {
			o.closeBody()
			return
		}
		if o.body == nil {
			continue
		}
		if buf == nil {
			buf = make([]byte, ChunkBytes)
		}
		ok = s.stream(o.body, o.n, buf)
		o.closeBody()
		if !ok {
			return
		}
	}
}

// stream sends n bytes of body as binary messages of at most len(buf) bytes.
// A body that cannot be read to its announced end closes the session: the
// client could not tell the frames that follow from the body. It reports
// whether the sender should go on.
func (s *Session) stream(body io.Reader, n int64, buf []byte) bool {
	for left := n; left > 0; {
		chunk := buf[:min(int64(len(buf)), left)]
		if _, err := io.ReadFull(body, chunk); err != nil {
			s.Close()
			return false
		}
		if !s.write(func() error { return s.ch.Send(chunk) }) {
			return false
		}
		left -= int64(len(chunk))
	}
	return true
}

// next waits for the next queued item. It reports false once the session is
// closed.
func (s *Session) next() (outgoing, bool) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return outgoing{}, false
		}
		if len(s.queue) > 0 {
			o := s.queue[0]
			s.queue[0] = outgoing{}
			s.queue = s.queue[1:]
			s.queuedText -= int64(len(o.text))
			if o.body != nil {
				s.queuedStreams--
			}
			s.mu.Unlock()
			return o, true
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.done:
			return outgoing{}, false
		}
	}
}

// write sends one message once the channel's buffer allows it. A send error
// closes the session. It reports whether the sender should go on.
func (s *Session) write(send func() error) bool {
	if s.ch.BufferedAmount() > maxBuffered {
		for s.ch.BufferedAmount() > lowWater {
			select {
			case <-s.low:
			case <-s.done:
				return false
			}
		}
	}
	select {
	case <-s.done:
		return false
	default:
	}
	if err := send(); err != nil {
		s.Close()
		return false
	}
	return true
}

// signal wakes whoever waits on c without ever blocking.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func encode(frame any) string {
	b, err := json.Marshal(frame)
	if err != nil {
		// Frames are structs of strings, numbers and string slices.
		panic("protocol: encode frame: " + err.Error())
	}
	return string(b)
}

// docEtags returns list, never nil.
func docEtags(list []DocEtag) []DocEtag { return slicesOr(list) }

// The frames the server sends. A revision or an etag that may be missing is
// a string or null on the wire.
type (
	// revisionFrame is head, changed and stale.
	revisionFrame struct {
		T        string  `json:"t"`
		Revision *string `json:"revision"`
	}
	listFrame struct {
		T        string    `json:"t"`
		Revision *string   `json:"revision"`
		Docs     []DocInfo `json:"docs"`
		Removed  []Removed `json:"removed"`
		Cursor   string    `json:"cursor,omitempty"`
	}
	docFrame struct {
		T string `json:"t"`
		Key
		Etag   string `json:"etag"`
		Bytes  int64  `json:"bytes"`
		Offset int64  `json:"offset"`
	}
	beganFrame struct {
		T      string   `json:"t"`
		Tx     string   `json:"tx"`
		Staged []Staged `json:"staged"`
	}
	stagedFrame struct {
		T string `json:"t"`
		Key
		Received int64 `json:"received"`
	}
	// txResultFrame is committed and conflict.
	txResultFrame struct {
		T        string    `json:"t"`
		Revision *string   `json:"revision"`
		Docs     []DocEtag `json:"docs"`
	}
	abortedFrame struct {
		T  string `json:"t"`
		Tx string `json:"tx"`
	}
	resolvedFrame struct {
		T      string `json:"t"`
		Server string `json:"server"`
	}
	divergedFrame struct {
		T       string           `json:"t"`
		Servers []DivergedServer `json:"servers"`
	}
	pairedFrame struct {
		T      string `json:"t"`
		Device string `json:"device"`
		Token  string `json:"token"`
	}
	welcomeFrame struct {
		T            string           `json:"t"`
		Proto        int              `json:"proto"`
		Role         string           `json:"role,omitempty"`
		Term         int              `json:"term,omitempty"`
		ServerID     string           `json:"serverId"`
		Name         string           `json:"name"`
		Revision     *string          `json:"revision"`
		Hosts        []string         `json:"hosts"`
		Subordinates *[]ServerInfo    `json:"subordinates,omitempty"`
		Diverged     []DivergedServer `json:"diverged,omitempty"`
	}
	errorFrame struct {
		T       string `json:"t"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	appsFrame struct {
		T    string    `json:"t"`
		Apps []AppInfo `json:"apps"`
	}
	devicesFrame struct {
		T       string        `json:"t"`
		Devices []DeviceEntry `json:"devices"`
		Removed []Tombstone   `json:"removed,omitempty"`
	}
	// termFrame answers term: the term and the main the server follows.
	termFrame struct {
		T    string `json:"t"`
		Term int    `json:"term"`
		Main string `json:"main"`
	}
	takeOverFrame struct {
		T string `json:"t"`
	}
	// invitedFrame answers invite: a pairing code of this server and when
	// it expires, in RFC 3339 UTC.
	invitedFrame struct {
		T         string `json:"t"`
		Code      string `json:"code"`
		ExpiresAt string `json:"expiresAt"`
	}
)

// serverList is list with every entry's hosts never nil, so that the frame
// always carries arrays.
func serverList(list []ServerInfo) []ServerInfo {
	out := make([]ServerInfo, len(list))
	for i, si := range list {
		if si.Hosts == nil {
			si.Hosts = []string{}
		}
		out[i] = si
	}
	return out
}

// appList is list, never nil, so that an empty list is [] on the wire.
func appList(list []AppInfo) []AppInfo {
	if list == nil {
		return []AppInfo{}
	}
	return list
}
