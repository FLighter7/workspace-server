// Package devices keeps the registry of paired devices, stored in
// devices.json. A device holds a token; the registry keeps only its SHA-256
// hash, so a copy of the file cannot be used to connect.
package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/fsutil"
	"github.com/FLighter7/workspace-server/internal/identity"
)

const (
	idLen         = 16
	tokenBytes    = 32
	maxNameRunes  = 64
	defaultName   = "Device"
	touchInterval = time.Minute
)

// KindServer is the kind of a device that is another workspace-server: a
// subordinate linked to this one. It is admitted to every app this server
// lists, and its App is empty. A device of any other kind (Kind empty) is
// admitted to its App only.
const KindServer = "server"

// Device is one paired device. TokenHash is the lowercase hex SHA-256 of the
// device's token. A server-kind device (Kind KindServer) has no App; Server
// is what it announced about itself, once it has. Replica marks a device of
// this server's main, which this server admits on the main's word (see
// SetReplica); it is never written to the file.
type Device struct {
	ID        string    `json:"id"`
	App       string    `json:"app"`
	Kind      string    `json:"kind,omitempty"`
	Name      string    `json:"name"`
	TokenHash string    `json:"tokenHash"`
	PairedAt  time.Time `json:"pairedAt"`
	LastSeen  time.Time `json:"lastSeen"`
	Server    *Server   `json:"server,omitempty"`
	Replica   bool      `json:"-"`
}

// IsServer reports whether d is a server-kind device.
func (d Device) IsServer() bool { return d.Kind == KindServer }

var tokenHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type file struct {
	Devices []Device    `json:"devices"`
	Removed []Tombstone `json:"removed,omitempty"`
}

// Registry is the set of paired devices. It is safe for concurrent use.
type Registry struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	devices []Device
	removed []Tombstone // the devices removed here or on the family's main; see Merge
	replica []Device    // the main's devices, on a subordinate; see SetReplica
	// touched records when Touch last wrote the file for a device. Writes made
	// by other operations are not recorded here.
	touched map[string]time.Time
}

