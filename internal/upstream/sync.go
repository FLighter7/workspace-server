package upstream

// The sync rule (docs/architecture.md, "The sync rule"): an app's set is
// compared document by document, by key, three ways: its base (the etags
// this server and its main last agreed on, link.json), this server's set and
// the main's. Per key:
//
//   - the same on both sides: agreed;
//   - only the main's moved since the base: pulled, written here;
//   - only this server's moved: pushed, written on the main;
//   - both moved, differently: the two diverged.
//
// Without a base (a first link, or a base agreed with another main) a side
// whose set is empty takes the other's, and two different sets that are
// both non-empty have diverged. For now a divergence is settled at once by
// the old rule: the main's set wins, and replaces this server's whole
// (diverge). Stage 3 of the files plan makes it wait for a device's choice
// instead; decide and diverge are its seams.
//
// Writes go both ways as transactions, all of an app's changes on one side
// in one: here through the hub's LinkTx, on the main through the client's
// begin, put, delete and commit. Every change names the etag it expects, so
// a device that wrote first on either side costs a decision, never a
// document. Bodies are streamed, never whole in memory: a pull writes what
// the main's get sends straight into the transaction's staging file here,
// and a push reads this server's file as the channel takes it. Both
// transactions are recorded in link.json before they start, so that one cut
// off (the connection dropped, the server restarted) resumes where its
// bodies stopped.
//
// The link's own commits come back to it through the hub's Changed, like a
// device's: sync then finds this server's set and the main's revision both
// at the base, and does nothing, without reaching the main.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/FLighter7/workspace-server/internal/hub"
	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// linkFile is the side file in an app's .wss folder that holds the link's
// state for the app.
const linkFile = "link.json"

// appLink is link.json: the main it was agreed with (ServerID), the base,
// and the transactions in flight on each side. A nil Base is no base: never
// agreed, or agreed with another main; an empty one is an agreed empty set.
type appLink struct {
	ServerID string            `json:"serverId"`
	Base     map[string]string `json:"base"`
	// Pull is the transaction here that takes the main's documents, Push
	// the one on the main that takes this server's, while they are open.
	Pull *pendingTx `json:"pull,omitempty"`
	Push *pendingTx `json:"push,omitempty"`
}

// pendingTx is a transaction in flight: its id, per document it puts the
// etag of the body it sends, by which a resume knows that a body staged
// before is the same one, and the documents it deletes. Both also tell, once
// the transaction may have committed without its base being saved, what the
// other side then holds (effectiveBase).
type pendingTx struct {
	Tx    string            `json:"tx"`
	Etags map[string]string `json:"etags"`
	Gone  []string          `json:"gone,omitempty"`
}

func (a *appLink) clone() *appLink {
	c := *a
	c.Base = maps.Clone(a.Base)
	c.Pull, c.Push = a.Pull.clone(), a.Push.clone()
	return &c
}

func (p *pendingTx) clone() *pendingTx {
	if p == nil {
		return nil
	}
	c := *p
	c.Etags = maps.Clone(p.Etags)
	c.Gone = slices.Clone(p.Gone)
	return &c
}

// targetsOf returns what cs writes: the etag of each body it puts, and the
// keys it deletes.
func targetsOf(cs []change) (map[string]string, []string) {
	puts, gone := map[string]string{}, []string(nil)
	for _, c := range cs {
		if c.to != nil {
			puts[c.key] = *c.to
		} else {
			gone = append(gone, c.key)
		}
	}
	return puts, gone
}

// valid reports whether a link.json read from disk makes sense: valid keys,
// etags and transaction ids.
func (a *appLink) valid() bool {
	for ks, etag := range a.Base {
		if _, ok := store.ParseKey(ks); !ok || !etagPattern.MatchString(etag) {
			return false
		}
	}
	for _, p := range []*pendingTx{a.Pull, a.Push} {
		if p == nil {
			continue
		}
		if !store.ValidTx(p.Tx) {
			return false
		}
		for ks, etag := range p.Etags {
			if _, ok := store.ParseKey(ks); !ok || !etagPattern.MatchString(etag) {
				return false
			}
		}
		for _, ks := range p.Gone {
			if _, ok := store.ParseKey(ks); !ok {
				return false
			}
		}
	}
	return true
}

