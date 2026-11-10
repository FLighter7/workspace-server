package mdns

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

const testName = "0b7d7a52-5d3c-4a4b-9a43-2f1c8e9f3a10.local"

const (
	unicastClass = dnsmessage.ClassINET | 0x8000 // the question's unicast-response bit
	flushClass   = dnsmessage.ClassINET | 0x8000 // the record's cache-flush bit
)

var lanIP = []net.IP{net.ParseIP("192.168.1.5")}

type question struct {
	name  string
	typ   dnsmessage.Type
	class dnsmessage.Class
}

// query builds a query with the given questions and a nonzero ID.
func query(t *testing.T, id uint16, qs ...question) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(q.name), Type: q.typ, Class: q.class})
		if err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

type parsed struct {
	header      dnsmessage.Header
	questions   []dnsmessage.Question
	answers     []dnsmessage.Resource
	additionals []dnsmessage.Resource
}

func parse(t *testing.T, msg []byte) parsed {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		t.Fatal(err)
	}
	qs, err := p.AllQuestions()
	if err != nil {
		t.Fatal(err)
	}
	as, err := p.AllAnswers()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SkipAllAuthorities(); err != nil {
		t.Fatal(err)
	}
	ad, err := p.AllAdditionals()
	if err != nil {
		t.Fatal(err)
	}
	return parsed{h, qs, as, ad}
}

// respondOK calls Respond and fails unless it answers.
func respondOK(t *testing.T, q []byte, ips []net.IP) (parsed, bool) {
	t.Helper()
	resp, unicast, ok, err := Respond(q, testName, ips)
	if err != nil || !ok {
		t.Fatalf("Respond: ok %v, err %v", ok, err)
	}
	return parse(t, resp), unicast
}

func checkA(t *testing.T, r dnsmessage.Resource, name string, want net.IP) {
	t.Helper()
	if r.Header.Type != dnsmessage.TypeA {
		t.Fatalf("type %v, want A", r.Header.Type)
	}
	if r.Header.Name.String() != name {
		t.Errorf("name %q, want %q", r.Header.Name.String(), name)
	}
	if r.Header.TTL != 120 {
		t.Errorf("TTL %d, want 120", r.Header.TTL)
	}
	if r.Header.Class != flushClass {
		t.Errorf("class %#x, want %#x", uint16(r.Header.Class), uint16(flushClass))
	}
	a, ok := r.Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("body %T", r.Body)
	}
	if !net.IP(a.A[:]).Equal(want) {
		t.Errorf("address %v, want %v", net.IP(a.A[:]), want)
	}
}

func checkMDNSHeader(t *testing.T, p parsed) {
	t.Helper()
	if p.header.ID != 0 || !p.header.Response || !p.header.Authoritative {
		t.Errorf("header %+v, want ID 0, response, authoritative", p.header)
	}
	if len(p.questions) != 0 {
		t.Errorf("%d questions, want none", len(p.questions))
	}
}

