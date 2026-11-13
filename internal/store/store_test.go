package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 13, 18, 35, 12, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t     *testing.T
	root  string
	clock *clock
	quota int64
	s     *Store
}

// newEnv returns a store on a fresh root, with app "todo" in mind.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, root: filepath.Join(t.TempDir(), "apps"), clock: &clock{t: t0}}
	e.s = e.open()
	return e
}

// open returns a new Store on e's root: a restart.
func (e *env) open() *Store {
	return New(e.root, e.clock.now, func(string) int64 { return e.quota })
}

// restart replaces e.s with a new Store on the same root and recovers it.
func (e *env) restart() {
	e.t.Helper()
	e.s = e.open()
	if err := e.s.Recover(); err != nil {
		e.t.Fatalf("Recover: %v", err)
	}
}

func key(t *testing.T, s string) Key {
	t.Helper()
	k, ok := ParseKey(s)
	if !ok {
		t.Fatalf("ParseKey(%q) is not a key", s)
	}
	return k
}

func etagOf(s string) string { return Etag([]byte(s)) }

func ptr(s string) *string { return &s }

// file returns the path of rel ("/"-separated) in app's folder.
func (e *env) file(app, rel string) string {
	return filepath.Join(e.root, app, filepath.FromSlash(rel))
}

