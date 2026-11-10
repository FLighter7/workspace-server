//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached starts binary with args in the background and lets go of it:
// no console window, no inherited handles (its standard streams are the null
// device), and a process group of its own, so that a Ctrl+C or the closing of
// the terminal that ran this command does not reach it. It also leaves the
// job object this process may be in, which a terminal or an installer can
// set to kill its children when it closes; a job that forbids it makes
// CreateProcess fail with "access denied", and the start is tried again
// without leaving.
func startDetached(binary string, args ...string) error {
	const flags = windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP
	err := spawn(binary, args, flags|windows.CREATE_BREAKAWAY_FROM_JOB)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		err = spawn(binary, args, flags)
	}
	return err
}

func spawn(binary string, args []string, flags uint32) error {
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	// The logon task starts the server with no working folder of its own, in
	// the system folder. The same here: a folder this command ran in, or the
	// server folder itself, would be kept from deletion for as long as the
	// server runs.
	if dir, err := windows.GetSystemDirectory(); err == nil {
		cmd.Dir = dir
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s in the background: %w", binary, err)
	}
	return cmd.Process.Release()
}
