package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

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
	}
	conformance.Read(t, "constants.json", &c)
	if c.Proto != Version {
		t.Errorf("Version = %d, the vectors say %d", Version, c.Proto)
	}
	if c.ChunkBytes != ChunkBytes {
		t.Errorf("ChunkBytes = %d, the vectors say %d", ChunkBytes, c.ChunkBytes)
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
		CodeTooLarge, CodeNotJSON, CodeNotAuthenticated, CodeInternal,
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
			Chunks []int `json:"chunks"`
		} `json:"cases"`
	}
	conformance.Read(t, "chunks.json", &v)
	for _, c := range v.Cases {
		b := newFakeBackend()
		b.doc = make([]byte, c.Bytes)
		s, fc := newSession(t, b, Options{})
		authenticate(t, s, fc)
		pre := len(barrier(t, s, fc))

		s.HandleText(`{"t":"get"}`)
		got := barrier(t, s, fc)[pre:]

		want := []any{obj{"t": "doc", "etag": nil, "bytes": float64(c.Bytes)}}
		for _, n := range c.Chunks {
			want = append(want, n)
		}
		expectMessages(t, got, want...)
	}
}

// exchange is one entry of frames.json; see its "about".
type exchange struct {
	Name    string                     `json:"name"`
	Session string                     `json:"session"`
	Backend map[string]json.RawMessage `json:"backend"`
	Call    *struct {
		Method string  `json:"method"`
		App    string  `json:"app"`
		Device string  `json:"device"`
		Token  string  `json:"token"`
		Secret string  `json:"secret"`
		Name   string  `json:"name"`
		IfEtag *string `json:"ifEtag"`
		Body   string  `json:"body"`
	} `json:"call"`
	Client     obj     `json:"client"`
	ClientBody *string `json:"clientBody"`
	Push       *string `json:"push"`
	Server     []obj   `json:"server"`
	ServerBody *string `json:"serverBody"`
	Closes     bool    `json:"closes"`
}

type frameVectors struct {
	Exchanges []exchange `json:"exchanges"`
	Tolerated struct {
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

	for key, raw := range state {
		switch key {
		case "welcome":
			var w struct {
				ServerID string   `json:"serverId"`
				Name     string   `json:"name"`
				Etag     *string  `json:"etag"`
				Hosts    []string `json:"hosts"`
			}
			decode(raw, &w)
			b.welcome = Welcome{ServerID: w.ServerID, Name: w.Name, Etag: w.Etag, Hosts: w.Hosts}
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
			decode(raw, &b.headEtag)
		case "doc":
			var d struct {
				Etag *string `json:"etag"`
				Body string  `json:"body"`
			}
			decode(raw, &d)
			b.doc, b.docEtag = []byte(d.Body), d.Etag
		case "put":
			var p map[string]*string
			decode(raw, &p)
			if etag, ok := p["stored"]; ok {
				b.putEtag = *etag
			}
			if current, ok := p["conflict"]; ok {
				b.putConflict, b.putCurrent = true, current
			}
		case "putError":
			b.putErr = refusal(raw)
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
		if n, _ := want["bytes"].(float64); n > 0 {
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

func TestConformanceExchanges(t *testing.T) {
	var v frameVectors
	conformance.Read(t, "frames.json", &v)
	if len(v.Exchanges) == 0 {
		t.Fatal("no exchanges in the vectors")
	}

	for _, ex := range v.Exchanges {
		t.Run(ex.Name, func(t *testing.T) {
			b := newFakeBackend()
			configure(t, b, ex.Backend)
			s, fc := newSession(t, b, Options{})

			pre := 0
			switch ex.Session {
			case "authenticated":
				authenticate(t, s, fc)
				pre = len(barrier(t, s, fc))
			case "new":
			default:
				t.Fatalf("unknown session state %q", ex.Session)
			}

			if ex.Push != nil {
				s.PushChanged(*ex.Push)
			} else {
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

			if ex.Call == nil {
				return
			}
			c := ex.Call
			switch c.Method {
			case "hello":
				if got, want := b.hellos[len(b.hellos)-1], [3]string{c.App, c.Device, c.Token}; got != want {
					t.Errorf("backend Hello got %v, want %v", got, want)
				}
			case "pair":
				if got, want := b.pairs[len(b.pairs)-1], [3]string{c.App, c.Secret, c.Name}; got != want {
					t.Errorf("backend Pair got %v, want %v", got, want)
				}
			case "put":
				puts := b.putCalls()
				if len(puts) != 1 {
					t.Fatalf("backend Put called %d times, want 1", len(puts))
				}
				if !reflect.DeepEqual(puts[0].ifEtag, c.IfEtag) || string(puts[0].body) != c.Body {
					t.Errorf("backend Put got ifEtag %v body %q, want %v %q", puts[0].ifEtag, puts[0].body, c.IfEtag, c.Body)
				}
			case "forget":
				if !slices.Contains(b.forgets, [2]string{"todo", "d1"}) {
					t.Errorf("backend Forget got %v, want todo/d1", b.forgets)
				}
			}
		})
	}
}

func TestConformanceToleratedClientFrames(t *testing.T) {
	var v frameVectors
	conformance.Read(t, "frames.json", &v)

	for _, tc := range v.Tolerated.Client {
		t.Run(tc.Name, func(t *testing.T) {
			b := newFakeBackend()
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
				if f["t"] == "error" {
					t.Fatalf("the session refused it: %v", f)
				}
			}
			if tc.Frame["t"] == "put" {
				puts := b.putCalls()
				if len(puts) != 1 || !reflect.DeepEqual(puts[0].ifEtag, tc.IfEtag) {
					t.Errorf("backend Put calls %+v, want one with ifEtag %v", puts, tc.IfEtag)
				}
			}
		})
	}
}
