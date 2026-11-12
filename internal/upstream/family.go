package upstream

// Switching the main (docs/architecture.md, "Switching the main"). A family
// is a main and the subordinates linked to it. Its term goes up by one every
// time a subordinate becomes the main, and the higher (term, server id)
// wins: a server that meets a main higher than its own follows it, and a
// main that meets a higher one steps down and links to it.
//
// Every server of a family can reach the others: a subordinate with its own
// server-kind device on the main, which the main's registry (handed to
// every subordinate, server-kind devices included) lets the others admit to
// a link session; a subordinate that becomes the main keeps that credential
// (family.json), so that it can tell the others it is the main now; and it
// mints one for its former main, which never had one, to hand over when it
// reaches it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/fsutil"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// ReasonMainSteppedDown: the main this server is linked to answers as a
// subordinate now (it stepped down to another main), and this server looks
// for the new main among the servers of the family.
const ReasonMainSteppedDown = "mainSteppedDown"

// ErrNotInStep: a planned switch found the documents not yet the same on
// this server and its main.
var ErrNotInStep = errors.New("the documents are not yet the same on this server and its main; try again in a moment")

const (
	// inStepWait bounds how long a planned switch waits for the documents
	// to be the same on both servers.
	inStepWait = 10 * time.Second
	// tellWait bounds one attempt to reach a server of the family and tell
	// it the term.
	tellWait = 15 * time.Second
	// watchMaxFactor: the family watcher waits up to this many times the
	// link's longest backoff between rounds.
	watchMaxFactor = 10
)

// family is family.json, what a main keeps about its family beyond its
// devices: the term, and, after it became the main, what it needs until
// every server of the family follows it.
type family struct {
	Term int `json:"term"`
	// Since is when this server became the main.
	Since time.Time `json:"since,omitzero"`
	// Credential is this server's own server-kind device on the main it
	// followed before, and its token: the other servers of the family admit
	// it, since they hold that main's registry. With it this server reaches
	// them to tell them it is the main now.
	Credential *credential `json:"credential,omitempty"`
	// Handout is a credential this server minted for its former main, which
	// had none here, to hand to it when it reaches it.
	Handout *handout `json:"handout,omitempty"`
	// Pending lists the servers this server waits for to follow it, by
	// server id: its former main and the other servers of the family.
	Pending []string `json:"pending,omitempty"`
}

type credential struct {
	Device string `json:"device"`
	Token  string `json:"token"`
}

type handout struct {
	ServerID string `json:"serverId"`
	Device   string `json:"device"`
	Token    string `json:"token"`
}

func (f *family) clone() *family {
	if f == nil {
		return nil
	}
	c := *f
	if f.Credential != nil {
		cr := *f.Credential
		c.Credential = &cr
	}
	if f.Handout != nil {
		h := *f.Handout
		c.Handout = &h
	}
	c.Pending = slices.Clone(f.Pending)
	return &c
}

// loadFamily reads family.json. A missing file is nil: a main of term 0
// that waits for nobody.
func loadFamily(path string) (*family, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f family
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch {
	case f.Term < 0:
		return nil, fmt.Errorf("%s: term must be 0 or more", path)
	case f.Credential != nil && (f.Credential.Device == "" || f.Credential.Token == ""):
		return nil, fmt.Errorf("%s: credential needs a device and a token", path)
	case f.Handout != nil && (!serverIDPattern.MatchString(f.Handout.ServerID) || f.Handout.Device == "" || f.Handout.Token == ""):
		return nil, fmt.Errorf("%s: handout needs a server id, a device and a token", path)
	}
	for _, id := range f.Pending {
		if !serverIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%s: pending: %q is not a server id", path, id)
		}
	}
	f.Since = f.Since.UTC()
	return &f, nil
}

// save writes f to path atomically, readable by its owner only: it holds
// tokens.
func (f *family) save(path string) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// higher reports whether the main (term, id) wins over (myTerm, myID).
func higher(term int, id string, myTerm int, myID string) bool {
	return term > myTerm || (term == myTerm && id > myID)
}

// Term is the family's term as this server knows it: its main's on a
// subordinate, its own on a main.
func (l *Link) Term() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, _ := l.keyLocked()
	return t
}

