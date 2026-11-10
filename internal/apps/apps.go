// Package apps keeps the list of apps this server serves, stored in
// apps.json. The list decides which apps may pair and where their data lives.
package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/fsutil"
)

// Defaults and limits for an app's settings.
const (
	DefaultHistory  = 10
	DefaultMaxBytes = int64(67108864)

	minHistory  = 1
	maxHistory  = 100
	minMaxBytes = int64(1024)
	maxMaxBytes = int64(1073741824)

	maxIDLen = 64
)

// Errors returned by the registry. They are wrapped with the reason, so test
// them with errors.Is.
var (
	ErrExists   = errors.New("app already exists")
	ErrNotFound = errors.New("app not found")
	ErrInvalid  = errors.New("invalid app")
)

// App is one served app. A zero History or MaxBytes reads as the default and
// a blank Name as the id.
type App struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	History  int    `json:"history"`
	MaxBytes int64  `json:"maxBytes"`
}

type file struct {
	Apps []App `json:"apps"`
}

// ValidID reports whether id matches ^[a-z0-9-]{1,64}$. The id becomes a path
// segment, so this is an allowlist.
func ValidID(id string) bool {
	if len(id) < 1 || len(id) > maxIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// normalize applies the defaults and then validates the ranges.
func normalize(a App) (App, error) {
	if !ValidID(a.ID) {
		return App{}, fmt.Errorf("%w: id %q must match ^[a-z0-9-]{1,64}$", ErrInvalid, a.ID)
	}
	if a.History == 0 {
		a.History = DefaultHistory
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = DefaultMaxBytes
	}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		a.Name = a.ID
	}
	if a.History < minHistory || a.History > maxHistory {
		return App{}, fmt.Errorf("%w: history %d is out of range %d-%d", ErrInvalid, a.History, minHistory, maxHistory)
	}
	if a.MaxBytes < minMaxBytes || a.MaxBytes > maxMaxBytes {
		return App{}, fmt.Errorf("%w: maxBytes %d is out of range %d-%d", ErrInvalid, a.MaxBytes, minMaxBytes, maxMaxBytes)
	}
	return a, nil
}

// stamp identifies a version of the file on disk.
type stamp struct {
	modTime time.Time
	size    int64
}

func stampOf(fi os.FileInfo) stamp { return stamp{fi.ModTime(), fi.Size()} }

func (s stamp) equal(o stamp) bool { return s.size == o.size && s.modTime.Equal(o.modTime) }

// Registry is the list of apps backed by an apps.json file. It is safe for
// concurrent use.
type Registry struct {
	path string

	mu     sync.Mutex
	apps   map[string]App
	seen   stamp // the file as last read or written by this registry
	merged bool  // Add or Remove took in a hand edit; ReloadIfChanged reports it
}

