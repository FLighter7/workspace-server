//go:build !darwin && !linux && !windows

package autostart

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Install is not available on this operating system.
func Install(binary, dir string, httpPort int) error { return errUnsupported() }

// Unregister does nothing: Install never registers anything here.
func Unregister() error { return nil }

// Uninstall reports that nothing was registered: Install never registers
// anything here, so stop and delete have nothing to remove.
func Uninstall() (bool, error) { return false, nil }

func errUnsupported() error {
	return fmt.Errorf("autostart is not supported on %s", runtime.GOOS)
}

func browserCommand(url string) *exec.Cmd { return nil }