// linkOf returns a copy of app's link state with the main mainID, read from
// link.json at first: a missing file, one that does not read, or one agreed
// with another main is a state without a base.
func (l *Link) linkOf(app, mainID string) *appLink {
	l.mu.Lock()
	cached := l.links[app]
	l.mu.Unlock()
	if cached != nil && cached.ServerID == mainID {
		return cached.clone()
	}
	a := &appLink{ServerID: mainID}
	raw, err := l.d.Store.ReadSide(app, linkFile)
	var read appLink
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		l.d.Logf("app %q: read %s: %v; syncing without a base", app, linkFile, err)
	case json.Unmarshal(raw, &read) != nil || !read.valid():
		l.d.Logf("app %q: %s does not read; syncing without a base", app, linkFile)
	case read.ServerID != mainID:
		if read.Base != nil {
			l.d.Logf("app %q: the base was agreed with another main (%s); syncing without one", app, read.ServerID)
		}
	default:
		a = &read
	}
	l.mu.Lock()
	l.links[app] = a.clone()
	l.mu.Unlock()
	return a
}

// saveLink writes app's link state to link.json, and keeps it.
func (l *Link) saveLink(app string, a *appLink) error {
	data, err := json.MarshalIndent(a, "", "\t")
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.links[app] = a.clone()
	l.mu.Unlock()
	if err := l.d.Store.WriteSide(app, linkFile, append(data, '\n')); err != nil {
		return fmt.Errorf("save %s of %q: %w", linkFile, app, err)
	}
	return nil
}

// forgetLink removes app's link.json: the next link is a first link.
func (l *Link) forgetLink(app string) {
	l.mu.Lock()
	delete(l.links, app)
	l.mu.Unlock()
	if err := l.d.Store.RemoveSide(app, linkFile); err != nil {
		l.d.Logf("app %q: delete %s: %v", app, linkFile, err)
	}
}

// forgetLinks removes the link.json of every app listed here: the server is
// no longer linked.
func (l *Link) forgetLinks() {
	if l.d.Apps == nil {
		return
	}
	for _, a := range l.d.Apps.List() {
		l.forgetLink(a.ID)
	}
}

// action is what one sync does with one document.
type action int

const (
	agreed   action = iota // both sides hold the same
	pull                   // only the main's moved: take it here
	push                   // only this server's moved: send it to the main
	conflict               // both moved, differently
)

// decide is the sync rule for one key, given its etag in the base, here and
// on the main, nil where it does not exist. A seam: stage 3 keeps the rule
// and changes what a conflict leads to (see diverge).
func decide(base, here, main *string) action {
	switch {
	case same(here, main):
		return agreed
	case same(here, base):
		return pull
	case same(main, base):
		return push
	default:
		return conflict
	}
}

// change is a write one side takes: the document key (Key.String()), the
// etag it has on that side now (nil: absent there), and the etag it gets
// (nil: deleted).
type change struct {
	key      string
	from, to *string
}

// plan is what one sync does: the changes written here and those written on
// the main, and the documents both sides already agree on. diverged names
// why the two diverged, empty when they did not; firstLink tells a
// divergence of a link without a base.
type plan struct {
	pulls, pushes []change
	agree         map[string]string
	diverged      []string
	firstLink     bool
}

// compare plans a sync of the sets here and main (etags by key) from base,
// nil when there is none.
func compare(base, here, main map[string]string) plan {
	if base == nil {
		switch {
		case len(here) == 0:
			base = here // everything moved on the main
		case len(main) == 0:
			base = main // everything moved here
		}
	}
	p := plan{agree: map[string]string{}}
	firstLink := base == nil
	for _, ks := range union(here, main) {
		h, m := etagIn(here, ks), etagIn(main, ks)
		if same(h, m) {
			p.agree[ks] = *h
			continue
		}
		if firstLink {
			p.diverged = append(p.diverged, ks)
			continue
		}
		switch decide(etagIn(base, ks), h, m) {
		case pull:
			p.pulls = append(p.pulls, change{key: ks, from: h, to: m})
		case push:
			p.pushes = append(p.pushes, change{key: ks, from: m, to: h})
		case conflict:
			p.diverged = append(p.diverged, ks)
		}
	}
	p.firstLink = firstLink && len(p.diverged) > 0
	return p
}

