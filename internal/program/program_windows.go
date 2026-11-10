//go:build windows

package program

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Register writes the uninstall entry, replacing the one an earlier start
// wrote: the key's name is fixed.
func Register(exe, version string) error {
	if err := writeEntry(uninstallKey, exe, version); err != nil {
		return fmt.Errorf("list the program in Settings → Apps: %w", err)
	}
	return nil
}

// Unregister deletes the uninstall entry. A missing entry is not an error.
func Unregister() error {
	if err := deleteEntry(uninstallKey); err != nil {
		return fmt.Errorf("remove the program from Settings → Apps: %w", err)
	}
	return nil
}

// writeEntry writes Entry's values under key in HKCU, creating the key. The
// key is a parameter so that a test can use one of its own and leave the
// real list of programs alone.
func writeEntry(key, exe, version string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	for _, v := range Entry(exe, version) {
		if v.DWord != nil {
			err = k.SetDWordValue(v.Name, *v.DWord)
		} else {
			err = k.SetStringValue(v.Name, v.String)
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", v.Name, err)
		}
	}
	return nil
}

// deleteEntry deletes key from HKCU. A missing key is not an error.
func deleteEntry(key string) error {
	err := registry.DeleteKey(registry.CURRENT_USER, key)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// Remove deletes the program file after this process has exited, which
// Windows does not let it do itself. It starts a hidden PowerShell that tries
// for about ten seconds (see helperScript) and returns at once; the file is
// still there when Remove returns. The helper has no console window and a
// process group of its own, and nothing is waited for: it outlives this
// process.
func Remove(path string) error {
	if err := checkRegular(path); err != nil {
		return err
	}
	cmd := exec.Command(powershell(), "-NoProfile", "-NonInteractive", "-Command", helperScript(path))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
	// Not in a folder the user may want to delete next: the helper holds its
	// working folder open while it runs.
	cmd.Dir = systemDir()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the helper that deletes %s: %w", path, err)
	}
	return cmd.Process.Release()
}

// systemDir returns the Windows system folder, or "" (this process's own
// working folder) when it cannot be found.
func systemDir() string {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	return dir
}

// powershell returns the path of Windows PowerShell 5.1, which every Windows
// 10 and 11 has, from the system folder and not from PATH.
func powershell() string {
	if dir := systemDir(); dir != "" {
		p := filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "powershell.exe"
}
