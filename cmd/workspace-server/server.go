package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/autostart"
	"github.com/FLighter7/workspace-server/internal/config"
	"github.com/FLighter7/workspace-server/internal/datadir"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/hub"
	"github.com/FLighter7/workspace-server/internal/identity"
	"github.com/FLighter7/workspace-server/internal/mdns"
	"github.com/FLighter7/workspace-server/internal/netaddr"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/program"
	"github.com/FLighter7/workspace-server/internal/rtc"
	"github.com/FLighter7/workspace-server/internal/store"
	"github.com/FLighter7/workspace-server/internal/upstream"
	"github.com/FLighter7/workspace-server/internal/web"
	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// Files in the server folder; storeDir is in the data folder, which is the
// server folder unless config.json names another.
const (
	identityFile = "identity.json"
	configFile   = "config.json"
	appsFile     = "apps.json"
	devicesFile  = "devices.json"
	upstreamFile = "upstream.json"
	storeDir     = "apps"
	logName      = "server.log"
	lockFile     = "server.lock"
)

const (
	appsCheckEvery    = 2 * time.Second
	addressCheckEvery = 30 * time.Second
	// maxLogBytes is the most server.log ever holds; when a line would take
	// it past this, its oldest lines are cut until logKeepBytes are left.
	maxLogBytes  = 1 << 20
	logKeepBytes = maxLogBytes / 2
	// httpStopTimeout bounds how long a web server that stops waits for the
	// requests in flight.
	httpStopTimeout = 5 * time.Second
	// fallbackName names the server when the host name gives nothing usable.
	fallbackName = "workspace-server"
	// maxNameRunes is config's limit on the name.
	maxNameRunes = 64
)

// serverOptions are the flags of run. Dir is the server folder. A zero port
// means the one in config.json.
type serverOptions struct {
	Dir        string
	UDPPort    int
	HTTPPort   int
	Background bool // -background: the console was released (Windows)
	// OnReady, if set, is called once in a goroutine with the HTTP port, when
	// the server first serves; interactive mode opens the browser with it.
	OnReady func(httpPort int)
}

// errAlreadyRunning matches the error runServer returns when another server
// uses the server folder.
var errAlreadyRunning = errors.New("another workspace-server is already running")

// alreadyRunningError says which folder is in use; errors.Is matches it to
// errAlreadyRunning.
type alreadyRunningError struct{ dir string }

func (e alreadyRunningError) Error() string {
	return "another workspace-server is already using " + e.dir
}

func (e alreadyRunningError) Is(target error) bool { return target == errAlreadyRunning }

// actionKind is what the server does when it stops serving.
type actionKind int

const (
	actStop         actionKind = iota // exit; a signal or a stop request
	actReset                          // forget identity, apps, devices and data; serve again
	actMoveData                       // move to another data folder; serve again
	actDeleteServer                   // exit and remove the server from the machine
)

// action is the request that ended a run of serveOnce.
type action struct {
	kind            actionKind
	removeAutostart bool    // actStop
	dataDir         string  // actMoveData: the new folder
	verdict         verdict // actMoveData: what the move does
}

// The removals of a server that deletes itself, and what they need; tests
// replace them.
var (
	removeDataFn        = removeData
	removeServerDirFn   = removeServerDir
	unregisterEntry     = program.Unregister
	removeProgram       = program.Remove
	unregisterAutostart = autostart.Unregister
	binaryPath          = autostart.BinaryPath
	defaultServerDir    = datadir.Default
	moveDataFn          = moveData
	// resetSignals gives SIGINT, SIGTERM and SIGHUP back their default
	// action: launchctl bootout, in Unregister, ends the agent it unloads
	// with SIGTERM, and that may be this very process, which main's
	// signal.NotifyContext would otherwise catch and ignore until launchd
	// kills it.
	resetSignals = func() { signal.Reset(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) }
)

