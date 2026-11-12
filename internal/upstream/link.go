// Package upstream is a subordinate server's link to its main: the upstream
// server whose apps it mirrors. The link pairs with the main once, as a
// server-kind device, and keeps the pairing in upstream.json. It then holds a
// link session, which tells it the main's app list, and one session per app,
// over which it keeps the app's document in line with the main's.
//
// What the link does for each app follows the sync rule in
// docs/architecture.md (see sync): it pulls when only the main's copy moved,
// pushes when only this server's did, and when both did, the main's copy
// wins here and this server's goes to history. Neither side ever merges:
// the devices do, as they always have, and write the result back.
//
// Over the link session the link also announces how devices reach this
// server, which the main hands out to its devices, and keeps the main's
// device registry, so that this server admits the main's devices, also
// while the main is out of reach.
//
// The link also switches the main (family.go): it makes this server the
// main of its family, follows a higher main, and, on a main that took the
// role over, tells the other servers of the family until they follow.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// The roles, as the protocol names them.
const (
	RoleMain        = wsclient.RoleMain
	RoleSubordinate = wsclient.RoleSubordinate
)

// Reasons a link stops or a pairing is refused, beside the protocol's
// codes (wsclient.Error codes).
const (
	// ReasonNotMain: the upstream server is not a main: a subordinate, or a
	// server from before roles. Only a main may be an upstream.
	ReasonNotMain = "notMain"
	// ReasonServerChanged: another server answers at the main's addresses.
	ReasonServerChanged = "serverChanged"
	// ReasonTooManyWrites: the write guard stopped an app (see
	// MaxPushesPerMinute).
	ReasonTooManyWrites = "tooManyWrites"
	// ReasonUnreachable: the main is out of reach now, and what was asked
	// needs it (Invite).
	ReasonUnreachable = "unreachable"
)

// The write guard, a last line of defence against a write loop, as in the
// client package's engine: one app's pushes to the main are counted over a
// sliding minute, and a push past MaxPushesPerMinute stops that app's sync
// until the link starts again (the server starts, or restarts after a reset
// or a data-folder move).
const (
	MaxPushesPerMinute = 10
	pushWindow         = time.Minute
)

// Defaults for Deps left zero.
const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = 30 * time.Second
	// blockedWait is how long the link waits after the main blocked its
	// address, which lasts 10 minutes at most.
	blockedWait = time.Minute
	// stableAfter: a session that lasted this long resets the backoff.
	stableAfter = 30 * time.Second
	// syncAttempts bounds how often one sync decides again after the main or
	// a device here wrote first. Whoever wrote also left the worker a reason
	// to sync again (a changed push, or a local write), so a sync that runs
	// out of attempts is followed by another.
	syncAttempts = 4
	// inviteWait bounds asking the main for a pairing code.
	inviteWait = 10 * time.Second
	// forgetBudget bounds asking the main, when unlinking, to forget this
	// server: reaching it, hello and forget.
	forgetBudget = 5 * time.Second
)

// errTooManyWrites: the write guard refused a push.
var errTooManyWrites = fmt.Errorf("more than %d pushes to the main within a minute (%s)", MaxPushesPerMinute, ReasonTooManyWrites)

// Errors of Pair, Unlink and Invite that are not the main's.
var (
	ErrLinked    = errors.New("this server is already linked to an upstream server")
	ErrNotLinked = errors.New("this server is not linked to an upstream server")
	ErrBusy      = errors.New("a link to an upstream server is being made or undone")
	ErrBadCode   = errors.New("this is not a pairing code of a workspace-server")
	ErrOwnCode   = errors.New("the pairing code is this server's own")
)

// RefusedError is a pairing the upstream server refused or that could not
// reach it. Reason is a protocol code (wsclient), or ReasonNotMain or
// ReasonServerChanged.
type RefusedError struct {
	Reason string
	Err    error
}

func (e *RefusedError) Error() string { return "the upstream server: " + e.Err.Error() }
func (e *RefusedError) Unwrap() error { return e.Err }

// Writer is what the link writes through: the hub, which stores a document
// and tells this server's sessions, tells link sessions of this server when
// its app list changes, and changes this server's own devices and sessions
// when it becomes the main or steps down.
type Writer interface {
	Replace(app string, body []byte, ifEtag *string) (string, error)
	AppsChanged()
	// Adopt merges a registry of the family into this server's own devices.
	Adopt(list []devices.Entry, removed []devices.Tombstone, self devices.Self) (bool, error)
	// ClearDevices forgets this server's own devices, handed over to its main.
	ClearDevices() error
	// MintServer gives a server a server-kind device here, and its token.
	MintServer(s devices.Server) (device, token string, err error)
	// Linked reports whether the server of that id has a link session here.
	Linked(serverID string) bool
	// CloseServers closes the sessions of server-kind devices but except's.
	CloseServers(except string)
}

