package main

import (
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

	"github.com/FLighter7/workspace-server/internal/config"
	"github.com/FLighter7/workspace-server/internal/web"
)

// exitRecorder replaces the removals a stopping server makes, records their
// order, and passes the data and folder removals on to the real functions.
type exitRecorder struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
	// at each call, whether the server folder still existed
	dirExisted map[string]bool
	dir        string
}

func (r *exitRecorder) note(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
	_, err := os.Stat(r.dir)
	r.dirExisted[name] = err == nil
	return r.fail[name]
}

func (r *exitRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// recordExits installs an exitRecorder for the server folder dir; main says
// whether dir counts as the default server folder.
func recordExits(t *testing.T, dir string, main bool) *exitRecorder {
	t.Helper()
	r := &exitRecorder{dir: dir, fail: map[string]error{}, dirExisted: map[string]bool{}}
	saved := []any{removeDataFn, removeServerDirFn, unregisterEntry, removeProgram, unregisterAutostart, binaryPath, defaultServerDir, resetSignals}
	resetSignals = func() { r.note("signals") }
	removeDataFn = func(d string) error {
		if err := r.note("data"); err != nil {
			return err
		}
		return removeData(d)
	}
	removeServerDirFn = func(d string) error {
		if err := r.note("serverDir"); err != nil {
			return err
		}
		return removeServerDir(d)
	}
	unregisterEntry = func() error { return r.note("entry") }
	removeProgram = func(path string) error {
		if path != "/opt/ws/workspace-server" {
			t.Errorf("removeProgram(%q), want the program file", path)
		}
		return r.note("program")
	}
	unregisterAutostart = func() error { return r.note("autostart") }
	binaryPath = func() (string, error) { return "/opt/ws/workspace-server", nil }
	defaultServerDir = func() (string, error) {
		if main {
			return dir, nil
		}
		return "/somewhere/else", nil
	}
	t.Cleanup(func() {
		removeDataFn = saved[0].(func(string) error)
		removeServerDirFn = saved[1].(func(string) error)
		unregisterEntry = saved[2].(func() error)
		removeProgram = saved[3].(func(string) error)
		unregisterAutostart = saved[4].(func() error)
		binaryPath = saved[5].(func() (string, error))
		defaultServerDir = saved[6].(func() (string, error))
		resetSignals = saved[7].(func())
	})
	return r
}

// exited waits for runServer to return by itself and returns its error.
func (ts *testServer) exited(t *testing.T) error {
	t.Helper()
	var err error
	ts.stopOnce.Do(func() {
		select {
		case err = <-ts.done:
		case <-time.After(2 * waitFor):
			t.Fatalf("runServer did not return\n%s", ts.logs)
		}
	})
	return err
}

// waitStatus polls the status until ok accepts it, across a restart.
func (ts *testServer) waitStatus(t *testing.T, ok func(web.Status) bool) web.Status {
	t.Helper()
	deadline := time.Now().Add(2 * waitFor)
	for time.Now().Before(deadline) {
		if code, data, err := ts.request(http.MethodGet, "/api/status", ""); err == nil && code == http.StatusOK {
			var st web.Status
			if json.Unmarshal(data, &st) == nil && ok(st) {
				return st
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the status never matched\n%s", ts.logs)
	return web.Status{}
}

func TestStopRequestStopsTheServer(t *testing.T) {
	dir := t.TempDir()
	rec := recordExits(t, dir, true)
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	if code, body := ts.send(t, http.MethodPost, "/api/server/stop", `{}`); code != http.StatusAccepted {
		t.Fatalf("stop = %d %s", code, body)
	}
	if err := ts.exited(t); err != nil {
		t.Fatalf("runServer = %v", err)
	}
	if got := rec.got(); len(got) != 0 {
		t.Errorf("a plain stop removed %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		t.Errorf("the server folder changed: %v", err)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "server.log")); !strings.Contains(string(log), "stopped") {
		t.Errorf("the log does not say stopped:\n%s", log)
	}
}

func TestStopRequestRemovesAutostartOfTheMainServer(t *testing.T) {
	for _, main := range []bool{true, false} {
		dir := t.TempDir()
		rec := recordExits(t, dir, main)
		udp, port := freePorts(t)
		ts := startServer(t, dir, udp, port)
		ts.send(t, http.MethodPost, "/api/server/stop", `{"removeAutostart":true}`)
		if err := ts.exited(t); err != nil {
			t.Fatal(err)
		}
		want := []string{"signals", "autostart"}
		if !main {
			want = nil
		}
		if got := rec.got(); !slices.Equal(got, want) {
			t.Errorf("main %v: calls %v, want %v", main, got, want)
		}
	}
}

func TestDeleteServerRemovesEverythingInOrder(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	rec := recordExits(t, dir, true)
	rec.fail["program"] = errors.New("program file busy")
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: dataDir})
	putData(t, dataDir, "todo", `{"a":1}`, 1)
	ts := startServer(t, dir, 0, port)
	if code, body := ts.send(t, http.MethodDelete, "/api/server", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE /api/server = %d %s", code, body)
	}
	if err := ts.exited(t); err != nil {
		t.Fatalf("runServer = %v", err)
	}
	// The signal handlers go before Unregister: launchctl bootout sends
	// this very process SIGTERM, which must end it rather than be ignored.
	if got, want := rec.got(), []string{"data", "serverDir", "entry", "program", "signals", "autostart"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
	if rec.dirExisted["entry"] {
		t.Error("the server folder still existed when the program entry was removed")
	}
	for _, p := range []string{dir, dataDir} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", p)
		}
	}
	if !strings.Contains(ts.logs.String(), "program file busy") {
		t.Errorf("the failed step is not reported:\n%s", ts.logs)
	}
}

func TestDeleteSideServerKeepsTheMainServersThings(t *testing.T) {
	dir := t.TempDir()
	rec := recordExits(t, dir, false)
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	ts.send(t, http.MethodDelete, "/api/server", "")
	if err := ts.exited(t); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.got(), []string{"serverDir"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestResetStartsOverWithTheSameSettings(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Attic", UDPPort: udp, HTTPPort: port})
	writeFile(t, filepath.Join(dir, "devices.json"), `{"devices":[{"id":"Q2xhdWRlUm9ja3Mx","app":"todo","name":"iPhone","tokenHash":"`+strings.Repeat("ab", 32)+`","pairedAt":"2026-11-08T10:00:00Z","lastSeen":"2026-11-08T10:00:00Z"}]}`)
	putData(t, dir, "todo", `{"a":1}`, 2)
	ts := startServer(t, dir, 0, port)
	before := ts.status(t)
	if code, _ := ts.send(t, http.MethodGet, "/api/devices", ""); code != http.StatusOK {
		t.Fatal("devices")
	}
	if code, body := ts.send(t, http.MethodDelete, "/api/data", ""); code != http.StatusAccepted || !strings.Contains(body, "restarting") {
		t.Fatalf("DELETE /api/data = %d %s", code, body)
	}
	after := ts.waitStatus(t, func(st web.Status) bool { return st.ServerID != before.ServerID })
	if after.Name != "Attic" || after.HTTPPort != port || after.UDPPort != udp {
		t.Errorf("after the reset: %+v, want the same name and ports", after)
	}
	if _, body := ts.send(t, http.MethodGet, "/api/devices", ""); strings.TrimSpace(body) != "[]" {
		t.Errorf("devices after the reset: %s", body)
	}
	var list []web.AppInfo
	_, body := ts.send(t, http.MethodGet, "/api/apps", "")
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) != 1 || list[0].ID != "todo" || list[0].Size != 0 || list[0].Versions != 0 {
		t.Errorf("apps after the reset: %s, want the default todo without data", body)
	}
	// The lock stayed taken across the restart.
	if !lockHeldElsewhere(t, dir) {
		t.Error("the server folder is not locked after the reset")
	}
	log, _ := os.ReadFile(filepath.Join(dir, "server.log"))
	if !strings.Contains(string(log), "resetting") || strings.Count(string(log), " starting; ") != 1 {
		t.Errorf("the log after the reset:\n%s", log)
	}
	if readConfig(t, dir).Name != "Attic" {
		t.Error("config.json changed")
	}
}

func TestMoveDataToANewFolder(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	udp, port := freePorts(t)
	putData(t, dir, "todo", `{"a":1}`, 1)
	ts := startServer(t, dir, udp, port)
	target := filepath.Join(t.TempDir(), "new")

	code, body := ts.send(t, http.MethodGet, "/api/data-dir?path="+target, "")
	if code != http.StatusOK || !strings.Contains(body, `"verdict":"create"`) || !strings.Contains(body, `"id":"todo"`) {
		t.Fatalf("check = %d %s", code, body)
	}
	if code, body := ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "create")); code != http.StatusAccepted {
		t.Fatalf("PUT dataDir = %d %s", code, body)
	}
	st := ts.waitStatus(t, func(st web.Status) bool { return st.DataDir == target })
	if st.LastError != "" {
		t.Errorf("lastError %q", st.LastError)
	}
	if code, body := ts.send(t, http.MethodGet, "/api/apps/todo/document", ""); code != http.StatusOK || body != `{"a":1}` {
		t.Errorf("the document after the move: %d %q", code, body)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the old apps folder is still there")
	}
	if got := readConfig(t, dir).DataDir; got != target {
		t.Errorf("config.json dataDir %q, want %q", got, target)
	}
}

func TestMoveDataBackToTheServerFolderStoresNoPath(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	other := t.TempDir()
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: other})
	putData(t, other, "todo", `{"a":1}`, 0)
	ts := startServer(t, dir, 0, port)
	ts.send(t, http.MethodPut, "/api/config", dataDirBody(dir, "move"))
	ts.waitStatus(t, func(st web.Status) bool { return st.DataDir == dir })
	if got := readConfig(t, dir).DataDir; got != "" {
		t.Errorf("config.json dataDir %q, want empty for the server folder", got)
	}
	if code, _ := ts.send(t, http.MethodGet, "/api/apps/todo/document", ""); code != http.StatusOK {
		t.Error("the document did not come back")
	}
}

