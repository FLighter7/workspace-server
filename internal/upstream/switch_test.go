package upstream

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests switch the main of a family of real servers in this process,
// linked over real WebRTC (see link_test.go).

// makeFamily links every subordinate in subs to main, and waits until each
// holds main's registry with every other server of the family in it, as a
// family that has run for a while does.
func makeFamily(t *testing.T, main *node, subs ...*node) {
	t.Helper()
	for _, s := range subs {
		s.linkTo(main)
	}
	for _, s := range subs {
		waitFor(t, s.name+" to know the other servers of the family", func() bool {
			st := s.link.snapshot()
			if st == nil || !s.link.Status().Connected {
				return false
			}
			known := 0
			for _, e := range st.Devices {
				if e.IsServer() && e.Server != nil {
					known++
				}
			}
			return known == len(subs)
		})
	}
}

// followsMain waits until n is a subordinate of main at term, connected.
func followsMain(t *testing.T, n, main *node, term int) {
	t.Helper()
	waitFor(t, n.name+" to follow "+main.name, func() bool {
		st := n.link.Status()
		return n.link.Role() == RoleSubordinate && st.ServerID == main.id.ServerID && st.Term == term && st.Connected
	})
}

// isMain waits until n is a main at term that waits for nobody.
func isMain(t *testing.T, n *node, term int) {
	t.Helper()
	waitFor(t, n.name+" to be the main every server follows", func() bool {
		st := n.link.Status()
		return n.link.Role() == RoleMain && st.Term == term && len(st.Waiting) == 0
	})
}

// admitted reports whether n admits the device id of app with token.
func (n *node) admitted(app, id, token string) bool {
	_, ok := n.devices.Authenticate(app, id, token)
	return ok
}

// A planned switch: the laptop, a subordinate of the desk, becomes the main.
// The documents are the same on both first; the desk steps down, links to
// the laptop and hands its devices over; the tablet, the desk's other
// subordinate, relinks to the laptop. A device paired with the desk is
// admitted everywhere without pairing again, and its write reaches every
// server.
func TestPlannedSwitch(t *testing.T) {
	desk := startNode(t, "Desk")
	laptop := startNode(t, "Laptop")
	tablet := startNode(t, "Tablet")
	v1 := desk.write("todo", `{"v":1}`, nil)
	phone, phoneToken := desk.pair("todo")
	makeFamily(t, desk, laptop, tablet)
	waitFor(t, "the laptop to hold the desk's document", func() bool { return laptop.etag("todo") == v1 })

	followed, err := laptop.link.Promote(ctx(t), false)
	if err != nil || !followed {
		t.Fatalf("Promote = %v, %v; want the desk to follow", followed, err)
	}
	if role, term := laptop.link.Role(), laptop.link.Term(); role != RoleMain || term != 1 {
		t.Fatalf("the laptop is %s at term %d, want the main at term 1", role, term)
	}
	followsMain(t, desk, laptop, 1)
	followsMain(t, tablet, laptop, 1)
	isMain(t, laptop, 1)

	// The documents were the same: the desk agrees at once, nothing moved.
	waitFor(t, "the desk to agree with the laptop", func() bool { return desk.agreed("todo", v1) })
	if desk.historyHolds("todo", `{"v":1}`) {
		t.Error("the desk's document went to history although the two were the same")
	}
	// The desk handed its devices over and admits them on the laptop's word.
	waitFor(t, "the desk's own devices to be the laptop's", func() bool { return len(desk.devices.List()) == 0 })
	for _, n := range []*node{desk, laptop, tablet} {
		if !n.admitted("todo", phone, phoneToken) {
			t.Errorf("%s does not admit the phone paired with the desk", n.name)
		}
	}
	if laptop.link.Status().Linked {
		t.Error("the laptop is still linked")
	}
	servers := map[string]bool{}
	for _, d := range laptop.devices.List() {
		if d.IsServer() && d.Server != nil {
			servers[d.Server.Name] = true
		}
	}
	if !servers["Desk"] || !servers["Tablet"] || servers["Laptop"] {
		t.Errorf("the laptop's linked servers %v, want the desk and the tablet", servers)
	}

	// The phone syncs through the new main; the write reaches both others.
	d := laptop.connect("todo", phone, phoneToken)
	if d.welcome.Role != RoleMain || d.welcome.Term != 1 {
		t.Errorf("the laptop's welcome %+v, want the main at term 1", d.welcome)
	}
	v2 := d.put(t, &v1, `{"v":2}`)
	waitFor(t, "the write to reach the desk and the tablet", func() bool {
		return desk.etag("todo") == v2 && tablet.etag("todo") == v2
	})
	if w := desk.connect("todo", phone, phoneToken).welcome; w.Role != RoleSubordinate || w.Term != 1 || w.Subordinates != nil {
		t.Errorf("the desk's welcome %+v, want a subordinate at term 1", w)
	}
}