// Deps are what the link works with.
type Deps struct {
	Path       string // upstream.json
	FamilyPath string // family.json: a main's term, and whom it waits for
	// Devices is this server's device registry, which the link reads when it
	// switches the main; its changes go through Writer.
	Devices  *devices.Registry
	ServerID string        // this server's own, to refuse its own code
	Name     func() string // this server's name, which the main shows
	Apps     *apps.Registry
	Store    *store.Store
	Writer   Writer
	// Dial opens a session to the main for app ("" for the link session).
	Dial func(ctx context.Context, t wsclient.Target, app string) (*wsclient.Client, error)
	// Self, if set, is how devices reach this server, read at every
	// announce: the link announces it to a main that takes announce.
	Self func() wsclient.Server
	// Admit, if set, is given the main's device registry whenever it
	// changes, and the one upstream.json kept when the link opens: the
	// devices this server admits on the main's word (devices.SetReplica).
	Admit func(list []devices.Entry)
	Now   func() time.Time
	Logf  func(format string, args ...any)
	// MinBackoff and MaxBackoff bound the wait before reconnecting; 0 means
	// 1 s and 30 s.
	MinBackoff, MaxBackoff time.Duration
}

// Status is what the page shows about the link.
type Status struct {
	Linked    bool       `json:"linked"`
	ServerID  string     `json:"serverId,omitempty"`
	Name      string     `json:"name,omitempty"`
	Hosts     []string   `json:"hosts,omitempty"`
	LinkedAt  *time.Time `json:"linkedAt,omitempty"`
	Connected bool       `json:"connected"`
	// Halted: the link stopped and does not retry until the server starts
	// again; Reason says why.
	Halted    bool   `json:"halted"`
	Reason    string `json:"reason,omitempty"`
	LastError string `json:"lastError,omitempty"`
	// LastSync: the last time a document moved between the two servers: a
	// pull, a push, or the main's copy replacing this server's.
	LastSync *time.Time `json:"lastSync,omitempty"`
	Apps     []string   `json:"apps"` // the apps synced now
	// Stopped lists the apps the write guard stopped, sorted; they do not
	// sync until the link starts again.
	Stopped []string `json:"stopped,omitempty"`
	// Devices counts the main's devices of apps this server admits: the
	// registry the main last sent, without the other servers.
	Devices int `json:"devices,omitempty"`
	// Term is the family's term as this server knows it.
	Term int `json:"term"`
	// Waiting lists, on a main that took the role over, the servers of the
	// family it waits for to follow it.
	Waiting []Waiting `json:"waiting,omitempty"`
	// Promoting: this server is becoming the main now.
	Promoting bool `json:"promoting,omitempty"`
}

// Link is a server's link to its upstream server. It is safe for concurrent
// use.
type Link struct {
	d Deps

	mu        sync.Mutex
	st        *state  // nil: not linked
	fam       *family // a main's family.json; nil on a subordinate, and on a main of term 0
	pairing   bool
	unlinking bool
	promoting bool
	connected bool
	halted    bool
	reason    string
	lastErr   string
	lastSync  *time.Time
	connects  int // link sessions opened, for tests
	syncing   []string
	pushes    map[string][]time.Time // per app, its pushes within the guard's window
	stopped   map[string]bool        // the apps the write guard stopped
	up        map[string]*string     // per app, the main's etag as last heard
	cancel    context.CancelFunc
	closed    bool

	watchCancel context.CancelFunc // the family watcher, on a main that waits for servers

	kick      chan struct{} // reconnect now
	watchKick chan struct{} // the family watcher: try again now
	announce  chan struct{} // announce again: how devices reach this server changed
	invites   chan inviteReq
	wg        sync.WaitGroup
	bg        sync.WaitGroup // a switch the main asked for (takeOver)

	// localMu guards local alone, so a device's write never waits for the
	// link's own work under mu (saving upstream.json, say).
	localMu sync.Mutex
	local   map[string]chan struct{} // per app, a write by a device here
}

// Open loads the link from deps.Path; a missing file is a main's state. It
// starts nothing: Start does.
func Open(d Deps) (*Link, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.MinBackoff <= 0 {
		d.MinBackoff = defaultMinBackoff
	}
	if d.MaxBackoff <= 0 {
		d.MaxBackoff = defaultMaxBackoff
	}
	st, err := load(d.Path)
	if err != nil {
		return nil, err
	}
	var fam *family
	if st == nil {
		if fam, err = loadFamily(d.FamilyPath); err != nil {
			return nil, err
		}
	}
	if st != nil && d.Admit != nil {
		// Admitted at once, before the main is reached: that is what lets
		// its devices sync here while it is away.
		d.Admit(slices.Clone(st.Devices))
	}
	return &Link{
		d: d, st: st, fam: fam, kick: make(chan struct{}, 1), watchKick: make(chan struct{}, 1),
		announce: make(chan struct{}, 1), invites: make(chan inviteReq),
		pushes: map[string][]time.Time{}, stopped: map[string]bool{}, up: map[string]*string{}, local: map[string]chan struct{}{},
	}, nil
}