// keyLocked is the main this server follows: its term and server id (this
// server's own on a main). The caller holds mu.
func (l *Link) keyLocked() (int, string) {
	if l.st != nil {
		return l.st.Term, l.st.ServerID
	}
	if l.fam != nil {
		return l.fam.Term, l.d.ServerID
	}
	return 0, l.d.ServerID
}

// keepTerm records a term the main named in a welcome, when it is higher
// than the one this server knew: the main took another family in.
func (l *Link) keepTerm(term int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st == nil || term <= l.st.Term {
		return
	}
	l.st.Term = term
	l.saveLocked()
}

// setUp records the etag the main's document of app has, as this server
// last heard it.
func (l *Link) setUp(app string, etag *string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if etag == nil {
		l.up[app] = nil
		return
	}
	e := *etag
	l.up[app] = &e
}

// inStep reports whether every app this server syncs holds the same
// document here and on the main, as both last agreed. Apps the write guard
// stopped are left out: they are settled by the sync rule at the next link.
func (l *Link) inStep() bool {
	l.mu.Lock()
	apps := slices.Clone(l.syncing)
	l.mu.Unlock()
	for _, app := range apps {
		if l.isStopped(app) {
			continue
		}
		_, local, err := l.d.Store.Read(app)
		if err != nil {
			return false
		}
		base, agreed := l.base(app)
		l.mu.Lock()
		up, heard := l.up[app]
		l.mu.Unlock()
		if !agreed || !heard || !same(local, base) || !same(up, base) {
			return false
		}
	}
	return true
}

// waitInStep waits until the documents are the same on both servers, at
// most inStepWait.
func (l *Link) waitInStep(ctx context.Context) error {
	deadline := time.Now().Add(inStepWait)
	for !l.inStep() {
		if time.Now().After(deadline) {
			return ErrNotInStep
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ErrNotInStep
		}
	}
	return nil
}

// Promote makes this server, a subordinate, the main of its family: the
// term goes up by one, the main's registry becomes this server's own (so
// every device the main admitted is admitted here, and no device pairs
// again), the former main gets a server-kind device here with a credential
// to hand to it, and the link ends. Then this server tells the former main
// and the other servers of the family that it is the main now, as often as
// it takes until each follows (see watch).
//
// planned (force false) needs the main to be reachable now (otherwise a
// *RefusedError with ReasonUnreachable) and the documents the same on both
// servers first (ErrNotInStep); it then tells the former main at once and
// reports whether it followed. A forced switch changes this server alone
// and returns at once: if the former main still runs, both act as main
// until they meet, and then the former, lower one steps down by itself.
// A server that is not linked is ErrNotLinked; one busy linking, unlinking
// or switching, ErrBusy.
func (l *Link) Promote(ctx context.Context, force bool) (followed bool, err error) {
	l.mu.Lock()
	switch {
	case l.st == nil:
		l.mu.Unlock()
		return false, ErrNotLinked
	case l.pairing || l.unlinking || l.promoting || l.closed:
		l.mu.Unlock()
		return false, ErrBusy
	case !force && !l.connected:
		l.mu.Unlock()
		return false, &RefusedError{Reason: ReasonUnreachable, Err: errors.New("the main is out of reach now; a forced switch does not need it")}
	}
	l.promoting = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.promoting = false
		l.mu.Unlock()
	}()
	if l.d.Self == nil || l.d.Devices == nil {
		return false, errors.New("this server cannot become the main: it does not know how devices reach it")
	}
	if !force {
		if err := l.waitInStep(ctx); err != nil {
			return false, err
		}
	}

	// The link stops first: nothing syncs or saves upstream.json any more.
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.wg.Wait()
	st := l.snapshot()
	restart := func() {
		l.mu.Lock()
		l.cancel = nil
		l.startLocked()
		l.mu.Unlock()
	}

	// The main's registry becomes this server's own, less this server.
	if _, err := l.d.Writer.Adopt(st.Devices, st.Removed, devices.Self{ServerID: l.d.ServerID, Device: st.Device}); err != nil {
		restart()
		return false, fmt.Errorf("adopt the main's devices: %w", err)
	}
	former := devices.Server{ServerID: st.ServerID, Name: st.Name, Password: st.Password, Fingerprint: st.Fingerprint, Port: st.Port, Hosts: append([]string(nil), st.Hosts...)}
	device, token, err := l.d.Writer.MintServer(former)
	if err != nil {
		restart()
		return false, fmt.Errorf("a credential for the former main: %w", err)
	}
	pending := []string{st.ServerID}
	for _, d := range l.d.Devices.List() {
		if d.IsServer() && d.Server != nil && d.Server.ServerID != l.d.ServerID && !slices.Contains(pending, d.Server.ServerID) {
			pending = append(pending, d.Server.ServerID)
		}
	}
	fam := &family{
		Term: st.Term + 1, Since: l.d.Now().UTC(),
		Credential: &credential{Device: st.Device, Token: st.Token},
		Handout:    &handout{ServerID: st.ServerID, Device: device, Token: token},
		Pending:    pending,
	}
	if err := fam.save(l.d.FamilyPath); err != nil {
		restart()
		return false, fmt.Errorf("save %s: %w", l.d.FamilyPath, err)
	}
	if err := removeFile(l.d.Path); err != nil {
		restart()
		return false, fmt.Errorf("delete %s: %w", l.d.Path, err)
	}
	l.mu.Lock()
	l.st, l.fam, l.cancel = nil, fam, nil
	l.connected, l.halted, l.reason, l.lastErr, l.lastSync = false, false, "", "", nil
	l.syncing = nil
	clear(l.stopped)
	clear(l.pushes)
	clear(l.up)
	l.mu.Unlock()
	if l.d.Admit != nil {
		l.d.Admit(nil) // the main's devices are this server's own now
	}
	how := "a planned switch"
	if force {
		how = "a forced switch: the former main, if it still runs, steps down when the two meet"
	}
	l.d.Logf("this server is the main now, at term %d (%s); %q (%s) and every other server of the family are told when they are reached", fam.Term, how, st.Name, st.ServerID)

	if !force {
		tctx, cancel := context.WithTimeout(ctx, tellWait)
		followed = l.tell(tctx, st.ServerID)
		cancel()
	}
	l.mu.Lock()
	l.startWatchLocked()
	l.mu.Unlock()
	return followed, nil
}

