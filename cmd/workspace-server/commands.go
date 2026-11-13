package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/autostart"
	"github.com/FLighter7/workspace-server/internal/config"
)

// stopWait is how long start, stop and delete wait for a server to go after
// asking it to stop.
const stopWait = 10 * time.Second

// startPageWait is how long start waits for the page of the server it has
// just started. The first start of a new, unsigned program file can be slow
// while the system checks it (seen once on macOS: over 10 s).
const startPageWait = time.Minute

// showPage opens the page on port, waiting up to wait for it, and tells the
// user what happened. done prefixes the line when the browser opened it,
// open when it did not.
func showPage(d deps, port int, wait time.Duration, done, open string) {
	url := autostart.PageURL(port)
	switch answered, opened := d.openPage(port, wait); {
	case !answered:
		fmt.Fprintf(d.stdout, "The server is still starting; open %s in a moment.\n", url)
	case opened:
		fmt.Fprintf(d.stdout, "%s%s\n", done, url)
	default:
		fmt.Fprintf(d.stdout, "%s%s\n", open, url)
	}
}

// interactive is what opening the program file does: it runs the server in
// this console and opens its page, or, when the folder's server already
// runs, opens that one's page and exits.
func interactive(ctx context.Context, d deps, opts serverOptions) int {
	if port, ok := d.probe(opts.Dir); ok {
		alreadyRunning(d, port)
		return 0
	}
	opts.OnReady = func(port int) {
		showPage(d, port, autostart.PageWait, "Opened the web page: ", "Open the web page: ")
	}
	err := d.serve(ctx, opts, d.stderr)
	if errors.Is(err, errAlreadyRunning) {
		// A server the probe missed holds the folder: one that was still
		// starting, or one on a port config.json does not name.
		deadline := time.Now().Add(d.retryFor)
		for {
			if port, ok := d.probe(opts.Dir); ok {
				alreadyRunning(d, port)
				return 0
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if err != nil {
		return fail(d, err)
	}
	return 0
}

func alreadyRunning(d deps, port int) {
	showPage(d, port, autostart.PageWait, "workspace-server is already running; opened ", "workspace-server is already running; open ")
}

// start registers this program file to start at login on the default server
// folder, runs it now (restarting a running server so that this file is the
// one that runs), lists it among the installed programs on Windows, and
// opens the page.
func start(d deps) int {
	// First: the installed server keeps running and autostart stays as it is.
	if d.version == devVersion {
		return fail(d, errDevStart)
	}
	binary, err := d.binaryPath()
	if err != nil {
		return fail(d, err)
	}
	dir, err := d.defaultDir()
	if err != nil {
		return fail(d, err)
	}
	if port, ok := d.probe(dir); ok {
		fmt.Fprintln(d.stdout, "Stopping the running server…")
		if err := d.requestStop(port); err != nil {
			fmt.Fprintf(d.stderr, "workspace-server: %v\n", err)
		}
		if !d.waitStopped(dir, stopWait) {
			// As stop does: through the OS, for a server that does not answer.
			if _, err := d.uninstall(); err != nil {
				fmt.Fprintf(d.stderr, "workspace-server: %v\n", err)
			}
			if !d.waitStopped(dir, stopWait) {
				return fail(d, errors.New("the running server did not stop; close its window, or run \"workspace-server stop\", and try again"))
			}
		}
	}
	port := installHTTPPort(dir, d.stderr)
	if err := d.install(binary, dir, port); err != nil {
		return fail(d, err)
	}
	if err := d.registerEntry(binary, d.version); err != nil {
		fmt.Fprintf(d.stderr, "workspace-server: warning: %v\n", err)
	}
	showPage(d, port, startPageWait, "Opened the web page: ", "Open the web page: ")
	return 0
}

// stopServer asks the server of dir to stop and, for the main server,
// removes autostart, which also stops through the OS a server that did not
// answer. It reports whether a server answered, whether autostart was
// there, and whether a server still answers afterwards.
func stopServer(d deps, dir string, main bool) (wasRunning, wasRegistered, stillRunning bool, err error) {
	if port, ok := d.probe(dir); ok {
		wasRunning = true
		if err := d.requestStop(port); err != nil {
			fmt.Fprintf(d.stderr, "workspace-server: %v\n", err)
		}
		d.waitStopped(dir, stopWait)
	}
	if main {
		if wasRegistered, err = d.uninstall(); err != nil {
			return wasRunning, wasRegistered, false, err
		}
	}
	// The OS-level stop of Uninstall ends a server asynchronously, and it
	// may stop one the probe did not find: wait until the folder is free.
	stillRunning = !d.waitStopped(dir, stopWait)
	return wasRunning, wasRegistered, stillRunning, nil
}

// isMain reports whether dir is the default server folder.
func isMain(d deps, dir string) bool {
	def, err := d.defaultDir()
	return err == nil && filepath.Clean(def) == filepath.Clean(dir)
}

func stopCommand(d deps, dir string) int {
	main := isMain(d, dir)
	running, registered, still, err := stopServer(d, dir, main)
	if err != nil {
		return fail(d, err)
	}
	if still {
		return fail(d, errors.New("the server is still running; close its window and try again"))
	}
	switch {
	case !main && running:
		fmt.Fprintln(d.stdout, "Stopped the server.")
	case !main:
		fmt.Fprintln(d.stdout, "Nothing to stop: the server is not running.")
	case running && registered:
		fmt.Fprintln(d.stdout, "Stopped the server and removed autostart.")
	case running:
		fmt.Fprintln(d.stdout, "Stopped the server; autostart was not installed.")
	case registered:
		fmt.Fprintln(d.stdout, "Removed autostart; the server was not running.")
	default:
		fmt.Fprintln(d.stdout, "Nothing to stop: the server is not running and autostart is not installed.")
	}
	return 0
}

// readLine reads one line of in, or gives up when ctx is done, as when the
// console window is closed under the question: a read would block on and
// keep the process alive until the system ends it.
func readLine(ctx context.Context, in *bufio.Reader) (string, bool) {
	got := make(chan string, 1)
	go func() {
		line, _ := in.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		return line, true
	case <-ctx.Done():
		return "", false
	}
}

func deleteCommand(ctx context.Context, d deps, o stopOptions) int {
	in := bufio.NewReader(d.stdin)
	if o.pause {
		// The Windows uninstaller's window closes when this exits; keep it
		// open on every outcome, a refusal or a failure most of all.
		defer func() {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprint(d.stdout, "Press Enter to close this window.")
			readLine(ctx, in)
		}()
	}
	if def, err := d.defaultDir(); err == nil {
		if err := checkDevDirs(d.version, def, o.dir, readDataDir(o.dir)); err != nil {
			fmt.Fprintln(d.stdout, "Nothing was deleted.")
			return fail(d, err)
		}
	}
	main := isMain(d, o.dir)
	if !o.yes && !d.isTerminal() {
		fmt.Fprintln(d.stderr, "workspace-server: delete asks for confirmation; run it in a terminal, or add --yes")
		return 2
	}
	exe := ""
	if main {
		var err error
		if exe, err = d.binaryPath(); err != nil {
			fmt.Fprintf(d.stderr, "workspace-server: warning: %v\n", err)
		}
	}
	dataDir := readDataDir(o.dir)
	if !describeDeletion(d, o.dir, dataDir, exe, main) {
		return 0
	}
	if !o.yes {
		fmt.Fprint(d.stdout, "Delete all of this? [y/N] ")
		answer, ok := readLine(ctx, in)
		if !ok {
			fmt.Fprintln(d.stdout)
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); !ok || a != "y" && a != "yes" {
			fmt.Fprintln(d.stdout, "Nothing was deleted.")
			return 1
		}
	}
	_, _, still, err := stopServer(d, o.dir, main)
	if err != nil {
		return fail(d, fmt.Errorf("%w; nothing was deleted", err))
	}
	if still {
		return fail(d, errors.New("the server is still running; nothing was deleted. Close its window and try again"))
	}

	failed := false
	step := func(what string, err error) {
		if err != nil {
			failed = true
			fmt.Fprintf(d.stderr, "workspace-server: %s: %v\n", what, err)
		}
	}
	if filepath.Clean(dataDir) != filepath.Clean(o.dir) {
		step("delete the data in "+dataDir, d.removeData(dataDir))
	}
	step("delete "+o.dir, d.removeServerDir(o.dir))
	if main {
		step("remove the program from the list of installed programs", d.unregisterEntry())
		if exe != "" {
			if err := d.removeProgram(exe); err != nil {
				step("could not delete the program file "+exe+"; delete it by hand", err)
			}
		}
	}
	if failed {
		fmt.Fprintln(d.stdout, "Some of it could not be deleted; see above.")
	} else {
		fmt.Fprintln(d.stdout, "Deleted.")
	}
	if failed {
		return 1
	}
	return 0
}

// readDataDir returns the data folder config.json in the server folder dir
// names, or dir itself; it never creates the file.
func readDataDir(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		return dir
	}
	var c config.Config
	if json.Unmarshal(data, &c) != nil || c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return dir
	}
	return c.DataDir
}

// describeDeletion prints what delete removes, and reports false when
// there is nothing at all: a side server whose folder does not exist. The
// numbers are best effort.
func describeDeletion(d deps, dir, dataDir, exe string, main bool) bool {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(d.stdout, "Nothing to delete in %s: it does not exist.\n", dir)
		if !main {
			return false
		}
		// The program file and autostart may still be there.
		fmt.Fprintln(d.stdout, "This deletes:")
		if exe != "" {
			fmt.Fprintf(d.stdout, "  the program file: %s\n", exe)
		}
		fmt.Fprintln(d.stdout, "  autostart at login")
		return true
	}
	fmt.Fprintln(d.stdout, "This deletes:")
	fmt.Fprintf(d.stdout, "  the server folder: %s\n", dir)
	apps, _ := listAppData(dataDir)
	fmt.Fprintf(d.stdout, "  the apps' data: %s, %s in documents and versions",
		plural(len(apps), "app", "apps"), byteCount(treeSize(filepath.Join(dataDir, storeDir))))
	if filepath.Clean(dataDir) != filepath.Clean(dir) {
		fmt.Fprintf(d.stdout, ", in %s", filepath.Join(dataDir, storeDir))
	}
	fmt.Fprintln(d.stdout)
	fmt.Fprintf(d.stdout, "  %s\n", plural(countDevices(dir), "paired device", "paired devices"))
	if main {
		if exe != "" {
			fmt.Fprintf(d.stdout, "  the program file: %s\n", exe)
		}
		fmt.Fprintln(d.stdout, "  autostart at login")
	}
	fmt.Fprintln(d.stdout, "Every device will have to pair again.")
	return true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func byteCount(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return plural(int(n), "byte", "bytes")
}

// treeSize sums the regular files below dir.
func treeSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// countDevices counts the entries of devices.json in dir.
func countDevices(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, devicesFile))
	if err != nil {
		return 0
	}
	var f struct {
		Devices []json.RawMessage `json:"devices"`
	}
	if json.Unmarshal(data, &f) != nil {
		return 0
	}
	return len(f.Devices)
}
