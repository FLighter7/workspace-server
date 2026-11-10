// Package identity keeps the server's fixed identity: the id, certificate,
// ICE password and .local name that paired devices rely on.
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/fsutil"
	"github.com/pion/webrtc/v4"
)

// Identity is the content of identity.json. It is created once and never
// changed: losing it means every device pairs again.
type Identity struct {
	ServerID string `json:"serverId"`
	CertPEM  string `json:"certPem"`
	ICEPwd   string `json:"icePwd"`
	MDNSName string `json:"mdnsName"`
}

// LoadOrCreate reads the identity at path, or creates and stores a new one if
// the file does not exist. An unreadable, corrupt or invalid file is an
// error: it is never replaced.
func LoadOrCreate(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		var id Identity
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if err := id.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if _, err := id.Certificate(); err != nil {
			return nil, fmt.Errorf("certificate in %s: %w", path, err)
		}

		return &id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	id, err := create()
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}

	return id, nil
}

var (
	serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{12}$`)
	icePwdPattern   = regexp.MustCompile(`^[A-Za-z0-9]{24}$`)
	mdnsNamePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.local$`)
)

// validate checks the fields as create makes them, so that a file damaged by
// hand fails here and not later as an obscure rtc or mDNS error. The ICE
// password is a credential, so its value stays out of the error.
func (id *Identity) validate() error {
	switch {
	case !serverIDPattern.MatchString(id.ServerID):
		return fmt.Errorf("serverId %q must be 12 characters of [A-Za-z0-9]", id.ServerID)
	case !icePwdPattern.MatchString(id.ICEPwd):
		return errors.New("icePwd must be 24 characters of [A-Za-z0-9]")
	case !mdnsNamePattern.MatchString(id.MDNSName):
		return fmt.Errorf("mdnsName %q must be a lowercase <uuid v4>.local name", id.MDNSName)
	}
	return nil
}

func create() (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return nil, err
	}
	notBefore := time.Now().AddDate(0, 0, -1)
	cert, err := webrtc.NewCertificate(key, x509.Certificate{
		SerialNumber: serial,
		Version:      2,
		Issuer:       pkix.Name{CommonName: "workspace-server"},
		Subject:      pkix.Name{CommonName: "workspace-server"},
		NotBefore:    notBefore,
		NotAfter:     notBefore.AddDate(20, 0, 0),
	})
	if err != nil {
		return nil, err
	}
	pemText, err := cert.PEM()
	if err != nil {
		return nil, err
	}
	name, err := uuidV4()
	if err != nil {
		return nil, err
	}

	return &Identity{
		ServerID: RandomString(12),
		CertPEM:  pemText,
		ICEPwd:   RandomString(24),
		MDNSName: name + ".local",
	}, nil
}

// Certificate parses the stored certificate and key.
func (id *Identity) Certificate() (webrtc.Certificate, error) {
	cert, err := webrtc.CertificateFromPEM(id.CertPEM)
	if err != nil {
		return webrtc.Certificate{}, err
	}

	return *cert, nil
}

// Fingerprint returns the SHA-256 fingerprint of the certificate, which
// clients pin.
func (id *Identity) Fingerprint() ([32]byte, error) {
	var fp [32]byte
	cert, err := id.Certificate()
	if err != nil {
		return fp, err
	}
	fps, err := cert.GetFingerprints()
	if err != nil {
		return fp, err
	}
	for _, f := range fps {
		if f.Algorithm != "sha-256" {
			continue
		}
		raw, err := hex.DecodeString(strings.ReplaceAll(f.Value, ":", ""))
		if err != nil {
			return fp, err
		}
		if len(raw) != len(fp) {
			return fp, fmt.Errorf("sha-256 fingerprint has %d bytes", len(raw))
		}
		copy(fp[:], raw)

		return fp, nil
	}

	return fp, errors.New("certificate has no sha-256 fingerprint")
}

// FingerprintHex returns Fingerprint as uppercase hex separated by colons.
func (id *Identity) FingerprintHex() (string, error) {
	fp, err := id.Fingerprint()
	if err != nil {
		return "", err
	}
	parts := make([]string, len(fp))
	for i, b := range fp {
		parts[i] = fmt.Sprintf("%02X", b)
	}

	return strings.Join(parts, ":"), nil
}

// RandomString returns n random characters from [A-Za-z0-9].
func RandomString(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// Reject bytes from the incomplete last block of 256 so no character is
	// likelier than another.
	const limit = 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2+8)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}

	return string(out)
}

// uuidV4 returns a lowercase random UUID with the version and variant bits set.
func uuidV4() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u[:])

	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