// "Hand the role to…" on the main: the desk asks the laptop to take over,
// which then switches as above.
func TestTakeOver(t *testing.T) {
	desk := startNode(t, "Desk")
	laptop := startNode(t, "Laptop")
	desk.write("todo", `{"v":1}`, nil)
	makeFamily(t, desk, laptop)

	if err := desk.hub.TakeOver(laptop.id.ServerID); err != nil {
		t.Fatal(err)
	}
	followsMain(t, desk, laptop, 1)
	isMain(t, laptop, 1)
	if err := laptop.hub.TakeOver("NoSuchServer"); err == nil {
		t.Error("TakeOver of an unknown server succeeded")
	}
}

// A forced switch while the desk is out of reach, the tablet still linked
// to it. The laptop becomes the main at once; the phone paired with the desk
// syncs through it, and the tablet follows it when the laptop reaches it.
// The term survives a restart. When the desk comes back, the two mains
// meet: the higher term wins, the desk steps down by itself and links to
// the laptop, its own writes made meanwhile kept in its history and merged
// back by its devices.
func TestForcedSwitchThenTheMainsMeet(t *testing.T) {
	desk := startNode(t, "Desk")
	laptop := startNode(t, "Laptop")
	tablet := startNode(t, "Tablet")
	v1 := desk.write("todo", `{"v":1}`, nil)
	phone, phoneToken := desk.pair("todo")
	makeFamily(t, desk, laptop, tablet)
	waitFor(t, "the laptop to hold the desk's document", func() bool { return laptop.etag("todo") == v1 })

	// The desk is away from both: it stays the main of its own side.
	laptop.cutOff(desk, true)
	tablet.cutOff(desk, true)
	laptop.link.NetworkChanged()
	waitFor(t, "the laptop to lose the desk", func() bool { return !laptop.link.Status().Connected })
	if _, err := laptop.link.Promote(ctx(t), false); !errors.As(err, new(*RefusedError)) {
		t.Errorf("a planned switch with the main away: %v, want unreachable", err)
	}
	followed, err := laptop.link.Promote(ctx(t), true)
	if err != nil || followed {
		t.Fatalf("Promote(force) = %v, %v", followed, err)
	}
	if laptop.link.Role() != RoleMain || laptop.link.Term() != 1 {
		t.Fatalf("the laptop is %s at term %d", laptop.link.Role(), laptop.link.Term())
	}
	followsMain(t, tablet, laptop, 1)

	// Both act as main meanwhile; the phone writes on each.
	v2 := laptop.connect("todo", phone, phoneToken).put(t, &v1, `{"v":"laptop"}`)
	waitFor(t, "the tablet to hold the laptop's write", func() bool { return tablet.etag("todo") == v2 })
	desk.connect("todo", phone, phoneToken).put(t, &v1, `{"v":"desk"}`)

	laptop.restart()
	if laptop.link.Term() != 1 || laptop.link.Role() != RoleMain || len(laptop.link.Status().Waiting) != 1 {
		t.Fatalf("after a restart the laptop is %s at term %d, waiting for %v", laptop.link.Role(), laptop.link.Term(), laptop.link.Status().Waiting)
	}
	if w := laptop.connect("todo", phone, phoneToken).welcome; w.Term != 1 {
		t.Errorf("the laptop's welcome after a restart names term %d", w.Term)
	}

	// They meet.
	laptop.cutOff(desk, false)
	tablet.cutOff(desk, false)
	laptop.link.NetworkChanged()
	followsMain(t, desk, laptop, 1)
	isMain(t, laptop, 1)
	waitFor(t, "the desk to take the laptop's document", func() bool { return desk.etag("todo") == v2 })
	if !desk.historyHolds("todo", `{"v":"desk"}`) {
		t.Error("the desk's own write is not in its history")
	}
	desk.restart()
	if desk.link.Term() != 1 || desk.link.Role() != RoleSubordinate {
		t.Errorf("after a restart the desk is %s at term %d", desk.link.Role(), desk.link.Term())
	}
	followsMain(t, desk, laptop, 1)
}

