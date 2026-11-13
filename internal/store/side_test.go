package store

import (
	"errors"
	"io/fs"
	"testing"
)

// RevisionOf gives the revision Head reports for the same documents.
func TestRevisionOf(t *testing.T) {
	e := newEnv(t)
	c := e.commitAll("todo", map[string]string{"data.json": "{}", "photos/2026/a.jpg": "jpeg"})
	got := RevisionOf(map[string]string{"data.json": etagOf("{}"), "photos/2026/a.jpg": etagOf("jpeg")})
	if got == nil || c.Revision == nil || *got != *c.Revision {
		t.Errorf("RevisionOf = %v, the store's revision %v", got, c.Revision)
	}
	if RevisionOf(nil) != nil || RevisionOf(map[string]string{}) != nil {
		t.Error("the revision of an empty set is not nil")
	}
}

// Side files live in the app's .wss folder, never under the store's own
// names, and go with the app.
func TestSideFiles(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.ReadSide("todo", "link.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadSide of a missing file = %v, want ErrNotExist", err)
	}
	if err := e.s.WriteSide("todo", "link.json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.ReadSide("todo", "link.json"); err != nil || string(got) != `{"a":1}` {
		t.Errorf("ReadSide = %q, %v", got, err)
	}
	for _, name := range []string{"index.json", "journal.json", "tx.json", "../x.json", "Link.json", "link.txt", "staging"} {
		if err := e.s.WriteSide("todo", name, []byte("{}")); err == nil {
			t.Errorf("WriteSide(%q) was allowed", name)
		}
	}
	// A side file is no document, and the set does not change.
	if docs := e.docs("todo"); len(docs) != 0 {
		t.Errorf("the set holds %v", docs)
	}
	if err := e.s.RemoveSide("todo", "link.json"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveSide("todo", "link.json"); err != nil {
		t.Errorf("removing a missing side file: %v", err)
	}
	if err := e.s.WriteSide("todo", "link.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteApp("todo"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ReadSide("todo", "link.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the side file outlived its app: %v", err)
	}
}
