package hub

import (
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"time"

	"github.com/FLighter7/workspace-server/internal/protocol"
)

// Divergences (docs/architecture.md, "The sync rule"). A subordinate whose
// copy of an app diverged from its main's keeps its own, stops syncing the
// app with the main, and waits for a device's choice: keep the main's copy,
// or its own, for the app's whole set. Every device of the app, on either
// server, is asked; the first choice wins.
//
// The hub lists the divergences that wait for a choice in every welcome and
// pushes the list (diverged) whenever it changes, and takes the devices'
// resolve:
//
//   - on a subordinate, its own divergence, which its link keeps
//     (Deps.OwnDiverged) and acts on (Deps.ChooseOwn);
//   - on a main, those its subordinates tell it over their link sessions
//     (diverged frames), kept in each app's .wss/divergences.json with the
//     choice a device of the main made, until the subordinate takes it: the
//     choice goes to the subordinate's link session as a resolve push, at
//     once or when it tells its divergence again (at every link session).
//
// The subordinate decides which choice is first: a choice made on the main
// counts once it reaches the subordinate, which ignores it when one of its
// own devices chose first.

// divergencesFile is the side file in an app's .wss folder in which a main
// keeps its subordinates' divergences on the app.
const divergencesFile = "divergences.json"

// subDivergence is a subordinate's divergence on an app, as its main keeps
// it: since (RFC 3339 UTC) as the subordinate told it, and the choice a
// device of the main made (Keep, at Chosen), which waits to reach the
// subordinate.
type subDivergence struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Since    string `json:"since"`
	Keep     string `json:"keep,omitempty"`
	Chosen   string `json:"chosen,omitempty"`
}

type divergencesDoc struct {
	Servers []subDivergence `json:"servers"`
}

// subDivergences reads the divergences app's subordinates told this server,
// which a main keeps: only those of servers linked to it now (a server-kind
// device of its own that announced that server id), each named as it last
// announced itself. A subordinate keeps none. The caller holds the app's
// lock.
func (h *Hub) subDivergences(app string) []subDivergence {
	if h.role() != protocol.RoleMain {
		return nil
	}
	raw, err := h.d.Store.ReadSide(app, divergencesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var doc divergencesDoc
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		h.d.Logf("app %q: read %s: %v; no subordinate's divergence is known", app, divergencesFile, err)
		return nil
	}
	names := map[string]string{}
	for _, d := range h.d.Devices.List() {
		if d.IsServer() && d.Server != nil {
			names[d.Server.ServerID] = d.ShownName()
		}
	}
	out := doc.Servers[:0]
	for _, sd := range doc.Servers {
		name, ok := names[sd.ServerID]
		if !ok || sd.ServerID == "" {
			continue
		}
		sd.Name = name
		out = append(out, sd)
	}
	return out
}

// saveSubDivergences writes the divergences of app's subordinates, or
// removes the file when there is none. The caller holds the app's lock.
func (h *Hub) saveSubDivergences(app string, list []subDivergence) error {
	if len(list) == 0 {
		return h.d.Store.RemoveSide(app, divergencesFile)
	}
	data, err := json.MarshalIndent(divergencesDoc{Servers: list}, "", "\t")
	if err != nil {
		return err
	}
	return h.d.Store.WriteSide(app, divergencesFile, append(data, '\n'))
}

// divergedLocked lists the divergences of app that wait for a device's
// choice, as welcome and diverged carry them: this server's own (a
// subordinate's), and its subordinates' that no device of this server chose
// for yet (a main's). The caller holds the app's lock.
func (h *Hub) divergedLocked(app string) []protocol.DivergedServer {
	var out []protocol.DivergedServer
	if h.d.OwnDiverged != nil {
		if own := h.d.OwnDiverged(app); own != nil {
			out = append(out, *own)
		}
	}
	for _, sd := range h.subDivergences(app) {
		if sd.Keep == "" {
			out = append(out, protocol.DivergedServer{ServerID: sd.ServerID, Name: sd.Name, Since: sd.Since})
		}
	}
	slices.SortFunc(out, func(a, b protocol.DivergedServer) int {
		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			}
			return 1
		}
		if a.ServerID < b.ServerID {
			return -1
		}
		if a.ServerID > b.ServerID {
			return 1
		}
		return 0
	})
	return out
}

