// Package config holds the settings the user may change: the server's display
// name, its two ports and the data folder, stored in config.json in the
// server folder.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/FLighter7/workspace-server/internal/fsutil"
)

// Default ports, used when config.json is first created.
const (
	DefaultUDPPort  = 47801
	DefaultHTTPPort = 47800
)

const maxNameRunes = 64

// Config is the content of config.json.
type Config struct {
	Name     string `json:"name"`
	UDPPort  int    `json:"udpPort"`
	HTTPPort int    `json:"httpPort"`
	// DataDir is the absolute path of the folder that holds the apps' data
	// (its apps/ folder). Empty means the server folder itself.
	DataDir string `json:"dataDir,omitempty"`
}

// Load reads the config at path. When the file does not exist it is created
// with defaultName and the default ports. The name is stored trimmed. An
// invalid file is left untouched and reported with its path.
func Load(path, defaultName string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := &Config{Name: defaultName, UDPPort: DefaultUDPPort, HTTPPort: DefaultHTTPPort}
		if err := c.Save(path); err != nil {
			return nil, err
		}
		c.Name = strings.TrimSpace(c.Name)
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Name = strings.TrimSpace(c.Name)
	return &c, nil
}

// Save validates the config and writes it atomically to path. The name is
// written trimmed; c itself is not modified.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	out := *c
	out.Name = strings.TrimSpace(out.Name)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// Validate checks that the name is 1-64 characters after trimming, that both
// ports are in 1-65535, that the two ports differ, and that a data folder,
// if set, is an absolute path. It does not modify c.
func (c *Config) Validate() error {
	n := utf8.RuneCountInString(strings.TrimSpace(c.Name))
	if n < 1 || n > maxNameRunes {
		return fmt.Errorf("name must be 1-%d characters, got %d", maxNameRunes, n)
	}
	if c.UDPPort < 1 || c.UDPPort > 65535 {
		return fmt.Errorf("udpPort %d is out of range 1-65535", c.UDPPort)
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		return fmt.Errorf("httpPort %d is out of range 1-65535", c.HTTPPort)
	}
	if c.UDPPort == c.HTTPPort {
		return fmt.Errorf("udpPort and httpPort must differ, both are %d", c.UDPPort)
	}
	if c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("dataDir %q must be an absolute path", c.DataDir)
	}
	return nil
}

// WithOverrides returns a copy with the UDP and HTTP ports replaced by the
// non-zero arguments (0 means no override). The result is never saved.
func (c *Config) WithOverrides(udp, http int) Config {
	out := *c
	if udp != 0 {
		out.UDPPort = udp
	}
	if http != 0 {
		out.HTTPPort = http
	}
	return out
}
