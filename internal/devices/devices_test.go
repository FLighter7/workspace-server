package devices

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
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
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openFresh(t *testing.T) (*Registry, string, *clock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "devices.json")
	clk := newClock()
	r, err := Open(path, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return r, path, clk
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPairAndAuthenticate(t *testing.T) {
	r, _, clk := openFresh(t)
	d, token, err := r.Pair("todo", "  Alex's phone  ")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{16}$`).MatchString(d.ID) {
		t.Errorf("id %q is not 16 alphanumeric characters", d.ID)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
		t.Errorf("token %q is not 32 bytes of base64url", token)
	}
	if d.App != "todo" || d.Name != "Alex's phone" {
		t.Errorf("device = %+v", d)
	}
	if !d.PairedAt.Equal(clk.now()) || !d.LastSeen.Equal(d.PairedAt) {
		t.Errorf("PairedAt %v, LastSeen %v, want both %v", d.PairedAt, d.LastSeen, clk.now())
	}

	got, ok := r.Authenticate("todo", d.ID, token)
	if !ok || !reflect.DeepEqual(got, d) {
		t.Errorf("Authenticate = %+v, %v; want %+v, true", got, ok, d)
	}
	if _, ok := r.Authenticate("todo", d.ID, token+"x"); ok {
		t.Error("wrong token authenticated")
	}
	if _, ok := r.Authenticate("todo", d.ID, ""); ok {
		t.Error("empty token authenticated")
	}
	if _, ok := r.Authenticate("notes", d.ID, token); ok {
		t.Error("right token for another app authenticated")
	}
	if _, ok := r.Authenticate("todo", "nosuchdevice0000", token); ok {
		t.Error("unknown id authenticated")
	}
}

func TestPairName(t *testing.T) {
	r, _, _ := openFresh(t)
	for _, tc := range []struct{ in, want string }{
		{"", "Device"},
		{"   \t", "Device"},
		{"Phone", "Phone"},
		{strings.Repeat("é", 100), strings.Repeat("é", 64)},
	} {
		d, _, err := r.Pair("todo", tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if d.Name != tc.want {
			t.Errorf("Pair(%q).Name = %q, want %q", tc.in, d.Name, tc.want)
		}
	}
}

func TestTokenNotStoredInPlaintext(t *testing.T) {
	r, path, _ := openFresh(t)
	d, token, err := r.Pair("todo", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(readFile(t, path), []byte(token)) {
		t.Error("devices.json contains the token")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(d.TokenHash) {
		t.Errorf("tokenHash %q is not 64 lowercase hex", d.TokenHash)
	}
	if !bytes.Contains(readFile(t, path), []byte(d.TokenHash)) {
		t.Error("devices.json does not contain the token hash")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	r, path, clk := openFresh(t)
	d, token, err := r.Pair("todo", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Authenticate("todo", d.ID, token)
	if !ok || !reflect.DeepEqual(got, d) {
		t.Errorf("after reopen: %+v, %v; want %+v, true", got, ok, d)
	}
	if !bytes.Contains(readFile(t, path), []byte(`"2026-11-07T12:00:00Z"`)) {
		t.Errorf("times are not RFC 3339 UTC:\n%s", readFile(t, path))
	}
}

func TestOpenMissingAndInvalid(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(filepath.Join(dir, "devices.json"), newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List() = %+v, want empty", got)
	}

	bad := filepath.Join(dir, "bad", "devices.json")
	if err := os.MkdirAll(filepath.Dir(bad), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(bad, newClock().now)
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("Open(invalid) error = %v, want one naming %s", err, bad)
	}
}

func TestOpenRejectsInvalidEntries(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	entry := func(id, app, tokenHash string) string {
		return fmt.Sprintf(`{"id":%q,"app":%q,"name":"Phone","tokenHash":%q,"pairedAt":"2026-11-07T12:00:00Z","lastSeen":"2026-11-07T12:00:00Z"}`, id, app, tokenHash)
	}
	for _, c := range []struct{ name, devices, want string }{
		{"empty id", entry("", "todo", hash), "empty id"},
		{"repeated id", entry("d1", "todo", hash) + "," + entry("d1", "notes", hash), `"d1" is used twice`},
		{"bad app", entry("d1", "To Do", hash), "app"},
		{"empty tokenHash", entry("d1", "todo", ""), "tokenHash"},
		{"short tokenHash", entry("d1", "todo", hash[:63]), "tokenHash"},
		{"uppercase tokenHash", entry("d1", "todo", strings.ToUpper(hash)), "tokenHash"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "devices.json")
			raw := []byte(`{"devices":[` + c.devices + `]}`)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Open(path, newClock().now)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Open: %v; want an error naming %s and %q", err, path, c.want)
			}
			if got := readFile(t, path); !bytes.Equal(got, raw) {
				t.Error("the invalid file was changed")
			}
		})
	}
}

func TestOpenReadsTimesAsUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	raw := `{"devices":[{"id":"d1","app":"todo","name":"Phone","tokenHash":"` + strings.Repeat("ab", 32) +
		`","pairedAt":"2026-11-07T14:00:00+02:00","lastSeen":"2026-11-07T15:30:00+02:00"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	d := r.List()[0]
	if d.PairedAt.Location() != time.UTC || d.LastSeen.Location() != time.UTC {
		t.Errorf("times %v, %v are not in UTC", d.PairedAt, d.LastSeen)
	}
	if want := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC); !d.PairedAt.Equal(want) {
		t.Errorf("PairedAt %v, want %v", d.PairedAt, want)
	}
}

// A last-seen write that fails is retried, and logged, at most once a
// minute, not on every hello.
func TestTouchFailureThrottled(t *testing.T) {
	r, _, clk := openFresh(t)
	d, _ := mustPair(t, r, "todo", "Phone")
	// A regular file where the folder should be: every write fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.path = filepath.Join(blocker, "devices.json")
	r.mu.Unlock()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	clk.advance(2 * time.Minute)
	for range 3 {
		r.Touch(d.ID)
		clk.advance(10 * time.Second)
	}
	if n := strings.Count(buf.String(), "save last-seen time"); n != 1 {
		t.Errorf("%d failure lines within a minute, want 1:\n%s", n, buf.String())
	}
	clk.advance(time.Minute)
	r.Touch(d.ID)
	if n := strings.Count(buf.String(), "save last-seen time"); n != 2 {
		t.Errorf("%d failure lines after a minute, want 2:\n%s", n, buf.String())
	}
}

func TestRemove(t *testing.T) {
	r, path, clk := openFresh(t)
	d, token, err := r.Pair("todo", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	other, otherToken, err := r.Pair("todo", "Tablet")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Remove(d.ID) {
		t.Fatal("Remove = false for an existing device")
	}
	if r.Remove(d.ID) {
		t.Error("Remove = true for a device already removed")
	}
	if _, ok := r.Authenticate("todo", d.ID, token); ok {
		t.Error("removed device still authenticates")
	}
	again, err := Open(path, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Authenticate("todo", d.ID, token); ok {
		t.Error("removed device authenticates after reopen")
	}
	if _, ok := again.Authenticate("todo", other.ID, otherToken); !ok {
		t.Error("the other device stopped authenticating")
	}
}

// mustPair pairs a device and fails the test on an error.
func mustPair(t *testing.T, r *Registry, app, name string) (Device, string) {
	t.Helper()
	d, token, err := r.Pair(app, name)
	if err != nil {
		t.Fatal(err)
	}
	return d, token
}

func TestRemoveApp(t *testing.T) {
	r, path, clk := openFresh(t)
	a1, _ := mustPair(t, r, "todo", "Phone")
	a2, _ := mustPair(t, r, "todo", "Tablet")
	keep, keepToken, err := r.Pair("notes", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	removed := r.RemoveApp("todo")
	if len(removed) != 2 {
		t.Fatalf("RemoveApp returned %+v, want 2 devices", removed)
	}
	ids := map[string]bool{removed[0].ID: true, removed[1].ID: true}
	if !ids[a1.ID] || !ids[a2.ID] {
		t.Errorf("RemoveApp returned %+v, want %s and %s", removed, a1.ID, a2.ID)
	}
	if got := r.List(); len(got) != 1 || got[0].ID != keep.ID {
		t.Errorf("List() = %+v, want only %s", got, keep.ID)
	}
	if got := r.RemoveApp("todo"); len(got) != 0 {
		t.Errorf("second RemoveApp returned %+v, want none", got)
	}
	again, err := Open(path, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Authenticate("notes", keep.ID, keepToken); !ok {
		t.Error("device of another app lost after reopen")
	}
	if got := again.List(); len(got) != 1 {
		t.Errorf("after reopen List() = %+v, want 1 device", got)
	}
}

func TestListSorted(t *testing.T) {
	r, _, _ := openFresh(t)
	for _, p := range [][2]string{{"todo", "b"}, {"notes", "z"}, {"todo", "a"}, {"notes", "a"}} {
		if _, _, err := r.Pair(p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, d := range r.List() {
		got = append(got, d.App+"/"+d.Name)
	}
	want := []string{"notes/a", "notes/z", "todo/a", "todo/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List order = %v, want %v", got, want)
	}
}

func TestTouchThrottlesWrites(t *testing.T) {
	r, path, clk := openFresh(t)
	d, token, err := r.Pair("todo", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	afterPair := readFile(t, path)

	clk.advance(10 * time.Second)
	r.Touch(d.ID)
	first := readFile(t, path)
	if bytes.Equal(first, afterPair) {
		t.Fatal("first Touch did not write the file")
	}

	clk.advance(30 * time.Second)
	r.Touch(d.ID)
	if got := readFile(t, path); !bytes.Equal(got, first) {
		t.Error("second Touch within a minute wrote the file")
	}
	got, ok := r.Authenticate("todo", d.ID, token)
	if !ok {
		t.Fatal("Authenticate failed")
	}
	if want := clk.now(); !got.LastSeen.Equal(want) {
		t.Errorf("in-memory LastSeen = %v, want %v", got.LastSeen, want)
	}

	clk.advance(61 * time.Second)
	r.Touch(d.ID)
	if got := readFile(t, path); bytes.Equal(got, first) {
		t.Error("Touch after 61 s did not write the file")
	}
}

func TestTouchUnknownIDIsNoOp(t *testing.T) {
	r, path, _ := openFresh(t)
	r.Touch("nosuchdevice0000")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Touch of an unknown id created the file (err = %v)", err)
	}
}

func TestOtherWritesPersistUnsavedLastSeen(t *testing.T) {
	r, path, clk := openFresh(t)
	d, _ := mustPair(t, r, "todo", "Phone")
	clk.advance(5 * time.Second)
	r.Touch(d.ID) // written
	clk.advance(5 * time.Second)
	r.Touch(d.ID) // throttled, in memory only
	want := clk.now()
	if _, _, err := r.Pair("todo", "Tablet"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range again.List() {
		if got.ID == d.ID && !got.LastSeen.Equal(want) {
			t.Errorf("persisted LastSeen = %v, want %v", got.LastSeen, want)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	r, _, _ := openFresh(t)
	d, token, err := r.Pair("todo", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Touch(d.ID)
			if _, ok := r.Authenticate("todo", d.ID, token); !ok {
				t.Error("Authenticate failed during concurrent use")
			}
			r.List()
			if _, _, err := r.Pair("notes", "x"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := len(r.List()); got != 9 {
		t.Errorf("List() has %d devices, want 9", got)
	}
}