// writeFile writes a file by hand into app's folder, as a person would.
func (e *env) writeFile(app, rel, body string) {
	e.t.Helper()
	path := e.file(app, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// readFile reads a document's file back.
func (e *env) readFile(app, rel string) (string, bool) {
	e.t.Helper()
	b, err := os.ReadFile(e.file(app, rel))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b), true
}

// docs returns app's listing as "key=body-etag-prefix" lines, checking that
// every listed etag is the hash of the file on disk.
func (e *env) docs(app string) map[string]string {
	e.t.Helper()
	l, err := e.s.List(app)
	if err != nil {
		e.t.Fatalf("List: %v", err)
	}
	out := map[string]string{}
	for _, d := range l.Docs {
		body, ok := e.readFile(app, d.Key.String())
		if !ok {
			e.t.Fatalf("listed %s has no file", d.Key)
		}
		if Etag([]byte(body)) != d.Etag || int64(len(body)) != d.Bytes {
			e.t.Fatalf("listed %s: etag %s, %d bytes; the file holds %q", d.Key, d.Etag[:8], d.Bytes, body)
		}
		out[d.Key.String()] = body
	}
	return out
}

// commitAll stages body for each key (in one transaction of device "dev")
// and commits it. A body of "-" deletes the key. ifEtags are taken from the
// current listing.
func (e *env) commitAll(app string, changes map[string]string) Committed {
	e.t.Helper()
	l, err := e.s.List(app)
	if err != nil {
		e.t.Fatal(err)
	}
	cur := map[string]*string{}
	for _, d := range l.Docs {
		cur[d.Key.String()] = ptr(d.Etag)
	}
	tx := "tx-commitall-0001"
	if _, err := e.s.Begin(app, "dev", tx); err != nil {
		e.t.Fatal(err)
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, ks := range keys {
		body := changes[ks]
		if body == "-" {
			if err := e.s.Delete(app, "dev", tx, key(e.t, ks), cur[ks]); err != nil {
				e.t.Fatal(err)
			}
			continue
		}
		e.put(app, "dev", tx, ks, cur[ks], body)
	}
	c, err := e.s.Commit(app, "dev", tx)
	if err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
	return c
}

// put stages body whole for ks.
func (e *env) put(app, device, tx, ks string, ifEtag *string, body string) {
	e.t.Helper()
	sink, err := e.s.Put(app, device, tx, key(e.t, ks), ifEtag, int64(len(body)), 0)
	if err != nil {
		e.t.Fatalf("Put(%s): %v", ks, err)
	}
	if _, err := io.WriteString(sink, body); err != nil {
		e.t.Fatalf("write %s: %v", ks, err)
	}
	if err := sink.Close(); err != nil {
		e.t.Fatalf("close %s: %v", ks, err)
	}
}

func wantDocs(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("docs = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("docs = %v, want %v", got, want)
		}
	}
}

func TestEmptyApp(t *testing.T) {
	e := newEnv(t)
	rev, err := e.s.Head("todo")
	if err != nil || rev != nil {
		t.Fatalf("Head = %v, %v; want nil", rev, err)
	}
	l, err := e.s.List("todo")
	if err != nil || l.Revision != nil || len(l.Docs) != 0 || len(l.Removed) != 0 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	st, err := e.s.Stats("todo")
	if err != nil || st != (Stats{Quota: 50 << 20}) {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
	// Reading an app that has nothing writes nothing.
	if _, err := os.Stat(filepath.Join(e.root, "todo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the app's folder was made for nothing: %v", err)
	}
}

func TestBadAppID(t *testing.T) {
	e := newEnv(t)
	for _, app := range []string{"", "..", "Todo", "a/b", `a\b`} {
		if _, err := e.s.Head(app); !errors.Is(err, ErrBadApp) {
			t.Errorf("Head(%q) = %v, want ErrBadApp", app, err)
		}
		if _, err := e.s.Begin(app, "dev", "tx-0123456789abcdef"); !errors.Is(err, ErrBadApp) {
			t.Errorf("Begin(%q) = %v, want ErrBadApp", app, err)
		}
	}
}

func TestKeysAtTheBoundary(t *testing.T) {
	e := newEnv(t)
	tx := "tx-0123456789abcdef"
	if _, err := e.s.Begin("todo", "dev", tx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []Key{
		{Path: "", ID: "", Type: "json"},
		{Path: "", ID: "con", Type: "json"},
		{Path: "", ID: "com0", Type: "json"},
		{Path: "lpt0", ID: "a", Type: "json"},
		{Path: "", ID: "a", Type: ""},
		{Path: "", ID: "A", Type: "json"},
		{Path: "", ID: "a.b", Type: "json"},
		{Path: "..", ID: "a", Type: "json"},
		{Path: ".wss", ID: "index", Type: "json"},
		{Path: "a/", ID: "a", Type: "json"},
		{Path: levels(11), ID: "a", Type: "json"},
		{Path: strings.Repeat("a", 200), ID: strings.Repeat("b", 60), Type: "json"},
	} {
		if _, err := e.s.Put("todo", "dev", tx, k, nil, 1, 0); !errors.Is(err, ErrBadPath) {
			t.Errorf("Put(%+v) = %v, want ErrBadPath", k, err)
		}
		if err := e.s.Delete("todo", "dev", tx, k, nil); !errors.Is(err, ErrBadPath) {
			t.Errorf("Delete(%+v) = %v, want ErrBadPath", k, err)
		}
		if _, _, err := e.s.Open("todo", k, "x"); !errors.Is(err, ErrBadPath) {
			t.Errorf("Open(%+v) = %v, want ErrBadPath", k, err)
		}
	}
	for _, id := range []string{"", "short", strings.Repeat("a", 65), "tx-0123456789abcde!", "tx 0123456789abcdef"} {
		if _, err := e.s.Begin("todo", "dev", id); !errors.Is(err, ErrBadTx) {
			t.Errorf("Begin(%q) = %v, want ErrBadTx", id, err)
		}
	}
	for _, id := range []string{"0123456789abcdef", strings.Repeat("Z", 64), "A-b_c-0123456789"} {
		if !ValidTx(id) {
			t.Errorf("ValidTx(%q) = false", id)
		}
	}
}

func TestRevision(t *testing.T) {
	if revisionOf(nil) != nil || revisionOf(map[string]docEntry{}) != nil {
		t.Fatal("the revision of an empty set is not nil")
	}
	docs := map[string]docEntry{
		"b.json":     {Etag: "e2"},
		"a/z.bin":    {Etag: "e1"},
		"a.json":     {Etag: "e0"},
		"a/b/c.json": {Etag: "e3"},
	}
	// "a.json" < "a/b/c.json" < "a/z.bin" < "b.json": byte order, "." < "/".
	sum := sha256.Sum256([]byte("a.json\x00e0\na/b/c.json\x00e3\na/z.bin\x00e1\nb.json\x00e2\n"))
	want := hex.EncodeToString(sum[:])
	for range 10 { // map order must not matter
		if got := revisionOf(docs); got == nil || *got != want {
			t.Fatalf("revisionOf = %v, want %s", got, want)
		}
	}
	// Two stores with the same files agree; size and mtime play no part.
	a, b := newEnv(t), newEnv(t)
	a.writeFile("todo", "x/1.json", "one")
	a.writeFile("todo", "2.txt", "two")
	b.writeFile("todo", "2.txt", "two")
	b.writeFile("todo", "x/1.json", "one")
	if err := os.Chtimes(b.file("todo", "2.txt"), t0, t0); err != nil {
		t.Fatal(err)
	}
	ra, _ := a.s.Head("todo")
	rb, _ := b.s.Head("todo")
	sum = sha256.Sum256([]byte("2.txt\x00" + etagOf("two") + "\nx/1.json\x00" + etagOf("one") + "\n"))
	if ra == nil || rb == nil || *ra != *rb || *ra != hex.EncodeToString(sum[:]) {
		t.Fatalf("revisions %v and %v; want both %x", ra, rb, sum)
	}
}

func TestReconcileTakesInHandEdits(t *testing.T) {
	racyWindow = 0 // the files below are hashed right after they are written
	t.Cleanup(func() { racyWindow = 3 * time.Second })
	e := newEnv(t)
	e.writeFile("todo", "data.json", `{"v":1}`)
	e.writeFile("todo", "photos/2026/a.jpg", "jpeg")
	e.writeFile("todo", "notes/n.md", "note")
	// Invisible: names that are not keys, and the server's own folder.
	for _, rel := range []string{
		"Upper.json", "noext", "a.b.json", "x.json.tmp-123", ".hidden.json",
		"notes/.git/config.json", "Bad/a.json", "con.json", "history/20260101T000000.000Z-abcd1234.json",
		levels(11) + "/deep.json", ".wss/stray.json",
	} {
		e.writeFile("todo", rel, "ignored")
	}
	changed, rev, err := e.s.Reconcile("todo")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("the first operation already loaded the files; Reconcile reports a change")
	}
	if rev == nil {
		t.Fatal("no revision")
	}
	wantDocs(t, e.docs("todo"), map[string]string{"data.json": `{"v":1}`, "photos/2026/a.jpg": "jpeg", "notes/n.md": "note"})

	// Unchanged files are not hashed again.
	hashes := 0
	hashFile = func(path string) (string, int64, int64, error) {
		hashes++
		return hashShared(path)
	}
	t.Cleanup(func() { hashFile = hashShared })
	if changed, _, err := e.s.Reconcile("todo"); changed || err != nil || hashes != 0 {
		t.Fatalf("Reconcile of untouched files = %v, %v; %d hashed", changed, err, hashes)
	}

	// A change of the same size is seen by its mtime; a new file is added; a
	// removed one becomes a tombstone.
	e.writeFile("todo", "data.json", `{"v":2}`)
	later := t0.Add(time.Hour)
	if err := os.Chtimes(e.file("todo", "data.json"), later, later); err != nil {
		t.Fatal(err)
	}
	e.writeFile("todo", "new.txt", "new")
	if err := os.Remove(e.file("todo", "notes/n.md")); err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Minute)
	changed, rev2, err := e.s.Reconcile("todo")
	if err != nil || !changed || *rev2 == *rev {
		t.Fatalf("Reconcile after edits = %v, %v, %v", changed, rev2, err)
	}
	if hashes != 2 {
		t.Errorf("hashed %d files, want 2 (the changed one and the new one)", hashes)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"data.json": `{"v":2}`, "photos/2026/a.jpg": "jpeg", "new.txt": "new"})
	l, _ := e.s.List("todo")
	if len(l.Removed) != 1 || l.Removed[0].Key.String() != "notes/n.md" || !l.Removed[0].At.Equal(t0.Add(time.Minute)) {
		t.Errorf("Removed = %+v", l.Removed)
	}
	// The invisible files are still there, untouched.
	if b, ok := e.readFile("todo", "Upper.json"); !ok || b != "ignored" {
		t.Error("an invisible file was touched")
	}

	// A touch that keeps the bytes changes no revision, and the index learns
	// the new time, so the file is not hashed again.
	if err := os.Chtimes(e.file("todo", "new.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	hashes = 0
	if changed, _, _ := e.s.Reconcile("todo"); changed || hashes != 1 {
		t.Errorf("after a touch: changed %v, hashed %d", changed, hashes)
	}
	if _, _, _ = e.s.Reconcile("todo"); hashes != 1 {
		t.Errorf("hashed the touched file again: %d", hashes)
	}

	// A restart reads the index back and agrees.
	e.restart()
	l2, _ := e.s.List("todo")
	if *l2.Revision != *rev2 || len(l2.Removed) != 1 {
		t.Errorf("after a restart: %+v", l2)
	}
	if hashes != 1 {
		t.Errorf("a restart hashed unchanged files again: %d", hashes-1)
	}
}

func TestReconcileSkipsLinks(t *testing.T) {
	e := newEnv(t)
	e.writeFile("todo", "a.json", "a")
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, e.file("todo", "link.json")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), e.file("todo", "dir")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "a"})
}

