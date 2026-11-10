package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/config"
	"github.com/FLighter7/workspace-server/internal/identity"
	"github.com/FLighter7/workspace-server/internal/mdns"
	"github.com/FLighter7/workspace-server/internal/netaddr"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/protocol"
	"github.com/FLighter7/workspace-server/internal/web"
)

// waitFor bounds every wait for the server to start, answer or stop.
const waitFor = 5 * time.Second

func TestRunServerServesStatus(t *testing.T) {
	dir := t.TempDir()
	udp, port := freePorts(t)

	ts := startServer(t, dir, udp, port)
	st := ts.status(t)
	if st.UDPPort != udp || st.HTTPPort != port {
		t.Errorf("status ports = UDP %d, HTTP %d; want UDP %d, HTTP %d", st.UDPPort, st.HTTPPort, udp, port)
	}
	if !strings.HasSuffix(st.MDNSName, ".local") {
		t.Errorf("status mdnsName = %q, want a .local name", st.MDNSName)
	}
	if st.ServerID == "" || st.Name == "" {
		t.Errorf("status = %+v, want a server id and a name", st)
	}
	for _, name := range []string{"identity.json", "config.json", "apps.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The pairing code carries the identity, the effective UDP port and the
	// current hosts.
	code := ts.pairingCode(t)
	id, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	fp, err := id.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	wantHosts := append([]string{st.MDNSName}, st.Addresses...)
	switch {
	case code.V != 1 || code.ID != st.ServerID || code.N != st.Name || code.P != id.ICEPwd || code.O != udp:
		t.Errorf("pairing code = %+v, want v 1, id %q, n %q, the ICE password, o %d", code, st.ServerID, st.Name, udp)
	case code.F != base64.RawURLEncoding.EncodeToString(fp[:]):
		t.Errorf("pairing code f = %q, want the base64url certificate fingerprint", code.F)
	case strings.Join(code.H, " ") != strings.Join(wantHosts, " "):
		t.Errorf("pairing code h = %v, want %v", code.H, wantHosts)
	case code.S == "":
		t.Errorf("pairing code has no secret")
	}

	ts.stop(t)
	if data, err := os.ReadFile(filepath.Join(dir, "server.log")); err != nil || len(data) == 0 {
		t.Errorf("server.log: %d bytes, error %v; want the run's log", len(data), err)
	}

	// The identity survives a restart.
	again := startServer(t, dir, udp, port)
	if st2 := again.status(t); st2.ServerID != st.ServerID || st2.MDNSName != st.MDNSName {
		t.Errorf("after a restart: serverId %q, mdnsName %q; want %q, %q", st2.ServerID, st2.MDNSName, st.ServerID, st.MDNSName)
	}
	if code2 := again.pairingCode(t); code2.P != code.P || code2.F != code.F {
		t.Errorf("after a restart: pairing code p %q, f %q; want %q, %q", code2.P, code2.F, code.P, code.F)
	}
	again.stop(t)
}

// A second server on a data directory in use, with ports of its own, fails at
// once and leaves the first one's files alone.
func TestRunServerLocksTheDataDirectory(t *testing.T) {
	dir := t.TempDir()
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)

	udp2 := freeUDPPort(t, udp, port)
	port2 := freeTCPPort(t, udp, port, udp2)
	// Already cancelled: a second server that wrongly starts stops at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runServer(ctx, serverOptions{Dir: dir, UDPPort: udp2, HTTPPort: port2}, io.Discard)
	if want := "another workspace-server is already using " + dir; err == nil || err.Error() != want {
		t.Fatalf("a second runServer on the directory = %v, want %q", err, want)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "server.log")); !strings.Contains(string(data), "starting") || strings.Contains(string(data), "already using") {
		t.Errorf("server.log after the second start:\n%s\nwant the first server's log only", data)
	}
	ts.status(t) // the first one still serves

	// The lock file stays; the next server on the directory starts.
	ts.stop(t)
	if _, err := os.Stat(filepath.Join(dir, "server.lock")); err != nil {
		t.Errorf("server.lock after a stop: %v", err)
	}
	startServer(t, dir, udp, port).stop(t)
}

