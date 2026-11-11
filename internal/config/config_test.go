package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := Load(path, "my-host")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Name: "my-host", UDPPort: 47801, HTTPPort: 47800}
	if *c != want {
		t.Errorf("got %+v, want %+v", *c, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created: %v", err)
	}
	again, err := Load(path, "another-host")
	if err != nil {
		t.Fatal(err)
	}
	if *again != want {
		t.Errorf("reload got %+v, want %+v (default name must not replace the stored one)", *again, want)
	}
}

func TestLoadTrimsDefaultName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(path, "  host \n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "host" {
		t.Errorf("Name %q, want trimmed %q", c.Name, "host")
	}
}

func TestLoadRejectsInvalidDefaultName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := Load(path, "  "); err == nil {
		t.Fatal("want an error for a blank default name")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an invalid config must not be written")
	}
}

func TestSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(path, "my-host")
	if err != nil {
		t.Fatal(err)
	}
	c.Name = "  Desk "
	c.HTTPPort = 5000
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, "my-host")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Name: "Desk", UDPPort: 47801, HTTPPort: 5000}
	if *got != want {
		t.Errorf("got %+v, want %+v", *got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only config.json (no leftover temp files)", len(entries))
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Config{Name: "", UDPPort: 1, HTTPPort: 2}
	if err := c.Save(path); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an invalid config must not be written")
	}
}

func TestLoadInvalidFileNamesPath(t *testing.T) {
	cases := map[string]string{
		"bad json":    `{"name": `,
		"empty name":  `{"name":" ","udpPort":1,"httpPort":2}`,
		"equal ports": `{"name":"x","udpPort":9,"httpPort":9}`,
		"missing":     `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path, "host")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name %s", err, path)
			}
			got, _ := os.ReadFile(path)
			if string(got) != body {
				t.Errorf("an invalid file must be left untouched, now %q", got)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       Config
		wantErr bool
	}{
		{"valid", Config{"Desk", 47800, 47801, ""}, false},
		{"valid 64 runes", Config{strings.Repeat("é", 64), 1, 65535, ""}, false},
		{"valid padded", Config{"  Desk  ", 1, 2, ""}, false},
		{"empty name", Config{"", 47800, 47801, ""}, true},
		{"blank name", Config{" \t ", 47800, 47801, ""}, true},
		{"65 characters", Config{strings.Repeat("a", 65), 47800, 47801, ""}, true},
		{"65 runes", Config{strings.Repeat("é", 65), 47800, 47801, ""}, true},
		{"udp port 0", Config{"x", 0, 47801, ""}, true},
		{"http port 0", Config{"x", 47800, 0, ""}, true},
		{"port 70000", Config{"x", 70000, 47801, ""}, true},
		{"http port 70000", Config{"x", 47800, 70000, ""}, true},
		{"negative port", Config{"x", -1, 47801, ""}, true},
		{"equal ports", Config{"x", 5000, 5000, ""}, true},
		{"absolute data folder", Config{"x", 1, 2, filepath.Join(os.TempDir(), "data", "ws")}, false},
		{"relative data folder", Config{"x", 1, 2, "data/ws"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.c
			err := tt.c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(before, tt.c) {
				t.Errorf("Validate mutated the config: %+v -> %+v", before, tt.c)
			}
		})
	}
}

func TestWithOverridesDoesNotTouchFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(path, "host")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	o := c.WithOverrides(1234, 0)
	if o.UDPPort != 1234 || o.HTTPPort != 47800 || o.Name != "host" {
		t.Errorf("override udp only: got %+v", o)
	}
	o = c.WithOverrides(0, 4321)
	if o.UDPPort != 47801 || o.HTTPPort != 4321 {
		t.Errorf("override http only: got %+v", o)
	}
	o = c.WithOverrides(0, 0)
	if o != *c {
		t.Errorf("no override: got %+v, want %+v", o, *c)
	}
	if c.UDPPort != 47801 || c.HTTPPort != 47800 {
		t.Errorf("receiver changed: %+v", *c)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("file changed: %q -> %q", before, after)
	}
}

func TestDataDirRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	dataDir := filepath.Join(t.TempDir(), "data")
	c := Config{Name: "Desk", UDPPort: 1, HTTPPort: 2, DataDir: dataDir}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, "x")
	if err != nil {
		t.Fatal(err)
	}
	if *got != c {
		t.Errorf("got %+v, want %+v", *got, c)
	}
}

func TestEmptyDataDirIsOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{Name: "Desk", UDPPort: 1, HTTPPort: 2}).Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dataDir") {
		t.Errorf("config.json names an empty dataDir:\n%s", data)
	}
}
