// Package protocol speaks the sync protocol over one data channel: JSON text
// frames with a "t" field, and document bodies as binary messages of at most
// ChunkBytes each. A Session holds the state of one channel; a Backend (the
// hub) answers what the frames ask for.
package protocol

import "time"

// Version is the protocol version a client's hello or pair must carry.
const Version = 1

// ChunkBytes is the largest binary message the server sends.
const ChunkBytes = 16384

// Error codes sent in error frames.
const (
	CodeProtocol         = "protocol"
	CodeUnknownApp       = "unknownApp"
	CodeBadSecret        = "badSecret"
	CodeBadToken         = "badToken"
	CodeTooLarge         = "tooLarge"
	CodeNotJSON          = "notJson"
	CodeNotAuthenticated = "notAuthenticated"
	CodeInternal         = "internal"
	// CodeBlocked answers hello and pair from an address blocked after too
	// many failed attempts; nothing else is checked. It is temporary: the
	// pairing still stands.
	CodeBlocked = "blocked"
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

// Error is a refusal sent to the client as an error frame. Message is
// English, for a developer.
type Error struct {
	Code    string
	Message string
}

// Welcome is what a welcome frame tells an authenticated client. Etag is nil
// when the app has no document, and always on a link session. Role is
// RoleMain or RoleSubordinate; an empty Role is left out of the frame.
type Welcome struct {
	ServerID string
	Name     string
	Etag     *string
	Hosts    []string
	Role     string
}

// AppInfo is one entry of the app list a server-kind session is told.
type AppInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
	Head(app string) (*string, *Error)
	Get(app string) ([]byte, *string, *Error)
	// MaxBytes is the largest body a put to app may announce.
	MaxBytes(app string) int64
	Put(from *Session, app, device string, ifEtag *string, body []byte) (etag string, conflict bool, current *string, err *Error)
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
