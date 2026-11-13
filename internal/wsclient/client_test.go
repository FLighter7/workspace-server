package wsclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
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
		_, err := cl.Put(context.Background(), testTx, testKey, nil, int64(len(body)), 0, bytes.NewReader(body))
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
	cl.Receive(true, []byte(`{"t":"staged","path":"","id":"data","type":"bin","received":1097728}`))
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
		"a negative body":       {{true, `{"t":"doc","path":"","id":"a","type":"b","etag":"e","bytes":-1}`}},
		"an offset past a body": {{true, `{"t":"doc","path":"","id":"a","type":"b","etag":"e","bytes":1,"offset":2}`}},
		"a doc nobody asked":    {{true, `{"t":"doc","path":"","id":"a","type":"b","etag":"e","bytes":1}`}},
		"binary with no body":   {{false, "x"}},
		"a reply nobody waited": {{true, `{"t":"head","revision":null}`}},
		"a malformed changed":   {{true, `{"t":"changed"}`}},
		"a malformed diverged":  {{true, `{"t":"diverged","servers":[{"name":"x"}]}`}},
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
	cl.OnChanged(func(rev *string) {
		if rev == nil {
			heard = append(heard, "empty")
			return
		}
		heard = append(heard, *rev)
	})
	cl.OnApps(func(list []App) { heard = append(heard, "apps:"+list[0].ID) })
	cl.OnDiverged(func(list []DivergedServer) { heard = append(heard, "diverged:"+list[0].ServerID) })
	cl.OnResolved(func(server string) { heard = append(heard, "resolved:"+server) })
	done := make(chan error, 1)
	var rev *string
	go func() {
		var err error
		rev, err = cl.Head(context.Background())
		done <- err
	}()
	waitFor(t, "head", func() bool { f, _, _ := fc.sent(); return len(f) == 1 })
	cl.Receive(true, []byte(`{"t":"changed","revision":"r2"}`))
	cl.Receive(true, []byte(`{"t":"changed","revision":null}`))
	cl.Receive(true, []byte(`{"t":"apps","apps":[{"id":"todo","name":"Todo"}]}`))
	cl.Receive(true, []byte(`{"t":"diverged","servers":[{"serverId":"Lp7tOp0123ab","name":"Laptop","since":"2026-11-13T09:00:00Z"}]}`))
	cl.Receive(true, []byte(`{"t":"resolved","server":"Lp7tOp0123ab"}`))
	cl.Receive(true, []byte(`{"t":"head","revision":"r3"}`))
	if err := <-done; err != nil || rev == nil || *rev != "r3" {
		t.Fatalf("Head = %v, %v; want r3", rev, err)
	}
	if want := []string{"r2", "empty", "apps:todo", "diverged:Lp7tOp0123ab", "resolved:Lp7tOp0123ab"}; !slices.Equal(heard, want) {
		t.Errorf("heard %v, want %v", heard, want)
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

const testTx = "tx-0123456789abcdef"

var testKey = Key{ID: "data", Type: "bin"}

// async runs f, which makes one request, and returns a channel of its error.
func async(f func() error) chan error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	return done
}

// A doc's body goes to the writer as it arrives; a writer that fails gets
// nothing more, the rest of the body is read and dropped, the get fails with
// the writer's error, and the client stays usable.
func TestGetStreamsIntoTheWriter(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	var got bytes.Buffer
	done := async(func() error { _, err := cl.Get(context.Background(), testKey, nil, 0, &got); return err })
	waitFor(t, "get", func() bool { f, _, _ := fc.sent(); return len(f) == 1 })
	cl.Receive(true, []byte(`{"t":"doc","path":"","id":"data","type":"bin","etag":"e1","bytes":5,"offset":0}`))
	cl.Receive(false, []byte("ab"))
	if got.String() != "ab" {
		t.Errorf("the writer holds %q before the body is complete, want ab", got.String())
	}
	cl.Receive(false, []byte("cde"))
	if err := <-done; err != nil || got.String() != "abcde" {
		t.Fatalf("Get = %v, %q", err, got.String())
	}

	w := &failingWriter{after: 2}
	done = async(func() error { _, err := cl.Get(context.Background(), testKey, nil, 0, w); return err })
	waitFor(t, "the second get", func() bool { f, _, _ := fc.sent(); return len(f) == 2 })
	cl.Receive(true, []byte(`{"t":"doc","path":"","id":"data","type":"bin","etag":"e1","bytes":5,"offset":0}`))
	cl.Receive(false, []byte("ab"))
	cl.Receive(false, []byte("cd"))
	cl.Receive(false, []byte("e"))
	if err := <-done; !errors.Is(err, errDiskFull) {
		t.Fatalf("Get = %v, want the writer's error", err)
	}
	if w.calls != 2 {
		t.Errorf("the writer was called %d times, want 2: none after it failed", w.calls)
	}
	done = async(func() error { _, err := cl.Head(context.Background()); return err })
	waitFor(t, "head", func() bool { f, _, _ := fc.sent(); return len(f) == 3 })
	cl.Receive(true, []byte(`{"t":"head","revision":null}`))
	if err := <-done; err != nil {
		t.Errorf("Head after a failed writer = %v", err)
	}
}

var errDiskFull = errors.New("disk full")

type failingWriter struct{ after, calls int }

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls >= w.after {
		return 0, errDiskFull
	}
	return len(p), nil
}

// A doc that is not the one asked for breaks the protocol.
func TestGetOfAnotherDocument(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	done := async(func() error { _, err := cl.Get(context.Background(), testKey, nil, 0, io.Discard); return err })
	waitFor(t, "get", func() bool { f, _, _ := fc.sent(); return len(f) == 1 })
	cl.Receive(true, []byte(`{"t":"doc","path":"","id":"other","type":"bin","etag":"e1","bytes":0,"offset":0}`))
	if err := <-done; !HasCode(err, CodeProtocol) || !fc.isClosed() {
		t.Errorf("Get = %v, closed %v; want a protocol error", err, fc.isClosed())
	}
}

// A put whose body ends before its announced size ends the client: the
// server would take the next frame for the rest of it.
func TestPutWithAShortBody(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	_, err := cl.Put(context.Background(), testTx, testKey, nil, 10, 0, strings.NewReader("short"))
	if err == nil || !fc.isClosed() || !HasCode(cl.Err(), CodeClosed) {
		t.Errorf("Put = %v, closed %v, Err %v; want the client ended", err, fc.isClosed(), cl.Err())
	}
	if _, err := cl.Put(context.Background(), testTx, testKey, nil, 1, 2, strings.NewReader("")); err == nil {
		t.Error("a put whose offset is past its size was sent")
	}
}

// ListAll reads every page at one revision, and starts over when the set
// changes between pages.
func TestListAllPages(t *testing.T) {
	fc := &fakeChannel{}
	cl := NewClient(fc, "todo")
	var l Listing
	done := async(func() error { var err error; l, err = cl.ListAll(context.Background()); return err })
	answer := func(n int, frame string) {
		t.Helper()
		waitFor(t, fmt.Sprintf("list %d", n), func() bool { f, _, _ := fc.sent(); return len(f) == n })
		cl.Receive(true, []byte(frame))
	}
	answer(1, `{"t":"list","revision":"r1","docs":[{"path":"","id":"a","type":"txt","etag":"e1","bytes":1}],"removed":[],"cursor":"d:a.txt"}`)
	answer(2, `{"t":"stale","revision":"r2"}`)
	answer(3, `{"t":"list","revision":"r2","docs":[{"path":"","id":"a","type":"txt","etag":"e1","bytes":1}],"removed":[],"cursor":"d:a.txt"}`)
	answer(4, `{"t":"list","revision":"r2","docs":[{"path":"","id":"b","type":"txt","etag":"e2","bytes":2}],"removed":[{"path":"","id":"c","type":"txt","at":"2026-11-13T10:00:00Z"}]}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	frames, _, _ := fc.sent()
	if frames[1]["revision"] != "r1" || frames[1]["cursor"] != "d:a.txt" || frames[2]["revision"] != nil || frames[3]["revision"] != "r2" {
		t.Errorf("sent %v", frames)
	}
	if *l.Revision != "r2" || len(l.Docs) != 2 || l.Docs[1].ID != "b" || len(l.Removed) != 1 {
		t.Errorf("listing %+v", l)
	}
}
