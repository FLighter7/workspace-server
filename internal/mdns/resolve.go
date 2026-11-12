package mdns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/netaddr"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// resolveGrace is how long Resolve keeps listening after the first answer,
// for the answers that other interfaces carry.
const resolveGrace = 150 * time.Millisecond

// Query is a one-shot mDNS query for the A records of name, a host name
// such as "<uuid>.local", with the given ID. Sent from a port other than
// 5353, it is a legacy query (RFC 6762 §6.7): responders answer it by
// unicast to the sender, repeating its ID.
func Query(name string, id uint16) ([]byte, error) {
	own, err := hostName(name)
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: own, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	return b.Finish()
}

// Addresses returns the IPv4 addresses that resp, an answer to the query
// with id, gives for name, from its answer and additional sections. Anything
// else, a packet that does not parse included, gives none.
func Addresses(resp []byte, name string, id uint16) []net.IP {
	own, err := hostName(name)
	if err != nil {
		return nil
	}
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil || !h.Response || h.ID != id {
		return nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil
	}
	var out []net.IP
	take := func(next func() (dnsmessage.ResourceHeader, error), skip func() error) bool {
		for {
			rh, err := next()
			if errors.Is(err, dnsmessage.ErrSectionDone) {
				return true
			}
			if err != nil {
				return false
			}
			if rh.Type != dnsmessage.TypeA || !strings.EqualFold(rh.Name.String(), own.String()) {
				if skip() != nil {
					return false
				}
				continue
			}
			a, err := p.AResource()
			if err != nil {
				return false
			}
			out = appendNew(out, net.IP(a.A[:]))
		}
	}
	if !take(p.AnswerHeader, p.SkipAnswer) {
		return out
	}
	if err := p.SkipAllAuthorities(); err != nil {
		return out
	}
	take(p.AdditionalHeader, p.SkipAdditional)
	return out
}

func appendNew(list []net.IP, ip net.IP) []net.IP {
	for _, have := range list {
		if have.Equal(ip) {
			return list
		}
	}
	return append(list, ip)
}

// Resolve looks name up on the LAN: it sends a legacy query to 224.0.0.251
// on every interface ifaces lists, and collects the A records answered until
// ctx ends, or a short while after the first answer. It returns an error when
// nothing answered.
func Resolve(ctx context.Context, name string, ifaces func() ([]netaddr.Interface, error)) ([]net.IP, error) {
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	q, err := Query(name, id)
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	all, err := ifaces()
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	if len(all) == 0 {
		return nil, errors.New("mdns: no LAN interface to ask on")
	}

	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	defer c.Close()
	p := ipv4.NewPacketConn(c)
	// A responder on this machine hears the query only with loopback on.
	_ = p.SetMulticastLoopback(true)
	_ = p.SetMulticastTTL(255)
	sent := 0
	for _, ifc := range all {
		if err := p.SetMulticastInterface(netInterface(ifc)); err != nil {
			continue
		}
		if _, err := p.WriteTo(q, nil, groupAddr); err == nil {
			sent++
		}
	}
	if sent == 0 {
		return nil, fmt.Errorf("mdns: could not ask for %s on any interface", name)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()) })
	defer stop()
	var out []net.IP
	buf := make([]byte, maxMessage)
	for {
		if err := c.SetReadDeadline(deadline); err != nil || ctx.Err() != nil {
			break
		}
		n, err := c.Read(buf)
		if err != nil {
			break
		}
		for _, ip := range Addresses(buf[:n], name, id) {
			out = appendNew(out, ip)
		}
		if len(out) > 0 {
			if grace := time.Now().Add(resolveGrace); grace.Before(deadline) {
				deadline = grace
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mdns: no answer for %s", name)
	}
	return out, nil
}
