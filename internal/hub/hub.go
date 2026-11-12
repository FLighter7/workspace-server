// Package hub is the protocol's backend: it authenticates hello and pair
// against the apps list, the device registry and the pairing secret, answers
// reads and writes from the document store, tells an app's other sessions
// about every stored write, and closes the sessions of a device or app that is
// removed. It also limits failed attempts per IP.
//
// A server-kind device (a subordinate server linked to this one) is admitted
// to every listed app, and also to a link session, which has no app: it is
// told the app list on request and again whenever the list changes. Over its
// link session a subordinate announces how devices reach it, which a main
// lists in every welcome, and is told the device registry, again whenever it
// changes. A subordinate pairs nothing: devices pair with its main.
//
// The servers of a family reach one another to settle which of them is the
// main (docs/architecture.md, "Switching the main"): a link session may tell
// a term, which the hub hands to Deps.Follow, and a main may ask one of its
// subordinates to take its role over (TakeOver). A server-kind device of the
// replica, another subordinate of the same main, is admitted to link
// sessions for that alone.
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

// Deps are what the hub works with. Name, Hosts and Role are read at every
// welcome, so changes show at once.
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
	// Role, if set, is the server's role: protocol.RoleMain, or
	// protocol.RoleSubordinate while it is linked to an upstream server.
	// Unset means RoleMain.
	Role func() string
	// Logf, if set, receives the text of every internal error; devices get
	// the error without it.
	Logf func(format string, args ...any)
	// Stored, if set, is called after a device's write is stored (Put), once
	// the hub's locks are released, and must not block. A subordinate passes
	// it to its link, which then pushes the document to its main. Replace
	// does not call it: the link writes with Replace, and a hook on its own
	// writes would send them back where they came from.
	Stored func(app string)
	// Invite, if set, issues a pairing code of this server with a new
	// secret, as its page's "Pair a device" does (the new secret revokes the
	// previous one). A subordinate asks for one over its link session
	// (invite), for a browser on its own computer.
	Invite func() (code string, expires time.Time, err error)
	// Term, if set, is the family's term as this server knows it, read at
	// every welcome. Unset means 0.
	Term func() int
	// Follow, if set, is told a term frame from the link session of the
	// server-kind device sender: the sender is the main of the family at
	// term, reached at server, and device and token, when given, are a
	// credential for this server there. It decides whether this server
	// follows the sender, must not wait for the network, and returns the
	// term and main this server follows then. An error is a term this
	// server cannot act on (a protocol error to the sender).
	Follow func(sender string, term int, server protocol.ServerInfo, device, token string) (protocol.TermAnswer, error)
}

// Errors of TakeOver.
var (
	ErrNoSuchServer = errors.New("no server of that id is linked to this one")
	ErrNotConnected = errors.New("that server is not connected to this one now")
)

// SessionInfo describes one authenticated session. App is empty for a link
// session.
type SessionInfo struct{ App, Device, Remote string }

// member is what the hub knows of a registered session. The hub records it at
// registration and never reads it back from the session. app is empty on a
// link session; server marks a session of a server-kind device; registry
// marks a link session that announced itself, which is told the device
// registry on every change; family marks the link session of a server-kind
// device of the replica (another server of this server's family), which may
// only tell a term.
type member struct {
	app, device string
	server      bool
	registry    bool
	family      bool
}