// runServer runs the server on the server folder opts.Dir until ctx is
// cancelled or the web page asks it to stop, and returns nil. A reset or a
// data-folder move restarts it in this process, with the folder's lock and
// log held throughout. After a requested stop it removes what was asked
// (autostart, or the whole server), once its files are closed. Logs go to
// logw and to server.log in the server folder. An error means the server
// could not start; another server using the folder is errAlreadyRunning.
func runServer(ctx context.Context, opts serverOptions, logw io.Writer) error {
	act, dataDir, err := runOwned(ctx, opts, logw)
	if err != nil {
		return err
	}
	exit(act, opts.Dir, dataDir, logw)
	return nil
}

// runOwned owns the server folder: it locks it, opens the log, and runs
// serveOnce until a run ends with something other than a restart. It
// returns that action and the data folder in effect at the end. Its deferred
// cleanups close the log and release the lock before the caller removes
// anything.
func runOwned(ctx context.Context, opts serverOptions, logw io.Writer) (action, string, error) {
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return action{}, "", fmt.Errorf("create the server folder: %w", err)
	}
	// Before anything in the folder is touched, the log included.
	unlock, lockErr := lockServerDir(opts.Dir)
	if errors.Is(lockErr, errLocked) {
		return action{}, "", alreadyRunningError{opts.Dir}
	}
	if lockErr == nil {
		defer unlock()
	}
	f, err := openLogFile(filepath.Join(opts.Dir, logName), maxLogBytes, logKeepBytes)
	if err != nil {
		return action{}, "", fmt.Errorf("open the log: %w", err)
	}
	defer f.Close()
	if logw == nil {
		logw = io.Discard
	}
	sink := &logSink{to: []io.Writer{f, logw}}
	logger := log.New(sink, "", log.LstdFlags)
	logger.Printf("workspace-server %s starting; server folder %s", version, opts.Dir)
	if lockErr != nil {
		logger.Printf("%v; nothing keeps a second server off the server folder", lockErr)
	}

	// The registries report the few errors they cannot return through the
	// standard logger.
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(sink)
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	}()

	var lastErr string
	var adopt []string
	ready := opts.OnReady
	restart := false
	for {
		if restart {
			beforeRestart()
		}
		act, dataDir, ports, err := serveOnce(ctx, opts, logger, lastErr, adopt, ready)
		if err != nil && restart {
			// The process lives on with the lock and the log; only autostart
			// would bring back a server that exited here. A port taken in the
			// gap, say, is often free again a moment later.
			act, dataDir, ports, err = retryRestart(ctx, opts, logger, lastErr, adopt, err)
			if err == nil && act.kind == actStop && ctx.Err() != nil {
				return act, dataDir, nil
			}
		}
		ready = nil
		if err != nil {
			// The caller shows the error; the log keeps it for a server
			// started at login, which has no terminal.
			log.New(f, "", log.LstdFlags).Printf("cannot start: %v", err)
			return action{}, "", err
		}
		lastErr, adopt, restart = "", nil, true
		// A port changed on the page outlives the restart, even where a flag
		// set it: newServer would otherwise apply the flag again.
		if opts.UDPPort != 0 {
			opts.UDPPort = ports.udp
		}
		if opts.HTTPPort != 0 {
			opts.HTTPPort = ports.http
		}
		switch act.kind {
		case actReset:
			logger.Printf("resetting: the identity, the apps, the devices and the data in %s go; the settings stay", dataDir)
			if err := resetServerDir(opts.Dir, dataDir); err != nil {
				lastErr = "reset: " + err.Error()
				logger.Printf("the reset failed: %v", err)
			}
		case actMoveData:
			if adopt, err = moveDataDir(opts.Dir, dataDir, act, logger); err != nil {
				lastErr = "move the data: " + err.Error()
				logger.Printf("the data folder stays %s: %v", dataDir, err)
			}
		default:
			return act, dataDir, nil
		}
		if ctx.Err() != nil {
			// A signal arrived during the restart.
			return action{kind: actStop}, dataDir, nil
		}
	}
}

// The retries of a restart that cannot start; variables for tests.
var (
	restartRetryEvery = 2 * time.Second
	restartRetryFor   = time.Minute
	// beforeRestart runs before each in-process restart.
	beforeRestart = func() {}
)