// With dataDir in config.json the documents live in that folder, and the
// log, the lock and the registries stay in the server folder.
func TestRunServerKeepsTheDataInTheDataFolder(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data") // missing; its parent exists
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: dataDir})
	if err := os.MkdirAll(filepath.Join(dataDir, "apps", "todo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "apps", "todo", "data.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := startServer(t, dir, 0, port)
	if code, body := ts.send(t, http.MethodGet, "/api/apps/todo/document", ""); code != http.StatusOK || body != `{"a":1}` {
		t.Errorf("GET the document = %d %q, want 200 and the file in the data folder", code, body)
	}
	ts.stop(t)
	for _, name := range []string{"server.log", "server.lock", "identity.json", "apps.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s in the server folder: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			t.Errorf("%s is in the data folder", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "apps")); err == nil {
		t.Errorf("the server folder has an apps folder; the data belongs in %s", dataDir)
	}
}

// A missing data folder is created when its parent exists.
func TestRunServerCreatesTheDataFolder(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "new")
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: dataDir})
	startServer(t, dir, 0, port).stop(t)
	if fi, err := os.Stat(dataDir); err != nil || !fi.IsDir() {
		t.Errorf("the data folder was not created: %v", err)
	}
}

// A data folder whose parent is missing (an unmounted disk) is not created:
// the server does not start and its log says why.
func TestRunServerRefusesAnUnavailableDataFolder(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "gone", "data")
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: dataDir})
	// Already cancelled: a server that wrongly starts stops at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runServer(ctx, serverOptions{Dir: dir}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), dataDir) {
		t.Fatalf("runServer = %v, want an error naming %s", err, dataDir)
	}
	if _, statErr := os.Stat(filepath.Dir(dataDir)); statErr == nil {
		t.Error("the missing parent was created")
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "server.log")); !strings.Contains(string(log), "cannot start") || !strings.Contains(string(log), dataDir) {
		t.Errorf("server.log does not say why:\n%s", log)
	}
}

func TestEffectiveDataDir(t *testing.T) {
	if got := effectiveDataDir("/srv", config.Config{}); got != "/srv" {
		t.Errorf("empty dataDir: %q, want the server folder", got)
	}
	if got := effectiveDataDir("/srv", config.Config{DataDir: "/data"}); got != "/data" {
		t.Errorf("dataDir set: %q, want /data", got)
	}
}

