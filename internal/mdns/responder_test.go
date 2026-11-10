package mdns

import (
	"net"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/netaddr"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

func TestArrival(t *testing.T) {
	ipNet := func(cidr string) net.IPNet {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		return *n
	}
	ifaces := []netaddr.Interface{
		{Index: 4, Name: "en0", IPs: []net.IP{net.ParseIP("192.168.1.5").To4()}, Nets: []net.IPNet{ipNet("192.168.1.0/24")}},
		{Index: 7, Name: "en5", IPs: []net.IP{net.ParseIP("10.0.0.2").To4(), net.ParseIP("10.0.1.2").To4()},
			Nets: []net.IPNet{ipNet("10.0.0.0/8"), ipNet("10.0.1.0/24")}},
		{Index: 8, Name: "en6", IPs: []net.IP{net.ParseIP("10.0.2.9").To4()}, Nets: []net.IPNet{ipNet("10.0.2.0/24")}},
	}
	for _, tc := range []struct {
		desc  string
		index int
		src   string
		want  string // "" for none
	}{
		{"the interface the system names", 7, "192.168.1.20", "en5"},
		{"another interface, from one of our addresses (a query from this machine)", 1, "10.0.1.2", "en5"},
		{"another interface, from elsewhere", 9, "10.0.1.3", ""},
		{"no interface named: the subnet holding the source", 0, "192.168.1.20", "en0"},
		{"no interface named: the narrowest subnet holding the source", 0, "10.0.2.77", "en6"},
		{"no interface named: a wider subnet", 0, "10.200.0.1", "en5"},
		{"no interface named: one of our addresses", 0, "10.0.2.9", "en6"},
		{"no interface named, on no subnet of ours", 0, "8.8.8.8", ""},
		{"no interface named, link-local", 0, "169.254.1.1", ""},
		{"no interface named, not IPv4", 0, "fe80::1", ""},
	} {
		got, ok := arrival(ifaces, tc.index, net.ParseIP(tc.src))
		if tc.want == "" {
			if ok {
				t.Errorf("%s: got %s, want none", tc.desc, got.Name)
			}
			continue
		}
		if !ok || got.Name != tc.want {
			t.Errorf("%s: got %q (ok %v), want %s", tc.desc, got.Name, ok, tc.want)
		}
	}
}

func TestRecentQueries(t *testing.T) {
	var r recentQueries
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if r.seen("4 10.0.0.5:5353 q", t0) {
		t.Fatal("a first query was seen")
	}
	if !r.seen("4 10.0.0.5:5353 q", t0.Add(time.Millisecond)) {
		t.Error("a copy 1 ms later was not seen")
	}
	if r.seen("7 10.0.0.5:5353 q", t0.Add(time.Millisecond)) {
		t.Error("the same query on another interface was seen")
	}
	if r.seen("4 10.0.0.5:5353 q", t0.Add(300*time.Millisecond)) {
		t.Error("a retry 300 ms later was seen")
	}
	if len(r.at) != 1 {
		t.Errorf("%d entries kept, want only the latest", len(r.at))
	}
}

func TestLogLimiter(t *testing.T) {
	var l logLimiter
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if ok, more := l.allow(t0); !ok || more != 0 {
		t.Fatalf("first error: %v, %d; want logged", ok, more)
	}
	for i := 1; i <= 5; i++ {
		if ok, _ := l.allow(t0.Add(time.Duration(i) * readRetry)); ok {
			t.Fatalf("error %d within a minute was logged", i)
		}
	}
	if ok, more := l.allow(t0.Add(readErrorEvery)); !ok || more != 5 {
		t.Errorf("after a minute: %v, %d; want logged with 5 held back", ok, more)
	}
}

func TestStartRefusesNothingToJoin(t *testing.T) {
	if r, err := Start(testName, func() ([]netaddr.Interface, error) { return nil, nil }, t.Logf); err == nil {
		r.Close()
		t.Fatal("Start with no interface succeeded")
	}
}

func TestStartRefusesBadName(t *testing.T) {
	if r, err := Start("bad..local", netaddr.LANInterfaces, t.Logf); err == nil {
		r.Close()
		t.Fatal("Start with a bad name succeeded")
	}
}

// startOrSkip starts a responder on the real port 5353 and LAN interfaces,
// skipping the test where the machine cannot host one (no LAN, the port
// not shareable, no multicast).
func startOrSkip(t *testing.T) *Responder {
	t.Helper()
	r, err := Start(testName, netaddr.LANInterfaces, t.Logf)
	if err != nil {
		t.Skipf("cannot host a responder here: %v", err)
	}
	return r
}

// Close stops everything at once, even mid-way through the announcements,
// and releases the port, so a new responder can start right after.
func TestStartCloseStart(t *testing.T) {
	r := startOrSkip(t)
	begin := time.Now()
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Errorf("Close took %v; it should not wait for the second announcement", d)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	r, err := Start(testName, netaddr.LANInterfaces, t.Logf)
	if err != nil {
		t.Fatalf("Start after Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A legacy query (from a port other than 5353) multicast on a LAN interface
// gets a unicast answer with its ID and that interface's addresses.
func TestAnswersOverTheNetwork(t *testing.T) {
	r := startOrSkip(t)
	defer r.Close()

	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	if err := pc.SetMulticastLoopback(true); err != nil {
		t.Fatal(err)
	}
	const firstID = 0x100
	for i, ifc := range r.ifaces {
		if err := pc.SetMulticastInterface(&net.Interface{Index: ifc.Index, Name: ifc.Name}); err != nil {
			t.Fatal(err)
		}
		q := query(t, uint16(firstID+i), question{testName + ".", dnsmessage.TypeA, dnsmessage.ClassINET})
		if _, err := pc.WriteTo(q, nil, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
			t.Fatalf("query on %s: %v", ifc.Name, err)
		}
	}

	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 9000)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	p := parse(t, buf[:n])
	i := int(p.header.ID) - firstID
	if i < 0 || i >= len(r.ifaces) {
		t.Fatalf("answer ID %#x matches no query", p.header.ID)
	}
	ifc := r.ifaces[i]
	if len(p.answers) != len(ifc.IPs) {
		t.Fatalf("%d answers, want one per address of %s %v", len(p.answers), ifc.Name, ifc.IPs)
	}
	for j, a := range p.answers {
		body, ok := a.Body.(*dnsmessage.AResource)
		if !ok || !net.IP(body.A[:]).Equal(ifc.IPs[j]) {
			t.Errorf("answer %d: %v, want A %v", j, a.Body, ifc.IPs[j])
		}
	}
}
