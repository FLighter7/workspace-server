package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/FLighter7/workspace-server/internal/autostart"
	"github.com/FLighter7/workspace-server/internal/config"
	"github.com/FLighter7/workspace-server/internal/datadir"
	"github.com/FLighter7/workspace-server/internal/program"
)

// version is overridden at build time with -ldflags "-X main.version=<v>".
var version = "dev"

const usage = `usage: workspace-server [-dir <dir>] [-udp-port <n>] [-http-port <n>]
       workspace-server run [-dir <dir>] [-udp-port <n>] [-http-port <n>] [-background]
       workspace-server start
       workspace-server stop [-dir <dir>]
       workspace-server delete [-dir <dir>] [--yes] [--pause]
       workspace-server version

commands:
  (none)   run the server here and open its web page; if it already runs,
           open its page (this is what opening the program file does)
  run      run the server here, without opening the page (for scripts)
  start    run the server in the background, start it at every login, and
           open its web page; a running server is restarted
  stop     stop the server and stop starting it at login; nothing is deleted
  delete   stop the server, stop starting it at login, and delete its data,
           its settings and the program file, after asking
  version  print the version

flags:
  -dir <dir>        the server folder: settings, identity, devices, log;
                    by default
                      macOS: ~/Library/Application Support/workspace-server
                      Windows: %APPDATA%\workspace-server
                      Linux: $XDG_DATA_HOME/workspace-server
                             (~/.local/share/workspace-server)
  -udp-port <n>     the UDP port devices connect to, for this run only
  -http-port <n>    the port of the web page on 127.0.0.1, for this run only
  -background       release the console window at start (Windows; for
                    autostart)
  --yes             delete without asking
  --pause           wait for Enter at the end (for the Windows uninstaller)
`

func main() {
	// SIGHUP: closing a terminal window (macOS Terminal asks first) ends the
	// server gracefully instead of killing it. Windows never delivers it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	// A second signal while the server stops ends the process at once.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, os.Args[1:], realDeps())
	stop()
	os.Exit(code)
}

// serveFunc runs the server until ctx is cancelled; runServer is the real
// one.
type serveFunc func(ctx context.Context, opts serverOptions, logw io.Writer) error

// deps is everything the commands touch outside the process, so that tests
// can stand in for it.
type deps struct {
	version string // the build's; devVersion for a development build

	serve       serveFunc
	probe       func(dir string) (port int, ok bool)
	requestStop func(port int) error
	waitStopped func(dir string, timeout time.Duration) bool

	install   func(binary, dir string, httpPort int) error
	uninstall func() (removed bool, err error)
	openPage  func(httpPort int, wait time.Duration) (answered, opened bool)

	registerEntry   func(exe, version string) error
	unregisterEntry func() error
	removeProgram   func(path string) error
	removeServerDir func(dir string) error
	removeData      func(dir string) error

	binaryPath    func() (string, error)
	defaultDir    func() (string, error)
	detachConsole func()

	stdin          io.Reader
	isTerminal     func() bool
	stdout, stderr io.Writer

	// retryFor is how long interactive mode waits for a server that holds
	// the folder's lock to answer.
	retryFor time.Duration
}

func realDeps() deps {
	return deps{
		version:         version,
		serve:           runServer,
		probe:           probeServer,
		requestStop:     requestStop,
		waitStopped:     waitStopped,
		install:         autostart.Install,
		uninstall:       autostart.Uninstall,
		openPage:        autostart.OpenPage,
		registerEntry:   program.Register,
		unregisterEntry: program.Unregister,
		removeProgram:   program.Remove,
		removeServerDir: removeServerDir,
		removeData:      removeData,
		binaryPath:      autostart.BinaryPath,
		defaultDir:      datadir.Default,
		detachConsole:   detachConsole,
		stdin:           os.Stdin,
		isTerminal:      stdinIsTerminal,
		stdout:          os.Stdout,
		stderr:          os.Stderr,
		retryFor:        3 * time.Second,
	}
}