// wireName is name in uncompressed DNS wire form.
func wireName(name string) []byte {
	var b []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

func TestAnswerA(t *testing.T) {
	q := query(t, 0x1234, question{testName + ".", dnsmessage.TypeA, dnsmessage.ClassINET})
	p, unicast := respondOK(t, q, lanIP)
	if unicast {
		t.Error("unicast is true for a question without the unicast-response bit")
	}
	checkMDNSHeader(t, p)
	if len(p.answers) != 1 {
		t.Fatalf("%d answers, want 1", len(p.answers))
	}
	checkA(t, p.answers[0], testName+".", lanIP[0])
	if len(p.additionals) != 1 {
		t.Fatalf("%d additional records, want the NSEC", len(p.additionals))
	}
	checkNSEC(t, p.additionals[0])
}

func TestAnswerMatchesCaseInsensitively(t *testing.T) {
	q := query(t, 0, question{strings.ToUpper(testName) + ".", dnsmessage.TypeA, dnsmessage.ClassINET})
	p, _ := respondOK(t, q, lanIP)
	if len(p.answers) != 1 {
		t.Fatalf("%d answers, want 1", len(p.answers))
	}
	if got := p.answers[0].Header.Name.String(); !strings.EqualFold(got, testName+".") {
		t.Errorf("name %q", got)
	}
}

func TestAnswerANYAndTwoAddresses(t *testing.T) {
	ips := []net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("10.0.0.7")}
	q := query(t, 0, question{testName + ".", dnsmessage.TypeALL, dnsmessage.ClassINET})
	p, _ := respondOK(t, q, ips)
	if len(p.answers) != 2 {
		t.Fatalf("%d answers, want 2", len(p.answers))
	}
	for i, ip := range ips {
		checkA(t, p.answers[i], testName+".", ip)
	}
	if len(p.additionals) != 1 {
		t.Fatalf("%d additional records, want one NSEC", len(p.additionals))
	}
	checkNSEC(t, p.additionals[0])
}

func checkNSEC(t *testing.T, r dnsmessage.Resource) {
	t.Helper()
	if r.Header.Type != 47 {
		t.Fatalf("type %v, want NSEC (47)", r.Header.Type)
	}
	if r.Header.Name.String() != testName+"." {
		t.Errorf("name %q", r.Header.Name.String())
	}
	if r.Header.TTL != 120 || r.Header.Class != flushClass {
		t.Errorf("TTL %d class %#x, want 120 and %#x", r.Header.TTL, uint16(r.Header.Class), uint16(flushClass))
	}
	u, ok := r.Body.(*dnsmessage.UnknownResource)
	if !ok {
		t.Fatalf("body %T", r.Body)
	}
	want := append(wireName(testName), 0x00, 0x01, 0x40) // window 0, length 1, only A
	if !bytes.Equal(u.Data, want) {
		t.Errorf("NSEC RDATA % x, want % x", u.Data, want)
	}
}

func TestAAAAGetsNSEC(t *testing.T) {
	q := query(t, 0, question{testName + ".", dnsmessage.TypeAAAA, dnsmessage.ClassINET})
	p, _ := respondOK(t, q, lanIP)
	checkMDNSHeader(t, p)
	if len(p.answers) != 1 {
		t.Fatalf("%d answers, want 1", len(p.answers))
	}
	checkNSEC(t, p.answers[0])
	if len(p.additionals) != 0 {
		t.Errorf("%d additional records, want none", len(p.additionals))
	}
}

// Any type other than A and ANY has no record, which NSEC asserts too
// (RFC 6762 §6.1).
func TestOtherTypeGetsNSEC(t *testing.T) {
	q := query(t, 0, question{testName + ".", dnsmessage.TypeTXT, dnsmessage.ClassINET})
	p, _ := respondOK(t, q, lanIP)
	if len(p.answers) != 1 {
		t.Fatalf("%d answers, want 1", len(p.answers))
	}
	checkNSEC(t, p.answers[0])
}

// A resolver asking for both address types at once gets both answers, each
// once, and nothing about other names.
func TestAAndAAAAInOneQuery(t *testing.T) {
	q := query(t, 0,
		question{"other.local.", dnsmessage.TypeA, dnsmessage.ClassINET},
		question{testName + ".", dnsmessage.TypeA, dnsmessage.ClassINET},
		question{testName + ".", dnsmessage.TypeALL, dnsmessage.ClassINET},
		question{testName + ".", dnsmessage.TypeAAAA, dnsmessage.ClassINET},
	)
	p, _ := respondOK(t, q, lanIP)
	if len(p.answers) != 2 {
		t.Fatalf("%d answers, want 2", len(p.answers))
	}
	if len(p.additionals) != 0 {
		t.Errorf("%d additional records, want none: the NSEC is an answer", len(p.additionals))
	}
	checkA(t, p.answers[0], testName+".", lanIP[0])
	checkNSEC(t, p.answers[1])
}

