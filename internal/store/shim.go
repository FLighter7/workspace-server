package store

// The shim between stage 1 and stage 2 of the files plan; stage 2 removes
// this file. Until the protocol speaks of documents and transactions, the
// hub, the link and the web page keep the one JSON document per app they
// knew, which is the document data.json at the app's root (DataKey) of the
// new store: Read and Write below. The etag they pass around is that
// document's etag, not the set's revision.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/FLighter7/workspace-server/internal/fsutil"
)

// ErrNotJSON is returned by Write when the body is not valid JSON. Stage 2
// removes it with the shim: the store does not look into documents.
var ErrNotJSON = errors.New("store: body is not valid JSON")

// DataKey is the document that Read and Write work on: data.json at the
// app's root.
var DataKey = Key{ID: "data", Type: "json"}

// Read reconciles the app's set with its files and returns the document
// DataKey and its etag, or nil, nil, nil when there is none.
func (s *Store) Read(app string) ([]byte, *string, error) {
	var body []byte
	var etag *string
	err := s.do(app, func(a *appState) error {
		if _, err := s.reconcile(app, a); err != nil {
			return err
		}
		ks := DataKey.String()
		if _, ok := a.idx.Docs[ks]; !ok {
			return nil
		}
		b, err := fsutil.ReadFileShared(s.docPath(app, ks))
		if errors.Is(err, fs.ErrNotExist) {
			_, err = s.reconcile(app, a)
			return err
		}
		if err != nil {
			return err
		}
		e := Etag(b)
		body, etag = b, &e
		if a.idx.Docs[ks].Etag != e { // changed by hand since the reconcile
			_, err = s.reconcile(app, a)
		}
		return err
	})
	return body, etag, err
}

// Write replaces the document DataKey with body, as a transaction of its own,
// and returns the new etag. body must be valid JSON. ifEtag must be the
// document's etag, or nil when it does not exist; otherwise Write returns a
// *ConflictError and changes nothing. A set that would exceed the app's quota
// is ErrQuota.
func (s *Store) Write(app string, body []byte, ifEtag *string) (string, error) {
	if !json.Valid(body) {
		return "", ErrNotJSON
	}
	var etag string
	err := s.do(app, func(a *appState) error {
		if _, err := s.reconcile(app, a); err != nil {
			return err
		}
		ks := DataKey.String()
		cur, ok := a.idx.Docs[ks]
		if !(ok && ifEtag != nil && cur.Etag == *ifEtag || !ok && ifEtag == nil) {
			var current *string
			if ok {
				current = cloneEtag(&cur.Etag)
			}
			return &ConflictError{Revision: cloneEtag(a.idx.Revision), Docs: []DocEtag{{Key: DataKey, Etag: current}}}
		}
		if err := s.checkTargets(app, a, []string{ks}); err != nil {
			return err
		}
		if s.overQuota(app, a, a.idx.Bytes-cur.Bytes+int64(len(body))) {
			return ErrQuota
		}

		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		name := "shim-" + hex.EncodeToString(id[:])
		dir := filepath.Join(s.wss(app), stagingName, name)
		if err := writeBody(filepath.Join(dir, "b0"), body); err != nil {
			_ = removeAll(dir)
			return err
		}
		etag = Etag(body)
		j := journal{Tx: "shim", Staging: name, At: s.now().UTC(), Ops: []journalOp{{
			Key: ks, From: stagingName + "/" + name + "/b0", Etag: etag, Bytes: int64(len(body)),
		}}}
		if err := s.commitJournal(app, a, &j, nil); err != nil {
			if !a.journal { // before the commit point
				_ = removeAll(dir)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return etag, nil
}

// writeBody writes a staged body and syncs it.
func writeBody(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
