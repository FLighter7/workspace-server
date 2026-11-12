package devices

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FLighter7/workspace-server/internal/apps"
)

// Server is how a device reaches another workspace-server: what a
// subordinate announced to this server, its main, about itself. It is a
// pairing code without the secret. Fingerprint is uppercase colon hex.
type Server struct {
	ServerID    string   `json:"serverId"`
	Name        string   `json:"name"`
	Password    string   `json:"password"`
	Fingerprint string   `json:"fingerprint"`
	Port        int      `json:"port"`
	Hosts       []string `json:"hosts"`
}

// MaxServerHosts bounds the hosts of an announced server.
const MaxServerHosts = 16

var (
	serverIDPattern    = regexp.MustCompile(`^[A-Za-z0-9]{12}$`)
	icePwdPattern      = regexp.MustCompile(`^[A-Za-z0-9+/]{22,256}$`)
	fingerprintPattern = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)
	// A host goes into a device's SDP answer as is: a .local name or an IPv4
	// address, and nothing that could end the line.
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)
	deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Validate reports what is wrong with s, nil when nothing is.
func (s Server) Validate() error {
	switch {
	case !serverIDPattern.MatchString(s.ServerID):
		return errors.New("serverId must be 12 characters of [A-Za-z0-9]")
	case strings.TrimSpace(s.Name) == "" || utf8.RuneCountInString(s.Name) > maxNameRunes:
		return fmt.Errorf("name must be 1 to %d characters", maxNameRunes)
	case !icePwdPattern.MatchString(s.Password):
		return errors.New("password must be 22 to 256 ICE characters")
	case !fingerprintPattern.MatchString(s.Fingerprint):
		return errors.New("fingerprint must be 32 bytes in uppercase colon hex")
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("port %d is out of range", s.Port)
	case len(s.Hosts) == 0 || len(s.Hosts) > MaxServerHosts:
		return fmt.Errorf("hosts must hold 1 to %d entries", MaxServerHosts)
	}
	for _, h := range s.Hosts {
		if !hostPattern.MatchString(h) {
			return fmt.Errorf("host %q is not a name or an address", h)
		}
	}
	return nil
}

func (s Server) equal(o Server) bool {
	return s.ServerID == o.ServerID && s.Name == o.Name && s.Password == o.Password &&
		s.Fingerprint == o.Fingerprint && s.Port == o.Port && slices.Equal(s.Hosts, o.Hosts)
}

// Entry is a device as a main tells its subordinates, and as a server that
// steps down hands its own over to the main it then follows: never its
// token, only the token's hash, with which a subordinate admits the device.
// A server-kind entry (another server of the family) has no app, and its
// Server once it announced itself. Name and PairedAt label the device on a
// server that adopts the entry as its own (see Merge).
type Entry struct {
	ID        string    `json:"id"`
	App       string    `json:"app"`
	Kind      string    `json:"kind,omitempty"`
	Name      string    `json:"name,omitempty"`
	TokenHash string    `json:"tokenHash"`
	PairedAt  time.Time `json:"pairedAt,omitzero"`
	Server    *Server   `json:"server,omitempty"`
}

// IsServer reports whether e is a server-kind device.
func (e Entry) IsServer() bool { return e.Kind == KindServer }

// Validate reports what is wrong with e, nil when nothing is.
func (e Entry) Validate() error {
	switch {
	case !deviceIDPattern.MatchString(e.ID):
		return fmt.Errorf("device id %q is not valid", e.ID)
	case e.Kind != "" && e.Kind != KindServer:
		return fmt.Errorf("unknown kind %q", e.Kind)
	case e.IsServer() && e.App != "":
		return fmt.Errorf("a server-kind device has no app, not %q", e.App)
	case !e.IsServer() && !apps.ValidID(e.App):
		return fmt.Errorf("app %q must match ^[a-z0-9-]{1,64}$", e.App)
	case !tokenHashPattern.MatchString(e.TokenHash):
		return errors.New("tokenHash must be 64 lowercase hex digits")
	case utf8.RuneCountInString(e.Name) > maxNameRunes:
		return fmt.Errorf("name must be at most %d characters", maxNameRunes)
	case e.Server != nil && !e.IsServer():
		return errors.New("only a server-kind device has a server")
	case e.Server != nil && e.Server.Validate() != nil:
		return fmt.Errorf("server: %v", e.Server.Validate())
	}
	return nil
}

// key is what tells two entries of one device apart: a new token or another
// kind is another admission; the labels are not.
func (e Entry) key() [4]string { return [4]string{e.ID, e.App, e.Kind, e.TokenHash} }

// SameAdmission reports whether a and b admit the same devices the same
// way: the same ids, apps, kinds and token hashes, in the same order.
func SameAdmission(a, b []Entry) bool {
	return slices.EqualFunc(a, b, func(x, y Entry) bool { return x.key() == y.key() })
}