// The main is gone for good, and both of its subordinates are made the main
// while apart: both at term 1. When they meet, the higher server id wins,
// and the other steps down and links to it.
func TestSameTermTheHigherServerIDWins(t *testing.T) {
	desk := startNode(t, "Desk")
	a := startNode(t, "Laptop")
	b := startNode(t, "Tablet")
	desk.write("todo", `{"v":1}`, nil)
	makeFamily(t, desk, a, b)
	if err := desk.l.Close(); err != nil { // gone for good
		t.Fatal(err)
	}
	a.cutOff(b, true)
	b.cutOff(a, true)
	for _, n := range []*node{a, b} {
		if _, err := n.link.Promote(ctx(t), true); err != nil {
			t.Fatalf("%s: Promote: %v", n.name, err)
		}
		if n.link.Term() != 1 {
			t.Fatalf("%s is at term %d", n.name, n.link.Term())
		}
	}
	high, low := a, b
	if b.id.ServerID > a.id.ServerID {
		high, low = b, a
	}

	a.cutOff(b, false)
	b.cutOff(a, false)
	a.link.NetworkChanged()
	b.link.NetworkChanged()
	followsMain(t, low, high, 1)
	if high.link.Role() != RoleMain {
		t.Errorf("%s, the higher id, is no longer the main", high.name)
	}
}

// The registry merges by union with tombstones: during a split, the desk
// pairs one device and removes another, and the laptop, made the main by
// force, does the same. When they meet, every device paired on either side
// is admitted on both, and every device removed on either side stays
// removed on both.
func TestRegistryUnionWithTombstones(t *testing.T) {
	desk := startNode(t, "Desk")
	laptop := startNode(t, "Laptop")
	removedOnDesk, removedOnDeskToken := desk.pair("todo")
	removedOnLaptop, removedOnLaptopToken := desk.pair("todo")
	kept, keptToken := desk.pair("todo")
	makeFamily(t, desk, laptop)
	waitFor(t, "the laptop to admit the desk's devices", func() bool {
		return laptop.admitted("todo", removedOnDesk, removedOnDeskToken) && laptop.admitted("todo", kept, keptToken)
	})

	laptop.cutOff(desk, true)
	laptop.link.NetworkChanged()
	waitFor(t, "the laptop to lose the desk", func() bool { return !laptop.link.Status().Connected })
	if _, err := laptop.link.Promote(ctx(t), true); err != nil {
		t.Fatal(err)
	}
	pairedOnLaptop, pairedOnLaptopToken := laptop.pair("todo")
	if !laptop.hub.RemoveDevice(removedOnLaptop) {
		t.Fatal("the laptop does not have the desk's device to remove")
	}
	pairedOnDesk, pairedOnDeskToken := desk.pair("todo")
	if !desk.hub.RemoveDevice(removedOnDesk) {
		t.Fatal("RemoveDevice on the desk")
	}

	laptop.cutOff(desk, false)
	laptop.link.NetworkChanged()
	followsMain(t, desk, laptop, 1)
	waitFor(t, "the desk to hand its devices over", func() bool { return len(desk.devices.List()) == 0 })
	waitFor(t, "the desk to admit the laptop's device", func() bool {
		return desk.admitted("todo", pairedOnLaptop, pairedOnLaptopToken)
	})
	for _, n := range []*node{desk, laptop} {
		for _, c := range []struct {
			what      string
			id, token string
			want      bool
		}{
			{"the device kept", kept, keptToken, true},
			{"the device paired on the desk", pairedOnDesk, pairedOnDeskToken, true},
			{"the device paired on the laptop", pairedOnLaptop, pairedOnLaptopToken, true},
			{"the device removed on the desk", removedOnDesk, removedOnDeskToken, false},
			{"the device removed on the laptop", removedOnLaptop, removedOnLaptopToken, false},
		} {
			if got := n.admitted("todo", c.id, c.token); got != c.want {
				t.Errorf("%s admits %s: %v, want %v", n.name, c.what, got, c.want)
			}
		}
	}
	removed := map[string]bool{}
	for _, tomb := range laptop.devices.Removed() {
		removed[tomb.ID] = true
	}
	if !removed[removedOnDesk] || !removed[removedOnLaptop] {
		t.Errorf("the laptop's removals %v, want both devices", laptop.devices.Removed())
	}
}

// family.json is refused when it does not read, like the other registry
// files, and left as it was.
func TestOpenRefusesADamagedFamilyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "family.json")
	for _, body := range []string{`{`, `{"term":-1}`, `{"term":1,"credential":{"device":""}}`, `{"term":1,"pending":["x"]}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(Deps{Path: filepath.Join(dir, "upstream.json"), FamilyPath: path}); err == nil {
			t.Errorf("Open accepted family.json %s", body)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("family.json was changed to %s", got)
		}
	}
}