func TestUnicastBit(t *testing.T) {
	q := query(t, 0, question{testName + ".", dnsmessage.TypeA, unicastClass})
	p, unicast := respondOK(t, q, lanIP)
	if !unicast {
		t.Error("unicast is false for a question with the unicast-response bit")
	}
	if len(p.answers) != 1 {
		t.Fatalf("%d answers, want 1", len(p.answers))
	}
	checkA(t, p.answers[0], testName+".", lanIP[0])
}

func TestOtherNameIgnored(t *testing.T) {
	q := query(t, 0, question{"other.local.", dnsmessage.TypeA, dnsmessage.ClassINET})
	resp, _, ok, err := Respond(q, testName, lanIP)
	if err != nil || ok || resp != nil {
		t.Fatalf("Respond: resp %v, ok %v, err %v; want nothing", resp, ok, err)
	}
}

func TestNothingToAnswer(t *testing.T) {
	ourA := question{testName + ".", dnsmessage.TypeA, dnsmessage.ClassINET}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(ourA.name), Type: ourA.typ, Class: ourA.class})
	response, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		desc string
		msg  []byte
		ips  []net.IP
	}{
		{"a response, such as our own looped-back answer", response, lanIP},
		{"a class other than IN and ANY", query(t, 0, question{ourA.name, ourA.typ, dnsmessage.ClassCHAOS}), lanIP},
		{"no IPv4 address on the arrival interface", query(t, 0, ourA), []net.IP{net.ParseIP("fe80::1")}},
	} {
		resp, _, ok, err := Respond(tc.msg, testName, tc.ips)
		if err != nil || ok || resp != nil {
			t.Errorf("%s: resp %v, ok %v, err %v; want nothing", tc.desc, resp, ok, err)
		}
	}
}

func TestMalformedQuery(t *testing.T) {
	if _, _, ok, err := Respond([]byte{1, 2, 3}, testName, lanIP); err == nil || ok {
		t.Fatalf("ok %v, err %v; want an error", ok, err)
	}
}

// A query from a port other than 5353 is a legacy one-shot query (RFC 6762
// §6.7): the answer echoes its ID and question, with a short TTL and no
// cache-flush bit.
func TestLegacyAnswer(t *testing.T) {
	q := query(t, 0x1234, question{testName + ".", dnsmessage.TypeA, dnsmessage.ClassINET})
	resp, _, ok, err := respond(q, testName, lanIP, true)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	p := parse(t, resp)
	if p.header.ID != 0x1234 || !p.header.Response || !p.header.Authoritative {
		t.Errorf("header %+v, want ID 0x1234, response, authoritative", p.header)
	}
	if len(p.questions) != 1 || p.questions[0].Name.String() != testName+"." || p.questions[0].Type != dnsmessage.TypeA {
		t.Errorf("questions %+v, want the query's", p.questions)
	}
	if len(p.answers) != 1 {
		t.Fatalf("%d answers, want 1", len(p.answers))
	}
	h := p.answers[0].Header
	if h.Type != dnsmessage.TypeA || h.TTL != 10 || h.Class != dnsmessage.ClassINET {
		t.Errorf("answer header %+v, want A, TTL 10, class IN", h)
	}
}

func TestAnnouncement(t *testing.T) {
	ips := []net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("10.0.0.7")}
	msg, err := Announcement(testName, ips)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, msg)
	checkMDNSHeader(t, p)
	if len(p.answers) != 2 {
		t.Fatalf("%d answers, want 2", len(p.answers))
	}
	for i, ip := range ips {
		checkA(t, p.answers[i], testName+".", ip)
	}
	if len(p.additionals) != 0 {
		t.Errorf("%d additional records in an announcement, want none", len(p.additionals))
	}
}
