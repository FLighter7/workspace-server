//go:build !darwin && !linux && !windows

package autostart

import "testing"

// Nothing can be registered here, so stop and delete find nothing to remove
// instead of failing.
func TestUnregisterAndUninstallFindNothing(t *testing.T) {
	if err := Unregister(); err != nil {
		t.Errorf("Unregister: %v", err)
	}
	if was, err := Uninstall(); was || err != nil {
		t.Errorf("Uninstall = %v, %v; want false, nil", was, err)
	}
	if err := Install("/bin/ws", "/tmp/ws", 47801); err == nil {
		t.Error("Install succeeded where autostart is not supported")
	}
}