// writeConfig writes config.json into the server folder dir.
func writeConfig(t *testing.T, dir string, c config.Config) {
	t.Helper()
	if err := c.Save(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRunServerRefusesEqualPorts(t *testing.T) {
	udp, _ := freePorts(t)
	err := runServer(context.Background(), serverOptions{Dir: t.TempDir(), UDPPort: udp, HTTPPort: udp}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("runServer with equal ports = %v, want an error saying they must differ", err)
	}
}

func TestSetConfigMovesTheWebPage(t *testing.T) {
	dir := t.TempDir()
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	next := freeTCPPort(t, udp, port)

	if code, body := ts.send(t, http.MethodPut, "/api/config", fmt.Sprintf(`{"httpPort":%d}`, next)); code != http.StatusNoContent {
		t.Fatalf("PUT /api/config = %d %s, want 204", code, body)
	}
	// The new port answers as soon as the change is accepted.
	old := ts.port
	ts.port = next
	if st := ts.status(t); st.HTTPPort != next {
		t.Errorf("status on the new port: httpPort %d, want %d", st.HTTPPort, next)
	}
	// The old one closes.
	deadline := time.Now().Add(waitFor)
	for {
		c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(old), time.Second)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("port %d still accepts connections %s after the move", old, waitFor)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The edited port is saved; the UDP port given as a flag is not.
	if c := readConfig(t, dir); c.HTTPPort != next || c.UDPPort != config.DefaultUDPPort {
		t.Errorf("config.json = %+v, want httpPort %d and the default udpPort", c, next)
	}
}

func TestSetConfigUDPPort(t *testing.T) {
	dir := t.TempDir()
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	before, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	// A port that cannot be bound changes nothing, the name sent with it
	// included.
	busy, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.LocalAddr().(*net.UDPAddr).Port
	if code, body := ts.send(t, http.MethodPut, "/api/config", fmt.Sprintf(`{"name":"Kitchen","udpPort":%d}`, busyPort)); code != http.StatusBadRequest {
		t.Fatalf("PUT a busy UDP port = %d %s, want 400", code, body)
	}
	if st := ts.status(t); st.UDPPort != udp || st.Name == "Kitchen" {
		t.Errorf("after a refused change: udpPort %d, name %q; want %d and the old name", st.UDPPort, st.Name, udp)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.json")); !bytes.Equal(after, before) {
		t.Errorf("config.json changed after a refused change:\n%s", after)
	}

	next := freeUDPPort(t, udp, port)
	if code, body := ts.send(t, http.MethodPut, "/api/config", fmt.Sprintf(`{"udpPort":%d}`, next)); code != http.StatusNoContent {
		t.Fatalf("PUT a free UDP port = %d %s, want 204", code, body)
	}
	if st := ts.status(t); st.UDPPort != next {
		t.Errorf("status udpPort = %d, want %d", st.UDPPort, next)
	}
	// The listener has moved: the old port is free, the new one is taken.
	if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udp}); err != nil {
		t.Errorf("the old UDP port is still bound: %v", err)
	} else {
		c.Close()
	}
	if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: next}); err == nil {
		c.Close()
		t.Errorf("the new UDP port %d is not bound", next)
	}

	// A name applies at once and is saved, with the edited UDP port and
	// without the HTTP port given as a flag.
	if code, body := ts.send(t, http.MethodPut, "/api/config", `{"name":"  Kitchen  "}`); code != http.StatusNoContent {
		t.Fatalf("PUT a name = %d %s, want 204", code, body)
	}
	if st := ts.status(t); st.Name != "Kitchen" {
		t.Errorf("status name = %q, want %q", st.Name, "Kitchen")
	}
	want := config.Config{Name: "Kitchen", UDPPort: next, HTTPPort: config.DefaultHTTPPort}
	if c := readConfig(t, dir); c != want {
		t.Errorf("config.json = %+v, want %+v", c, want)
	}
}

// A hand edit that drops an app, taken in by an Add on the page before the
// watcher saw it, still closes the dropped app's sessions.
func TestReloadAppsAfterAPageEditTookInAHandEdit(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	if err := s.AddApp(apps.App{ID: "notes"}); err != nil {
		t.Fatal(err)
	}
	_, notesCh := pairSession(t, s, "notes")
	path := filepath.Join(dir, "apps.json")
	writeFile(t, path, `{"apps":[{"id":"todo","name":"Todo"}]}`)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if err := s.AddApp(apps.App{ID: "other"}); err != nil {
		t.Fatal(err)
	}
	s.reloadApps()
	if !notesCh.isClosed() {
		t.Error("the session of the app the hand edit dropped is still open")
	}
	if _, ok := s.apps.Get("other"); !ok {
		t.Error("the page's app is not listed")
	}
}

func TestReloadAppsClosesTheSessionsOfDroppedApps(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	s.refusals.Record("notes", "Phone")
	if err := s.AddApp(apps.App{ID: "notes"}); err != nil {
		t.Fatal(err)
	}
	if r := s.Refusals(); len(r) != 0 {
		t.Errorf("refusals after adding the app = %v, want none", r)
	}
	_, todoCh := pairSession(t, s, "todo")
	_, notesCh := pairSession(t, s, "notes")
	if n := len(s.hub.Sessions()); n != 2 {
		t.Fatalf("%d sessions, want 2", n)
	}

	// apps.json edited by hand: notes is gone.
	writeFile(t, filepath.Join(dir, "apps.json"), `{"apps":[{"id":"todo","name":"Todo"}]}`)
	s.reloadApps()

	if !notesCh.isClosed() {
		t.Error("the session of the dropped app is still open")
	}
	if todoCh.isClosed() {
		t.Error("the session of a still listed app was closed")
	}
	if got := s.hub.Sessions(); len(got) != 1 || got[0].App != "todo" {
		t.Errorf("sessions = %+v, want the todo one only", got)
	}
}