// diverge settles a divergence: for now the main's set wins here, the whole
// of it (the files plan's decision 5), replacing this server's: what only
// this server holds goes, what it changed takes the main's. The devices here
// merge the main's set with what they hold, as they always have, and write
// the result back, which the next sync pushes. Stage 3 makes this wait for a
// device's choice instead.
func diverge(p plan, here, main map[string]string) plan {
	out := plan{agree: p.agree, diverged: p.diverged, firstLink: p.firstLink}
	for _, ks := range union(here, main) {
		h, m := etagIn(here, ks), etagIn(main, ks)
		if !same(h, m) {
			out.pulls = append(out.pulls, change{key: ks, from: h, to: m})
		}
	}
	return out
}

func etagIn(set map[string]string, ks string) *string {
	if e, ok := set[ks]; ok {
		return &e
	}
	return nil
}

// union returns the keys of a and b, sorted.
func union(a, b map[string]string) []string {
	keys := slices.Collect(maps.Keys(a))
	for ks := range b {
		if _, ok := a[ks]; !ok {
			keys = append(keys, ks)
		}
	}
	slices.Sort(keys)
	return keys
}

// errRedecide: what a sync decided on no longer holds (a device here or on
// the main wrote first, or the main's set changed under a read); it decides
// again.
var errRedecide = errors.New("the documents changed meanwhile")

// sync brings app in line with the main, whose set the link last heard to be
// at revision up, and returns the main's revision as it knows it now. It
// decides again, a few times at most, when a write loses a race with a
// device on either side; whoever wrote also left a reason to sync again (a
// changed push, or a local write), so a sync that runs out of attempts is
// followed by another.
func (l *Link) sync(ctx context.Context, cl *wsclient.Client, app, mainID string, up *string) (*string, error) {
	for range syncAttempts {
		lk := l.linkOf(app, mainID)
		local, err := l.d.Store.Head(app)
		if err != nil {
			return up, err
		}
		if lk.Base != nil && lk.Pull == nil && lk.Push == nil {
			if base := store.RevisionOf(lk.Base); same(local, base) && same(l.comparable(app, up), base) {
				return up, nil // in step: nothing to read on either side
			}
		}
		here, err := l.d.Store.List(app)
		if err != nil {
			return up, err
		}
		main, err := cl.ListAll(ctx)
		if err != nil {
			return up, refused(err)
		}
		up = main.Revision
		l.setUp(app, up)
		hereSet, mainSet, sizes := setOf(here), l.mainSet(app, main), map[string]int64{}
		for _, d := range main.Docs {
			sizes[d.Key.String()] = d.Bytes
		}
		l.setComparable(app, up, mainSet, len(main.Docs))

		base := effectiveBase(lk, hereSet, mainSet)
		p := compare(base, hereSet, mainSet)
		if len(p.diverged) > 0 {
			l.logDivergence(app, lk, p, hereSet, mainSet)
			p = diverge(p, hereSet, mainSet)
		}
		if len(p.pulls) == 0 && len(p.pushes) == 0 {
			l.abortPending(ctx, cl, app, lk)
			lk.Base = p.agree
			if err := l.saveLink(app, lk); err != nil {
				return up, err
			}
			return up, nil
		}
		// The base the writes settle into, saved only once one commits: a
		// base saved before would tell the next sync that what neither
		// side's commit has written yet was agreed.
		next := nextBase(base, p.agree, union(hereSet, mainSet))

		var pullErr error
		if len(p.pulls) > 0 {
			err := l.pull(ctx, cl, app, lk, next, p, sizes)
			if errors.Is(err, errRedecide) {
				continue
			}
			// A pull that fails (one this server's quota refuses, say) does
			// not hold this server's own changes back: they are pushed all
			// the same, and the error then ends the session, which is tried
			// again after its backoff.
			pullErr = err
		}
		if len(p.pushes) > 0 {
			rev, err := l.push(ctx, cl, app, lk, next, p.pushes)
			if errors.Is(err, errRedecide) && pullErr == nil {
				continue
			}
			if err != nil && !errors.Is(err, errRedecide) {
				return up, err
			}
			if err == nil {
				up = rev
				l.setUp(app, up)
			}
		}
		if pullErr != nil {
			return up, pullErr
		}
	}
	return up, nil
}