// serverDirOf returns the server folder a -dir value names: the default one
// when it is empty, otherwise the value made absolute and clean, so that it
// compares equal to the paths the server and the page derive from it.
func serverDirOf(d deps, dir string) (string, error) {
	if dir == "" {
		return d.defaultDir()
	}
	return filepath.Abs(dir)
}

// run dispatches a command and returns the process exit code: 0 success, 1
// failure, 2 a usage error.
func run(ctx context.Context, args []string, d deps) int {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "run":
		opts, err := parseRunFlags(args, d.stderr)
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if err != nil {
			return 2
		}
		if opts.Background {
			d.detachConsole()
		}
		if opts.Dir, err = serverDirOf(d, opts.Dir); err != nil {
			return fail(d, err)
		}
		if cmd == "run" {
			if err := d.serve(ctx, opts, d.stderr); err != nil {
				return fail(d, err)
			}
			return 0
		}
		return interactive(ctx, d, opts)
	case "start":
		if len(args) > 0 {
			fmt.Fprintf(d.stderr, "unexpected argument %q\n%s", args[0], usage)
			return 2
		}
		return start(d)
	case "stop", "delete":
		o, err := parseStopFlags(cmd, args, d.stderr)
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if err != nil {
			return 2
		}
		if o.dir, err = serverDirOf(d, o.dir); err != nil {
			return fail(d, err)
		}
		if cmd == "stop" {
			return stopCommand(d, o.dir)
		}
		return deleteCommand(ctx, d, o)
	case "version":
		fmt.Fprintf(d.stdout, "workspace-server %s\n", version)
		return 0
	default:
		fmt.Fprint(d.stderr, usage)
		return 2
	}
}

// fail prints err and returns exit code 1.
func fail(d deps, err error) int {
	fmt.Fprintf(d.stderr, "workspace-server: %v\n", err)
	return 1
}

// parseRunFlags parses the flags of run and of interactive mode. An empty
// Dir means the default server folder. On an error it has printed the error
// and the usage to stderr; on -h the usage alone, and the error is
// flag.ErrHelp.
func parseRunFlags(args []string, stderr io.Writer) (serverOptions, error) {
	var opts serverOptions
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.StringVar(&opts.Dir, "dir", "", "")
	fs.IntVar(&opts.UDPPort, "udp-port", 0, "")
	fs.IntVar(&opts.HTTPPort, "http-port", 0, "")
	fs.BoolVar(&opts.Background, "background", false, "")
	if err := fs.Parse(args); err != nil {
		return serverOptions{}, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		fmt.Fprintf(stderr, "%v\n", err)
		fs.Usage()
		return serverOptions{}, err
	}
	return opts, nil
}

// stopOptions are the flags of stop and delete.
type stopOptions struct {
	dir        string
	yes, pause bool
}

func parseStopFlags(cmd string, args []string, stderr io.Writer) (stopOptions, error) {
	var o stopOptions
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.StringVar(&o.dir, "dir", "", "")
	if cmd == "delete" {
		fs.BoolVar(&o.yes, "yes", false, "")
		fs.BoolVar(&o.pause, "pause", false, "")
	}
	if err := fs.Parse(args); err != nil {
		return stopOptions{}, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		fmt.Fprintf(stderr, "%v\n", err)
		fs.Usage()
		return stopOptions{}, err
	}
	return o, nil
}

// installHTTPPort reads the HTTP port from config.json in dir without
// creating it. A missing file means the default port; an unreadable or
// invalid one means the default port and a warning.
func installHTTPPort(dir string, stderr io.Writer) int {
	file := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return config.DefaultHTTPPort
	}
	var c config.Config
	if err == nil {
		if err = json.Unmarshal(data, &c); err == nil {
			err = c.Validate()
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "workspace-server: warning: %s: %v; opening the page on the default port %d\n",
			file, err, config.DefaultHTTPPort)
		return config.DefaultHTTPPort
	}
	return c.HTTPPort
}
