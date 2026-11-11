package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDeps stands in for everything the commands touch: the server, a
// running server's page, autostart, the program file, the folders, the
// terminal. It records the calls that change something, in order.
type fakeDeps struct {
	mu    sync.Mutex
	calls []string

	out, errOut bytes.Buffer
	stdin       string
	terminal    bool

	defaultDir string
	exe        string

	// serve
	served     []serverOptions
	serveErr   error
	readyPorts []int
	// a running server: probe answers while running
	running      bool
	port         int
	answersAfter bool // probe starts answering once serve has returned
	stopWorks    bool // requestStop makes the server go
	stopErr      error
	osStopFails  bool // uninstall's OS-level stop leaves the server running
	pageSlow     bool // the page does not answer within the wait
	pageWaits    []time.Duration
	stdinR       io.Reader // replaces stdin when set
	// autostart
	registered bool
	installErr error
	// the removals
	failRemove map[string]error
}

func newFakeDeps(t *testing.T) *fakeDeps {
	return &fakeDeps{
		defaultDir: filepath.Join(t.TempDir(), "workspace-server"),
		exe:        "/opt/ws/workspace-server",
		port:       47800,
		stopWorks:  true,
		terminal:   true,
		failRemove: map[string]error{},
	}
}

func (f *fakeDeps) note(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeDeps) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeDeps) deps() deps {
	return deps{
		serve: func(_ context.Context, opts serverOptions, _ io.Writer) error {
			f.note("serve")
			f.mu.Lock()
			f.served = append(f.served, opts)
			f.mu.Unlock()
			if opts.OnReady != nil && f.serveErr == nil {
				opts.OnReady(f.port)
			}
			if f.answersAfter {
				f.running = true
			}
			return f.serveErr
		},
		probe: func(dir string) (int, bool) { return f.port, f.running },
		requestStop: func(port int) error {
			f.note("requestStop")
			if f.stopErr != nil {
				return f.stopErr
			}
			if f.stopWorks {
				f.running = false
			}
			return nil
		},
		waitStopped: func(dir string, _ time.Duration) bool {
			f.note("waitStopped")
			return !f.running
		},
		install: func(binary, dir string, port int) error {
			f.note("install " + binary + " " + dir)
			if f.installErr == nil {
				f.registered = true
			}
			return f.installErr
		},
		uninstall: func() (bool, error) {
			f.note("uninstall")
			was := f.registered
			f.registered = false
			if !f.osStopFails {
				f.running = false // the OS-level stop
			}
			return was, nil
		},
		openPage: func(port int, wait time.Duration) (bool, bool) {
			f.note("openPage")
			f.mu.Lock()
			f.pageWaits = append(f.pageWaits, wait)
			f.mu.Unlock()
			return !f.pageSlow, !f.pageSlow
		},
		registerEntry:   func(exe, version string) error { f.note("registerEntry " + exe); return nil },
		unregisterEntry: func() error { f.note("unregisterEntry"); return f.failRemove["unregisterEntry"] },
		removeProgram:   func(path string) error { f.note("removeProgram " + path); return f.failRemove["removeProgram"] },
		removeServerDir: func(dir string) error { f.note("removeServerDir " + dir); return f.failRemove["removeServerDir"] },
		removeData:      func(dir string) error { f.note("removeData " + dir); return f.failRemove["removeData"] },
		binaryPath:      func() (string, error) { return f.exe, nil },
		defaultDir:      func() (string, error) { return f.defaultDir, nil },
		detachConsole:   func() { f.note("detachConsole") },
		stdin:           f.stdinReader(),
		isTerminal:      func() bool { return f.terminal },
		stdout:          &f.out,
		stderr:          &f.errOut,
		retryFor:        200 * time.Millisecond,
	}
}

func (f *fakeDeps) stdinReader() io.Reader {
	if f.stdinR != nil {
		return f.stdinR
	}
	return strings.NewReader(f.stdin)
}

// runWith runs the command line with f's fakes and returns the exit code.
func (f *fakeDeps) run(args ...string) int {
	return f.runCtx(context.Background(), args...)
}

// runCtx is run with a context, as the signals cancel it.
func (f *fakeDeps) runCtx(ctx context.Context, args ...string) int {
	return run(ctx, args, f.deps())
}

