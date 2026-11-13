// Package upstream is a subordinate server's link to its main: the upstream
// server whose apps it mirrors. The link pairs with the main once, as a
// server-kind device, and keeps the pairing in upstream.json. It then holds a
// link session, which tells it the main's app list, and one session per app,
// over which it keeps the app's set of documents in line with the main's.
//
// What the link does for each app follows the sync rule in
// docs/architecture.md (sync.go): document by document, against the base
// both last agreed on (the app's .wss/link.json), it pulls what only the
// main changed, pushes what only this server changed, and when both changed,
// the main's set wins here and replaces this server's. Neither side ever
// merges a document: the devices do, as they always have, and write the
// result back.
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
	"io"
	"slices"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/hub"
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
// until it is resumed (Resume, the page's button) or the link starts again
// (the server starts, or restarts after a reset or a data-folder move).
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

// Errors of Pair, Unlink, Invite and Resume that are not the main's.
var (
	ErrLinked     = errors.New("this server is already linked to an upstream server")
	ErrNotLinked  = errors.New("this server is not linked to an upstream server")
	ErrBusy       = errors.New("a link to an upstream server is being made or undone")
	ErrBadCode    = errors.New("this is not a pairing code of a workspace-server")
	ErrOwnCode    = errors.New("the pairing code is this server's own")
	ErrNotStopped = errors.New("the write guard has not stopped this app")
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

// Writer is what the link writes through: the hub, which takes the main's
// documents in as one transaction (LinkTx) and tells this server's sessions,
// tells link sessions of this server when its app list changes, and changes
// this server's own devices and sessions when it becomes the main or steps
// down.
type Writer interface {
	// BeginLink opens the link's transaction tx on app here, or returns to
	// it with what it staged.
	BeginLink(app, tx string) (*hub.LinkTx, []store.Staged, error)
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
	// DivergedChanged tells the sessions of app this server's divergences
	// as they are now; DivergencesChanged does so for every app.
	DivergedChanged(app string)
	DivergencesChanged()
	// OwnResolved tells the sessions of app that a choice from the main
	// settled this server's divergence on it.
	OwnResolved(app string)
	// ForgetDivergences forgets what this server's subordinates told it of
	// their divergences: it follows a main now.
	ForgetDivergences()
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
	// LastSync: the last time documents moved between the two servers: a
	// pull, a push, or the main's set replacing this server's.
	LastSync *time.Time `json:"lastSync,omitempty"`
	Apps     []string   `json:"apps"` // the apps synced now
	// Diverged lists the apps whose documents diverged from the main's and
	// wait for a device's choice, sorted; they do not sync with the main
	// until one chooses.
	Diverged []string `json:"diverged,omitempty"`
	// Stopped lists the apps the write guard stopped, sorted; they do not
	// sync until they are resumed or the link starts again.
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
	pushes    map[string][]time.Time   // per app, its pushes within the guard's window
	stopped   map[string]bool          // the apps the write guard stopped
	resumes   map[string]chan struct{} // per app, poked by Resume; a stopped app's worker waits on it
	up        map[string]*string       // per app, the main's revision as last heard
	links     map[string]*appLink      // per app, link.json as last read or written
	hidden    map[string][2]*string    // per app, a main's revision and that of its keys valid here, when they differ
	cancel    context.CancelFunc
	closed    bool

	// wrapPull and wrapPush, set by tests, wrap the body of each pull and
	// push: a test cuts a transfer off with them.
	wrapPull func(w io.Writer, cl *wsclient.Client) io.Writer
	wrapPush func(r io.Reader, cl *wsclient.Client) io.Reader

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

	// divMu guards divs, each app's diverged.json as last read or written
	// (nil: none), and orders the choices made for it. It is taken under
	// the hub's app lock (OwnDiverged), so the link never calls the hub
	// holding it.
	divMu sync.Mutex
	divs  map[string]*divergence
	// divTell: an app's divergence changed, and the link session tells the
	// main.
	divTell chan struct{}

	// running is the worker of each app now, for AppRemoving, which stops
	// it; removing marks the apps being removed here, whose workers do not
	// start. relist tells the link session that a removal ended. All under
	// mu.
	running  map[string]*appWorker
	removing map[string]bool
	relist   chan struct{}
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
		pushes: map[string][]time.Time{}, stopped: map[string]bool{}, resumes: map[string]chan struct{}{},
		up: map[string]*string{}, links: map[string]*appLink{}, hidden: map[string][2]*string{}, local: map[string]chan struct{}{},
		divs: map[string]*divergence{}, divTell: make(chan struct{}, 1),
		running: map[string]*appWorker{}, removing: map[string]bool{}, relist: make(chan struct{}, 1),
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

// LocalWrite tells the link that app's set has a new revision on this
// server, so that the app's worker syncs it with the main at once (a push,
// usually). The hub calls it after every new revision, the link's own
// included: the worker finds those in step with the base and the main, and
// does nothing. It never blocks. Before the app's first session to the main,
// and on a main, it does nothing: every session syncs once it opens anyway.
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
	return l.statusDiverged(l.status())
}

// status reports the link but its divergences.
func (l *Link) status() Status {
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

// statusDiverged adds the apps that wait for a device's choice to st. It
// reads diverged.json, so it runs outside mu.
func (l *Link) statusDiverged(st Status) Status {
	if st.Linked {
		st.Diverged = l.divergedApps()
	}
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
	l.d.Writer.ForgetDivergences()
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
		Term: w.Term,
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
// the apps it took from the main stay listed here, with their documents,
// and the main keeps its own; the bases agreed with the main go. The main's devices are no longer
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
	l.forgetLinks()
	l.clearDivergences()
	l.d.Writer.DivergencesChanged()
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

// Resume undoes the write guard's stop of app: the guard forgets the app's
// pushes within its minute, so the app has MaxPushesPerMinute again, and the
// app syncs with the main at once, by the sync rule, over a new session;
// while the main is out of reach, it syncs once the link reconnects. The
// guard itself stays as it is: a write loop that goes on stops the app again.
// Other apps are left alone.
//
// A server that is not linked is ErrNotLinked. An app the guard has not
// stopped is ErrNotStopped, or an error wrapping apps.ErrNotFound when this
// server does not serve it. A halted link syncs nothing until the server
// starts again, so it resumes nothing: a *RefusedError naming why it halted.
func (l *Link) Resume(app string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.st == nil:
		return ErrNotLinked
	case !l.stopped[app]:
		if l.d.Apps != nil {
			if _, ok := l.d.Apps.Get(app); !ok {
				return fmt.Errorf("%w: %q", apps.ErrNotFound, app)
			}
		}
		return ErrNotStopped
	case l.halted:
		return &RefusedError{Reason: l.reason, Err: errors.New(l.lastErr)}
	}
	delete(l.stopped, app)
	delete(l.pushes, app)
	poke(l.resumeSignalLocked(app))
	when := "now"
	if !l.connected {
		when = "once the main is reachable"
	}
	l.d.Logf("app %q: resumed after the write guard stopped it; its count of pushes starts again, and it syncs with the main %s", app, when)
	return nil
}

// resumeSignal returns the channel Resume pokes for app, creating it.
func (l *Link) resumeSignal(app string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resumeSignalLocked(app)
}

func (l *Link) resumeSignalLocked(app string) chan struct{} {
	ch := l.resumes[app]
	if ch == nil {
		ch = make(chan struct{}, 1)
		l.resumes[app] = ch
	}
	return ch
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
	var choicesMu sync.Mutex
	var choiceList []wsclient.Choice
	choices := make(chan struct{}, 1)
	cl.OnChoice(func(c wsclient.Choice) {
		choicesMu.Lock()
		choiceList = append(choiceList, c)
		choicesMu.Unlock()
		poke(choices)
	})
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
	workers := map[string]appWorker{}
	// What this session told the main of each app's divergence, and the
	// apps the main lists.
	told := map[string]string{}
	var want []string
	var wg sync.WaitGroup
	// startWorkers starts a worker for every app the main lists that has
	// none running, but an app this server does not list (removed here, it
	// comes back with the main's next list) or that is being removed.
	startWorkers := func() {
		for _, id := range want {
			if w, ok := workers[id]; ok {
				select {
				case <-w.done: // stopped for a removal: started again
				default:
					continue
				}
			}
			delete(workers, id)
			if _, listed := l.d.Apps.Get(id); !listed || l.isRemoving(id) {
				continue
			}
			wctx, stop := context.WithCancel(sctx)
			w := appWorker{stop: stop, done: make(chan struct{})}
			workers[id] = w
			l.setRunning(id, &w)
			wg.Go(func() {
				defer close(w.done)
				defer l.setRunning(id, nil)
				l.worker(wctx, id, fatal)
			})
		}
	}
	defer func() {
		cancel()
		wg.Wait()
		l.setSyncing(nil)
	}()
	for {
		select {
		case list := <-lists:
			want = l.adopt(list)
			for id, w := range workers {
				if !slices.Contains(want, id) {
					w.stop()
					<-w.done // a sync in flight must not save its base after it is forgotten
					delete(workers, id)
					l.d.Logf("app %q: the main no longer serves it; it stops syncing, and its data here stays", id)
				}
			}
			l.forgetDropped(want)
			startWorkers()
			l.setSyncing(want)
			if err := l.tellDivergences(ctx, cl, st.ServerID, want, told); err != nil {
				return err
			}
		case <-l.relist:
			// An app's removal here has ended: one still listed here (the
			// removal failed) syncs again; one removed comes back with the
			// main's next list.
			startWorkers()
		case <-l.divTell:
			if err := l.tellDivergences(ctx, cl, st.ServerID, want, told); err != nil {
				return err
			}
		case <-choices:
			choicesMu.Lock()
			list := choiceList
			choiceList = nil
			choicesMu.Unlock()
			for _, c := range list {
				l.takeChoice(c)
			}
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
// serve yet to apps.json, gives every app the main's quota, and returns the
// ids to sync. A quota out of this server's range is left as it was, and
// logged.
func (l *Link) adopt(list []wsclient.App) []string {
	var want []string
	changed := false
	for _, a := range list {
		if !apps.ValidID(a.ID) {
			continue
		}
		if _, ok := l.d.Apps.Get(a.ID); !ok {
			err := l.d.Apps.Add(apps.App{ID: a.ID, Name: a.Name, Quota: a.Quota})
			if errors.Is(err, apps.ErrInvalid) && a.Quota != 0 {
				l.d.Logf("app %q: the main's quota of %d bytes is out of range here; it gets this server's default", a.ID, a.Quota)
				err = l.d.Apps.Add(apps.App{ID: a.ID, Name: a.Name})
			}
			switch {
			case err == nil:
				changed = true
				l.d.Logf("app %q added: the main serves it", a.ID)
			case !errors.Is(err, apps.ErrExists):
				l.d.Logf("app %q: cannot add it, so it does not sync: %v", a.ID, err)
				continue
			}
		}
		if a.Quota != 0 {
			switch moved, err := l.d.Apps.SetQuota(a.ID, a.Quota); {
			case err != nil:
				l.d.Logf("app %q: cannot take the main's quota of %d bytes: %v", a.ID, a.Quota, err)
			case moved:
				changed = true
				l.d.Logf("app %q: its quota is the main's now, %d bytes", a.ID, a.Quota)
			}
		}
		want = append(want, a.ID)
	}
	if changed {
		l.d.Writer.AppsChanged()
	}
	slices.Sort(want)
	return want
}

// forgetDropped forgets the base of the apps listed here that the main no
// longer serves (want): a later return is a first link. Their workers have
// stopped, so no sync saves link.json again after it.
func (l *Link) forgetDropped(want []string) {
	for _, a := range l.d.Apps.List() {
		if !slices.Contains(want, a.ID) {
			l.forgetLink(a.ID)
		}
	}
}

// setRunning records w as app's worker now; nil when it returned.
func (l *Link) setRunning(app string, w *appWorker) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w == nil {
		delete(l.running, app)
		return
	}
	l.running[app] = w
}

func (l *Link) isRemoving(app string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.removing[app]
}

// AppRemoving tells the link that app is about to be deleted here, its
// folder with it (link.json and diverged.json included). It stops the app's
// worker and waits until no sync of it runs, so that none writes the old
// base into the new folder or acts on a set that is gone; until done is
// called, no worker of the app starts. The caller deletes the app, and then
// calls done, which forgets what the link held of the app in memory, so
// that its next sync is a first link: an empty set here takes the main's,
// whole, rather than pushing the deletion of everything the main holds.
// The link session tells the main that no divergence of the app waits
// any more. An app this server still lists after done (the removal failed)
// syncs again at once; one removed comes back with the main's next app list.
func (l *Link) AppRemoving(app string) (done func()) {
	l.mu.Lock()
	l.removing[app] = true
	w := l.running[app]
	l.mu.Unlock()
	if w != nil {
		w.stop()
		<-w.done
	}
	return func() {
		l.mu.Lock()
		delete(l.removing, app)
		delete(l.links, app)
		delete(l.up, app)
		delete(l.hidden, app)
		delete(l.stopped, app)
		delete(l.pushes, app)
		l.mu.Unlock()
		l.divMu.Lock()
		delete(l.divs, app)
		l.divMu.Unlock()
		poke(l.relist)
		poke(l.divTell)
	}
}

// appWorker is the worker of one app: stop ends it, done closes once it
// returned.
type appWorker struct {
	stop context.CancelFunc
	done chan struct{}
}

// worker keeps app's session to the main up and syncs over it until ctx
// ends. A failure that stops the whole link goes to fatal. An app the write
// guard stopped has no session to the main: its worker waits until the app
// is resumed (Resume), and then opens one, whose welcome syncs it at once.
// A new link clears the guard (startLocked), so a restart resumes it too.
// An app that diverged from the main waits the same way, until a device
// chooses which set to keep (diverge.go), which also pokes it.
func (l *Link) worker(ctx context.Context, app string, fatal chan<- error) {
	resumed := l.resumeSignal(app)
	for {
		// Checked again after every signal: one left over from a resume
		// that found no worker waiting must not end a later stop.
		for l.isStopped(app) || l.isPaused(app) {
			select {
			case <-resumed:
			case <-ctx.Done():
				return
			}
		}
		if !l.syncApp(ctx, app, fatal) {
			return
		}
	}
}

// syncApp keeps app's session to the main up, reconnecting after a backoff,
// and reports true when the write guard stopped app or it diverged, false
// when ctx ended or the link failed for good.
func (l *Link) syncApp(ctx context.Context, app string, fatal chan<- error) bool {
	delay := l.d.MinBackoff
	last := ""
	for {
		started := time.Now()
		err := l.appSession(ctx, app)
		if ctx.Err() != nil {
			return false
		}
		if errors.Is(err, errTooManyWrites) {
			l.stop(app)
			return true
		}
		if errors.Is(err, errPaused) {
			return true // the worker waits for a device's choice
		}
		if halts(err) && !wsclient.HasCode(err, "unknownApp") {
			select {
			case fatal <- err:
			default:
			}
			return false
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
			return false
		}
		delay = min(2*delay, l.d.MaxBackoff)
	}
}

// appSession syncs app once at the welcome, and again at every changed push
// from the main and every new revision here, until the session ends. The
// sync at the welcome is what sends the main the writes made here while the
// two could not reach each other.
func (l *Link) appSession(ctx context.Context, app string) error {
	st := l.snapshot()
	cl, err := l.d.Dial(ctx, st.target(), app)
	if err != nil {
		return refused(err)
	}
	defer cl.Close()
	changed := make(chan *string, 1)
	cl.OnChanged(func(rev *string) { latest(changed, rev) })
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
	l.setUp(app, w.Revision)
	main := mainRef{ServerID: st.ServerID, Name: st.Name}
	up, err := l.sync(ctx, cl, app, main, w.Revision)
	for err == nil {
		l.setUp(app, up)
		select {
		case rev := <-changed:
			up = rev
		case <-local:
			select { // the main's newest revision, if one came meanwhile
			case rev := <-changed:
				up = rev
			default:
			}
		case <-cl.Done():
			return refused(cl.Err())
		case <-ctx.Done():
			return nil
		}
		l.setUp(app, up)
		up, err = l.sync(ctx, cl, app, main, up)
	}
	return err
}

func same(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// short is a revision's or an etag's first 8 digits, for the log.
func short(e *string) string {
	if e == nil {
		return "none"
	}
	return (*e)[:min(8, len(*e))]
}

// moved records that documents moved between the two servers, and logs
// what happened.
func (l *Link) moved(app string, format string, args ...any) {
	now := l.d.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSync = &now
	l.d.Logf("app %q: "+format, append([]any{app}, args...)...)
}

// mayPush reports whether app may push: fewer than MaxPushesPerMinute of
// its pushes were committed on the main within the last minute.
func (l *Link) mayPush(app string) bool {
	now := l.d.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := slices.DeleteFunc(l.pushes[app], func(at time.Time) bool { return now.Sub(at) >= pushWindow })
	l.pushes[app] = recent
	return len(recent) < MaxPushesPerMinute
}

// countPush counts a push of app the main committed.
func (l *Link) countPush(app string) {
	now := l.d.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pushes[app] = append(l.pushes[app], now)
}

// setComparable records, when the main's listing at revision up held keys
// that are not valid here (listed of them in all, kept of them valid), the
// revision of what is left: the set the link can hold and compare, which is
// what its base is the revision of. Without it, a main holding such a key
// would never be in step with this server.
func (l *Link) setComparable(app string, up *string, kept map[string]string, listed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(kept) == listed {
		delete(l.hidden, app)
		return
	}
	l.hidden[app] = [2]*string{up, store.RevisionOf(kept)}
}

// comparable returns the main's revision up as the link compares it with
// its base: without the keys not valid here, when the listing at up had any.
// A revision the link has not listed is taken as it is.
func (l *Link) comparable(app string, up *string) *string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, ok := l.hidden[app]; ok && same(h[0], up) {
		return h[1]
	}
	return up
}

// stop records that the write guard stopped app.
func (l *Link) stop(app string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped[app] = true
	l.d.Logf("app %q: stopped syncing with the main until it is resumed on this server's page or this server starts again: %v. The documents on both sides stay as they are", app, errTooManyWrites)
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
