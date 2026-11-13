// Package protocol speaks the sync protocol over one data channel: JSON text
// frames with a "t" field, and document bodies as binary messages of at most
// ChunkBytes each. A Session holds the state of one channel; a Backend (the
// hub) answers what the frames ask for.
//
// Bodies never sit whole in memory: a put's body goes chunk by chunk into a
// Sink the backend gives, written in the message handler as it arrives, and
// a doc's body is read chunk by chunk from the file the backend opened, as
// the channel takes it.
package protocol

import (
	"io"
	"time"
)

// Version is the protocol version a client's hello or pair must carry.
const Version = 1

// ChunkBytes is the largest binary message the server sends.
const ChunkBytes = 16384

// ListPage is the most entries, documents and tombstones together, that one
// list frame carries.
const ListPage = 500

// listPage is the page size the session uses: ListPage, smaller in tests.
var listPage = ListPage

// Error codes sent in error frames. CodeProtocol closes the session; the
// others leave it open, except where a frame's handling says otherwise
// (hello, pair).
const (
	CodeProtocol         = "protocol"
	CodeUnknownApp       = "unknownApp"
	CodeBadSecret        = "badSecret"
	CodeBadToken         = "badToken"
	CodeNotAuthenticated = "notAuthenticated"
	CodeInternal         = "internal"
	// CodeBadPath: a document's path, id or type breaks the rules of a key,
	// or its path is taken on disk by something that is no document.
	CodeBadPath = "badPath"
	// CodeQuota: the app's files would exceed its quota with the
	// transaction.
	CodeQuota = "quota"
	// CodeNoTransaction: no open transaction of that id for this device.
	CodeNoTransaction = "noTransaction"
	// CodeIncomplete: a commit found a body that has not fully arrived.
	CodeIncomplete = "incomplete"
	// CodeBlocked answers hello and pair from an address blocked after too
	// many failed attempts; nothing else is checked. It is temporary: the
	// pairing still stands.
	CodeBlocked = "blocked"
	// CodeNotMain answers pair on a subordinate: devices pair only with the
	// main, which then hands its subordinates out to them.
	CodeNotMain = "notMain"
)

// The roles a server names in welcome: a main has no upstream server; a
// subordinate is linked to a main and mirrors its apps.
const (
	RoleMain        = "main"
	RoleSubordinate = "subordinate"
)

// The kinds a pair may name. A device pairs for one app; a server (a
// subordinate linking to this server) is admitted to every app, and its
// pairing session is a link session, which has no app.
const (
	KindDevice = "device"
	KindServer = "server"
)

// The sides a resolve may keep when an app's servers diverged.
const (
	KeepMain        = "main"
	KeepSubordinate = "subordinate"
)

// Error is a refusal sent to the client as an error frame. Message is
// English, for a developer. It is an error too, so that a Sink can return
// one.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Welcome is what a welcome frame tells an authenticated client. Revision is
// the revision of the app's set, nil for an empty set, and always on a link
// session. Role is RoleMain or RoleSubordinate; an empty Role is left out of
// the frame. Term is the family's term as this server knows it (see
// docs/protocol.md, "Switching the main"); 0 is left out of the frame.
// Subordinates is what a main lists: nil leaves the field out of the frame
// (a subordinate), and an empty, non-nil list sends [] (a main without any).
// Diverged lists the servers whose divergence on the app waits for a
// device's choice; empty leaves the field out.
type Welcome struct {
	ServerID     string
	Name         string
	Revision     *string
	Hosts        []string
	Role         string
	Term         int
	Subordinates []ServerInfo
	Diverged     []DivergedServer
}

// Key names a document as the frames carry it: its folder ("" for the app's
// root), its id and its type. The backend checks it.
type Key struct {
	Path string `json:"path"`
	ID   string `json:"id"`
	Type string `json:"type"`
}

// String returns the key's canonical form, "path/id.type", or "id.type" at
// the app's root: what a listing is sorted by, byte for byte.
func (k Key) String() string {
	if k.Path == "" {
		return k.ID + "." + k.Type
	}
	return k.Path + "/" + k.ID + "." + k.Type
}

// DocInfo is a document of an app's set: its etag (the lowercase hex
// SHA-256 of its bytes) and its size.
type DocInfo struct {
	Key
	Etag  string `json:"etag"`
	Bytes int64  `json:"bytes"`
}

// Removed is the tombstone of a deleted document, and when it went (RFC
// 3339 UTC).
type Removed struct {
	Key
	At string `json:"at"`
}

