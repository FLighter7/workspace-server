package devices

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
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

// Entry is a device of an app as a main tells its subordinates: never its
// token, only the token's hash. A subordinate admits the device with it.
type Entry struct {
	ID        string `json:"id"`
	App       string `json:"app"`
	TokenHash string `json:"tokenHash"`
}

// Validate reports what is wrong with e, nil when nothing is.
func (e Entry) Validate() error {
	switch {
	case !deviceIDPattern.MatchString(e.ID):
		return fmt.Errorf("device id %q is not valid", e.ID)
	case !apps.ValidID(e.App):
		return fmt.Errorf("app %q must match ^[a-z0-9-]{1,64}$", e.App)
	case !tokenHashPattern.MatchString(e.TokenHash):
		return errors.New("tokenHash must be 64 lowercase hex digits")
	}
	return nil
}

// Entries returns this server's own devices of apps, the registry a main
// sends its subordinates, sorted by app and id. Server-kind devices and
// devices from a main are left out.
func (r *Registry) Entries() []Entry {
	r.mu.Lock()
	out := make([]Entry, 0, len(r.devices))
	for _, d := range r.devices {
		if !d.IsServer() {
			out = append(out, Entry{ID: d.ID, App: d.App, TokenHash: d.TokenHash})
		}
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
// id wins. An entry that is not valid is left out.
func (r *Registry) SetReplica(entries []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replica = r.replica[:0:0]
	for _, e := range entries {
		if e.Validate() != nil {
			continue
		}
		r.replica = append(r.replica, Device{ID: e.ID, App: e.App, TokenHash: e.TokenHash, Replica: true})
	}
}

// Replica returns the devices SetReplica gave, in its order.
func (r *Registry) Replica() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.replica))
	for _, d := range r.replica {
		out = append(out, Entry{ID: d.ID, App: d.App, TokenHash: d.TokenHash})
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