// Hub routes the sessions of all apps. It is safe for concurrent use.
//
// Locking: an app's lock (appLock) orders everything that admits a session
// to the app or changes its document, devices or listing, so a session is
// never admitted with a device or app that is being removed, and pushes leave
// in the order the writes were stored. A server-kind device has sessions of
// every app and a link session, so its removal cannot take one app's lock:
// admissions and writes read-lock servers, and that removal write-locks it.
// mu guards only the session set and is never held while calling into a
// session: Session.Close calls Closed, which takes mu. Lock order: an app's
// lock, then servers, then mu.
type Hub struct {
	d     Deps
	limit limiter

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // per app id; only ids that were listed

	servers   sync.RWMutex
	appsMu    sync.Mutex // orders the app lists pushed to link sessions
	devicesMu sync.Mutex // orders the registries pushed to link sessions

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

// Hello authenticates a known device of a listed app and registers s. With
// no app it opens a link session, which only a server-kind device may.
func (h *Hub) Hello(s *protocol.Session, app, device, token string) (protocol.Welcome, *protocol.Error) {
	ip := hostOf(s.Remote())
	if h.limit.blocked(ip, h.d.Now()) {
		return protocol.Welcome{}, errBlocked()
	}
	admit := h.admitDevice
	if app == "" {
		admit = h.admitLink
	}
	if e := admit(s, app, device, token); e != nil {
		h.limit.fail(ip, h.d.Now())
		return protocol.Welcome{}, e
	}
	h.d.Devices.Touch(device)
	return h.welcome(s, app)
}

// admitDevice checks the app and the device's token and registers s, all
// under the app's lock. A server-kind device is admitted to every listed
// app.
func (h *Hub) admitDevice(s *protocol.Session, app, device, token string) *protocol.Error {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return errUnknownApp(app)
	}
	defer unlock()
	h.servers.RLock()
	defer h.servers.RUnlock()
	d, ok := h.d.Devices.Authenticate(app, device, token)
	if !ok {
		return errBadToken()
	}
	h.register(s, member{app: app, device: device, server: d.IsServer()})
	return nil
}

// admitLink admits a server-kind device to a link session: a hello with no
// app. A device of an app gets unknownApp, since no app "" is listed; an
// unknown device or a wrong token gets badToken, so that a server removed
// here learns that its pairing is gone.
func (h *Hub) admitLink(s *protocol.Session, app, device, token string) *protocol.Error {
	if d, ok := h.d.Devices.Get(device); ok && !d.IsServer() {
		return errUnknownApp(app)
	}
	h.servers.RLock()
	defer h.servers.RUnlock()
	d, ok := h.d.Devices.Authenticate("", device, token)
	if !ok {
		return errBadToken()
	}
	h.register(s, member{device: device, server: true, family: d.Replica})
	return nil
}

