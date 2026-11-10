// Package hub is the protocol's backend: it authenticates hello and pair
// against the apps list, the device registry and the pairing secret, answers
// reads and writes from the document store, tells an app's other sessions
// about every stored write, and closes the sessions of a device or app that is
// removed. It also limits failed attempts per IP.
package hub

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/store"
)

// Rate limit: maxFailures failed hello or pair attempts from one IP within
// failureWindow block that IP for blockFor.
const (
	maxFailures   = 10
	failureWindow = time.Minute
	blockFor      = 10 * time.Minute
)

// Deps are what the hub works with. Name and Hosts are read at every welcome,
// so changes show at once.
type Deps struct {
	ServerID string
	Name     func() string   // live, from config
	Hosts    func() []string // live: [mdnsName, ...LAN IPv4]
	Apps     *apps.Registry
	Devices  *devices.Registry
	Store    *store.Store
	Secrets  *pairing.Secrets
	Refusals *pairing.Refusals
	Now      func() time.Time
	// Logf, if set, receives the text of every internal error; devices get
	// the error without it.
	Logf func(format string, args ...any)
}

// SessionInfo describes one authenticated session.
type SessionInfo struct{ App, Device, Remote string }

// member is what the hub knows of a registered session. The hub records it at
// registration and never reads it back from the session.
type member struct{ app, device string }

// Hub routes the sessions of all apps. It is safe for concurrent use.
//
// Locking: an app's lock (appLock) orders everything that admits a session
// to the app or changes its document, devices or listing, so a session is
// never admitted with a device or app that is being removed, and pushes leave
// in the order the writes were stored. mu guards only the session set and is
// never held while calling into a session: Session.Close calls Closed, which
// takes mu. Lock order: an app's lock, then mu.
type Hub struct {
	d     Deps
	limit limiter

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // per app id; only ids that were listed

	mu       sync.Mutex
	sessions map[*protocol.Session]member
}

var _ protocol.Backend = (*Hub)(nil)

// New returns a hub over d.
func New(d Deps) *Hub {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return &Hub{
		d:        d,
		limit:    limiter{failures: map[string][]time.Time{}, until: map[string]time.Time{}},
		locks:    map[string]*sync.Mutex{},
		sessions: map[*protocol.Session]member{},
	}
}

// Hello authenticates a known device of a listed app and registers s.
func (h *Hub) Hello(s *protocol.Session, app, device, token string) (protocol.Welcome, *protocol.Error) {
	ip := hostOf(s.Remote())
	if h.limit.blocked(ip, h.d.Now()) {
		return protocol.Welcome{}, errBlocked()
	}
	if e := h.admitDevice(s, app, device, token); e != nil {
		h.limit.fail(ip, h.d.Now())
		return protocol.Welcome{}, e
	}
	h.d.Devices.Touch(device)
	return h.welcome(s, app)
}

// admitDevice checks the app and the device's token and registers s, all
// under the app's lock.
func (h *Hub) admitDevice(s *protocol.Session, app, device, token string) *protocol.Error {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return errUnknownApp(app)
	}
	defer unlock()
	if _, ok := h.d.Devices.Authenticate(app, device, token); !ok {
		return &protocol.Error{Code: protocol.CodeBadToken, Message: "unknown device or wrong token; pair the device again"}
	}
	h.register(s, app, device)
	return nil
}

// Pair registers a new device of a listed app with the one-time secret and
// registers s. A valid secret for an unlisted app is refused without using
// the secret, and the attempt is remembered for the web page.
func (h *Hub) Pair(s *protocol.Session, app, secret, name string) (string, string, protocol.Welcome, *protocol.Error) {
	ip := hostOf(s.Remote())
	if h.limit.blocked(ip, h.d.Now()) {
		return "", "", protocol.Welcome{}, errBlocked()
	}
	if !h.d.Secrets.Check(secret) {
		h.limit.fail(ip, h.d.Now())
		return "", "", protocol.Welcome{}, errBadSecret()
	}
	_, unlock, ok := h.lockListed(app)
	if !ok {
		h.d.Refusals.Record(app, name)
		h.limit.fail(ip, h.d.Now())
		return "", "", protocol.Welcome{}, errUnknownApp(app)
	}
	if !h.d.Secrets.Consume(secret) { // another pairing used it meanwhile
		unlock()
		h.limit.fail(ip, h.d.Now())
		return "", "", protocol.Welcome{}, errBadSecret()
	}
	d, token, err := h.d.Devices.Pair(app, name)
	if err != nil {
		unlock()
		return "", "", protocol.Welcome{}, h.errInternal(err)
	}
	h.register(s, app, d.ID)
	unlock()
	h.d.Refusals.Clear(app)

	w, e := h.welcome(s, app)
	if e != nil {
		// The client never gets the token, so the entry could never be used.
		h.d.Devices.Remove(d.ID)
		return "", "", protocol.Welcome{}, e
	}
	return d.ID, token, w, nil
}

