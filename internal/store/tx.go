package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A transaction stages changes in .wss/staging/<tx>/: tx.json (txRecord) and
// one file per body, written as it arrives. Nothing in the app's set changes
// until Commit. A device has at most one open transaction per app; Begin with
// another id discards the one it had. A transaction lives StagingTTL after
// its last activity and survives a restart: received is saved as the body
// arrives (every persistEvery bytes and when a put's body stops), so a put
// cut off resumes from there.

// persistEvery is how many bytes of a body arrive between two saves of
// tx.json; a restart resumes the body from the last save.
var persistEvery int64 = 8 << 20

// ValidTx reports whether tx is a valid transaction id: 16 to 64 characters
// of [A-Za-z0-9_-], chosen by the client.
func ValidTx(tx string) bool {
	if len(tx) < 16 || len(tx) > 64 {
		return false
	}
	for i := 0; i < len(tx); i++ {
		if c := tx[i]; !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// opRecord is one staged change: a put of a body of Bytes bytes, of which
// Received have arrived into File, or a delete. IfEtag nil means the
// document must not exist. Etag is the body's, once it is complete.
type opRecord struct {
	Key      string  `json:"key"`
	Delete   bool    `json:"delete,omitempty"`
	IfEtag   *string `json:"ifEtag"`
	Bytes    int64   `json:"bytes"`
	Received int64   `json:"received"`
	Etag     string  `json:"etag,omitempty"`
	File     string  `json:"file,omitempty"`
}

// txRecord is tx.json.
type txRecord struct {
	Tx      string     `json:"tx"`
	Device  string     `json:"device"`
	Created time.Time  `json:"created"`
	Active  time.Time  `json:"active"` // the last activity
	Next    int        `json:"next"`   // the number of the next body file
	Ops     []opRecord `json:"ops"`
}

// txState is an open transaction in memory; the app's lock guards it.
type txState struct {
	rec   txRecord
	dir   string
	sinks map[string]*Sink // bodies being written, by key
	dead  bool             // committed, aborted, discarded or expired
}

func (t *txState) op(ks string) *opRecord {
	for i := range t.rec.Ops {
		if t.rec.Ops[i].Key == ks {
			return &t.rec.Ops[i]
		}
	}
	return nil
}

// kill ends the transaction's sinks; their writes fail from now on.
func (t *txState) kill() {
	for _, k := range t.sinks {
		k.kill()
	}
	t.sinks = map[string]*Sink{}
	t.dead = true
}

// Staged is a change a transaction holds, as Begin reports it for resuming:
// a put of Bytes bytes, of which Received have arrived, or a delete.
type Staged struct {
	Key      Key
	Delete   bool
	IfEtag   *string
	Bytes    int64
	Received int64
}

func stagedOf(t *txState) []Staged {
	out := make([]Staged, 0, len(t.rec.Ops))
	for _, op := range t.rec.Ops {
		k, _ := ParseKey(op.Key)
		out = append(out, Staged{Key: k, Delete: op.Delete, IfEtag: cloneEtag(op.IfEtag), Bytes: op.Bytes, Received: op.Received})
	}
	return out
}

func cloneEtag(e *string) *string {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// validBodyFile reports whether name is a body file name, "b<n>".
func validBodyFile(name string) bool {
	if len(name) < 2 || name[0] != 'b' {
		return false
	}
	_, err := strconv.Atoi(name[1:])
	return err == nil && !strings.ContainsAny(name[1:], "+-")
}

// loadTxs reads app's open transactions from its staging folder. A folder
// whose tx.json is missing or does not make sense is left out, for sweep to
// remove. What a body file holds bounds what counts as received: a crash
// may have lost the end of a write that tx.json already counted.
func (s *Store) loadTxs(app string) (map[string]*txState, error) {
	txs := map[string]*txState{}
	dir := filepath.Join(s.wss(app), stagingName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return txs, nil
	}
	if err != nil {
		return nil, err
	}
next:
	for _, e := range entries {
		if !e.IsDir() || !ValidTx(e.Name()) {
			continue
		}
		tdir := filepath.Join(dir, e.Name())
		data, err := readFile(filepath.Join(tdir, txName))
		if err != nil {
			continue
		}
		var rec txRecord
		if json.Unmarshal(data, &rec) != nil || rec.Tx != e.Name() || rec.Device == "" {
			continue
		}
		for i := range rec.Ops {
			op := &rec.Ops[i]
			if _, ok := ParseKey(op.Key); !ok {
				continue next
			}
			if op.Delete {
				continue
			}
			if !validBodyFile(op.File) || op.Bytes < 0 {
				continue next
			}
			var size int64
			if fi, err := os.Lstat(filepath.Join(tdir, op.File)); err == nil {
				size = fi.Size()
			}
			op.Received = max(min(op.Received, size, op.Bytes), 0)
			if op.Received < op.Bytes {
				op.Etag = ""
			}
		}
		if rec.Ops == nil {
			rec.Ops = []opRecord{}
		}
		txs[rec.Tx] = &txState{rec: rec, dir: tdir, sinks: map[string]*Sink{}}
	}
	return txs, nil
}

func (s *Store) persistTx(t *txState) error {
	data, err := json.MarshalIndent(t.rec, "", "\t")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(t.dir, txName), append(data, '\n'), 0o600)
}

// txOf returns device's open transaction tx.
func (s *Store) txOf(a *appState, device, tx string) (*txState, error) {
	t := a.txs[tx]
	if t == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoTransaction, tx)
	}
	if t.rec.Device != device {
		return nil, fmt.Errorf("%w: %q belongs to another device", ErrNoTransaction, tx)
	}
	return t, nil
}

