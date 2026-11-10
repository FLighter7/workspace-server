package netaddr

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// nets parses CIDR addresses; a bare address has no mask.
func nets(t *testing.T, addrs ...string) []net.IPNet {
	t.Helper()
	var out []net.IPNet
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			out = append(out, net.IPNet{IP: ip})
			continue
		}
		ip, n, err := net.ParseCIDR(a)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, net.IPNet{IP: ip, Mask: n.Mask})
	}
	return out
}

// filter keeps only up, non-loopback, non-point-to-point, non-link-local IPv4.
func TestFilter(t *testing.T) {
	in := []RawInterface{
		{1, "lo0", net.FlagUp | net.FlagLoopback, nets(t, "127.0.0.1/8")},
		{2, "en0", net.FlagUp, nets(t, "192.168.1.5/24", "fe80::1/64", "169.254.3.4/16")},
		{3, "utun5", net.FlagUp | net.FlagPointToPoint, nets(t, "100.64.0.1/32")},
		{4, "en1", 0, nets(t, "10.0.0.2/8")},
	}
	got := filter(in)
	if len(got) != 1 || got[0].Name != "en0" || got[0].Index != 2 || len(got[0].IPs) != 1 || !got[0].IPs[0].Equal(net.ParseIP("192.168.1.5")) {
		t.Fatalf("got %+v", got)
	}
	if len(got[0].Nets) != 1 || got[0].Nets[0].String() != "192.168.1.0/24" {
		t.Errorf("Nets %v, want [192.168.1.0/24]", got[0].Nets)
	}
}

// A mask in 16-byte form is read as IPv4; an address without one is /32.
func TestFilterMasks(t *testing.T) {
	in := []RawInterface{
		{2, "en0", net.FlagUp, []net.IPNet{
			{IP: net.ParseIP("10.1.2.3"), Mask: net.CIDRMask(112, 128)},
			{IP: net.ParseIP("10.9.9.9")},
		}},
	}
	got := filter(in)
	if len(got) != 1 || len(got[0].Nets) != 2 {
		t.Fatalf("got %+v", got)
	}
	if a, b := got[0].Nets[0].String(), got[0].Nets[1].String(); a != "10.1.0.0/16" || b != "10.9.9.9/32" {
		t.Errorf("Nets %s, %s; want 10.1.0.0/16 and 10.9.9.9/32", a, b)
	}
}

func TestFilterDropsInterfaceWithoutUsableAddress(t *testing.T) {
	in := []RawInterface{
		{1, "en0", net.FlagUp, nets(t, "fe80::1", "169.254.1.1")},
	}
	if got := filter(in); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestScanReportsErrors(t *testing.T) {
	up := net.Interface{Index: 2, Name: "en0", Flags: net.FlagUp}
	down := net.Interface{Index: 3, Name: "en1"}
	lo := net.Interface{Index: 1, Name: "lo0", Flags: net.FlagUp | net.FlagLoopback}
	boom := errors.New("boom")
	addrs := func(fail ...string) func(*net.Interface) ([]net.Addr, error) {
		return func(ifc *net.Interface) ([]net.Addr, error) {
			for _, name := range fail {
				if ifc.Name == name {
					return nil, boom
				}
			}
			return []net.Addr{&net.IPNet{IP: net.IPv4(192, 168, 1, 5), Mask: net.CIDRMask(24, 32)}}, nil
		}
	}
	list := func(ifaces ...net.Interface) func() ([]net.Interface, error) {
		return func() ([]net.Interface, error) { return ifaces, nil }
	}

	if _, err := scan(func() ([]net.Interface, error) { return nil, boom }, addrs()); !errors.Is(err, boom) {
		t.Errorf("a failed interface list: err = %v, want boom", err)
	}
	if _, err := scan(list(up, lo), addrs("en0")); !errors.Is(err, boom) || !strings.Contains(err.Error(), "en0") {
		t.Errorf("a failed address list of a LAN candidate: err = %v, want boom naming en0", err)
	}
	// Interfaces that could not be on the LAN are skipped.
	got, err := scan(list(up, down, lo), addrs("en1", "lo0"))
	if err != nil || len(got) != 1 || got[0].Name != "en0" {
		t.Errorf("scan = %+v, %v; want en0 only", got, err)
	}
}

func TestLANIPv4MatchesLANInterfaces(t *testing.T) {
	ifaces, err := LANInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, ifc := range ifaces {
		want += len(ifc.IPs)
	}
	ips, err := LANIPv4()
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != want {
		t.Fatalf("LANIPv4 has %d addresses, interfaces have %d", len(ips), want)
	}
	for _, ip := range ips {
		if ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			t.Errorf("unexpected address %s", ip)
		}
	}
}
