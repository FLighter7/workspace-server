package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The documents of an app session (docs/protocol.md): an app's set is a set
// of documents, each named by a Key. Bodies never sit whole in memory here:
// a put reads its body from an io.Reader as the channel takes it, and a get
// writes the body to an io.Writer as it arrives.

// ListPage is the most entries, documents and tombstones together, that one
// list page of a server carries. A client counts on no page size.
const ListPage = 500

// The sides a resolve may keep when an app's servers diverged.
const (
	KeepMain        = "main"
	KeepSubordinate = "subordinate"
)

// listAttempts bounds how often ListAll starts over after the set changed
// between its pages.
const listAttempts = 8

// Key names a document: its folder ("" for the app's root), its id and its
// type.
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

// Page is one page of a listing: the set's revision (nil for an empty set),
// its documents, then its tombstones, and the cursor of the next page, empty
// after the last.
type Page struct {
	Revision *string   `json:"revision"`
	Docs     []DocInfo `json:"docs"`
	Removed  []Removed `json:"removed"`
	Cursor   string    `json:"cursor,omitempty"`
}

// Listing is an app's whole set, every page of one revision.
type Listing struct {
	Revision *string
	Docs     []DocInfo
	Removed  []Removed
}

// Doc is what a get answered: the document, and the offset its body was sent
// from.
type Doc struct {
	DocInfo
	Offset int64 `json:"offset"`
}

// StaleError answers a request made against a view the set has left: the
// document asked for is not there with that etag, or the listing moved on.
// Revision is the set's revision now. The client stays usable: list again.
type StaleError struct {
	Revision *string
}

func (e *StaleError) Error() string {
	return "the app's set changed (now at " + shortRevision(e.Revision) + "); list it again"
}

// IsStale reports whether err is a *StaleError, and returns it.
func IsStale(err error) (*StaleError, bool) {
	var s *StaleError
	ok := errors.As(err, &s)
	return s, ok
}

func shortRevision(r *string) string {
	if r == nil {
		return "an empty set"
	}
	return (*r)[:min(8, len(*r))]
}

// Staged is a change an open transaction holds, as Begin reports it for
// resuming: a put of Bytes bytes, of which Received have arrived, or a
// delete. IfEtag nil means the document must not exist at the commit.
type Staged struct {
	Key
	Delete   bool    `json:"delete,omitempty"`
	IfEtag   *string `json:"ifEtag"`
	Bytes    int64   `json:"bytes"`
	Received int64   `json:"received"`
}

// Began is what a begin answered: the transaction, and what it holds.
type Began struct {
	Tx     string   `json:"tx"`
	Staged []Staged `json:"staged"`
}

// Ack answers a put or a delete: the document staged, and how many bytes of
// its body the server holds (0 for a delete).
type Ack struct {
	Key
	Received int64 `json:"received"`
}

// DocEtag is a document and its etag, nil when it does not exist.
type DocEtag struct {
	Key
	Etag *string `json:"etag"`
}

// CommitResult is what a commit answered. Committed: the set's new revision,
// and each document written, with its etag, or deleted, with a nil etag.
// Not Committed: a conflict, with the set's revision now and the current
// etag of each document that conflicts; the transaction stays open.
type CommitResult struct {
	Committed bool      `json:"committed"`
	Revision  *string   `json:"revision"`
	Docs      []DocEtag `json:"docs"`
}

// Head returns the revision of the app's set, nil for an empty set.
func (c *Client) Head(ctx context.Context) (*string, error) {
	var rev *string
	err := c.request(ctx, map[string]any{"t": "head"}, nil, nil, func(in incoming) (bool, error) {
		r, ok := readRevision(in.frame)
		if in.t != "head" || !ok {
			return false, protocolError("unexpected %s", in.t)
		}
		rev = r
		return true, nil
	})
	return rev, err
}

