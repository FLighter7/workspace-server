//go:build linux

package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Install writes the systemd user unit that runs binary on the server folder
// dir, enables and starts it, and restarts it so the current binary runs. If
// a systemctl step fails it disables the unit and removes the file again.
func Install(binary, dir string, httpPort int) error {
	if err := checkSystemdBinary(binary); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	unit := systemdUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unit, []byte(SystemdUnit(binary, dir)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", systemdUnitName},
		// enable --now leaves a running service alone, which would keep an
		// older binary running after a second start.
		{"--user", "restart", systemdUnitName},
	} {
		if err := runCommand("systemctl", args...); err != nil {
			_, _ = runSteps(uninstallSteps("linux", home, os.Getuid()), runCommand)
			return err
		}
	}
	fmt.Fprintf(Out, "Wrote %s and started the server; it starts at login.\n", unit)
	return nil
}

// Unregister disables the unit and deletes its file without stopping the
// calling process; see unregisterSteps. Nothing registered is not an error.
func Unregister() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	_, err = runSteps(unregisterSteps("linux", home, os.Getuid()), runCommand)
	return err
}

// Uninstall stops and disables the unit and deletes its file, the file even
// when stopping fails, so that a retry reports nothing installed. removed
// reports whether the unit file was there. Nothing installed is not an
// error.
func Uninstall() (removed bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("find the home directory: %w", err)
	}
	unit := systemdUnitPath(home)
	if _, err := os.Stat(unit); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := runSteps(uninstallSteps("linux", home, os.Getuid()), runCommand); err != nil {
		return true, fmt.Errorf("removed %s, but the server may still be enabled or running: %w", unit, err)
	}
	return true, nil
}

func browserCommand(url string) *exec.Cmd { return exec.Command("xdg-open", url) }
