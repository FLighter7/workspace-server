package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/config"
)

// --- start -----------------------------------------------------------------

func TestStartRegistersAndOpensThePage(t *testing.T) {
	f := newFakeDeps(t)
	if code := f.run("start"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	want := []string{"install /opt/ws/workspace-server " + f.defaultDir, "registerEntry /opt/ws/workspace-server", "openPage"}
	if got := f.got(); !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestStartStopsTheRunningServerFirst(t *testing.T) {
	f := newFakeDeps(t)
	f.running = true
	if code := f.run("start"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	got := f.got()
	if len(got) < 3 || got[0] != "requestStop" || got[1] != "waitStopped" || !strings.HasPrefix(got[2], "install ") {
		t.Errorf("calls %v, want the stop before the install", got)
	}
}

// A running server that does not answer the stop request is stopped through
// the OS, as stop does.
func TestStartFallsBackToTheOS(t *testing.T) {
	f := newFakeDeps(t)
	f.running, f.registered, f.stopWorks = true, true, false
	if code := f.run("start"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	want := []string{"requestStop", "waitStopped", "uninstall", "waitStopped",
		"install /opt/ws/workspace-server " + f.defaultDir, "registerEntry /opt/ws/workspace-server", "openPage"}
	if got := f.got(); !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

// start waits longer for the page of the server it has just started, and
// says so when the page is not up yet instead of opening an error.
func TestStartSaysTheServerIsStillStarting(t *testing.T) {
	f := newFakeDeps(t)
	f.pageSlow = true
	if code := f.run("start"); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(f.out.String(), "The server is still starting; open http://127.0.0.1:47801/ in a moment.") {
		t.Errorf("stdout %q", f.out.String())
	}
	if strings.Contains(f.out.String(), "Opened") {
		t.Errorf("stdout %q claims the page opened", f.out.String())
	}
	if len(f.pageWaits) != 1 || f.pageWaits[0] < 30*time.Second {
		t.Errorf("waited %v for the page, want at least 30 s", f.pageWaits)
	}
}

func TestStartFailsWhenTheServerDoesNotStop(t *testing.T) {
	f := newFakeDeps(t)
	f.running, f.stopWorks, f.osStopFails = true, false, true
	if code := f.run("start"); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	for _, c := range f.got() {
		if strings.HasPrefix(c, "install") {
			t.Errorf("start installed although the server kept running: %v", f.got())
		}
	}
	if !strings.Contains(f.errOut.String(), "did not stop") {
		t.Errorf("stderr %q", f.errOut.String())
	}
}

func TestStartReportsAFailedInstall(t *testing.T) {
	f := newFakeDeps(t)
	f.installErr = errors.New("launchctl: boom")
	if code := f.run("start"); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(f.errOut.String(), "boom") {
		t.Errorf("stderr %q", f.errOut.String())
	}
}

func TestStartRefusesArguments(t *testing.T) {
	f := newFakeDeps(t)
	if code := f.run("start", "-dir", "/x"); code != 2 || len(f.got()) != 0 {
		t.Errorf("exit code %d, calls %v; want 2 and nothing done", code, f.got())
	}
}

// --- stop ------------------------------------------------------------------

func TestStopMessages(t *testing.T) {
	for _, tc := range []struct {
		running, registered bool
		want                string
	}{
		{true, true, "Stopped the server and removed autostart.\n"},
		{true, false, "Stopped the server; autostart was not installed.\n"},
		{false, true, "Removed autostart; the server was not running.\n"},
		{false, false, "Nothing to stop: the server is not running and autostart is not installed.\n"},
	} {
		f := newFakeDeps(t)
		f.running, f.registered = tc.running, tc.registered
		if code := f.run("stop"); code != 0 {
			t.Fatalf("%+v: exit code %d, stderr %q", tc, code, f.errOut.String())
		}
		if f.out.String() != tc.want {
			t.Errorf("%+v: stdout %q, want %q", tc, f.out.String(), tc.want)
		}
		for _, c := range f.got() {
			if strings.HasPrefix(c, "unregisterEntry") || strings.HasPrefix(c, "removeProgram") || strings.HasPrefix(c, "remove") {
				t.Errorf("%+v: stop removed something: %v", tc, f.got())
			}
		}
	}
}

func TestStopAsksTheServerThenUnregisters(t *testing.T) {
	f := newFakeDeps(t)
	f.running, f.registered = true, true
	f.run("stop")
	if got, want := f.got(), []string{"requestStop", "waitStopped", "uninstall", "waitStopped"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

// A server that does not answer the request is stopped by the OS through
// Uninstall.
func TestStopFallsBackToTheOS(t *testing.T) {
	f := newFakeDeps(t)
	f.running, f.registered, f.stopWorks = true, true, false
	if code := f.run("stop"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if got := f.got(); !slices.Contains(got, "uninstall") {
		t.Errorf("calls %v, want uninstall", got)
	}
}

func TestStopFailsWhenTheServerStillAnswers(t *testing.T) {
	f := newFakeDeps(t)
	f.running, f.stopWorks = true, false
	other := filepath.Join(t.TempDir(), "side") // no autostart to fall back on
	if code := f.run("stop", "-dir", other); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
}

func TestStopSideServerKeepsAutostart(t *testing.T) {
	f := newFakeDeps(t)
	f.running, f.registered = true, true
	if code := f.run("stop", "-dir", filepath.Join(t.TempDir(), "side")); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got := f.got(); slices.Contains(got, "uninstall") {
		t.Errorf("a side server's stop removed autostart: %v", got)
	}
	if f.out.String() != "Stopped the server.\n" {
		t.Errorf("stdout %q", f.out.String())
	}
}

// --- delete ----------------------------------------------------------------

// ourFolder makes the default server folder hold a server's files.
func ourFolder(t *testing.T, f *fakeDeps, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(f.defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, f.defaultDir, config.Config{Name: "x", UDPPort: 1, HTTPPort: 2, DataDir: dataDir})
	writeFile(t, filepath.Join(f.defaultDir, "identity.json"), `{"serverId":"AbCd"}`)
	writeFile(t, filepath.Join(f.defaultDir, "devices.json"), `{"devices":[{"id":"a"},{"id":"b"}]}`)
}

func TestDeleteNeedsATerminalOrYes(t *testing.T) {
	f := newFakeDeps(t)
	f.terminal, f.running, f.registered = false, true, true
	ourFolder(t, f, "")
	if code := f.run("delete"); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if len(f.got()) != 0 {
		t.Errorf("delete without a terminal did %v", f.got())
	}
	if !strings.Contains(f.errOut.String(), "--yes") {
		t.Errorf("stderr %q", f.errOut.String())
	}
}

func TestDeleteShowsWhatGoes(t *testing.T) {
	f := newFakeDeps(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	ourFolder(t, f, dataDir)
	putData(t, dataDir, "todo", `{"a":1}`, 2)
	f.stdin = "n\n"
	f.run("delete")
	out := f.out.String()
	for _, want := range []string{f.defaultDir, dataDir, "/opt/ws/workspace-server", "Delete all of this? [y/N] ", "pair again"} {
		if !strings.Contains(out, want) {
			t.Errorf("the question lacks %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"1 app", "2 paired devices"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary lacks %q:\n%s", want, out)
		}
	}
}

func TestDeleteDoesNothingWithoutYes(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "N\n", "no\n", "delete\n", ""} {
		f := newFakeDeps(t)
		f.running, f.registered = true, true
		ourFolder(t, f, "")
		f.stdin = answer
		if code := f.run("delete"); code != 1 {
			t.Errorf("answer %q: exit code %d, want 1", answer, code)
		}
		if len(f.got()) != 0 {
			t.Errorf("answer %q did %v", answer, f.got())
		}
	}
}

func TestDeleteRemovesEverythingInOrder(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", " YES \n"} {
		f := newFakeDeps(t)
		f.running, f.registered = true, true
		dataDir := filepath.Join(t.TempDir(), "data")
		ourFolder(t, f, dataDir)
		f.stdin = answer
		if code := f.run("delete"); code != 0 {
			t.Fatalf("answer %q: exit code %d, stderr %q", answer, code, f.errOut.String())
		}
		want := []string{
			"requestStop", "waitStopped", "uninstall", "waitStopped",
			"removeData " + dataDir, "removeServerDir " + f.defaultDir,
			"unregisterEntry", "removeProgram /opt/ws/workspace-server",
		}
		if got := f.got(); !slices.Equal(got, want) {
			t.Errorf("answer %q: calls\n%v\nwant\n%v", answer, got, want)
		}
	}
}

func TestDeleteYesSkipsTheQuestion(t *testing.T) {
	f := newFakeDeps(t)
	f.terminal = false
	ourFolder(t, f, "")
	if code := f.run("delete", "--yes"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if strings.Contains(f.out.String(), "[y/N]") {
		t.Error("--yes still asked")
	}
	// The data folder is the server folder: no separate data removal.
	if got := f.got(); slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "removeData") }) {
		t.Errorf("calls %v", got)
	}
}

func TestDeleteAbortsWhenTheServerKeepsRunning(t *testing.T) {
	f := newFakeDeps(t)
	ourFolder(t, f, "")
	f.running, f.stopWorks = true, false
	other := f.defaultDir
	f.defaultDir = filepath.Join(t.TempDir(), "elsewhere") // a side server: no OS-level fallback
	if code := f.run("delete", "--yes", "-dir", other); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	for _, c := range f.got() {
		if strings.HasPrefix(c, "remove") || strings.HasPrefix(c, "unregister") {
			t.Errorf("deleted although the server runs: %v", f.got())
		}
	}
}

func TestDeleteMissingFolderStillStops(t *testing.T) {
	f := newFakeDeps(t)
	f.registered = true
	if code := f.run("delete", "--yes"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if !strings.Contains(f.out.String(), "Nothing to delete in") {
		t.Errorf("stdout %q", f.out.String())
	}
	if strings.Contains(f.out.String(), "the server folder:") || strings.Contains(f.out.String(), "pair again") {
		t.Errorf("stdout %q lists a folder that does not exist", f.out.String())
	}
	if !strings.Contains(f.out.String(), "autostart at login") {
		t.Errorf("stdout %q does not list autostart", f.out.String())
	}
	if got := f.got(); !slices.Contains(got, "uninstall") {
		t.Errorf("calls %v, want the stop flow", got)
	}

	// A side server whose folder does not exist: nothing at all.
	f = newFakeDeps(t)
	missing := filepath.Join(t.TempDir(), "gone")
	if code := f.run("delete", "-dir", missing); code != 0 {
		t.Fatalf("side: exit code %d, stderr %q", code, f.errOut.String())
	}
	if out := f.out.String(); strings.Contains(out, "This deletes") || strings.Contains(out, "[y/N]") {
		t.Errorf("side: stdout %q", out)
	}
	if got := f.got(); len(got) != 0 {
		t.Errorf("side: calls %v, want none", got)
	}
}

// A console window closed under the question ends delete at once, with
// nothing deleted, instead of a read that keeps the process alive.
func TestDeleteStopsWhenTheConsoleCloses(t *testing.T) {
	for _, pause := range []bool{false, true} {
		f := newFakeDeps(t)
		r, w := io.Pipe()
		defer w.Close()
		f.stdinR = r
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		args := []string{"delete"}
		if pause {
			args = append(args, "--pause")
		}
		done := make(chan int, 1)
		go func() { done <- f.runCtx(ctx, args...) }()
		select {
		case code := <-done:
			if code != 1 {
				t.Errorf("pause %v: exit code %d, want 1", pause, code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("pause %v: delete kept waiting after the console closed", pause)
		}
		if got := f.got(); len(got) != 0 {
			t.Errorf("pause %v: calls %v, want nothing done", pause, got)
		}
		if !strings.Contains(f.out.String(), "Nothing was deleted") {
			t.Errorf("pause %v: stdout %q", pause, f.out.String())
		}
	}
}

func TestDeleteCommandOnASideServer(t *testing.T) {
	f := newFakeDeps(t)
	f.registered = true
	side := filepath.Join(t.TempDir(), "side")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(side, "identity.json"), "{}")
	if code := f.run("delete", "--yes", "-dir", side); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if got, want := f.got(), []string{"waitStopped", "removeServerDir " + side}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
	if strings.Contains(f.out.String(), "/opt/ws/workspace-server") {
		t.Errorf("a side server's question names the program file:\n%s", f.out.String())
	}
}

func TestDeleteReportsAFailedStepAndGoesOn(t *testing.T) {
	f := newFakeDeps(t)
	ourFolder(t, f, "")
	f.failRemove["removeServerDir"] = errors.New("busy")
	if code := f.run("delete", "--yes"); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if got := f.got(); !slices.Contains(got, "removeProgram /opt/ws/workspace-server") {
		t.Errorf("calls %v, want the later steps too", got)
	}
	if !strings.Contains(f.errOut.String(), "busy") {
		t.Errorf("stderr %q", f.errOut.String())
	}
}

func TestDeletePauseWaitsForEnter(t *testing.T) {
	f := newFakeDeps(t)
	ourFolder(t, f, "")
	f.stdin = "y\n\n"
	if code := f.run("delete", "--pause"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if !strings.Contains(f.out.String(), "Press Enter") {
		t.Errorf("stdout %q", f.out.String())
	}
}

// The Windows uninstaller's window must stay open on every outcome, the
// refusals included, since that is when there is something to read.
func TestDeletePauseWaitsOnEveryOutcome(t *testing.T) {
	side := filepath.Join(t.TempDir(), "side")
	writeFileAll(t, filepath.Join(side, "identity.json"), "{}")
	for _, tc := range []struct {
		name  string
		setup func(f *fakeDeps)
		args  []string
	}{
		{"no terminal", func(f *fakeDeps) { f.terminal = false; f.stdin = "\n" }, []string{"delete", "--pause"}},
		{"answered no", func(f *fakeDeps) { f.stdin = "n\n\n" }, []string{"delete", "--pause"}},
		{"server keeps running", func(f *fakeDeps) {
			f.running, f.stopWorks = true, false
			f.defaultDir = filepath.Join(f.defaultDir, "elsewhere")
			f.stdin = "\n"
		}, []string{"delete", "--pause", "--yes", "-dir", side}},
	} {
		f := newFakeDeps(t)
		tc.setup(f)
		if code := f.run(tc.args...); code == 0 {
			t.Errorf("%s: exit code 0, want a failure", tc.name)
		}
		if !strings.Contains(f.out.String(), "Press Enter") {
			t.Errorf("%s: no pause; stdout %q", tc.name, f.out.String())
		}
	}
}