// effectiveBase is lk's base, with what a transaction lk still records shows
// to have landed on the side it writes: a document that side holds at the
// etag the transaction puts, or lacks when the transaction deletes it, has
// that as its base. A commit whose base was not saved after it (a crash, a
// save that failed) then leaves nothing that looks changed on both sides,
// which would be a divergence. A record of a transaction that never
// committed changes nothing this way that the two do not already agree on.
func effectiveBase(lk *appLink, here, main map[string]string) map[string]string {
	base := maps.Clone(lk.Base)
	apply := func(p *pendingTx, side map[string]string) {
		if p == nil {
			return
		}
		for ks, etag := range p.Etags {
			if e, ok := side[ks]; ok && e == etag {
				if base == nil {
					base = map[string]string{}
				}
				base[ks] = etag
			}
		}
		for _, ks := range p.Gone {
			if _, ok := side[ks]; !ok && base != nil {
				delete(base, ks)
			}
		}
	}
	apply(lk.Push, main)
	apply(lk.Pull, here)
	return base
}

// setOf returns a store listing's documents as etags by key.
func setOf(list store.Listing) map[string]string {
	set := make(map[string]string, len(list.Docs))
	for _, d := range list.Docs {
		set[d.Key.String()] = d.Etag
	}
	return set
}

// mainSet returns the main's listing as etags by key. A key that is not a
// valid one here is left out, and logged: it can be neither written nor
// compared. The revision of what is left is what the link compares the base
// with (setComparable).
func (l *Link) mainSet(app string, list wsclient.Listing) map[string]string {
	set := make(map[string]string, len(list.Docs))
	for _, d := range list.Docs {
		ks := d.Key.String()
		if k, ok := store.ParseKey(ks); !ok || k.Path != d.Path || k.ID != d.ID || k.Type != d.Type {
			l.d.Logf("app %q: the main lists %q, which is not a valid document name here; it is left out", app, ks)
			continue
		}
		set[ks] = d.Etag
	}
	return set
}

// nextBase returns the base before the writes of a sync: what both sides
// agree on is agreed, whatever base said; a document neither side holds is
// agreed to be gone; any other of keys keeps what base said of it.
func nextBase(base, agree map[string]string, keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, ks := range keys {
		if e, ok := agree[ks]; ok {
			out[ks] = e
		} else if e, ok := base[ks]; ok {
			out[ks] = e
		}
	}
	return out
}

// settle records in base what a commit reports it wrote: each document at
// the etag the committing side gives it now, or gone. It is the commit's
// word, never what the link meant to write, so a body that is not the one
// meant (a resume gone wrong) shows as a difference the next sync settles,
// instead of hiding behind the base. It returns the documents whose etag is
// not the one cs meant.
func settle(base map[string]string, cs []change, written map[string]*string) []string {
	var odd []string
	for _, c := range cs {
		etag, ok := written[c.key]
		switch {
		case !ok:
			odd = append(odd, c.key) // not reported: the base keeps what it said
			continue
		case etag == nil:
			delete(base, c.key)
		default:
			base[c.key] = *etag
		}
		if !same(etag, c.to) {
			odd = append(odd, c.key)
		}
	}
	return odd
}