// retryRestart retries a restart that failed with err, every
// restartRetryEvery for restartRetryFor, until it starts or ctx is done (an
// actStop then). The page shows why the first attempt failed once it is
// back. It returns the last error when the server never starts.
func retryRestart(ctx context.Context, opts serverOptions, logger *log.Logger, lastErr string, adopt []string, err error) (action, string, servedPorts, error) {
	logger.Printf("cannot start again: %v; retrying every %s for %s", err, restartRetryEvery, restartRetryFor)
	shown := "the server could not start again at first: " + err.Error()
	if lastErr != "" {
		shown = lastErr + "; " + shown
	}
	deadline := time.Now().Add(restartRetryFor)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return action{kind: actStop}, "", servedPorts{}, nil
		case <-time.After(restartRetryEvery):
		}
		var act action
		var dataDir string
		var ports servedPorts
		act, dataDir, ports, err = serveOnce(ctx, opts, logger, shown, adopt, nil)
		if err == nil {
			return act, dataDir, ports, nil
		}
	}
	return action{}, "", servedPorts{}, err
}

// moveDataDir switches the data folder from the current one to act.dataDir:
// it copies the data there when there is any, saves config.json, and only
// then deletes the old copy, so a crash leaves either the old folder in use
// or the new one with a stale copy behind. It returns the apps to add to the
// list when the new folder's data is adopted.
func moveDataDir(serverDir, current string, act action, logger *log.Logger) (adopt []string, err error) {
	to := act.dataDir
	logger.Printf("moving the data from %s to %s", current, to)
	// The server has stopped since the page's request was checked, and the
	// folders may have changed again: a switch would leave data that
	// appeared meanwhile behind, unseen.
	c, err := checkDataDir(current, to)
	if err != nil {
		return nil, err
	}
	if c.Verdict != act.verdict {
		return nil, fmt.Errorf("the folders changed after the change was asked (it was %q, it is %q now); nothing was changed", act.verdict, c.Verdict)
	}
	if err := ensureDataDir(to); err != nil {
		return nil, err
	}
	moved := false
	if act.verdict == verdictMove || act.verdict == verdictCreate {
		has, err := hasData(current)
		if err != nil {
			return nil, err
		}
		if has {
			if err := moveDataFn(current, to); err != nil {
				return nil, err
			}
			moved = true
		}
	}
	cfgPath := filepath.Join(serverDir, configFile)
	host, _ := os.Hostname()
	cfg, err := config.Load(cfgPath, defaultName(host))
	if err != nil {
		return nil, err
	}
	cfg.DataDir = to
	if filepath.Clean(to) == filepath.Clean(serverDir) {
		cfg.DataDir = ""
	}
	if err := cfg.Save(cfgPath); err != nil {
		if moved {
			_ = removeApps(to)
		}
		return nil, fmt.Errorf("save %s: %w", cfgPath, err)
	}
	if moved {
		// The server folder stays whatever happens; another data folder goes
		// too once nothing is left in it, as the user would expect after a
		// move.
		remove := removeData
		if filepath.Clean(current) == filepath.Clean(serverDir) {
			remove = removeApps
		}
		if err := remove(current); err != nil {
			logger.Printf("the data is in %s now, but its old copy in %s stays: %v", to, current, err)
		}
	}
	if act.verdict == verdictAdopt {
		adopt = adoptableApps(to)
	}
	return adopt, nil
}

// exit performs what a requested stop asked for, after the server's files
// are closed: removing the server deletes its data, its folder, then (main
// server only) the program entry, the program file and autostart, which
// comes last because on some systems it ends the calling process. Each step
// runs even if an earlier one failed; failures go to logw, since the log
// file may be gone.
func exit(act action, serverDir, dataDir string, logw io.Writer) {
	if logw == nil {
		logw = io.Discard
	}
	report := func(what string, err error) {
		if err != nil {
			fmt.Fprintf(logw, "workspace-server: %s: %v\n", what, err)
		}
	}
	main := isMainServer(serverDir)
	switch {
	case act.kind == actDeleteServer:
		if filepath.Clean(dataDir) != filepath.Clean(serverDir) {
			report("delete the data", removeDataFn(dataDir))
		}
		report("delete the server folder", removeServerDirFn(serverDir))
		if !main {
			return
		}
		report("remove the program from the list of installed programs", unregisterEntry())
		if exe, err := binaryPath(); err != nil {
			report("find the program file", err)
		} else if err := removeProgram(exe); err != nil {
			fmt.Fprintf(logw, "workspace-server: could not delete the program file %s; delete it by hand: %v\n", exe, err)
		}
		resetSignals()
		report("remove autostart", unregisterAutostart())
	case act.kind == actStop && act.removeAutostart && main:
		resetSignals()
		report("remove autostart", unregisterAutostart())
	}
}

