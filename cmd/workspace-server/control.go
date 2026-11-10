package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The commands' client of a running server: they find it through its web
// page and ask it to stop there, since a server in the background has no
// console to close.

const (
	probeTimeout = time.Second
	stopTimeout  = 5 * time.Second
)

var controlClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// probeServer reports whether the server of the server folder dir answers,
// and on which port. It reads the folder's identity.json and the HTTP port in
// its config.json, never creating either, and asks GET /api/status: only an
// answer with the same serverId counts, so another server on that port is
// not taken for this folder's. A server started with -http-port that is not
// in config.json is not found.
func probeServer(dir string) (port int, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return 0, false
	}
	var id struct {
		ServerID string `json:"serverId"`
	}
	if json.Unmarshal(data, &id) != nil || id.ServerID == "" {
		return 0, false
	}
	port = installHTTPPort(dir, io.Discard)
	req, err := http.NewRequest(http.MethodGet, pageBase(port)+"/api/status", nil)
	if err != nil {
		return 0, false
	}
	client := *controlClient
	client.Timeout = probeTimeout
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var st struct {
		ServerID string `json:"serverId"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&st) != nil {
		return 0, false
	}
	return port, st.ServerID == id.ServerID
}

// requestStop asks the server on port to stop, without removing anything:
// the command that asks does its own removals once the server is gone, so
// that it never races the dying server's.
func requestStop(port int) error {
	req, err := http.NewRequest(http.MethodPost, pageBase(port)+"/api/server/stop", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("X-Workspace-Server", "1")
	req.Header.Set("Content-Type", "application/json")
	client := *controlClient
	client.Timeout = stopTimeout
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ask the server to stop: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("ask the server to stop: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// waitStopped waits up to timeout for the server of dir to be gone: its page
// does not answer and its folder's lock is free. A folder without a lock file
// has no server, and is not created.
func waitStopped(dir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, up := probeServer(dir); !up && lockFree(dir) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// lockFree reports whether no process holds the lock of the server folder
// dir. It takes and releases the lock, and creates nothing.
func lockFree(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, lockFile)); errors.Is(err, os.ErrNotExist) {
		return true
	}
	unlock, err := lockServerDir(dir)
	if err != nil {
		return !errors.Is(err, errLocked)
	}
	unlock()
	return true
}

// pageBase is the web page's origin on port.
func pageBase(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }
