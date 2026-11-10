package pairing

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestCodeRoundTrip(t *testing.T) {
	in := Code{
		V: 1, ID: "server-1", N: "Home", P: "icepassword",
		F: "q80-fingerprint", O: 47800,
		H: []string{"abc.local", "192.168.1.5"}, S: "secret",
	}
	s := EncodeCode(in)
	if len(s) <= len("wsrv1.") || s[:6] != "wsrv1." {
		t.Fatalf("code %q lacks the wsrv1. prefix", s)
	}
	out, err := DecodeCode(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("decoded %+v, want %+v", out, in)
	}
	for _, bad := range []string{"xyz", "wsrv1.!!", "wsrv1.bm90IGpzb24", ""} {
		if _, err := DecodeCode(bad); err == nil {
			t.Errorf("DecodeCode(%q) succeeded, want an error", bad)
		}
	}
}

func TestSecretLifecycle(t *testing.T) {
	clk := newClock()
	s := NewSecrets(clk.now)

	if s.Check("") || s.Consume("") {
		t.Error("empty secret matched with nothing issued")
	}
	first, expires := s.Issue()
	if want := clk.now().Add(10 * time.Minute); !expires.Equal(want) {
		t.Errorf("expires %v, want %v", expires, want)
	}
	if len(first) != 22 { // 16 bytes, unpadded base64url
		t.Errorf("secret %q has length %d, want 22", first, len(first))
	}
	if !s.Check(first) || !s.Check(first) {
		t.Error("issued secret does not check true repeatedly")
	}
	if s.Check("") || s.Check(first+"x") || s.Check(first[:len(first)-1]) {
		t.Error("wrong secret checked true")
	}
	if !s.Consume(first) {
		t.Error("first Consume = false")
	}
	if s.Consume(first) || s.Check(first) {
		t.Error("consumed secret still valid")
	}

	a, _ := s.Issue()
	b, _ := s.Issue()
	if a == b {
		t.Fatal("two Issue calls returned the same secret")
	}
	if s.Check(a) || s.Consume(a) {
		t.Error("revoked secret still valid")
	}
	if !s.Check(b) {
		t.Error("latest secret not valid")
	}

	clk.advance(10*time.Minute - time.Second)
	if !s.Check(b) {
		t.Error("secret expired before 10 minutes")
	}
	clk.advance(2 * time.Second) // 10 min 1 s after Issue
	if s.Check(b) || s.Consume(b) {
		t.Error("secret still valid after 10 minutes")
	}
}

func TestSecretExpiresAtExactlyTenMinutes(t *testing.T) {
	clk := newClock()
	s := NewSecrets(clk.now)
	secret, _ := s.Issue()
	clk.advance(10 * time.Minute)
	if s.Check(secret) {
		t.Error("secret valid at now == expires")
	}
}

func TestRefusals(t *testing.T) {
	clk := newClock()
	r := NewRefusals(clk.now)
	if got := r.Recent(); len(got) != 0 {
		t.Fatalf("fresh Recent = %v", got)
	}

	apps := []string{"a1", "a2", "a3", "a4", "a5", "a6"}
	for _, app := range apps {
		r.Record(app, "phone-"+app)
		clk.advance(time.Second)
	}
	got := r.Recent()
	if len(got) != 5 {
		t.Fatalf("Recent has %d entries, want 5", len(got))
	}
	for i, want := range []string{"a6", "a5", "a4", "a3", "a2"} {
		if got[i].App != want || got[i].Device != "phone-"+want {
			t.Errorf("Recent[%d] = %+v, want app %s", i, got[i], want)
		}
	}

	// Entries age out.
	r = NewRefusals(clk.now)
	r.Record("old", "tablet")
	clk.advance(11 * time.Minute)
	r.Record("new", "phone")
	got = r.Recent()
	if len(got) != 1 || got[0].App != "new" {
		t.Errorf("Recent = %+v, want only the new entry", got)
	}
	clk.advance(10*time.Minute + time.Second)
	if got := r.Recent(); len(got) != 0 {
		t.Errorf("Recent after expiry = %+v", got)
	}

	// Clear removes one app's entries.
	r = NewRefusals(clk.now)
	r.Record("notes", "a")
	r.Record("todo", "b")
	r.Record("notes", "c")
	r.Clear("notes")
	got = r.Recent()
	if len(got) != 1 || got[0].App != "todo" {
		t.Errorf("after Clear(notes) Recent = %+v", got)
	}
}