// Follow handles a term frame: the server from says it is the main of the
// family at term; device and token, when given, are a credential for this
// server there. When (term, from's id) is higher than the main this server
// follows, it follows from: a subordinate relinks there with its own
// credential, which from admits since it holds the family's registry; a
// main steps down and links there, with the credential given or the one it
// kept from its own time as a subordinate, and hands its own registry over
// (see Announce). It returns the term and the main this server follows
// then. sender is the device whose link session told it, which stepping
// down leaves open for the answer.
func (l *Link) Follow(sender string, term int, from wsclient.Server, device, token string) (int, string, error) {
	l.mu.Lock()
	myTerm, myMain := l.keyLocked()
	if from.ServerID == l.d.ServerID || !higher(term, from.ServerID, myTerm, myMain) || l.pairing || l.unlinking || l.promoting || l.closed {
		l.mu.Unlock()
		return myTerm, myMain, nil
	}
	if l.st != nil {
		l.relinkLocked(from, term)
		l.mu.Unlock()
		l.d.Logf("the server %q (%s) is the main now, at term %d: this server follows it", from.Name, from.ServerID, term)
		return term, from.ServerID, nil
	}
	if device == "" && l.fam != nil && l.fam.Credential != nil {
		device, token = l.fam.Credential.Device, l.fam.Credential.Token
	}
	l.mu.Unlock()
	if device == "" || token == "" {
		return 0, "", errors.New("this main has no credential for the new main, and none was given")
	}
	if err := l.stepDown(from, device, token, term, sender); err != nil {
		l.d.Logf("the server %q (%s) is the main at term %d, but this server cannot step down: %v", from.Name, from.ServerID, term, err)
		l.mu.Lock()
		defer l.mu.Unlock()
		t, m := l.keyLocked()
		return t, m, nil
	}
	return term, from.ServerID, nil
}

