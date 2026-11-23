package devices

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A device paired on its own gets a machine named after the kind of device it
// names; one paired to a machine joins it, and one paired to a machine that
// is gone gets a new one.
func TestPairToAMachine(t *testing.T) {
	r, path, _ := openFresh(t)
	phone, _, err := r.Pair("todo", "iPhone Safari (PWA)")
	if err != nil {
		t.Fatal(err)
	}
	if phone.Machine == "" {
		t.Fatal("the device got no machine")
	}
	notes, _, err := r.PairTo("notes", "iPhone Safari", phone.Machine)
	if err != nil {
		t.Fatal(err)
	}
	if notes.Machine != phone.Machine {
		t.Errorf("notes is on %q, want the phone's %q", notes.Machine, phone.Machine)
	}
	lost, _, err := r.PairTo("todo", "Windows Edge", "NoSuchMachine")
	if err != nil {
		t.Fatal(err)
	}
	if lost.Machine == "" || lost.Machine == phone.Machine || lost.Machine == "NoSuchMachine" {
		t.Errorf("a device paired to a machine that is gone is on %q", lost.Machine)
	}
	want := []Machine{{ID: phone.Machine, Name: "iPhone"}, {ID: lost.Machine, Name: "Windows"}}
	if got := r.Machines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Machines = %+v, want %+v", got, want)
	}
	server, _, err := r.PairServer("Laptop")
	if err != nil {
		t.Fatal(err)
	}
	if server.Machine != "" {
		t.Errorf("a server-kind device is on machine %q", server.Machine)
	}

	again, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Machines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Machines after reopening = %+v, want %+v", got, want)
	}
}