// Role is this server's role: a subordinate while it is linked.
func (l *Link) Role() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st != nil {
		return RoleSubordinate
	}
	return RoleMain
}

// Start runs the link, when there is one, and the family watcher, on a main
// that waits for servers to follow it, until Close.
func (l *Link) Start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st != nil {
		l.startLocked()
	}
	l.startWatchLocked()
}

func (l *Link) startLocked() {
	if l.closed || l.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.halted, l.reason = false, ""
	clear(l.stopped)
	clear(l.pushes)
	select { // a change before the link ran asks for nothing
	case <-l.kick:
	default:
	}
	l.wg.Go(func() { l.run(ctx) })
}

// Close stops the link and waits until nothing of it runs. Later calls do
// nothing.
func (l *Link) Close() {
	l.mu.Lock()
	l.closed = true
	cancel, watch := l.cancel, l.watchCancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if watch != nil {
		watch()
	}
	l.bg.Wait()
	l.wg.Wait()
}

// NetworkChanged tells the link that this server's addresses changed: it
// reconnects at once, and stops waiting out a backoff.
func (l *Link) NetworkChanged() {
	poke(l.kick)
	poke(l.watchKick)
}

// SelfChanged tells the link that how devices reach this server changed
// (its name or its UDP port): it announces itself again to the main over
// the link session it holds. An address change needs no call: the link
// reconnects then, and every link session announces. It never blocks.
func (l *Link) SelfChanged() { poke(l.announce) }

// LocalWrite tells the link that a device stored a document of app on this
// server, so that the app's worker syncs it with the main at once (a push,
// usually). The hub calls it after every device's write and never after the
// link's own, which would go back where they came from. It never blocks.
// Before the app's first session to the main, and on a main, it does
// nothing: every session syncs once it opens anyway.
func (l *Link) LocalWrite(app string) {
	l.localMu.Lock()
	ch := l.local[app]
	l.localMu.Unlock()
	if ch != nil {
		poke(ch)
	}
}

// localWrites returns the channel LocalWrite pokes for app, creating it.
func (l *Link) localWrites(app string) chan struct{} {
	l.localMu.Lock()
	defer l.localMu.Unlock()
	ch := l.local[app]
	if ch == nil {
		ch = make(chan struct{}, 1)
		l.local[app] = ch
	}
	return ch
}

// poke puts a token into a channel of one, unless one waits there already.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Status reports the link.
func (l *Link) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := Status{Linked: l.st != nil, Apps: []string{}, Promoting: l.promoting}
	st.Term, _ = l.keyLocked()
	for _, id := range l.syncing {
		if !l.stopped[id] {
			st.Apps = append(st.Apps, id)
		}
	}
	if l.st == nil {
		st.Waiting = l.waitingLocked()
		return st
	}
	at := l.st.LinkedAt
	st.ServerID, st.Name, st.Hosts, st.LinkedAt = l.st.ServerID, l.st.Name, append([]string(nil), l.st.Hosts...), &at
	for _, d := range l.st.Devices {
		if !d.IsServer() {
			st.Devices++
		}
	}
	st.Connected, st.Halted, st.Reason, st.LastError, st.LastSync = l.connected, l.halted, l.reason, l.lastErr, l.lastSync
	for id := range l.stopped {
		st.Stopped = append(st.Stopped, id)
	}
	slices.Sort(st.Stopped)
	return st
}

// Pair links this server to the main whose pairing code is code: it pairs as
// a server-kind device, refuses a server that is not a main, keeps the
// pairing in upstream.json and starts the link. A server already linked is
// ErrLinked. The main's refusals and a main out of reach are a
// *RefusedError.
func (l *Link) Pair(ctx context.Context, code string) error {
	c, err := wsclient.ParseCode(code)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadCode, err)
	}
	if c.ServerID == l.d.ServerID {
		return ErrOwnCode
	}
	l.mu.Lock()
	switch {
	case l.st != nil:
		l.mu.Unlock()
		return ErrLinked
	case l.pairing || l.unlinking || l.promoting:
		l.mu.Unlock()
		return ErrBusy
	}
	l.pairing = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.pairing = false
		l.mu.Unlock()
	}()

	st, err := l.pair(ctx, c)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st, l.lastErr = st, ""
	if l.watchCancel != nil {
		l.watchCancel()
		l.watchCancel = nil
	}
	if l.fam != nil {
		// This server joins another family: what it kept of its own goes.
		l.fam = nil
		if err := removeFile(l.d.FamilyPath); err != nil {
			l.d.Logf("delete %s: %v", l.d.FamilyPath, err)
		}
	}
	l.d.Logf("linked to the main %q (%s) as device %s", st.Name, st.ServerID, st.Device)
	l.startLocked()
	return nil
}