// relinkLocked points this subordinate's link at the main m of term, with
// its own credential, and makes the link reconnect. The agreed etags stay:
// an etag is the document's hash, the same on every server holding it. The
// caller holds mu.
func (l *Link) relinkLocked(m wsclient.Server, term int) {
	l.st.ServerID, l.st.Name, l.st.Password, l.st.Fingerprint, l.st.Port = m.ServerID, m.Name, m.Password, m.Fingerprint, m.Port
	l.st.Hosts = append([]string(nil), m.Hosts...)
	l.st.Term = term
	l.saveLocked()
	clear(l.up)
	if l.halted {
		l.cancel = nil
	}
	if l.cancel == nil {
		l.startLocked()
	} else {
		poke(l.kick)
	}
}

// stepDown makes this server, a main, a subordinate of the main m of term,
// with the credential device and token there: it saves upstream.json (with
// its own registry to hand over), forgets family.json, closes the sessions
// of its own subordinates but sender's (they reconnect, find it a
// subordinate, and look for the main), and starts the link.
func (l *Link) stepDown(m wsclient.Server, device, token string, term int, sender string) error {
	l.mu.Lock()
	if l.st != nil || l.closed || l.pairing || l.unlinking || l.promoting {
		l.mu.Unlock()
		return ErrBusy
	}
	st := &state{
		ServerID: m.ServerID, Name: m.Name, Password: m.Password, Fingerprint: m.Fingerprint, Port: m.Port,
		Hosts:  append([]string(nil), m.Hosts...),
		Device: device, Token: token, LinkedAt: l.d.Now().UTC(),
		Apps: map[string]appState{}, Term: term, Handover: true,
	}
	if err := st.validate(); err != nil {
		l.mu.Unlock()
		return err
	}
	if err := st.save(l.d.Path); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("save %s: %w", l.d.Path, err)
	}
	if l.watchCancel != nil {
		l.watchCancel()
		l.watchCancel = nil
	}
	l.st, l.fam = st, nil
	if l.d.FamilyPath != "" {
		if err := removeFile(l.d.FamilyPath); err != nil {
			l.d.Logf("delete %s: %v", l.d.FamilyPath, err)
		}
	}
	l.cancel = nil
	l.startLocked()
	l.mu.Unlock()
	l.d.Writer.CloseServers(sender)
	l.d.Logf("stepped down: %q (%s) is the main now, at term %d; this server links to it and hands its devices over", m.Name, m.ServerID, term)
	return nil
}

// startWatchLocked starts the family watcher when this server is a main
// that waits for servers to follow it. The caller holds mu.
func (l *Link) startWatchLocked() {
	if l.st != nil || l.fam == nil || l.fam.Credential == nil || l.watchCancel != nil || l.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.watchCancel = cancel
	l.wg.Go(func() { l.watch(ctx) })
}

// watch tells the servers this main waits for that it is the main, round
// after round, waiting longer after each round (and not at all after the
// network changed), until each follows or this server steps down.
func (l *Link) watch(ctx context.Context) {
	delay := l.d.MinBackoff
	for {
		if l.watchRound(ctx) || ctx.Err() != nil {
			return
		}
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-l.watchKick:
			delay = l.d.MinBackoff
		case <-ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
		delay = min(2*delay, watchMaxFactor*l.d.MaxBackoff)
	}
}

// watchRound tries every server this main waits for once, at the same
// time, and reports whether there is nothing left to wait for.
func (l *Link) watchRound(ctx context.Context) bool {
	l.mu.Lock()
	fam := l.fam.clone()
	l.mu.Unlock()
	if fam == nil || fam.Credential == nil {
		return true
	}
	var wg sync.WaitGroup
	for _, id := range fam.Pending {
		if l.serverOf(id) == nil || l.d.Writer.Linked(id) {
			l.followed(id) // removed here, or linked: it follows
			continue
		}
		wg.Go(func() {
			tctx, cancel := context.WithTimeout(ctx, tellWait)
			defer cancel()
			l.tell(tctx, id)
		})
	}
	wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st != nil || l.fam == nil {
		return true // stepped down meanwhile
	}
	if len(l.fam.Pending) > 0 {
		return false
	}
	l.fam.Credential, l.fam.Handout = nil, nil
	if err := l.fam.save(l.d.FamilyPath); err != nil {
		l.d.Logf("save %s: %v", l.d.FamilyPath, err)
	}
	l.d.Logf("every server of the family follows this one now")
	return true
}

