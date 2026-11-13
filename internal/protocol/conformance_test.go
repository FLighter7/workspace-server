package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/conformance"
)

// The shared vectors in testdata/conformance, played against a real Session.
// The client package's tests play the same files against its client, so a
// change on one side that the other does not follow fails a test.

func TestConformanceConstants(t *testing.T) {
	var c struct {
		Proto      int    `json:"proto"`
		ChunkBytes int    `json:"chunkBytes"`
		High       uint64 `json:"bufferHighBytes"`
		Low        uint64 `json:"bufferLowBytes"`
		ListPage   int    `json:"listPage"`
	}
	conformance.Read(t, "constants.json", &c)
	if c.Proto != Version {
		t.Errorf("Version = %d, the vectors say %d", Version, c.Proto)
	}
	if c.ChunkBytes != ChunkBytes {
		t.Errorf("ChunkBytes = %d, the vectors say %d", ChunkBytes, c.ChunkBytes)
	}
	if c.ListPage != ListPage {
		t.Errorf("ListPage = %d, the vectors say %d", ListPage, c.ListPage)
	}
	if c.High != maxBuffered || c.Low != lowWater {
		t.Errorf("flow control = %d/%d, the vectors say %d/%d", maxBuffered, lowWater, c.High, c.Low)
	}
}

func TestConformanceErrorCodes(t *testing.T) {
	var v struct {
		Server []string `json:"server"`
	}
	conformance.Read(t, "error-codes.json", &v)
	ours := []string{
		CodeProtocol, CodeUnknownApp, CodeBadSecret, CodeBadToken, CodeBlocked,
		CodeBadPath, CodeQuota, CodeNoTransaction, CodeIncomplete,
		CodeNotAuthenticated, CodeInternal, CodeNotMain,
	}
	slices.Sort(ours)
	theirs := slices.Clone(v.Server)
	slices.Sort(theirs)
	if !slices.Equal(ours, theirs) {
		t.Errorf("error codes = %v, the vectors say %v", ours, theirs)
	}
}

func TestConformanceChunks(t *testing.T) {
	var v struct {
		Cases []struct {
			Bytes  int   `json:"bytes"`
			Offset int   `json:"offset"`
			Chunks []int `json:"chunks"`
		} `json:"cases"`
	}
	conformance.Read(t, "chunks.json", &v)
	for _, c := range v.Cases {
		t.Run(fmt.Sprintf("%d from %d", c.Bytes, c.Offset), func(t *testing.T) {
			b := newFakeBackend()
			doc := body(c.Bytes)
			b.setDoc(docKey, "e1", doc)
			s, fc := newSession(t, b, Options{})
			authenticate(t, s, fc)
			pre := len(barrier(t, s, fc))

			frame := getFrame
			if c.Offset > 0 {
				frame = fmt.Sprintf(`{"t":"get","path":"notes","id":"n1","type":"json","etag":"e1","offset":%d}`, c.Offset)
			}
			s.HandleText(frame)
			got := barrier(t, s, fc)[pre:]

			want := []any{docObj("e1", c.Bytes, c.Offset)}
			for _, n := range c.Chunks {
				want = append(want, n)
			}
			expectMessages(t, got, want...)
			if sent := bytes.Join(fc.binaries(), nil); !bytes.Equal(sent, doc[c.Offset:]) {
				t.Error("the chunks are not the document's bytes past the offset")
			}
		})
	}
}