// Same reports whether a and b are the same entries, labels and all.
func Same(a, b []Entry) bool {
	return slices.EqualFunc(a, b, func(x, y Entry) bool {
		return x.key() == y.key() && x.Name == y.Name && x.PairedAt.Equal(y.PairedAt) &&
			((x.Server == nil && y.Server == nil) || (x.Server != nil && y.Server != nil && x.Server.equal(*y.Server)))
	})
}

func entryOf(d Device) Entry {
	e := Entry{ID: d.ID, App: d.App, Kind: d.Kind, Name: d.Name, TokenHash: d.TokenHash, PairedAt: d.PairedAt}
	if d.Server != nil {
		s := *d.Server
		s.Hosts = slices.Clone(s.Hosts)
		e.Server = &s
	}
	return e
}

// Tombstone is a device removed from the family: by its id, which no device
// ever gets again, and when. A registry merged in later never brings it
// back.
type Tombstone struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// Validate reports what is wrong with t, nil when nothing is.
func (t Tombstone) Validate() error {
	if !deviceIDPattern.MatchString(t.ID) {
		return fmt.Errorf("removed device id %q is not valid", t.ID)
	}
	return nil
}

// MaxTombstones bounds the removed devices a registry keeps: the newest
// ones. A single user removes few devices; one removed before the newest
// MaxTombstones could come back with a registry merged in from a server
// that still held it.
const MaxTombstones = 1000

// buryLocked records id as removed now. The caller holds r.mu.
func (r *Registry) buryLocked(id string) {
	list := slices.DeleteFunc(slices.Clone(r.removed), func(t Tombstone) bool { return t.ID == id })
	r.removed = capTombstones(append(list, Tombstone{ID: id, At: r.now().UTC()}))
}

// capTombstones keeps the newest MaxTombstones of list, sorted by time and
// id.
func capTombstones(list []Tombstone) []Tombstone {
	slices.SortFunc(list, func(a, b Tombstone) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(list) > MaxTombstones {
		list = slices.Clone(list[len(list)-MaxTombstones:])
	}
	return list
}

// Entries returns this server's own devices, the registry a main sends its
// subordinates: devices of apps and server-kind devices alike, sorted by app
// and id (server-kind devices, which have no app, first). Devices from a
// main are left out.
func (r *Registry) Entries() []Entry {
	r.mu.Lock()
	out := make([]Entry, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, entryOf(d))
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Entry) int {
		if c := strings.Compare(a.App, b.App); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Removed returns the devices removed here, or on a main whose registry was
// merged in, oldest first.
func (r *Registry) Removed() []Tombstone {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.removed)
}

// Self names this server when a registry is merged in: its server id and,
// when it was a subordinate, its own server-kind device on its main. An
// entry for either is not taken.
type Self struct {
	ServerID string
	Device   string
}

func (s Self) is(e Entry) bool {
	return (s.Device != "" && e.ID == s.Device) || (e.IsServer() && e.Server != nil && e.Server.ServerID == s.ServerID)
}

// Merge takes list and removed, a registry of the family (a main's, which a
// subordinate becoming the main adopts, or the own registry of a main that
// steps down, which the new main takes over), into this server's own
// devices: the union of both, less every device removed on either side.
// Device ids are random and never reused, so a removal is final: an id
// removed on either side stays removed, and one paired on either side is
// kept. A device this server already has keeps its own entry, and so does a
// server it already has under another id; an entry for this server itself
// (self) is not taken. It returns the devices of this server's own that
// removed drops, whose sessions the caller closes. Entries that are not
// valid are left out.
func (r *Registry) Merge(list []Entry, removed []Tombstone, self Self) ([]Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gone := map[string]bool{}
	all := slices.Clone(r.removed)
	for _, t := range r.removed {
		gone[t.ID] = true
	}
	changed := false
	for _, t := range removed {
		if t.Validate() != nil || gone[t.ID] {
			continue
		}
		gone[t.ID] = true
		all = append(all, Tombstone{ID: t.ID, At: t.At.UTC()})
		changed = true
	}
	var dropped []Device
	kept := make([]Device, 0, len(r.devices)+len(list))
	for _, d := range r.devices {
		if gone[d.ID] {
			dropped = append(dropped, d)
			continue
		}
		kept = append(kept, d)
	}
	has := func(e Entry) bool {
		return slices.ContainsFunc(kept, func(d Device) bool {
			return d.ID == e.ID || (e.IsServer() && e.Server != nil && d.IsServer() && d.Server != nil && d.Server.ServerID == e.Server.ServerID)
		})
	}
	for _, e := range list {
		if e.Validate() != nil || gone[e.ID] || self.is(e) || has(e) {
			continue
		}
		d := Device{ID: e.ID, App: e.App, Kind: e.Kind, Name: cleanName(e.Name), TokenHash: e.TokenHash, PairedAt: e.PairedAt.UTC(), LastSeen: e.PairedAt.UTC()}
		if e.Server != nil {
			s := *e.Server
			s.Hosts = slices.Clone(s.Hosts)
			d.Server = &s
		}
		kept = append(kept, d)
		changed = true
	}
	if !changed && len(dropped) == 0 {
		return nil, nil
	}
	prevDevices, prevRemoved := r.devices, r.removed
	r.devices, r.removed = kept, capTombstones(all)
	if err := r.write(); err != nil {
		r.devices, r.removed = prevDevices, prevRemoved
		return nil, fmt.Errorf("write %s: %w", r.path, err)
	}
	for _, d := range dropped {
		delete(r.touched, d.ID)
	}
	return dropped, nil
}

// Clear forgets this server's own devices and removals, once they have been
// handed over to the main it now follows, whose registry then holds them:
// the server admits them on the main's word from then on (the replica).
func (r *Registry) Clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.devices) == 0 && len(r.removed) == 0 {
		return nil
	}
	prevDevices, prevRemoved := r.devices, r.removed
	r.devices, r.removed = nil, nil
	if err := r.write(); err != nil {
		r.devices, r.removed = prevDevices, prevRemoved
		return fmt.Errorf("write %s: %w", r.path, err)
	}
	clear(r.touched)
	return nil
}