// serverOf is this server's own server-kind device for the server id, nil
// when there is none.
func (l *Link) serverOf(id string) *devices.Server {
	for _, d := range l.d.Devices.List() {
		if d.IsServer() && d.Server != nil && d.Server.ServerID == id {
			s := *d.Server
			return &s
		}
	}
	return nil
}

// followed records that the server id follows this main, or was removed
// here: it is no longer waited for, and the credential minted for it, if it
// was the former main, is no longer kept.
func (l *Link) followed(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fam == nil || !slices.Contains(l.fam.Pending, id) {
		return
	}
	l.fam.Pending = slices.DeleteFunc(l.fam.Pending, func(p string) bool { return p == id })
	if l.fam.Handout != nil && l.fam.Handout.ServerID == id {
		l.fam.Handout = nil
	}
	if err := l.fam.save(l.d.FamilyPath); err != nil {
		l.d.Logf("save %s: %v", l.d.FamilyPath, err)
	}
}

// tell reaches the server id of the family with this main's credential, and
// reports whether it follows this server now. A main that answers higher
// than this one makes this server step down and link to it, with the
// credential it just accepted; any other server is told the term (with the
// credential minted for it, when it is the former main), and follows when
// this server is higher than its main.
func (l *Link) tell(ctx context.Context, id string) bool {
	si := l.serverOf(id)
	l.mu.Lock()
	fam := l.fam.clone()
	l.mu.Unlock()
	if si == nil || fam == nil || fam.Credential == nil {
		return false
	}
	t := wsclient.Target{Password: si.Password, Fingerprint: si.Fingerprint, Port: si.Port, Hosts: append([]string(nil), si.Hosts...)}
	cl, err := l.d.Dial(ctx, t, "")
	if err != nil {
		return false
	}
	defer cl.Close()
	w, err := cl.Hello(ctx, fam.Credential.Device, fam.Credential.Token)
	if err != nil {
		if wsclient.HasCode(err, "badToken") {
			l.d.Logf("the server %q (%s) does not know this server's credential; it follows only when it reaches this one, or is linked again", si.Name, si.ServerID)
		}
		return false
	}
	if w.ServerID != id {
		return false
	}
	if w.Role == RoleMain && higher(w.Term, w.ServerID, fam.Term, l.d.ServerID) {
		m := wsclient.Server(*si)
		m.Name = w.Name
		if len(w.Hosts) > 0 {
			m.Hosts = w.Hosts
		}
		if err := l.stepDown(m, fam.Credential.Device, fam.Credential.Token, w.Term, ""); err != nil {
			l.d.Logf("the server %q (%s) is the main at term %d, but this server cannot step down: %v", w.Name, w.ServerID, w.Term, err)
		}
		return false
	}
	var device, token string
	if fam.Handout != nil && fam.Handout.ServerID == id {
		device, token = fam.Handout.Device, fam.Handout.Token
	}
	a, err := cl.Term(ctx, fam.Term, l.d.Self(), device, token)
	if err != nil {
		l.d.Logf("the server %q (%s) did not take the term: %v", si.Name, si.ServerID, err)
		return false
	}
	if a.Main != l.d.ServerID {
		l.d.Logf("the server %q (%s) follows %s at term %d, which wins over this server", si.Name, si.ServerID, a.Main, a.Term)
		return false
	}
	l.d.Logf("the server %q (%s) follows this one now, at term %d", si.Name, si.ServerID, a.Term)
	l.followed(id)
	return true
}

