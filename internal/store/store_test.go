package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 18, 35, 12, 0, time.UTC)

func newStore(t *testing.T) (*Store, string, *time.Time) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "apps")
	clock := t0
	return New(root, func() time.Time { return clock }), root, &clock
}

func mustWrite(t *testing.T, s *Store, body string, ifEtag *string, history int) string {
	t.Helper()
	etag, err := s.Write("todo", []byte(body), ifEtag, history)
	if err != nil {
		t.Fatalf("Write(%q): %v", body, err)
	}
	return etag
}

func historyFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "todo", "history"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFirstWrite(t *testing.T) {
	s, _, _ := newStore(t)
	body := []byte(`{"a" :1}`)
	etag, err := s.Write("todo", body, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if etag != Etag(body) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(etag) {
		t.Errorf("etag = %q, want 64 lowercase hex of the body", etag)
	}
	got, gotEtag, err := s.Read("todo")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("Read body = %q, want %q", got, body)
	}
	if gotEtag == nil || *gotEtag != etag {
		t.Errorf("Read etag = %v, want %q", gotEtag, etag)
	}
}

func TestReadWithoutDocument(t *testing.T) {
	s, _, _ := newStore(t)
	body, etag, err := s.Read("todo")
	if body != nil || etag != nil || err != nil {
		t.Errorf("Read = %q, %v, %v; want nil, nil, nil", body, etag, err)
	}
}

func TestCreateWhenExistsConflicts(t *testing.T) {
	s, _, _ := newStore(t)
	first := mustWrite(t, s, `1`, nil, 10)
	_, err := s.Write("todo", []byte(`2`), nil, 10)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if ce.Current == nil || *ce.Current != first {
		t.Errorf("Current = %v, want %q", ce.Current, first)
	}
}

func TestEtagForMissingDocumentConflicts(t *testing.T) {
	s, _, _ := newStore(t)
	etag := Etag([]byte(`1`))
	_, err := s.Write("todo", []byte(`1`), &etag, 10)
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != nil {
		t.Errorf("err = %v, want ConflictError with nil Current", err)
	}
}

func TestStaleEtagConflicts(t *testing.T) {
	s, _, _ := newStore(t)
	first := mustWrite(t, s, `1`, nil, 10)
	second := mustWrite(t, s, `2`, &first, 10)
	_, err := s.Write("todo", []byte(`3`), &first, 10)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if ce.Current == nil || *ce.Current != second {
		t.Errorf("Current = %v, want %q", ce.Current, second)
	}
	if body, _, _ := s.Read("todo"); string(body) != `2` {
		t.Errorf("document = %q, want it untouched", body)
	}
}

func TestMatchingEtagWrites(t *testing.T) {
	s, _, _ := newStore(t)
	first := mustWrite(t, s, `1`, nil, 10)
	second := mustWrite(t, s, `2`, &first, 10)
	if second != Etag([]byte(`2`)) {
		t.Errorf("etag = %q, want etag of the new body", second)
	}
	if body, _, _ := s.Read("todo"); string(body) != `2` {
		t.Errorf("document = %q, want 2", body)
	}
}

func TestHistory(t *testing.T) {
	s, root, clock := newStore(t)
	first := mustWrite(t, s, `{"v": 1}`, nil, 2)

	// The old document moves to history, named by the replacement time.
	etag := mustWrite(t, s, `{"v": 2}`, &first, 2)
	want := "20261107T183512.000Z-" + first[:8] + ".json"
	if names := historyFiles(t, root); len(names) != 1 || names[0] != want {
		t.Fatalf("history = %v, want [%s]", names, want)
	}
	got, err := os.ReadFile(filepath.Join(root, "todo", "history", want))
	if err != nil || string(got) != `{"v": 1}` {
		t.Errorf("history bytes = %q, %v; want the replaced bytes", got, err)
	}

	// Three more replacements, a second apart; only the newest 2 stay.
	var etags []string
	for i := 3; i <= 5; i++ {
		*clock = clock.Add(time.Second)
		etags = append(etags, etag)
		etag = mustWrite(t, s, `{"v": `+string(rune('0'+i))+`}`, &etag, 2)
	}
	names := historyFiles(t, root)
	wantNames := []string{
		"20261107T183514.000Z-" + etags[1][:8] + ".json",
		"20261107T183515.000Z-" + etags[2][:8] + ".json",
	}
	if len(names) != 2 || names[0] != wantNames[0] || names[1] != wantNames[1] {
		t.Errorf("history = %v, want %v", names, wantNames)
	}
}