// Pair registers a new device of a listed app with the one-time secret and
// registers s. A valid secret for an unlisted app is refused without using
// the secret, and the attempt is remembered for the web page. A server-kind
// pair names no app: the new device is admitted to every app, and s is its
// link session.
func (h *Hub) Pair(s *protocol.Session, kind, app, secret, name string) (string, string, protocol.Welcome, *protocol.Error) {
	ip := hostOf(s.Remote())
	if h.limit.blocked(ip, h.d.Now()) {
		return "", "", protocol.Welcome{}, errBlocked()
	}
	if h.role() == protocol.RoleSubordinate {
		// Nothing is checked or used: the secret, if any, stays good.
		return "", "", protocol.Welcome{}, errNotMain()
	}
	if !h.d.Secrets.Check(secret) {
		h.limit.fail(ip, h.d.Now())
		return "", "", protocol.Welcome{}, errBadSecret()
	}
	if kind == protocol.KindServer {
		return h.pairServer(s, ip, secret, name)
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
	h.register(s, member{app: app, device: d.ID})
	unlock()
	h.d.Refusals.Clear(app)

	w, e := h.welcome(s, app)
	if e != nil {
		// The client never gets the token, so the entry could never be used.
		h.d.Devices.Remove(d.ID)
		h.DevicesChanged()
		return "", "", protocol.Welcome{}, e
	}
	h.DevicesChanged()
	return d.ID, token, w, nil
}

// pairServer registers a new server-kind device with the one-time secret,
// which the caller has checked, and registers s as its link session.
func (h *Hub) pairServer(s *protocol.Session, ip, secret, name string) (string, string, protocol.Welcome, *protocol.Error) {
	h.servers.RLock()
	if !h.d.Secrets.Consume(secret) { // another pairing used it meanwhile
		h.servers.RUnlock()
		h.limit.fail(ip, h.d.Now())
		return "", "", protocol.Welcome{}, errBadSecret()
	}
	d, token, err := h.d.Devices.PairServer(name)
	if err != nil {
		h.servers.RUnlock()
		return "", "", protocol.Welcome{}, h.errInternal(err)
	}
	h.register(s, member{device: d.ID, server: true})
	h.servers.RUnlock()

	w, e := h.welcome(s, "")
	if e != nil {
		h.d.Devices.Remove(d.ID)
		h.DevicesChanged()
		return "", "", protocol.Welcome{}, e
	}
	h.DevicesChanged()
	return d.ID, token, w, nil
}

// welcome reads the document's etag for s, which the caller has registered:
// a write stored after the registration is pushed to s (the session holds the
// push until its welcome is out) and one stored before is in the etag. A link
// session (app "") has no document. On failure it unregisters s.
func (h *Hub) welcome(s *protocol.Session, app string) (protocol.Welcome, *protocol.Error) {
	var etag *string
	if app != "" {
		var e *protocol.Error
		if etag, e = h.Head(app); e != nil {
			h.Closed(s)
			return protocol.Welcome{}, e
		}
	}
	w := protocol.Welcome{ServerID: h.d.ServerID, Name: h.d.Name(), Etag: etag, Hosts: h.d.Hosts(), Role: h.role(), Term: h.term()}
	if w.Role == protocol.RoleMain {
		w.Subordinates = h.subordinates()
	}
	return w, nil
}

// term is the family's term as this server knows it now.
func (h *Hub) term() int {
	if h.d.Term != nil {
		return h.d.Term()
	}
	return 0
}

// role is the server's role now.
func (h *Hub) role() string {
	if h.d.Role != nil {
		return h.d.Role()
	}
	return protocol.RoleMain
}

// subordinates lists what this server's subordinates announced, never nil:
// a main always sends the list, [] included, so that a device drops the
// subordinates it no longer has, and a subordinate knows that its main
// takes announce.
func (h *Hub) subordinates() []protocol.ServerInfo {
	list := h.d.Devices.Subordinates()
	out := make([]protocol.ServerInfo, 0, len(list))
	for _, si := range list {
		out = append(out, protocol.ServerInfo(si))
	}
	return out
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

// Apps returns the app list to a session of a server-kind device. Any other
// session gets a protocol error, which closes it.
func (h *Hub) Apps(s *protocol.Session) ([]protocol.AppInfo, *protocol.Error) {
	h.mu.Lock()
	m, ok := h.sessions[s]
	h.mu.Unlock()
	if !ok || !m.server || m.family {
		return nil, &protocol.Error{Code: protocol.CodeProtocol, Message: "only a server linked to this one may ask for the app list"}
	}
	return h.appList(), nil
}

// AppsChanged tells every link session the app list as it is now. The
// server calls it after every change to the list.
func (h *Hub) AppsChanged() {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	list := h.appList()
	for _, s := range h.registered(func(m member) bool { return m.server && m.app == "" && !m.family }) {
		s.PushApps(list)
	}
}

// Announce keeps how devices reach the subordinate whose link session s is,
// and answers with the device registry, which s is told again on every
// change from now on. handover, when not nil, is the subordinate's own
// registry: it was a main and stepped down to this one, and this server
// takes its devices and removals over (devices.Registry.Merge). Only a link
// session of a server-kind device may announce; a server that is not valid,
// or this server's own id, is a protocol error, which closes the session,
// and so is a handover to a subordinate.
func (h *Hub) Announce(s *protocol.Session, server protocol.ServerInfo, handover *protocol.Registry) *protocol.Error {
	h.mu.Lock()
	m, ok := h.sessions[s]
	h.mu.Unlock()
	if !ok || !m.server || m.app != "" || m.family {
		return &protocol.Error{Code: protocol.CodeProtocol, Message: "only the link session of a server linked to this one may announce"}
	}
	if server.ServerID == h.d.ServerID {
		return &protocol.Error{Code: protocol.CodeProtocol, Message: "a server cannot be its own subordinate"}
	}
	si := devices.Server(server)
	if err := si.Validate(); err != nil {
		return &protocol.Error{Code: protocol.CodeProtocol, Message: "announce: " + err.Error()}
	}
	if handover != nil && h.role() != protocol.RoleMain {
		return &protocol.Error{Code: protocol.CodeProtocol, Message: "only a main takes a registry over"}
	}
	// Under servers, which the removal of a server-kind device holds: a
	// device removed meanwhile is not written back.
	h.servers.RLock()
	changed, err := h.d.Devices.SetServer(m.device, si)
	h.servers.RUnlock()
	if err != nil {
		return h.errInternal(err)
	}
	if changed {
		h.d.Logf("the subordinate %q (%s) is reached at port %d, hosts %v", si.Name, si.ServerID, si.Port, si.Hosts)
	}
	if handover != nil {
		list, removed, err := fromWire(*handover)
		if err != nil {
			return &protocol.Error{Code: protocol.CodeProtocol, Message: "announce: " + err.Error()}
		}
		merged, err := h.Adopt(list, removed, devices.Self{ServerID: h.d.ServerID})
		if err != nil {
			return h.errInternal(err)
		}
		h.d.Logf("the subordinate %q (%s) was a main: its registry (%d devices, %d removed) is merged into this one's", si.Name, si.ServerID, len(list), len(removed))
		changed = changed || merged
	}

	h.devicesMu.Lock()
	h.mu.Lock()
	m, ok = h.sessions[s]
	if ok {
		m.registry = true
		h.sessions[s] = m
	}
	h.mu.Unlock()
	h.devicesMu.Unlock()
	switch {
	case changed:
		// Every subordinate keeps the others' connection data: the answer
		// goes to s with the push to all.
		h.DevicesChanged()
	case ok: // a session closed meanwhile is told nothing
		h.devicesMu.Lock()
		s.PushDevices(h.registry())
		h.devicesMu.Unlock()
	}
	return nil
}

// fromWire reads a registry as it travels.
func fromWire(reg protocol.Registry) ([]devices.Entry, []devices.Tombstone, error) {
	list := make([]devices.Entry, 0, len(reg.Devices))
	for _, d := range reg.Devices {
		e := devices.Entry{ID: d.ID, App: d.App, Kind: d.Kind, Name: d.Name, TokenHash: d.TokenHash}
		if d.PairedAt != "" {
			at, err := time.Parse(time.RFC3339, d.PairedAt)
			if err != nil {
				return nil, nil, fmt.Errorf("device %q: pairedAt: %v", d.ID, err)
			}
			e.PairedAt = at
		}
		if d.Server != nil {
			si := devices.Server(*d.Server)
			e.Server = &si
		}
		list = append(list, e)
	}
	removed := make([]devices.Tombstone, 0, len(reg.Removed))
	for _, t := range reg.Removed {
		at, err := time.Parse(time.RFC3339, t.At)
		if err != nil {
			return nil, nil, fmt.Errorf("removed %q: at: %v", t.ID, err)
		}
		removed = append(removed, devices.Tombstone{ID: t.ID, At: at})
	}
	return list, removed, nil
}

// ToWire is a registry as it travels.
func ToWire(list []devices.Entry, removed []devices.Tombstone) protocol.Registry {
	out := protocol.Registry{Devices: make([]protocol.DeviceEntry, 0, len(list))}
	for _, e := range list {
		d := protocol.DeviceEntry{ID: e.ID, App: e.App, Kind: e.Kind, Name: e.Name, TokenHash: e.TokenHash}
		if !e.PairedAt.IsZero() {
			d.PairedAt = e.PairedAt.UTC().Format(time.RFC3339)
		}
		if e.Server != nil {
			si := protocol.ServerInfo(*e.Server)
			d.Server = &si
		}
		out.Devices = append(out.Devices, d)
	}
	for _, t := range removed {
		out.Removed = append(out.Removed, protocol.Tombstone{ID: t.ID, At: t.At.UTC().Format(time.RFC3339)})
	}
	return out
}

// Term hands a term frame from the link session s to Deps.Follow: the
// sender says it is the main of the family. Only a link session of a
// server-kind device may tell one: this server's subordinate, or another
// server of its family; any other session gets a protocol error, which
// closes it.
func (h *Hub) Term(s *protocol.Session, term int, server protocol.ServerInfo, device, token string) (protocol.TermAnswer, *protocol.Error) {
	h.mu.Lock()
	m, ok := h.sessions[s]
	h.mu.Unlock()
	if !ok || !m.server || m.app != "" {
		return protocol.TermAnswer{}, &protocol.Error{Code: protocol.CodeProtocol, Message: "only a server's link session may tell a term"}
	}
	if err := devices.Server(server).Validate(); err != nil || server.ServerID == h.d.ServerID {
		return protocol.TermAnswer{}, &protocol.Error{Code: protocol.CodeProtocol, Message: "term: the server is not valid, or is this one"}
	}
	if h.d.Follow == nil {
		return protocol.TermAnswer{Term: h.term(), Main: h.d.ServerID}, nil
	}
	a, err := h.d.Follow(m.device, term, server, device, token)
	if err != nil {
		return protocol.TermAnswer{}, &protocol.Error{Code: protocol.CodeProtocol, Message: "term: " + err.Error()}
	}
	return a, nil
}

// Invite issues a pairing code of this server for the subordinate whose link
// session s is: the code this server's own page issues, so a browser on the
// subordinate's computer pairs with this server, the main, and then learns
// the subordinate from its welcome. The code names no app, as no pairing
// code does: the device's pair names its own. Only a link session of a
// server-kind device may ask; any other session gets a protocol error, which
// closes it. A subordinate answers notMain: its codes would be refused.
func (h *Hub) Invite(s *protocol.Session) (string, time.Time, *protocol.Error) {
	h.mu.Lock()
	m, ok := h.sessions[s]
	h.mu.Unlock()
	if !ok || !m.server || m.app != "" || m.family {
		return "", time.Time{}, &protocol.Error{Code: protocol.CodeProtocol, Message: "only the link session of a server linked to this one may ask for a pairing code"}
	}
	if h.role() == protocol.RoleSubordinate {
		return "", time.Time{}, errNotMain()
	}
	if h.d.Invite == nil {
		return "", time.Time{}, h.errInternal(errors.New("this server issues no pairing codes"))
	}
	code, expires, err := h.d.Invite()
	if err != nil {
		return "", time.Time{}, h.errInternal(err)
	}
	name := m.device
	if d, ok := h.d.Devices.Get(m.device); ok {
		name = d.Name
		if d.Server != nil {
			name = d.Server.Name
		}
	}
	h.d.Logf("issued a pairing code for a browser on the subordinate %q; it replaces any code this server's page showed", name)
	return code, expires, nil
}

// DevicesChanged tells every link session that announced itself the device
// registry as it is now. The hub calls it after every change it makes to
// the registry.
func (h *Hub) DevicesChanged() {
	h.devicesMu.Lock()
	defer h.devicesMu.Unlock()
	list := h.registry()
	for _, s := range h.registered(func(m member) bool { return m.registry }) {
		s.PushDevices(list)
	}
}

// Replicate makes list the devices this server admits on its main's word
// (devices.SetReplica), and closes the sessions of the devices it no longer
// holds: a device removed on the main is refused here from the moment the
// main's registry says so. The link calls it with every registry it gets.
func (h *Hub) Replicate(list []devices.Entry) {
	// Under servers, which every admission and write read-locks: none slips
	// between the new list and the sessions closed for it.
	h.servers.Lock()
	before := h.d.Devices.Replica()
	h.d.Devices.SetReplica(list)
	kept := map[[4]string]bool{}
	for _, e := range h.d.Devices.Replica() {
		kept[admission(e)] = true
	}
	gone := map[string]bool{}
	for _, e := range before {
		if d, ok := h.d.Devices.Get(e.ID); !kept[admission(e)] && (!ok || d.Replica) {
			gone[e.ID] = true // an own device with that id keeps its sessions
		}
	}
	targets := h.registered(func(m member) bool { return (!m.server || m.family) && gone[m.device] })
	h.servers.Unlock()
	closeAll(targets)
}

func admission(e devices.Entry) [4]string { return [4]string{e.ID, e.App, e.Kind, e.TokenHash} }

// Adopt merges list and removed, a registry of the family, into this
// server's own devices (devices.Registry.Merge), closes the sessions of the
// devices it drops, and tells the link sessions the new registry. A
// subordinate becoming the main adopts its main's registry so; a main takes
// over the registry of a main that stepped down to it. It reports whether
// the registry changed.
func (h *Hub) Adopt(list []devices.Entry, removed []devices.Tombstone, self devices.Self) (bool, error) {
	h.servers.Lock()
	before := len(h.d.Devices.Entries()) + len(h.d.Devices.Removed())
	dropped, err := h.d.Devices.Merge(list, removed, self)
	after := len(h.d.Devices.Entries()) + len(h.d.Devices.Removed())
	gone := map[string]bool{}
	for _, d := range dropped {
		gone[d.ID] = true
	}
	targets := h.registered(func(m member) bool { return gone[m.device] })
	h.servers.Unlock()
	closeAll(targets)
	if err != nil {
		return false, err
	}
	changed := before != after || len(dropped) > 0
	if changed {
		h.DevicesChanged()
	}
	return changed, nil
}

// ClearDevices forgets this server's own devices once it has handed them
// over to the main it now follows (devices.Registry.Clear), and closes the
// sessions of those this server no longer admits on the main's word either.
func (h *Hub) ClearDevices() error {
	h.servers.Lock()
	err := h.d.Devices.Clear()
	targets := h.registered(func(m member) bool {
		_, ok := h.d.Devices.Get(m.device)
		return !ok
	})
	h.servers.Unlock()
	closeAll(targets)
	return err
}

// MintServer gives the server s a server-kind device here and returns the
// device's id and its new token (devices.Registry.MintServer), and tells the
// link sessions the new registry.
func (h *Hub) MintServer(s devices.Server) (string, string, error) {
	h.servers.RLock()
	d, token, err := h.d.Devices.MintServer(s)
	h.servers.RUnlock()
	if err != nil {
		return "", "", err
	}
	h.DevicesChanged()
	return d.ID, token, nil
}

// Linked reports whether the server serverID is linked to this one now: a
// server-kind device of this server's own with that id has a link session.
func (h *Hub) Linked(serverID string) bool {
	id := h.serverDevice(serverID)
	if id == "" {
		return false
	}
	return len(h.registered(func(m member) bool { return m.device == id && m.server && m.app == "" && !m.family })) > 0
}

// serverDevice is the id of this server's own server-kind device for the
// server serverID, "" when there is none.
func (h *Hub) serverDevice(serverID string) string {
	for _, d := range h.d.Devices.List() {
		if d.IsServer() && d.Server != nil && d.Server.ServerID == serverID {
			return d.ID
		}
	}
	return ""
}

// TakeOver asks the server serverID, a subordinate linked to this one, over
// its link session, to become the main of the family (takeOver). It is
// ErrNoSuchServer when no server-kind device here has that id, and
// ErrNotConnected when it has no link session now.
func (h *Hub) TakeOver(serverID string) error {
	id := h.serverDevice(serverID)
	if id == "" {
		return ErrNoSuchServer
	}
	sessions := h.registered(func(m member) bool { return m.device == id && m.server && m.app == "" && !m.family })
	if len(sessions) == 0 {
		return ErrNotConnected
	}
	for _, s := range sessions {
		s.PushTakeOver()
	}
	return nil
}

// registry is the device registry a main tells its subordinates: its own
// devices, server-kind devices included, with their token hashes, and the
// devices removed.
func (h *Hub) registry() protocol.Registry {
	return ToWire(h.d.Devices.Entries(), h.d.Devices.Removed())
}

// appList is the listed apps, sorted by id.
func (h *Hub) appList() []protocol.AppInfo {
	list := h.d.Apps.List()
	out := make([]protocol.AppInfo, 0, len(list))
	for _, a := range list {
		out = append(out, protocol.AppInfo{ID: a.ID, Name: a.Name})
	}
	return out
}

// Put stores body as app's document if the stored one has ifEtag, and then
// tells every other session of the app the new etag, and Deps.Stored. A put
// from a device removed meanwhile stores nothing.
func (h *Hub) Put(from *protocol.Session, app, device string, ifEtag *string, body []byte) (string, bool, *string, *protocol.Error) {
	a, unlock, ok := h.lockListed(app)
	if !ok {
		return "", false, nil, &protocol.Error{Code: protocol.CodeUnknownApp, Message: fmt.Sprintf("this server no longer serves the app %q", app)}
	}
	stored := false
	defer func() { // deferred first, so it runs after the locks are released
		if stored && h.d.Stored != nil {
			h.d.Stored(app)
		}
	}()
	defer unlock()
	// Under the app's lock, which RemoveDevice takes too, and the servers
	// lock, which the removal of a server-kind device takes.
	h.servers.RLock()
	defer h.servers.RUnlock()
	if !h.admitted(device, app) {
		return "", false, nil, &protocol.Error{Code: protocol.CodeBadToken, Message: "this device has been removed; pair it again"}
	}
	etag, err := h.writeLocked(a, body, ifEtag, from)
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &conflict):
		return "", true, conflict.Current, nil
	case errors.Is(err, store.ErrNotJSON):
		return "", false, nil, &protocol.Error{Code: protocol.CodeNotJSON, Message: "the body is not valid JSON"}
	case err != nil:
		return "", false, nil, h.errInternal(err)
	}
	stored = true
	return etag, false, nil, nil
}