// findMain looks for the main among the servers of the family this
// subordinate knows from its main's registry, when its main stepped down:
// it says hello to each with its own credential, and follows the highest
// that answers as a main higher than the one it followed. It reports
// whether it found one.
func (l *Link) findMain(ctx context.Context) bool {
	st := l.snapshot()
	type found struct {
		s wsclient.Server
		w wsclient.Welcome
	}
	var (
		mu   sync.Mutex
		best *found
		wg   sync.WaitGroup
	)
	for _, e := range st.Devices {
		if !e.IsServer() || e.Server == nil || e.Server.ServerID == l.d.ServerID || e.Server.ServerID == st.ServerID {
			continue
		}
		s := wsclient.Server(*e.Server)
		wg.Go(func() {
			tctx, cancel := context.WithTimeout(ctx, tellWait)
			defer cancel()
			cl, err := l.d.Dial(tctx, wsclient.Target{Password: s.Password, Fingerprint: s.Fingerprint, Port: s.Port, Hosts: s.Hosts}, "")
			if err != nil {
				return
			}
			defer cl.Close()
			w, err := cl.Hello(tctx, st.Device, st.Token)
			if err != nil || w.ServerID != s.ServerID || w.Role != RoleMain || !higher(w.Term, w.ServerID, st.Term, st.ServerID) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if best == nil || higher(w.Term, w.ServerID, best.w.Term, best.w.ServerID) {
				best = &found{s, w}
			}
		})
	}
	wg.Wait()
	if best == nil {
		return false
	}
	m := best.s
	m.Name = best.w.Name
	if len(best.w.Hosts) > 0 {
		m.Hosts = best.w.Hosts
	}
	l.mu.Lock()
	if l.st == nil || l.st.ServerID != st.ServerID {
		l.mu.Unlock()
		return true // something else moved the link meanwhile
	}
	l.st.ServerID, l.st.Name, l.st.Password, l.st.Fingerprint, l.st.Port = m.ServerID, m.Name, m.Password, m.Fingerprint, m.Port
	l.st.Hosts = append([]string(nil), m.Hosts...)
	l.st.Term = best.w.Term
	l.saveLocked()
	clear(l.up)
	l.mu.Unlock()
	l.d.Logf("the main stepped down; %q (%s) is the main now, at term %d: this server follows it", m.Name, m.ServerID, best.w.Term)
	return true
}

// Waiting is a server a main waits for to follow it.
type Waiting struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
}

// waitingLocked lists the servers this main waits for, by name. The caller
// holds mu.
func (l *Link) waitingLocked() []Waiting {
	if l.fam == nil || len(l.fam.Pending) == 0 || l.d.Devices == nil {
		return nil
	}
	var out []Waiting
	for _, id := range l.fam.Pending {
		w := Waiting{ServerID: id, Name: id}
		if s := l.serverOf(id); s != nil {
			w.Name = s.Name
		}
		out = append(out, w)
	}
	return out
}

// toEntries reads a registry as the main sent it; an entry that does not
// read is left out, and logged.
func (l *Link) toEntries(reg wsclient.Registry) ([]devices.Entry, []devices.Tombstone) {
	entries := make([]devices.Entry, 0, len(reg.Devices))
	for _, d := range reg.Devices {
		e := devices.Entry{ID: d.ID, App: d.App, Kind: d.Kind, Name: d.Name, TokenHash: d.TokenHash}
		if d.PairedAt != "" {
			if at, err := time.Parse(time.RFC3339, d.PairedAt); err == nil {
				e.PairedAt = at.UTC()
			}
		}
		if d.Server != nil {
			s := devices.Server(*d.Server)
			e.Server = &s
		}
		if err := e.Validate(); err != nil {
			l.d.Logf("the main's device %q is left out: %v", d.ID, err)
			continue
		}
		entries = append(entries, e)
	}
	removed := make([]devices.Tombstone, 0, len(reg.Removed))
	for _, t := range reg.Removed {
		at, err := time.Parse(time.RFC3339, t.At)
		tomb := devices.Tombstone{ID: t.ID, At: at.UTC()}
		if err != nil || tomb.Validate() != nil {
			continue
		}
		removed = append(removed, tomb)
	}
	return entries, removed
}

// ownRegistry is this server's own registry as it travels, for the
// handover after it stepped down.
func (l *Link) ownRegistry() *wsclient.Registry {
	reg := &wsclient.Registry{Devices: []wsclient.Device{}}
	for _, e := range l.d.Devices.Entries() {
		d := wsclient.Device{ID: e.ID, App: e.App, Kind: e.Kind, Name: e.Name, TokenHash: e.TokenHash}
		if !e.PairedAt.IsZero() {
			d.PairedAt = e.PairedAt.UTC().Format(time.RFC3339)
		}
		if e.Server != nil {
			s := wsclient.Server(*e.Server)
			d.Server = &s
		}
		reg.Devices = append(reg.Devices, d)
	}
	for _, t := range l.d.Devices.Removed() {
		reg.Removed = append(reg.Removed, wsclient.Tombstone{ID: t.ID, At: t.At.UTC().Format(time.RFC3339)})
	}
	return reg
}