// isMainServer reports whether dir is the default server folder, the one
// autostart, the program file and the Windows program entry belong to.
func isMainServer(dir string) bool {
	def, err := defaultServerDir()
	return err == nil && filepath.Clean(def) == filepath.Clean(dir)
}

// servedPorts are the ports a run ended with, the page's changes included.
type servedPorts struct{ udp, http int }

// serveOnce loads the server folder and serves until ctx is cancelled or a
// request stops or restarts the server. It returns the request (actStop for
// a cancelled ctx), the data folder it served and the ports in effect at the
// end. lastErr is shown on the page as the reason the previous restart
// failed; adopt lists apps to add to the app list first; ready, if set, is
// called once the server serves.
func serveOnce(ctx context.Context, opts serverOptions, logger *log.Logger, lastErr string, adopt []string, ready func(int)) (action, string, servedPorts, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := newServer(opts, logger)
	if err != nil {
		return action{}, "", servedPorts{}, err
	}
	s.cancel, s.lastErr = cancel, lastErr
	for _, id := range adopt {
		if _, ok := s.apps.Get(id); ok {
			continue
		}
		if err := s.apps.Add(apps.App{ID: id}); err != nil {
			logger.Printf("add the app %q found in the data folder: %v", id, err)
		} else {
			logger.Printf("app %q found in the data folder; added", id)
		}
	}
	if err := s.start(); err != nil {
		return action{}, "", servedPorts{}, err
	}
	if ready != nil {
		go ready(s.httpPort())
	}
	s.watch(runCtx)
	<-runCtx.Done()
	s.logf("stopping")
	s.stop()
	s.logf("stopped")
	s.mu.Lock()
	act := s.pending
	ports := servedPorts{udp: s.cfg.UDPPort, http: s.cfg.HTTPPort}
	s.mu.Unlock()
	if act == nil {
		return action{kind: actStop}, s.dataDir, ports, nil
	}
	return *act, s.dataDir, ports, nil
}

// server is the running server: the registries and the hub, which last
// for the whole run, and the UDP listener, the mDNS responder and the web
// server, which are replaced when the addresses or the ports change.
//
// Locking: ctl serializes everything that opens or closes a listener, the
// responder or a web server (start, the address check, SetConfig, stop).
// mu guards the live settings that the hub and the web page read; it is
// never held while calling out. Lock order: ctl, then mu.
type server struct {
	id          *identity.Identity
	cert        webrtc.Certificate
	fingerprint string // base64url of the certificate's SHA-256, for pairing codes
	cfgPath     string
	apps        *apps.Registry
	devices     *devices.Registry
	store       *store.Store
	secrets     *pairing.Secrets
	refusals    *pairing.Refusals
	hub         *hub.Hub
	link        *upstream.Link // the link to an upstream server; idle on a main
	handler     http.Handler
	logger      *log.Logger
	pageLog     *log.Logger // the web server's own errors

	appsErr string // the apps.json failure last logged; only the apps watcher uses it

	serverDir string             // the server folder
	dataDir   string             // the data folder in effect
	cancel    context.CancelFunc // ends this run; set by serveOnce
	lastErr   string             // why the previous restart failed

	bg     sync.WaitGroup // the watchers
	httpWG sync.WaitGroup // web servers serving or stopping

	ctl       sync.Mutex
	fileCfg   config.Config // config.json as saved
	listener  *rtc.Listener // nil while it cannot be reopened after an address change
	listenErr string        // the listener failure last logged
	addrsErr  string        // the address-scan failure last logged
	responder *mdns.Responder
	mdnsErr   string       // the responder failure last logged
	page      *http.Server // serves the web page
	stopping  bool

	// lanAddrs and lanIfaces list the LAN addresses and interfaces; tests
	// replace them.
	lanAddrs  func() ([]net.IP, error)
	lanIfaces func() ([]netaddr.Interface, error)

	mu          sync.Mutex
	cfg         config.Config // in effect: fileCfg with the flags' ports
	addrs       []net.IP      // the LAN IPv4 addresses
	mdnsRunning bool
	listening   bool    // the UDP listener is open: devices can connect
	pending     *action // what a stop request asked for; the first one wins
	pendingMove *action // a data-folder change waiting for Restart
}

