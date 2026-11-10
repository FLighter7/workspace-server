//go:build windows

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
)

// Install creates the logon task that runs binary on the server folder dir
// (replacing an earlier one) and starts the server now, detached and without
// a console window. It does not use "schtasks /Run": that would start the
// server the way the task does, and the task's console flashes.
func Install(binary, dir string, httpPort int) error {
	// A task an earlier install left running keeps the old binary running;
	// /End fails when it is not running.
	_ = runCommand("schtasks", "/End", "/TN", taskName)
	if err := createTask(binary, dir); err != nil {
		return err
	}
	if err := startDetached(binary, "run", "-background", "-dir", dir); err != nil {
		_ = runCommand("schtasks", "/Delete", "/TN", taskName, "/F")
		return err
	}
	fmt.Fprintf(Out, "Created the logon task %q and started the server; it starts at login.\n", taskName)
	return nil
}

// createTask registers the logon task from an XML definition written to a
// temporary file, which it deletes afterwards.
func createTask(binary, dir string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("find the current user: %w", err)
	}
	f, err := os.CreateTemp("", "workspace-server-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(utf16LEWithBOM(SchtasksTaskXML(binary, dir, u.Username)))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return runCommand("schtasks", "/Create", "/TN", taskName, "/XML", f.Name(), "/F")
}

// taskExists reports whether the logon task exists. "schtasks /Query /TN"
// fails both when there is no such task and when the query itself fails
// (access denied, the Task Scheduler service down), with the same exit status
// and a localized message. So after a failure it lists every task: a list
// that cannot be had is an error, and a list without the task means there is
// none.
func taskExists() (bool, error) {
	queryErr := runCommand("schtasks", "/Query", "/TN", taskName)
	if queryErr == nil {
		return true, nil
	}
	list, err := exec.Command("schtasks", "/Query", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false, fmt.Errorf("find out whether the logon task exists: %w", queryErr)
	}
	return taskInList(list, taskName), nil
}

// Unregister deletes the task without ending a running instance; see
// unregisterSteps. Nothing registered is not an error.
func Unregister() error {
	exists, err := taskExists()
	if err != nil || !exists {
		return err
	}
	_, err = runSteps(unregisterSteps("windows", "", 0), runCommand)
	return err
}

// Uninstall ends the running task and deletes it. removed reports whether
// the task existed. Nothing installed is not an error.
func Uninstall() (removed bool, err error) {
	exists, err := taskExists()
	if err != nil || !exists {
		return false, err
	}
	_, err = runSteps(uninstallSteps("windows", "", 0), runCommand)
	return true, err
}

func browserCommand(url string) *exec.Cmd {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
}