func TestAdoptDataListsItsApps(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	target := t.TempDir()
	putData(t, target, "notes", `{"n":1}`, 0)
	if code, body := ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "adopt")); code != http.StatusAccepted {
		t.Fatalf("PUT dataDir = %d %s", code, body)
	}
	ts.waitStatus(t, func(st web.Status) bool { return st.DataDir == target })
	_, body := ts.send(t, http.MethodGet, "/api/apps", "")
	if !strings.Contains(body, `"id":"notes"`) || !strings.Contains(body, `"id":"todo"`) {
		t.Errorf("apps after adopting: %s, want notes added to todo", body)
	}
	if code, body := ts.send(t, http.MethodGet, "/api/apps/notes/document", ""); code != http.StatusOK || body != `{"n":1}` {
		t.Errorf("the adopted document: %d %q", code, body)
	}
}

func TestMoveDataRefusesAConflict(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	udp, port := freePorts(t)
	putData(t, dir, "todo", `{"a":1}`, 0)
	ts := startServer(t, dir, udp, port)
	before := ts.status(t)
	target := t.TempDir()
	putData(t, target, "todo", `{"b":2}`, 0)
	code, body := ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "move"))
	if code != http.StatusConflict || !strings.Contains(body, "dataExists") {
		t.Fatalf("PUT dataDir = %d %s, want 409 dataExists", code, body)
	}
	if n := strings.Count(body, "already holds data"); n != 1 {
		t.Errorf("the answer says it %d times: %s", n, body)
	}
	time.Sleep(200 * time.Millisecond)
	if st := ts.status(t); st.ServerID != before.ServerID || st.DataDir != dir {
		t.Errorf("the server changed: %+v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "apps", "todo", "data.json")); string(b) != `{"b":2}` {
		t.Errorf("the target's data changed: %q", b)
	}
}