func TestTombstones(t *testing.T) {
	e := newEnv(t)
	x := e.s
	// Fill the index directly: 1000 commits would be slow for nothing.
	if err := x.do("todo", func(a *appState) error {
		for i := range MaxTombstones + 5 {
			ks := "t" + strings.Repeat("0", 4-len(itoa(i))) + itoa(i) + ".json"
			a.idx.put(ks, docEntry{Etag: "e"})
			a.idx.remove(ks, t0.Add(time.Duration(i)*time.Second))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l, _ := x.List("todo")
	if len(l.Removed) != MaxTombstones {
		t.Fatalf("%d tombstones, want %d", len(l.Removed), MaxTombstones)
	}
	if l.Removed[0].Key.String() != "t0005.json" {
		t.Errorf("the oldest kept is %s, want t0005.json", l.Removed[0].Key)
	}

	// Through commits: a delete leaves one, a re-creation drops it, a second
	// delete leaves one again.
	e2 := newEnv(t)
	e2.commitAll("todo", map[string]string{"a.json": "1", "b.json": "2"})
	e2.clock.add(time.Second)
	e2.commitAll("todo", map[string]string{"a.json": "-"})
	l, _ = e2.s.List("todo")
	if len(l.Removed) != 1 || l.Removed[0].Key.String() != "a.json" || !l.Removed[0].At.Equal(t0.Add(time.Second)) {
		t.Fatalf("after a delete: %+v", l.Removed)
	}
	e2.commitAll("todo", map[string]string{"a.json": "3"})
	if l, _ = e2.s.List("todo"); len(l.Removed) != 0 {
		t.Fatalf("the re-created document keeps its tombstone: %+v", l.Removed)
	}
	e2.commitAll("todo", map[string]string{"a.json": "-"})
	e2.commitAll("todo", map[string]string{"a.json": "4"})
	e2.commitAll("todo", map[string]string{"a.json": "-"})
	if l, _ = e2.s.List("todo"); len(l.Removed) != 1 {
		t.Fatalf("one key deleted three times: %+v", l.Removed)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestOpen(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a/b.bin": "0123456789"})
	k := key(t, "a/b.bin")
	f, info, err := e.s.Open("todo", k, etagOf("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if info != (DocInfo{Key: k, Etag: etagOf("0123456789"), Bytes: 10}) {
		t.Errorf("info = %+v", info)
	}
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(f)
	f.Close()
	if string(rest) != "456789" {
		t.Errorf("read from offset 4: %q", rest)
	}

	for name, tc := range map[string]struct {
		key  string
		etag string
	}{
		"another etag": {"a/b.bin", etagOf("x")},
		"no such doc":  {"a/c.bin", etagOf("0123456789")},
	} {
		if _, _, err := e.s.Open("todo", key(t, tc.key), tc.etag); !errors.Is(err, ErrStale) {
			t.Errorf("%s: Open = %v, want ErrStale", name, err)
		}
	}

	// Edited by hand since the index saw it: reconciled, and stale.
	e.writeFile("todo", "a/b.bin", "abcdefghijk")
	if _, _, err := e.s.Open("todo", k, etagOf("0123456789")); !errors.Is(err, ErrStale) {
		t.Fatalf("Open of a hand-edited file = %v, want ErrStale", err)
	}
	f, _, err = e.s.Open("todo", k, etagOf("abcdefghijk"))
	if err != nil {
		t.Fatalf("Open with the new etag: %v", err)
	}
	f.Close()
	// Removed by hand: stale, and the index learns it.
	if err := os.Remove(e.file("todo", "a/b.bin")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Open("todo", k, etagOf("abcdefghijk")); !errors.Is(err, ErrStale) {
		t.Fatalf("Open of a removed file = %v, want ErrStale", err)
	}
	if rev, _ := e.s.Head("todo"); rev != nil {
		t.Errorf("revision %v after the only file went", *rev)
	}
}

func TestStats(t *testing.T) {
	e := newEnv(t)
	e.quota = 4 << 20
	e.clock.add(time.Hour)
	e.commitAll("todo", map[string]string{"a.json": "12345", "b/c.txt": "678"})
	st, err := e.s.Stats("todo")
	if err != nil {
		t.Fatal(err)
	}
	if st != (Stats{Files: 2, Bytes: 8, Quota: 4 << 20, Modified: t0.Add(time.Hour)}) {
		t.Errorf("Stats = %+v", st)
	}
}

func TestDeleteApp(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "1"})
	if _, err := e.s.Begin("todo", "dev", "tx-open-0123456789"); err != nil {
		t.Fatal(err)
	}
	sink, err := e.s.Put("todo", "dev", "tx-open-0123456789", key(t, "b.json"), nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteApp("todo"); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("x")); !errors.Is(err, ErrNoTransaction) {
		t.Errorf("a write after the app went = %v, want ErrNoTransaction", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "todo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the app's folder stays: %v", err)
	}
	if rev, err := e.s.Head("todo"); rev != nil || err != nil {
		t.Errorf("Head after DeleteApp = %v, %v", rev, err)
	}
	if err := e.s.DeleteApp("todo"); err != nil {
		t.Errorf("DeleteApp of a missing app: %v", err)
	}
}

func TestIndexOnDisk(t *testing.T) {
	e := newEnv(t)
	e.commitAll("todo", map[string]string{"a.json": "1"})
	// A hand edit of the index that points outside the app is dropped.
	path := filepath.Join(e.root, "todo", ".wss", "index.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"a.json"`) || !strings.Contains(string(b), `"revision"`) {
		t.Fatalf("index.json: %s", b)
	}
	evil := strings.Replace(string(b), `"a.json"`, `"../../evil.json"`, 1)
	if err := os.WriteFile(path, []byte(evil), 0o600); err != nil {
		t.Fatal(err)
	}
	e.restart()
	wantDocs(t, e.docs("todo"), map[string]string{"a.json": "1"})
	// A corrupt index is an error, not an empty set.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.s = e.open()
	if _, err := e.s.Head("todo"); err == nil {
		t.Error("a corrupt index.json loads")
	}
}