// Open reads the registry at path. A missing file is an empty registry (the
// file is created on the first change); an unreadable or invalid file is an
// error and is left untouched: an entry with an empty or repeated id, an
// unknown kind, an invalid app id (a server-kind entry has none), a
// tokenHash that is not 64 lowercase hex digits, or a server that is not
// valid or not a server-kind device's, or a removed entry with an id that is
// not valid. Times are read into UTC. now supplies the clock for PairedAt, LastSeen and write
// throttling.
func Open(path string, now func() time.Time) (*Registry, error) {
	r := &Registry{path: path, now: now, touched: map[string]time.Time{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	for i := range f.Devices {
		d := &f.Devices[i]
		switch {
		case d.ID == "":
			return nil, fmt.Errorf("%s: devices[%d]: empty id", path, i)
		case seen[d.ID]:
			return nil, fmt.Errorf("%s: devices[%d]: id %q is used twice", path, i, d.ID)
		case d.Kind != "" && d.Kind != KindServer:
			return nil, fmt.Errorf("%s: devices[%d]: unknown kind %q", path, i, d.Kind)
		case d.IsServer() && d.App != "":
			return nil, fmt.Errorf("%s: devices[%d]: a server-kind device has no app, not %q", path, i, d.App)
		case !d.IsServer() && !apps.ValidID(d.App):
			return nil, fmt.Errorf("%s: devices[%d]: app %q must match ^[a-z0-9-]{1,64}$", path, i, d.App)
		case !tokenHashPattern.MatchString(d.TokenHash):
			return nil, fmt.Errorf("%s: devices[%d]: tokenHash must be 64 lowercase hex digits", path, i)
		case d.Server != nil && !d.IsServer():
			return nil, fmt.Errorf("%s: devices[%d]: only a server-kind device has a server", path, i)
		case d.Server != nil && d.Server.Validate() != nil:
			return nil, fmt.Errorf("%s: devices[%d]: server: %v", path, i, d.Server.Validate())
		}
		seen[d.ID] = true
		d.PairedAt, d.LastSeen = d.PairedAt.UTC(), d.LastSeen.UTC()
	}
	for i := range f.Removed {
		t := &f.Removed[i]
		if !deviceIDPattern.MatchString(t.ID) {
			return nil, fmt.Errorf("%s: removed[%d]: id %q is not valid", path, i, t.ID)
		}
		t.At = t.At.UTC()
	}
	r.devices, r.removed = f.Devices, f.Removed

	return r, nil
}

// Pair registers a new device for app and returns it with its token. The
// token is returned only here; it cannot be recovered later. The name is
// trimmed and cut to 64 runes, and an empty name becomes "Device".
func (r *Registry) Pair(app, name string) (Device, string, error) {
	return r.pair("", app, name)
}

// PairServer registers a new server-kind device, admitted to every app, and
// returns it with its token, as Pair does.
func (r *Registry) PairServer(name string) (Device, string, error) {
	return r.pair(KindServer, "", name)
}

func (r *Registry) pair(kind, app, name string) (Device, string, error) {
	token, err := newToken()
	if err != nil {
		return Device{}, "", err
	}
	at := r.now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	d := Device{
		ID:        r.newID(),
		App:       app,
		Kind:      kind,
		Name:      cleanName(name),
		TokenHash: hashToken(token),
		PairedAt:  at,
		LastSeen:  at,
	}
	r.devices = append(r.devices, d)
	if err := r.write(); err != nil {
		r.devices = r.devices[:len(r.devices)-1]
		return Device{}, "", fmt.Errorf("write %s: %w", r.path, err)
	}
	delete(r.touched, d.ID)

	return d, token, nil
}

// Authenticate reports whether token belongs to device id of app. A
// server-kind device matches any app, and app "" too: it is admitted to
// every app. A device of the replica (SetReplica) is checked the same way,
// against the hash its main sent: the token is 32 random bytes, so its
// SHA-256 alone proves it, with no salt. A server-kind device of the replica
// (another server of the family) matches app "" only: it may reach this
// server's link sessions, to tell it of a new main, and nothing else. It
// does not update LastSeen; call Touch for that.
func (r *Registry) Authenticate(app, id, token string) (Device, bool) {
	got := hashToken(token)

	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.getLocked(id)
	if !ok || (d.App != app && !d.IsServer()) || (d.Replica && d.IsServer() && app != "") {
		return Device{}, false
	}
	if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(got)) != 1 {
		return Device{}, false
	}
	return d, true
}

// Touch sets LastSeen of device id to now. The file is written only if Touch
// has not tried to write it for this device within the last minute, so a
// write that fails is retried, and logged, at most once a minute; an
// unwritten LastSeen is saved with the next write of any kind. An unknown id
// is ignored.
func (r *Registry) Touch(id string) {
	now := r.now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.indexOf(id)
	if i < 0 {
		return
	}
	r.devices[i].LastSeen = now
	if last, ok := r.touched[id]; ok && now.Sub(last) < touchInterval {
		return
	}
	r.touched[id] = now
	if err := r.write(); err != nil {
		log.Printf("devices: save last-seen time: %v", err)
	}
}

// Remove deletes device id and reports whether it existed. The device's token
// stops working at once even if the file cannot be written. The id is kept
// as removed (see Merge), so that a registry merged in later does not bring
// the device back. A device of the replica is not this server's to remove:
// it goes when the main's registry drops it.
func (r *Registry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.indexOf(id)
	if i < 0 {
		return false
	}
	r.devices = append(r.devices[:i:i], r.devices[i+1:]...)
	r.buryLocked(id)
	delete(r.touched, id)
	if err := r.write(); err != nil {
		log.Printf("devices: write %s after removing a device: %v", r.path, err)
	}

	return true
}

// RemoveApp deletes every device of app and returns them, so the caller can
// close their sessions.
func (r *Registry) RemoveApp(app string) []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed, kept []Device
	for _, d := range r.devices {
		if d.App == app {
			removed = append(removed, d)
			r.buryLocked(d.ID)
			delete(r.touched, d.ID)
		} else {
			kept = append(kept, d)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	r.devices = kept
	if err := r.write(); err != nil {
		log.Printf("devices: write %s after removing app %q: %v", r.path, app, err)
	}

	return removed
}

// Get returns device id: this server's own, or else one of the replica.
func (r *Registry) Get(id string) (Device, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLocked(id)
}

// getLocked returns device id: this server's own first, then the replica's.
// The caller holds r.mu.
func (r *Registry) getLocked(id string) (Device, bool) {
	if i := r.indexOf(id); i >= 0 {
		return r.devices[i], true
	}
	return r.replicaLocked(id)
}

// List returns a copy of this server's own devices, sorted by app, then
// name, then id. Server-kind devices, which have no app, come first. The
// replica is not listed.
func (r *Registry) List() []Device {
	r.mu.Lock()
	out := append([]Device(nil), r.devices...)
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.App != b.App {
			return a.App < b.App
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})

	return out
}

func (r *Registry) indexOf(id string) int {
	for i, d := range r.devices {
		if d.ID == id {
			return i
		}
	}
	return -1
}

// newID returns an id not used by any device. The caller holds r.mu.
func (r *Registry) newID() string {
	for {
		id := identity.RandomString(idLen)
		if r.indexOf(id) < 0 {
			return id
		}
	}
}

// write saves the current devices. The caller holds r.mu.
func (r *Registry) write() error {
	list := r.devices
	if list == nil {
		list = []Device{}
	}
	data, err := json.MarshalIndent(file{Devices: list, Removed: r.removed}, "", "  ")
	if err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(r.path, append(data, '\n'), 0o600)
}

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > maxNameRunes {
		name = string(runes[:maxNameRunes])
	}
	if name == "" {
		return defaultName
	}

	return name
}
