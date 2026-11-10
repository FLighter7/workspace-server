package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/FLighter7/workspace-server/internal/netaddr"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

const (
	port = 5353
	// maxMessage is the largest mDNS message (RFC 6762 §17).
	maxMessage = 9000
	// announceGap separates the two announcements (RFC 6762 §8.3).
	announceGap = time.Second
	// readRetry spaces out reads after an unexpected read error.
	readRetry = 100 * time.Millisecond
	// readErrorEvery spaces out the log lines of a read error that repeats.
	readErrorEvery = time.Minute
	// dupWindow: the same query from the same source on the same interface
	// within this time is answered once. One socket joined on several
	// interfaces can be handed one datagram once per interface; where the
	// system does not name the interface (Windows), every copy maps to the
	// same guess. It is well below a resolver's retry after about 1 s.
	dupWindow = 250 * time.Millisecond
)

var (
	group     = net.IPv4(224, 0, 0, 251)
	groupAddr = &net.UDPAddr{IP: group, Port: port}
)

// tracef, when set (by the live test, before Start), hears about every
// query for the name and what became of it.
var tracef func(format string, args ...any)

// Responder answers mDNS queries about one name on the LAN interfaces.
type Responder struct {
	name   string
	ifaces []netaddr.Interface // those whose group membership succeeded
	logf   func(string, ...any)
	p      *ipv4.PacketConn

	// noControl is set where the system gives no control messages
	// (Windows): the arrival interface is then guessed from the source
	// address, and the outgoing one chosen with a socket option, under
	// writeMu so that choosing and writing go together.
	noControl bool
	writeMu   sync.Mutex

	recent recentQueries // used by serve only

	done      chan struct{}
	wg        sync.WaitGroup // the read loop and the announcements
	closeOnce sync.Once
}

// Start publishes name, a host name such as "<uuid>.local", on the
// interfaces ifaces returns (called once; restart the responder when they
// change; an error from it fails Start). It opens 0.0.0.0:5353 shared with the system's own responder,
// joins 224.0.0.251 on each interface, announces the name's A records twice,
// a second apart, and answers queries until Close. An interface whose
// membership fails is logged and left out; Start fails only when none
// succeeds. logf may be nil.
func Start(name string, ifaces func() ([]netaddr.Interface, error), logf func(string, ...any)) (*Responder, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, err := hostName(name); err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	all, err := ifaces()
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	if len(all) == 0 {
		return nil, errors.New("mdns: no LAN interface")
	}

	lc := net.ListenConfig{Control: reuse}
	c, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	p := ipv4.NewPacketConn(c)
	r := &Responder{name: name, logf: logf, p: p, done: make(chan struct{})}
	for _, ifc := range all {
		if err := p.JoinGroup(netInterface(ifc), &net.UDPAddr{IP: group}); err != nil {
			logf("mdns: join %v on %s: %v", group, ifc.Name, err)
			continue
		}
		r.ifaces = append(r.ifaces, ifc)
	}
	if len(r.ifaces) == 0 {
		_ = c.Close()
		return nil, fmt.Errorf("mdns: could not join %v on any LAN interface", group)
	}
	if err := p.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		r.noControl = true
		logf("mdns: the system does not tell the arrival interface (%v); guessing it from the source address", err)
	}
	// Loopback lets the system's responder on this machine hear the answers.
	if err := p.SetMulticastLoopback(true); err != nil {
		logf("mdns: multicast loopback: %v", err)
	}
	// RFC 6762 §11: mDNS packets are sent with IP TTL 255.
	if err := p.SetMulticastTTL(255); err != nil {
		logf("mdns: multicast TTL: %v", err)
	}
	if err := p.SetTTL(255); err != nil {
		logf("mdns: TTL: %v", err)
	}

	r.wg.Add(2)
	go r.serve()
	go r.announce()
	return r, nil
}

// Close stops answering and announcing, leaves the group on every interface
// and releases the socket; it returns once nothing of the responder runs.
// Later calls do nothing.
func (r *Responder) Close() error {
	var err error
	r.closeOnce.Do(func() {
		close(r.done)
		for _, ifc := range r.ifaces {
			// An interface that has gone refuses; closing the socket drops
			// whatever membership is left anyway.
			_ = r.p.LeaveGroup(netInterface(ifc), &net.UDPAddr{IP: group})
		}
		err = r.p.Close()
		r.wg.Wait()
	})
	return err
}

func (r *Responder) closing() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *Responder) serve() {
	defer r.wg.Done()
	buf := make([]byte, maxMessage)
	var readErrs logLimiter
	for {
		n, cm, src, err := r.p.ReadFrom(buf)
		if err != nil {
			if r.closing() || errors.Is(err, net.ErrClosed) {
				return
			}
			if ok, more := readErrs.allow(time.Now()); ok && more > 0 {
				r.logf("mdns: read: %v (and %d more read errors since the last line)", err, more)
			} else if ok {
				r.logf("mdns: read: %v", err)
			}
			select {
			case <-r.done:
				return
			case <-time.After(readRetry):
			}
			continue
		}
		r.answer(buf[:n], cm, src)
	}
}