// List asks for one page of the app's set: the first when cursor is empty.
// pinned asks for the page at revision only (nil: the empty set); a next
// page must be pinned to the revision of the page that named its cursor. A
// set that is no longer at that revision is a *StaleError.
func (c *Client) List(ctx context.Context, pinned bool, revision *string, cursor string) (Page, error) {
	frame := map[string]any{"t": "list"}
	if pinned {
		frame["revision"] = revision
	}
	if cursor != "" {
		frame["cursor"] = cursor
	}
	var page Page
	var stale *StaleError
	err := c.request(ctx, frame, nil, nil, func(in incoming) (bool, error) {
		switch in.t {
		case "stale":
			r, ok := readRevision(in.frame)
			if !ok {
				return false, protocolError("a malformed stale")
			}
			stale = &StaleError{Revision: r}
			return true, nil
		case "list":
			p, err := readPage(in.frame)
			page = p
			return true, err
		}
		return false, protocolError("unexpected %s", in.t)
	})
	if err == nil && stale != nil {
		return Page{}, stale
	}
	return page, err
}

// ListAll reads the app's whole set, page by page, all of one revision; it
// starts over when the set changes between pages, a few times at most, and
// then returns the *StaleError.
func (c *Client) ListAll(ctx context.Context) (Listing, error) {
	var err error
	for range listAttempts {
		var l Listing
		if l, err = c.listOnce(ctx); err == nil {
			return l, nil
		}
		if _, stale := IsStale(err); !stale {
			return Listing{}, err
		}
	}
	return Listing{}, err
}

func (c *Client) listOnce(ctx context.Context) (Listing, error) {
	page, err := c.List(ctx, false, nil, "")
	if err != nil {
		return Listing{}, err
	}
	l := Listing{Revision: page.Revision, Docs: page.Docs, Removed: page.Removed}
	for page.Cursor != "" {
		if page, err = c.List(ctx, true, l.Revision, page.Cursor); err != nil {
			return Listing{}, err
		}
		l.Docs = append(l.Docs, page.Docs...)
		l.Removed = append(l.Removed, page.Removed...)
	}
	return l, nil
}

// Get reads the document k into w, from offset on: the document as it is
// when etag is nil, or only if its etag is *etag (an offset past 0 needs the
// etag of the body it resumes). The body goes to w piece by piece as it
// arrives, on the goroutine that calls Receive. A document that is not there
// (with that etag) is a *StaleError, and nothing is written. A w that fails
// gets nothing more, and Get returns its error once the rest of the body has
// arrived; the client stays usable.
func (c *Client) Get(ctx context.Context, k Key, etag *string, offset int64, w io.Writer) (Doc, error) {
	if offset < 0 || offset > 0 && etag == nil {
		return Doc{}, fmt.Errorf("wsclient: an offset of %d needs the etag of the body it resumes", offset)
	}
	frame := map[string]any{"t": "get", "path": k.Path, "id": k.ID, "type": k.Type}
	if etag != nil {
		frame["etag"] = *etag
	}
	if offset > 0 {
		frame["offset"] = offset
	}
	var doc Doc
	var stale *StaleError
	var werr error
	err := c.request(ctx, frame, nil, w, func(in incoming) (bool, error) {
		switch in.t {
		case "stale":
			r, ok := readRevision(in.frame)
			if !ok {
				return false, protocolError("a malformed stale")
			}
			stale = &StaleError{Revision: r}
			return true, nil
		case "doc":
			d, ok := readDoc(in.frame)
			if !ok || d.Key != k || d.Offset != offset || etag != nil && d.Etag != *etag {
				return false, protocolError("a doc that is not the one asked for")
			}
			doc, werr = d, in.werr
			return true, nil
		}
		return false, protocolError("unexpected %s", in.t)
	})
	switch {
	case err != nil:
		return Doc{}, err
	case stale != nil:
		return Doc{}, stale
	case werr != nil:
		return Doc{}, fmt.Errorf("write the body of %s: %w", k, werr)
	}
	return doc, nil
}