// Replace stores body as app's document if the stored one has ifEtag, as a
// put does, and tells every session of the app the new etag, since none of
// them wrote it. The link to the upstream server stores the main's document
// with it. It does not call Deps.Stored. A mismatch is a
// *store.ConflictError naming the current etag; an app that is not listed is
// an error wrapping apps.ErrNotFound.
func (h *Hub) Replace(app string, body []byte, ifEtag *string) (string, error) {
	a, unlock, ok := h.lockListed(app)
	if !ok {
		return "", fmt.Errorf("%w: %q", apps.ErrNotFound, app)
	}
	defer unlock()
	return h.writeLocked(a, body, ifEtag, nil)
}

// writeLocked writes body as a's document if the stored one has ifEtag, and
// pushes the new etag to every session of the app but from. The caller holds
// the app's lock.
func (h *Hub) writeLocked(a apps.App, body []byte, ifEtag *string, from *protocol.Session) (string, error) {
	etag, err := h.d.Store.Write(a.ID, body, ifEtag, a.History)
	if err != nil {
		return "", err
	}
	// Still under the app's lock, so each session's changed frames follow the
	// order the writes were stored, and its last frame carries the newest
	// etag. A welcome reads its etag outside this lock, so it can be newer
	// than a push still to come: welcome(e3), changed(e2), changed(e3). That
	// is all protocol.md promises. PushChanged never blocks and never calls
	// back into the hub.
	for _, s := range h.registered(func(m member) bool { return m.app == a.ID }) {
		if s != from {
			s.PushChanged(etag)
		}
	}
	return etag, nil
}