func TestRefusalsDeduplicated(t *testing.T) {
	clk := newClock()
	r := NewRefusals(clk.now)
	r.Record("a1", "phone")
	clk.advance(time.Second)
	r.Record("a2", "tablet")
	clk.advance(time.Second)
	r.Record("a1", "phone")
	got := r.Recent()
	if len(got) != 2 {
		t.Fatalf("Recent = %+v, want 2 entries", got)
	}
	if got[0].App != "a1" || got[0].Device != "phone" || !got[0].At.Equal(clk.now()) {
		t.Errorf("Recent[0] = %+v, want a1/phone at the latest time", got[0])
	}
	if got[1].App != "a2" {
		t.Errorf("Recent[1] = %+v, want a2/tablet", got[1])
	}

	// One device retrying cannot push the others out.
	for i := 0; i < 20; i++ {
		r.Record("a3", "laptop")
	}
	if got := r.Recent(); len(got) != 3 {
		t.Errorf("Recent = %+v, want 3 entries", got)
	}

	// The same name for another app is another entry.
	r.Record("a2", "phone")
	if got := r.Recent(); len(got) != 4 {
		t.Errorf("Recent = %+v, want 4 entries", got)
	}
}

func TestRefusalAgesOutAfterExactlyTenMinutes(t *testing.T) {
	clk := newClock()
	r := NewRefusals(clk.now)
	r.Record("app", "phone")
	clk.advance(10 * time.Minute)
	if got := r.Recent(); len(got) != 1 {
		t.Errorf("Recent at exactly 10 minutes = %+v, want the entry", got)
	}
	clk.advance(time.Nanosecond)
	if got := r.Recent(); len(got) != 0 {
		t.Errorf("Recent 1 ns after 10 minutes = %+v, want none", got)
	}
}

func TestRefusalsMemoryBounded(t *testing.T) {
	clk := newClock()
	r := NewRefusals(clk.now)
	for i := 0; i < 100; i++ {
		r.Record("app", fmt.Sprint("device-", i))
	}
	r.mu.Lock()
	n := len(r.entries)
	r.mu.Unlock()
	if n != 5 {
		t.Errorf("%d entries kept, want 5", n)
	}
}

func TestConcurrentUse(t *testing.T) {
	clk := newClock()
	s := NewSecrets(clk.now)
	r := NewRefusals(clk.now)

	var (
		mu       sync.Mutex
		consumed = map[string]int{}
		wins     int
	)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				secret, _ := s.Issue()
				s.Check(secret)
				if s.Consume(secret) {
					mu.Lock()
					consumed[secret]++
					wins++
					mu.Unlock()
				}
				r.Record("app", fmt.Sprint("device-", i))
				if n := len(r.Recent()); n == 0 || n > 5 {
					t.Errorf("Recent has %d entries, want 1 to 5", n)
				}
				r.Clear("other")
				clk.advance(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	for secret, n := range consumed {
		if n != 1 {
			t.Errorf("secret %q consumed %d times", secret, n)
		}
	}
	if wins == 0 {
		t.Error("no Consume succeeded")
	}
	got := r.Recent()
	if len(got) != 5 {
		t.Fatalf("Recent has %d entries, want 5", len(got))
	}
	seen := map[string]bool{}
	for i, e := range got {
		if e.App != "app" || seen[e.Device] {
			t.Errorf("Recent[%d] = %+v: wrong app or a repeated device", i, e)
		}
		seen[e.Device] = true
		if i > 0 && e.At.After(got[i-1].At) {
			t.Errorf("Recent is not newest first: %+v", got)
		}
	}
	secret, _ := s.Issue()
	if !s.Consume(secret) {
		t.Error("a fresh secret could not be consumed after the concurrent use")
	}
}

func TestConsumeIsSingleUseUnderContention(t *testing.T) {
	s := NewSecrets(newClock().now)
	secret, _ := s.Issue()
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Consume(secret) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("Consume succeeded %d times, want 1", wins)
	}
}