// answer answers one packet if it asks about the name: by multicast on the
// arrival interface, or by unicast to the asker when it asked for that or
// sent a legacy query (from a port other than 5353).
func (r *Responder) answer(query []byte, cm *ipv4.ControlMessage, src net.Addr) {
	from, ok := src.(*net.UDPAddr)
	if !ok {
		return
	}
	index := 0
	if cm != nil {
		index = cm.IfIndex
	}
	ifc, ok := arrival(r.ifaces, index, from.IP)
	if !ok {
		if tracef != nil {
			if _, _, asks, _ := respond(query, r.name, r.ifaces[0].IPs, false); asks {
				tracef("mdns: query from %v on interface %d (%s), not a joined LAN interface: not answered", from, index, describe(query))
			}
		}
		return
	}
	legacy := from.Port != port
	// A malformed packet from the network is not worth a log line, and
	// Start has checked the name.
	resp, unicast, ok, err := respond(query, r.name, ifc.IPs, legacy)
	if err != nil || !ok {
		return
	}
	if r.recent.seen(fmt.Sprint(ifc.Index, " ", from, " ", string(query)), time.Now()) {
		if tracef != nil {
			tracef("mdns: query from %v on interface %d (%s): a copy of one just answered, not answered", from, index, describe(query))
		}
		return
	}
	how := "multicast"
	if unicast || legacy {
		how = "unicast"
		_, err = r.p.WriteTo(resp, nil, from)
	} else {
		err = r.multicast(resp, ifc)
	}
	if tracef != nil {
		tracef("mdns: query from %v on interface %d (%s): answered on %s by %s, error %v", from, index, describe(query), ifc.Name, how, err)
	}
	if err != nil && !r.closing() {
		r.logf("mdns: answer %v on %s: %v", from, ifc.Name, err)
	}
}

// announce sends the announcement on every interface twice, a second apart.
func (r *Responder) announce() {
	defer r.wg.Done()
	for round := 0; round < 2; round++ {
		if round > 0 {
			select {
			case <-r.done:
				return
			case <-time.After(announceGap):
			}
		}
		for _, ifc := range r.ifaces {
			msg, err := Announcement(r.name, ifc.IPs)
			if err == nil {
				err = r.multicast(msg, ifc)
			}
			if err != nil {
				if r.closing() {
					return
				}
				r.logf("mdns: announce on %s: %v", ifc.Name, err)
			}
		}
	}
}

// multicast sends msg to 224.0.0.251:5353 on ifc.
func (r *Responder) multicast(msg []byte, ifc netaddr.Interface) error {
	if r.noControl {
		r.writeMu.Lock()
		defer r.writeMu.Unlock()
		if err := r.p.SetMulticastInterface(netInterface(ifc)); err != nil {
			return err
		}
	}
	_, err := r.p.WriteTo(msg, &ipv4.ControlMessage{IfIndex: ifc.Index}, groupAddr)
	return err
}

// arrival is the interface among ifaces that a packet from src came in on:
// the one the system names by index; failing that, the one holding src
// itself, as a query from this machine may arrive on loopback; and where the
// system names none (index 0, as on Windows), the one whose subnet holds
// src, the narrowest if several do. A source on no subnet of ours is not
// on the link, and its packet is dropped (RFC 6762 §11).
func arrival(ifaces []netaddr.Interface, index int, src net.IP) (netaddr.Interface, bool) {
	if index > 0 {
		for _, ifc := range ifaces {
			if ifc.Index == index {
				return ifc, true
			}
		}
	}
	src4 := src.To4()
	if src4 == nil {
		return netaddr.Interface{}, false
	}
	for _, ifc := range ifaces {
		for _, ip := range ifc.IPs {
			if ip.Equal(src4) {
				return ifc, true
			}
		}
	}
	if index != 0 {
		return netaddr.Interface{}, false
	}
	best, bestLen := -1, -1
	for i, ifc := range ifaces {
		for _, n := range ifc.Nets {
			if ones, _ := n.Mask.Size(); n.Contains(src4) && ones > bestLen {
				best, bestLen = i, ones
			}
		}
	}
	if best < 0 {
		return netaddr.Interface{}, false
	}
	return ifaces[best], true
}

// recentQueries remembers the queries answered within dupWindow, by a key
// naming the interface, the source and the query's bytes.
type recentQueries struct {
	at map[string]time.Time
}

// seen reports whether key was answered within dupWindow before now, and
// otherwise records it as answered now.
func (r *recentQueries) seen(key string, now time.Time) bool {
	if r.at == nil {
		r.at = map[string]time.Time{}
	}
	for k, t := range r.at {
		if now.Sub(t) >= dupWindow {
			delete(r.at, k)
		}
	}
	if _, ok := r.at[key]; ok {
		return true
	}
	r.at[key] = now
	return false
}

// logLimiter lets a repeating error through at most once per
// readErrorEvery, counting the ones it holds back.
type logLimiter struct {
	last       time.Time
	started    bool
	suppressed int
}

// allow reports whether to log now and how many were held back since the
// last line.
func (l *logLimiter) allow(now time.Time) (bool, int) {
	if l.started && now.Sub(l.last) < readErrorEvery {
		l.suppressed++
		return false, 0
	}
	more := l.suppressed
	l.started, l.last, l.suppressed = true, now, 0
	return true, more
}

// describe lists a query's questions with their type and whether they ask
// for a unicast (QU) or multicast (QM) answer, for tracef.
func describe(query []byte) string {
	var p dnsmessage.Parser
	if _, err := p.Start(query); err != nil {
		return "unparsable"
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return "unparsable"
	}
	parts := make([]string, 0, len(qs))
	for _, q := range qs {
		mode := "QM"
		if q.Class&topBit != 0 {
			mode = "QU"
		}
		parts = append(parts, fmt.Sprintf("%s %v %s", q.Name, q.Type, mode))
	}
	return strings.Join(parts, ", ")
}

func netInterface(ifc netaddr.Interface) *net.Interface {
	return &net.Interface{Index: ifc.Index, Name: ifc.Name}
}
