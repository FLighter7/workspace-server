package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/fsutil"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// state is upstream.json: the main this server is linked to, the
// server-kind device it is there, per app the etag both sides last agreed
// on, and the main's device registry as it last came.
type state struct {
	ServerID    string    `json:"serverId"`
	Name        string    `json:"name"`
	Password    string    `json:"password"`
	Fingerprint string    `json:"fingerprint"` // uppercase colon hex
	Port        int       `json:"port"`
	Hosts       []string  `json:"hosts"` // from the code, then from each welcome
	Device      string    `json:"device"`
	Token       string    `json:"token"`
	LinkedAt    time.Time `json:"linkedAt"`
	// Apps holds, per app synced so far, the etag on which this server and
	// the main last agreed. An app missing here has never been agreed on;
	// one with a null base was agreed to have no document.
	Apps map[string]appState `json:"apps"`
	// Devices is the main's device registry, which this server admits by
	// (hashes only, as the main sent them).
	Devices []devices.Entry `json:"devices,omitempty"`
}

type appState struct {
	Base *string `json:"base"`
}

var (
	serverIDPattern    = regexp.MustCompile(`^[A-Za-z0-9]{12}$`)
	etagPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fingerprintPattern = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)
)

func (s *state) target() wsclient.Target {
	return wsclient.Target{Password: s.Password, Fingerprint: s.Fingerprint, Port: s.Port, Hosts: append([]string(nil), s.Hosts...)}
}

// clone returns a deep copy of s.
func (s *state) clone() *state {
	c := *s
	c.Hosts = append([]string(nil), s.Hosts...)
	c.Apps = make(map[string]appState, len(s.Apps))
	for id, a := range s.Apps {
		c.Apps[id] = a
	}
	c.Devices = append([]devices.Entry(nil), s.Devices...)
	return &c
}

// load reads upstream.json. A missing file is nil: the server is a main. A
// file that does not read or is not valid is an error, and is left
// untouched, like the other registry files.
func load(path string) (*state, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Apps == nil {
		s.Apps = map[string]appState{}
	}
	s.LinkedAt = s.LinkedAt.UTC()
	return &s, nil
}

func (s *state) validate() error {
	switch {
	case !serverIDPattern.MatchString(s.ServerID):
		return errors.New("serverId must be 12 characters of [A-Za-z0-9]")
	case len(s.Password) < 22:
		return errors.New("password is too short")
	case !fingerprintPattern.MatchString(s.Fingerprint):
		return errors.New("fingerprint must be 32 bytes in uppercase colon hex")
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("port %d is out of range", s.Port)
	case len(s.Hosts) == 0:
		return errors.New("hosts is empty")
	case s.Device == "" || s.Token == "":
		return errors.New("device and token must be set")
	}
	for id, a := range s.Apps {
		if !apps.ValidID(id) {
			return fmt.Errorf("apps: %q is not a valid app id", id)
		}
		if a.Base != nil && !etagPattern.MatchString(*a.Base) {
			return fmt.Errorf("apps: %q: base must be 64 lowercase hex digits or null", id)
		}
	}
	for i, d := range s.Devices {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("devices[%d]: %w", i, err)
		}
	}
	return nil
}

// save writes s to path atomically, readable by its owner only: it holds the
// token.
func (s *state) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// removeFile deletes path; a missing file is not an error.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
