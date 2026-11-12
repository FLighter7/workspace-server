package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/protocol"
)

// linkWait bounds every wait for the link between two servers.
const linkWait = 20 * time.Second

// Two whole servers in this process: the subordinate links to the main
// through its page's API and pulls the main's document; the roles show in
// the status; a second link is refused; a reset forgets the link.
func TestTwoServersLinkAndPull(t *testing.T) {
	mainDir, subDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(mainDir, "apps.json"), `{"apps":[{"id":"todo","name":"Todo"},{"id":"notes","name":"Notes"}]}`)
	if err := os.MkdirAll(filepath.Join(mainDir, "apps", "todo"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mainDir, "apps", "todo", "data.json"), `{"v":1}`)

	mu, mp := freePorts(t)
	su := freeUDPPort(t, mu, mp)
	sp := freeTCPPort(t, mu, mp, su)
	main := startServer(t, mainDir, mu, mp)
	sub := startServer(t, subDir, su, sp)
	if st := sub.status(t); st.Role != "main" {
		t.Fatalf("a server with no upstream has role %q, want main", st.Role)
	}

	// The main's code with 127.0.0.1 as its only host, so the test does
	// not depend on this machine's LAN.
	c := main.pairingCode(t)
	c.H = []string{"127.0.0.1"}
	body, _ := json.Marshal(map[string]string{"code": pairing.EncodeCode(c)})
	code, resp := sub.send(t, http.MethodPost, "/api/upstream", string(body))
	if code != http.StatusOK {
		t.Fatalf("POST /api/upstream = %d %s\n%s", code, resp, sub.logs)
	}
	var st struct {
		Linked   bool
		ServerID string
	}
	if err := json.Unmarshal([]byte(resp), &st); err != nil || !st.Linked || st.ServerID != c.ID {
		t.Errorf("POST /api/upstream answered %s, want the link to %s", resp, c.ID)
	}
	if role := sub.status(t).Role; role != "subordinate" {
		t.Errorf("the linked server's role is %q, want subordinate", role)
	}

	waitUntil(t, "the pull", func() bool {
		code, doc := sub.send(t, http.MethodGet, "/api/apps/todo/document", "")
		return code == http.StatusOK && doc == `{"v":1}`
	})
	waitUntil(t, "notes on the subordinate", func() bool {
		_, list := sub.send(t, http.MethodGet, "/api/apps", "")
		var apps []struct{ ID string }
		_ = json.Unmarshal([]byte(list), &apps)
		for _, a := range apps {
			if a.ID == "notes" {
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(subDir, "upstream.json")); err != nil {
		t.Errorf("upstream.json: %v", err)
	}

	again, _ := json.Marshal(map[string]string{"code": pairing.EncodeCode(c)})
	if code, resp := sub.send(t, http.MethodPost, "/api/upstream", string(again)); code != http.StatusConflict {
		t.Errorf("a second link = %d %s, want 409", code, resp)
	}

	// The main knows the subordinate as a device with no app.
	_, devs := main.send(t, http.MethodGet, "/api/devices", "")
	var list []struct{ App, Name string }
	if err := json.Unmarshal([]byte(devs), &list); err != nil || len(list) != 1 || list[0].App != "" {
		t.Errorf("the main's devices %s, want one server-kind device", devs)
	}

	if code, resp := sub.send(t, http.MethodDelete, "/api/data", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE /api/data = %d %s", code, resp)
	}
	waitUntil(t, "the reset", func() bool {
		_, err := os.Stat(filepath.Join(subDir, "upstream.json"))
		return errors.Is(err, os.ErrNotExist)
	})
	waitUntil(t, "the server to come back as a main", func() bool {
		code, resp, err := sub.request(http.MethodGet, "/api/status", "")
		return err == nil && code == http.StatusOK && json.Valid(resp) && sub.status(t).Role == "main"
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(linkWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A link session has no app: reloading apps.json leaves it open whatever
// the file drops.
func TestReloadAppsKeepsLinkSessions(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	secret, _ := s.secrets.Issue()
	ch := &fakeChannel{}
	link := protocol.NewSession(ch, "192.0.2.1:50000", s.hub, protocol.Options{})
	t.Cleanup(link.Close)
	link.HandleText(`{"t":"pair","proto":1,"kind":"server","secret":"` + secret + `","name":"Laptop"}`)
	if !link.Authenticated() || link.App() != "" {
		t.Fatal("the server-kind pairing did not open a link session")
	}
	writeFile(t, filepath.Join(dir, "apps.json"), `{"apps":[]}`)
	s.reloadApps()
	if ch.isClosed() {
		t.Error("reloading apps.json closed the link session")
	}
}