// Begin opens the transaction tx (16 to 64 of [A-Za-z0-9_-], chosen by the
// client), or returns to it, and reports what it holds. It discards the
// device's other open transaction on the app.
func (c *Client) Begin(ctx context.Context, tx string) (Began, error) {
	var b Began
	err := c.request(ctx, map[string]any{"t": "begin", "tx": tx}, nil, nil, func(in incoming) (bool, error) {
		if in.t != "began" || json.Unmarshal(in.frame["staged"], &b.Staged) != nil || b.Staged == nil {
			return false, protocolError("unexpected %s", in.t)
		}
		got, ok := readString(in.frame["tx"])
		if !ok || got != tx {
			return false, protocolError("a began of another transaction")
		}
		b.Tx = tx
		return true, nil
	})
	return b, err
}

// Put stages a body of size bytes for k in the transaction tx: the bytes
// past offset, read from body as the channel takes them (offset 0 starts the
// body afresh, a larger one resumes it, and offset == size only changes
// ifEtag). ifEtag is the etag k must have at the commit, nil if it must not
// exist. The server answers once the whole body has arrived, so a refusal
// (the quota, say) comes after it. A body that cannot be read to its end
// ends the client: the server would take the next frame for the rest of it.
func (c *Client) Put(ctx context.Context, tx string, k Key, ifEtag *string, size, offset int64, body io.Reader) (Ack, error) {
	if size < 0 || offset < 0 || offset > size {
		return Ack{}, fmt.Errorf("wsclient: a put of %d bytes from %d", size, offset)
	}
	frame := map[string]any{"t": "put", "tx": tx, "path": k.Path, "id": k.ID, "type": k.Type, "ifEtag": ifEtag, "bytes": size, "offset": offset}
	return c.staged(ctx, frame, k, &outBody{r: body, n: size - offset})
}

// Delete stages the removal of k in the transaction tx; ifEtag is the etag k
// must have at the commit.
func (c *Client) Delete(ctx context.Context, tx string, k Key, ifEtag *string) (Ack, error) {
	frame := map[string]any{"t": "delete", "tx": tx, "path": k.Path, "id": k.ID, "type": k.Type, "ifEtag": ifEtag}
	return c.staged(ctx, frame, k, nil)
}

func (c *Client) staged(ctx context.Context, frame map[string]any, k Key, body *outBody) (Ack, error) {
	var a Ack
	err := c.request(ctx, frame, body, nil, func(in incoming) (bool, error) {
		key, ok1 := readKey(in.frame)
		received, ok2 := readCount(in.frame["received"])
		if in.t != "staged" || !ok1 || !ok2 || key != k {
			return false, protocolError("unexpected %s", in.t)
		}
		a = Ack{Key: key, Received: received}
		return true, nil
	})
	return a, err
}

// Commit applies the transaction tx, all of it or nothing. A conflict is no
// error: the result is not Committed, and the transaction stays open. An
// error after the server's commit point may leave the commit applied in
// part: list again, and Begin the same tx to see whether it is still open.
func (c *Client) Commit(ctx context.Context, tx string) (CommitResult, error) {
	var r CommitResult
	err := c.request(ctx, map[string]any{"t": "commit", "tx": tx}, nil, nil, func(in incoming) (bool, error) {
		rev, ok := readRevision(in.frame)
		docs, ok2 := readDocEtags(in.frame["docs"])
		if in.t != "committed" && in.t != "conflict" || !ok || !ok2 {
			return false, protocolError("unexpected %s", in.t)
		}
		r = CommitResult{Committed: in.t == "committed", Revision: rev, Docs: docs}
		return true, nil
	})
	return r, err
}

// Abort discards the transaction tx.
func (c *Client) Abort(ctx context.Context, tx string) error {
	return c.request(ctx, map[string]any{"t": "abort", "tx": tx}, nil, nil, func(in incoming) (bool, error) {
		got, ok := readString(in.frame["tx"])
		if in.t != "aborted" || !ok || got != tx {
			return false, protocolError("unexpected %s", in.t)
		}
		return true, nil
	})
}