func TestReloadAppsLogsEachFailureOnce(t *testing.T) {
	logs := &lockedBuffer{}
	s, dir := newTestServer(t, logs)
	path := filepath.Join(dir, "apps.json")
	count := func(s string) int { return strings.Count(logs.String(), s) }

	writeFile(t, path, "{")
	s.reloadApps()
	s.reloadApps()
	if n := count("still serving"); n != 1 {
		t.Errorf("after the same failure twice: %d failure lines, want 1\n%s", n, logs)
	}

	writeFile(t, path, `{"apps":[{"id":"Not Valid"}]}`)
	s.reloadApps()
	s.reloadApps()
	if n := count("still serving"); n != 2 {
		t.Errorf("after a different failure: %d failure lines, want 2\n%s", n, logs)
	}
	if _, ok := s.apps.Get("todo"); !ok {
		t.Error("the last good list was lost")
	}

	writeFile(t, path, `{"apps":[{"id":"todo"},{"id":"extra"}]}`)
	s.reloadApps()
	s.reloadApps()
	if n := count("valid again"); n != 1 {
		t.Errorf("after the fix: %d recovery lines, want 1\n%s", n, logs)
	}
	if _, ok := s.apps.Get("extra"); !ok {
		t.Error("the fixed file was not loaded")
	}
}

// An address change reopens the UDP sockets once; a check with no change
// reopens nothing.
func TestCheckNetworkReopensOnAnAddressChange(t *testing.T) {
	logs := &lockedBuffer{}
	s, _ := newTestServer(t, logs)
	udp, port := freePorts(t)
	s.cfg.UDPPort, s.cfg.HTTPPort = udp, port
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stop)

	// An address the machine does not have (TEST-NET-1), as if it had left.
	s.mu.Lock()
	s.addrs = []net.IP{net.IPv4(192, 0, 2, 1)}
	s.mu.Unlock()
	old := s.listener
	s.checkNetwork()
	if s.listener == nil || s.listener == old {
		t.Fatalf("the listener was not reopened\n%s", logs)
	}
	if n := strings.Count(logs.String(), "reopening the UDP sockets"); n != 1 {
		t.Errorf("%d reopen lines, want 1\n%s", n, logs)
	}
	if st := s.Status(); strings.Contains(strings.Join(st.Addresses, " "), "192.0.2.1") {
		t.Errorf("addresses %v still hold the address that left", st.Addresses)
	}

	reopened := s.listener
	s.checkNetwork()
	if s.listener != reopened {
		t.Error("the listener was reopened with no address change")
	}
	if n := strings.Count(logs.String(), "reopening the UDP sockets"); n != 1 {
		t.Errorf("after a check with no change: %d reopen lines, want 1\n%s", n, logs)
	}
}

// An address scan that fails keeps the addresses known before and reopens
// nothing; the failure is logged once, and the recovery once.
func TestCheckNetworkKeepsAddressesOnAScanError(t *testing.T) {
	logs := &lockedBuffer{}
	s, _ := newTestServer(t, logs)
	udp, port := freePorts(t)
	s.cfg.UDPPort, s.cfg.HTTPPort = udp, port
	var known []net.IP // no LAN address: the listener binds 127.0.0.1 only
	var scanErr error
	var scanMu sync.Mutex
	s.lanAddrs = func() ([]net.IP, error) {
		scanMu.Lock()
		defer scanMu.Unlock()
		if scanErr != nil {
			return nil, scanErr
		}
		return known, nil
	}
	fail := func(err error) {
		scanMu.Lock()
		defer scanMu.Unlock()
		scanErr = err
	}
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stop)
	old := s.listener
	// An address the machine does not have (TEST-NET-1), known from before.
	known = []net.IP{net.IPv4(192, 0, 2, 1)}
	s.mu.Lock()
	s.addrs = known
	s.mu.Unlock()

	fail(errors.New("list the network interfaces: busy"))
	s.checkNetwork()
	s.checkNetwork()
	if s.listener != old {
		t.Error("a failed scan reopened the listener")
	}
	if st := s.Status(); !strings.Contains(strings.Join(st.Addresses, " "), "192.0.2.1") {
		t.Errorf("addresses %v lost the known one", st.Addresses)
	}
	if n := strings.Count(logs.String(), "keeping the addresses known before"); n != 1 {
		t.Errorf("%d failure lines, want 1\n%s", n, logs)
	}

	fail(nil)
	s.checkNetwork()
	s.checkNetwork()
	if s.listener != old {
		t.Error("the recovery reopened the listener with no address change")
	}
	if n := strings.Count(logs.String(), "can be listed again"); n != 1 {
		t.Errorf("%d recovery lines, want 1\n%s", n, logs)
	}
}

