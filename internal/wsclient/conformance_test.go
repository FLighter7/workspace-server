package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/conformance"
)

// The shared vectors in testdata/conformance, played against this client,
// as the client package's conformance.test.ts plays them against its own.
// The server's tests play the same files against its Session.

// fakeChannel records what the client sends; its buffered amount grows with
// every message until drain empties it.
type fakeChannel struct {
	mu       sync.Mutex
	texts    []string
	binaries [][]byte
	buffered uint64
	onLow    func()
	closed   bool
}

func (f *fakeChannel) SendText(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("closed")
	}
	f.texts = append(f.texts, s)
	f.buffered += uint64(len(s))
	return nil
}

func (f *fakeChannel) Send(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("closed")
	}
	f.binaries = append(f.binaries, append([]byte(nil), data...))
	f.buffered += uint64(len(data))
	return nil
}

func (f *fakeChannel) BufferedAmount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buffered
}

func (f *fakeChannel) SetBufferedAmountLowThreshold(uint64) {}

func (f *fakeChannel) OnBufferedAmountLow(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onLow = fn
}

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeChannel) drain() {
	f.mu.Lock()
	f.buffered = 0
	fn := f.onLow
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (f *fakeChannel) sent() (frames []map[string]any, body []byte, pieces []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.texts {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			panic(err)
		}
		frames = append(frames, m)
	}
	for _, b := range f.binaries {
		body = append(body, b...)
		pieces = append(pieces, len(b))
	}
	return frames, body, pieces
}

func (f *fakeChannel) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// waitFor polls cond for two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// normal is v as JSON decodes it, so that Go values compare with vectors.
func normal(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConformanceConstants(t *testing.T) {
	var c struct {
		Proto  int    `json:"proto"`
		Prefix string `json:"pairingCodePrefix"`
		Chunk  int    `json:"chunkBytes"`
		High   int    `json:"bufferHighBytes"`
		Low    int    `json:"bufferLowBytes"`
	}
	conformance.Read(t, "constants.json", &c)
	if c.Proto != Version || c.Prefix != fmt.Sprintf("wsrv%d.", CodeVersion) || c.Chunk != ChunkBytes ||
		c.High != BufferHigh || c.Low != BufferLow {
		t.Errorf("constants %+v differ from the client's", c)
	}
	var codes struct {
		Server []string `json:"server"`
	}
	conformance.Read(t, "error-codes.json", &codes)
	ours, theirs := slices.Clone(ServerCodes), slices.Clone(codes.Server)
	slices.Sort(ours)
	slices.Sort(theirs)
	if !slices.Equal(ours, theirs) {
		t.Errorf("server codes %v, the vectors say %v", ours, theirs)
	}
}

func TestConformanceAnswers(t *testing.T) {
	var v struct {
		Cases []struct {
			Name   string   `json:"name"`
			Offer  []string `json:"offer"`
			Target Target   `json:"target"`
			Ufrag  string   `json:"ufrag"`
			Now    int64    `json:"now"`
			Answer []string `json:"answer"`
		} `json:"cases"`
	}
	conformance.Read(t, "answers.json", &v)
	if len(v.Cases) == 0 {
		t.Fatal("no answers in the vectors")
	}
	sdp := func(lines []string) string { return strings.Join(lines, "\r\n") + "\r\n" }
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if got, want := BuildAnswer(sdp(c.Offer), c.Target, c.Ufrag, c.Now), sdp(c.Answer); got != want {
				t.Errorf("answer =\n%s\nwant\n%s", got, want)
			}
		})
	}
	if a, b := RandomUfrag(), RandomUfrag(); a == b || len(a) != 8 {
		t.Errorf("two ufrags %q and %q; want 8 characters, new every time", a, b)
	}
}

