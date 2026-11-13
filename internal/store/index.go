package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"time"
)

// docEntry is a document in the index: its etag, and the size and
// modification time (Unix nanoseconds) of its file when the etag was taken,
// by which Reconcile tells that the file changed.
type docEntry struct {
	Etag  string `json:"etag"`
	Bytes int64  `json:"bytes"`
	Mtime int64  `json:"mtime"`
	Racy  bool   `json:"racy,omitempty"` // hashed too soon after its mtime to trust it
}

// tombstone records a deleted document.
type tombstone struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}

// index is .wss/index.json. Docs is keyed by Key.String(). Bytes, the set's
// size, and Revision are derived from Docs; they are stored for whoever
// reads the file, and derived again on load. Removed is oldest first.
type index struct {
	Revision *string             `json:"revision"`
	Bytes    int64               `json:"bytes"`
	Written  time.Time           `json:"written,omitzero"` // the last change of the revision
	Docs     map[string]docEntry `json:"docs"`
	Removed  []tombstone         `json:"removed"`
}

func newIndex() *index { return &index{Docs: map[string]docEntry{}, Removed: []tombstone{}} }

// loadIndex reads the index at path. A missing file is an empty index, and
// onDisk false. Entries whose key is not valid are dropped, so a hand edit
// cannot smuggle a path out of the app's folder.
func loadIndex(path string) (idx *index, onDisk bool, err error) {
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newIndex(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	idx = newIndex()
	if err := json.Unmarshal(data, idx); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if idx.Docs == nil {
		idx.Docs = map[string]docEntry{}
	}
	for ks := range idx.Docs {
		if _, ok := ParseKey(ks); !ok {
			delete(idx.Docs, ks)
		}
	}
	idx.Removed = slices.DeleteFunc(idx.Removed, func(t tombstone) bool {
		_, ok := ParseKey(t.Key)
		return !ok
	})
	if idx.Removed == nil {
		idx.Removed = []tombstone{}
	}
	idx.derive()
	return idx, true, nil
}

// derive computes Bytes and Revision from Docs.
func (x *index) derive() {
	x.Bytes = 0
	for _, e := range x.Docs {
		x.Bytes += e.Bytes
	}
	x.Revision = revisionOf(x.Docs)
}

// revisionOf returns the revision of a set: the lowercase hex SHA-256 of the
// lines "<key>\x00<etag>\n", sorted by key, byte for byte; nil for an empty
// set. Two servers holding the same files have the same revision.
func revisionOf(docs map[string]docEntry) *string {
	if len(docs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(docs))
	for ks := range docs {
		keys = append(keys, ks)
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, ks := range keys {
		h.Write([]byte(ks))
		h.Write([]byte{0})
		h.Write([]byte(docs[ks].Etag))
		h.Write([]byte{'\n'})
	}
	rev := hex.EncodeToString(h.Sum(nil))
	return &rev
}

// put records the document ks; a tombstone of it goes.
func (x *index) put(ks string, e docEntry) {
	x.Docs[ks] = e
	x.dropTombstone(ks)
}

// remove forgets the document ks, if the index has it, and leaves its
// tombstone, keeping the newest MaxTombstones.
func (x *index) remove(ks string, at time.Time) {
	if _, ok := x.Docs[ks]; !ok {
		return
	}
	delete(x.Docs, ks)
	x.dropTombstone(ks)
	x.Removed = append(x.Removed, tombstone{Key: ks, At: at.UTC()})
	if over := len(x.Removed) - MaxTombstones; over > 0 {
		x.Removed = slices.Delete(x.Removed, 0, over)
	}
}

func (x *index) dropTombstone(ks string) {
	x.Removed = slices.DeleteFunc(x.Removed, func(t tombstone) bool { return t.Key == ks })
}

// finish derives Bytes and Revision after a change and reports whether the
// revision changed; if it did, Written becomes now.
func (x *index) finish(now time.Time) bool {
	before := x.Revision
	x.derive()
	if sameEtag(before, x.Revision) {
		return false
	}
	x.Written = now.UTC()
	return true
}

func sameEtag(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// empty reports whether the index holds nothing worth a file.
func (x *index) empty() bool { return len(x.Docs) == 0 && len(x.Removed) == 0 }

// save writes the index of app to its .wss folder, creating the folder.
func (s *Store) save(app string, a *appState) error {
	data, err := json.MarshalIndent(a.idx, "", "\t")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.wss(app), indexName), append(data, '\n'), 0o600); err != nil {
		return err
	}
	a.onDisk = true
	return nil
}