func TestVersionCommand(t *testing.T) {
	f := newFakeDeps(t)
	if code := f.run("version"); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if got := f.out.String(); got != "workspace-server dev\n" {
		t.Fatalf("got %q", got)
	}
	if len(f.got()) != 0 {
		t.Fatalf("version did %v", f.got())
	}
}

func TestUnknownCommands(t *testing.T) {
	for _, cmd := range []string{"bogus", "install", "uninstall"} {
		f := newFakeDeps(t)
		if code := f.run(cmd); code != 2 {
			t.Errorf("%s: exit code %d, want 2", cmd, code)
		}
		if len(f.got()) != 0 {
			t.Errorf("%s did %v", cmd, f.got())
		}
		if !strings.Contains(f.errOut.String(), "usage:") {
			t.Errorf("%s: stderr %q, want the usage", cmd, f.errOut.String())
		}
	}
}

func TestUsageListsTheCommands(t *testing.T) {
	for _, want := range []string{"workspace-server start", "workspace-server stop", "workspace-server delete", "workspace-server run", "-background", "--yes", "--pause"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage lacks %q", want)
		}
	}
	for _, gone := range []string{"workspace-server install", "workspace-server uninstall", "-data"} {
		if strings.Contains(usage, gone) {
			t.Errorf("usage still names %q", gone)
		}
	}
}

// --- Interactive and run ---------------------------------------------------

func TestInteractiveRunsTheServerAndOpensThePage(t *testing.T) {
	for _, args := range [][]string{nil, {"-http-port", "5001"}} {
		f := newFakeDeps(t)
		if code := f.run(args...); code != 0 {
			t.Fatalf("%v: exit code %d, stderr %q", args, code, f.errOut.String())
		}
		if len(f.served) != 1 || f.served[0].Dir != f.defaultDir || f.served[0].OnReady == nil {
			t.Fatalf("%v: served %+v, want the default folder with OnReady", args, f.served)
		}
		if got := f.got(); !slices.Equal(got, []string{"serve", "openPage"}) {
			t.Errorf("%v: calls %v, want serve then openPage (from OnReady)", args, got)
		}
	}
}

func TestInteractiveOpensTheRunningServer(t *testing.T) {
	f := newFakeDeps(t)
	f.running = true
	if code := f.run(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got := f.got(); !slices.Equal(got, []string{"openPage"}) {
		t.Errorf("calls %v, want openPage only", got)
	}
	if want := "workspace-server is already running; opened http://localhost:47800/\n"; f.out.String() != want {
		t.Errorf("stdout %q, want %q", f.out.String(), want)
	}
}

// A server the probe did not find (another port) holds the lock; once it
// answers, the page opens.
func TestInteractiveRetriesTheProbeAfterTheLock(t *testing.T) {
	f := newFakeDeps(t)
	f.serveErr = alreadyRunningError{f.defaultDir}
	f.answersAfter = true
	if code := f.run(); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if got := f.got(); !slices.Equal(got, []string{"serve", "openPage"}) {
		t.Errorf("calls %v", got)
	}
	if !strings.Contains(f.out.String(), "already running") {
		t.Errorf("stdout %q", f.out.String())
	}
}

func TestInteractiveGivesUpWhenTheServerNeverAnswers(t *testing.T) {
	f := newFakeDeps(t)
	f.serveErr = alreadyRunningError{f.defaultDir}
	if code := f.run(); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if !strings.Contains(f.errOut.String(), "another workspace-server is already using") {
		t.Errorf("stderr %q", f.errOut.String())
	}
}

func TestRunNeitherProbesNorOpensThePage(t *testing.T) {
	f := newFakeDeps(t)
	f.running = true // even with a server answering
	if code := f.run("run"); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got := f.got(); !slices.Equal(got, []string{"serve"}) {
		t.Errorf("calls %v, want serve only", got)
	}
	if f.served[0].OnReady != nil || f.served[0].Dir != f.defaultDir {
		t.Errorf("served %+v", f.served[0])
	}
}

func TestRunBackgroundReleasesTheConsoleFirst(t *testing.T) {
	f := newFakeDeps(t)
	ws := filepath.Join(t.TempDir(), "ws")
	if code := f.run("run", "-background", "-dir", ws); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got := f.got(); !slices.Equal(got, []string{"detachConsole", "serve"}) {
		t.Errorf("calls %v", got)
	}
	if !f.served[0].Background || f.served[0].Dir != ws {
		t.Errorf("served %+v", f.served[0])
	}
}

func TestRunFlags(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	for _, args := range [][]string{
		{"run", "-dir", ws, "-udp-port", "5000", "-http-port", "5001"},
		{"run", "-dir=" + ws, "-http-port=5001", "-udp-port=5000"},
	} {
		f := newFakeDeps(t)
		if code := f.run(args...); code != 0 {
			t.Fatalf("%q: exit code %d, stderr %q", args, code, f.errOut.String())
		}
		o := f.served[0]
		if o.Dir != ws || o.UDPPort != 5000 || o.HTTPPort != 5001 || o.Background {
			t.Errorf("%q: options %+v", args, o)
		}
	}
}

// A relative -dir, or one with a trailing separator, is made absolute and
// clean, so that the main server is still recognized by its folder.
func TestDirIsMadeAbsolute(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	f := newFakeDeps(t)
	if code := f.run("run", "-dir", "rel"+string(filepath.Separator)+"x"+string(filepath.Separator)); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.errOut.String())
	}
	if want := filepath.Join(base, "rel", "x"); f.served[0].Dir != want {
		t.Errorf("served %q, want %q", f.served[0].Dir, want)
	}

	f = newFakeDeps(t)
	f.defaultDir = filepath.Join(base, "main")
	f.registered = true
	if code := f.run("stop", "-dir", "main"+string(filepath.Separator)); code != 0 {
		t.Fatalf("stop: exit code %d, stderr %q", code, f.errOut.String())
	}
	if !slices.Contains(f.got(), "uninstall") {
		t.Errorf("calls %v: a relative -dir naming the default folder is not the main server", f.got())
	}
}

func TestRunRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-bogus"}, {"run", "-bogus"}, {"-udp-port", "many"}, {"run", "extra"}, {"-data", "/tmp/ws"},
	} {
		f := newFakeDeps(t)
		if code := f.run(args...); code != 2 {
			t.Errorf("%q: exit code %d, want 2", args, code)
		}
		if len(f.got()) != 0 {
			t.Errorf("%q did %v", args, f.got())
		}
		if !strings.Contains(f.errOut.String(), "usage:") {
			t.Errorf("%q: stderr %q, want the usage", args, f.errOut.String())
		}
	}
}

func TestRunHelp(t *testing.T) {
	f := newFakeDeps(t)
	if code := f.run("-h"); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if len(f.got()) != 0 || !strings.Contains(f.errOut.String(), "-udp-port") {
		t.Fatalf("did %v, stderr %q; want the usage only", f.got(), f.errOut.String())
	}
}

func TestRunReportsAServerThatCannotStart(t *testing.T) {
	f := newFakeDeps(t)
	f.serveErr = errors.New("port taken")
	if code := f.run("run", "-dir", t.TempDir()); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if got := f.errOut.String(); got != "workspace-server: port taken\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestInstallHTTPPort(t *testing.T) {
	cases := []struct {
		name, content string // an empty content means no file
		want          int
		warns         bool
	}{
		{"missing", "", 47800, false},
		{"custom", `{"name":"pc","udpPort":50000,"httpPort":50001}`, 50001, false},
		{"invalid json", `{`, 47800, true},
		{"invalid values", `{"name":"pc","udpPort":50000,"httpPort":0}`, 47800, true},
	}
	for _, c := range cases {
		dir := t.TempDir()
		file := filepath.Join(dir, "config.json")
		if c.content != "" {
			if err := os.WriteFile(file, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var errOut bytes.Buffer
		if got := installHTTPPort(dir, &errOut); got != c.want {
			t.Errorf("%s: port %d, want %d", c.name, got, c.want)
		}
		if warned := errOut.Len() > 0; warned != c.warns {
			t.Errorf("%s: warning %q, want warning: %v", c.name, errOut.String(), c.warns)
		}
		if c.content == "" {
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: config.json was created (%v)", c.name, err)
			}
		}
	}
}
