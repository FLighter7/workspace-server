package wsclient

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"
)

// A body pauses while more than BufferHigh bytes wait in the channel and
// goes on once the channel drains.
func TestPutWaitsForTheBuffer(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	body := make([]byte, BufferHigh+3*ChunkBytes)
	done := make(chan error, 1)
	go func() {
		_, _, err := cl.Put(context.Background(), nil, body)
		done <- err
	}()
	waitFor(t, "the sender to pause", func() bool { return fc.BufferedAmount() > BufferHigh })
	time.Sleep(50 * time.Millisecond)
	_, sent, _ := fc.sent()
	if len(sent) >= len(body) {
		t.Fatal("the whole body went out without a pause")
	}
	fc.drain()
	waitFor(t, "the rest of the body", func() bool { _, b, _ := fc.sent(); return len(b) == len(body) })
	cl.Receive(true, []byte(`{"t":"stored","etag":"e1"}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// What the protocol does not allow ends the client and closes the channel.
func TestBrokenServerFramesEndTheClient(t *testing.T) {
	for name, msgs := range map[string][]struct {
		text bool
		data string
	}{
		"not JSON":              {{true, `{"t":`}},
		"no t":                  {{true, `{"etag":null}`}},
		"a negative body":       {{true, `{"t":"doc","etag":null,"bytes":-1}`}},
		"binary with no body":   {{false, "x"}},
		"a body overrun":        {{true, `{"t":"doc","etag":null,"bytes":1}`}, {false, "xy"}},
		"text inside a body":    {{true, `{"t":"doc","etag":null,"bytes":2}`}, {false, "x"}, {true, `{"t":"head","etag":null}`}},
		"a reply nobody waited": {{true, `{"t":"head","etag":null}`}},
		"a malformed app list":  {{true, `{"t":"apps","apps":[{"name":"x"}]}`}},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeChannel{}
			cl := NewClient(fc, "todo")
			for _, m := range msgs {
				cl.Receive(m.text, []byte(m.data))
			}
			select {
			case <-cl.Done():
			default:
				t.Fatal("the client did not end")
			}
			if !HasCode(cl.Err(), CodeProtocol) || !fc.isClosed() {
				t.Errorf("Err %v, channel closed %v; want a protocol error and a closed channel", cl.Err(), fc.isClosed())
			}
			if _, err := cl.Head(context.Background()); !HasCode(err, CodeProtocol) {
				t.Errorf("Head after the end = %v, want the protocol error", err)
			}
		})
	}
}

// Pushes reach their listeners whatever request is in flight, and a push
// does not answer that request.
func TestPushesDuringARequest(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	var heard []string
	cl.OnChanged(func(etag string) { heard = append(heard, etag) })
	cl.OnApps(func(list []App) { heard = append(heard, "apps:"+list[0].ID) })
	done := make(chan error, 1)
	var etag *string
	go func() {
		var err error
		etag, err = cl.Head(context.Background())
		done <- err
	}()
	waitFor(t, "head", func() bool { f, _, _ := fc.sent(); return len(f) == 1 })
	cl.Receive(true, []byte(`{"t":"changed","etag":"e2"}`))
	cl.Receive(true, []byte(`{"t":"apps","apps":[{"id":"todo","name":"Todo"}]}`))
	cl.Receive(true, []byte(`{"t":"head","etag":"e3"}`))
	if err := <-done; err != nil || etag == nil || *etag != "e3" {
		t.Fatalf("Head = %v, %v; want e3", etag, err)
	}
	if !slices.Equal(heard, []string{"e2", "apps:todo"}) {
		t.Errorf("heard %v, want e2 and the app list", heard)
	}
}

// A request given up ends the client: a late reply could not be told from
// the next request's.
func TestCancelledRequestEndsTheClient(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := cl.Head(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Head = %v, want the deadline", err)
	}
	if !HasCode(cl.Err(), CodeClosed) || !fc.isClosed() {
		t.Errorf("Err %v, closed %v; want the client closed", cl.Err(), fc.isClosed())
	}
}

// The channel's end ends a request in flight.
func TestChannelClosedEndsARequest(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	done := make(chan error, 1)
	go func() {
		_, err := cl.Head(context.Background())
		done <- err
	}()
	waitFor(t, "head", func() bool { f, _, _ := fc.sent(); return len(f) == 1 })
	cl.ChannelClosed()
	if err := <-done; !HasCode(err, CodeClosed) {
		t.Errorf("Head = %v, want closed", err)
	}
}

// Hosts become IPv4 candidates in order: a .local name what it resolves to,
// an address as it is, the rest left out.
func TestCandidates(t *testing.T) {
	resolve := func(_ context.Context, name string) ([]net.IP, error) {
		if name == "a.local" {
			return []net.IP{net.IPv4(192, 168, 1, 5), net.ParseIP("fe80::1"), net.IPv4(10, 0, 0, 1)}, nil
		}
		return nil, errors.New("no answer")
	}
	got := candidates(context.Background(),
		[]string{"a.local", "b.local", "10.0.0.1", "192.168.1.9", "example.com", "fe80::2"},
		DialOptions{Resolve: resolve, ResolveTimeout: time.Second})
	if want := []string{"192.168.1.5", "10.0.0.1", "192.168.1.9"}; !slices.Equal(got, want) {
		t.Errorf("candidates %v, want %v", got, want)
	}
	if got := candidates(context.Background(), []string{"a.local"}, DialOptions{}); len(got) != 0 {
		t.Errorf("candidates without a resolver %v, want none", got)
	}
}
