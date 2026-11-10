// Package netaddr lists the addresses this machine can be reached at on its
// local network.
package netaddr

import (
	"fmt"
	"net"
)

// Interface is a network interface with its usable IPv4 addresses. Nets
// holds the same addresses with their masks, in the same order.
type Interface struct {
	Index int
	Name  string
	IPs   []net.IP
	Nets  []net.IPNet
}

// RawInterface is what the operating system reports about an interface. An
// address reported without a mask has a full-length one.
type RawInterface struct {
	Index int
	Name  string
	Flags net.Flags
	Addrs []net.IPNet
}

// LANInterfaces returns the interfaces that are up, not loopback, not
// point-to-point, with their non-link-local IPv4 addresses. Interfaces
// without such an address are left out. A failure to list the interfaces, or
// the addresses of one that could be on the LAN, is an error: an empty result
// must mean that there is no LAN address, not that the system did not say.
func LANInterfaces() ([]Interface, error) {
	return scan(net.Interfaces, (*net.Interface).Addrs)
}

// scan is LANInterfaces over the given system calls.
func scan(list func() ([]net.Interface, error), addrsOf func(*net.Interface) ([]net.Addr, error)) ([]Interface, error) {
	ifaces, err := list()
	if err != nil {
		return nil, fmt.Errorf("list the network interfaces: %w", err)
	}
	raw := make([]RawInterface, 0, len(ifaces))
	for i := range ifaces {
		ifc := &ifaces[i]
		addrs, err := addrsOf(ifc)
		if err != nil {
			if lanCandidate(ifc.Flags) {
				return nil, fmt.Errorf("list the addresses of %s: %w", ifc.Name, err)
			}
			continue
		}
		r := RawInterface{Index: ifc.Index, Name: ifc.Name, Flags: ifc.Flags}
		for _, addr := range addrs {
			switch a := addr.(type) {
			case *net.IPNet:
				r.Addrs = append(r.Addrs, *a)
			case *net.IPAddr:
				r.Addrs = append(r.Addrs, net.IPNet{IP: a.IP})
			}
		}
		raw = append(raw, r)
	}

	return filter(raw), nil
}

// LANIPv4 returns every address of LANInterfaces.
func LANIPv4() ([]net.IP, error) {
	ifaces, err := LANInterfaces()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, ifc := range ifaces {
		ips = append(ips, ifc.IPs...)
	}

	return ips, nil
}

// lanCandidate reports whether an interface with flags could be on the LAN:
// up, not loopback, not point-to-point.
func lanCandidate(flags net.Flags) bool {
	return flags&net.FlagUp != 0 && flags&net.FlagLoopback == 0 && flags&net.FlagPointToPoint == 0
}

func filter(in []RawInterface) []Interface {
	var out []Interface
	for _, r := range in {
		if !lanCandidate(r.Flags) {
			continue
		}
		var ips []net.IP
		var nets []net.IPNet
		for _, a := range r.Addrs {
			ip4 := a.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			mask := a.Mask
			if len(mask) == net.IPv6len {
				mask = mask[12:]
			}
			if len(mask) != net.IPv4len {
				mask = net.CIDRMask(32, 32)
			}
			ips = append(ips, ip4)
			nets = append(nets, net.IPNet{IP: ip4.Mask(mask), Mask: mask})
		}
		if len(ips) > 0 {
			out = append(out, Interface{Index: r.Index, Name: r.Name, IPs: ips, Nets: nets})
		}
	}

	return out
}