// Open loads the registry from path. When the file does not exist it is
// created with the default "todo" app. An invalid file is left untouched and
// reported with its path.
func Open(path string) (*Registry, error) {
	r := &Registry{path: path}
	list, st, err := r.read()
	if errors.Is(err, os.ErrNotExist) {
		def := []App{{ID: "todo", Name: "Todo", History: DefaultHistory, MaxBytes: DefaultMaxBytes}}
		r.apps = index(def)
		if err := r.write(); err != nil {
			return nil, err
		}
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	r.apps = index(list)
	r.seen = st
	return r, nil
}

func index(list []App) map[string]App {
	m := make(map[string]App, len(list))
	for _, a := range list {
		m[a.ID] = a
	}
	return m
}

// read parses and validates the file. The stamp comes from the same open
// file as the bytes, so an edit made after the read is never mistaken for
// the content that was read. A missing file yields an error satisfying
// errors.Is(err, os.ErrNotExist).
func (r *Registry) read() ([]App, stamp, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, stamp{}, fmt.Errorf("read %s: %w", r.path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, stamp{}, fmt.Errorf("read %s: %w", r.path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, stamp{}, fmt.Errorf("read %s: %w", r.path, err)
	}
	var doc file
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, stamp{}, fmt.Errorf("parse %s: %w", r.path, err)
	}
	r.logUnknownKeys(data)
	seen := make(map[string]bool, len(doc.Apps))
	list := make([]App, 0, len(doc.Apps))
	for i, a := range doc.Apps {
		n, err := normalize(a)
		if err != nil {
			return nil, stamp{}, fmt.Errorf("%s: apps[%d]: %w", r.path, i, err)
		}
		if seen[n.ID] {
			return nil, stamp{}, fmt.Errorf("%s: apps[%d]: %w: duplicate id %q", r.path, i, ErrInvalid, n.ID)
		}
		seen[n.ID] = true
		list = append(list, n)
	}
	return list, stampOf(fi), nil
}

// knownKeys are the fields of the file and of an app. Unknown keys are
// logged, not refused: a file written by a later version must still load.
var knownKeys = map[string][]string{
	"":    {"apps"},
	"app": {"id", "name", "history", "maxBytes"},
}

// logUnknownKeys logs every key of data that the registry does not read,
// such as a misspelt "histroy", which would otherwise give the default
// without a word. JSON field names match without regard to case, as
// encoding/json does.
func (r *Registry) logUnknownKeys(data []byte) {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return
	}
	known := func(kind, key string) bool {
		for _, k := range knownKeys[kind] {
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}
	for _, key := range sortedKeys(top) {
		if !known("", key) {
			log.Printf("%s: unknown key %q ignored", r.path, key)
		}
	}
	var doc struct {
		Apps []map[string]json.RawMessage `json:"apps"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return
	}
	for i, a := range doc.Apps {
		for _, key := range sortedKeys(a) {
			if !known("app", key) {
				log.Printf("%s: apps[%d]: unknown key %q ignored", r.path, i, key)
			}
		}
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// catchUp takes in an edit of the file that the watcher has not read yet,
// so that a change made on the page applies to the file as it is now
// instead of writing back the list read before. A missing file is left to
// write, which recreates it. The caller holds r.mu.
func (r *Registry) catchUp() error {
	fi, err := os.Stat(r.path)
	if err != nil || stampOf(fi).equal(r.seen) {
		return nil
	}
	list, st, err := r.read()
	if err != nil {
		return err
	}
	r.apps, r.seen, r.merged = index(list), st, true
	return nil
}

func (r *Registry) sorted() []App {
	list := make([]App, 0, len(r.apps))
	for _, a := range r.apps {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// write saves the current list atomically and remembers the file's new
// stamp, so the registry's own writes do not later count as changes. The
// stamp is the new file's own, taken before the rename: an edit right after
// the write is still seen. The caller holds r.mu or has not yet shared r.
func (r *Registry) write() error {
	data, err := json.MarshalIndent(file{Apps: r.sorted()}, "", "  ")
	if err != nil {
		return err
	}
	fi, err := fsutil.WriteFileAtomicStat(r.path, append(data, '\n'), 0o644)
	if err != nil {
		return fmt.Errorf("write %s: %w", r.path, err)
	}
	r.seen = stampOf(fi)
	return nil
}

// List returns the apps sorted by id.
func (r *Registry) List() []App {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sorted()
}

// Get returns the app with the given id.
func (r *Registry) Get(id string) (App, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.apps[id]
	return a, ok
}

// Add applies the defaults to a (zero History, zero MaxBytes, blank Name),
// validates it and stores it, in the file as it is now: a hand edit not yet
// read is kept. It returns ErrInvalid (an invalid hand edit included) or
// ErrExists.
func (r *Registry) Add(a App) error {
	n, err := normalize(a)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.catchUp(); err != nil {
		return fmt.Errorf("%w: fix %s first: %v", ErrInvalid, filepath.Base(r.path), err)
	}
	if _, ok := r.apps[n.ID]; ok {
		return fmt.Errorf("%w: %q", ErrExists, n.ID)
	}
	r.apps[n.ID] = n
	if err := r.write(); err != nil {
		delete(r.apps, n.ID)
		return err
	}
	return nil
}

// Remove deletes the app with the given id, from the file as it is now: a
// hand edit not yet read is kept, and one that already removed the app
// leaves nothing to do. An invalid hand edit is replaced by the list known
// before, since the app's data may already be gone. It returns ErrNotFound
// when the registry knows no such app. The app's documents are not touched.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.apps[id]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err := r.catchUp(); err != nil {
		log.Printf("%v; writing the list known before without %q", err, id)
	}
	old, ok := r.apps[id]
	if !ok {
		return nil // the hand edit removed it already
	}
	delete(r.apps, id)
	if err := r.write(); err != nil {
		r.apps[id] = old
		return err
	}
	return nil
}

// ReloadIfChanged rereads the file when its modification time or size differs
// from what the registry last read or wrote, and reports whether the list was
// replaced, by this reread or by a hand edit Add or Remove took in. When the
// file cannot be read or is invalid it returns an error naming the file and
// keeps the previous list; the error is returned again on every call until
// the file is fixed.
func (r *Registry) ReloadIfChanged() (changed bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	merged := r.merged
	r.merged = false
	fi, err := os.Stat(r.path)
	if err != nil {
		return merged, fmt.Errorf("stat %s: %w", r.path, err)
	}
	if stampOf(fi).equal(r.seen) {
		return merged, nil
	}
	list, st, err := r.read()
	if err != nil {
		return merged, err
	}
	r.apps = index(list)
	r.seen = st
	return true, nil
}