var _ web.Backend = (*server)(nil)

// newServer loads the files of the server folder, makes sure the data folder
// exists, and builds the registries and the hub. It opens no socket.
func newServer(opts serverOptions, logger *log.Logger) (*server, error) {
	dir := opts.Dir
	s := &server{
		cfgPath:   filepath.Join(dir, configFile),
		logger:    logger,
		pageLog:   log.New(logger.Writer(), "web: ", logger.Flags()|log.Lmsgprefix),
		lanAddrs:  netaddr.LANIPv4,
		lanIfaces: netaddr.LANInterfaces,
	}

	id, err := identity.LoadOrCreate(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, err
	}
	cert, err := id.Certificate()
	if err != nil {
		return nil, err
	}
	fp, err := id.Fingerprint()
	if err != nil {
		return nil, err
	}
	s.id, s.cert, s.fingerprint = id, cert, base64.RawURLEncoding.EncodeToString(fp[:])

	host, _ := os.Hostname()
	fileCfg, err := config.Load(s.cfgPath, defaultName(host))
	if err != nil {
		return nil, err
	}
	eff := fileCfg.WithOverrides(opts.UDPPort, opts.HTTPPort)
	if err := eff.Validate(); err != nil {
		return nil, fmt.Errorf("the ports of %s with -udp-port and -http-port: %w", s.cfgPath, err)
	}
	s.fileCfg, s.cfg = *fileCfg, eff
	dataDir := effectiveDataDir(dir, *fileCfg)
	s.serverDir, s.dataDir = dir, dataDir
	if err := ensureDataDir(dataDir); err != nil {
		return nil, err
	}
	if dataDir != dir {
		logger.Printf("the apps' data is in %s", dataDir)
	}

	if s.apps, err = apps.Open(filepath.Join(dir, appsFile)); err != nil {
		return nil, err
	}
	if s.devices, err = devices.Open(filepath.Join(dir, devicesFile), time.Now); err != nil {
		return nil, err
	}
	s.store = store.New(filepath.Join(dataDir, storeDir), time.Now)
	s.secrets = pairing.NewSecrets(time.Now)
	s.refusals = pairing.NewRefusals(time.Now)
	s.hub = hub.New(hub.Deps{
		ServerID: id.ServerID,
		Name:     s.name,
		Hosts:    s.hosts,
		Apps:     s.apps,
		Devices:  s.devices,
		Store:    s.store,
		Secrets:  s.secrets,
		Refusals: s.refusals,
		Now:      time.Now,
		Role:     func() string { return s.link.Role() },
		Logf:     func(format string, args ...any) { s.logf("hub: "+format, args...) },
	})
	s.link, err = upstream.Open(upstream.Deps{
		Path:     filepath.Join(dir, upstreamFile),
		ServerID: id.ServerID,
		Name:     s.name,
		Apps:     s.apps,
		Store:    s.store,
		Writer:   s.hub,
		Dial:     s.dialUpstream,
		Logf:     func(format string, args ...any) { s.logf("upstream: "+format, args...) },
	})
	if err != nil {
		return nil, err
	}
	s.handler = web.NewHandler(s, s.httpPort)
	return s, nil
}

// dialUpstream opens a session to the upstream server, resolving its .local
// name over mDNS on the LAN interfaces.
func (s *server) dialUpstream(ctx context.Context, t wsclient.Target, app string) (*wsclient.Client, error) {
	return wsclient.Dial(ctx, t, app, wsclient.DialOptions{
		Resolve: func(ctx context.Context, name string) ([]net.IP, error) {
			return mdns.Resolve(ctx, name, s.lanIfaces)
		},
	})
}