// Resolve settles a divergence of the app's servers: server is the
// subordinate's server id, keep KeepMain or KeepSubordinate. The listener
// set with OnResolved hears the answer too.
func (c *Client) Resolve(ctx context.Context, server, keep string) error {
	return c.request(ctx, map[string]any{"t": "resolve", "server": server, "keep": keep}, nil, nil, func(in incoming) (bool, error) {
		got, ok := readString(in.frame["server"])
		if in.t != "resolved" || !ok || got != server {
			return false, protocolError("unexpected %s", in.t)
		}
		return true, nil
	})
}

func readKey(frame map[string]json.RawMessage) (Key, bool) {
	path, ok1 := readString(frame["path"])
	id, ok2 := readString(frame["id"])
	typ, ok3 := readString(frame["type"])
	return Key{Path: path, ID: id, Type: typ}, ok1 && ok2 && ok3
}

func readDoc(frame map[string]json.RawMessage) (Doc, bool) {
	k, ok1 := readKey(frame)
	etag, ok2 := readString(frame["etag"])
	size, ok3 := readCount(frame["bytes"])
	offset, ok4 := int64(0), true
	if raw, present := frame["offset"]; present {
		offset, ok4 = readCount(raw)
	}
	return Doc{DocInfo: DocInfo{Key: k, Etag: etag, Bytes: size}, Offset: offset}, ok1 && ok2 && ok3 && ok4
}

// readPage reads a list frame. Its documents and tombstones must name their
// keys in full; a field this client does not know is left alone.
func readPage(frame map[string]json.RawMessage) (Page, error) {
	rev, ok := readRevision(frame)
	var docs []struct {
		Path, ID, Type, Etag *string
		Bytes                *int64
	}
	var removed []struct {
		Path, ID, Type, At *string
	}
	if !ok || json.Unmarshal(frame["docs"], &docs) != nil || docs == nil ||
		json.Unmarshal(frame["removed"], &removed) != nil || removed == nil {
		return Page{}, protocolError("a malformed list")
	}
	p := Page{Revision: rev, Docs: make([]DocInfo, 0, len(docs)), Removed: make([]Removed, 0, len(removed))}
	for _, d := range docs {
		if d.Path == nil || d.ID == nil || d.Type == nil || d.Etag == nil || d.Bytes == nil || *d.Bytes < 0 {
			return Page{}, protocolError("a malformed list")
		}
		p.Docs = append(p.Docs, DocInfo{Key: Key{*d.Path, *d.ID, *d.Type}, Etag: *d.Etag, Bytes: *d.Bytes})
	}
	for _, r := range removed {
		if r.Path == nil || r.ID == nil || r.Type == nil || r.At == nil {
			return Page{}, protocolError("a malformed list")
		}
		p.Removed = append(p.Removed, Removed{Key: Key{*r.Path, *r.ID, *r.Type}, At: *r.At})
	}
	if raw, present := frame["cursor"]; present {
		cursor, ok := readString(raw)
		if !ok {
			return Page{}, protocolError("a malformed list")
		}
		p.Cursor = cursor
	}
	return p, nil
}

func readDocEtags(raw json.RawMessage) ([]DocEtag, bool) {
	var list []struct {
		Path, ID, Type *string
		Etag           json.RawMessage
	}
	if raw == nil || json.Unmarshal(raw, &list) != nil || list == nil {
		return nil, false
	}
	out := make([]DocEtag, 0, len(list))
	for _, d := range list {
		etag, ok := readNullable(map[string]json.RawMessage{"etag": d.Etag}, "etag")
		if d.Path == nil || d.ID == nil || d.Type == nil || d.Etag == nil || !ok {
			return nil, false
		}
		out = append(out, DocEtag{Key: Key{*d.Path, *d.ID, *d.Type}, Etag: etag})
	}
	return out, true
}
