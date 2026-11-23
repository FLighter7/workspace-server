package devices

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FLighter7/workspace-server/internal/identity"
)

// Machine is one physical device of the user's: a phone, a laptop. The
// devices of apps paired on it share it, so the page can show the phone once,
// with its apps, instead of once for each app.
//
// The registry cannot tell by itself that two pairings come from the same
// machine: a browser gives no hardware id, and each origin, like each web app
// installed on an iPhone, keeps its own storage, so two apps on one phone
// cannot even recognise each other. The user says so instead: when the
// pairing code is issued (PairTo), or later by moving a pairing to another
// machine (MoveDevice).
//
// Every device of an app belongs to exactly one machine, and a machine left
// with no devices is dropped. A server-kind device has none.
type Machine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var (
	// ErrNoMachine is returned for a machine id the registry does not hold.
	ErrNoMachine = errors.New("no such device")
	// ErrNoDevice is returned for a device the registry does not hold as its
	// own device of an app: unknown, a server, or a device of the main.
	ErrNoDevice = errors.New("no such paired app")
	// ErrInvalidName is returned for a machine name that is empty.
	ErrInvalidName = errors.New("the name is empty")
)

// machineKinds are the kinds of device a client names itself after (see
// deviceName in the client package): "iPhone Safari (PWA)".
var machineKinds = []string{"iPhone", "iPad", "Android", "Mac", "Windows", "Linux"}

// machineName is the name a new machine takes from the device first paired
// on it: the kind of device the client named itself after, "iPhone" for
// "iPhone Safari (PWA)", or the device's whole name when it names no known
// kind. The user can rename it.
func machineName(device string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(device), " ")
	if slices.Contains(machineKinds, first) {
		return first
	}
	return cleanName(device)
}

// entryMachineName is the name of the machine entry e names: the one its
// server gave it, or else one taken from the device.
func entryMachineName(e Entry) string {
	if strings.TrimSpace(e.MachineName) != "" {
		return cleanName(e.MachineName)
	}
	return machineName(e.Name)
}

// machineIndex returns the index of machine id, or -1. The caller holds r.mu.
func (r *Registry) machineIndex(id string) int {
	return slices.IndexFunc(r.machines, func(m Machine) bool { return m.ID == id })
}

// newMachineLocked adds a machine named after the device name and returns
// its id. The caller holds r.mu.
func (r *Registry) newMachineLocked(device string) string {
	for {
		id := identity.RandomString(idLen)
		if r.machineIndex(id) < 0 {
			r.machines = append(slices.Clip(r.machines), Machine{ID: id, Name: machineName(device)})
			return id
		}
	}
}

// tidyMachinesLocked keeps every device of an app on a machine the registry
// holds, and drops the machines left without devices. It reports whether it
// changed anything. The caller holds r.mu.
func (r *Registry) tidyMachinesLocked() bool {
	changed := false
	for i := range r.devices {
		d := &r.devices[i]
		switch {
		case d.IsServer():
			if d.Machine != "" {
				d.Machine, changed = "", true
			}
		case d.Machine == "":
			d.Machine, changed = r.newMachineLocked(d.Name), true
		case r.machineIndex(d.Machine) < 0:
			r.machines = append(slices.Clip(r.machines), Machine{ID: d.Machine, Name: machineName(d.Name)})
			changed = true
		}
	}
	used := map[string]bool{}
	for _, d := range r.devices {
		used[d.Machine] = true
	}
	kept := slices.DeleteFunc(slices.Clone(r.machines), func(m Machine) bool { return !used[m.ID] })
	if len(kept) != len(r.machines) {
		changed = true
	}
	r.machines = kept
	return changed
}

// Machines returns this server's machines, sorted by name regardless of
// case, then id.
func (r *Registry) Machines() []Machine {
	r.mu.Lock()
	out := slices.Clone(r.machines)
	r.mu.Unlock()
	sortMachines(out)
	return out
}

func sortMachines(list []Machine) {
	slices.SortFunc(list, func(a, b Machine) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// RenameMachine names machine id. The name is trimmed and cut to 64 runes;
// an empty one is ErrInvalidName, an unknown machine ErrNoMachine.
func (r *Registry) RenameMachine(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrInvalidName
	}
	name = cleanName(name)

	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.machineIndex(id)
	if i < 0 {
		return ErrNoMachine
	}
	if r.machines[i].Name == name {
		return nil
	}
	prev := r.machines
	r.machines = slices.Clone(r.machines)
	r.machines[i].Name = name
	if err := r.write(); err != nil {
		r.machines = prev
		return fmt.Errorf("write %s: %w", r.path, err)
	}
	return nil
}

// MoveDevice puts device id, this server's own device of an app, on machine,
// or on a new machine named after it when machine is "", and returns the
// machine it is on now. Moving every device of one machine onto another
// merges the two; moving one to a new machine splits it off. A machine left
// with no devices is dropped. An unknown device, a server-kind one or one of
// the main is ErrNoDevice; an unknown machine ErrNoMachine. A device alone on
// its machine that moves to a new one stays where it is.
func (r *Registry) MoveDevice(id, machine string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.indexOf(id)
	if i < 0 || r.devices[i].IsServer() {
		return "", ErrNoDevice
	}
	from := r.devices[i].Machine
	if machine == from {
		return from, nil
	}
	if machine == "" && !slices.ContainsFunc(r.devices, func(d Device) bool { return d.ID != id && d.Machine == from }) {
		return from, nil
	}
	if machine != "" && r.machineIndex(machine) < 0 {
		return "", ErrNoMachine
	}
	prevDevices, prevMachines := r.devices, r.machines
	r.devices = slices.Clone(r.devices)
	if machine == "" {
		machine = r.newMachineLocked(r.devices[i].Name)
	}
	r.devices[i].Machine = machine
	if err := r.write(); err != nil {
		r.devices, r.machines = prevDevices, prevMachines
		return "", fmt.Errorf("write %s: %w", r.path, err)
	}
	return machine, nil
}

// ReplicaMachines returns the machines of the main's devices, as its last
// registry named them (SetReplica), sorted by name, then id: a subordinate's
// page offers them when it asks the main for a pairing code.
func (r *Registry) ReplicaMachines() []Machine {
	r.mu.Lock()
	out := slices.Clone(r.replicaMachines)
	r.mu.Unlock()
	sortMachines(out)
	return out
}

// machineOf returns the machine a device of this server's own is on, and
// its name, both "" for a server-kind device. The caller holds r.mu.
func (r *Registry) machineOf(d Device) (string, string) {
	if i := r.machineIndex(d.Machine); d.Machine != "" && i >= 0 {
		return d.Machine, r.machines[i].Name
	}
	return d.Machine, ""
}
