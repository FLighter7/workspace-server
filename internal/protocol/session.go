package protocol

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const (
	// maxBuffered is the buffered amount above which the sender pauses.
	maxBuffered = 1 << 20
	// lowWater is the buffered amount at which a paused sender resumes.
	lowWater = 256 << 10
	// queueSlack is how many bytes of replies and pushes may wait to be sent,
	// beyond one document of the app's largest size. A session that queues
	// more is not reading what it asks for, and is closed.
	queueSlack = 1 << 20
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
	queued        int64 // bytes in queue
	queueLimit    int64 // the most queued may reach; see queueLocked
	authenticated bool
	app, device   string     // app is empty on a link session
	held          *string    // the latest push that arrived before the welcome
	heldApps      *[]AppInfo // the latest app list pushed before the welcome
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
	ifEtag    *string
	announced int64
	remaining int64
	body      []byte
	limit     int64 // when announced exceeds it, the body is counted, not kept
}

func (p *putState) tooLarge() bool { return p.announced > p.limit }

// outgoing is one queued item: a text frame and the body that follows it, or
// the order to close once everything before it has been sent.
type outgoing struct {
	text  string
	body  []byte
	close bool
}

func (o outgoing) size() int64 { return int64(len(o.text) + len(o.body)) }

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
		queueLimit: queueSlack,
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
	var f struct {
		T      string      `json:"t"`
		Proto  *int        `json:"proto"`
		App    string      `json:"app"`
		Device string      `json:"device"`
		Token  string      `json:"token"`
		Secret string      `json:"secret"`
		Name   string      `json:"name"`
		Kind   *string     `json:"kind"`
		IfEtag *string     `json:"ifEtag"`
		Bytes  *int64      `json:"bytes"`
		Server *ServerInfo `json:"server"`
	}
	if err := json.Unmarshal([]byte(msg), &f); err != nil {
		s.fail(CodeProtocol, "malformed text frame: "+err.Error())
		return
	}
	switch f.T {
	case "hello", "pair", "head", "get", "put", "forget", "apps", "announce", "invite":
	default:
		s.fail(CodeProtocol, fmt.Sprintf("unknown frame type %q", f.T))
		return
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
	if app == "" && (f.T == "head" || f.T == "get" || f.T == "put") {
		s.fail(CodeProtocol, "a link session has no document; say hello for an app")
		return
	}

	switch f.T {
	case "head":
		etag, e := s.b.Head(app)
		if e != nil {
			s.reply(e)
			return
		}
		s.queueFrame(etagFrame{"head", etag}, nil)
	case "get":
		doc, etag, e := s.b.Get(app)
		if e != nil {
			s.reply(e)
			return
		}
		s.queueFrame(docFrame{"doc", etag, len(doc)}, doc)
	case "put":
		if f.Bytes == nil || *f.Bytes < 0 {
			s.fail(CodeProtocol, "put needs a non-negative bytes field")
			return
		}
		// A missing ifEtag reads as null: a put is always conditional.
		s.put = putState{
			active:    true,
			ifEtag:    f.IfEtag,
			announced: *f.Bytes,
			remaining: *f.Bytes,
			limit:     s.b.MaxBytes(app),
		}
		if s.put.remaining == 0 {
			s.finishPut(app, device)
		}
	case "forget":
		s.b.Forget(app, device)
		s.closeAfter()
	case "apps":
		list, e := s.b.Apps(s)
		switch {
		case e == nil:
			s.queueFrame(appsFrame{"apps", appList(list)}, nil)
		case e.Code == CodeProtocol:
			s.fail(e.Code, e.Message)
		default:
			s.reply(e)
		}
	case "announce":
		switch {
		case app != "":
			s.fail(CodeProtocol, "only a link session may announce a server")
		case f.Server == nil:
			s.fail(CodeProtocol, "announce needs a server")
		default:
			// The backend answers with PushDevices itself, so that the answer
			// and the pushes leave in the order the registry changed.
			if e := s.b.Announce(s, *f.Server); e != nil {
				if e.Code == CodeProtocol {
					s.fail(e.Code, e.Message)
				} else {
					s.reply(e)
				}
			}
		}
	case "invite":
		if app != "" {
			s.fail(CodeProtocol, "only a link session may ask for a pairing code")
			return
		}
		code, expires, e := s.b.Invite(s)
		switch {
		case e == nil:
			s.queueFrame(invitedFrame{"invited", code, expires.UTC().Format(time.RFC3339)}, nil)
		case e.Code == CodeProtocol:
			s.fail(e.Code, e.Message)
		default:
			s.reply(e)
		}
	}
}

// HandleBinary handles one incoming binary message: a piece of a put body.
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
	if !s.put.tooLarge() {
		s.put.body = append(s.put.body, data...)
	}
	if s.put.remaining == 0 {
		_, app, device := s.identity()
		s.finishPut(app, device)
	}
}

// finishPut answers a put whose announced body has fully arrived.
func (s *Session) finishPut(app, device string) {
	p := s.put
	s.put = putState{}
	if p.tooLarge() {
		s.reply(&Error{CodeTooLarge, fmt.Sprintf("a body of %d bytes exceeds the app's limit of %d", p.announced, p.limit)})
		return
	}
	if p.body == nil {
		p.body = []byte{}
	}
	etag, conflict, current, e := s.b.Put(s, app, device, p.ifEtag, p.body)
	switch {
	case e != nil:
		s.reply(e)
	case conflict:
		s.queueFrame(etagFrame{"conflict", current}, nil)
	default:
		s.queueFrame(etagFrame{"stored", &etag}, nil)
	}
}

