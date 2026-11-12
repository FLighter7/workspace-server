package wsclient

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

const (
	ufragAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	ufragLength   = 8
	// firstPriority is the priority of the first host candidate; each later
	// one is one lower, so the order holds.
	firstPriority = 2130706431
)

// Target is what a client needs to reach a server: everything the answer it
// builds is made from.
type Target struct {
	// Password is the server's fixed ICE password.
	Password string `json:"password"`
	// Fingerprint is the SHA-256 fingerprint of the server's certificate,
	// uppercase colon-separated hex.
	Fingerprint string `json:"fingerprint"`
	// Port is the server's UDP port.
	Port int `json:"port"`
	// Hosts are the server's .local name first, then its LAN IPv4
	// addresses.
	Hosts []string `json:"hosts"`
}

// RandomUfrag returns a fresh ICE username fragment for the server's side of
// one connection. The server keys connections by it, so it must be new every
// time.
func RandomUfrag() string {
	var b [ufragLength]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("wsclient: read random bytes: " + err.Error())
	}
	out := make([]byte, ufragLength)
	for i, c := range b {
		// The modulo slightly favours the first characters. Harmless: the
		// ufrag only has to differ between connections; it is no secret.
		out[i] = ufragAlphabet[int(c)%len(ufragAlphabet)]
	}
	return string(out)
}

var midLine = regexp.MustCompile(`(?m)^a=mid:(\S+)`)

// BuildAnswer is the server's SDP answer to this client's offer, built here
// instead of asked for: the server is ICE-lite with a fixed password and
// certificate, so everything in it is known from the pairing, and no
// signaling is needed. The layout follows docs/protocol.md, "The client's
// side of the connection", line by line; testdata/conformance/answers.json
// pins it. hosts are the candidates, in order; nowMillis only fills the
// session version in the origin line.
func BuildAnswer(offerSDP string, t Target, ufrag string, nowMillis int64) string {
	mid := "0"
	if m := midLine.FindStringSubmatch(offerSDP); m != nil {
		mid = m[1]
	}
	lines := []string{
		"v=0",
		fmt.Sprintf("o=- %d 2 IN IP4 127.0.0.1", nowMillis),
		"s=-",
		"t=0 0",
		"a=group:BUNDLE " + mid,
		"a=ice-lite",
		"a=msid-semantic: WMS",
		fmt.Sprintf("m=application %d UDP/DTLS/SCTP webrtc-datachannel", t.Port),
		"c=IN IP4 0.0.0.0",
	}
	for i, host := range t.Hosts {
		lines = append(lines, fmt.Sprintf("a=candidate:%d 1 udp %d %s %d typ host", i+1, firstPriority-i, host, t.Port))
	}
	lines = append(lines,
		"a=end-of-candidates",
		"a=ice-ufrag:"+ufrag,
		"a=ice-pwd:"+t.Password,
		"a=fingerprint:sha-256 "+t.Fingerprint,
		"a=setup:passive",
		"a=mid:"+mid,
		"a=sctp-port:5000",
		"a=max-message-size:262144",
	)
	return strings.Join(lines, "\r\n") + "\r\n"
}
