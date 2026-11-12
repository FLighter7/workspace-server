package wsclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// CodeVersion is the pairing code version this client reads: codes start
// with "wsrv1.".
const CodeVersion = 1

const (
	fingerprintBytes = 32
	// minPasswordLength: the server's ICE password is 24 characters; ICE
	// itself asks for at least 22.
	minPasswordLength = 22
)

// The errors ParseCode returns, as Error codes.
const (
	CodeNotPairingCode = "notPairingCode"
	CodeTooNew         = "tooNew"
	CodeDamaged        = "damaged"
)

// PairingCode is a parsed "wsrv1." pairing code, as a server's page shows it.
type PairingCode struct {
	Target
	ServerID string
	Name     string
	// Secret is the one-time secret that proves the user stood at the
	// server's screen.
	Secret string
}

var (
	codePattern   = regexp.MustCompile(`^wsrv(\d+)\.([A-Za-z0-9_-]+)$`)
	base64urlText = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func damaged() *Error { return &Error{Code: CodeDamaged, Message: "the pairing code is damaged"} }

// ParseCode reads a pairing code scanned or pasted from a server's page, the
// way the client package's parsePairingCode does. Whitespace around it is
// ignored; anything else wrong is an *Error whose code says what:
// CodeNotPairingCode, CodeTooNew or CodeDamaged.
func ParseCode(text string) (PairingCode, error) {
	m := codePattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return PairingCode{}, &Error{Code: CodeNotPairingCode, Message: "this is not a workspace-server pairing code"}
	}
	if v, err := strconv.Atoi(m[1]); err != nil || v > CodeVersion {
		return PairingCode{}, &Error{Code: CodeTooNew, Message: "the pairing code comes from a newer server"}
	}
	raw, err := fromBase64URL(m[2])
	if err != nil || !utf8.Valid(raw) {
		return PairingCode{}, damaged()
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return PairingCode{}, damaged()
	}

	if v, ok := fields["v"].(float64); !ok || v != CodeVersion {
		return PairingCode{}, damaged()
	}
	id, idOK := nonEmpty(fields["id"])
	name, nameOK := nonEmpty(fields["n"])
	secret, secretOK := nonEmpty(fields["s"])
	if !idOK || !nameOK || !secretOK {
		return PairingCode{}, damaged()
	}
	password, ok := fields["p"].(string)
	if !ok || len(utf16.Encode([]rune(password))) < minPasswordLength {
		return PairingCode{}, damaged()
	}
	port, ok := fields["o"].(float64)
	if !ok || port != math.Trunc(port) || port < 1 || port > 65535 {
		return PairingCode{}, damaged()
	}
	list, ok := fields["h"].([]any)
	if !ok || len(list) == 0 {
		return PairingCode{}, damaged()
	}
	hosts := make([]string, 0, len(list))
	for _, h := range list {
		host, ok := nonEmpty(h)
		if !ok {
			return PairingCode{}, damaged()
		}
		hosts = append(hosts, host)
	}
	fp, ok := fields["f"].(string)
	if !ok || !base64urlText.MatchString(fp) {
		return PairingCode{}, damaged()
	}
	fpBytes, err := fromBase64URL(fp)
	if err != nil || len(fpBytes) != fingerprintBytes {
		return PairingCode{}, damaged()
	}

	return PairingCode{
		Target:   Target{Password: password, Fingerprint: colonHex(fpBytes), Port: int(port), Hosts: hosts},
		ServerID: id,
		Name:     name,
		Secret:   secret,
	}, nil
}

// fromBase64URL decodes unpadded base64url, as a browser's atob does after
// the alphabet is swapped: a length that leaves one character over is not
// base64 at all.
func fromBase64URL(s string) ([]byte, error) {
	if len(s)%4 == 1 {
		return nil, fmt.Errorf("base64url of length %d", len(s))
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func nonEmpty(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

// colonHex is b in uppercase hex, the bytes separated by colons, as SDP
// writes a fingerprint.
func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(parts, ":")
}
