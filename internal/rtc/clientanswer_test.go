package rtc

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/conformance"
	"github.com/FLighter7/workspace-server/internal/identity"
)

// midLine finds the media id in the client's own offer.
var midLine = regexp.MustCompile(`(?m)^a=mid:(\S+)`)

// answerTarget is what a client knows from the pairing code: icePwd, the
// certificate fingerprint in uppercase colon hex, the UDP port and the hosts,
// the UUID .local name first.
type answerTarget struct {
	Password    string   `json:"password"`
	Fingerprint string   `json:"fingerprint"`
	Port        int      `json:"port"`
	Hosts       []string `json:"hosts"`
}

// answerFor builds the server's SDP answer the way a client does, with no help
// from the server: from the pairing data and the client's own offer, whose
// media id it copies, with the server ufrag and the time (milliseconds) given.
//
// It follows the spec's "The client's side of the connection" line by line.
// testdata/conformance/answers.json pins its output, and the client package's
// builder is checked against the same file; the loopback tests prove the real
// listener accepts what it builds.
func answerFor(offerSDP string, target answerTarget, ufrag string, nowMillis int64) string {
	m := midLine.FindStringSubmatch(offerSDP)
	if m == nil {
		panic("answerFor: the offer has no a=mid line")
	}
	mid := m[1]

	lines := []string{
		"v=0",
		fmt.Sprintf("o=- %d 2 IN IP4 127.0.0.1", nowMillis),
		"s=-",
		"t=0 0",
		"a=group:BUNDLE " + mid,
		"a=ice-lite",
		"a=msid-semantic: WMS",
		fmt.Sprintf("m=application %d UDP/DTLS/SCTP webrtc-datachannel", target.Port),
		"c=IN IP4 0.0.0.0",
	}
	for i, host := range target.Hosts {
		lines = append(lines, fmt.Sprintf("a=candidate:%d 1 udp %d %s %d typ host", i+1, 2130706431-i, host, target.Port))
	}
	lines = append(lines,
		"a=end-of-candidates",
		"a=ice-ufrag:"+ufrag,
		"a=ice-pwd:"+target.Password,
		"a=fingerprint:sha-256 "+target.Fingerprint,
		"a=setup:passive",
		"a=mid:"+mid,
		"a=sctp-port:5000",
		"a=max-message-size:262144",
	)

	return strings.Join(lines, "\r\n") + "\r\n"
}

// buildAnswer is answerFor as a client calls it for a real connection: the
// server ufrag is 8 random [A-Za-z0-9] characters, new for every connection,
// and the server learns it from the first connectivity check.
func buildAnswer(offerSDP, icePwd, fingerprintHex string, port int, hosts []string) string {
	target := answerTarget{Password: icePwd, Fingerprint: fingerprintHex, Port: port, Hosts: hosts}
	return answerFor(offerSDP, target, identity.RandomString(8), time.Now().UnixMilli())
}

// TestAnswerConformance checks the reference answer against the shared vectors.
func TestAnswerConformance(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Name   string       `json:"name"`
			Offer  []string     `json:"offer"`
			Target answerTarget `json:"target"`
			Ufrag  string       `json:"ufrag"`
			Now    int64        `json:"now"`
			Answer []string     `json:"answer"`
		} `json:"cases"`
	}
	conformance.Read(t, "answers.json", &vectors)
	if len(vectors.Cases) == 0 {
		t.Fatal("no answers in the vectors")
	}
	sdp := func(lines []string) string { return strings.Join(lines, "\r\n") + "\r\n" }

	for _, c := range vectors.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := answerFor(sdp(c.Offer), c.Target, c.Ufrag, c.Now)
			if want := sdp(c.Answer); got != want {
				t.Errorf("answer =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// TestBuildAnswerUfragIsFresh: the server keys connections by the ufrag, so
// two answers must not share one.
func TestBuildAnswerUfragIsFresh(t *testing.T) {
	const offer = "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=group:BUNDLE data\r\n" +
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\nc=IN IP4 0.0.0.0\r\na=mid:data\r\n"
	ufragLine := regexp.MustCompile(`a=ice-ufrag:([A-Za-z0-9]{8})\r\n`)
	ufrag := func(a string) string {
		m := ufragLine.FindStringSubmatch(a)
		if m == nil {
			t.Fatalf("answer has no 8-character ufrag: %q", a)
		}
		return m[1]
	}

	first := buildAnswer(offer, "abcdefghijklmnopqrstuvwx", "0A:1B", 47800, []string{"192.168.1.20"})
	again := buildAnswer(offer, "abcdefghijklmnopqrstuvwx", "0A:1B", 47800, []string{"192.168.1.20"})
	if ufrag(first) == ufrag(again) {
		t.Errorf("two answers share the ufrag %q; it must be new for every connection", ufrag(first))
	}
}