// pair runs the pairing session and returns the saved state. The pairing
// is confirmed (by asking for the app list) only once upstream.json holds
// the token: a pairing the main keeps is never one this server lost. A main
// that is not one is left unconfirmed, which undoes the pairing there.
func (l *Link) pair(ctx context.Context, c wsclient.PairingCode) (*state, error) {
	cl, err := l.d.Dial(ctx, c.Target, "")
	if err != nil {
		return nil, refused(err)
	}
	defer cl.Close()
	p, w, err := cl.Pair(ctx, wsclient.KindServer, c.Secret, l.d.Name())
	if err != nil {
		return nil, refused(err)
	}
	if err := checkWelcome(w, c.ServerID); err != nil {
		return nil, err
	}
	hosts := w.Hosts
	if len(hosts) == 0 {
		hosts = c.Hosts
	}
	st := &state{
		ServerID: w.ServerID, Name: w.Name,
		Password: c.Password, Fingerprint: c.Fingerprint, Port: c.Port, Hosts: hosts,
		Device: p.Device, Token: p.Token, LinkedAt: l.d.Now().UTC(),
		Apps: map[string]appState{}, Term: w.Term,
	}
	if err := st.save(l.d.Path); err != nil {
		return nil, fmt.Errorf("save %s: %w", l.d.Path, err)
	}
	if _, err := cl.Apps(ctx); err != nil {
		// The main may not have heard the confirmation, and then it drops
		// the pairing: so does this server.
		_ = removeFile(l.d.Path)
		return nil, refused(err)
	}
	return st, nil
}

// Unlink ends the link: this server becomes a main again. Its data stays:
// the apps it took from the main stay listed here, with their documents and
// history, and the main keeps its own. The main's devices are no longer
// admitted here (Deps.Admit gets nil), and upstream.json is deleted. When
// the link is connected, the main is then asked to forget this server, so
// that it stops handing it out to its devices; forgotten reports whether the
// main no longer knows this server, because it forgot it now or had removed
// it already. A main that could not be told keeps the server-kind device
// until it is removed on its page. A server that is not linked is
// ErrNotLinked; one that is linking or unlinking meanwhile, ErrBusy.
func (l *Link) Unlink(ctx context.Context) (forgotten bool, err error) {
	l.mu.Lock()
	switch {
	case l.st == nil:
		l.mu.Unlock()
		return false, ErrNotLinked
	case l.pairing || l.unlinking || l.promoting || l.closed:
		l.mu.Unlock()
		return false, ErrBusy
	}
	l.unlinking = true
	st, connected, cancel := l.st.clone(), l.connected, l.cancel
	forgotten = l.halted && l.reason == "badToken" // the main removed this server
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.unlinking = false
		l.mu.Unlock()
	}()

	// First the link stops, so nothing syncs, reconnects or saves
	// upstream.json any more.
	if cancel != nil {
		cancel()
	}
	l.wg.Wait()
	if err := removeFile(l.d.Path); err != nil {
		// Still linked, as the file says: the link starts again.
		l.mu.Lock()
		l.cancel = nil
		l.startLocked()
		l.mu.Unlock()
		return false, fmt.Errorf("delete %s: %w", l.d.Path, err)
	}
	l.mu.Lock()
	l.st, l.cancel = nil, nil
	l.connected, l.halted, l.reason, l.lastErr, l.lastSync = false, false, "", "", nil
	l.syncing = nil
	clear(l.stopped)
	clear(l.pushes)
	clear(l.up)
	l.mu.Unlock()
	if l.d.Admit != nil {
		l.d.Admit(nil)
	}

	if connected && !forgotten {
		forgotten = l.forget(ctx, st)
	}
	if forgotten {
		l.d.Logf("unlinked from the main %q (%s), which no longer knows this server; this server is a main again, and its data stays", st.Name, st.ServerID)
	} else {
		l.d.Logf("unlinked from the main %q (%s), which could not be told: it lists this server until it is removed on its page; this server is a main again, and its data stays", st.Name, st.ServerID)
	}
	return forgotten, nil
}

// forget asks the main, over a link session of its own, to forget this
// server, within forgetBudget. It reports whether the main no longer knows
// it: the main closed the session after forget, which it does once the
// device is gone, or refused the hello with badToken.
func (l *Link) forget(ctx context.Context, st *state) bool {
	ctx, cancel := context.WithTimeout(ctx, forgetBudget)
	defer cancel()
	cl, err := l.d.Dial(ctx, st.target(), "")
	if err != nil {
		return false
	}
	defer cl.Close()
	w, err := cl.Hello(ctx, st.Device, st.Token)
	if err != nil {
		return wsclient.HasCode(err, "badToken")
	}
	if checkWelcome(w, st.ServerID) != nil {
		return false
	}
	_ = cl.Forget(ctx)
	select {
	case <-cl.Done():
		return true
	default:
		return false
	}
}

type inviteReq struct{ reply chan inviteResult }

type inviteResult struct {
	inv wsclient.Invitation
	err error
}