func (s *server) logf(format string, args ...any) { s.logger.Printf(format, args...) }

// start opens the UDP listener, the mDNS responder and the web page. Only
// the responder may fail to start: the server then works by IP alone.
func (s *server) start() error {
	s.ctl.Lock()
	defer s.ctl.Unlock()
	addrs, err := s.lanAddrs()
	if err != nil {
		s.addrsErr = err.Error()
		s.logf("%v; listening on 127.0.0.1 only until the next address check", err)
	}
	s.mu.Lock()
	s.addrs = addrs
	udpPort, httpPort := s.cfg.UDPPort, s.cfg.HTTPPort
	s.mu.Unlock()

	l, err := s.listen(addrs, udpPort)
	if err != nil {
		return err
	}
	ln, err := listenHTTP(httpPort)
	if err != nil {
		_ = l.Close()
		return fmt.Errorf("web page: %w", err)
	}
	s.listener = l
	s.setListening(true)
	s.logf("devices connect on UDP port %d at %s", udpPort, formatAddrs(withLoopback(addrs)))
	s.startResponder()
	s.page = s.serveHTTP(ln)
	s.logf("the web page is at http://localhost:%d/", httpPort)
	s.link.Start()
	return nil
}

// watch starts the watchers, which run until ctx is cancelled.
func (s *server) watch(ctx context.Context) {
	s.bg.Go(func() { every(ctx, appsCheckEvery, s.reloadApps) })
	s.bg.Go(func() { every(ctx, addressCheckEvery, s.checkNetwork) })
}