// Listing is an app's set: its revision (nil for an empty set), its
// documents and its tombstones, each sorted by Key.String(), byte for byte.
type Listing struct {
	Revision *string
	Docs     []DocInfo
	Removed  []Removed
}

// Doc is a document opened for a get. Body reads its bytes from the start;
// the session seeks it to the offset asked for, reads it as the channel
// takes it, and closes it.
type Doc struct {
	DocInfo
	Body io.ReadSeekCloser
}

// Stale answers a request made against a view that the set has left: the
// document asked for is not there (with that etag), or the listing moved
// on. Revision is the set's revision now.
type Stale struct {
	Revision *string
}

// Staged is a change an open transaction holds, as began reports it for
// resuming: a put of Bytes bytes, of which Received have arrived, or a
// delete. IfEtag nil means the document must not exist at the commit.
type Staged struct {
	Key
	Delete   bool    `json:"delete,omitempty"`
	IfEtag   *string `json:"ifEtag"`
	Bytes    int64   `json:"bytes"`
	Received int64   `json:"received"`
}

// DocEtag is a document and its etag, nil when it does not exist.
type DocEtag struct {
	Key
	Etag *string `json:"etag"`
}

// Committed is what a commit changed: the set's new revision, and each
// document written, with its etag, or deleted, with a nil etag.
type Committed struct {
	Revision *string
	Docs     []DocEtag
}

// Conflict answers a commit whose documents are not at the etags it
// expects: the set's revision now, and the current etag of each document
// that conflicts. The transaction stays open.
type Conflict struct {
	Revision *string
	Docs     []DocEtag
}

// DivergedServer is a server whose copy of an app diverged from its main's,
// and since when (RFC 3339 UTC), as welcome and diverged frames carry it.
type DivergedServer struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Since    string `json:"since"`
}

// Sink receives a put's body. The session writes each binary message into
// it as it arrives, in the message handler, and closes it when the body
// stops, complete or not; it also closes it when the session ends under the
// body, so that a later put resumes from what arrived. Close may be called
// more than once, and concurrently with a Write. An error from either that
// is an *Error is answered as such; any other is answered as internal.
type Sink interface {
	Write(p []byte) (int, error)
	Close() error
}

// AppInfo is one entry of the app list a server-kind session is told.
type AppInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ServerInfo is how a device reaches a server: a pairing code without the
// secret. A subordinate announces its own to its main, and a main lists its
// subordinates' in every welcome. Fingerprint is uppercase colon hex.
type ServerInfo struct {
	ServerID    string   `json:"serverId"`
	Name        string   `json:"name"`
	Password    string   `json:"password"`
	Fingerprint string   `json:"fingerprint"`
	Port        int      `json:"port"`
	Hosts       []string `json:"hosts"`
}

// DeviceEntry is one device in the registry a main sends its subordinates:
// never a token, only the token's SHA-256 in lowercase hex. A server-kind
// device (another server of the family) has no app, Kind "server", and
// Server once it announced itself. Name and PairedAt (RFC 3339 UTC) label
// the device for a server that adopts the registry.
type DeviceEntry struct {
	ID        string      `json:"id"`
	App       string      `json:"app"`
	Kind      string      `json:"kind,omitempty"`
	Name      string      `json:"name,omitempty"`
	TokenHash string      `json:"tokenHash"`
	PairedAt  string      `json:"pairedAt,omitempty"`
	Server    *ServerInfo `json:"server,omitempty"`
}

// Tombstone is a device removed from the family, by id, and when (RFC 3339
// UTC).
type Tombstone struct {
	ID string `json:"id"`
	At string `json:"at"`
}

// Registry is a device registry as it travels: the devices and the ids
// removed. A main sends its own to its subordinates; a main that steps down
// hands its own to the main it then follows, in its first announce.
type Registry struct {
	Devices []DeviceEntry
	Removed []Tombstone
}

// TermAnswer answers a term frame: the term and the main the receiving
// server follows once it handled it. Main is the sender's server id when
// the receiver now follows the sender.
type TermAnswer struct {
	Term int    `json:"term"`
	Main string `json:"main"`
}

// Channel is the data channel a Session speaks over. *webrtc.DataChannel
// satisfies it as is.
type Channel interface {
	SendText(s string) error
	Send(data []byte) error
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(th uint64)
	OnBufferedAmountLow(f func())
	Close() error
}