// Invite asks the main, over the link session, for a pairing code of its
// own, the one its page's "Pair a device" would show, for a browser on this
// computer: the browser pairs with the main, and learns this server from the
// main's welcome. It needs the main: a link that is not connected now is a
// *RefusedError with ReasonUnreachable, and a halted one names why it
// halted. A server that is not linked is ErrNotLinked.
func (l *Link) Invite(ctx context.Context) (wsclient.Invitation, error) {
	l.mu.Lock()
	linked, connected, halted, reason, lastErr := l.st != nil, l.connected, l.halted, l.reason, l.lastErr
	l.mu.Unlock()
	switch {
	case !linked:
		return wsclient.Invitation{}, ErrNotLinked
	case halted:
		return wsclient.Invitation{}, &RefusedError{Reason: reason, Err: errors.New(lastErr)}
	case !connected:
		return wsclient.Invitation{}, errUnreachable()
	}
	req := inviteReq{reply: make(chan inviteResult, 1)}
	select {
	case l.invites <- req:
	case <-ctx.Done():
		return wsclient.Invitation{}, errUnreachable()
	}
	select {
	case r := <-req.reply:
		return r.inv, r.err
	case <-ctx.Done():
		return wsclient.Invitation{}, errUnreachable()
	}
}

func errUnreachable() error {
	return &RefusedError{Reason: ReasonUnreachable, Err: errors.New("the main is out of reach now, and only it issues pairing codes")}
}

// invite asks the main for a pairing code over its link session cl, and
// checks that the code is the main's own.
func (l *Link) invite(ctx context.Context, cl *wsclient.Client, mainID string) inviteResult {
	ctx, cancel := context.WithTimeout(ctx, inviteWait)
	defer cancel()
	inv, err := cl.Invite(ctx)
	if err != nil {
		return inviteResult{err: refused(err)}
	}
	if c, err := wsclient.ParseCode(inv.Code); err != nil || c.ServerID != mainID {
		return inviteResult{err: &RefusedError{Reason: wsclient.CodeProtocol, Err: errors.New("the main sent a pairing code that is not its own")}}
	}
	l.d.Logf("the main issued a pairing code for a browser on this computer, valid until %s", inv.ExpiresAt.Format(time.RFC3339))
	return inviteResult{inv: inv}
}

// checkWelcome refuses a welcome from a server other than id, or one that is
// not a main. A main that answers as a subordinate now stepped down to
// another main: ReasonMainSteppedDown, after which the link looks for the
// new main.
func checkWelcome(w wsclient.Welcome, id string) error {
	if w.ServerID != id {
		return &RefusedError{Reason: ReasonServerChanged, Err: fmt.Errorf("another server (%s) answers at the main's addresses", w.ServerID)}
	}
	if w.Role == RoleSubordinate {
		return &RefusedError{Reason: ReasonMainSteppedDown, Err: fmt.Errorf("the main %q stepped down to another main; looking for it among the servers of the family", w.Name)}
	}
	if w.Role != RoleMain {
		role := w.Role
		if role == "" {
			role = "a server too old to have subordinates"
		}
		return &RefusedError{Reason: ReasonNotMain, Err: fmt.Errorf("the server %q is not a main but %s; only a main can be an upstream server", w.Name, role)}
	}
	return nil
}

// refused wraps an error from the main, or from reaching it.
func refused(err error) error {
	var re *RefusedError
	if errors.As(err, &re) {
		return err
	}
	var we *wsclient.Error
	if errors.As(err, &we) {
		return &RefusedError{Reason: we.Code, Err: err}
	}
	return &RefusedError{Reason: wsclient.CodeClosed, Err: err}
}

// halts reports whether err stops the link for good: the main no longer
// knows this server, is not a main, is another server, or broke the
// protocol.
func halts(err error) bool {
	var re *RefusedError
	if errors.As(err, &re) {
		switch re.Reason {
		case ReasonNotMain, ReasonServerChanged, "badToken", "unknownApp", wsclient.CodeProtocol:
			return true
		}
	}
	return false
}

// run keeps the link up until ctx ends or the link halts, waiting longer
// after each failure, and not at all after the network changed.
func (l *Link) run(ctx context.Context) {
	delay := l.d.MinBackoff
	for {
		started := time.Now()
		err := l.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if halts(err) {
			l.halt(err)
			return
		}
		var re *RefusedError
		if errors.As(err, &re) && re.Reason == ReasonMainSteppedDown {
			l.failed(err)
			l.setReason(ReasonMainSteppedDown)
			if l.findMain(ctx) {
				l.setReason("")
				delay = l.d.MinBackoff
				continue
			}
		} else {
			l.setReason("")
		}
		kicked := errors.Is(err, errKicked)
		if !kicked {
			l.failed(err)
		}
		if time.Since(started) >= stableAfter || kicked {
			delay = l.d.MinBackoff
		}
		wait := delay
		if wsclient.HasCode(err, "blocked") {
			wait = blockedWait
		}
		if !kicked && !l.sleep(ctx, wait) {
			return
		}
		delay = min(2*delay, l.d.MaxBackoff)
	}
}

var errKicked = errors.New("the network changed")