// A server that cannot list its addresses at start listens on loopback and
// picks the addresses up at the next check.
func TestStartWithoutAddressList(t *testing.T) {
	logs := &lockedBuffer{}
	s, _ := newTestServer(t, logs)
	udp, port := freePorts(t)
	s.cfg.UDPPort, s.cfg.HTTPPort = udp, port
	var mu sync.Mutex
	failing := true
	s.lanAddrs = func() ([]net.IP, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return nil, errors.New("list the network interfaces: busy")
		}
		return []net.IP{net.IPv4(192, 0, 2, 1)}, nil // TEST-NET-1: binding it fails, which is logged
	}
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stop)
	if !strings.Contains(logs.String(), "listening on 127.0.0.1 only") {
		t.Errorf("log does not say why only loopback listens\n%s", logs)
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	s.checkNetwork()
	if st := s.Status(); !strings.Contains(strings.Join(st.Addresses, " "), "192.0.2.1") {
		t.Errorf("addresses %v, want the listed one after the check", st.Addresses)
	}
}

// The status says whether devices can connect: false while the UDP port
// cannot be reopened after an address change, true again once it can. The
// failure is logged once.
func TestListeningFlag(t *testing.T) {
	logs := &lockedBuffer{}
	s, _ := newTestServer(t, logs)
	udp, port := freePorts(t)
	s.cfg.UDPPort, s.cfg.HTTPPort = udp, port
	s.lanAddrs = func() ([]net.IP, error) { return nil, nil } // loopback only
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stop)
	if !s.Status().Listening {
		t.Fatal("not listening after start")
	}

	// The port is taken while the sockets are reopened.
	busy, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.UDPPort = busy.LocalAddr().(*net.UDPAddr).Port
	s.addrs = []net.IP{net.IPv4(192, 0, 2, 1)} // as if an address had left
	s.mu.Unlock()
	s.checkNetwork()
	s.checkNetwork()
	if s.Status().Listening {
		t.Error("listening while the port cannot be opened")
	}
	if n := strings.Count(logs.String(), "devices cannot connect"); n != 1 {
		t.Errorf("%d failure lines, want 1\n%s", n, logs)
	}

	busy.Close()
	s.checkNetwork()
	if !s.Status().Listening {
		t.Errorf("not listening once the port is free\n%s", logs)
	}
}

// A responder that cannot start is retried at every address check; the same
// failure is logged once, with what the responder logged while starting.
func TestResponderRetryLogsEachFailureOnce(t *testing.T) {
	logs := &lockedBuffer{}
	s, _ := newTestServer(t, logs)
	udp, port := freePorts(t)
	s.cfg.UDPPort, s.cfg.HTTPPort = udp, port
	s.lanAddrs = func() ([]net.IP, error) { return nil, nil }
	var mu sync.Mutex
	calls, reason := 0, "no LAN interface"
	saved := startMDNS
	startMDNS = func(name string, ifaces func() ([]netaddr.Interface, error), logf func(string, ...any)) (*mdns.Responder, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		logf("mdns: join on en0: refused")
		return nil, errors.New("mdns: " + reason)
	}
	t.Cleanup(func() { startMDNS = saved })
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stop)
	s.checkNetwork()
	s.checkNetwork()
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 3 {
		t.Errorf("the responder was tried %d times, want at start and at each check", n)
	}
	if got := strings.Count(logs.String(), "the mDNS responder is down"); got != 1 {
		t.Errorf("%d down lines for one repeated failure, want 1\n%s", got, logs)
	}
	if got := strings.Count(logs.String(), "join on en0"); got != 1 {
		t.Errorf("the responder's own line logged %d times, want 1\n%s", got, logs)
	}
	if s.Status().MDNSRunning {
		t.Error("mdnsRunning with no responder")
	}

	mu.Lock()
	reason = "something else"
	mu.Unlock()
	s.checkNetwork()
	if got := strings.Count(logs.String(), "the mDNS responder is down"); got != 2 {
		t.Errorf("%d down lines after a new failure, want 2\n%s", got, logs)
	}
}

