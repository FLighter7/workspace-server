package apps

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sub", "apps.json")
}

func openFresh(t *testing.T) (*Registry, string) {
	t.Helper()
	path := newPath(t)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return r, path
}

func TestOpenWritesDefault(t *testing.T) {
	r, path := openFresh(t)
	want := []App{{ID: "todo", Name: "Todo", History: 10, MaxBytes: 67108864}}
	if got := r.List(); !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("apps.json not created: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.List(); !reflect.DeepEqual(got, want) {
		t.Errorf("reopened List() = %+v, want %+v", got, want)
	}
}

func TestValidID(t *testing.T) {
	valid := []string{"todo", "a", "my-app-2", strings.Repeat("a", 64)}
	invalid := []string{"", "Todo", "../x", "a/b", "a.b", "a b", "é", "a\n", strings.Repeat("a", 65)}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func TestAddValidates(t *testing.T) {
	r, path := openFresh(t)

	invalid := map[string]App{
		"empty id":       {ID: ""},
		"uppercase id":   {ID: "Notes"},
		"traversal id":   {ID: "../x"},
		"history high":   {ID: "n", History: 101},
		"history neg":    {ID: "n", History: -1},
		"maxBytes low":   {ID: "n", MaxBytes: 1023},
		"maxBytes high":  {ID: "n", MaxBytes: 1073741825},
		"maxBytes neg":   {ID: "n", MaxBytes: -5},
		"id and history": {ID: "N", History: 500},
	}
	for name, a := range invalid {
		if err := r.Add(a); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Add(%+v) = %v, want ErrInvalid", name, a, err)
		}
	}
	if err := r.Add(App{ID: "todo"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: got %v, want ErrExists", err)
	}
	if got := r.List(); len(got) != 1 {
		t.Fatalf("failed adds changed the list: %+v", got)
	}

	// Range edges are accepted.
	for _, a := range []App{
		{ID: "lo", History: 1, MaxBytes: 1024},
		{ID: "hi", History: 100, MaxBytes: 1073741824},
	} {
		if err := r.Add(a); err != nil {
			t.Errorf("Add(%+v) = %v, want nil", a, err)
		}
	}

	if err := r.Add(App{ID: "notes", Name: "Notes", History: 5, MaxBytes: 2048}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("notes")
	want := App{ID: "notes", Name: "Notes", History: 5, MaxBytes: 2048}
	if !ok || got != want {
		t.Errorf("Get(notes) after reopen = %+v, %v; want %+v", got, ok, want)
	}
}

func TestAddAppliesDefaults(t *testing.T) {
	r, path := openFresh(t)
	if err := r.Add(App{ID: "notes"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(App{ID: "blank", Name: "  \t "}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(App{ID: "padded", Name: "  Padded  "}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []App{
		{ID: "notes", Name: "notes", History: 10, MaxBytes: 67108864},
		{ID: "blank", Name: "blank", History: 10, MaxBytes: 67108864},
		{ID: "padded", Name: "Padded", History: 10, MaxBytes: 67108864},
	} {
		if got, ok := reopened.Get(want.ID); !ok || got != want {
			t.Errorf("Get(%s) = %+v, %v; want %+v", want.ID, got, ok, want)
		}
	}
}

func TestListSortedAndCopied(t *testing.T) {
	r, _ := openFresh(t)
	for _, id := range []string{"zeta", "alpha", "mid"} {
		if err := r.Add(App{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for _, a := range r.List() {
		ids = append(ids, a.ID)
	}
	if want := []string{"alpha", "mid", "todo", "zeta"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids %v, want %v", ids, want)
	}
	list := r.List()
	list[0].Name = "changed"
	if a, _ := r.Get("alpha"); a.Name == "changed" {
		t.Error("List returned the registry's own slice")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) = true")
	}
}

func TestRemove(t *testing.T) {
	r, path := openFresh(t)
	if err := r.Remove("todo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("todo"); ok {
		t.Error("todo still present")
	}
	if err := r.Remove("todo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Remove = %v, want ErrNotFound", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.List(); len(got) != 0 {
		t.Errorf("removal did not persist (an empty list must not be replaced by the default): %+v", got)
	}
}

func TestReloadIfChanged(t *testing.T) {
	r, path := openFresh(t)

	changed, err := r.ReloadIfChanged()
	if changed || err != nil {
		t.Fatalf("untouched file: changed=%v err=%v", changed, err)
	}

	hand := `{"apps":[{"id":"todo","name":"Todo","history":10,"maxBytes":67108864},{"id":"extra","name":"Extra","history":3,"maxBytes":4096}]}`
	if err := os.WriteFile(path, []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = r.ReloadIfChanged()
	if !changed || err != nil {
		t.Fatalf("hand edit: changed=%v err=%v", changed, err)
	}
	if got, ok := r.Get("extra"); !ok || got != (App{ID: "extra", Name: "Extra", History: 3, MaxBytes: 4096}) {
		t.Errorf("Get(extra) = %+v, %v", got, ok)
	}
	if changed, err = r.ReloadIfChanged(); changed || err != nil {
		t.Errorf("second call: changed=%v err=%v", changed, err)
	}

	if err := os.WriteFile(path, []byte(`{"apps": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = r.ReloadIfChanged()
	if changed || err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("invalid JSON: changed=%v err=%v, want an error naming %s", changed, err, path)
	}
	if len(r.List()) != 2 {
		t.Errorf("invalid file replaced the list: %+v", r.List())
	}
	if _, ok := r.Get("extra"); !ok {
		t.Error("last good list lost")
	}

	// Fixing the file is picked up.
	if err := os.WriteFile(path, []byte(`{"apps":[{"id":"only"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err = r.ReloadIfChanged(); !changed || err != nil {
		t.Fatalf("fixed file: changed=%v err=%v", changed, err)
	}
	want := []App{{ID: "only", Name: "only", History: 10, MaxBytes: 67108864}}
	if got := r.List(); !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %+v, want %+v (zero means default)", got, want)
	}
}

func TestReloadRejectsInvalidEntries(t *testing.T) {
	cases := map[string]string{
		"bad id":        `{"apps":[{"id":"Bad"}]}`,
		"duplicate":     `{"apps":[{"id":"a"},{"id":"a"}]}`,
		"history range": `{"apps":[{"id":"a","history":101}]}`,
		"negative":      `{"apps":[{"id":"a","maxBytes":-1}]}`,
		"maxBytes low":  `{"apps":[{"id":"a","maxBytes":10}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			r, path := openFresh(t)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			changed, err := r.ReloadIfChanged()
			if changed || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), path) {
				t.Errorf("changed=%v err=%v, want ErrInvalid naming the file", changed, err)
			}
			if _, ok := r.Get("todo"); !ok || len(r.List()) != 1 {
				t.Errorf("previous list not kept: %+v", r.List())
			}
			if _, err := Open(path); !errors.Is(err, ErrInvalid) {
				t.Errorf("Open: %v, want ErrInvalid", err)
			}
		})
	}
}

func TestOwnWritesAreNotChanges(t *testing.T) {
	r, _ := openFresh(t)
	if err := r.Add(App{ID: "notes"}); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReloadIfChanged(); changed || err != nil {
		t.Errorf("after Add: changed=%v err=%v", changed, err)
	}
	if err := r.Remove("notes"); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReloadIfChanged(); changed || err != nil {
		t.Errorf("after Remove: changed=%v err=%v", changed, err)
	}
}

func TestReloadMissingFileKeepsList(t *testing.T) {
	r, path := openFresh(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	changed, err := r.ReloadIfChanged()
	if changed || err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("changed=%v err=%v, want an error naming the file", changed, err)
	}
	if _, ok := r.Get("todo"); !ok {
		t.Error("list lost")
	}
}

func TestReloadSeesSameSizeEditWithNewTime(t *testing.T) {
	r, path := openFresh(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `"Todo"`, `"Tada"`, 1)
	if len(edited) != len(data) {
		t.Fatal("test edit changed the size")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReloadIfChanged(); !changed || err != nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if a, _ := r.Get("todo"); a.Name != "Tada" {
		t.Errorf("name %q, want Tada", a.Name)
	}
}

// handEdit writes content as a person would, with a time the registry
// cannot have seen.
func handEdit(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func ids(list []App) []string {
	var out []string
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

func TestAddKeepsAHandEdit(t *testing.T) {
	r, path := openFresh(t)
	handEdit(t, path, `{"apps":[{"id":"todo"},{"id":"extra"}]}`)
	if err := r.Add(App{ID: "notes"}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(again.List()); !reflect.DeepEqual(got, []string{"extra", "notes", "todo"}) {
		t.Errorf("the file lists %v, want the hand edit and the page's app", got)
	}
	if changed, err := r.ReloadIfChanged(); !changed || err != nil {
		t.Errorf("first reload after the merge: changed %v, %v; want true", changed, err)
	}
	if changed, err := r.ReloadIfChanged(); changed || err != nil {
		t.Errorf("second reload: changed %v, %v; want false", changed, err)
	}
}

func TestRemoveKeepsAHandEdit(t *testing.T) {
	r, path := openFresh(t)
	handEdit(t, path, `{"apps":[{"id":"todo"},{"id":"extra"}]}`)
	if err := r.Remove("todo"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(again.List()); !reflect.DeepEqual(got, []string{"extra"}) {
		t.Errorf("the file lists %v, want only the hand edit's extra", got)
	}

	// A hand edit that removed the app already leaves nothing to do.
	handEdit(t, path, `{"apps":[]}`)
	if err := r.Remove("extra"); err != nil {
		t.Errorf("Remove of an app the hand edit removed: %v", err)
	}
}

func TestAddRefusesAnInvalidHandEdit(t *testing.T) {
	r, path := openFresh(t)
	handEdit(t, path, "{")
	if err := r.Add(App{ID: "notes"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Add = %v, want ErrInvalid", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{" {
		t.Errorf("the hand edit was overwritten: %q", data)
	}
}

func TestUnknownKeysAreLogged(t *testing.T) {
	r, path := openFresh(t)
	var buf strings.Builder
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	handEdit(t, path, `{"apps":[{"id":"todo","Name":"Todo","histroy":3}],"extra":1}`)
	if changed, err := r.ReloadIfChanged(); !changed || err != nil {
		t.Fatalf("changed %v, %v", changed, err)
	}
	if a, _ := r.Get("todo"); a.History != DefaultHistory {
		t.Errorf("history %d, want the default", a.History)
	}
	out := buf.String()
	for _, want := range []string{path, `apps[0]: unknown key "histroy"`, `unknown key "extra"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, `"Name"`) {
		t.Errorf("log %q names a key that is read (JSON keys ignore case)", out)
	}
}

func TestOpenRejectsInvalidFile(t *testing.T) {
	path := newPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const junk = "{not json"
	if err := os.WriteFile(path, []byte(junk), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err == nil || r != nil {
		t.Fatalf("Open = %v, %v; want an error", r, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %s", err, path)
	}
	if data, _ := os.ReadFile(path); string(data) != junk {
		t.Errorf("invalid file was modified: %q", data)
	}
}