// sleep waits for d, the network to change or ctx to end; it reports false
// for the last.
func (l *Link) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-l.kick:
	case <-ctx.Done():
		return false
	}
	return true
}

func (l *Link) halt(err error) {
	var re *RefusedError
	errors.As(err, &re)
	l.mu.Lock()
	l.halted, l.reason, l.lastErr = true, re.Reason, err.Error()
	l.d.Logf("the link to the main stopped: %v", err)
	// badToken: the main removed this server, so its registry is no longer
	// this server's to admit by.
	forget := re.Reason == "badToken" && len(l.st.Devices) > 0
	if forget {
		l.st.Devices = nil
		l.saveLocked()
		l.d.Logf("the main's devices are no longer admitted here")
	}
	l.mu.Unlock()
	if forget && l.d.Admit != nil {
		l.d.Admit(nil)
	}
}

// setReason records why the link is not connected while it is not halted:
// ReasonMainSteppedDown while it looks for the new main, "" otherwise.
func (l *Link) setReason(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.halted {
		l.reason = reason
	}
}

func (l *Link) failed(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if msg := err.Error(); msg != l.lastErr {
		l.lastErr = msg
		l.d.Logf("the link to the main: %v; retrying", err)
	}
}

func (l *Link) snapshot() *state {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st.clone()
}