// Backend answers the requests of authenticated sessions. A nil *Error means
// success. Every method is called from the goroutine that delivers the
// session's incoming messages, except Closed and an undoing Forget.
type Backend interface {
	// Hello authenticates a known device. s.App() and s.Device() are still
	// empty during the call; s.Remote() is set. s.PushChanged and
	// s.PushApps may be called during the call: the session holds the push
	// until after its welcome. An empty app asks for a link session, which
	// only a server-kind device may open.
	Hello(s *Session, app, device, token string) (Welcome, *Error)
	// Pair registers a new device with a one-time secret. As for Hello. kind
	// is KindDevice or KindServer; a server-kind pair names no app (app is
	// empty) and opens a link session.
	Pair(s *Session, kind, app, secret, name string) (device, token string, w Welcome, err *Error)
	// Apps returns the app list for an authenticated session that asks for
	// it. Only a server-kind device may ask: any other gets an error with
	// CodeProtocol, which closes the session.
	Apps(s *Session) ([]AppInfo, *Error)
	// Announce takes a subordinate's connection data from its link session
	// and answers with the device registry, which it sends with
	// s.PushDevices, as it does again whenever the registry changes.
	// handover, when not nil, is the subordinate's own registry, which it
	// hands over because it was a main and has stepped down to this one. The
	// session calls it only for a link session; an error with CodeProtocol
	// closes the session.
	Announce(s *Session, server ServerInfo, handover *Registry) *Error
	// Term tells this server that the sender is the main of its family at
	// term, reached at server; device and token, when given, are a
	// server-kind credential for this server there. The backend follows the
	// sender when (term, server's id) is higher than its own main's, and
	// answers with the term and main it follows then. The session calls it
	// only for a link session; an error with CodeProtocol closes the
	// session.
	Term(s *Session, term int, server ServerInfo, device, token string) (TermAnswer, *Error)
	// Invite issues a pairing code of this server for a subordinate, which
	// shows it for a browser on its own computer: the code this server's own
	// page issues, with a new secret. The session calls it only for a link
	// session; an error with CodeProtocol closes the session, any other is
	// answered and the session stays open.
	Invite(s *Session) (code string, expires time.Time, err *Error)

	// The documents, for app sessions only. An error with CodeProtocol
	// closes the session; any other is answered, and the session stays
	// open.

	// Head returns the revision of app's set, nil for an empty set.
	Head(app string) (*string, *Error)
	// List returns app's set, sorted as Listing says.
	List(app string) (Listing, *Error)
	// Get opens the document k of app if its etag is *etag, or whatever its
	// etag when etag is nil. A document that is not there (with that etag)
	// is a *Stale.
	Get(app string, k Key, etag *string) (Doc, *Stale, *Error)
	// Begin opens device's transaction tx on app, or returns to it, and
	// returns what it has staged. It discards the device's other open
	// transaction on the app.
	Begin(app, device, tx string) ([]Staged, *Error)
	// Put stages a body of size bytes for k in device's transaction tx, to
	// be written from offset on, and returns the Sink that takes the bytes
	// past offset.
	Put(app, device, tx string, k Key, ifEtag *string, size, offset int64) (Sink, *Error)
	// Delete stages the removal of k in device's transaction tx.
	Delete(app, device, tx string, k Key, ifEtag *string) *Error
	// Commit applies device's transaction tx, all of it or nothing, and
	// tells every other session of the app the new revision. It answers
	// itself, with from.AnswerCommitted or, on a conflict, which leaves the
	// transaction open, from.AnswerConflict, under the lock that orders its
	// pushes of changed: the session's frames then follow the order of the
	// revisions. An error is answered by the session.
	Commit(from *Session, app, device, tx string) *Error
	// Abort discards device's transaction tx.
	Abort(app, device, tx string) *Error
	// Resolve settles a divergence of the app's servers: server is the
	// subordinate's server id, keep KeepMain or KeepSubordinate.
	Resolve(from *Session, app, device, server, keep string) *Error

	// Forget deletes the device and closes its sessions. It answers the
	// client's forget, and it also undoes a pairing the client has not
	// confirmed (see Session) when its session ends: then it may be called
	// from any goroutine, but never under a lock the backend holds.
	Forget(app, device string)
	// Closed is told that s has closed. It may be called from any goroutine,
	// concurrently with a Hello or Pair for s, and more than once for the same
	// session: when s closes while its Hello or Pair is in flight, Closed is
	// called again after that call returns, so a registration made inside it
	// does not outlive the session.
	Closed(s *Session)
}

// Options configure a Session.
type Options struct {
	// AuthTimeout closes a session that has not authenticated in time. Zero
	// means no deadline.
	AuthTimeout time.Duration
	// OnClose, if set, runs once when the session closes, after the channel
	// and the backend have been told.
	OnClose func()
}
