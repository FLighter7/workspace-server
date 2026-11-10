package identity

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

var uuidLocal = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.local$`)

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	a, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.ServerID) != 12 {
		t.Errorf("ServerID %q: want 12 characters", a.ServerID)
	}
	if len(a.ICEPwd) != 24 {
		t.Errorf("ICEPwd %q: want 24 characters", a.ICEPwd)
	}
	if !uuidLocal.MatchString(a.MDNSName) {
		t.Errorf("MDNSName %q is not <uuid v4>.local", a.MDNSName)
	}
	cert, err := a.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if !cert.Expires().After(time.Now().AddDate(19, 0, 0)) {
		t.Errorf("certificate expires %s, want more than 19 years away", cert.Expires())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", info.Mode().Perm())
		}
	}

	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if *a != *b {
		t.Errorf("second call regenerated the identity:\n%+v\n%+v", a, b)
	}

	hex, err := b.FingerprintHex()
	if err != nil {
		t.Fatal(err)
	}
	if len(hex) != 95 || hex != strings.ToUpper(hex) || strings.Count(hex, ":") != 31 {
		t.Errorf("FingerprintHex %q: want 95 uppercase characters, colon-separated", hex)
	}
}

func TestFingerprintMatchesHex(t *testing.T) {
	id, err := LoadOrCreate(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	fp, err := id.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	hex, err := id.FingerprintHex()
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, b := range fp {
		parts = append(parts, fmt.Sprintf("%02X", b))
	}
	if got := strings.Join(parts, ":"); got != hex {
		t.Errorf("Fingerprint %s != FingerprintHex %s", got, hex)
	}
}

// The fingerprint is the SHA-256 of the certificate's DER, computed here
// without Pion.
func TestFingerprintIsTheCertificateHash(t *testing.T) {
	id, err := LoadOrCreate(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	fp, err := id.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	rest := []byte(id.CertPEM)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			t.Fatal("no CERTIFICATE block in certPem")
		}
		if b.Type == "CERTIFICATE" {
			if sha256.Sum256(b.Bytes) != fp {
				t.Errorf("Fingerprint %x is not the SHA-256 of the certificate", fp)
			}
			return
		}
	}
}

func TestLoadOrCreateRejectsDamagedFields(t *testing.T) {
	good := filepath.Join(t.TempDir(), "identity.json")
	id, err := LoadOrCreate(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, field string
		damage      func(*Identity)
	}{
		{"empty serverId", "serverId", func(i *Identity) { i.ServerID = "" }},
		{"short serverId", "serverId", func(i *Identity) { i.ServerID = "abc" }},
		{"serverId with a dash", "serverId", func(i *Identity) { i.ServerID = "abcdef-12345" }},
		{"empty icePwd", "icePwd", func(i *Identity) { i.ICEPwd = "" }},
		{"long icePwd", "icePwd", func(i *Identity) { i.ICEPwd += "x" }},
		{"icePwd with a space", "icePwd", func(i *Identity) { i.ICEPwd = i.ICEPwd[:23] + " " }},
		{"uppercase mdnsName", "mdnsName", func(i *Identity) { i.MDNSName = strings.ToUpper(i.MDNSName[:36]) + ".local" }},
		{"mdnsName without .local", "mdnsName", func(i *Identity) { i.MDNSName = strings.TrimSuffix(i.MDNSName, ".local") }},
		{"host name as mdnsName", "mdnsName", func(i *Identity) { i.MDNSName = "desk.local" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			damaged := *id
			c.damage(&damaged)
			raw, err := json.Marshal(damaged)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "identity.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = LoadOrCreate(path)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), c.field) {
				t.Fatalf("want an error naming %s and %s, got %v", path, c.field, err)
			}
			if c.field == "icePwd" && damaged.ICEPwd != "" && strings.Contains(err.Error(), damaged.ICEPwd) {
				t.Errorf("the error %q shows the ICE password", err)
			}
			if got, _ := os.ReadFile(path); string(got) != string(raw) {
				t.Error("the damaged file was changed")
			}
		})
	}
}

func TestLoadOrCreateRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("want an error naming %s, got %v", path, err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{not json" {
		t.Errorf("a corrupt identity was overwritten: %q", raw)
	}
}

func TestRandomString(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z0-9]{16}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s := RandomString(16)
		if !re.MatchString(s) {
			t.Fatalf("RandomString(16) = %q", s)
		}
		seen[s] = true
	}
	if len(seen) < 49 {
		t.Errorf("only %d distinct strings out of 50", len(seen))
	}
}