// Forget deletes the device, if it belongs to app or is a server-kind device,
// and closes its sessions, the asking one included: the backend is not told
// which session asks, and that session closes after Forget anyway.
func (h *Hub) Forget(app, device string) {
	if h.admitted(device, app) {
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
// existed. A device of the replica (its main's, on a subordinate) is not
// this server's to delete: only its sessions close.
func (h *Hub) RemoveDevice(id string) bool {
	d, ok := h.d.Devices.Get(id)
	if !ok {
		return false
	}
	if d.IsServer() {
		// Its sessions span every app; holding servers keeps any of them
		// from being admitted, or writing, meanwhile.
		h.servers.Lock()
		removed := h.d.Devices.Remove(id)
		targets := h.registered(func(m member) bool { return m.device == id })
		h.servers.Unlock()
		closeAll(targets)
		if removed {
			h.DevicesChanged()
		}
		return removed
	}
	l := h.appLock(d.App)
	l.Lock()
	removed := h.d.Devices.Remove(id)
	targets := h.registered(func(m member) bool { return m.device == id })
	l.Unlock()
	closeAll(targets)
	if removed {
		h.DevicesChanged()
	}
	return removed
}

// RemoveApp deletes a listed app: its devices, its documents and its entry in
// apps.json, and closes its sessions. It stops at the first step that fails,
// leaving the app listed so that it can be removed again. An app that is not
// listed is an error wrapping apps.ErrNotFound, and nothing is touched.
// Server-kind devices stay: they are admitted to every app.
func (h *Hub) RemoveApp(id string) error {
	_, unlock, ok := h.lockListed(id)
	if !ok {
		return fmt.Errorf("%w: %q", apps.ErrNotFound, id)
	}
	removed := h.d.Devices.RemoveApp(id)
	err := h.d.Store.DeleteApp(id)
	if err == nil {
		err = h.d.Apps.Remove(id)
	}
	targets := h.registered(func(m member) bool { return m.app == id })
	unlock()
	closeAll(targets) // the devices are gone even if a later step failed
	if len(removed) > 0 {
		h.DevicesChanged()
	}
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

func (h *Hub) register(s *protocol.Session, m member) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[s] = m
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

// admitted reports whether device id exists and may use app: it is a device
// of app, or a server-kind device.
func (h *Hub) admitted(id, app string) bool {
	d, ok := h.d.Devices.Get(id)
	return ok && (d.IsServer() || d.App == app)
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

func errBadToken() *protocol.Error {
	return &protocol.Error{Code: protocol.CodeBadToken, Message: "unknown device or wrong token; pair the device again"}
}

func errBadSecret() *protocol.Error {
	return &protocol.Error{Code: protocol.CodeBadSecret, Message: "the pairing secret is wrong, used or expired; get a new pairing code"}
}

func errNotMain() *protocol.Error {
	return &protocol.Error{Code: protocol.CodeNotMain, Message: "this server is a subordinate; pair with its main, which hands this server out to its devices"}
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

// CloseServers closes every session of a server-kind device but those of
// the device except. The server calls it when it becomes a subordinate
// itself: its subordinates reconnect, read the new role in the welcome, and
// look for the main (or stop, when this server was linked by hand), since
// only a main may be an upstream. except is the server that told this one
// to step down, whose answer must still leave.
func (h *Hub) CloseServers(except string) {
	closeAll(h.registered(func(m member) bool { return m.server && m.device != except }))
}