func TestConformancePairingCodes(t *testing.T) {
	var v struct {
		Valid []struct {
			Name   string         `json:"name"`
			Code   string         `json:"code"`
			Parsed map[string]any `json:"parsed"`
		} `json:"valid"`
		Invalid []struct {
			Name  string `json:"name"`
			Code  string `json:"code"`
			Error string `json:"error"`
		} `json:"invalid"`
		Tolerated []struct {
			Name  string `json:"name"`
			Input string `json:"input"`
			Same  string `json:"same"`
		} `json:"tolerated"`
	}
	conformance.Read(t, "pairing-codes.json", &v)
	parsed := func(t *testing.T, text string) any {
		t.Helper()
		c, err := ParseCode(text)
		if err != nil {
			t.Fatalf("ParseCode: %v", err)
		}
		return normal(t, map[string]any{
			"serverId": c.ServerID, "name": c.Name, "secret": c.Secret, "password": c.Password,
			"fingerprint": c.Fingerprint, "port": c.Port, "hosts": c.Hosts,
		})
	}
	byName := map[string]any{}
	for _, c := range v.Valid {
		t.Run(c.Name, func(t *testing.T) {
			got := parsed(t, c.Code)
			if want := normal(t, c.Parsed); !reflect.DeepEqual(got, want) {
				t.Errorf("parsed %v, want %v", got, want)
			}
			byName[c.Name] = got
		})
	}
	for _, c := range v.Invalid {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := ParseCode(c.Code); !HasCode(err, c.Error) {
				t.Errorf("ParseCode = %v, want code %s", err, c.Error)
			}
		})
	}
	for _, c := range v.Tolerated {
		t.Run(c.Name, func(t *testing.T) {
			if got := parsed(t, c.Input); !reflect.DeepEqual(got, byName[c.Same]) {
				t.Errorf("parsed %v, want the same as %q", got, c.Same)
			}
		})
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
		t.Run(fmt.Sprint(c.Bytes), func(t *testing.T) {
			fc := &fakeChannel{}
			cl := NewClient(fc, "todo")
			done := make(chan error, 1)
			go func() {
				_, _, err := cl.Put(context.Background(), nil, make([]byte, c.Bytes))
				done <- err
			}()
			waitFor(t, "the body", func() bool {
				frames, body, _ := fc.sent()
				return len(frames) == 1 && len(body) == c.Bytes
			})
			frames, _, pieces := fc.sent()
			if want := map[string]any{"t": "put", "ifEtag": nil, "bytes": float64(c.Bytes)}; !reflect.DeepEqual(frames, []map[string]any{want}) {
				t.Errorf("frames %v, want %v", frames, want)
			}
			if !slices.Equal(pieces, c.Chunks) && !(len(pieces) == 0 && len(c.Chunks) == 0) {
				t.Errorf("pieces %v, want %v", pieces, c.Chunks)
			}
			cl.Receive(true, []byte(`{"t":"stored","etag":"e1"}`))
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
}

// call is the client call an exchange makes.
type call struct {
	Method string  `json:"method"`
	Kind   string  `json:"kind"`
	App    string  `json:"app"`
	Device string  `json:"device"`
	Token  string  `json:"token"`
	Secret string  `json:"secret"`
	Name   string  `json:"name"`
	IfEtag *string `json:"ifEtag"`
	Body   string  `json:"body"`
	Server *Server `json:"server"`
}

type exchange struct {
	Name        string           `json:"name"`
	Call        *call            `json:"call"`
	Client      map[string]any   `json:"client"`
	ClientBody  string           `json:"clientBody"`
	Push        *string          `json:"push"`
	PushApps    []App            `json:"pushApps"`
	PushDevices []Device         `json:"pushDevices"`
	Server      []map[string]any `json:"server"`
	ServerBody  string           `json:"serverBody"`
	Closes      bool             `json:"closes"`
	Result      json.RawMessage  `json:"result"`
	Error       string           `json:"error"`
}

// start makes the call on cl and returns its result as the vectors write
// it.
func start(cl *Client, c *call) (any, error) {
	ctx := context.Background()
	switch c.Method {
	case "hello":
		w, err := cl.Hello(ctx, c.Device, c.Token)
		return welcomeResult(w), err
	case "pair":
		p, w, err := cl.Pair(ctx, c.Kind, c.Secret, c.Name)
		return map[string]any{"device": p.Device, "token": p.Token, "welcome": welcomeResult(w)}, err
	case "head":
		return cl.Head(ctx)
	case "get":
		etag, body, err := cl.Get(ctx)
		return map[string]any{"etag": etag, "body": string(body)}, err
	case "put":
		stored, etag, err := cl.Put(ctx, c.IfEtag, []byte(c.Body))
		return map[string]any{"stored": stored, "etag": etag}, err
	case "forget":
		return nil, cl.Forget(ctx)
	case "apps":
		return cl.Apps(ctx)
	case "announce":
		return cl.Announce(ctx, *c.Server)
	case "invite":
		inv, err := cl.Invite(ctx)
		return map[string]any{"code": inv.Code, "expiresAt": inv.ExpiresAt.UTC().Format(time.RFC3339)}, err
	}
	panic("unknown method " + c.Method)
}

// welcomeResult is w as the vectors write a welcome: role and subordinates
// only when the frame carried them, an empty list included.
func welcomeResult(w Welcome) map[string]any {
	r := map[string]any{"serverId": w.ServerID, "name": w.Name, "etag": w.Etag, "hosts": w.Hosts}
	if w.Role != "" {
		r["role"] = w.Role
	}
	if w.Subordinates != nil {
		r["subordinates"] = w.Subordinates
	}
	return r
}

// play sends the server's frames into the client, a body after the frame
// that announces one, in pieces of at most ChunkBytes.
func play(cl *Client, frames []map[string]any, body string) {
	for _, f := range frames {
		if f["t"] == "error" && f["message"] == nil {
			f["message"] = "x" // the real server always explains an error
		}
		b, _ := json.Marshal(f)
		cl.Receive(true, b)
		if n, _ := f["bytes"].(float64); n > 0 {
			for off := 0; off < len(body); off += ChunkBytes {
				cl.Receive(false, []byte(body[off:min(off+ChunkBytes, len(body))]))
			}
		}
	}
}

func playExchange(t *testing.T, ex exchange) {
	fc := &fakeChannel{}
	app := ""
	if ex.Call != nil {
		app = ex.Call.App
	}
	cl := NewClient(fc, app)

	if ex.Push != nil || ex.PushApps != nil || ex.PushDevices != nil {
		var heard []any
		cl.OnChanged(func(etag string) { heard = append(heard, etag) })
		cl.OnApps(func(list []App) { heard = append(heard, list) })
		cl.OnDevices(func(list []Device) { heard = append(heard, list) })
		play(cl, ex.Server, "")
		var want any
		if err := json.Unmarshal(ex.Result, &want); err != nil {
			t.Fatal(err)
		}
		if got := normal(t, heard); !reflect.DeepEqual(got, []any{want}) {
			t.Errorf("heard %v, want %v", got, want)
		}
		return
	}

	type outcome struct {
		result any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := start(cl, ex.Call)
		done <- outcome{r, err}
	}()
	waitFor(t, "the client's frame", func() bool {
		frames, body, _ := fc.sent()
		return len(frames) == 1 && len(body) == len(ex.ClientBody)
	})
	frames, body, _ := fc.sent()
	if got, want := normal(t, frames[0]), normal(t, ex.Client); !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if string(body) != ex.ClientBody {
		t.Errorf("sent body %q, want %q", body, ex.ClientBody)
	}

	play(cl, ex.Server, ex.ServerBody)
	if ex.Closes {
		cl.ChannelClosed()
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the call did not finish")
	}
	if ex.Error != "" {
		if !HasCode(o.err, ex.Error) {
			t.Errorf("error %v, want code %s", o.err, ex.Error)
		}
		return
	}
	if o.err != nil {
		t.Fatalf("error %v, want %s", o.err, ex.Result)
	}
	var want any
	if err := json.Unmarshal(ex.Result, &want); err != nil {
		t.Fatal(err)
	}
	if got := normal(t, o.result); !reflect.DeepEqual(got, want) {
		t.Errorf("result %v, want %v", got, want)
	}
}

func TestConformanceExchanges(t *testing.T) {
	var v struct {
		Exchanges  []exchange `json:"exchanges"`
		ServerKind []exchange `json:"serverKind"`
		Tolerated  struct {
			Server []struct {
				Name   string           `json:"name"`
				Call   *call            `json:"call"`
				Frames []map[string]any `json:"frames"`
				Result json.RawMessage  `json:"result"`
				Error  string           `json:"error"`
			} `json:"server"`
		} `json:"tolerated"`
	}
	conformance.Read(t, "frames.json", &v)
	if len(v.Exchanges) == 0 || len(v.ServerKind) == 0 || len(v.Tolerated.Server) == 0 {
		t.Fatal("no exchanges in the vectors")
	}
	for _, ex := range append(v.Exchanges, v.ServerKind...) {
		t.Run(ex.Name, func(t *testing.T) { playExchange(t, ex) })
	}
	for _, ex := range v.Tolerated.Server {
		t.Run("tolerates "+ex.Name, func(t *testing.T) {
			fc := &fakeChannel{}
			cl := NewClient(fc, ex.Call.App)
			done := make(chan error, 1)
			var result any
			go func() {
				r, err := start(cl, ex.Call)
				result = r
				done <- err
			}()
			waitFor(t, "the client's frame", func() bool { f, _, _ := fc.sent(); return len(f) == 1 })
			play(cl, ex.Frames, "")
			err := <-done
			if ex.Error != "" {
				if !HasCode(err, ex.Error) {
					t.Errorf("error %v, want code %s", err, ex.Error)
				}
				return
			}
			var want any
			_ = json.Unmarshal(ex.Result, &want)
			if got := normal(t, result); err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("result %v, %v; want %v", got, err, want)
			}
		})
	}
}
