package upstream

// A divergence (docs/architecture.md, "The sync rule"): both servers changed
// the same documents of an app differently since they last agreed, or two
// different non-empty sets meet at a first link. This server, the
// subordinate, then keeps its own set, stops syncing the app with the main
// (no pulls, no pushes; other apps go on), and waits for a device's choice,
// which any device of the app makes, on either server: keep the main's set
// or this server's, the whole of it (the files plan's decision 5). Nothing
// is merged.
//
// The divergence is kept in the app's .wss/diverged.json, which survives a
// restart. The link tells its main over the link session (a diverged frame
// at every link session and at every change), and the hub lists it to this
// server's sessions of the app. A choice comes from a device here
// (ChooseOwn, through the hub's resolve) or from the main (a resolve push),
// and this server decides which is first: one that comes after it is
// ignored. The choice is then carried out as one ordinary transaction in the
// right direction (the main's whole set pulled here, or this server's pushed
// there), resumable as any pull or push; once it commits, the set it wrote
// is the base, diverged.json goes, and the app syncs by the table again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// divergedFile is the side file in an app's .wss folder that holds the
// app's divergence from the main, while there is one.
const divergedFile = "diverged.json"

// The kinds of divergence diverged.json names.
const (
	// reasonChanged: both servers changed the same documents differently
	// since they last agreed.
	reasonChanged = "changed"
	// reasonFirstLink: the two were never agreed on (a first link, or a base
	// agreed with another main), and both hold documents that differ.
	reasonFirstLink = "firstLink"
)

// maxDivergedKeys bounds the keys diverged.json names, for the log and the
// page.
const maxDivergedKeys = 10

// divergence is diverged.json: when the app diverged from the main named,
// why, a few of the documents that differ, and, once a device chose, which
// set it keeps (protocol.KeepMain or KeepSubordinate) and when.
type divergence struct {
	Since  time.Time  `json:"since"`
	Reason string     `json:"reason"`
	Keys   []string   `json:"keys,omitempty"`
	Main   mainRef    `json:"main"`
	Keep   string     `json:"keep,omitempty"`
	Chosen *time.Time `json:"chosen,omitempty"`
}

// mainRef names the main a divergence is from.
type mainRef struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
}

func (d *divergence) clone() *divergence {
	if d == nil {
		return nil
	}
	c := *d
	c.Keys = append([]string(nil), d.Keys...)
	if d.Chosen != nil {
		t := *d.Chosen
		c.Chosen = &t
	}
	return &c
}

// sinceText is a divergence's since as the frames carry it.
func sinceText(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// valid reports whether a diverged.json read from disk makes sense.
func (d *divergence) valid() bool {
	return !d.Since.IsZero() && serverIDPattern.MatchString(d.Main.ServerID) &&
		(d.Reason == reasonChanged || d.Reason == reasonFirstLink) &&
		(d.Keep == "" || d.Keep == protocol.KeepMain || d.Keep == protocol.KeepSubordinate)
}

// divergenceOf returns a copy of app's divergence from the main mainID, nil
// when there is none. A diverged.json from another main is no divergence
// from this one: the next sync with this main is a first link, which finds
// whatever differs anew. It is removed. The caller holds divMu.
func (l *Link) divergenceOfLocked(app, mainID string) *divergence {
	d, known := l.divs[app]
	if !known {
		d = l.readDivergence(app)
		l.divs[app] = d
	}
	if d != nil && d.Main.ServerID != mainID {
		l.d.Logf("app %q: its divergence from %q (%s) is moot: this server follows another main now", app, d.Main.Name, d.Main.ServerID)
		l.removeDivergenceLocked(app)
		return nil
	}
	return d.clone()
}

// readDivergence reads app's diverged.json; nil when there is none, or one
// that does not read, which is logged and treated as none.
func (l *Link) readDivergence(app string) *divergence {
	raw, err := l.d.Store.ReadSide(app, divergedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		l.d.Logf("app %q: read %s: %v; taken as no divergence", app, divergedFile, err)
		return nil
	}
	var d divergence
	if json.Unmarshal(raw, &d) != nil || !d.valid() {
		l.d.Logf("app %q: %s does not read; taken as no divergence", app, divergedFile)
		return nil
	}
	d.Since = d.Since.UTC()
	return &d
}

// saveDivergenceLocked writes app's diverged.json and keeps it. The caller
// holds divMu.
func (l *Link) saveDivergenceLocked(app string, d *divergence) error {
	data, err := json.MarshalIndent(d, "", "\t")
	if err != nil {
		return err
	}
	if err := l.d.Store.WriteSide(app, divergedFile, append(data, '\n')); err != nil {
		return fmt.Errorf("save %s of %q: %w", divergedFile, app, err)
	}
	l.divs[app] = d.clone()
	return nil
}

// removeDivergenceLocked removes app's diverged.json. The caller holds
// divMu.
func (l *Link) removeDivergenceLocked(app string) {
	if err := l.d.Store.RemoveSide(app, divergedFile); err != nil {
		l.d.Logf("app %q: delete %s: %v", app, divergedFile, err)
		delete(l.divs, app) // read again next time
		return
	}
	l.divs[app] = nil
}

// divergenceOf returns a copy of app's divergence from the main mainID, nil
// when there is none.
func (l *Link) divergenceOf(app, mainID string) *divergence {
	l.divMu.Lock()
	defer l.divMu.Unlock()
	return l.divergenceOfLocked(app, mainID)
}

// mainID is the server id of the main this server follows, "" on a main.
func (l *Link) mainID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st == nil {
		return ""
	}
	return l.st.ServerID
}