// discard ends t and removes its staging folder.
func (s *Store) discard(a *appState, t *txState) error {
	t.kill()
	delete(a.txs, t.rec.Tx)
	return retryBusy(func() error { return removeAll(t.dir) })
}

// sweep discards a's transactions that expired by now, and removes whatever
// else lies in the staging folder: the leftovers of a crash, or of a removal
// that failed.
func (s *Store) sweep(app string, a *appState, now time.Time) error {
	var errs []error
	for _, t := range a.txs {
		if now.Sub(t.rec.Active) >= StagingTTL {
			if err := s.discard(a, t); err != nil {
				errs = append(errs, err)
			}
		}
	}
	dir := filepath.Join(s.wss(app), stagingName)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if a.txs[e.Name()] != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := retryBusy(func() error { return removeAll(path) }); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// projected returns the size app's set would have after t, with extra in
// place of t's change to the same key: a put frees what the document it
// replaces takes, and a delete frees all of it.
func (s *Store) projected(a *appState, t *txState, extra *opRecord) int64 {
	size := a.idx.Bytes
	add := func(op opRecord) {
		cur := a.idx.Docs[op.Key].Bytes // 0 for a new document
		if op.Delete {
			size -= cur
		} else {
			size += op.Bytes - cur
		}
	}
	for _, op := range t.rec.Ops {
		if extra == nil || op.Key != extra.Key {
			add(op)
		}
	}
	if extra != nil {
		add(*extra)
	}
	return size
}

// Begin opens the transaction tx of device on app, or returns to it, and
// returns what it has staged, for resuming. A device's other open
// transaction on the app is discarded. A transaction of that id that belongs
// to another device is ErrNoTransaction.
func (s *Store) Begin(app, device, tx string) ([]Staged, error) {
	if !ValidTx(tx) {
		return nil, fmt.Errorf("%w: %q", ErrBadTx, tx)
	}
	if device == "" {
		return nil, fmt.Errorf("%w: no device", ErrNoTransaction)
	}
	var staged []Staged
	err := s.do(app, func(a *appState) error {
		now := s.now()
		if t := a.txs[tx]; t != nil {
			if t.rec.Device != device {
				return fmt.Errorf("%w: %q belongs to another device", ErrNoTransaction, tx)
			}
			t.rec.Active = now
			if err := s.persistTx(t); err != nil {
				return err
			}
			staged = stagedOf(t)
			return nil
		}
		for _, t := range a.txs {
			if t.rec.Device == device {
				// A folder that will not go is a stray for sweep.
				_ = s.discard(a, t)
			}
		}
		t := &txState{
			rec:   txRecord{Tx: tx, Device: device, Created: now.UTC(), Active: now.UTC(), Ops: []opRecord{}},
			dir:   filepath.Join(s.wss(app), stagingName, tx),
			sinks: map[string]*Sink{},
		}
		if err := s.persistTx(t); err != nil {
			_ = removeAll(t.dir)
			return err
		}
		a.txs[tx] = t
		staged = []Staged{}
		return nil
	})
	return staged, err
}

// Put stages a body of size bytes for document k in device's transaction tx
// and returns the Sink to write it into, from offset on. ifEtag is the etag
// the document must have at Commit, nil if it must not exist.
//
// offset 0 starts the body afresh. A larger offset resumes a body of the
// same size staged before, from at most what it received (Staged.Received);
// offset == bytes changes only ifEtag of a complete body, and its Sink takes
// no bytes. Anything else is ErrBadBody. A body that would take the set past
// the app's quota is ErrQuota at once, before any byte.
//
// The caller writes the body into the Sink without the app's lock, and
// closes it: the body is synced to disk when it is complete.
func (s *Store) Put(app, device, tx string, k Key, ifEtag *string, bytes, offset int64) (*Sink, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if bytes < 0 || offset < 0 || offset > bytes {
		return nil, fmt.Errorf("%w: offset %d of %d bytes", ErrBadBody, offset, bytes)
	}
	var sink *Sink
	err := s.do(app, func(a *appState) error {
		t, err := s.txOf(a, device, tx)
		if err != nil {
			return err
		}
		ks := k.String()
		if old := t.sinks[ks]; old != nil {
			old.kill()
			delete(t.sinks, ks)
		}
		op := t.op(ks)
		if offset > 0 && (op == nil || op.Delete || op.Bytes != bytes || offset > op.Received) {
			return fmt.Errorf("%w: %s has not received %d of %d bytes", ErrBadBody, ks, offset, bytes)
		}
		next := opRecord{Key: ks, IfEtag: cloneEtag(ifEtag), Bytes: bytes}
		if s.projected(a, t, &next) > s.quotaOf(app) {
			return ErrQuota
		}
		if offset > 0 && offset == bytes {
			op.IfEtag = next.IfEtag
			t.rec.Active = s.now()
			if err := s.persistTx(t); err != nil {
				return err
			}
			sink = &Sink{bytes: bytes, received: bytes, done: true}
			return nil
		}

		var f *os.File
		h := sha256.New()
		if offset == 0 {
			next.File = "b" + strconv.Itoa(t.rec.Next)
			if err := os.MkdirAll(t.dir, 0o700); err != nil {
				return err
			}
			f, err = os.OpenFile(filepath.Join(t.dir, next.File), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			t.rec.Next++
			if op != nil && op.File != "" {
				_ = removeFile(filepath.Join(t.dir, op.File))
			}
			if op == nil {
				t.rec.Ops = append(t.rec.Ops, next)
				op = &t.rec.Ops[len(t.rec.Ops)-1]
			} else {
				*op = next
			}
		} else {
			// Resume: what lies past offset goes, and the hash starts over
			// the part kept.
			f, err = os.OpenFile(filepath.Join(t.dir, op.File), os.O_RDWR, 0)
			if err != nil {
				return err
			}
			n, err := resumeAt(f, h, offset)
			if err != nil || n < offset {
				_ = f.Close()
				if err == nil {
					op.Received, op.Etag = n, ""
					_ = s.persistTx(t)
					err = fmt.Errorf("%w: %s holds %d bytes only", ErrBadBody, ks, n)
				}
				return err
			}
			op.IfEtag, op.Received, op.Etag = next.IfEtag, offset, ""
		}
		t.rec.Active = s.now()
		if bytes == 0 {
			op.Etag = hex.EncodeToString(h.Sum(nil))
		}
		if err := s.persistTx(t); err != nil {
			_ = f.Close()
			return err
		}
		if bytes == 0 {
			err := f.Sync()
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			sink = &Sink{done: true}
			return err
		}
		sink = &Sink{s: s, a: a, t: t, key: ks, bytes: bytes, f: f, h: h, received: offset, saved: offset}
		t.sinks[ks] = sink
		return nil
	})
	return sink, err
}

// resumeAt cuts f to offset, hashes what it keeps into h and leaves f at its
// end. It returns how many bytes it hashed, fewer than offset if f is
// shorter.
func resumeAt(f *os.File, h hash.Hash, offset int64) (int64, error) {
	if fi, err := f.Stat(); err != nil {
		return 0, err
	} else if fi.Size() > offset {
		if err := f.Truncate(offset); err != nil {
			return 0, err
		}
	}
	n, err := io.CopyN(h, f, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// Delete stages the removal of document k in device's transaction tx, in
// place of anything staged for k before. ifEtag is the etag the document
// must have at Commit; nil means it must not exist.
func (s *Store) Delete(app, device, tx string, k Key, ifEtag *string) error {
	if err := k.Validate(); err != nil {
		return err
	}
	return s.do(app, func(a *appState) error {
		t, err := s.txOf(a, device, tx)
		if err != nil {
			return err
		}
		ks := k.String()
		if old := t.sinks[ks]; old != nil {
			old.kill()
			delete(t.sinks, ks)
		}
		next := opRecord{Key: ks, Delete: true, IfEtag: cloneEtag(ifEtag)}
		if op := t.op(ks); op != nil {
			if op.File != "" {
				_ = removeFile(filepath.Join(t.dir, op.File))
			}
			*op = next
		} else {
			t.rec.Ops = append(t.rec.Ops, next)
		}
		t.rec.Active = s.now()
		return s.persistTx(t)
	})
}

// Abort discards device's transaction tx.
func (s *Store) Abort(app, device, tx string) error {
	return s.do(app, func(a *appState) error {
		t, err := s.txOf(a, device, tx)
		if err != nil {
			return err
		}
		return s.discard(a, t)
	})
}

// Sink receives a body that Put staged. It is written without the app's
// lock, by one writer at a time, and closed when the body stops, complete or
// not. Once Received reaches the size Put was given, the body is synced to
// disk and the Sink closes itself.
type Sink struct {
	s     *Store
	a     *appState
	t     *txState
	key   string
	bytes int64

	mu       sync.Mutex // guards the rest; taken after the app's lock, never before
	f        *os.File
	h        hash.Hash
	received int64
	saved    int64 // received as tx.json last recorded it
	done     bool  // complete or closed
	err      error // why the transaction ended under it
}

// Received returns how many bytes of the body have arrived.
func (k *Sink) Received() int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.received
}

// Complete reports whether the whole body has arrived.
func (k *Sink) Complete() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.received == k.bytes && k.err == nil
}

// Write appends p to the body. Bytes past the announced size are
// ErrBadBody, and nothing of p is written; a write after the transaction
// ended is ErrNoTransaction.
func (k *Sink) Write(p []byte) (int, error) {
	k.mu.Lock()
	switch {
	case k.err != nil:
		k.mu.Unlock()
		return 0, k.err
	case int64(len(p)) > k.bytes-k.received:
		k.mu.Unlock()
		return 0, fmt.Errorf("%w: more than %d bytes", ErrBadBody, k.bytes)
	case len(p) == 0:
		k.mu.Unlock()
		return 0, nil
	case k.done:
		k.mu.Unlock()
		return 0, fmt.Errorf("%w: the body is closed", ErrBadBody)
	}
	n, err := k.f.Write(p)
	k.h.Write(p[:n])
	k.received += int64(n)
	received := k.received
	if err != nil {
		k.mu.Unlock()
		return n, err
	}
	if received == k.bytes {
		// Complete: synced once, here, outside the app's lock.
		err := k.f.Sync()
		if cerr := k.f.Close(); err == nil {
			err = cerr
		}
		k.f, k.done = nil, true
		etag := hex.EncodeToString(k.h.Sum(nil))
		k.mu.Unlock()
		if err != nil {
			k.record(0, "", true) // the body cannot be trusted: resume from 0
			return n, err
		}
		return n, k.record(received, etag, true)
	}
	persist := received-k.saved >= persistEvery
	k.mu.Unlock()
	if persist {
		return n, k.record(received, "", false)
	}
	return n, nil
}

// Close ends the body where it stands. The transaction keeps what arrived,
// and a later Put resumes it. Closing a complete body does nothing.
func (k *Sink) Close() error {
	k.mu.Lock()
	if k.done || k.err != nil {
		k.mu.Unlock()
		return nil
	}
	k.done = true
	err := k.f.Close()
	k.f = nil
	received := k.received
	k.mu.Unlock()
	if rerr := k.record(received, "", true); err == nil {
		err = rerr
	}
	return err
}

// record saves how much of the body has arrived, and its etag once
// complete, in tx.json; final also lets the transaction forget the Sink. It
// does nothing for a Sink that its transaction no longer has.
func (k *Sink) record(received int64, etag string, final bool) error {
	k.a.mu.Lock()
	defer k.a.mu.Unlock()
	if k.t.dead || k.t.sinks[k.key] != k {
		return fmt.Errorf("%w: the transaction ended", ErrNoTransaction)
	}
	op := k.t.op(k.key)
	if op == nil {
		return fmt.Errorf("%w: the transaction ended", ErrNoTransaction)
	}
	op.Received, op.Etag = received, etag
	k.t.rec.Active = k.s.now()
	if final {
		delete(k.t.sinks, k.key)
	}
	err := k.s.persistTx(k.t)
	k.mu.Lock()
	k.saved = received
	k.mu.Unlock()
	return err
}

// kill ends the Sink because its transaction ended. The caller holds the
// app's lock.
func (k *Sink) kill() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.f != nil {
		_ = k.f.Close()
		k.f = nil
	}
	if !k.done {
		k.err = fmt.Errorf("%w: the transaction ended", ErrNoTransaction)
	}
	k.done = true
}
