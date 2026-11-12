// Package upstream is a subordinate server's link to its main: the upstream
// server whose apps it mirrors. The link pairs with the main once, as a
// server-kind device, and keeps the pairing in upstream.json. It then holds a
// link session, which tells it the main's app list, and one session per app,
// over which it pulls the main's document whenever the main's copy moved and
// this server's did not.
//
// What the link does for each app follows the sync rule in
// docs/architecture.md. This version pulls only: when this server's copy
// moved since the two last agreed, the app waits (pushing and settling a
// divergence come later; see sync).
package upstream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
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
	// pullAttempts bounds how often one sync retries a pull that lost the
	// race with a local write.
	pullAttempts = 3
)

// Errors of Pair that are not the main's.
var (
	ErrLinked  = errors.New("this server is already linked to an upstream server")
	ErrBusy    = errors.New("a link to an upstream server is being made")
	ErrBadCode = errors.New("this is not a pairing code of a workspace-server")
	ErrOwnCode = errors.New("the pairing code is this server's own")
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
// and tells this server's sessions, and tells link sessions of this server
// when its app list changes.
type Writer interface {
	Replace(app string, body []byte, ifEtag *string) (string, error)
	AppsChanged()
}

// Deps are what the link works with.
type Deps struct {
	Path     string        // upstream.json
	ServerID string        // this server's own, to refuse its own code
	Name     func() string // this server's name, which the main shows
	Apps     *apps.Registry
	Store    *store.Store
	Writer   Writer
	// Dial opens a session to the main for app ("" for the link session).
	Dial func(ctx context.Context, t wsclient.Target, app string) (*wsclient.Client, error)
	Now  func() time.Time
	Logf func(format string, args ...any)
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
	Halted    bool       `json:"halted"`
	Reason    string     `json:"reason,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	LastSync  *time.Time `json:"lastSync,omitempty"` // the last pull
	Apps      []string   `json:"apps"`               // the apps synced now
}

// Link is a server's link to its upstream server. It is safe for concurrent
// use.
type Link struct {
	d Deps

	mu        sync.Mutex
	st        *state // nil: not linked
	pairing   bool
	connected bool
	halted    bool
	reason    string
	lastErr   string
	lastSync  *time.Time
	connects  int // link sessions opened, for tests
	syncing   []string
	waiting   map[string]string // per app, the wait last logged
	cancel    context.CancelFunc
	closed    bool

	kick chan struct{} // reconnect now
	wg   sync.WaitGroup
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
	return &Link{d: d, st: st, waiting: map[string]string{}, kick: make(chan struct{}, 1)}, nil
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

// Start runs the link, when there is one, until Close.
func (l *Link) Start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st != nil {
		l.startLocked()
	}
}

func (l *Link) startLocked() {
	if l.closed || l.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.halted, l.reason = false, ""
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
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.wg.Wait()
}

// NetworkChanged tells the link that this server's addresses changed: it
// reconnects at once, and stops waiting out a backoff.
func (l *Link) NetworkChanged() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Status reports the link.
func (l *Link) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := Status{Linked: l.st != nil, Apps: append([]string{}, l.syncing...)}
	if l.st == nil {
		return st
	}
	at := l.st.LinkedAt
	st.ServerID, st.Name, st.Hosts, st.LinkedAt = l.st.ServerID, l.st.Name, append([]string(nil), l.st.Hosts...), &at
	st.Connected, st.Halted, st.Reason, st.LastError, st.LastSync = l.connected, l.halted, l.reason, l.lastErr, l.lastSync
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
	case l.pairing:
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
		Apps: map[string]appState{},
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

// checkWelcome refuses a welcome from a server other than id, or one that is
// not a main.
func checkWelcome(w wsclient.Welcome, id string) error {
	if w.ServerID != id {
		return &RefusedError{Reason: ReasonServerChanged, Err: fmt.Errorf("another server (%s) answers at the main's addresses", w.ServerID)}
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
	defer l.mu.Unlock()
	l.halted, l.reason, l.lastErr = true, re.Reason, err.Error()
	l.d.Logf("the link to the main stopped: %v", err)
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
	w, err := cl.Hello(ctx, st.Device, st.Token)
	if err != nil {
		return refused(err)
	}
	if err := checkWelcome(w, st.ServerID); err != nil {
		return err
	}
	l.keepHosts(w.Hosts)
	if _, err := cl.Apps(ctx); err != nil {
		return refused(err)
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
// ends. A failure that stops the whole link goes to fatal.
func (l *Link) worker(ctx context.Context, app string, fatal chan<- error) {
	delay := l.d.MinBackoff
	last := ""
	for {
		started := time.Now()
		err := l.appSession(ctx, app)
		if ctx.Err() != nil {
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

// appSession syncs app once at the welcome and again at every changed push,
// until the session ends.
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
	if err := l.sync(ctx, cl, app, w.Etag); err != nil {
		return err
	}
	for {
		select {
		case etag := <-changed:
			if err := l.sync(ctx, cl, app, &etag); err != nil {
				return err
			}
		case <-cl.Done():
			return refused(cl.Err())
		case <-ctx.Done():
			return nil
		}
	}
}

// sync brings app in line with the main, whose document has etag upEtag:
//
//   - the same etag on both sides: that is the agreed base;
//   - only the main moved (this server's copy is the base, or it has none):
//     pull: get the main's document and write it here if this server's copy
//     is still the one read, telling this server's sessions;
//   - otherwise this server's copy moved since the two last agreed. Pushing
//     it, and settling a divergence (the main's copy wins here, this one
//     kept in history), come with the next version: the app waits, logged
//     once.
//
// A pull that loses the race with a local write decides again.
func (l *Link) sync(ctx context.Context, cl *wsclient.Client, app string, upEtag *string) error {
	for range pullAttempts {
		_, local, err := l.d.Store.Read(app)
		if err != nil {
			return err
		}
		base, agreed := l.base(app)
		switch {
		case same(upEtag, local):
			l.agree(app, local)
			return nil
		case upEtag != nil && (local == nil || (agreed && same(local, base))):
			etag, body, err := cl.Get(ctx)
			if err != nil {
				return refused(err)
			}
			if etag == nil || same(etag, local) {
				// The main's copy changed again meanwhile; its push follows.
				upEtag = etag
				continue
			}
			stored, err := l.d.Writer.Replace(app, body, local)
			var conflict *store.ConflictError
			if errors.As(err, &conflict) {
				upEtag = etag
				continue
			}
			if err != nil {
				return err
			}
			l.pulled(app, stored)
			return nil
		default:
			l.wait(app, local, base, agreed, upEtag)
			return nil
		}
	}
	return nil
}

func same(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

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
	delete(l.waiting, app)
	l.saveLocked()
}

// pulled records a pull of the document with etag.
func (l *Link) pulled(app, etag string) {
	now := l.d.Now().UTC()
	l.agree(app, &etag)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSync = &now
	l.d.Logf("app %q: pulled the main's document %s", app, etag[:8])
}

// wait logs, once per situation, that app's local copy moved and waits.
func (l *Link) wait(app string, local, base *string, agreed bool, up *string) {
	short := func(e *string) string {
		if e == nil {
			return "none"
		}
		return (*e)[:min(8, len(*e))]
	}
	b := "never agreed"
	if agreed {
		b = short(base)
	}
	key := short(local) + "/" + b + "/" + short(up)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiting[app] == key {
		return
	}
	l.waiting[app] = key
	l.d.Logf("app %q: this server's document (%s) moved since the main and this server last agreed (%s); the main has %s. Sending it to the main is not built yet, so it waits", app, short(local), b, short(up))
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
