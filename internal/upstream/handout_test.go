package upstream

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// Handing subordinates out to devices, on real servers linked over WebRTC:
// the subordinate announces itself, the main lists it in every welcome and
// sends it its device registry, and the subordinate admits the main's
// devices, also while the main is away and after a restart.

// The subordinate announces itself at every link session, and again when
// how devices reach it changes; the main lists it in every welcome, and a
// subordinate's own welcome lists nothing. A subordinate pairs nothing.
func TestAnnounceAndWelcomeList(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })

	phone := main.device("todo")
	if got, want := phone.welcome.Subordinates, []wsclient.Server{sub.self()}; !reflect.DeepEqual(got, want) {
		t.Errorf("the main's welcome lists %+v, want %+v", got, want)
	}
	if phone.welcome.Role != RoleMain {
		t.Errorf("the main's role %q", phone.welcome.Role)
	}
	tab := sub.device("todo")
	if tab.welcome.Subordinates != nil || tab.welcome.Role != RoleSubordinate {
		t.Errorf("the subordinate's welcome %+v, want role subordinate and no list", tab.welcome)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	secret, _ := sub.secrets.Issue()
	cl := sub.dial(ctx, "todo").cl
	if _, _, err := cl.Pair(ctx, "", secret, "Phone"); !wsclient.HasCode(err, "notMain") {
		t.Errorf("pair on the subordinate: %v, want notMain", err)
	}
	if !sub.secrets.Check(secret) {
		t.Error("the refused pairing used the secret up")
	}

	sub.selfName.Store("Laptop 2")
	sub.selfPort.Store(47999)
	sub.link.SelfChanged()
	waitFor(t, "the main to keep the new announce", func() bool {
		list := main.devices.Subordinates()
		return len(list) == 1 && list[0].Name == "Laptop 2" && list[0].Port == 47999
	})
	if phone := main.device("todo"); len(phone.welcome.Subordinates) != 1 || phone.welcome.Subordinates[0].Port != 47999 {
		t.Errorf("the main's welcome after the change lists %+v", phone.welcome.Subordinates)
	}

	for _, d := range main.devices.List() {
		if d.IsServer() {
			main.hub.RemoveDevice(d.ID)
		}
	}
	if phone := main.device("todo"); phone.welcome.Subordinates == nil || len(phone.welcome.Subordinates) != 0 {
		t.Errorf("after the subordinate's removal the main lists %+v, want []", phone.welcome.Subordinates)
	}
}

// The main's registry reaches the subordinate at every link session and on
// every change: a device paired on the main is admitted on the subordinate,
// and one removed on the main is refused there, its session closed.
// upstream.json keeps the hashes, never a token.
func TestRegistryReplication(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	waitFor(t, "the link", func() bool { return sub.link.Status().Connected })

	id, token := main.pair("todo")
	waitFor(t, "the subordinate to admit the device", func() bool { _, ok := sub.devices.Get(id); return ok })
	tab := sub.connect("todo", id, token)
	if st := sub.link.Status(); st.Devices != 1 {
		t.Errorf("status %+v, want 1 device admitted", st)
	}
	raw, err := os.ReadFile(sub.linkDeps.Path)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := main.devices.Get(id)
	if !strings.Contains(string(raw), d.TokenHash) || strings.Contains(string(raw), token) {
		t.Errorf("upstream.json %s: want the token's hash and never the token", raw)
	}

	main.hub.RemoveDevice(id)
	waitFor(t, "the subordinate to drop the device", func() bool { _, ok := sub.devices.Get(id); return !ok })
	select {
	case <-tab.cl.Done():
	case <-ctx(t).Done():
		t.Fatal("the removed device's session on the subordinate stayed open")
	}
	if _, err := sub.tryConnect("todo", id, token); !wsclient.HasCode(err, "badToken") {
		t.Errorf("hello after the removal: %v, want badToken", err)
	}
}

// With the main out of reach, the subordinate still admits the main's
// devices, after its own restart too. A device removed on the main
// meanwhile is still admitted, until the next link brings the registry.
func TestReplicaWhileTheMainIsAway(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	v1 := main.write("todo", `{"v":1}`, nil)
	sub.linkTo(main)
	waitFor(t, "the first pull", func() bool { return sub.etag("todo") == v1 })
	id, token := main.pair("todo")
	kept, keptToken := main.pair("todo")
	waitFor(t, "the subordinate to admit both", func() bool {
		_, ok1 := sub.devices.Get(id)
		_, ok2 := sub.devices.Get(kept)
		return ok1 && ok2
	})

	if err := main.l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to notice", func() bool { return !sub.link.Status().Connected })
	tab := sub.connect("todo", id, token)
	away := tab.put(t, &v1, `{"v":"away"}`)

	sub.restart()
	if sub.link.Role() != RoleSubordinate {
		t.Fatal("the restarted server is not a subordinate")
	}
	tab = sub.connect("todo", id, token)
	if tab.welcome.Etag == nil || *tab.welcome.Etag != away {
		t.Errorf("after the restart the device sees %v, want %s", tab.welcome.Etag, away)
	}

	main.hub.RemoveDevice(id)
	if _, err := sub.tryConnect("todo", id, token); err != nil {
		t.Errorf("until the next link the removed device is admitted; hello: %v", err)
	}

	main.listen()
	waitFor(t, "the push after the main came back", func() bool { return main.etag("todo") == away })
	waitFor(t, "the subordinate to drop the device", func() bool { _, ok := sub.devices.Get(id); return !ok })
	if _, err := sub.tryConnect("todo", id, token); !wsclient.HasCode(err, "badToken") {
		t.Errorf("hello after the next link: %v, want badToken", err)
	}
	sub.connect("todo", kept, keptToken)
}

// A subordinate the main removed forgets the main's registry along with
// the link: the main's devices are no longer admitted there.
func TestRemovedSubordinateForgetsTheRegistry(t *testing.T) {
	main := startNode(t, "Desk")
	sub := startNode(t, "Laptop")
	sub.linkTo(main)
	id, _ := main.pair("todo")
	waitFor(t, "the subordinate to admit the device", func() bool { _, ok := sub.devices.Get(id); return ok })

	for _, d := range main.devices.List() {
		if d.IsServer() {
			main.hub.RemoveDevice(d.ID)
		}
	}
	waitFor(t, "the link to halt", func() bool { return sub.link.Status().Halted })
	if _, ok := sub.devices.Get(id); ok {
		t.Error("the main's device is still admitted")
	}
	saved, err := load(sub.linkDeps.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Devices) != 0 {
		t.Errorf("upstream.json still holds %+v", saved.Devices)
	}
}

// ctx is a context that ends at the test's deadline for the network.
func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), testDeadline)
	t.Cleanup(cancel)
	return c
}