// MintServer gives the server s a server-kind device here, with s as what it
// announced, and returns the device and its new token: the credential a new
// main hands to the main it replaces, which has none, so that it can link
// here. A server-kind device for s.ServerID already here gets a new token
// instead, so minting again never makes a second one.
func (r *Registry) MintServer(s Server) (Device, string, error) {
	if err := s.Validate(); err != nil {
		return Device{}, "", err
	}
	s.Hosts = slices.Clone(s.Hosts)
	token, err := newToken()
	if err != nil {
		return Device{}, "", err
	}
	at := r.now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.devices, func(d Device) bool {
		return d.IsServer() && d.Server != nil && d.Server.ServerID == s.ServerID
	})
	prevDevices := r.devices
	if i < 0 {
		r.devices = append(slices.Clone(r.devices), Device{
			ID: r.newID(), Kind: KindServer, Name: cleanName(s.Name), PairedAt: at, LastSeen: at,
		})
		i = len(r.devices) - 1
	} else {
		r.devices = slices.Clone(r.devices)
	}
	r.devices[i].TokenHash, r.devices[i].Server = hashToken(token), &s
	if err := r.write(); err != nil {
		r.devices = prevDevices
		return Device{}, "", fmt.Errorf("write %s: %w", r.path, err)
	}
	delete(r.touched, r.devices[i].ID)
	return r.devices[i], token, nil
}

// SetServer keeps s, which the server-kind device id announced, with the
// device, and writes the file when it changed. It reports whether it did.
func (r *Registry) SetServer(id string, s Server) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	s.Hosts = slices.Clone(s.Hosts)

	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.indexOf(id)
	switch {
	case i < 0:
		return false, fmt.Errorf("no device %q", id)
	case !r.devices[i].IsServer():
		return false, fmt.Errorf("device %q is not a server", id)
	case r.devices[i].Server != nil && r.devices[i].Server.equal(s):
		return false, nil
	}
	prev := r.devices[i].Server
	r.devices[i].Server = &s
	if err := r.write(); err != nil {
		r.devices[i].Server = prev
		return false, fmt.Errorf("write %s: %w", r.path, err)
	}
	return true, nil
}

// Subordinates returns what the server-kind devices announced, sorted by
// name and server id: the servers a main lists in every welcome. A
// server-kind device that has not announced itself yet is left out.
func (r *Registry) Subordinates() []Server {
	r.mu.Lock()
	out := []Server{}
	for _, d := range r.devices {
		if d.IsServer() && d.Server != nil {
			s := *d.Server
			s.Hosts = slices.Clone(s.Hosts)
			out = append(out, s)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Server) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ServerID, b.ServerID)
	})
	return out
}

// SetReplica replaces the devices this server admits on its main's word:
// the main's registry, as its link last received it. They are kept in
// memory only (the link keeps them on disk), are never listed and never
// written to devices.json, and a device of this server's own with the same
// id wins. A server-kind device among them (another server of the family)
// is admitted to link sessions only (see Authenticate). An entry that is not
// valid is left out.
func (r *Registry) SetReplica(entries []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replica = r.replica[:0:0]
	for _, e := range entries {
		if e.Validate() != nil {
			continue
		}
		d := Device{ID: e.ID, App: e.App, Kind: e.Kind, Name: e.Name, TokenHash: e.TokenHash, PairedAt: e.PairedAt, Replica: true}
		if e.Server != nil {
			s := *e.Server
			s.Hosts = slices.Clone(s.Hosts)
			d.Server = &s
		}
		r.replica = append(r.replica, d)
	}
}

// Replica returns the devices SetReplica gave, in its order.
func (r *Registry) Replica() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.replica))
	for _, d := range r.replica {
		out = append(out, entryOf(d))
	}
	return out
}

// replicaLocked returns the device of the replica with id. The caller holds
// r.mu.
func (r *Registry) replicaLocked(id string) (Device, bool) {
	for _, d := range r.replica {
		if d.ID == id {
			return d, true
		}
	}
	return Device{}, false
}
