package rtc

import "strings"

// The server never sees the client's real offer, so it answers a stand-in.
// Only the client's ufrag, taken from the first connectivity check, is real.
// The password and the fingerprint are placeholders: the server is ICE-lite
// and never checks the client's side of ICE, and it does not verify the
// client's certificate (the client proves itself with a token inside the
// channel instead).
const (
	placeholderPwd         = "placeholderplaceholder00"
	placeholderFingerprint = "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"
)

// standInOffer is the offer the server pretends the client with clientUfrag
// sent.
func standInOffer(clientUfrag string) string {
	return strings.Join([]string{
		"v=0",
		"o=- 0 2 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"a=group:BUNDLE 0",
		"a=msid-semantic: WMS",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"c=IN IP4 0.0.0.0",
		"a=ice-ufrag:" + clientUfrag,
		"a=ice-pwd:" + placeholderPwd,
		"a=fingerprint:sha-256 " + placeholderFingerprint,
		"a=setup:actpass",
		"a=mid:0",
		"a=sctp-port:5000",
		"a=max-message-size:262144",
	}, "\r\n") + "\r\n"
}