func (s *Session) hello(app, device, token string) {
	w, e := s.b.Hello(s, app, device, token)
	if e != nil {
		s.fail(e.Code, e.Message)
		return
	}
	s.authenticate(app, device, w, nil, s.b.MaxBytes(app))
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
	s.authenticate(app, device, w, &pairedFrame{"paired", device, token}, s.b.MaxBytes(app))
}

// authenticate completes a successful Hello or Pair: it queues paired (when
// given), the welcome, and then the push held since before the welcome, and
// lets the queue hold one document of maxBytes. If the session closed while
// the backend was deciding, it sends nothing and tells the backend again, so
// that a registration made in the call does not linger, and undoes a pairing:
// the client never got its token.
func (s *Session) authenticate(app, device string, w Welcome, paired *pairedFrame, maxBytes int64) {
	hosts := w.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	welcome := welcomeFrame{"welcome", Version, w.Role, w.ServerID, w.Name, w.Etag, hosts, nil}
	if w.Subordinates != nil {
		list := serverList(w.Subordinates)
		welcome.Subordinates = &list
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
	s.queueLimit = max(maxBytes, 0) + queueSlack
	if s.timer != nil {
		s.timer.Stop()
	}
	if paired != nil {
		s.unconfirmed = true
		s.queueLocked(outgoing{text: encode(paired)})
	}
	s.queueLocked(outgoing{text: encode(welcome)})
	if held := s.held; held != nil && (w.Etag == nil || *w.Etag != *held) {
		s.queueLocked(outgoing{text: encode(etagFrame{"changed", held})})
	}
	if list := s.heldApps; list != nil {
		s.queueLocked(outgoing{text: encode(appsFrame{"apps", appList(*list)})})
	}
	s.held, s.heldApps = nil, nil
	s.mu.Unlock()
}

// PushChanged tells the client that the app's document now has etag. It never
// blocks. Before the welcome has been queued (for instance when called from
// inside the backend's Hello or Pair) the push is held, only the latest kept,
// and sent right after the welcome unless the welcome carries the same etag.
// On a closed session it does nothing.
func (s *Session) PushChanged(etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed || s.closing:
	case !s.authenticated:
		s.held = &etag
	default:
		s.queueLocked(outgoing{text: encode(etagFrame{"changed", &etag})})
	}
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
func (s *Session) PushDevices(list []DeviceEntry) {
	if list == nil {
		list = []DeviceEntry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || !s.authenticated {
		return
	}
	s.queueLocked(outgoing{text: encode(devicesFrame{"devices", list})})
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
// first call closes the channel, then tells the backend, then runs OnClose;
// later calls do nothing. It is safe from any goroutine, including from
// inside HandleText and the backend's methods.
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
// timer. It reports whether this was the first close.
func (s *Session) markClosedLocked() bool {
	if s.closed {
		return false
	}
	s.closed = true
	s.queue = nil
	s.queued = 0
	s.held, s.heldApps = nil, nil
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
	s.mu.Unlock()
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

// reply queues an error frame; the channel stays open.
func (s *Session) reply(e *Error) {
	s.queueFrame(errorFrame{"error", e.Code, e.Message}, nil)
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

// queueFrame queues frame, followed by body when it announces one.
func (s *Session) queueFrame(frame any, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing {
		return
	}
	s.queueLocked(outgoing{text: encode(frame), body: body})
}

// queueLocked queues o. A queue that would grow past queueLimit closes the
// session instead: its client asks for more than it reads. An item that finds
// the queue empty is always taken, so that a document larger than the limit
// (one stored before the app's limit was lowered) can still be read.
func (s *Session) queueLocked(o outgoing) {
	if s.closed {
		return
	}
	if len(s.queue) > 0 && s.queued+o.size() > s.queueLimit {
		// The caller may hold the backend's locks (PushChanged does), and
		// closing tells the backend, so the close runs on its own.
		if s.markClosedLocked() {
			go s.finishClose()
		}
		return
	}
	s.queue = append(s.queue, o)
	s.queued += o.size()
	signal(s.wake)
}

// sendLoop is the sender goroutine: it sends the queue in order until the
// session closes.
func (s *Session) sendLoop() {
	defer close(s.senderDone)
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
			return
		}
		for off := 0; off < len(o.body); off += ChunkBytes {
			chunk := o.body[off:min(off+ChunkBytes, len(o.body))]
			if !s.write(func() error { return s.ch.Send(chunk) }) {
				return
			}
		}
	}
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
			s.queued -= o.size()
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

// The frames the server sends. An etag is a string or null on the wire.
type (
	// etagFrame is head, stored, conflict and changed.
	etagFrame struct {
		T    string  `json:"t"`
		Etag *string `json:"etag"`
	}
	docFrame struct {
		T     string  `json:"t"`
		Etag  *string `json:"etag"`
		Bytes int     `json:"bytes"`
	}
	pairedFrame struct {
		T      string `json:"t"`
		Device string `json:"device"`
		Token  string `json:"token"`
	}
	welcomeFrame struct {
		T            string        `json:"t"`
		Proto        int           `json:"proto"`
		Role         string        `json:"role,omitempty"`
		ServerID     string        `json:"serverId"`
		Name         string        `json:"name"`
		Etag         *string       `json:"etag"`
		Hosts        []string      `json:"hosts"`
		Subordinates *[]ServerInfo `json:"subordinates,omitempty"`
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
