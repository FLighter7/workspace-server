package hub

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/store"
)

// The documents of an app session, answered from the store (see
// docs/architecture.md, "Storage"). The hub keeps, per app, the revision it
// last announced; whenever it finds the store's another (announceLocked),
// it tells the app's sessions and Deps.Changed, so that every session's
// changed frames follow the order of the revisions and the last one names
// the newest. A commit (the link's included), a reconcile and a get, whose
// open reconciles a file changed by hand, announce under the app's lock as
// they finish; any other move the store reports (Store.SetOnMove: a journal
// applied again, by whatever operation came next) is announced by
// storeMoved. Begin, put, delete and abort run under it too, so that none
// recreates an app's staging while RemoveApp deletes the app; a body is
// written outside it. Head and list only read the index.

// LinkDevice is the device the link's transactions are staged under. A
// device id is made of [A-Za-z0-9_-], so it never is one.
const LinkDevice = "~link"

// Head returns the revision of app's set, nil for an empty set.
func (h *Hub) Head(app string) (*string, *protocol.Error) {
	if e := h.serves(app); e != nil {
		return nil, e
	}
	rev, err := h.d.Store.Head(app)
	if err != nil {
		return nil, h.storeError(err)
	}
	return rev, nil
}

// List returns app's set as the store's index holds it.
func (h *Hub) List(app string) (protocol.Listing, *protocol.Error) {
	if e := h.serves(app); e != nil {
		return protocol.Listing{}, e
	}
	l, err := h.d.Store.List(app)
	if err != nil {
		return protocol.Listing{}, h.storeError(err)
	}
	out := protocol.Listing{
		Revision: l.Revision,
		Docs:     make([]protocol.DocInfo, 0, len(l.Docs)),
		Removed:  make([]protocol.Removed, 0, len(l.Removed)),
	}
	for _, d := range l.Docs {
		out.Docs = append(out.Docs, protocol.DocInfo{Key: wireKey(d.Key), Etag: d.Etag, Bytes: d.Bytes})
	}
	for _, r := range l.Removed {
		out.Removed = append(out.Removed, protocol.Removed{Key: wireKey(r.Key), At: r.At.UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// Get opens the document k of app, at etag when it is given. The session
// reads the file without any lock and closes it; a commit meanwhile does not
// change what it reads (store.Open).
func (h *Hub) Get(app string, k protocol.Key, etag *string) (protocol.Doc, *protocol.Stale, *protocol.Error) {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return protocol.Doc{}, nil, errNoLongerServed(app)
	}
	var (
		file *os.File
		info store.DocInfo
		err  error
	)
	if etag != nil {
		file, info, err = h.d.Store.Open(app, storeKey(k), *etag)
	} else {
		file, info, err = h.d.Store.OpenLatest(app, storeKey(k))
	}
	// The open may have reconciled a file changed by hand.
	after := h.announceLocked(app, nil)
	unlock()
	after()
	if err == nil {
		return protocol.Doc{DocInfo: protocol.DocInfo{Key: k, Etag: info.Etag, Bytes: info.Bytes}, Body: file}, nil, nil
	}
	if errors.Is(err, store.ErrStale) {
		rev, herr := h.d.Store.Head(app)
		if herr != nil {
			return protocol.Doc{}, nil, h.storeError(herr)
		}
		return protocol.Doc{}, &protocol.Stale{Revision: rev}, nil
	}
	return protocol.Doc{}, nil, h.storeError(err)
}

// Begin opens device's transaction tx on app, or returns to it.
func (h *Hub) Begin(app, device, tx string) ([]protocol.Staged, *protocol.Error) {
	var staged []store.Staged
	e, err := h.writing(app, device, func() (err error) {
		staged, err = h.d.Store.Begin(app, device, tx)
		return err
	})
	if e != nil {
		return nil, e
	}
	if err != nil {
		return nil, h.storeError(err)
	}
	out := make([]protocol.Staged, 0, len(staged))
	for _, st := range staged {
		out = append(out, protocol.Staged{Key: wireKey(st.Key), Delete: st.Delete, IfEtag: st.IfEtag, Bytes: st.Bytes, Received: st.Received})
	}
	return out, nil
}

// Put stages a body for k in device's transaction tx; the session writes it
// into the Sink returned, which writes it straight into the transaction's
// staging folder.
func (h *Hub) Put(app, device, tx string, k protocol.Key, ifEtag *string, size, offset int64) (protocol.Sink, *protocol.Error) {
	var s *store.Sink
	e, err := h.writing(app, device, func() (err error) {
		s, err = h.d.Store.Put(app, device, tx, storeKey(k), ifEtag, size, offset)
		return err
	})
	if e != nil {
		return nil, e
	}
	if err != nil {
		return nil, h.storeError(err)
	}
	return sink{h, s}, nil
}

// sink is a store's Sink whose errors are protocol errors.
type sink struct {
	h *Hub
	s *store.Sink
}

func (k sink) Write(p []byte) (int, error) {
	n, err := k.s.Write(p)
	if err != nil {
		return n, k.h.storeError(err)
	}
	return n, nil
}

func (k sink) Close() error {
	if err := k.s.Close(); err != nil {
		return k.h.storeError(err)
	}
	return nil
}

// Delete stages the removal of k in device's transaction tx.
func (h *Hub) Delete(app, device, tx string, k protocol.Key, ifEtag *string) *protocol.Error {
	e, err := h.writing(app, device, func() error {
		return h.d.Store.Delete(app, device, tx, storeKey(k), ifEtag)
	})
	if e != nil {
		return e
	}
	if err != nil {
		return h.storeError(err)
	}
	return nil
}

// Abort discards device's transaction tx. A transaction that is gone
// already is aborted as far as the device is concerned.
func (h *Hub) Abort(app, device, tx string) *protocol.Error {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return errNoLongerServed(app)
	}
	err := h.d.Store.Abort(app, device, tx)
	unlock()
	if err != nil && !errors.Is(err, store.ErrNoTransaction) {
		return h.storeError(err)
	}
	return nil
}

// Commit applies device's transaction tx, answers the session (committed,
// or conflict, which leaves the transaction open) and tells every other
// session of the app the new revision, and Deps.Changed. The answer is
// queued under the app's lock, so no changed of a later commit can reach
// the session before it. A device removed meanwhile commits nothing. The
// commit's own reconcile may take in a file changed by hand: the revision
// is compared before and after whatever the outcome, and the sessions are
// told when it moved (after a refusal or a failure, the committing session
// too).
//
// A failure that is none of the store's own refusals is internal, and may
// have come after the store's commit point, with the commit applied in part
// (docs/architecture.md, "Storage"): the store has ended the transaction
// there, so a commit of it again is noTransaction. The client lists again; a
// begin of the same id tells it whether the transaction is still open (a
// failure before the commit point), with what it staged.
func (h *Hub) Commit(from *protocol.Session, app, device, tx string) *protocol.Error {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return errNoLongerServed(app)
	}
	after := func() {}
	defer func() { after() }() // deferred first, so it runs after the locks are released
	defer unlock()
	// Under the app's lock, which RemoveDevice takes too, and the servers
	// lock, which the removal of a server-kind device takes.
	h.servers.RLock()
	defer h.servers.RUnlock()
	if !h.admitted(device, app) {
		return errRemoved()
	}
	c, err := h.d.Store.Commit(app, device, tx)
	var conflict *store.ConflictError
	switch {
	case err == nil:
		from.AnswerCommitted(protocol.Committed{Revision: c.Revision, Docs: wireDocs(c.Docs)})
		after = h.announceLocked(app, from) // the committed frame names the revision
		return nil
	case errors.As(err, &conflict):
		from.AnswerConflict(protocol.Conflict{Revision: conflict.Revision, Docs: wireDocs(conflict.Docs)})
		after = h.announceLocked(app, from) // so does the conflict frame
		return nil
	case refusal(err):
		after = h.announceLocked(app, nil)
		return h.storeError(err)
	}
	after = h.announceLocked(app, nil)
	return h.errInternal(err)
}

// announceLocked reads app's revision and, when it is not the one last
// announced, tells every session of the app but except and makes it the one
// announced. It returns the function that tells Deps.Changed, which the
// caller calls once it has released the app's lock, which it holds for this
// call. Every revision a session hears goes through here, under the app's
// lock, so the pushes follow the order of the revisions, whatever moved
// them. A revision that cannot be read (a journal waiting) is announced
// later, by the operation that applies the journal (Store.SetOnMove).
func (h *Hub) announceLocked(app string, except *protocol.Session) func() {
	now, err := h.d.Store.Head(app)
	if err != nil {
		return func() {}
	}
	h.annMu.Lock()
	last := h.announced[app]
	h.announced[app] = now
	h.annMu.Unlock()
	if sameRevision(last, now) {
		return func() {}
	}
	h.pushLocked(app, now, except)
	return func() { h.changed(app, now) }
}

// storeMoved is the store's report that an operation moved app's revision
// (Store.SetOnMove). It may come from inside a call that holds the app's
// lock, so the announcement runs on a goroutine of its own; by then the
// revision may have moved again, and announceLocked tells the newest.
func (h *Hub) storeMoved(app string) {
	go func() {
		_, unlock, ok := h.lockListed(app)
		if !ok {
			return
		}
		after := h.announceLocked(app, nil)
		unlock()
		after()
	}()
}

func wireDocs(list []store.DocEtag) []protocol.DocEtag {
	docs := make([]protocol.DocEtag, 0, len(list))
	for _, d := range list {
		docs = append(docs, protocol.DocEtag{Key: wireKey(d.Key), Etag: d.Etag})
	}
	return docs
}

// Resolve settles a divergence of the app's servers. Stage 3 of the files
// plan brings divergence; until then no app has one, so every resolve finds
// it settled, as one that another device's answer settled first: the session
// answers resolved.
func (h *Hub) Resolve(from *protocol.Session, app, device, server, keep string) *protocol.Error {
	return h.serves(app)
}

// Reconcile takes the files of app changed by hand into its set
// (store.Reconcile) and, when that changed the revision, tells every session
// of the app and Deps.Changed. The hub does it at every hello for the app.
func (h *Hub) Reconcile(app string) error {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return fmt.Errorf("%w: %q", apps.ErrNotFound, app)
	}
	_, _, err := h.d.Store.Reconcile(app)
	after := h.announceLocked(app, nil)
	unlock()
	after()
	return err
}