// call is the client call an exchange makes, as the vectors give it.
type call struct {
	Method   string          `json:"method"`
	Kind     string          `json:"kind"`
	App      string          `json:"app"`
	Device   string          `json:"device"`
	Token    string          `json:"token"`
	Secret   string          `json:"secret"`
	Name     string          `json:"name"`
	Server   json.RawMessage `json:"server"` // connection data, or a server id in resolve
	Term     *int            `json:"term"`
	Devices  *[]DeviceEntry  `json:"devices"`
	Removed  []Tombstone     `json:"removed"`
	Path     string          `json:"path"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Etag     *string         `json:"etag"`
	IfEtag   *string         `json:"ifEtag"`
	Offset   int64           `json:"offset"`
	Body     *string         `json:"body"`
	Tx       string          `json:"tx"`
	Keep     string          `json:"keep"`
	Revision *string         `json:"revision"`
	Cursor   *string         `json:"cursor"`
}

func (c *call) key() Key { return Key{Path: c.Path, ID: c.ID, Type: c.Type} }

// serverInfo is the call's server as connection data.
func (c *call) serverInfo(t *testing.T) *ServerInfo {
	t.Helper()
	var si *ServerInfo
	if len(c.Server) > 0 {
		if err := json.Unmarshal(c.Server, &si); err != nil {
			t.Fatalf("call server %s: %v", c.Server, err)
		}
	}
	return si
}

// exchange is one entry of frames.json; see its "about".
type exchange struct {
	Name         string                     `json:"name"`
	Session      string                     `json:"session"`
	Backend      map[string]json.RawMessage `json:"backend"`
	PageSize     int                        `json:"pageSize"`
	Call         *call                      `json:"call"`
	Client       obj                        `json:"client"`
	ClientBody   *string                    `json:"clientBody"`
	Push         json.RawMessage            `json:"push"` // a revision, which may be null
	PushDiverged []DivergedServer           `json:"pushDiverged"`
	PushResolved *string                    `json:"pushResolved"`
	PushApps     []AppInfo                  `json:"pushApps"`
	PushDevices  []DeviceEntry              `json:"pushDevices"`
	PushRemoved  []Tombstone                `json:"pushRemoved"`
	PushTakeOver bool                       `json:"pushTakeOver"`
	Server       []obj                      `json:"server"`
	ServerBody   *string                    `json:"serverBody"`
	Closes       bool                       `json:"closes"`
	Error        string                     `json:"error"`
}

type frameVectors struct {
	Exchanges  []exchange `json:"exchanges"`
	ServerKind []exchange `json:"serverKind"`
	Tolerated  struct {
		Client []struct {
			Name   string  `json:"name"`
			Frame  obj     `json:"frame"`
			Body   *string `json:"body"`
			IfEtag *string `json:"ifEtag"`
		} `json:"client"`
	} `json:"tolerated"`
}

// configure puts the backend in the state an exchange describes.
func configure(t *testing.T, b *fakeBackend, state map[string]json.RawMessage) {
	t.Helper()
	decode := func(raw json.RawMessage, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("decode backend state %s: %v", raw, err)
		}
	}
	refusal := func(raw json.RawMessage) *Error {
		var code string
		decode(raw, &code)
		return &Error{Code: code, Message: "refused by the conformance test"}
	}
	txResult := func(raw json.RawMessage) (*string, []DocEtag) {
		var r struct {
			Revision *string   `json:"revision"`
			Docs     []DocEtag `json:"docs"`
		}
		decode(raw, &r)
		return r.Revision, r.Docs
	}

	for key, raw := range state {
		switch key {
		case "welcome":
			var w struct {
				ServerID     string           `json:"serverId"`
				Name         string           `json:"name"`
				Revision     *string          `json:"revision"`
				Hosts        []string         `json:"hosts"`
				Role         string           `json:"role"`
				Term         int              `json:"term"`
				Subordinates []ServerInfo     `json:"subordinates"`
				Diverged     []DivergedServer `json:"diverged"`
			}
			decode(raw, &w)
			b.welcome = Welcome{
				ServerID: w.ServerID, Name: w.Name, Revision: w.Revision, Hosts: w.Hosts, Role: w.Role,
				Term: w.Term, Subordinates: w.Subordinates, Diverged: w.Diverged,
			}
		case "helloError":
			b.helloErr = refusal(raw)
		case "paired":
			var p struct {
				Device string `json:"device"`
				Token  string `json:"token"`
			}
			decode(raw, &p)
			b.pairDevice, b.pairToken = p.Device, p.Token
		case "pairError":
			b.pairErr = refusal(raw)
		case "head":
			decode(raw, &b.head)
		case "list":
			var l struct {
				Revision *string   `json:"revision"`
				Docs     []DocInfo `json:"docs"`
				Removed  []Removed `json:"removed"`
			}
			decode(raw, &l)
			b.listing = Listing{Revision: l.Revision, Docs: l.Docs, Removed: l.Removed}
		case "listError":
			b.listErr = refusal(raw)
		case "doc":
			var d struct {
				Key
				Etag string `json:"etag"`
				Body string `json:"body"`
			}
			decode(raw, &d)
			b.setDoc(d.Key, d.Etag, []byte(d.Body))
		case "stale":
			decode(raw, &b.revision)
		case "getError":
			b.getErr = refusal(raw)
		case "began":
			decode(raw, &b.began)
		case "beginError":
			b.beginErr = refusal(raw)
		case "putError":
			b.putErr = refusal(raw)
		case "deleteError":
			b.deleteErr = refusal(raw)
		case "committed":
			b.committed.Revision, b.committed.Docs = txResult(raw)
		case "conflict":
			rev, docs := txResult(raw)
			b.conflict = &Conflict{Revision: rev, Docs: docs}
		case "commitError":
			b.commitErr = refusal(raw)
		case "abortError":
			b.abortErr = refusal(raw)
		case "resolveError":
			b.resolveErr = refusal(raw)
		case "apps":
			decode(raw, &b.apps)
		case "appsError":
			b.appsErr = refusal(raw)
		case "devices":
			decode(raw, &b.devices)
		case "removed":
			decode(raw, &b.removed)
		case "term":
			decode(raw, &b.termAnswer)
		case "termError":
			b.termErr = refusal(raw)
		case "announceError":
			b.announceErr = refusal(raw)
		case "invite":
			var i struct {
				Code      string    `json:"code"`
				ExpiresAt time.Time `json:"expiresAt"`
			}
			decode(raw, &i)
			b.inviteCode, b.inviteExpires = i.Code, i.ExpiresAt
		case "inviteError":
			b.inviteErr = refusal(raw)
		default:
			t.Fatalf("unknown backend state %q", key)
		}
	}
}

func withoutBarriers(msgs []message) []message {
	var out []message
	for _, m := range msgs {
		if !isBarrier(m) {
			out = append(out, m)
		}
	}
	return out
}

// frameMatches compares a sent frame with a vector. An error frame's message
// is the server's own wording: it must be there, but its text is not compared.
func frameMatches(got, want obj) bool {
	if want["t"] != "error" {
		return reflect.DeepEqual(got, want)
	}
	msg, ok := got["message"].(string)
	if !ok || msg == "" {
		return false
	}
	rest := obj{}
	for k, v := range got {
		if k != "message" {
			rest[k] = v
		}
	}
	return reflect.DeepEqual(rest, want)
}

// announcesBody reports the size of the body that follows frame f: a doc's
// bytes past its offset.
func announcesBody(f obj) bool {
	if f["t"] != "doc" {
		return false
	}
	n, _ := f["bytes"].(float64)
	off, _ := f["offset"].(float64)
	return n > off
}

// expectServerSent checks the session's messages against the vector's
// server frames, a body following the frame that announces one.
func expectServerSent(t *testing.T, got []message, ex exchange) {
	t.Helper()
	var body []byte
	if ex.ServerBody != nil {
		body = []byte(*ex.ServerBody)
	}

	i := 0
	for _, want := range ex.Server {
		if i >= len(got) || got[i].text == nil || !frameMatches(got[i].text, want) {
			t.Fatalf("sent %s\nwant frame %d to be %v", describe(got), i, want)
		}
		i++
		if announcesBody(want) {
			var received []byte
			for i < len(got) && got[i].text == nil {
				received = append(received, got[i].binary...)
				i++
			}
			if !bytes.Equal(received, body) {
				t.Fatalf("body = %q, want %q", received, body)
			}
		}
	}
	if i != len(got) {
		t.Fatalf("sent %s\nwant only %d frames", describe(got), len(ex.Server))
	}
}

// linkHello authenticates a link session: the hello of a server-kind
// device, with no app.
const linkHello = `{"t":"hello","proto":1,"device":"s1","token":"x"}`

func TestConformanceExchanges(t *testing.T) {
	var v frameVectors
	conformance.Read(t, "frames.json", &v)
	if len(v.Exchanges) == 0 || len(v.ServerKind) == 0 {
		t.Fatal("no exchanges in the vectors")
	}

	for _, ex := range append(v.Exchanges, v.ServerKind...) {
		t.Run(ex.Name, func(t *testing.T) {
			if ex.PageSize > 0 {
				listPage = ex.PageSize
				t.Cleanup(func() { listPage = ListPage })
			}
			b := newFakeBackend()
			configure(t, b, ex.Backend)
			s, fc := newSession(t, b, Options{})

			pre := 0
			switch ex.Session {
			case "authenticated":
				authenticate(t, s, fc)
				pre = len(barrier(t, s, fc))
			case "link":
				s.HandleText(linkHello)
				if !s.Authenticated() || s.App() != "" {
					t.Fatal("the link session did not authenticate")
				}
				pre = len(barrier(t, s, fc))
			case "new":
			default:
				t.Fatalf("unknown session state %q", ex.Session)
			}

			switch {
			case len(ex.Push) > 0:
				var rev *string
				if err := json.Unmarshal(ex.Push, &rev); err != nil {
					t.Fatal(err)
				}
				s.PushChanged(rev)
			case ex.PushDiverged != nil:
				s.PushDiverged(ex.PushDiverged)
			case ex.PushResolved != nil:
				s.PushResolved(*ex.PushResolved)
			case ex.PushApps != nil:
				s.PushApps(ex.PushApps)
			case ex.PushDevices != nil:
				s.PushDevices(Registry{Devices: ex.PushDevices, Removed: ex.PushRemoved})
			case ex.PushTakeOver:
				s.PushTakeOver()
			default:
				frame, err := json.Marshal(ex.Client)
				if err != nil {
					t.Fatal(err)
				}
				s.HandleText(string(frame))
				if ex.ClientBody != nil && *ex.ClientBody != "" {
					s.HandleBinary([]byte(*ex.ClientBody))
				}
			}

			var got []message
			switch {
			case ex.Closes:
				waitFor(t, "the channel to close", fc.isClosed)
				got = withoutBarriers(fc.messages())[pre:]
			case s.Authenticated():
				got = barrier(t, s, fc)[pre:]
				if fc.isClosed() {
					t.Fatal("the session closed the channel")
				}
			default:
				fc.waitTexts(t, len(ex.Server))
				got = fc.messages()
				if fc.isClosed() {
					t.Fatal("the session closed the channel")
				}
			}
			expectServerSent(t, got, ex)

			if ex.Call != nil {
				checkCall(t, b, ex)
			}
		})
	}
}

// checkCall checks that the backend was asked what the exchange's call asks.
func checkCall(t *testing.T, b *fakeBackend, ex exchange) {
	t.Helper()
	c := ex.Call
	b.mu.Lock()
	defer b.mu.Unlock()
	switch c.Method {
	case "hello":
		if got, want := b.hellos[len(b.hellos)-1], [3]string{c.App, c.Device, c.Token}; got != want {
			t.Errorf("backend Hello got %v, want %v", got, want)
		}
	case "pair":
		kind := c.Kind
		if kind == "" {
			kind = KindDevice
		}
		if kind != KindDevice && kind != KindServer {
			if len(b.pairs) != 0 {
				t.Errorf("backend Pair got %v for an unknown kind", b.pairs)
			}
			return
		}
		if got, want := b.pairs[len(b.pairs)-1], [4]string{kind, c.App, c.Secret, c.Name}; got != want {
			t.Errorf("backend Pair got %v, want %v", got, want)
		}
	case "apps", "invite", "head", "list":
		if ex.Error != "" {
			return // the backend refused, or the session did before asking it
		}
		n := 0
		for _, call := range b.calls {
			if call == c.Method {
				n++
			}
		}
		if n != 1 {
			t.Errorf("backend %s called %d times, want 1", c.Method, n)
		}
	case "announce":
		if ex.Error != "" {
			return
		}
		si := c.serverInfo(t)
		if len(b.announces) != 1 || !reflect.DeepEqual(b.announces[0], *si) {
			t.Errorf("backend Announce got %+v, want %+v", b.announces, *si)
		}
		handover := b.handovers[0]
		switch {
		case c.Devices == nil && handover != nil:
			t.Errorf("backend Announce got a handover %+v, want none", *handover)
		case c.Devices != nil && (handover == nil || !reflect.DeepEqual(handover.Devices, *c.Devices) || !reflect.DeepEqual(handover.Removed, c.Removed)):
			t.Errorf("backend Announce got handover %+v, want devices %+v removed %+v", handover, *c.Devices, c.Removed)
		}
	case "term":
		si := c.serverInfo(t)
		if ex.Session != "link" || si == nil {
			if len(b.terms) != 0 {
				t.Errorf("backend Term got %+v for a refused frame", b.terms)
			}
			return
		}
		want := termCall{*c.Term, *si, c.Device, c.Token}
		if len(b.terms) != 1 || !reflect.DeepEqual(b.terms[0], want) {
			t.Errorf("backend Term got %+v, want %+v", b.terms, want)
		}
	case "get":
		if ex.Session == "link" {
			if len(b.gets) != 0 {
				t.Errorf("backend Get got %+v on a link session", b.gets)
			}
			return
		}
		if len(b.gets) != 1 || b.gets[0].key != c.key() || !reflect.DeepEqual(b.gets[0].etag, c.Etag) {
			t.Errorf("backend Get got %+v, want %v at %v", b.gets, c.key(), c.Etag)
		}
	case "begin", "commit", "abort":
		calls := map[string][][3]string{"begin": b.begins, "commit": b.commits, "abort": b.aborts}[c.Method]
		if ex.Session == "link" {
			if len(calls) != 0 {
				t.Errorf("backend %s got %v on a link session", c.Method, calls)
			}
			return
		}
		if want := [3]string{"todo", "d1", c.Tx}; len(calls) != 1 || calls[0] != want {
			t.Errorf("backend %s got %v, want %v", c.Method, calls, want)
		}
	case "put":
		if len(b.puts) != 1 {
			t.Fatalf("backend Put called %d times, want 1", len(b.puts))
		}
		p := b.puts[0]
		size := int64(len(*c.Body))
		if p.tx != c.Tx || p.key != c.key() || !reflect.DeepEqual(p.ifEtag, c.IfEtag) || p.size != size || p.offset != c.Offset {
			t.Errorf("backend Put got %+v, want tx %q key %v ifEtag %v bytes %d offset %d", p, c.Tx, c.key(), c.IfEtag, size, c.Offset)
		}
		if p.sink != nil {
			data, _, closes := p.sink.state()
			if string(data) != (*c.Body)[c.Offset:] || closes == 0 {
				t.Errorf("the sink got %q (closed %d times), want %q, closed", data, closes, (*c.Body)[c.Offset:])
			}
		}
	case "delete":
		want := deleteCall{"todo", "d1", c.Tx, c.key(), c.IfEtag}
		if len(b.deletes) != 1 || !reflect.DeepEqual(b.deletes[0], want) {
			t.Errorf("backend Delete got %+v, want %+v", b.deletes, want)
		}
	case "resolve":
		var server string
		_ = json.Unmarshal(c.Server, &server)
		if want := [4]string{"todo", "d1", server, c.Keep}; len(b.resolves) != 1 || b.resolves[0] != want {
			t.Errorf("backend Resolve got %v, want %v", b.resolves, want)
		}
	case "forget":
		if !slices.Contains(b.forgets, [2]string{"todo", "d1"}) {
			t.Errorf("backend Forget got %v, want todo/d1", b.forgets)
		}
	default:
		t.Fatalf("unknown call %q", c.Method)
	}
}

func TestConformanceToleratedClientFrames(t *testing.T) {
	var v frameVectors
	conformance.Read(t, "frames.json", &v)

	for _, tc := range v.Tolerated.Client {
		t.Run(tc.Name, func(t *testing.T) {
			b := newFakeBackend()
			b.setDoc(Key{ID: "data", Type: "json"}, "e1", []byte("{}"))
			s, fc := newSession(t, b, Options{})
			if tc.Frame["t"] != "hello" && tc.Frame["t"] != "pair" {
				authenticate(t, s, fc)
			}

			frame, err := json.Marshal(tc.Frame)
			if err != nil {
				t.Fatal(err)
			}
			s.HandleText(string(frame))
			if tc.Body != nil && *tc.Body != "" {
				s.HandleBinary([]byte(*tc.Body))
			}
			barrier(t, s, fc)

			if fc.isClosed() {
				t.Fatal("the session closed the channel")
			}
			for _, f := range fc.texts() {
				if f["t"] == "error" || f["t"] == "stale" {
					t.Fatalf("the session refused it: %v", f)
				}
			}
			if tc.Frame["t"] == "put" {
				puts := b.putCalls()
				if len(puts) != 1 || !reflect.DeepEqual(puts[0].ifEtag, tc.IfEtag) || puts[0].offset != 0 {
					t.Errorf("backend Put calls %+v, want one with ifEtag %v from 0", puts, tc.IfEtag)
				}
			}
		})
	}
}