// logDivergence says why app diverged, and what the old rule does about it.
func (l *Link) logDivergence(app string, lk *appLink, p plan, here, main map[string]string) {
	why := "they were never agreed on"
	switch {
	case !p.firstLink:
		why = "both changed " + keyList(p.diverged) + " since they last agreed"
	case lk.Base == nil && lk.ServerID != "" && len(here) > 0 && len(main) > 0:
		why = "they were never agreed on, and both hold documents"
	}
	l.d.Logf("app %q: this server's documents and the main's diverged (%s); the main's set wins here", app, why)
}

// keyList names a few keys, for the log.
func keyList(keys []string) string {
	if len(keys) > 3 {
		return strings.Join(keys[:3], ", ") + fmt.Sprintf(" and %d more", len(keys)-3)
	}
	return strings.Join(keys, ", ")
}

// newTx returns a new transaction id: 22 characters of base64url.
func newTx() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// abortPending ends the transactions lk still records, when the two sides
// are in step without them, so their staging goes at once rather than at its
// expiry.
func (l *Link) abortPending(ctx context.Context, cl *wsclient.Client, app string, lk *appLink) {
	if lk.Pull != nil {
		if tx, _, err := l.d.Writer.BeginLink(app, lk.Pull.Tx); err == nil {
			_ = tx.Abort()
		}
		lk.Pull = nil
	}
	if lk.Push != nil {
		_ = cl.Abort(ctx, lk.Push.Tx)
		lk.Push = nil
	}
}

// pull writes the changes p.pulls here, in one transaction (LinkTx), with
// the bodies read from the main: what a transaction cut off before staged is
// resumed from where it stopped. A device here that wrote first, or a main's
// document that changed under the read, is errRedecide.
func (l *Link) pull(ctx context.Context, cl *wsclient.Client, app string, lk *appLink, next map[string]string, p plan, sizes map[string]int64) error {
	tx, staged, err := l.beginPull(app, lk, p.pulls)
	if err != nil {
		return err
	}
	resumed := int64(0)
	for _, c := range p.pulls {
		k, _ := store.ParseKey(c.key)
		if c.to == nil {
			if err := tx.Delete(k, c.from); err != nil {
				return fmt.Errorf("stage the delete of %s: %w", c.key, err)
			}
			continue
		}
		size := sizes[c.key]
		offset := int64(0)
		if s, ok := staged[c.key]; ok && !s.Delete && s.Bytes == size {
			offset = s.Received
		}
		sink, err := tx.Put(k, c.from, size, offset)
		if err != nil && offset > 0 {
			offset = 0 // what was staged is gone: start the body again
			sink, err = tx.Put(k, c.from, size, 0)
		}
		if err != nil {
			return fmt.Errorf("stage %s: %w", c.key, err)
		}
		resumed += offset
		if offset == size {
			continue // the whole body is staged already
		}
		_, err = cl.Get(ctx, wsclient.Key{Path: k.Path, ID: k.ID, Type: k.Type}, c.to, offset, l.pullWriter(sink, cl))
		// Closed whatever happened: what arrived stays staged, synced, for
		// the next attempt to resume.
		cerr := sink.Close()
		if _, stale := wsclient.IsStale(err); stale {
			return errRedecide
		}
		if err != nil {
			return refused(err)
		}
		if cerr != nil {
			return fmt.Errorf("stage %s: %w", c.key, cerr)
		}
	}
	c, err := tx.Commit()
	if err != nil {
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			l.d.Logf("app %q: a device here wrote before the main's documents were taken in; deciding again", app)
			return errRedecide
		}
		return fmt.Errorf("take the main's documents in: %w", err)
	}
	how := fmt.Sprintf("pulled from the main: %s", describe(p.pulls))
	if len(p.diverged) > 0 {
		how = fmt.Sprintf("the main's set replaced this server's: %s", describe(p.pulls))
	}
	if resumed > 0 {
		how += fmt.Sprintf(" (resumed, %d bytes were staged already)", resumed)
	}
	l.moved(app, "%s", how)
	written := map[string]*string{}
	for _, d := range c.Docs {
		written[d.Key.String()] = d.Etag
	}
	l.settled(app, lk, next, p.pulls, written, "here")
	lk.Pull = nil
	return l.saveLink(app, lk)
}

