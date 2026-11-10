//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Install writes the LaunchAgent that runs binary on the server folder dir
// and loads it, replacing a loaded one so the current binary runs. If it
// cannot load the agent it removes the plist again, so that launchd does not
// load a half-installed agent at the next login.
func Install(binary, dir string, httpPort int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	plist := launchdPlistPath(home)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(LaunchdPlist(binary, dir)), 0o644); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	// Unload what an earlier install loaded; it fails when nothing is loaded.
	unloaded := runCommand("launchctl", "bootout", launchdTarget(os.Getuid())) == nil
	// launchd finishes a bootout asynchronously, and a bootstrap right after
	// it can fail with an I/O error, so retry.
	attempts := 1
	if unloaded {
		attempts = 10
	}
	for i := 1; ; i++ {
		err = runCommand("launchctl", "bootstrap", domain, plist)
		if err == nil {
			break
		}
		if i >= attempts {
			_ = os.Remove(plist)
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Fprintf(Out, "Wrote %s and started the server; it starts at login.\n", plist)
	return nil
}

// Unregister deletes the plist and then unloads the agent, without stopping
// the calling process first; see unregisterSteps. Nothing registered is not
// an error.
func Unregister() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	_, err = runSteps(unregisterSteps("darwin", home, os.Getuid()), runCommand)
	return err
}

// Uninstall unloads the agent, which stops the server it runs, and deletes
// the plist. removed reports whether the agent was loaded or the plist was
// there. Nothing installed is not an error.
func Uninstall() (removed bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("find the home directory: %w", err)
	}
	return runSteps(uninstallSteps("darwin", home, os.Getuid()), runCommand)
}

func browserCommand(url string) *exec.Cmd { return exec.Command("open", url) }