// session holds one link session and a session per app the main serves,
// until one of them fails for good, the link session ends, the network
// changes or ctx ends.
func (l *Link) session(ctx context.Context) error {
	st := l.snapshot()
	cl, err := l.d.Dial(ctx, st.target(), "")
	if err != nil {
		return refused(err)
	}
	defer cl.Close()
	lists := make(chan []wsclient.App, 1)
	cl.OnApps(func(list []wsclient.App) { latest(lists, list) })
	registries := make(chan wsclient.Registry, 1)
	cl.OnDevices(func(reg wsclient.Registry) { latest(registries, reg) })
	takeOvers := make(chan struct{}, 1)
	cl.OnTakeOver(func() { poke(takeOvers) })
	w, err := cl.Hello(ctx, st.Device, st.Token)
	if err != nil {
		return refused(err)
	}
	if err := checkWelcome(w, st.ServerID); err != nil {
		return err
	}
	l.keepHosts(w.Hosts)
	l.keepTerm(w.Term)
	if _, err := cl.Apps(ctx); err != nil {
		return refused(err)
	}
	// A main that lists subordinates takes announce; one from before does
	// not, and this server then stays unknown to its devices.
	announces := w.Subordinates != nil && l.d.Self != nil
	if announces {
		select { // this announce carries the change
		case <-l.announce:
		default:
		}
		// A server that stepped down hands its own registry over, once.
		var handover *wsclient.Registry
		if st.Handover && l.d.Devices != nil {
			handover = l.ownRegistry()
		}
		reg, err := cl.Announce(ctx, l.d.Self(), handover)
		if err != nil {
			return refused(err)
		}
		if handover != nil {
			l.replicate(reg)
			if err := l.d.Writer.ClearDevices(); err != nil {
				l.d.Logf("forget this server's own devices, handed over to the main: %v", err)
			} else {
				l.mu.Lock()
				if l.st != nil {
					l.st.Handover = false
					l.saveLocked()
				}
				l.mu.Unlock()
				l.d.Logf("this server's own devices are the main's now (%d devices, %d removed)", len(handover.Devices), len(handover.Removed))
			}
		}
	}
	l.setConnected(true)
	defer l.setConnected(false)

	sctx, cancel := context.WithCancel(ctx)
	fatal := make(chan error, 1)
	workers := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		l.setSyncing(nil)
	}()
	for {
		select {
		case list := <-lists:
			want := l.adopt(list)
			for id, stop := range workers {
				if !slices.Contains(want, id) {
					stop()
					delete(workers, id)
					l.d.Logf("app %q: the main no longer serves it; it stops syncing, and its data here stays", id)
				}
			}
			for _, id := range want {
				if workers[id] == nil {
					wctx, stop := context.WithCancel(sctx)
					workers[id] = stop
					wg.Go(func() { l.worker(wctx, id, fatal) })
				}
			}
			l.setSyncing(want)
		case reg := <-registries:
			l.replicate(reg)
		case <-takeOvers:
			// The main hands its role to this server: a planned switch,
			// beside the link, which it stops.
			l.bg.Go(func() {
				followed, err := l.Promote(context.Background(), false)
				switch {
				case err != nil:
					l.d.Logf("the main asked this server to become the main, which failed: %v", err)
				case !followed:
					l.d.Logf("the main asked this server to become the main; the former main had not followed yet")
				}
			})
		case <-l.announce:
			if !announces {
				continue
			}
			if _, err := cl.Announce(ctx, l.d.Self(), nil); err != nil {
				return refused(err)
			}
		case req := <-l.invites:
			// Beside the loop: the main's answer is awaited without holding
			// up the app list, the registry or a failure.
			wg.Go(func() { req.reply <- l.invite(sctx, cl, st.ServerID) })
		case err := <-fatal:
			return err
		case <-cl.Done():
			return refused(cl.Err())
		case <-l.kick:
			return errKicked
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// latest puts v into a channel of one, replacing what waits there.
func latest[T any](ch chan T, v T) {
	for {
		select {
		case ch <- v:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

// replicate keeps the main's device registry: in upstream.json, so that a
// restart still admits the main's devices while the main is out of reach,
// and should this server become the main, and with Admit. An entry that is
// not valid is left out.
func (l *Link) replicate(reg wsclient.Registry) {
	entries, removed := l.toEntries(reg)
	l.mu.Lock()
	if l.st == nil || (devices.Same(l.st.Devices, entries) && slices.Equal(l.st.Removed, removed)) {
		l.mu.Unlock()
		return
	}
	l.st.Devices, l.st.Removed = entries, removed
	l.saveLocked()
	l.d.Logf("the main's device registry: %d devices admitted here", len(entries))
	l.mu.Unlock()
	if l.d.Admit != nil {
		l.d.Admit(slices.Clone(entries))
	}
}

// adopt takes the main's app list: it adds the apps this server does not
// serve yet to apps.json, forgets the agreed etag of the apps the main no
// longer serves (a later return is a first link), and returns the ids to
// sync.
func (l *Link) adopt(list []wsclient.App) []string {
	var want []string
	added := false
	for _, a := range list {
		if !apps.ValidID(a.ID) {
			continue
		}
		if _, ok := l.d.Apps.Get(a.ID); !ok {
			err := l.d.Apps.Add(apps.App{ID: a.ID, Name: a.Name})
			switch {
			case err == nil:
				added = true
				l.d.Logf("app %q added: the main serves it", a.ID)
			case !errors.Is(err, apps.ErrExists):
				l.d.Logf("app %q: cannot add it, so it does not sync: %v", a.ID, err)
				continue
			}
		}
		want = append(want, a.ID)
	}
	if added {
		l.d.Writer.AppsChanged()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for id := range l.st.Apps {
		if !slices.Contains(want, id) {
			delete(l.st.Apps, id)
			changed = true
		}
	}
	if changed {
		l.saveLocked()
	}
	slices.Sort(want)
	return want
}

// worker keeps app's session to the main up and syncs over it until ctx
// ends. A failure that stops the whole link goes to fatal. An app the write
// guard stopped does not run until the link starts again.
func (l *Link) worker(ctx context.Context, app string, fatal chan<- error) {
	if l.isStopped(app) {
		return
	}
	delay := l.d.MinBackoff
	last := ""
	for {
		started := time.Now()
		err := l.appSession(ctx, app)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errTooManyWrites) {
			l.stop(app)
			return
		}
		if halts(err) && !wsclient.HasCode(err, "unknownApp") {
			select {
			case fatal <- err:
			default:
			}
			return
		}
		if msg := err.Error(); msg != last {
			last = msg
			l.d.Logf("app %q: %v; retrying", app, err)
		}
		if time.Since(started) >= stableAfter {
			delay = l.d.MinBackoff
		}
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		delay = min(2*delay, l.d.MaxBackoff)
	}
}

// appSession syncs app once at the welcome, and again at every changed push
// from the main and every write by a device here, until the session ends.
// The sync at the welcome is what sends the main the writes made here while
// the two could not reach each other.
func (l *Link) appSession(ctx context.Context, app string) error {
	st := l.snapshot()
	cl, err := l.d.Dial(ctx, st.target(), app)
	if err != nil {
		return refused(err)
	}
	defer cl.Close()
	changed := make(chan string, 1)
	cl.OnChanged(func(etag string) { latest(changed, etag) })
	w, err := cl.Hello(ctx, st.Device, st.Token)
	if err != nil {
		return refused(err)
	}
	if err := checkWelcome(w, st.ServerID); err != nil {
		return err
	}
	local := l.localWrites(app)
	select { // the sync below reads what was written before
	case <-local:
	default:
	}
	l.setUp(app, w.Etag)
	up, err := l.sync(ctx, cl, app, w.Etag)
	for err == nil {
		l.setUp(app, up)
		select {
		case etag := <-changed:
			up = &etag
		case <-local:
			select { // the main's newest etag, if one came meanwhile
			case etag := <-changed:
				up = &etag
			default:
			}
		case <-cl.Done():
			return refused(cl.Err())
		case <-ctx.Done():
			return nil
		}
		l.setUp(app, up)
		up, err = l.sync(ctx, cl, app, up)
	}
	return err
}

// mainDoc is the main's document as one sync read it.
type mainDoc struct {
	etag *string
	body []byte
}

// sync brings app in line with the main, whose document the link last knew
// to have etag up (nil: none), and returns the etag it knows the main's
// document to have now. With this server's etag local and base the etag the
// two last agreed on (if they ever did):
//
//   - up = local: that is the agreed base;
//   - the main has no document, or only this server's copy moved (up is the
//     base): push: put this server's document to the main if the main's is
//     still up; on a conflict, decide again with the main's etag;
//   - only the main moved (local is the base, or this server has none):
//     pull: write the main's document here if this server's copy is still
//     local, telling this server's sessions;
//   - both moved, or they never agreed and differ: the main's copy wins:
//     written here the same way, and this server's goes to history. The
//     devices here merge it with what they hold, as they always do, and
//     write the result back, which the next sync pushes.
//
// Every write is conditional, so a stale up costs a decision, never a
// document: a write here that loses the race with a device decides again,
// and so does a main's document that is not the one up said.
func (l *Link) sync(ctx context.Context, cl *wsclient.Client, app string, up *string) (*string, error) {
	var got *mainDoc
	for range syncAttempts {
		body, local, err := l.d.Store.Read(app)
		if err != nil {
			return up, err
		}
		base, agreed := l.base(app)
		switch {
		case same(up, local):
			l.agree(app, local)
			return up, nil
		case up == nil || (local != nil && agreed && same(up, base)):
			if !l.allowPush(app) {
				return up, errTooManyWrites
			}
			stored, etag, err := cl.Put(ctx, up, body)
			if err != nil {
				return up, refused(err)
			}
			if !stored {
				l.d.Logf("app %q: the main's document changed before this server's reached it (it has %s now); deciding again", app, short(etag))
				up = etag
				continue
			}
			l.moved(app, *etag, "pushed this server's document %s to the main", short(etag))
			return etag, nil
		default:
			if got == nil || !same(got.etag, up) {
				etag, doc, err := cl.Get(ctx)
				if err != nil {
					return up, refused(err)
				}
				got = &mainDoc{etag, doc}
				if !same(etag, up) { // the main's copy moved again meanwhile
					up = etag
					continue
				}
			}
			stored, err := l.d.Writer.Replace(app, got.body, local)
			var conflict *store.ConflictError
			if errors.As(err, &conflict) {
				continue // a device here wrote first
			}
			if err != nil {
				return up, err
			}
			if local == nil || (agreed && same(local, base)) {
				l.moved(app, stored, "pulled the main's document %s", short(&stored))
			} else {
				was := "they were never agreed on"
				if agreed {
					was = "both changed since they agreed on " + short(base)
				}
				l.moved(app, stored, "the main's document %s replaced this server's %s (%s); this server's is kept in history, and the devices here merge it back", short(&stored), short(local), was)
			}
			return up, nil
		}
	}
	return up, nil
}

func same(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// short is an etag's first 8 digits, for the log.
func short(e *string) string {
	if e == nil {
		return "none"
	}
	return (*e)[:min(8, len(*e))]
}

// base returns the etag app last agreed on, and whether it ever agreed.
func (l *Link) base(app string) (*string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.st.Apps[app]
	return a.Base, ok
}

// agree records etag as app's agreed base.
func (l *Link) agree(app string, etag *string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.st.Apps[app]; ok && same(a.Base, etag) {
		return
	}
	l.st.Apps[app] = appState{Base: etag}
	l.saveLocked()
}

// moved records that a document moved between the two servers, after which
// both have etag, and logs what happened.
func (l *Link) moved(app, etag string, format string, args ...any) {
	now := l.d.Now().UTC()
	l.agree(app, &etag)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSync = &now
	l.d.Logf("app %q: "+format, append([]any{app}, args...)...)
}

// allowPush counts a push of app, and refuses it when MaxPushesPerMinute
// pushes of app happened within the last minute.
func (l *Link) allowPush(app string) bool {
	now := l.d.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := slices.DeleteFunc(l.pushes[app], func(at time.Time) bool { return now.Sub(at) >= pushWindow })
	if len(recent) >= MaxPushesPerMinute {
		l.pushes[app] = recent
		return false
	}
	l.pushes[app] = append(recent, now)
	return true
}

// stop records that the write guard stopped app.
func (l *Link) stop(app string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped[app] = true
	l.d.Logf("app %q: stopped syncing with the main until this server starts again: %v. The documents on both sides stay as they are", app, errTooManyWrites)
}

func (l *Link) isStopped(app string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped[app]
}

// keepHosts stores the main's hosts from a welcome, when they changed.
func (l *Link) keepHosts(hosts []string) {
	if len(hosts) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if slices.Equal(l.st.Hosts, hosts) {
		return
	}
	l.st.Hosts = append([]string(nil), hosts...)
	l.saveLocked()
}

// saveLocked writes the state; a failure is logged, and the next change
// writes again. The caller holds mu.
func (l *Link) saveLocked() {
	if err := l.st.save(l.d.Path); err != nil {
		l.d.Logf("save %s: %v", l.d.Path, err)
	}
}

func (l *Link) setConnected(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.connected = on
	if on {
		l.lastErr = ""
		l.connects++
	}
}

func (l *Link) setSyncing(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncing = append([]string(nil), ids...)
}