// ErrDiverged: a subordinate is not asked to take the role of main while
// one of this server's subordinates diverged on an app, waiting for a
// choice or for one made here to reach it.
var ErrDiverged = errors.New("a subordinate's documents diverged from this server's, and wait for a device's choice or for one to reach it; choose on a device first")

// anyDivergence reports whether a subordinate of this server diverged on an
// app it lists, a choice waiting or not yet taken there.
func (h *Hub) anyDivergence() bool {
	for _, a := range h.d.Apps.List() {
		_, unlock, ok := h.lockListed(a.ID)
		if !ok {
			continue
		}
		n := len(h.subDivergences(a.ID))
		unlock()
		if n > 0 {
			return true
		}
	}
	return false
}

// Divergences lists the divergences of app that wait for a device's choice,
// as a welcome names them: for the page.
func (h *Hub) Divergences(app string) []protocol.DivergedServer {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return nil
	}
	defer unlock()
	return h.divergedLocked(app)
}

// pushDivergedLocked tells every session of app the divergences that wait
// for a choice now. The caller holds the app's lock.
func (h *Hub) pushDivergedLocked(app string) {
	list := h.divergedLocked(app)
	for _, s := range h.registered(func(m member) bool { return m.app == app }) {
		s.PushDiverged(list)
	}
}

// pushResolvedLocked tells every session of app but except that the
// divergence of server was settled by a device's choice. The caller holds
// the app's lock.
func (h *Hub) pushResolvedLocked(app, server string, except *protocol.Session) {
	for _, s := range h.registered(func(m member) bool { return m.app == app }) {
		if s != except {
			s.PushResolved(server)
		}
	}
}

// DivergedChanged tells every session of app the divergences that wait for
// a choice now. The link calls it when its own divergence on app changes.
func (h *Hub) DivergedChanged(app string) {
	if _, unlock, ok := h.lockListed(app); ok {
		h.pushDivergedLocked(app)
		unlock()
	}
}

// DivergencesChanged tells every session of every app the divergences that
// wait for a choice now: this server's role changed, or the main it follows.
func (h *Hub) DivergencesChanged() {
	for _, a := range h.d.Apps.List() {
		h.DivergedChanged(a.ID)
	}
}

// OwnResolved tells every session of app that this server's own divergence
// on it was settled, by a choice that came from its main: resolved, and the
// list without it.
func (h *Hub) OwnResolved(app string) {
	if _, unlock, ok := h.lockListed(app); ok {
		h.pushResolvedLocked(app, h.d.ServerID, nil)
		h.pushDivergedLocked(app)
		unlock()
	}
}

// ForgetDivergences forgets what this server's subordinates told it of
// their divergences, on every app: it is a subordinate now, and they follow
// another main.
func (h *Hub) ForgetDivergences() {
	for _, a := range h.d.Apps.List() {
		if _, unlock, ok := h.lockListed(a.ID); ok {
			if err := h.d.Store.RemoveSide(a.ID, divergencesFile); err != nil {
				h.d.Logf("app %q: delete %s: %v", a.ID, divergencesFile, err)
			}
			unlock()
		}
	}
}

// Resolve takes a device's choice for the divergence of server on app. The
// first choice wins: every other session of the app is told resolved (the
// session answers from itself) and the list without it. A choice for a
// divergence that waits for none (another device chose first, it was
// settled, or there never was one) changes nothing; the session answers
// resolved all the same.
//
// For this server's own divergence (a subordinate's), the link acts on it
// (Deps.ChooseOwn). For a subordinate's, a main keeps the choice and pushes
// it to the subordinate's link session, now or when it tells its divergence
// again.
func (h *Hub) Resolve(from *protocol.Session, app, device, server, keep string) *protocol.Error {
	if e := h.serves(app); e != nil {
		return e
	}
	if server == h.d.ServerID {
		if h.d.ChooseOwn == nil || !h.d.ChooseOwn(app, keep) {
			return nil
		}
		if _, unlock, ok := h.lockListed(app); ok {
			h.pushResolvedLocked(app, server, from)
			h.pushDivergedLocked(app)
			unlock()
		}
		return nil
	}
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return errNoLongerServed(app)
	}
	defer unlock()
	list := h.subDivergences(app)
	i := slices.IndexFunc(list, func(sd subDivergence) bool { return sd.ServerID == server })
	if i < 0 || list[i].Keep != "" {
		return nil
	}
	list[i].Keep, list[i].Chosen = keep, h.d.Now().UTC().Format(time.RFC3339)
	if err := h.saveSubDivergences(app, list); err != nil {
		return h.errInternal(err)
	}
	h.d.Logf("app %q: a device chose to keep the %s's copy where the subordinate %q (%s) diverged; the choice goes to the subordinate", app, keep, list[i].Name, server)
	h.pushResolvedLocked(app, server, from)
	h.pushDivergedLocked(app)
	h.forwardLocked(app, list[i])
	return nil
}