// every calls f every d until ctx is cancelled.
func every(ctx context.Context, d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

// stop waits for the watchers, whose context the caller has cancelled, and
// then closes the web page, the listener and the responder. It returns once
// all of them have stopped.
func (s *server) stop() {
	s.bg.Wait()
	// First: the link writes through the hub.
	s.link.Close()
	s.ctl.Lock()
	s.stopping = true // SetConfig opens nothing from now on
	srv, l, r := s.page, s.listener, s.responder
	s.page, s.listener, s.responder = nil, nil, nil
	s.ctl.Unlock()

	// Not under ctl: a SetConfig in flight waits for it, and the web server
	// waits for that request.
	if srv != nil {
		s.stopHTTP(srv)
	}
	s.httpWG.Wait()
	if l != nil {
		if err := l.Close(); err != nil {
			s.logf("close the UDP listener: %v", err)
		}
	}
	if r != nil {
		if err := r.Close(); err != nil {
			s.logf("close the mDNS responder: %v", err)
		}
	}
	s.setMDNSRunning(false)
	s.setListening(false)
}

// reloadApps rereads apps.json when it has changed on disk and closes the
// sessions of the apps it no longer lists. A file that cannot be read keeps
// the last good list; its error is logged when it first appears or changes,
// and the recovery once. Only the apps watcher calls it.
func (s *server) reloadApps() {
	before := s.apps.List()
	changed, err := s.apps.ReloadIfChanged()
	if err != nil {
		if msg := err.Error(); msg != s.appsErr {
			s.appsErr = msg
			s.logf("%v; still serving the apps read before", err)
		}
		return
	}
	if s.appsErr != "" {
		s.appsErr = ""
		s.logf("%s is valid again", appsFile)
	}
	if !changed {
		return
	}
	after := s.apps.List()
	ids := make([]string, 0, len(after))
	for _, a := range after {
		ids = append(ids, a.ID)
	}
	s.logf("%s changed; serving %s", appsFile, strings.Join(ids, ", "))
	s.hub.AppsChanged()
	// The apps with sessions too: a page edit may have taken in the hand
	// edit already, so before can lack an app that still has sessions.
	var gone []string
	for _, a := range before {
		gone = append(gone, a.ID)
	}
	for _, si := range s.hub.Sessions() {
		if si.App != "" { // a link session has no app
			gone = append(gone, si.App)
		}
	}
	slices.Sort(gone)
	for _, id := range slices.Compact(gone) {
		if !slices.Contains(ids, id) {
			s.logf("app %q is no longer listed; closing its sessions", id)
			s.hub.CloseApp(id)
		}
	}
}

// checkNetwork runs every 30 s. When the LAN addresses have changed it
// reopens the UDP sockets and the responder on the new ones; it also
// retries either of them that is down. When the addresses cannot be listed,
// it keeps the previous ones: a passing failure must not look like every
// address gone, which would end every session twice. The failure is logged
// when it first appears or changes, and the recovery once.
func (s *server) checkNetwork() {
	now, err := s.lanAddrs()
	s.ctl.Lock()
	defer s.ctl.Unlock()
	switch {
	case err != nil:
		if msg := err.Error(); msg != s.addrsErr {
			s.addrsErr = msg
			s.logf("%v; keeping the addresses known before", err)
		}
	case s.addrsErr != "":
		s.addrsErr = ""
		s.logf("the LAN addresses can be listed again")
	}
	s.mu.Lock()
	if err != nil {
		now = s.addrs
	}
	changed := !sameAddrs(s.addrs, now)
	if changed {
		s.addrs = now
	}
	port := s.cfg.UDPPort
	s.mu.Unlock()

	if changed {
		s.logf("the LAN addresses are now %s; reopening the UDP sockets and the mDNS responder", formatAddrs(now))
		if s.listener != nil {
			// Close before listening again: the new sockets take the same
			// port on the addresses that stay.
			if err := s.listener.Close(); err != nil {
				s.logf("close the UDP listener: %v", err)
			}
			s.listener = nil
		}
		s.stopResponder()
		s.link.NetworkChanged()
	}
	if s.listener == nil {
		s.reopenListener(now, port)
	}
	s.setListening(s.listener != nil)
	if s.responder == nil {
		s.startResponder()
	}
}

// reopenListener opens the UDP sockets again after an address change. A
// failure leaves them closed until the next check and is logged unless it
// repeats the last one. The caller holds ctl.
func (s *server) reopenListener(addrs []net.IP, port int) {
	l, err := s.listen(addrs, port)
	if err != nil {
		if msg := err.Error(); msg != s.listenErr {
			s.listenErr = msg
			s.logf("devices cannot connect: %v; retrying every %s", err, addressCheckEvery)
		}
		return
	}
	s.listener, s.listenErr = l, ""
	s.logf("devices connect on UDP port %d at %s", port, formatAddrs(withLoopback(addrs)))
}

// listen opens the UDP sockets for devices on port, at addrs and at
// 127.0.0.1.
func (s *server) listen(addrs []net.IP, port int) (*rtc.Listener, error) {
	return rtc.Listen(rtc.Config{
		Addresses:   withLoopback(addrs),
		Port:        port,
		ICEPwd:      s.id.ICEPwd,
		Certificate: s.cert,
		Backend:     s.hub,
		Logf:        func(format string, args ...any) { s.logf("rtc: "+format, args...) },
	})
}

// startResponder starts the mDNS responder. On failure it stays down until
// the next address check retries it; the failure is logged, with what the
// responder logged while starting, unless it repeats the last one. The
// caller holds ctl.
func (s *server) startResponder() {
	hl := &heldLog{logf: s.logf}
	r, err := startMDNS(s.id.MDNSName, s.lanIfaces, hl.printf)
	if err != nil {
		lines := hl.held()
		if key := strings.Join(append(lines, err.Error()), "\n"); key != s.mdnsErr {
			s.mdnsErr = key
			for _, line := range lines {
				s.logf("%s", line)
			}
			s.logf("the mDNS responder is down: %v; devices reach the server by IP until it runs", err)
		}
		return
	}
	hl.release()
	s.responder, s.mdnsErr = r, ""
	s.setMDNSRunning(true)
	s.logf("the mDNS responder publishes %s", s.id.MDNSName)
}

// stopResponder closes the responder if it runs. The caller holds ctl.
func (s *server) stopResponder() {
	if s.responder == nil {
		return
	}
	if err := s.responder.Close(); err != nil {
		s.logf("close the mDNS responder: %v", err)
	}
	s.responder = nil
	s.setMDNSRunning(false)
}

func (s *server) setMDNSRunning(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mdnsRunning = on
}

func (s *server) setListening(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listening = on
}

// startMDNS starts the responder; a test replaces it.
var startMDNS = mdns.Start

// listenHTTP binds the web page's port on 127.0.0.1 only.
func listenHTTP(port int) (net.Listener, error) {
	return net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// serveHTTP serves the web page on ln until the returned server stops.
func (s *server) serveHTTP(ln net.Listener) *http.Server {
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: s.pageLog}
	s.httpWG.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("the web page on %s stopped: %v", ln.Addr(), err)
		}
	})
	return srv
}