func TestZeroHistoryKeepsNone(t *testing.T) {
	s, root, _ := newStore(t)
	first := mustWrite(t, s, `1`, nil, 0)
	mustWrite(t, s, `2`, &first, 0)
	if names := historyFiles(t, root); len(names) != 0 {
		t.Errorf("history = %v, want none", names)
	}
}

func TestIdenticalRewriteInSameSecond(t *testing.T) {
	s, root, _ := newStore(t)
	first := mustWrite(t, s, `1`, nil, 5)
	mustWrite(t, s, `1`, &first, 5)
	mustWrite(t, s, `1`, &first, 5)
	if names := historyFiles(t, root); len(names) != 1 {
		t.Errorf("history = %v, want one file", names)
	}
}

func TestNotJSON(t *testing.T) {
	s, root, _ := newStore(t)
	first := mustWrite(t, s, `{"ok":true}`, nil, 10)
	_, err := s.Write("todo", []byte("not json"), &first, 10)
	if !errors.Is(err, ErrNotJSON) {
		t.Fatalf("err = %v, want ErrNotJSON", err)
	}
	body, etag, _ := s.Read("todo")
	if string(body) != `{"ok":true}` || *etag != first {
		t.Errorf("document = %q (%v), want it untouched", body, etag)
	}
	if names := historyFiles(t, root); len(names) != 0 {
		t.Errorf("history = %v, want none", names)
	}
}

func TestFailedWriteKeepsPreviousDocument(t *testing.T) {
	s, root, _ := newStore(t)
	first := mustWrite(t, s, `{"ok":true}`, nil, 10)
	// A read-only app directory makes the temp file creation fail.
	dir := filepath.Join(root, "todo")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil { // running as root
		f.Close()
		os.Remove(f.Name())
		t.Skip("directory permissions are not enforced")
	}
	if _, err := s.Write("todo", []byte(`{"ok":false}`), &first, 10); err == nil {
		t.Fatal("Write succeeded in a read-only directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, etag, _ := s.Read("todo")
	if string(body) != `{"ok":true}` || *etag != first {
		t.Errorf("document = %q, want the previous one", body)
	}
	if names := historyFiles(t, root); len(names) != 0 {
		t.Errorf("history = %v, want the failed write's link removed", names)
	}
}

// On a file system without hard links (exFAT, FAT, most SMB and CIFS mounts)
// history holds copies of the replaced bytes, and a failed write removes the
// copy it made.
func TestHistoryWithoutHardLinks(t *testing.T) {
	link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errors.ErrUnsupported}
	}
	t.Cleanup(func() { link = os.Link })

	s, root, clock := newStore(t)
	first := mustWrite(t, s, `{"v": 1}`, nil, 10)
	second := mustWrite(t, s, `{"v": 2}`, &first, 10)
	want := "20261107T183512.000Z-" + first[:8] + ".json"
	if names := historyFiles(t, root); len(names) != 1 || names[0] != want {
		t.Fatalf("history = %v, want [%s]", names, want)
	}
	if got, err := os.ReadFile(filepath.Join(root, "todo", "history", want)); err != nil || string(got) != `{"v": 1}` {
		t.Errorf("history bytes = %q, %v; want the replaced bytes", got, err)
	}

	// Every replaced version gets its copy, the same bytes at another point
	// included: v1, v2, v1.
	third := mustWrite(t, s, `{"v": 1}`, &second, 10)
	mustWrite(t, s, `{"v": 3}`, &third, 10)
	if names := historyFiles(t, root); len(names) != 3 {
		t.Errorf("history = %v, want three files", names)
	}

	// A write that fails after its copy is made removes the copy.
	*clock = clock.Add(time.Second)
	dir := filepath.Join(root, "todo")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil { // running as root
		f.Close()
		os.Remove(f.Name())
		t.Skip("directory permissions are not enforced")
	}
	current := Etag([]byte(`{"v": 3}`))
	if _, err := s.Write("todo", []byte(`{"v": 4}`), &current, 10); err == nil {
		t.Fatal("Write succeeded in a read-only directory")
	}
	if names := historyFiles(t, root); len(names) != 3 {
		t.Errorf("history = %v, want the failed write's copy removed", names)
	}
}

