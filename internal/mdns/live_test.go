package mdns

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/identity"
	"github.com/FLighter7/workspace-server/internal/netaddr"
)

// TestLiveResponder publishes a fresh <uuid>.local for 60 s so that the
// system's resolvers can be checked by hand, from this machine and others:
//
//	macOS:   dns-sd -G v4 <name>   an Add line with this machine's LAN IPv4
//	         dns-sd -G v6 <name>   no address
//	Linux:   avahi-resolve -4 -n <name>
//	Windows: Resolve-DnsName -Name <name> -Type A
//
// It runs only with WSRV_MDNS_LIVE=1.
func TestLiveResponder(t *testing.T) {
	if os.Getenv("WSRV_MDNS_LIVE") != "1" {
		t.Skip("set WSRV_MDNS_LIVE=1 to run")
	}
	id, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	name := id.MDNSName

	tracef = func(format string, args ...any) {
		t.Logf(time.Now().Format("15:04:05.000 ")+format, args...)
	}
	defer func() { tracef = nil }()
	r, err := Start(name, netaddr.LANInterfaces, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	t.Logf("publishing %s for 60 s on:", name)
	for _, ifc := range r.ifaces {
		t.Logf("  %s (index %d) %v", ifc.Name, ifc.Index, ifc.IPs)
	}
	time.Sleep(60 * time.Second)
}