// stopHTTP stops srv, letting the requests in flight finish for a while.
func (s *server) stopHTTP(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), httpStopTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

// name, hosts and httpPort are the live settings the hub and the web page
// read.

func (s *server) name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Name
}

func (s *server) hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostsLocked()
}

// hostsLocked returns the .local name and the LAN addresses. The caller
// holds mu.
func (s *server) hostsLocked() []string {
	return append([]string{s.id.MDNSName}, addrStrings(s.addrs)...)
}

func (s *server) httpPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.HTTPPort
}

// effectiveDataDir is the data folder: the one config.json names, or the
// server folder serverDir.
func effectiveDataDir(serverDir string, cfg config.Config) string {
	if cfg.DataDir == "" {
		return serverDir
	}
	return cfg.DataDir
}

// ensureDataDir creates the data folder path when it is missing and its
// parent exists. A missing parent means an unmounted disk or a vanished
// share: creating the folder there would show the user an empty server where
// their data used to be, so it is an error instead.
func ensureDataDir(path string) error {
	fi, err := os.Stat(path)
	if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("the data folder %s is not available: not a folder", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the data folder %s is not available: %w", path, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return fmt.Errorf("the data folder %s is not available: %w", path, err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("the data folder %s is not available: %w", path, err)
	}
	return nil
}

// defaultName turns the computer's host name into the server's first name:
// trimmed, without the ".local" macOS adds, at most 64 characters, and
// "workspace-server" when nothing is left.
func defaultName(host string) string {
	const suffix = ".local"
	n := strings.TrimSpace(host)
	if len(n) >= len(suffix) && strings.EqualFold(n[len(n)-len(suffix):], suffix) {
		n = strings.TrimSpace(n[:len(n)-len(suffix)])
	}
	if r := []rune(n); len(r) > maxNameRunes {
		n = strings.TrimSpace(string(r[:maxNameRunes]))
	}
	if n == "" {
		return fallbackName
	}
	return n
}

// logSink copies every log line to each writer. A writer that fails, such
// as a closed terminal, does not keep the line from the others.
type logSink struct {
	mu sync.Mutex
	to []io.Writer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.to {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// heldLog holds the lines the responder logs while it starts, so that a
// start failing the same way at every check logs once. After release, lines
// go straight to logf.
type heldLog struct {
	logf func(string, ...any)

	mu       sync.Mutex
	lines    []string
	released bool
}

func (h *heldLog) printf(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		h.logf(format, args...)
		return
	}
	h.lines = append(h.lines, fmt.Sprintf(format, args...))
}

// held returns the lines held so far.
func (h *heldLog) held() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.lines)
}

// release logs the held lines and passes every later one straight on.
func (h *heldLog) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.released = true
	for _, line := range h.lines {
		h.logf("%s", line)
	}
	h.lines = nil
}

// sameAddrs reports whether a and b hold the same addresses, in any order.
func sameAddrs(a, b []net.IP) bool {
	x, y := addrStrings(a), addrStrings(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

func addrStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// withLoopback returns addrs and 127.0.0.1, the addresses the listener binds.
func withLoopback(addrs []net.IP) []net.IP {
	return append(slices.Clone(addrs), net.IPv4(127, 0, 0, 1))
}

func formatAddrs(ips []net.IP) string {
	if len(ips) == 0 {
		return "no address"
	}
	return strings.Join(addrStrings(ips), ", ")
}