// welcome reads the document's etag for s, which the caller has registered:
// a write stored after the registration is pushed to s (the session holds the
// push until its welcome is out) and one stored before is in the etag. On
// failure it unregisters s.
func (h *Hub) welcome(s *protocol.Session, app string) (protocol.Welcome, *protocol.Error) {
	etag, e := h.Head(app)
	if e != nil {
		h.Closed(s)
		return protocol.Welcome{}, e
	}
	return protocol.Welcome{ServerID: h.d.ServerID, Name: h.d.Name(), Etag: etag, Hosts: h.d.Hosts()}, nil
}

// Head returns the etag of app's document, nil when there is none.
func (h *Hub) Head(app string) (*string, *protocol.Error) {
	_, etag, err := h.d.Store.Read(app)
	if err != nil {
		return nil, h.errInternal(err)
	}
	return etag, nil
}

// Get returns app's document and its etag, nil and nil when there is none.
func (h *Hub) Get(app string) ([]byte, *string, *protocol.Error) {
	doc, etag, err := h.d.Store.Read(app)
	if err != nil {
		return nil, nil, h.errInternal(err)
	}
	return doc, etag, nil
}

// MaxBytes returns app's size limit, 0 when it is not listed.
func (h *Hub) MaxBytes(app string) int64 {
	a, ok := h.d.Apps.Get(app)
	if !ok {
		return 0
	}
	return a.MaxBytes
}

// Put stores body as app's document if the stored one has ifEtag, and then
// tells every other session of the app the new etag. A put from a device
// removed meanwhile stores nothing.
func (h *Hub) Put(from *protocol.Session, app, device string, ifEtag *string, body []byte) (string, bool, *string, *protocol.Error) {
	a, unlock, ok := h.lockListed(app)
	if !ok {
		return "", false, nil, &protocol.Error{Code: protocol.CodeUnknownApp, Message: fmt.Sprintf("this server no longer serves the app %q", app)}
	}
	defer unlock()
	// Under the app's lock, which RemoveDevice takes too.
	if owner, ok := h.deviceApp(device); !ok || owner != app {
		return "", false, nil, &protocol.Error{Code: protocol.CodeBadToken, Message: "this device has been removed; pair it again"}
	}
	etag, err := h.d.Store.Write(app, body, ifEtag, a.History)
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &conflict):
		return "", true, conflict.Current, nil
	case errors.Is(err, store.ErrNotJSON):
		return "", false, nil, &protocol.Error{Code: protocol.CodeNotJSON, Message: "the body is not valid JSON"}
	case err != nil:
		return "", false, nil, h.errInternal(err)
	}
	// Still under the app's lock, so each session's changed frames follow the
	// order the writes were stored, and its last frame carries the newest
	// etag. A welcome reads its etag outside this lock, so it can be newer
	// than a push still to come: welcome(e3), changed(e2), changed(e3). That
	// is all protocol.md promises. PushChanged never blocks and never calls
	// back into the hub.
	for _, s := range h.registered(func(m member) bool { return m.app == app }) {
		if s != from {
			s.PushChanged(etag)
		}
	}
	return etag, false, nil, nil
}

// Forget deletes the device, if it belongs to app, and closes its sessions,
// the asking one included: the backend is not told which session asks, and
// that session closes after Forget anyway.
func (h *Hub) Forget(app, device string) {
	if owner, ok := h.deviceApp(device); ok && owner == app {
		h.RemoveDevice(device)
	}
}

// Closed unregisters s. It does nothing for a session that is not registered.
func (h *Hub) Closed(s *protocol.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, s)
}

// Sessions lists the authenticated sessions, sorted by app, device and
// remote address.
func (h *Hub) Sessions() []SessionInfo {
	h.mu.Lock()
	out := make([]SessionInfo, 0, len(h.sessions))
	for s, m := range h.sessions {
		out = append(out, SessionInfo{App: m.app, Device: m.device, Remote: s.Remote()})
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.App != b.App {
			return a.App < b.App
		}
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.Remote < b.Remote
	})
	return out
}

// RemoveDevice deletes device id, closes its sessions, and reports whether it
// existed.
func (h *Hub) RemoveDevice(id string) bool {
	app, ok := h.deviceApp(id)
	if !ok {
		return false
	}
	l := h.appLock(app)
	l.Lock()
	removed := h.d.Devices.Remove(id)
	targets := h.registered(func(m member) bool { return m.device == id })
	l.Unlock()
	closeAll(targets)
	return removed
}

// RemoveApp deletes a listed app: its devices, its documents and its entry in
// apps.json, and closes its sessions. It stops at the first step that fails,
// leaving the app listed so that it can be removed again. An app that is not
// listed is an error wrapping apps.ErrNotFound, and nothing is touched.
func (h *Hub) RemoveApp(id string) error {
	_, unlock, ok := h.lockListed(id)
	if !ok {
		return fmt.Errorf("%w: %q", apps.ErrNotFound, id)
	}
	h.d.Devices.RemoveApp(id)
	err := h.d.Store.DeleteApp(id)
	if err == nil {
		err = h.d.Apps.Remove(id)
	}
	targets := h.registered(func(m member) bool { return m.app == id })
	unlock()
	closeAll(targets) // the devices are gone even if a later step failed
	return err
}