// settled records a commit's writes in next, which becomes lk's base, and
// logs the documents the commit did not write as meant.
func (l *Link) settled(app string, lk *appLink, next map[string]string, cs []change, written map[string]*string, where string) {
	if odd := settle(next, cs, written); len(odd) > 0 {
		l.d.Logf("app %q: the commit %s wrote %s otherwise than meant; the next sync settles it", app, where, keyList(odd))
	}
	lk.Base = next
}

// beginPull opens the transaction here that takes the changes cs: the one
// link.json records, when everything it staged is still wanted, or a new
// one. It returns what that transaction staged before, by key, for the
// bodies whose etag is still the one wanted.
func (l *Link) beginPull(app string, lk *appLink, cs []change) (*hub.LinkTx, map[string]store.Staged, error) {
	targets, gone := targetsOf(cs)
	if lk.Pull != nil {
		tx, staged, err := l.d.Writer.BeginLink(app, lk.Pull.Tx)
		if err != nil {
			return nil, nil, err
		}
		if keep := reusable(staged, lk.Pull.Etags, cs, targets); keep != nil {
			lk.Pull.Etags, lk.Pull.Gone = targets, gone
			if err := l.saveLink(app, lk); err != nil {
				return nil, nil, err
			}
			return tx, keep, nil
		}
		_ = tx.Abort()
	}
	lk.Pull = &pendingTx{Tx: newTx(), Etags: targets, Gone: gone}
	if err := l.saveLink(app, lk); err != nil {
		return nil, nil, err
	}
	tx, _, err := l.d.Writer.BeginLink(app, lk.Pull.Tx)
	return tx, map[string]store.Staged{}, err
}

// reusable returns the bodies a transaction staged, by key, when the
// transaction may go on with cs: every change it staged is one of cs, and
// every body it staged is recorded (was) at the etag cs puts there now. It
// returns nil otherwise, and a new transaction starts: a body staged for
// another etag is never resumed, and never left in a transaction whose
// record names the new etag, where a cut before it is staged again would
// resume it as the new body, mixing the two.
func reusable(staged []store.Staged, was map[string]string, cs []change, targets map[string]string) map[string]store.Staged {
	want := map[string]bool{}
	for _, c := range cs {
		want[c.key] = true
	}
	keep := map[string]store.Staged{}
	for _, s := range staged {
		ks := s.Key.String()
		if !want[ks] {
			return nil
		}
		if s.Delete {
			continue
		}
		if was[ks] == "" || was[ks] != targets[ks] {
			return nil
		}
		keep[ks] = s
	}
	return keep
}

// push writes the changes cs on the main, in one transaction there, with
// the bodies read from this server's files: what a transaction cut off
// before staged is resumed from where it stopped. It returns the main's new
// revision. A device here that wrote meanwhile, or a main whose documents
// are no longer the ones cs expects, is errRedecide. Each push the main
// commits counts against the write guard, which refuses one past it before
// it starts.
func (l *Link) push(ctx context.Context, cl *wsclient.Client, app string, lk *appLink, next map[string]string, cs []change) (*string, error) {
	if !l.mayPush(app) {
		return nil, errTooManyWrites
	}
	tx, staged, err := l.beginPush(ctx, cl, app, lk, cs)
	if err != nil {
		return nil, err
	}
	resumed := int64(0)
	for _, c := range cs {
		k, _ := store.ParseKey(c.key)
		wk := wsclient.Key{Path: k.Path, ID: k.ID, Type: k.Type}
		if c.to == nil {
			if _, err := cl.Delete(ctx, tx, wk, c.from); err != nil {
				return nil, refused(err)
			}
			continue
		}
		f, info, err := l.d.Store.Open(app, k, *c.to)
		if errors.Is(err, store.ErrStale) {
			return nil, errRedecide // a device here changed it meanwhile
		}
		if err != nil {
			return nil, err
		}
		offset := int64(0)
		if s, ok := staged[c.key]; ok && !s.Delete && s.Bytes == info.Bytes {
			offset = s.Received
		}
		resumed += offset
		_, err = f.Seek(offset, io.SeekStart)
		if err == nil {
			_, err = cl.Put(ctx, tx, wk, c.from, info.Bytes, offset, l.pushReader(f, cl))
		}
		_ = f.Close()
		if err != nil {
			return nil, refused(err)
		}
	}
	r, err := cl.Commit(ctx, tx)
	if err != nil {
		return nil, refused(err)
	}
	if !r.Committed {
		l.d.Logf("app %q: the main's documents changed before this server's reached it (it is at %s now); deciding again", app, short(r.Revision))
		return nil, errRedecide
	}
	l.countPush(app)
	how := fmt.Sprintf("pushed to the main: %s", describe(cs))
	if resumed > 0 {
		how += fmt.Sprintf(" (resumed, %d bytes were staged already)", resumed)
	}
	l.moved(app, "%s", how)
	written := map[string]*string{}
	for _, d := range r.Docs {
		written[d.Key.String()] = d.Etag
	}
	l.settled(app, lk, next, cs, written, "on the main")
	lk.Push = nil
	return r.Revision, l.saveLink(app, lk)
}