// pushLocked tells every session of app but except the revision rev. The
// caller holds the app's lock. PushChanged never blocks and never calls back
// into the hub. A welcome reads its revision outside this lock, so it can be
// newer than a push still to come: welcome(r3), changed(r2), changed(r3).
// That is all protocol.md promises.
func (h *Hub) pushLocked(app string, rev *string, except *protocol.Session) {
	for _, s := range h.registered(func(m member) bool { return m.app == app }) {
		if s != except {
			s.PushChanged(rev)
		}
	}
}

// changed tells Deps.Changed; the caller holds none of the hub's locks.
func (h *Hub) changed(app string, rev *string) {
	if h.d.Changed != nil {
		h.d.Changed(app, rev)
	}
}

// LinkTx is a transaction the link to the main makes on this server's store:
// the main's documents, written here as one change. It is staged under
// LinkDevice, so an app has at most one at a time, and survives a restart
// as a device's does: BeginLink with the same id returns to it. Its bodies
// go straight to the staging folder through the store's Sinks, never
// through memory.
type LinkTx struct {
	h       *Hub
	app, tx string
}

// BeginLink opens the link's transaction tx on app, or returns to it with
// what it staged; the link's other transaction on the app is discarded. tx
// is 16 to 64 of [A-Za-z0-9_-], chosen by the link. An app that is not
// listed is an error wrapping apps.ErrNotFound.
func (h *Hub) BeginLink(app, tx string) (*LinkTx, []store.Staged, error) {
	var staged []store.Staged
	err := h.locked(app, func() (err error) {
		staged, err = h.d.Store.Begin(app, LinkDevice, tx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return &LinkTx{h: h, app: app, tx: tx}, staged, nil
}

// App and Tx name the transaction.
func (t *LinkTx) App() string { return t.app }
func (t *LinkTx) Tx() string  { return t.tx }

// Put stages a body of size bytes for k, as store.Store.Put: the caller
// writes the bytes past offset into the Sink, without any lock, and closes
// it. ifEtag is the etag k must have here at the commit, nil if it must not
// exist.
func (t *LinkTx) Put(k store.Key, ifEtag *string, size, offset int64) (*store.Sink, error) {
	var s *store.Sink
	err := t.h.locked(t.app, func() (err error) {
		s, err = t.h.d.Store.Put(t.app, LinkDevice, t.tx, k, ifEtag, size, offset)
		return err
	})
	return s, err
}

// Delete stages the removal of k, which must have ifEtag here at the
// commit.
func (t *LinkTx) Delete(k store.Key, ifEtag *string) error {
	return t.h.locked(t.app, func() error { return t.h.d.Store.Delete(t.app, LinkDevice, t.tx, k, ifEtag) })
}

// Abort discards the transaction.
func (t *LinkTx) Abort() error {
	return t.h.locked(t.app, func() error { return t.h.d.Store.Abort(t.app, LinkDevice, t.tx) })
}

// Commit applies the transaction, all of it or nothing, and tells every
// session of the app the new revision (none of them wrote it), and
// Deps.Changed. A *store.ConflictError, store.ErrIncomplete, store.ErrQuota
// and store.ErrBadPath leave the transaction open. Any other error may come
// after the store's commit point, with the commit applied in part and the
// transaction ended: the sessions are told the revision if it moved, and
// the link lists again rather than commit again (BeginLink with the same id
// tells whether the transaction is still open). An app no longer listed is
// an error wrapping apps.ErrNotFound.
func (t *LinkTx) Commit() (store.Committed, error) {
	h := t.h
	_, unlock, ok := h.lockListed(t.app)
	if !ok {
		return store.Committed{}, fmt.Errorf("%w: %q", apps.ErrNotFound, t.app)
	}
	c, err := h.d.Store.Commit(t.app, LinkDevice, t.tx)
	// Whatever the outcome: its reconcile may have taken in a hand edit.
	after := h.announceLocked(t.app, nil)
	unlock()
	after()
	return c, err
}

// refusal reports whether err is one of the store's refusals of a commit
// that leave the transaction open (a conflict aside). ErrUnreadable is one:
// it is answered internal, as a passing failure of the server's, but the
// commit stopped before the store's commit point.
func refusal(err error) bool {
	return errors.Is(err, store.ErrIncomplete) || errors.Is(err, store.ErrQuota) ||
		errors.Is(err, store.ErrBadPath) || errors.Is(err, store.ErrNoTransaction) ||
		errors.Is(err, store.ErrUnreadable)
}

// serves refuses an app that is not listed (any more).
func (h *Hub) serves(app string) *protocol.Error {
	if _, ok := h.d.Apps.Get(app); !ok {
		return errNoLongerServed(app)
	}
	return nil
}

// writing runs op, a store call on device's transaction, under the app's
// lock and the servers lock: an app or a device removed meanwhile (both
// removals take these locks) is refused, and op never recreates the staging
// of an app that RemoveApp deleted. The protocol error is a refusal; err is
// op's.
func (h *Hub) writing(app, device string, op func() error) (*protocol.Error, error) {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return errNoLongerServed(app), nil
	}
	defer unlock()
	h.servers.RLock()
	defer h.servers.RUnlock()
	if !h.admitted(device, app) {
		return errRemoved(), nil
	}
	return nil, op()
}

// locked runs op under the app's lock, for the link's transactions. An app
// that is not listed is an error wrapping apps.ErrNotFound.
func (h *Hub) locked(app string, op func() error) error {
	_, unlock, ok := h.lockListed(app)
	if !ok {
		return fmt.Errorf("%w: %q", apps.ErrNotFound, app)
	}
	defer unlock()
	return op()
}

// storeError is the answer to an error of the store. The store's own
// refusals name keys and transaction ids only, never a server path; any
// other error is internal, and its text goes to the log only.
func (h *Hub) storeError(err error) *protocol.Error {
	code := ""
	switch {
	case errors.Is(err, store.ErrBadPath):
		code = protocol.CodeBadPath
	case errors.Is(err, store.ErrQuota):
		code = protocol.CodeQuota
	case errors.Is(err, store.ErrNoTransaction):
		code = protocol.CodeNoTransaction
	case errors.Is(err, store.ErrIncomplete):
		code = protocol.CodeIncomplete
	case errors.Is(err, store.ErrBadTx), errors.Is(err, store.ErrBadBody):
		// The client's view of its transaction is wrong: the session closes,
		// and the client's next begin reports what is staged.
		code = protocol.CodeProtocol
	case errors.Is(err, store.ErrUnreadable):
		// A passing failure for the device, which it reads as internal; the
		// log says which file holds the commit back.
		h.d.Logf("a commit waits for a file the server cannot read now (another program holds it, or its permissions refuse the server); the transaction stays open: %v", err)
		return &protocol.Error{Code: protocol.CodeInternal, Message: "internal error; the server's log has the details"}
	default:
		return h.errInternal(err)
	}
	return &protocol.Error{Code: code, Message: err.Error()}
}

func errNoLongerServed(app string) *protocol.Error {
	return &protocol.Error{Code: protocol.CodeUnknownApp, Message: fmt.Sprintf("this server no longer serves the app %q", app)}
}

func errRemoved() *protocol.Error {
	return &protocol.Error{Code: protocol.CodeBadToken, Message: "this device has been removed; pair it again"}
}

// sameRevision reports whether two revisions are equal, nil (an empty set)
// included.
func sameRevision(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func storeKey(k protocol.Key) store.Key { return store.Key{Path: k.Path, ID: k.ID, Type: k.Type} }

func wireKey(k store.Key) protocol.Key { return protocol.Key{Path: k.Path, ID: k.ID, Type: k.Type} }
