// Package mdns publishes the server's own <uuid>.local name on the local
// network with A records only (RFC 6762). It answers questions about that
// one name and ignores every other, side by side with the operating system's
// own responder on port 5353.
package mdns

import (
	"errors"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// ttl is the lifetime of the records in multicast answers.
	ttl = 120
	// legacyTTL caps the lifetime of the records in answers to legacy
	// one-shot queries (RFC 6762 §6.7).
	legacyTTL = 10
	// topBit is the unicast-response bit in a question's class and the
	// cache-flush bit in a record's class (RFC 6762 §5.4, §10.2).
	topBit = 0x8000
	// typeNSEC is the NSEC record type, which dnsmessage does not name.
	typeNSEC dnsmessage.Type = 47
)

// Respond answers an mDNS query about name, a host name such as
// "<uuid>.local", with the IPv4 addresses among ifaceIPs, those of the
// interface the query arrived on. A question of type A or ANY gets one A
// record per address; any other type gets an NSEC record asserting that the
// name has A records only (RFC 6762 §6.1). The response is a multicast one:
// ID 0, authoritative, no questions, TTL 120 and the cache-flush bit.
// unicast reports that every question about name asked for a unicast
// response. ok is false when the query asks nothing about name (or is a
// response, or the interface has no IPv4 address); err is set when the
// query cannot be parsed or name is not a valid host name.
func Respond(query []byte, name string, ifaceIPs []net.IP) (resp []byte, unicast bool, ok bool, err error) {
	return respond(query, name, ifaceIPs, false)
}

// respond is Respond; with legacy set it answers a legacy one-shot query
// (one sent from a port other than 5353) as RFC 6762 §6.7 requires: the
// query's ID and questions repeated, TTL 10, no cache-flush bit.
func respond(query []byte, name string, ifaceIPs []net.IP, legacy bool) (resp []byte, unicast bool, ok bool, err error) {
	own, err := hostName(name)
	if err != nil {
		return nil, false, false, err
	}
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, false, false, err
	}
	if h.Response || h.OpCode != 0 {
		return nil, false, false, nil
	}
	questions, err := p.AllQuestions()
	if err != nil {
		return nil, false, false, err
	}

	var wantA, wantNSEC bool
	unicast = true
	for _, q := range questions {
		class := q.Class &^ topBit
		if !strings.EqualFold(q.Name.String(), own.String()) || (class != dnsmessage.ClassINET && class != dnsmessage.ClassANY) {
			continue
		}
		if q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeALL {
			wantA = true
		} else {
			wantNSEC = true
		}
		if q.Class&topBit == 0 {
			unicast = false
		}
	}
	ips := ipv4s(ifaceIPs)
	if (!wantA && !wantNSEC) || len(ips) == 0 {
		return nil, false, false, nil
	}

	hdr := dnsmessage.Header{Response: true, Authoritative: true}
	rh := dnsmessage.ResourceHeader{Name: own, Class: dnsmessage.ClassINET | topBit, TTL: ttl}
	if legacy {
		hdr.ID = h.ID
		rh.Class, rh.TTL = dnsmessage.ClassINET, legacyTTL
	}
	b := dnsmessage.NewBuilder(nil, hdr)
	b.EnableCompression()
	if legacy {
		if err := b.StartQuestions(); err != nil {
			return nil, false, false, err
		}
		for _, q := range questions {
			if err := b.Question(q); err != nil {
				return nil, false, false, err
			}
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil, false, false, err
	}
	if wantA {
		if err := addA(&b, rh, ips); err != nil {
			return nil, false, false, err
		}
	}
	if wantNSEC {
		if err := addNSEC(&b, rh); err != nil {
			return nil, false, false, err
		}
	} else {
		// The A records come with an NSEC in the additional section, which
		// says there is no other type, AAAA included (RFC 6762 §6.1).
		if err := b.StartAdditionals(); err != nil {
			return nil, false, false, err
		}
		if err := addNSEC(&b, rh); err != nil {
			return nil, false, false, err
		}
	}
	resp, err = b.Finish()
	if err != nil {
		return nil, false, false, err
	}
	return resp, unicast, true, nil
}

// Announcement is an unsolicited response announcing name's A records, one
// per IPv4 address among ips, with TTL 120 and the cache-flush bit.
func Announcement(name string, ips []net.IP) ([]byte, error) {
	own, err := hostName(name)
	if err != nil {
		return nil, err
	}
	addrs := ipv4s(ips)
	if len(addrs) == 0 {
		return nil, errors.New("mdns: no IPv4 address to announce")
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	rh := dnsmessage.ResourceHeader{Name: own, Class: dnsmessage.ClassINET | topBit, TTL: ttl}
	if err := addA(&b, rh, addrs); err != nil {
		return nil, err
	}
	return b.Finish()
}

// hostName is name as a fully qualified DNS name, checked to be one.
func hostName(name string) (dnsmessage.Name, error) {
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	n, err := dnsmessage.NewName(name)
	if err != nil {
		return dnsmessage.Name{}, err
	}
	if _, err := wireForm(n); err != nil {
		return dnsmessage.Name{}, err
	}
	return n, nil
}

// wireForm is n in uncompressed DNS wire form: each label prefixed with its
// length, then a zero byte.
func wireForm(n dnsmessage.Name) ([]byte, error) {
	s := strings.TrimSuffix(n.String(), ".")
	if s == "" {
		return nil, errors.New("mdns: empty host name")
	}
	var b []byte
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, errors.New("mdns: bad label in host name " + n.String())
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0), nil
}

func ipv4s(ips []net.IP) [][4]byte {
	var out [][4]byte
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			out = append(out, [4]byte(ip4))
		}
	}
	return out
}

func addA(b *dnsmessage.Builder, rh dnsmessage.ResourceHeader, ips [][4]byte) error {
	for _, ip := range ips {
		if err := b.AResource(rh, dnsmessage.AResource{A: ip}); err != nil {
			return err
		}
	}
	return nil
}

// addNSEC adds an NSEC record whose next domain name is the owner itself,
// uncompressed, and whose type bitmap holds only A: window 0, one byte,
// bit 1 set (RFC 4034 §4.1, RFC 6762 §6.1).
func addNSEC(b *dnsmessage.Builder, rh dnsmessage.ResourceHeader) error {
	next, err := wireForm(rh.Name)
	if err != nil {
		return err
	}
	data := append(next, 0x00, 0x01, 0x40)
	return b.UnknownResource(rh, dnsmessage.UnknownResource{Type: typeNSEC, Data: data})
}