func TestMachineName(t *testing.T) {
	for in, want := range map[string]string{
		"iPhone Safari (PWA)": "iPhone",
		"Android Chrome":      "Android",
		"Mac Firefox":         "Mac",
		"Alex's phone":        "Alex's phone",
		"":                    "Device",
	} {
		if got := machineName(in); got != want {
			t.Errorf("machineName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenameMachine(t *testing.T) {
	r, _, _ := openFresh(t)
	d, _ := mustPair(t, r, "todo", "iPhone Safari")
	if err := r.RenameMachine(d.Machine, "  Alex's phone  "); err != nil {
		t.Fatal(err)
	}
	if got := r.Machines(); len(got) != 1 || got[0].Name != "Alex's phone" {
		t.Errorf("Machines = %+v", got)
	}
	if err := r.RenameMachine(d.Machine, "   "); !errors.Is(err, ErrInvalidName) {
		t.Errorf("an empty name: %v, want ErrInvalidName", err)
	}
	if err := r.RenameMachine("NoSuchMachine", "Phone"); !errors.Is(err, ErrNoMachine) {
		t.Errorf("an unknown machine: %v, want ErrNoMachine", err)
	}
	if err := r.RenameMachine(d.Machine, strings.Repeat("я", 100)); err != nil {
		t.Fatal(err)
	}
	if got := r.Machines()[0].Name; len([]rune(got)) != maxNameRunes {
		t.Errorf("a long name was kept as %d runes, want %d", len([]rune(got)), maxNameRunes)
	}
}

// Moving every device of one machine onto another merges them, and the
// machine left empty goes; moving one to a new machine splits it off.
func TestMoveDevice(t *testing.T) {
	r, _, _ := openFresh(t)
	todo, _ := mustPair(t, r, "todo", "iPhone Safari (PWA)")
	notes, _ := mustPair(t, r, "notes", "iPhone Safari")
	if todo.Machine == notes.Machine {
		t.Fatal("two devices paired on their own share a machine")
	}
	got, err := r.MoveDevice(notes.ID, todo.Machine)
	if err != nil || got != todo.Machine {
		t.Fatalf("MoveDevice = %q, %v; want %q", got, err, todo.Machine)
	}
	if list := r.Machines(); len(list) != 1 || list[0].ID != todo.Machine {
		t.Errorf("after the merge Machines = %+v, want the phone's alone", list)
	}

	split, err := r.MoveDevice(notes.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if split == "" || split == todo.Machine {
		t.Errorf("splitting off gave machine %q", split)
	}
	if list := r.Machines(); len(list) != 2 {
		t.Errorf("after the split Machines = %+v, want 2", list)
	}
	if again, err := r.MoveDevice(notes.ID, ""); err != nil || again != split {
		t.Errorf("a device alone on its machine moved to a new one: %q, %v; want it to stay on %q", again, err, split)
	}

	if _, err := r.MoveDevice(notes.ID, "NoSuchMachine"); !errors.Is(err, ErrNoMachine) {
		t.Errorf("to an unknown machine: %v, want ErrNoMachine", err)
	}
	if _, err := r.MoveDevice("NoSuchDevice", todo.Machine); !errors.Is(err, ErrNoDevice) {
		t.Errorf("an unknown device: %v, want ErrNoDevice", err)
	}
	server, _, _ := r.PairServer("Laptop")
	if _, err := r.MoveDevice(server.ID, todo.Machine); !errors.Is(err, ErrNoDevice) {
		t.Errorf("a server-kind device: %v, want ErrNoDevice", err)
	}
}

// A machine goes with its last device.
func TestRemoveDropsAnEmptyMachine(t *testing.T) {
	r, _, _ := openFresh(t)
	todo, _ := mustPair(t, r, "todo", "Android Chrome")
	notes, _, _ := r.PairTo("notes", "Android Chrome", todo.Machine)
	r.Remove(todo.ID)
	if list := r.Machines(); len(list) != 1 {
		t.Errorf("after removing one of two devices Machines = %+v", list)
	}
	r.Remove(notes.ID)
	if list := r.Machines(); len(list) != 0 {
		t.Errorf("after removing both Machines = %+v, want none", list)
	}
	other, _ := mustPair(t, r, "todo", "Mac Safari")
	r.RemoveApp("todo")
	if list := r.Machines(); len(list) != 0 {
		t.Errorf("after RemoveApp Machines = %+v, want none (%s's went)", list, other.Machine)
	}
}

// A file written before machines gives each device one, and is written so.
func TestOpenGivesDevicesMachines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	hash := strings.Repeat("a", 64)
	body := `{"devices":[
		{"id":"d1","app":"todo","name":"iPhone Safari","tokenHash":"` + hash + `"},
		{"id":"d2","app":"notes","name":"iPhone Safari","tokenHash":"` + hash + `","machine":"m1"}
	],"machines":[{"id":"m1","name":"Phone"},{"id":"m2","name":"Unused"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	list := r.Machines()
	if len(list) != 2 {
		t.Fatalf("Machines = %+v, want d1's new one and m1", list)
	}
	d1, _ := r.Get("d1")
	if d1.Machine == "" || d1.Machine == "m1" {
		t.Errorf("d1 is on %q", d1.Machine)
	}
	if !strings.Contains(string(readFile(t, path)), d1.Machine) {
		t.Error("the machine given at Open was not written")
	}
}

func TestOpenRejectsInvalidMachines(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for name, body := range map[string]string{
		"a machine id that is not valid": `{"devices":[],"machines":[{"id":"no spaces","name":"x"}]}`,
		"a repeated machine id":          `{"devices":[],"machines":[{"id":"m1","name":"x"},{"id":"m1","name":"y"}]}`,
		"a device's machine not valid":   `{"devices":[{"id":"d1","app":"todo","name":"x","tokenHash":"` + hash + `","machine":"no spaces"}]}`,
	} {
		path := filepath.Join(t.TempDir(), "devices.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, newClock().now); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

// A registry merged in brings its machines along; a machine this server
// already has keeps its own name.
func TestMergeKeepsMachines(t *testing.T) {
	r, _, _ := openFresh(t)
	own, _ := mustPair(t, r, "todo", "iPhone Safari")
	if err := r.RenameMachine(own.Machine, "My phone"); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("b", 64)
	list := []Entry{
		{ID: "Dev0000000000aa", App: "notes", Name: "iPhone Safari", TokenHash: hash, Machine: own.Machine, MachineName: "Their phone"},
		{ID: "Dev0000000000bb", App: "todo", Name: "Mac Safari", TokenHash: hash, Machine: "Mac0000000000ab", MachineName: "Desk"},
		{ID: "Dev0000000000cc", App: "todo", Name: "Android Chrome", TokenHash: hash},
	}
	if _, err := r.Merge(list, nil, Self{}); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, m := range r.Machines() {
		names[m.ID] = m.Name
	}
	if names[own.Machine] != "My phone" || names["Mac0000000000ab"] != "Desk" || len(names) != 3 {
		t.Errorf("Machines after the merge = %v", names)
	}
	if d, _ := r.Get("Dev0000000000aa"); d.Machine != own.Machine {
		t.Errorf("the merged device is on %q, want %q", d.Machine, own.Machine)
	}
}

// A subordinate keeps the machines its main's registry names, for its page.
func TestReplicaMachines(t *testing.T) {
	r, _, _ := openFresh(t)
	hash := strings.Repeat("c", 64)
	r.SetReplica([]Entry{
		{ID: "Dev0000000000aa", App: "todo", Name: "iPhone Safari", TokenHash: hash, Machine: "Mac0000000000aa", MachineName: "Phone"},
		{ID: "Dev0000000000bb", App: "notes", Name: "iPhone Safari", TokenHash: hash, Machine: "Mac0000000000aa", MachineName: "Phone"},
		{ID: "Dev0000000000cc", App: "todo", Name: "Windows Edge", TokenHash: hash, Machine: "Mac0000000000bb"},
	})
	want := []Machine{{ID: "Mac0000000000aa", Name: "Phone"}, {ID: "Mac0000000000bb", Name: "Windows"}}
	if got := r.ReplicaMachines(); !reflect.DeepEqual(got, want) {
		t.Errorf("ReplicaMachines = %+v, want %+v", got, want)
	}
	if got := r.Machines(); len(got) != 0 {
		t.Errorf("the main's machines were taken as this server's own: %+v", got)
	}
}

func TestEntryMachineValidate(t *testing.T) {
	hash := strings.Repeat("d", 64)
	if err := (Entry{ID: "a", Kind: KindServer, TokenHash: hash, Machine: "m1"}).Validate(); err == nil {
		t.Error("a server-kind entry on a machine was valid")
	}
	if err := (Entry{ID: "a", App: "todo", TokenHash: hash, Machine: "no spaces"}).Validate(); err == nil {
		t.Error("an entry on a machine id that is not valid was valid")
	}
	if err := (Entry{ID: "a", App: "todo", TokenHash: hash, Machine: "m1", MachineName: strings.Repeat("x", 65)}).Validate(); err == nil {
		t.Error("an entry with a machine name over 64 characters was valid")
	}
}