// forwardLocked pushes the choice made for sd to the link session of its
// server, if it has one now. The caller holds the app's lock.
func (h *Hub) forwardLocked(app string, sd subDivergence) {
	id := h.serverDevice(sd.ServerID)
	if id == "" {
		return
	}
	for _, s := range h.registered(func(m member) bool { return m.device == id && m.server && m.app == "" && !m.family }) {
		s.PushResolve(app, sd.Keep, sd.Since)
	}
}

// Diverged takes what a subordinate tells, over its link session s, of its
// copy of app: since, when it diverged and began to wait for a choice, or nil
// when none waits for one. A main keeps it (and lists it to the app's
// sessions until a device chooses), pushes back a choice it holds for that
// divergence, and forgets one that no longer waits. Only the link session of
// a server-kind device of this server's own may tell it; any other session
// gets a protocol error, which closes it. A server that never announced
// itself, an app not listed here, and a server that is not a main ignore
// it.
func (h *Hub) Diverged(s *protocol.Session, app string, since *string) *protocol.Error {
	h.mu.Lock()
	m, ok := h.sessions[s]
	h.mu.Unlock()
	if !ok || !m.server || m.app != "" || m.family {
		return &protocol.Error{Code: protocol.CodeProtocol, Message: "only the link session of a server linked to this one may tell a divergence"}
	}
	d, ok := h.d.Devices.Get(m.device)
	if !ok || d.Server == nil || h.role() != protocol.RoleMain {
		return nil
	}
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return nil
	}
	defer unlock()
	list := h.subDivergences(app)
	i := slices.IndexFunc(list, func(sd subDivergence) bool { return sd.ServerID == d.Server.ServerID })
	switch {
	case since == nil && i < 0:
		return nil
	case since == nil:
		gone := list[i]
		list = slices.Delete(list, i, i+1)
		if err := h.saveSubDivergences(app, list); err != nil {
			h.d.Logf("app %q: save %s: %v", app, divergencesFile, err)
		}
		if gone.Keep == "" {
			// A device of the subordinate chose first.
			h.d.Logf("app %q: the divergence of the subordinate %q (%s) was settled there", app, gone.Name, gone.ServerID)
			h.pushResolvedLocked(app, gone.ServerID, nil)
			h.pushDivergedLocked(app)
		}
		return nil
	case i >= 0 && list[i].Since == *since:
		if list[i].Keep != "" {
			h.forwardLocked(app, list[i]) // the choice made while it was away
		}
		return nil
	}
	sd := subDivergence{ServerID: d.Server.ServerID, Name: d.ShownName(), Since: *since}
	if i >= 0 {
		list[i] = sd // a new divergence: a choice made for the one before is not for it
	} else {
		list = append(list, sd)
	}
	if err := h.saveSubDivergences(app, list); err != nil {
		h.d.Logf("app %q: save %s: %v", app, divergencesFile, err)
	}
	h.d.Logf("app %q: the subordinate %q (%s) diverged from this server at %s; its devices and this server's are asked which copy to keep", app, sd.Name, sd.ServerID, sd.Since)
	h.pushDivergedLocked(app)
	return nil
}

// dropServerDivergences forgets the divergences of the server serverID,
// removed here, on every app, and tells the apps' sessions.
func (h *Hub) dropServerDivergences(serverID string) {
	for _, a := range h.d.Apps.List() {
		_, unlock, ok := h.lockListed(a.ID)
		if !ok {
			continue
		}
		// subDivergences already leaves the removed server out.
		raw, err := h.d.Store.ReadSide(a.ID, divergencesFile)
		var doc divergencesDoc
		if err == nil && json.Unmarshal(raw, &doc) == nil && slices.ContainsFunc(doc.Servers, func(sd subDivergence) bool { return sd.ServerID == serverID }) {
			if err := h.saveSubDivergences(a.ID, h.subDivergences(a.ID)); err != nil {
				h.d.Logf("app %q: save %s: %v", a.ID, divergencesFile, err)
			}
			h.pushDivergedLocked(a.ID)
		}
		unlock()
	}
}