func TestMoveDataFailureKeepsTheOldFolder(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	saved := moveDataFn
	moveDataFn = func(from, to string) error { return errors.New("disk full") }
	t.Cleanup(func() { moveDataFn = saved })
	udp, port := freePorts(t)
	putData(t, dir, "todo", `{"a":1}`, 0)
	ts := startServer(t, dir, udp, port)
	target := t.TempDir()
	if code, body := ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "move")); code != http.StatusAccepted {
		t.Fatalf("PUT dataDir = %d %s", code, body)
	}
	st := ts.waitStatus(t, func(st web.Status) bool { return st.LastError != "" })
	if st.DataDir != dir || !strings.Contains(st.LastError, "disk full") {
		t.Errorf("after a failed move: dataDir %q, lastError %q", st.DataDir, st.LastError)
	}
	if code, _ := ts.send(t, http.MethodGet, "/api/apps/todo/document", ""); code != http.StatusOK {
		t.Error("the document is gone")
	}
	if readConfig(t, dir).DataDir != "" {
		t.Error("config.json points at the new folder")
	}
}

func TestSecondRequestWhileStoppingIsHarmless(t *testing.T) {
	dir := t.TempDir()
	rec := recordExits(t, dir, false)
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	ts.send(t, http.MethodPost, "/api/server/stop", `{}`)
	// The page may already be gone; either answer is fine.
	_, _, _ = ts.request(http.MethodDelete, "/api/server", "")
	if err := ts.exited(t); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 0 {
		t.Errorf("the second request ran %v; the first one wins", got)
	}
}