// A new web page port that is taken is refused, and nothing changes.
func TestSetConfigHTTPPortBusy(t *testing.T) {
	dir := t.TempDir()
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	before, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, body := ts.send(t, http.MethodPut, "/api/config", fmt.Sprintf(`{"name":"Kitchen","httpPort":%d}`, busyPort))
	if code != http.StatusBadRequest {
		t.Fatalf("PUT /api/config = %d %s, want 400", code, body)
	}
	if st := ts.status(t); st.Name == "Kitchen" || st.HTTPPort != port {
		t.Errorf("status after the refusal: %+v", st)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.json")); !bytes.Equal(after, before) {
		t.Errorf("config.json changed:\n%s", after)
	}
}

func TestDefaultName(t *testing.T) {
	long := strings.Repeat("ж", 70)
	for host, want := range map[string]string{
		"Alexanders-MacBook-Pro.local":  "Alexanders-MacBook-Pro",
		"  Desk.LOCAL ":                 "Desk",
		"DESKTOP-1234":                  "DESKTOP-1234",
		"nas.lan":                       "nas.lan",
		long:                            strings.Repeat("ж", 64),
		strings.Repeat("a", 63) + " xy": strings.Repeat("a", 63),
		"":                              "workspace-server",
		"   ":                           "workspace-server",
		".local":                        "workspace-server",
	} {
		if got := defaultName(host); got != want {
			t.Errorf("defaultName(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestSameAddrs(t *testing.T) {
	ip := net.ParseIP
	a, b, c := ip("192.168.1.2"), ip("10.0.0.5"), ip("172.16.0.1")
	for _, tc := range []struct {
		x, y []net.IP
		want bool
	}{
		{nil, nil, true},
		{[]net.IP{a, b}, []net.IP{b, a}, true},
		{[]net.IP{a, b}, []net.IP{a, c}, false},
		{[]net.IP{a}, []net.IP{a, b}, false},
		{[]net.IP{a}, nil, false},
	} {
		if got := sameAddrs(tc.x, tc.y); got != tc.want {
			t.Errorf("sameAddrs(%v, %v) = %v, want %v", tc.x, tc.y, got, tc.want)
		}
	}
}

// testServer is runServer running in the background.
type testServer struct {
	port     int // the HTTP port requests go to
	cancel   context.CancelFunc
	done     chan error
	logs     *lockedBuffer
	stopOnce sync.Once
}

// startServer runs the server on dir with the given ports and waits until
// its status answers. The server is stopped at the end of the test.
func startServer(t *testing.T, dir string, udp, port int) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{port: port, cancel: cancel, done: make(chan error, 1), logs: &lockedBuffer{}}
	go func() {
		ts.done <- runServer(ctx, serverOptions{Dir: dir, UDPPort: udp, HTTPPort: port}, ts.logs)
	}()
	t.Cleanup(func() { ts.stop(t) })

	deadline := time.Now().Add(waitFor)
	for {
		select {
		case err := <-ts.done:
			t.Fatalf("runServer returned before serving: %v\n%s", err, ts.logs)
		default:
		}
		if code, _, err := ts.request(http.MethodGet, "/api/status", ""); err == nil && code == http.StatusOK {
			return ts
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /api/status did not answer 200 within %s\n%s", waitFor, ts.logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop cancels the server's context and waits for runServer to return.
func (ts *testServer) stop(t *testing.T) {
	t.Helper()
	ts.stopOnce.Do(func() {
		ts.cancel()
		select {
		case err := <-ts.done:
			if err != nil {
				t.Errorf("runServer returned %v", err)
			}
		case <-time.After(waitFor):
			t.Fatalf("runServer did not return within %s of the cancel\n%s", waitFor, ts.logs)
		}
	})
}

// client sends each request on a connection of its own, so no idle
// connection outlives a test.
var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: waitFor}

// request sends a request the page's guard accepts.
func (ts *testServer) request(method, path, body string) (int, []byte, error) {
	host := "127.0.0.1:" + strconv.Itoa(ts.port)
	req, err := http.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Host = host
	// As the page's api() does, on every method.
	req.Header.Set("X-Workspace-Server", "1")
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (ts *testServer) send(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	code, data, err := ts.request(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, string(data)
}

func (ts *testServer) status(t *testing.T) web.Status {
	t.Helper()
	code, body := ts.send(t, http.MethodGet, "/api/status", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/status = %d %s", code, body)
	}
	var st web.Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("status %s: %v", body, err)
	}
	return st
}

func (ts *testServer) pairingCode(t *testing.T) pairing.Code {
	t.Helper()
	code, body := ts.send(t, http.MethodPost, "/api/pairing", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/pairing = %d %s", code, body)
	}
	var resp struct{ Code string }
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	c, err := pairing.DecodeCode(resp.Code)
	if err != nil {
		t.Fatalf("pairing code %q: %v", resp.Code, err)
	}
	return c
}

// freePorts returns a UDP and a TCP port that were free a moment ago and
// differ from each other.
func freePorts(t *testing.T) (udp, tcp int) {
	t.Helper()
	udp = freeUDPPort(t)
	return udp, freeTCPPort(t, udp)
}

// freeUDPPort returns a UDP port free on every address, other than avoid.
func freeUDPPort(t *testing.T, avoid ...int) int {
	t.Helper()
	for {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{})
		if err != nil {
			t.Fatal(err)
		}
		port := c.LocalAddr().(*net.UDPAddr).Port
		c.Close()
		if !slices.Contains(avoid, port) {
			return port
		}
	}
}

// freeTCPPort returns a TCP port free on 127.0.0.1, other than avoid.
func freeTCPPort(t *testing.T, avoid ...int) int {
	t.Helper()
	for {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if !slices.Contains(avoid, port) {
			return port
		}
	}
}

func readConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c config.Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("config.json %s: %v", data, err)
	}
	return c
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestServer builds the server on a new data directory without starting
// its network.
func newTestServer(t *testing.T, logw io.Writer) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := newServer(serverOptions{Dir: dir}, log.New(logw, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// pairSession pairs a session over a fake channel for app.
func pairSession(t *testing.T, s *server, app string) (*protocol.Session, *fakeChannel) {
	t.Helper()
	secret, _ := s.secrets.Issue()
	ch := &fakeChannel{}
	sess := protocol.NewSession(ch, "192.0.2.1:50000", s.hub, protocol.Options{})
	t.Cleanup(sess.Close)
	sess.HandleText(fmt.Sprintf(`{"t":"pair","proto":1,"app":%q,"secret":%q,"name":"Phone"}`, app, secret))
	if !sess.Authenticated() {
		t.Fatalf("pairing a session for %q failed", app)
	}
	return sess, ch
}

// fakeChannel is a data channel that takes every frame and remembers whether
// it was closed.
type fakeChannel struct {
	mu     sync.Mutex
	closed bool
}

func (c *fakeChannel) SendText(string) error                { return nil }
func (c *fakeChannel) Send([]byte) error                    { return nil }
func (c *fakeChannel) BufferedAmount() uint64               { return 0 }
func (c *fakeChannel) SetBufferedAmountLowThreshold(uint64) {}
func (c *fakeChannel) OnBufferedAmountLow(func())           {}

func (c *fakeChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeChannel) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// lockedBuffer is a log writer the test can read while the server writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