// isPaused reports whether app waits for a device's choice: it diverged
// from the main, and no device chose yet. Its worker opens no session to
// the main meanwhile.
func (l *Link) isPaused(app string) bool {
	d := l.divergenceOf(app, l.mainID())
	return d != nil && d.Keep == ""
}

// diverged records that app diverged from the main: diverged.json, so that
// it survives a restart. The app then waits for a device's choice: the hub
// lists it to this server's sessions of the app, and the link session tells
// the main.
func (l *Link) diverged(app string, main mainRef, lk *appLink, p plan, here, upSet map[string]string) error {
	d := &divergence{Since: l.d.Now().UTC().Truncate(time.Second), Reason: reasonChanged, Main: main}
	if p.firstLink {
		d.Reason = reasonFirstLink
	}
	d.Keys = append([]string(nil), p.diverged[:min(len(p.diverged), maxDivergedKeys)]...)
	l.divMu.Lock()
	err := l.saveDivergenceLocked(app, d)
	l.divMu.Unlock()
	if err != nil {
		return err
	}
	l.logDivergence(app, lk, p, here, upSet)
	l.divergenceMoved(app)
	return nil
}

// errPaused: app diverged from the main and waits for a device's choice; its
// worker opens no session to the main until one comes.
var errPaused = errors.New("the documents diverged from the main's, and wait for a device's choice")

// OwnDiverged is this server's divergence on app from its main while it
// waits for a device's choice, as the hub lists it: this server, and since
// when. It is nil on a main, and once a device chose.
func (l *Link) OwnDiverged(app string) *protocol.DivergedServer {
	mainID := l.mainID()
	if mainID == "" {
		return nil
	}
	d := l.divergenceOf(app, mainID)
	if d == nil || d.Keep != "" {
		return nil
	}
	return &protocol.DivergedServer{ServerID: l.d.ServerID, Name: l.d.Name(), Since: sinceText(d.Since)}
}

// ChooseOwn takes a device's choice keep for this server's divergence on
// app, and reports whether it was the first: false when no divergence of
// the app waits for one (another device chose first, or there is none).
// The choice is kept in diverged.json, and the app's worker carries it out.
// It does not call the hub: the hub, which calls it, tells the sessions.
func (l *Link) ChooseOwn(app, keep string) bool {
	return l.choose(app, keep, "", "a device of this server")
}