// CloseApp closes the sessions of an app, for an app removed from apps.json
// by hand. Its devices and documents stay.
func (h *Hub) CloseApp(id string) {
	l := h.appLock(id)
	l.Lock()
	targets := h.registered(func(m member) bool { return m.app == id })
	l.Unlock()
	closeAll(targets)
}

func (h *Hub) register(s *protocol.Session, app, device string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[s] = member{app: app, device: device}
}

// registered returns the registered sessions that match.
func (h *Hub) registered(match func(member) bool) []*protocol.Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*protocol.Session
	for s, m := range h.sessions {
		if match(m) {
			out = append(out, s)
		}
	}
	return out
}

// closeAll closes sessions; the caller holds none of the hub's locks.
func closeAll(sessions []*protocol.Session) {
	for _, s := range sessions {
		s.Close()
	}
}

// appLock returns the lock of app, creating it. Callers pass only ids that
// are or were listed, so the table stays as small as the apps list.
func (h *Hub) appLock(app string) *sync.Mutex {
	h.locksMu.Lock()
	defer h.locksMu.Unlock()
	l, ok := h.locks[app]
	if !ok {
		l = new(sync.Mutex)
		h.locks[app] = l
	}
	return l
}

// lockListed takes the lock of app if app is listed, and returns the app and
// the function that releases the lock. It reports false, holding nothing,
// when app is not listed. The list is checked again under the lock, so a
// removal that took the lock first is seen.
func (h *Hub) lockListed(app string) (apps.App, func(), bool) {
	if _, ok := h.d.Apps.Get(app); !ok {
		return apps.App{}, nil, false
	}
	l := h.appLock(app)
	l.Lock()
	a, ok := h.d.Apps.Get(app)
	if !ok {
		l.Unlock()
		return apps.App{}, nil, false
	}
	return a, l.Unlock, true
}

// deviceApp returns the app of device id.
func (h *Hub) deviceApp(id string) (string, bool) {
	for _, d := range h.d.Devices.List() {
		if d.ID == id {
			return d.App, true
		}
	}
	return "", false
}

// hostOf returns the host part of a host:port address, or the address as is
// when it has no port.
func hostOf(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

func errUnknownApp(app string) *protocol.Error {
	return &protocol.Error{Code: protocol.CodeUnknownApp, Message: fmt.Sprintf("this server does not serve the app %q", app)}
}

func errBadSecret() *protocol.Error {
	return &protocol.Error{Code: protocol.CodeBadSecret, Message: "the pairing secret is wrong, used or expired; get a new pairing code"}
}

func errBlocked() *protocol.Error {
	return &protocol.Error{Code: protocol.CodeBlocked, Message: "too many failed attempts from this address; try again in a few minutes"}
}

// errInternal logs err and returns the error a device gets, which leaves out
// err's text: it can name files on the server's computer.
func (h *Hub) errInternal(err error) *protocol.Error {
	h.d.Logf("internal error: %v", err)
	return &protocol.Error{Code: protocol.CodeInternal, Message: "internal error; the server's log has the details"}
}

// limiter counts failed hello and pair attempts per IP and blocks an IP that
// reaches maxFailures within failureWindow. It is safe for concurrent use.
type limiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time // per IP, within failureWindow, oldest first
	until    map[string]time.Time   // per blocked IP, when the block ends
}

// blocked reports whether ip is blocked at now.
func (l *limiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	end, ok := l.until[ip]
	if !ok {
		return false
	}
	if now.Before(end) {
		return true
	}
	delete(l.until, ip)
	return false
}

// fail records a failure from ip at now; the maxFailures-th within the window
// blocks ip for blockFor from now. A failure from an IP already blocked (an
// attempt that passed the check just before the block began) changes nothing.
func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if _, ok := l.until[ip]; ok {
		return
	}
	times := append(l.failures[ip], now)
	if len(times) >= maxFailures {
		delete(l.failures, ip)
		l.until[ip] = now.Add(blockFor)
		return
	}
	l.failures[ip] = times
}

// pruneLocked forgets failures older than the window and blocks that have
// ended, so memory stays bounded by the recent attempts.
func (l *limiter) pruneLocked(now time.Time) {
	for ip, end := range l.until {
		if !now.Before(end) {
			delete(l.until, ip)
		}
	}
	for ip, times := range l.failures {
		i := 0
		for i < len(times) && now.Sub(times[i]) >= failureWindow {
			i++
		}
		switch {
		case i == len(times):
			delete(l.failures, ip)
		case i > 0:
			l.failures[ip] = append([]time.Time(nil), times[i:]...)
		}
	}
}