// Several replacements within one second keep their order: the newest
// versions stay, not the ones whose etag sorts last.
func TestHistoryKeepsTheNewestWithinOneSecond(t *testing.T) {
	s, root, _ := newStore(t)
	etag := mustWrite(t, s, `{"v": 1}`, nil, 2)
	for i := 2; i <= 6; i++ {
		etag = mustWrite(t, s, `{"v": `+string(rune('0'+i))+`}`, &etag, 2)
	}
	names := historyFiles(t, root)
	if len(names) != 2 {
		t.Fatalf("history = %v, want 2 files", names)
	}
	for i, want := range []string{`{"v": 4}`, `{"v": 5}`} {
		got, err := os.ReadFile(filepath.Join(root, "todo", "history", names[i]))
		if err != nil || string(got) != want {
			t.Errorf("history[%d] %s = %q, %v; want %s", i, names[i], got, err, want)
		}
	}
	if !strings.HasPrefix(names[0], "20261107T183512.003Z-") || !strings.HasPrefix(names[1], "20261107T183512.004Z-") {
		t.Errorf("history = %v, want stamps a millisecond apart", names)
	}
}

// Names from before milliseconds sort by their time among the new ones, and
// the oldest go first.
func TestHistoryWithOldNames(t *testing.T) {
	s, root, clock := newStore(t)
	ordered := func() []string { // the store's own order, not the names'
		t.Helper()
		names, err := s.historyNames("todo")
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	first := mustWrite(t, s, `{"v": 1}`, nil, 3)
	hdir := filepath.Join(root, "todo", "history")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"20261107T183510Z-aaaaaaaa.json", "20261107T183512Z-bbbbbbbb.json"} {
		if err := os.WriteFile(filepath.Join(hdir, name), []byte(`{"old": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Now is the second of the newer old name: the new entry goes after it.
	second := mustWrite(t, s, `{"v": 2}`, &first, 3)
	want := []string{
		"20261107T183510Z-aaaaaaaa.json",
		"20261107T183512Z-bbbbbbbb.json",
		"20261107T183512.001Z-" + first[:8] + ".json",
	}
	if names := ordered(); !slices.Equal(names, want) {
		t.Fatalf("history = %v, want %v", names, want)
	}
	*clock = clock.Add(time.Second)
	mustWrite(t, s, `{"v": 3}`, &second, 3)
	want = append(want[1:], "20261107T183513.000Z-"+second[:8]+".json")
	if names := ordered(); !slices.Equal(names, want) {
		t.Errorf("after pruning, history = %v, want %v", names, want)
	}
}

// A crash between the history link and the rename leaves the current
// document in history already; the next write adds no second copy of it.
func TestCrashLeftoverIsNotDuplicated(t *testing.T) {
	for _, hardLinks := range []bool{true, false} {
		t.Run(map[bool]string{true: "hard link", false: "copy"}[hardLinks], func(t *testing.T) {
			if !hardLinks {
				link = func(oldname, newname string) error {
					return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errors.ErrUnsupported}
				}
				t.Cleanup(func() { link = os.Link })
			}
			s, root, clock := newStore(t)
			first := mustWrite(t, s, `{"v": 1}`, nil, 10)
			hdir := filepath.Join(root, "todo", "history")
			if err := os.MkdirAll(hdir, 0o700); err != nil {
				t.Fatal(err)
			}
			leftover := filepath.Join(hdir, "20261107T183511Z-"+first[:8]+".json")
			if err := os.WriteFile(leftover, []byte(`{"v": 1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			*clock = clock.Add(time.Second)
			mustWrite(t, s, `{"v": 2}`, &first, 10)
			if names := historyFiles(t, root); len(names) != 1 {
				t.Errorf("history = %v, want only the leftover", names)
			}
		})
	}
}

func TestHandEditedFileChangesEtag(t *testing.T) {
	s, root, _ := newStore(t)
	mustWrite(t, s, `1`, nil, 10)
	edited := []byte(`{"edited":  true}`)
	if err := os.WriteFile(filepath.Join(root, "todo", "data.json"), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	body, etag, err := s.Read("todo")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, edited) || etag == nil || *etag != Etag(edited) {
		t.Errorf("Read = %q, %v; want the edited bytes and their etag", body, etag)
	}
}

func TestConcurrentWritersOneWins(t *testing.T) {
	s, _, _ := newStore(t)
	base := mustWrite(t, s, `0`, nil, 10)

	const n = 8
	var wg sync.WaitGroup
	etags := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			etags[i], errs[i] = s.Write("todo", []byte(`{"writer":`+string(rune('1'+i))+`}`), &base, 10)
		}()
	}
	wg.Wait()

	winner := ""
	for i := range errs {
		if errs[i] == nil {
			if winner != "" {
				t.Fatal("more than one writer succeeded")
			}
			winner = etags[i]
		}
	}
	if winner == "" {
		t.Fatal("no writer succeeded")
	}
	conflicts := 0
	for _, err := range errs {
		var ce *ConflictError
		if errors.As(err, &ce) {
			conflicts++
			if ce.Current == nil || *ce.Current != winner {
				t.Errorf("conflict Current = %v, want winner %q", ce.Current, winner)
			}
		} else if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

func TestStatAndDeleteApp(t *testing.T) {
	s, root, clock := newStore(t)
	st, err := s.Stat("todo")
	if err != nil || st.Exists || st.Versions != 0 {
		t.Errorf("Stat of empty app = %+v, %v", st, err)
	}
	first := mustWrite(t, s, `{"a":1}`, nil, 10)
	*clock = clock.Add(time.Second)
	mustWrite(t, s, `{"a":22}`, &first, 10)

	st, err = s.Stat("todo")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Size != int64(len(`{"a":22}`)) || st.Versions != 1 || st.Modified.IsZero() {
		t.Errorf("Stat = %+v", st)
	}

	if err := s.DeleteApp("todo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "todo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("apps/todo still exists: %v", err)
	}
	if body, etag, err := s.Read("todo"); body != nil || etag != nil || err != nil {
		t.Errorf("Read after delete = %q, %v, %v", body, etag, err)
	}
	if err := s.DeleteApp("todo"); err != nil {
		t.Errorf("DeleteApp of absent app: %v", err)
	}
}

func TestRejectsBadAppID(t *testing.T) {
	s, _, _ := newStore(t)
	for _, id := range []string{"../x", "", "A", "a/b"} {
		if _, _, err := s.Read(id); !errors.Is(err, ErrBadApp) {
			t.Errorf("Read(%q) err = %v, want ErrBadApp", id, err)
		}
		if _, err := s.Write(id, []byte(`1`), nil, 1); !errors.Is(err, ErrBadApp) {
			t.Errorf("Write(%q) err = %v, want ErrBadApp", id, err)
		}
		if _, err := s.Stat(id); !errors.Is(err, ErrBadApp) {
			t.Errorf("Stat(%q) err = %v, want ErrBadApp", id, err)
		}
		if err := s.DeleteApp(id); !errors.Is(err, ErrBadApp) {
			t.Errorf("DeleteApp(%q) err = %v, want ErrBadApp", id, err)
		}
	}
}