// choose records keep as the choice for app's divergence from the main,
// when it is the first, and, when since is not empty, only for the
// divergence that began then. who names the chooser for the log.
func (l *Link) choose(app, keep, since, who string) bool {
	if keep != protocol.KeepMain && keep != protocol.KeepSubordinate {
		return false
	}
	mainID := l.mainID()
	if mainID == "" {
		return false
	}
	l.divMu.Lock()
	d := l.divergenceOfLocked(app, mainID)
	if d == nil || d.Keep != "" || (since != "" && sinceText(d.Since) != since) {
		l.divMu.Unlock()
		return false
	}
	now := l.d.Now().UTC()
	d.Keep, d.Chosen = keep, &now
	err := l.saveDivergenceLocked(app, d)
	l.divMu.Unlock()
	if err != nil {
		l.d.Logf("app %q: %v; the choice is not kept", app, err)
		return false
	}
	which := fmt.Sprintf("the main's (%s)", d.Main.Name)
	if keep == protocol.KeepSubordinate {
		which = "this server's"
	}
	l.d.Logf("app %q: %s chose to keep %s set where the two diverged; it replaces the other's now", app, who, which)
	poke(l.resumeSignal(app))
	poke(l.divTell)
	return true
}

// takeChoice takes a choice the main pushed for app's divergence that
// began at since, and tells this server's sessions when it was the first.
// One that comes after another choice, or for another divergence, is
// ignored: the link session tells the main the state again.
func (l *Link) takeChoice(c wsclient.Choice) {
	if l.choose(c.App, c.Keep, c.Since, "a device of the main") {
		l.d.Writer.OwnResolved(c.App)
		return
	}
	l.d.Logf("app %q: a choice from the main for its divergence came after another, or for one that is over; ignored", c.App)
	poke(l.divTell)
}

// settledDivergence ends app's divergence once the choice is carried out:
// diverged.json goes, and the app syncs by the table again.
func (l *Link) settledDivergence(app string, d *divergence) {
	l.divMu.Lock()
	l.removeDivergenceLocked(app)
	l.divMu.Unlock()
	which := "the main's set"
	if d.Keep == protocol.KeepSubordinate {
		which = "this server's set"
	}
	l.d.Logf("app %q: the divergence is settled, %s kept on both servers; the app syncs with the main again", app, which)
	l.divergenceMoved(app)
}

// clearDivergences forgets every app's divergence: this server is no longer
// a subordinate of the main they were from.
func (l *Link) clearDivergences() {
	if l.d.Apps == nil {
		return
	}
	l.divMu.Lock()
	for _, a := range l.d.Apps.List() {
		if d, known := l.divs[a.ID]; known && d == nil {
			continue
		}
		l.removeDivergenceLocked(a.ID)
	}
	l.divMu.Unlock()
}

// divergenceMoved tells whoever lists app's divergence that it changed: the
// hub, which pushes the list to this server's sessions of the app, and the
// link session, which tells the main.
func (l *Link) divergenceMoved(app string) {
	if l.d.Writer != nil {
		l.d.Writer.DivergedChanged(app)
	}
	poke(l.divTell)
}

// divergedApps lists, sorted, the apps whose divergence from the main waits
// for a device's choice.
func (l *Link) divergedApps() []string {
	mainID := l.mainID()
	if mainID == "" || l.d.Apps == nil {
		return nil
	}
	var out []string
	for _, a := range l.d.Apps.List() {
		if d := l.divergenceOf(a.ID, mainID); d != nil && d.Keep == "" {
			out = append(out, a.ID)
		}
	}
	return out
}

// anyDivergence reports whether any app listed here diverged from the main,
// a choice waiting or being carried out.
func (l *Link) anyDivergence() bool {
	mainID := l.mainID()
	if mainID == "" || l.d.Apps == nil {
		return false
	}
	for _, a := range l.d.Apps.List() {
		if l.divergenceOf(a.ID, mainID) != nil {
			return true
		}
	}
	return false
}

// tellDivergences tells the main, over the link session cl, the state of
// each app it lists (want) that the session has not told yet, or that
// changed since: since when the app waits for a choice, or none. told is the
// session's record of what it told, by app.
func (l *Link) tellDivergences(ctx context.Context, cl *wsclient.Client, mainID string, want []string, told map[string]string) error {
	for _, app := range want {
		state := ""
		var since *time.Time
		if d := l.divergenceOf(app, mainID); d != nil && d.Keep == "" {
			t := d.Since
			since, state = &t, sinceText(t)
		}
		if was, ok := told[app]; ok && was == state {
			continue
		}
		if err := cl.Diverged(ctx, app, since); err != nil {
			return refused(err)
		}
		told[app] = state
	}
	return nil
}