func TestStatusNamesTheFolders(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, true)
	udp, port := freePorts(t)
	st := startServer(t, dir, udp, port).status(t)
	if st.ServerDir != dir || st.DataDir != dir || st.Program != "/opt/ws/workspace-server" || !st.MainServer || st.LastError != "" {
		t.Errorf("status %+v", st)
	}
}

func TestAlreadyRunningError(t *testing.T) {
	dir := t.TempDir()
	udp, port := freePorts(t)
	startServer(t, dir, udp, port)
	err := runServer(t.Context(), serverOptions{Dir: dir, UDPPort: freeUDPPort(t, udp, port), HTTPPort: freeTCPPort(t, udp, port)}, nil)
	if !errors.Is(err, errAlreadyRunning) {
		t.Errorf("err %v does not match errAlreadyRunning", err)
	}
	if err == nil || err.Error() != "another workspace-server is already using "+dir {
		t.Errorf("text %q", err)
	}
}

// A port changed on the page outlives a reset even where a flag set it.
func TestRestartKeepsThePortsChangedOnThePage(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port) // both ports given as flags
	nextUDP := freeUDPPort(t, udp, port)
	nextHTTP := freeTCPPort(t, udp, port, nextUDP)
	if code, body := ts.send(t, http.MethodPut, "/api/config", fmt.Sprintf(`{"udpPort":%d}`, nextUDP)); code != http.StatusNoContent {
		t.Fatalf("PUT udpPort = %d %s", code, body)
	}
	if code, body := ts.send(t, http.MethodPut, "/api/config", fmt.Sprintf(`{"httpPort":%d}`, nextHTTP)); code != http.StatusNoContent {
		t.Fatalf("PUT httpPort = %d %s", code, body)
	}
	ts.port = nextHTTP
	before := ts.status(t)
	if code, body := ts.send(t, http.MethodDelete, "/api/data", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE /api/data = %d %s", code, body)
	}
	after := ts.waitStatus(t, func(st web.Status) bool { return st.ServerID != before.ServerID })
	if after.HTTPPort != nextHTTP || after.UDPPort != nextUDP {
		t.Errorf("after the reset: ports UDP %d, HTTP %d; want %d, %d", after.UDPPort, after.HTTPPort, nextUDP, nextHTTP)
	}
}

// holdPortOnRestart takes the web page's port in the gap of the next
// in-process restart, for d.
func holdPortOnRestart(t *testing.T, port int, d time.Duration) {
	t.Helper()
	saved := beforeRestart
	beforeRestart = func() {
		ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Errorf("hold port %d: %v", port, err)
			return
		}
		go func() { time.Sleep(d); ln.Close() }()
	}
	t.Cleanup(func() { beforeRestart = saved })
}

// A restart whose port is taken in the gap is retried, and comes back.
func TestRestartRetriesWhenItCannotStart(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	savedEvery := restartRetryEvery
	restartRetryEvery = 50 * time.Millisecond
	t.Cleanup(func() { restartRetryEvery = savedEvery })
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	holdPortOnRestart(t, port, 300*time.Millisecond)
	before := ts.status(t)
	if code, body := ts.send(t, http.MethodDelete, "/api/data", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE /api/data = %d %s", code, body)
	}
	after := ts.waitStatus(t, func(st web.Status) bool { return st.ServerID != before.ServerID })
	if !strings.Contains(after.LastError, "could not start again") {
		t.Errorf("lastError %q, want why the first attempt failed", after.LastError)
	}
	if !strings.Contains(ts.logs.String(), "retrying every") {
		t.Errorf("the log does not say it retries\n%s", ts.logs)
	}
}

// A restart that never starts ends the run with the error.
func TestRestartGivesUp(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	savedEvery, savedFor := restartRetryEvery, restartRetryFor
	restartRetryEvery, restartRetryFor = 20*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { restartRetryEvery, restartRetryFor = savedEvery, savedFor })
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	holdPortOnRestart(t, port, 2*time.Second)
	if code, body := ts.send(t, http.MethodDelete, "/api/data", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE /api/data = %d %s", code, body)
	}
	if err := ts.exited(t); err == nil {
		t.Error("runServer returned no error for a restart that never started")
	}
}

// dataDirBody is the page's PUT /api/config for a data-folder change it
// checked with verdict.
func dataDirBody(path, verdict string) string {
	return fmt.Sprintf(`{"dataDir":%q,"verdict":%q}`, path, verdict)
}

