package mdns

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/netaddr"
)

// A query Resolve sends gets the responder's legacy answer, and Addresses
// reads the A records from it; a multicast answer (ID 0) or one for another
// query or name gives none.
func TestQueryAndAddresses(t *testing.T) {
	ips := []net.IP{net.IPv4(192, 168, 1, 5), net.IPv4(10, 0, 0, 2)}
	q, err := Query(testName, 0x2a2a)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, q)
	if p.header.Response || p.header.ID != 0x2a2a || len(p.questions) != 1 || p.questions[0].Name.String() != testName+"." {
		t.Fatalf("query %+v, want one question for %s with ID 0x2a2a", p, testName)
	}

	legacy, _, ok, err := respond(q, testName, ips, true)
	if err != nil || !ok {
		t.Fatalf("respond: %v, %v", ok, err)
	}
	got := Addresses(legacy, testName, 0x2a2a)
	if !slices.EqualFunc(got, ips, net.IP.Equal) {
		t.Errorf("Addresses = %v, want %v", got, ips)
	}
	if got := Addresses(legacy, testName, 0x2a2b); got != nil {
		t.Errorf("Addresses for another ID = %v, want none", got)
	}
	if got := Addresses(legacy, "7c9e6679-7425-40de-944b-e07fc1f90ae7.local", 0x2a2a); got != nil {
		t.Errorf("Addresses for another name = %v, want none", got)
	}
	if got := Addresses(q, testName, 0x2a2a); got != nil {
		t.Errorf("Addresses of the query itself = %v, want none", got)
	}
	if got := Addresses([]byte{1, 2, 3}, testName, 0x2a2a); got != nil {
		t.Errorf("Addresses of a broken packet = %v, want none", got)
	}
	if _, err := Query("bad..local", 1); err == nil {
		t.Error("Query accepted a bad name")
	}
}

// Resolve finds a responder's name over the network, and gives up on a name
// nobody answers.
func TestResolveOverTheNetwork(t *testing.T) {
	r := startOrSkip(t)
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := Resolve(ctx, testName, netaddr.LANInterfaces)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var want []net.IP
	for _, ifc := range r.ifaces {
		want = append(want, ifc.IPs...)
	}
	for _, ip := range got {
		if !slices.ContainsFunc(want, ip.Equal) {
			t.Errorf("Resolve gave %v, which is none of the responder's %v", ip, want)
		}
	}

	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if got, err := Resolve(ctx, "7c9e6679-7425-40de-944b-e07fc1f90ae7.local", netaddr.LANInterfaces); err == nil {
		t.Errorf("Resolve of a name nobody has = %v, want an error", got)
	}
}
