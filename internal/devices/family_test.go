package devices

import (
	"strings"
	"testing"
	"time"
)

func entry(id, app string) Entry {
	return Entry{ID: id, App: app, Name: "Phone", TokenHash: strings.Repeat("a", 64), PairedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}

func ids(list []Device) map[string]bool {
	out := map[string]bool{}
	for _, d := range list {
		out[d.ID] = true
	}
	return out
}

// Merge takes the union of both registries less every device removed on
// either side, keeps its own entry of a device both have, never takes this
// server itself, and takes a server it already has under another id once.
// What it drops is returned, and the result survives a reopen.
func TestMerge(t *testing.T) {
	r, path, _ := openFresh(t)
	own, _, _ := r.Pair("todo", "Desk phone")
	gone, _, _ := r.Pair("todo", "Old phone")
	lost, _, _ := r.Pair("todo", "Lost phone")
	r.Remove(lost.ID) // removed here; the other side still has it
	srv, _, _ := r.PairServer("Laptop")
	if _, err := r.SetServer(srv.ID, laptop()); err != nil {
		t.Fatal(err)
	}

	self := laptop()
	self.ServerID, self.Name = "SelfServer01", "Self"
	other := laptop()
	other.ServerID = "Lp7tOp0123ab" // the laptop again, under another id
	list := []Entry{
		{ID: own.ID, App: "todo", Name: "Renamed", TokenHash: own.TokenHash},
		entry("NewPhone000001", "todo"),
		{ID: lost.ID, App: "todo", Name: "Lost phone", TokenHash: lost.TokenHash},
		{ID: "SelfDevice0001", Kind: KindServer, TokenHash: strings.Repeat("b", 64), Server: &self},
		{ID: "MyCredential01", Kind: KindServer, TokenHash: strings.Repeat("c", 64)},
		{ID: "LaptopAgain001", Kind: KindServer, TokenHash: strings.Repeat("d", 64), Server: &other},
		{ID: "Broken", App: "", TokenHash: "nope"},
	}
	dropped, err := r.Merge(list, []Tombstone{{ID: gone.ID, At: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}}, Self{ServerID: "SelfServer01", Device: "MyCredential01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].ID != gone.ID {
		t.Errorf("dropped %+v, want the device removed on the other side", dropped)
	}
	want := map[string]bool{own.ID: true, srv.ID: true, "NewPhone000001": true}
	reopened, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []*Registry{r, reopened} {
		if got := ids(reg.List()); len(got) != len(want) || !got[own.ID] || !got[srv.ID] || !got["NewPhone000001"] {
			t.Errorf("devices %v, want %v", got, want)
		}
		removed := map[string]bool{}
		for _, tomb := range reg.Removed() {
			removed[tomb.ID] = true
		}
		if !removed[gone.ID] || !removed[lost.ID] {
			t.Errorf("removed %+v, want both removals", reg.Removed())
		}
	}
	if d, _ := r.Get(own.ID); d.Name != "Desk phone" {
		t.Errorf("an own device's entry became %+v", d)
	}
	if d, ok := r.Get("NewPhone000001"); !ok || d.Name != "Phone" || d.LastSeen.IsZero() {
		t.Errorf("a merged device %+v, want its name and a time", d)
	}

	// Nothing new: nothing written, nothing dropped.
	if dropped, err := r.Merge(list, nil, Self{ServerID: "SelfServer01"}); err != nil || len(dropped) != 0 {
		t.Errorf("Merge again = %v, %v", dropped, err)
	}
}

// Removals are kept newest first up to MaxTombstones.
func TestTombstonesAreCapped(t *testing.T) {
	r, _, clk := openFresh(t)
	var removed []Tombstone
	for i := range MaxTombstones + 5 {
		removed = append(removed, Tombstone{ID: "Gone" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + strings.Repeat("y", i/26%10) + "z" + itoa(i), At: clk.now().Add(time.Duration(i) * time.Second)})
	}
	if _, err := r.Merge(nil, removed, Self{}); err != nil {
		t.Fatal(err)
	}
	got := r.Removed()
	if len(got) != MaxTombstones || got[0].ID != removed[5].ID || got[len(got)-1].ID != removed[len(removed)-1].ID {
		t.Errorf("%d removals kept, first %s; want the newest %d", len(got), got[0].ID, MaxTombstones)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

// MintServer makes one server-kind device for a server, and a new token for
// it when minted again.
func TestMintServer(t *testing.T) {
	r, _, _ := openFresh(t)
	d1, token1, err := r.MintServer(laptop())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Authenticate("todo", d1.ID, token1); !ok || !d1.IsServer() || d1.Server == nil || d1.Name != "Laptop" {
		t.Fatalf("the minted device %+v is not a server admitted with its token", d1)
	}
	d2, token2, err := r.MintServer(laptop())
	if err != nil {
		t.Fatal(err)
	}
	if d2.ID != d1.ID || token2 == token1 || len(r.List()) != 1 {
		t.Errorf("minted again: %+v (%d devices), want the same device with a new token", d2, len(r.List()))
	}
	if _, ok := r.Authenticate("", d1.ID, token1); ok {
		t.Error("the old token still works")
	}
	if _, _, err := r.MintServer(Server{ServerID: "bad"}); err == nil {
		t.Error("minted for a server that is not valid")
	}
}

// Clear forgets the own devices and removals, and the replica stays.
func TestClear(t *testing.T) {
	r, path, _ := openFresh(t)
	d, _, _ := r.Pair("todo", "Phone")
	r.Remove(d.ID)
	r.Pair("todo", "Tablet")
	r.SetReplica([]Entry{entry("Dev0123456789ab", "todo")})
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 0 || len(r.Removed()) != 0 {
		t.Errorf("after Clear: %+v, removed %+v", r.List(), r.Removed())
	}
	if _, ok := r.Get("Dev0123456789ab"); !ok {
		t.Error("the replica went with the own devices")
	}
	if again, err := Open(path, newClock().now); err != nil || len(again.List()) != 0 {
		t.Errorf("reopened after Clear: %v, %v", again, err)
	}
}

// A server-kind device of the replica, another server of the family, is
// admitted to link sessions only.
func TestAReplicaServerIsAdmittedToLinkSessionsOnly(t *testing.T) {
	r, _, _ := openFresh(t)
	token := "family-token"
	r.SetReplica([]Entry{{ID: "Laptop00000001", Kind: KindServer, TokenHash: hashToken(token), Server: new(laptop())}})
	if _, ok := r.Authenticate("", "Laptop00000001", token); !ok {
		t.Error("a family server is not admitted to a link session")
	}
	if _, ok := r.Authenticate("todo", "Laptop00000001", token); ok {
		t.Error("a family server is admitted to an app")
	}
}

// Entries that break the rules are refused by Validate.
func TestEntryValidate(t *testing.T) {
	s := laptop()
	for name, e := range map[string]Entry{
		"a server with an app":      {ID: "Laptop00000001", App: "todo", Kind: KindServer, TokenHash: strings.Repeat("a", 64)},
		"a device without an app":   {ID: "Phone000000001", TokenHash: strings.Repeat("a", 64)},
		"an unknown kind":           {ID: "Phone000000001", App: "todo", Kind: "robot", TokenHash: strings.Repeat("a", 64)},
		"a device with a server":    {ID: "Phone000000001", App: "todo", TokenHash: strings.Repeat("a", 64), Server: &s},
		"a long name":               {ID: "Phone000000001", App: "todo", Name: strings.Repeat("я", 65), TokenHash: strings.Repeat("a", 64)},
		"a server that is not one":  {ID: "Laptop00000001", Kind: KindServer, TokenHash: strings.Repeat("a", 64), Server: &Server{ServerID: "x"}},
		"a hash that is not a hash": {ID: "Phone000000001", App: "todo", TokenHash: "abc"},
	} {
		if e.Validate() == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if err := (Entry{ID: "Laptop00000001", Kind: KindServer, TokenHash: strings.Repeat("a", 64), Server: &s}).Validate(); err != nil {
		t.Errorf("a server-kind entry: %v", err)
	}
}
