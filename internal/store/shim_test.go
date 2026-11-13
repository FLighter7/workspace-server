package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShimReadWrite(t *testing.T) {
	e := newEnv(t)
	body, etag, err := e.s.Read("todo")
	if body != nil || etag != nil || err != nil {
		t.Fatalf("Read of nothing = %q, %v, %v", body, etag, err)
	}
	e1, err := e.s.Write("todo", []byte(`{"v":1}`), nil)
	if err != nil || e1 != etagOf(`{"v":1}`) {
		t.Fatalf("Write = %q, %v", e1, err)
	}
	body, etag, err = e.s.Read("todo")
	if string(body) != `{"v":1}` || etag == nil || *etag != e1 || err != nil {
		t.Fatalf("Read = %q, %v, %v", body, etag, err)
	}
	// The document is data.json in the set, with a revision of its own.
	wantDocs(t, e.docs("todo"), map[string]string{"data.json": `{"v":1}`})
	if rev, _ := e.s.Head("todo"); rev == nil || *rev == e1 {
		t.Errorf("revision %v", rev)
	}

	var conflict *ConflictError
	for name, ifEtag := range map[string]*string{"nil over a document": nil, "a stale etag": ptr(etagOf("x"))} {
		if _, err := e.s.Write("todo", []byte(`{"v":2}`), ifEtag); !errors.As(err, &conflict) || *conflict.Current(DataKey) != e1 {
			t.Errorf("%s: Write = %v, want a conflict naming %s", name, err, e1[:8])
		}
	}
	if _, err := e.s.Write("todo", []byte(`{"v":`), &e1); !errors.Is(err, ErrNotJSON) {
		t.Errorf("Write of bad JSON = %v, want ErrNotJSON", err)
	}
	e2, err := e.s.Write("todo", []byte(`{"v":2}`), &e1)
	if err != nil {
		t.Fatal(err)
	}
	if body, _, _ := e.s.Read("todo"); string(body) != `{"v":2}` {
		t.Errorf("Read = %q", body)
	}
	// No staging is left behind.
	if entries, _ := os.ReadDir(filepath.Join(e.root, "todo", ".wss", stagingName)); len(entries) != 0 {
		t.Errorf("staging holds %d entries", len(entries))
	}

	// A data.json put there by hand (the old layout, or a person) is read.
	e.writeFile("todo", "data.json", `{"hand":true}`)
	body, etag, _ = e.s.Read("todo")
	if string(body) != `{"hand":true}` || *etag != etagOf(`{"hand":true}`) {
		t.Errorf("Read after a hand edit = %q", body)
	}
	if _, err := e.s.Write("todo", []byte(`{}`), &e2); !errors.As(err, &conflict) {
		t.Errorf("Write over a hand edit = %v, want a conflict", err)
	}
}

func TestShimQuota(t *testing.T) {
	e := newEnv(t)
	e.quota = 1 << 20
	e.writeFile("todo", "other.bin", strings.Repeat("o", 600<<10))
	jsonString := func(n int) []byte { return []byte(`"` + strings.Repeat("a", n-2) + `"`) }
	if _, err := e.s.Write("todo", jsonString(500<<10), nil); !errors.Is(err, ErrQuota) {
		t.Fatalf("Write past the quota = %v, want ErrQuota", err)
	}
	e1, err := e.s.Write("todo", jsonString(300<<10), nil)
	if err != nil {
		t.Fatalf("Write that fits: %v", err)
	}
	// Replacing data.json frees its size.
	if _, err := e.s.Write("todo", jsonString(400<<10), &e1); err != nil {
		t.Fatalf("Write of a replacement that fits: %v", err)
	}
}

func TestShimCrash(t *testing.T) {
	e := newEnv(t)
	e1, _ := e.s.Write("todo", []byte(`{"v":1}`), nil)
	failRename := true
	inject(t, func(op, path string) bool { return failRename && op == "rename" })
	if _, err := e.s.Write("todo", []byte(`{"v":2}`), &e1); !errors.Is(err, errInjected) {
		t.Fatalf("Write = %v, want the injected failure", err)
	}
	failRename = false
	e.restart()
	if body, _, _ := e.s.Read("todo"); string(body) != `{"v":2}` {
		t.Errorf("after the restart: %q, want the committed write", body)
	}
	// Failing before the commit point leaves no stray.
	inject(t, func(op, path string) bool { return op == "write" && filepath.Base(path) == journalName })
	if _, err := e.s.Write("todo", []byte(`{"v":3}`), ptr(etagOf(`{"v":2}`))); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.root, "todo", ".wss", stagingName)); len(entries) != 0 {
		t.Errorf("staging holds %d entries", len(entries))
	}
}