// The page saw a switch, but the server folder gained data before the PUT:
// the change is refused, since it would leave that data behind unseen.
func TestDataDirChangeRefusedWhenTheVerdictChanged(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	udp, port := freePorts(t)
	ts := startServer(t, dir, udp, port)
	target := t.TempDir()
	if _, body := ts.send(t, http.MethodGet, "/api/data-dir?path="+target, ""); !strings.Contains(body, `"verdict":"switch"`) {
		t.Fatalf("check = %s, want switch", body)
	}
	putData(t, dir, "todo", `{"a":1}`, 0)
	code, body := ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "switch"))
	if code != http.StatusConflict || !strings.Contains(body, `"changed"`) {
		t.Fatalf("PUT dataDir = %d %s, want 409 changed", code, body)
	}
	if readConfig(t, dir).DataDir != "" {
		t.Error("config.json changed")
	}
}

// A second change while one waits for its restart is refused.
func TestSecondDataDirChangeIsRefused(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	a, b := t.TempDir(), t.TempDir()
	if restart, err := s.SetDataDir(a, "switch"); err != nil || !restart {
		t.Fatalf("SetDataDir(a) = %v, %v", restart, err)
	}
	if _, err := s.SetDataDir(b, "switch"); !errors.Is(err, web.ErrBusy) {
		t.Errorf("SetDataDir(b) = %v, want ErrBusy", err)
	}
	if s.pendingMove.dataDir != a {
		t.Errorf("the recorded change is %s, want %s", s.pendingMove.dataDir, a)
	}
	_ = dir
}

// The move checks the folders a third time once the server has stopped; a
// change since then is refused and nothing moves.
func TestMoveDataChecksAgainAfterTheStop(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	t.Run("data appeared for a switch", func(t *testing.T) {
		dir := t.TempDir()
		putData(t, dir, "todo", `{"a":1}`, 0)
		target := t.TempDir()
		_, err := moveDataDir(dir, dir, action{kind: actMoveData, dataDir: target, verdict: verdictSwitch}, logger)
		if err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("err = %v, want the folders changed", err)
		}
		if _, err := os.Stat(filepath.Join(dir, configFile)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("config.json was written (%v)", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "apps", "todo", "data.json")); err != nil {
			t.Errorf("the data moved: %v", err)
		}
	})
	t.Run("both hold data for an adopt", func(t *testing.T) {
		dir := t.TempDir()
		putData(t, dir, "todo", `{"a":1}`, 0)
		target := t.TempDir()
		putData(t, target, "notes", `{"n":1}`, 0)
		if _, err := moveDataDir(dir, dir, action{kind: actMoveData, dataDir: target, verdict: verdictAdopt}, logger); err == nil {
			t.Fatal("an adopt that became a conflict went ahead")
		}
	})
}

// After a move from a data folder of its own, that folder goes too once it
// is empty; the server folder always stays.
func TestMoveDataRemovesTheEmptiedOldFolder(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	old := filepath.Join(t.TempDir(), "old")
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: old})
	putData(t, old, "todo", `{"a":1}`, 0)
	ts := startServer(t, dir, 0, port)
	target := t.TempDir()
	if code, body := ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "move")); code != http.StatusAccepted {
		t.Fatalf("PUT dataDir = %d %s", code, body)
	}
	ts.waitStatus(t, func(st web.Status) bool { return st.DataDir == target })
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied old folder is still there: %v", err)
	}
}

// Moving data away deletes only the server's app folders from the old
// folder, never a foreign file under its apps.
func TestMoveDataLeavesForeignFilesInTheOldFolder(t *testing.T) {
	dir := t.TempDir()
	recordExits(t, dir, false)
	old := t.TempDir()
	udp, port := freePorts(t)
	writeConfig(t, dir, config.Config{Name: "Desk", UDPPort: udp, HTTPPort: port, DataDir: old})
	putData(t, old, "todo", `{"a":1}`, 0)
	writeFileAll(t, filepath.Join(old, "apps", "MyTool", "notes.txt"), "the user's")
	ts := startServer(t, dir, 0, port)
	target := t.TempDir()
	ts.send(t, http.MethodPut, "/api/config", dataDirBody(target, "move"))
	ts.waitStatus(t, func(st web.Status) bool { return st.DataDir == target })
	if _, err := os.Stat(filepath.Join(old, "apps", "MyTool", "notes.txt")); err != nil {
		t.Errorf("the move deleted the user's file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old, "apps", "todo")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the old copy of todo is still there")
	}
}