// beginPush opens the transaction on the main that takes the changes cs, as
// beginPull does here.
func (l *Link) beginPush(ctx context.Context, cl *wsclient.Client, app string, lk *appLink, cs []change) (string, map[string]store.Staged, error) {
	targets, gone := targetsOf(cs)
	if lk.Push != nil {
		b, err := cl.Begin(ctx, lk.Push.Tx)
		if err != nil {
			return "", nil, refused(err)
		}
		staged := make([]store.Staged, 0, len(b.Staged))
		for _, s := range b.Staged {
			k, ok := store.ParseKey(s.Key.String())
			if !ok {
				staged = nil
				break
			}
			staged = append(staged, store.Staged{Key: k, Delete: s.Delete, IfEtag: s.IfEtag, Bytes: s.Bytes, Received: s.Received})
		}
		if keep := reusable(staged, lk.Push.Etags, cs, targets); staged != nil && keep != nil {
			lk.Push.Etags, lk.Push.Gone = targets, gone
			if err := l.saveLink(app, lk); err != nil {
				return "", nil, err
			}
			return lk.Push.Tx, keep, nil
		}
		// A begin of another id discards this one on the main.
	}
	lk.Push = &pendingTx{Tx: newTx(), Etags: targets, Gone: gone}
	if err := l.saveLink(app, lk); err != nil {
		return "", nil, err
	}
	if _, err := cl.Begin(ctx, lk.Push.Tx); err != nil {
		return "", nil, refused(err)
	}
	return lk.Push.Tx, map[string]store.Staged{}, nil
}

// pullWriter is where a pulled body goes: the sink, or what a test wraps it
// in.
func (l *Link) pullWriter(sink io.Writer, cl *wsclient.Client) io.Writer {
	l.mu.Lock()
	wrap := l.wrapPull
	l.mu.Unlock()
	if wrap != nil {
		return wrap(sink, cl)
	}
	return sink
}

// pushReader is where a pushed body comes from: the file, or what a test
// wraps it in.
func (l *Link) pushReader(f io.Reader, cl *wsclient.Client) io.Reader {
	l.mu.Lock()
	wrap := l.wrapPush
	l.mu.Unlock()
	if wrap != nil {
		return wrap(f, cl)
	}
	return f
}

// describe names what a transaction changed, for the log.
func describe(cs []change) string {
	var put, del []string
	for _, c := range cs {
		if c.to == nil {
			del = append(del, c.key)
		} else {
			put = append(put, c.key)
		}
	}
	var parts []string
	if len(put) > 0 {
		parts = append(parts, "wrote "+keyList(put))
	}
	if len(del) > 0 {
		parts = append(parts, "deleted "+keyList(del))
	}
	return strings.Join(parts, "; ")
}
