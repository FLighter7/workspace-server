package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/config"
)

// fakeServer serves /api/status with serverId on 127.0.0.1 and records the
// stop requests; it writes its port into config.json and the id into
// identity.json of a new server folder.
type fakeServer struct {
	*httptest.Server
	dir   string
	port  int
	stops chan *http.Request
	code  int // the stop answer
}

func newFakeServer(t *testing.T, fileID, serverID string) *fakeServer {
	t.Helper()
	f := &fakeServer{dir: t.TempDir(), stops: make(chan *http.Request, 4), code: http.StatusAccepted}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"serverId": serverID})
	})
	mux.HandleFunc("POST /api/server/stop", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Header.Set("X-Test-Body", string(body))
		f.stops <- r
		w.WriteHeader(f.code)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	f.port = f.Listener.Addr().(*net.TCPAddr).Port
	writeConfig(t, f.dir, config.Config{Name: "x", UDPPort: freeUDPPort(t, f.port), HTTPPort: f.port})
	if fileID != "" {
		writeFile(t, filepath.Join(f.dir, "identity.json"), `{"serverId":"`+fileID+`"}`)
	}
	return f
}

func TestProbeServerFindsTheServerOfTheFolder(t *testing.T) {
	f := newFakeServer(t, "AbCd", "AbCd")
	port, ok := probeServer(f.dir)
	if !ok || port != f.port {
		t.Errorf("probeServer = %d %v, want %d true", port, ok, f.port)
	}
}

func TestProbeServerIgnoresAnotherServer(t *testing.T) {
	f := newFakeServer(t, "AbCd", "XyZw")
	if _, ok := probeServer(f.dir); ok {
		t.Error("a server with another serverId was taken for this folder's")
	}
}

func TestProbeServerWithoutIdentityCreatesNothing(t *testing.T) {
	f := newFakeServer(t, "", "AbCd")
	before, _ := os.ReadDir(f.dir)
	if _, ok := probeServer(f.dir); ok {
		t.Error("probeServer found a server without identity.json")
	}
	after, _ := os.ReadDir(f.dir)
	if len(after) != len(before) {
		t.Errorf("probeServer created files: %d entries before, %d after", len(before), len(after))
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, ok := probeServer(missing); ok {
		t.Error("probeServer found a server for a missing folder")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("probeServer created the missing folder")
	}
}

func TestProbeServerNothingListening(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, config.Config{Name: "x", UDPPort: 1, HTTPPort: freeTCPPort(t, 1)})
	writeFile(t, filepath.Join(dir, "identity.json"), `{"serverId":"AbCd"}`)
	start := time.Now()
	if _, ok := probeServer(dir); ok {
		t.Error("probeServer found a server where nothing listens")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("probeServer took %s", d)
	}
}

func TestRequestStop(t *testing.T) {
	f := newFakeServer(t, "AbCd", "AbCd")
	if err := requestStop(f.port); err != nil {
		t.Fatalf("requestStop = %v", err)
	}
	r := <-f.stops
	if r.Header.Get("X-Workspace-Server") != "1" || r.Header.Get("X-Test-Body") != "{}" {
		t.Errorf("request header %q, body %q", r.Header.Get("X-Workspace-Server"), r.Header.Get("X-Test-Body"))
	}
	f.code = http.StatusForbidden
	if err := requestStop(f.port); err == nil {
		t.Error("requestStop accepted a 403")
	}
}

func TestWaitStoppedSeesTheServerGo(t *testing.T) {
	f := newFakeServer(t, "AbCd", "AbCd")
	go func() {
		time.Sleep(200 * time.Millisecond)
		f.Close()
	}()
	if !waitStopped(f.dir, 3*time.Second) {
		t.Error("waitStopped = false after the server closed")
	}
}

func TestWaitStoppedWaitsForTheLock(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, config.Config{Name: "x", UDPPort: 1, HTTPPort: freeTCPPort(t, 1)})
	unlock, err := lockServerDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if !lockHeldElsewhere(t, dir) {
		t.Skip("this system's locks do not exclude the same process")
	}
	if waitStopped(dir, 300*time.Millisecond) {
		t.Error("waitStopped = true while the folder is locked")
	}
}

func TestWaitStoppedMissingFolderCreatesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if !waitStopped(missing, time.Second) {
		t.Error("waitStopped = false for a folder without a server")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("waitStopped created the folder")
	}
}
