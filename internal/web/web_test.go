package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/pairing"
)

const testPort = 47801

var testNow = time.Date(2026, 11, 9, 15, 4, 5, 0, time.Local)

type configCall struct {
	name      *string
	udp, http *int
}

// fakeBackend records what the handler asks of it. Its errors mirror the real
// registries': AddApp and DeleteApp wrap the apps package's errors.
type fakeBackend struct {
	status    Status
	apps      []AppInfo
	devices   []DeviceInfo
	docs      map[string][]byte
	refusals  []pairing.Refusal
	configErr error
	deleteErr error

	configCalls []configCall
	added       []apps.App
	deleted     []string
	removed     []string
	issued      int

	// The server controls. calls lists them in order ("stop", "stop+autostart",
	// "reset", "delete", "restart"); onCall runs at each, so that a test can
	// look at the response written so far.
	calls      []string
	onCall     func(string)
	check      DataDirCheck
	checkErr   error
	checked    []string
	addErr     error // AddApp fails with it
	dataDirs   []string
	verdicts   []string
	dataDirErr error
	sameDir    bool
}

func (f *fakeBackend) record(call string) {
	f.calls = append(f.calls, call)
	if f.onCall != nil {
		f.onCall(call)
	}
}

func (f *fakeBackend) Stop(removeAutostart bool) {
	if removeAutostart {
		f.record("stop+autostart")
	} else {
		f.record("stop")
	}
}

func (f *fakeBackend) Reset()        { f.record("reset") }
func (f *fakeBackend) DeleteServer() { f.record("delete") }
func (f *fakeBackend) Restart()      { f.record("restart") }

func (f *fakeBackend) CheckDataDir(path string) (DataDirCheck, error) {
	f.checked = append(f.checked, path)
	return f.check, f.checkErr
}

func (f *fakeBackend) SetDataDir(path, verdict string) (bool, error) {
	f.dataDirs = append(f.dataDirs, path)
	f.verdicts = append(f.verdicts, verdict)
	if f.dataDirErr != nil {
		return false, f.dataDirErr
	}
	return !f.sameDir, nil
}

func newFake() *fakeBackend {
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	return &fakeBackend{
		status: Status{
			Name: "Studio", ServerID: "AbCdEfGhIjKl",
			MDNSName: "0b7c2f1e-8d4a-4c7b-9a51-3f2e6d8c9b10.local", MDNSRunning: true,
			Addresses: []string{"192.168.1.20"}, UDPPort: 47800, HTTPPort: testPort,
			Program: "/Applications/ws/workspace-server", ServerDir: "/Users/a/Library/Application Support/workspace-server",
			DataDir: "/Users/a/Data", MainServer: true,
		},
		apps: []AppInfo{{
			App:  apps.App{ID: "todo", Name: "Todo", History: 10, MaxBytes: apps.DefaultMaxBytes},
			Size: 12, Modified: &at, Versions: 3, Devices: 1,
		}},
		devices: []DeviceInfo{{
			ID: "Q2xhdWRlUm9ja3Mx", App: "todo", Name: "iPhone",
			PairedAt: at, LastSeen: at, Connected: true,
		}},
		docs: map[string][]byte{"todo": []byte(`{"items":[]}`)},
	}
}

func (f *fakeBackend) Status() Status { return f.status }

func (f *fakeBackend) SetConfig(name *string, udp, http *int) error {
	f.configCalls = append(f.configCalls, configCall{name, udp, http})
	return f.configErr
}

func (f *fakeBackend) Apps() []AppInfo { return f.apps }

func (f *fakeBackend) AddApp(a apps.App) error {
	if f.addErr != nil {
		return f.addErr
	}
	if !apps.ValidID(a.ID) {
		return fmt.Errorf("%w: id %q", apps.ErrInvalid, a.ID)
	}
	if a.History < 0 || a.History > 100 {
		return fmt.Errorf("%w: history %d is out of range 1-100", apps.ErrInvalid, a.History)
	}
	for _, x := range f.apps {
		if x.ID == a.ID {
			return fmt.Errorf("%w: %q", apps.ErrExists, a.ID)
		}
	}
	f.added = append(f.added, a)
	f.apps = append(f.apps, AppInfo{App: a})
	return nil
}

func (f *fakeBackend) DeleteApp(id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for i, x := range f.apps {
		if x.ID == id {
			f.apps = slices.Delete(f.apps, i, i+1)
			f.deleted = append(f.deleted, id)
			return nil
		}
	}
	return fmt.Errorf("%w: %q", apps.ErrNotFound, id)
}

func (f *fakeBackend) Document(id string) ([]byte, bool) {
	d, ok := f.docs[id]
	return d, ok
}

func (f *fakeBackend) Devices() []DeviceInfo { return f.devices }

func (f *fakeBackend) RemoveDevice(id string) bool {
	for i, d := range f.devices {
		if d.ID == id {
			f.devices = slices.Delete(f.devices, i, i+1)
			f.removed = append(f.removed, id)
			return true
		}
	}
	return false
}

func (f *fakeBackend) IssuePairing() (string, time.Time, error) {
	f.issued++
	code := pairing.EncodeCode(pairing.Code{
		V: 1, ID: f.status.ServerID, N: f.status.Name, P: "abcdefghijklmnopqrstuvwx",
		F: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", O: f.status.UDPPort,
		H: []string{f.status.MDNSName, "192.168.1.20"}, S: "c2VjcmV0c2VjcmV0c2VjcmV0",
	})
	return code, testNow.Add(10 * time.Minute), nil
}

func (f *fakeBackend) Refusals() []pairing.Refusal { return f.refusals }

func newTestHandler(f *fakeBackend) http.Handler {
	return newHandler(f, func() int { return testPort }, func() time.Time { return testNow })
}

// request builds a request the way the page sends it: to 127.0.0.1 on the
// page's port, with the marker header on every method but GET and HEAD.
func request(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("X-Workspace-Server", "1")
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// wantError checks that w is a JSON error with the given status and code.
func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %q", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var e struct{ Error, Message string }
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q is not JSON: %v", w.Body.String(), err)
	}
	if e.Error != code {
		t.Errorf("error = %q, want %q (message %q)", e.Error, code, e.Message)
	}
	if e.Message == "" {
		t.Errorf("error %q has no message", e.Error)
	}
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %q", w.Code, status, w.Body.String())
	}
}

// keysOf returns the sorted keys of a JSON object.
func keysOf(t *testing.T, obj map[string]any) []string {
	t.Helper()
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- The guard -------------------------------------------------------------

func TestGuardHost(t *testing.T) {
	h := newTestHandler(newFake())
	cases := []struct {
		host string
		want int
	}{
		{"evil.example", http.StatusForbidden},
		{"evil.example:47801", http.StatusForbidden},
		{"127.0.0.1:47801", http.StatusOK},
		{"localhost:47801", http.StatusOK},
		{"127.0.0.1", http.StatusForbidden},
		{"localhost", http.StatusForbidden},
		{"127.0.0.1:47802", http.StatusForbidden},
		{"[::1]:47801", http.StatusForbidden},
		{"192.168.1.20:47801", http.StatusForbidden},
		{"", http.StatusForbidden},
	}
	// The page and its assets are guarded too: DNS rebinding reads them.
	for _, path := range []string{"/", "/assets/app.js", "/api/status"} {
		for _, tc := range cases {
			r := request(http.MethodGet, path, "")
			r.Host = tc.host
			w := serve(h, r)
			if tc.want == http.StatusForbidden {
				wantError(t, w, http.StatusForbidden, "forbidden")
			} else if w.Code != tc.want {
				t.Errorf("GET %s with Host %q: status %d, want %d", path, tc.host, w.Code, tc.want)
			}
		}
	}
}

func TestGuardReadsThePortOnEveryRequest(t *testing.T) {
	port := testPort
	h := newHandler(newFake(), func() int { return port }, time.Now)
	get := func(host string) int {
		r := request(http.MethodGet, "/api/status", "")
		r.Host = host
		return serve(h, r).Code
	}
	if c := get("127.0.0.1:47801"); c != http.StatusOK {
		t.Fatalf("before the change: status %d", c)
	}
	port = 50123
	if c := get("127.0.0.1:47801"); c != http.StatusForbidden {
		t.Errorf("old port after the change: status %d, want 403", c)
	}
	if c := get("localhost:50123"); c != http.StatusOK {
		t.Errorf("new port after the change: status %d, want 200", c)
	}
}

func TestGuardMutatingNeedsHeader(t *testing.T) {
	f := newFake()
	h := newTestHandler(f)

	r := request(http.MethodPost, "/api/pairing", "")
	r.Header.Del("X-Workspace-Server")
	wantError(t, serve(h, r), http.StatusForbidden, "forbidden")

	r = request(http.MethodPost, "/api/pairing", "")
	r.Header.Set("X-Workspace-Server", "0")
	wantError(t, serve(h, r), http.StatusForbidden, "forbidden")
	if f.issued != 0 {
		t.Fatalf("a refused request issued %d secrets", f.issued)
	}

	wantStatus(t, serve(h, request(http.MethodPost, "/api/pairing", "")), http.StatusOK)
	if f.issued != 1 {
		t.Errorf("issued = %d, want 1", f.issued)
	}

	// Every mutating route, and a CORS preflight, is refused without it and
	// never reaches the backend.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/config", `{"udpPort":1}`},
		{http.MethodPost, "/api/apps", `{"id":"notes"}`},
		{http.MethodDelete, "/api/apps/todo", `{"confirm":"todo"}`},
		{http.MethodDelete, "/api/devices/Q2xhdWRlUm9ja3Mx", ""},
		{http.MethodOptions, "/api/pairing", ""},
		{http.MethodPost, "/api/server/stop", `{"removeAutostart":true}`},
		{http.MethodDelete, "/api/data", ""},
		{http.MethodDelete, "/api/server", ""},
		{http.MethodPut, "/api/config", `{"dataDir":"/x"}`},
	} {
		r := request(tc.method, tc.path, tc.body)
		r.Header.Del("X-Workspace-Server")
		w := serve(h, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s without the header: status %d, want 403", tc.method, tc.path, w.Code)
		}
	}
	if len(f.configCalls)+len(f.added)+len(f.deleted)+len(f.removed)+len(f.calls)+len(f.dataDirs) != 0 || f.issued != 1 {
		t.Errorf("refused requests reached the backend: %+v", f)
	}
}

func TestGuardOrigin(t *testing.T) {
	f := newFake()
	h := newTestHandler(f)
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{"https://evil.example", http.StatusForbidden},
		{"http://127.0.0.1:47801", http.StatusOK},
		{"http://localhost:47801", http.StatusOK},
		{"null", http.StatusForbidden},
		{"", http.StatusForbidden},
		{"http://127.0.0.1:47802", http.StatusForbidden},
		{"https://127.0.0.1:47801", http.StatusForbidden},
		{"http://127.0.0.1:47801.evil.example", http.StatusForbidden},
	} {
		r := request(http.MethodGet, "/api/status", "")
		r.Header["Origin"] = []string{tc.origin}
		w := serve(h, r)
		if tc.want == http.StatusForbidden {
			wantError(t, w, http.StatusForbidden, "forbidden")
		} else if w.Code != tc.want {
			t.Errorf("Origin %q: status %d, want %d", tc.origin, w.Code, tc.want)
		}
	}

	// A foreign Origin is refused even with the marker header.
	r := request(http.MethodPost, "/api/pairing", "")
	r.Header.Set("Origin", "https://evil.example")
	wantError(t, serve(h, r), http.StatusForbidden, "forbidden")
	if f.issued != 0 {
		t.Errorf("a foreign Origin issued %d secrets", f.issued)
	}
	r = request(http.MethodPost, "/api/pairing", "")
	r.Header.Set("Origin", "http://localhost:47801")
	wantStatus(t, serve(h, r), http.StatusOK)

	// The server controls refuse a foreign Origin and a foreign Host.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/server/stop", `{}`},
		{http.MethodDelete, "/api/data", ""},
		{http.MethodDelete, "/api/server", ""},
		{http.MethodPut, "/api/config", `{"dataDir":"/x"}`},
	} {
		r := request(tc.method, tc.path, tc.body)
		r.Header.Set("Origin", "https://evil.example")
		wantError(t, serve(h, r), http.StatusForbidden, "forbidden")
		r = request(tc.method, tc.path, tc.body)
		r.Host = "evil.example:47801"
		wantError(t, serve(h, r), http.StatusForbidden, "forbidden")
	}
	if len(f.calls)+len(f.dataDirs) != 0 {
		t.Errorf("refused requests reached the backend: %v %v", f.calls, f.dataDirs)
	}
}

// --- The server controls ---------------------------------------------------

// controlCall sends a request and reports the backend's calls, and whether
// the answer had been written and flushed when each call was made.
func controlCall(t *testing.T, f *fakeBackend, method, path, body string) (*httptest.ResponseRecorder, []bool) {
	t.Helper()
	h := newTestHandler(f)
	w := httptest.NewRecorder()
	var answered []bool
	f.onCall = func(string) { answered = append(answered, w.Code == http.StatusAccepted && w.Flushed) }
	h.ServeHTTP(w, request(method, path, body))
	return w, answered
}

func wantJSONBody(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("body %s, want %s", got, want)
	}
}

func TestStopServer(t *testing.T) {
	for _, tc := range []struct{ body, call string }{
		{`{}`, "stop"},
		{`{"removeAutostart":false}`, "stop"},
		{`{"removeAutostart":true}`, "stop+autostart"},
	} {
		f := newFake()
		w, answered := controlCall(t, f, http.MethodPost, "/api/server/stop", tc.body)
		wantStatus(t, w, http.StatusAccepted)
		wantJSONBody(t, w, `{"stopping":true}`)
		if !slices.Equal(f.calls, []string{tc.call}) || !slices.Equal(answered, []bool{true}) {
			t.Errorf("%s: calls %v (answered first %v), want [%s] after the answer", tc.body, f.calls, answered, tc.call)
		}
	}
	for _, body := range []string{``, `{"deleteServer":true}`, `{"removeAutostart":"yes"}`} {
		f := newFake()
		w, _ := controlCall(t, f, http.MethodPost, "/api/server/stop", body)
		wantError(t, w, http.StatusBadRequest, "invalid")
		if len(f.calls) != 0 {
			t.Errorf("%q reached the backend: %v", body, f.calls)
		}
	}
}

func TestResetAndDeleteServer(t *testing.T) {
	for _, tc := range []struct{ path, call, body string }{
		{"/api/data", "reset", `{"restarting":true}`},
		{"/api/server", "delete", `{"stopping":true}`},
	} {
		f := newFake()
		w, answered := controlCall(t, f, http.MethodDelete, tc.path, "")
		wantStatus(t, w, http.StatusAccepted)
		wantJSONBody(t, w, tc.body)
		if !slices.Equal(f.calls, []string{tc.call}) || !slices.Equal(answered, []bool{true}) {
			t.Errorf("DELETE %s: calls %v (answered first %v), want [%s] after the answer", tc.path, f.calls, answered, tc.call)
		}
	}
	// The other methods on these paths are not routes.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/data"}, {http.MethodGet, "/api/server"}, {http.MethodGet, "/api/server/stop"},
	} {
		f := newFake()
		if w, _ := controlCall(t, f, tc.method, tc.path, "{}"); w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 405 or 404", tc.method, tc.path, w.Code)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s %s reached the backend", tc.method, tc.path)
		}
	}
}

func TestCheckDataDir(t *testing.T) {
	f := newFake()
	f.check = DataDirCheck{
		Path: "/Users/a/New", Verdict: "move", Message: "",
		Current: []AppData{{ID: "todo", Size: 12, Versions: 3}}, Target: []AppData{},
	}
	h := newTestHandler(f)
	w := serve(h, withMarker(request(http.MethodGet, "/api/data-dir?path=%2FUsers%2Fa%2FNew", "")))
	wantStatus(t, w, http.StatusOK)
	wantJSONBody(t, w, `{"path":"/Users/a/New","verdict":"move","message":"","current":[{"id":"todo","size":12,"versions":3}],"target":[]}`)
	if !slices.Equal(f.checked, []string{"/Users/a/New"}) {
		t.Errorf("checked %v", f.checked)
	}

	wantError(t, serve(h, withMarker(request(http.MethodGet, "/api/data-dir", ""))), http.StatusBadRequest, "invalid")

	// It touches the file system (a probe file) and reveals what exists, so
	// a GET from another site, which an <img> can send without Origin, must
	// not reach it: it needs the marker header like the mutating routes.
	r := request(http.MethodGet, "/api/data-dir?path=%2Fx", "")
	f.checked = nil
	wantError(t, serve(h, r), http.StatusForbidden, "forbidden")
	if len(f.checked) != 0 {
		t.Errorf("a GET without the header reached the backend")
	}
	f.checkErr = errors.New("cannot read the current data")
	wantError(t, serve(h, withMarker(request(http.MethodGet, "/api/data-dir?path=%2Fx", ""))), http.StatusInternalServerError, "internal")

	// Nil lists are [] in JSON.
	f.check, f.checkErr = DataDirCheck{Path: "/x", Verdict: "switch"}, nil
	w = serve(h, withMarker(request(http.MethodGet, "/api/data-dir?path=%2Fx", "")))
	wantJSONBody(t, w, `{"path":"/x","verdict":"switch","message":"","current":[],"target":[]}`)
}

func TestSetDataDir(t *testing.T) {
	f := newFake()
	w, answered := controlCall(t, f, http.MethodPut, "/api/config", `{"dataDir":"/Users/a/New","verdict":"move"}`)
	wantStatus(t, w, http.StatusAccepted)
	wantJSONBody(t, w, `{"restarting":true}`)
	if !slices.Equal(f.verdicts, []string{"move"}) {
		t.Errorf("verdicts %v, want the page's", f.verdicts)
	}
	if !slices.Equal(f.dataDirs, []string{"/Users/a/New"}) || !slices.Equal(f.calls, []string{"restart"}) || !slices.Equal(answered, []bool{true}) {
		t.Errorf("dataDirs %v, calls %v (answered first %v); want the change, then the restart after the answer", f.dataDirs, f.calls, answered)
	}
	if len(f.configCalls) != 0 {
		t.Errorf("SetConfig was called too")
	}

	// The current folder: nothing to do, no restart.
	f = newFake()
	f.sameDir = true
	w, _ = controlCall(t, f, http.MethodPut, "/api/config", `{"dataDir":"/Users/a/Data","verdict":"same"}`)
	wantStatus(t, w, http.StatusNoContent)
	if len(f.calls) != 0 {
		t.Errorf("calls %v, want none", f.calls)
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: both hold data", ErrDataConflict), http.StatusConflict, "dataExists"},
		{fmt.Errorf("%w: it now holds data", ErrDataChanged), http.StatusConflict, "changed"},
		{fmt.Errorf("%w: not absolute", ErrDataInvalid), http.StatusBadRequest, "invalid"},
		{fmt.Errorf("%w: a change is under way", ErrBusy), http.StatusConflict, "busy"},
		{errors.New("disk on fire"), http.StatusInternalServerError, "internal"},
	} {
		f := newFake()
		f.dataDirErr = tc.err
		w, _ := controlCall(t, f, http.MethodPut, "/api/config", `{"dataDir":"/x","verdict":"switch"}`)
		wantError(t, w, tc.status, tc.code)
		if len(f.calls) != 0 {
			t.Errorf("%v: calls %v, want no restart", tc.err, f.calls)
		}
	}

	// The data folder changes on its own, and with the verdict the page saw.
	for _, body := range []string{
		`{"dataDir":"/x","verdict":"switch","name":"Attic"}`,
		`{"dataDir":"/x"}`,
		`{"verdict":"switch"}`,
		`{"name":"Attic","verdict":"switch"}`,
	} {
		f = newFake()
		w, _ = controlCall(t, f, http.MethodPut, "/api/config", body)
		wantError(t, w, http.StatusBadRequest, "invalid")
		if len(f.dataDirs)+len(f.configCalls)+len(f.calls) != 0 {
			t.Errorf("%s reached the backend", body)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(newFake())
	foreign := request(http.MethodGet, "/", "")
	foreign.Host = "evil.example"
	for name, r := range map[string]*http.Request{
		"page":      request(http.MethodGet, "/", ""),
		"asset":     request(http.MethodGet, "/assets/style.css", ""),
		"api":       request(http.MethodGet, "/api/apps", ""),
		"not found": request(http.MethodGet, "/nope", ""),
		"forbidden": foreign,
	} {
		w := serve(h, r)
		hd := w.Header()
		if got := hd.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", name, got)
		}
		csp := hd.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "img-src 'self' data:"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: Content-Security-Policy %q lacks %q", name, csp, want)
			}
		}
		if got := hd.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", name, got)
		}
		if got := hd.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", name, got)
		}
		// no-referrer could make the page's own writes carry Origin: null,
		// which the guard refuses.
		if got := hd.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s: Referrer-Policy = %q, want same-origin", name, got)
		}
		for k := range hd {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: sent CORS header %s", name, k)
			}
		}
	}
}

// --- The API ---------------------------------------------------------------

func TestPairingResponse(t *testing.T) {
	h := newTestHandler(newFake())
	w := serve(h, request(http.MethodPost, "/api/pairing", ""))
	wantStatus(t, w, http.StatusOK)
	var resp struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
		QR        string    `json:"qr"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Code, "wsrv1.") {
		t.Errorf("code = %q, want the wsrv1. prefix", resp.Code)
	}
	if _, err := pairing.DecodeCode(resp.Code); err != nil {
		t.Errorf("code does not decode: %v", err)
	}
	if !resp.ExpiresAt.Equal(testNow.Add(10 * time.Minute)) {
		t.Errorf("expiresAt = %v, want %v", resp.ExpiresAt, testNow.Add(10*time.Minute))
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(resp.QR, prefix) {
		t.Fatalf("qr = %.40q..., want the %s prefix", resp.QR, prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(resp.QR, prefix))
	if err != nil {
		t.Fatalf("qr is not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("qr is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Errorf("qr is %dx%d, want 512x512", b.Dx(), b.Dy())
	}
}

func TestAddAppValidation(t *testing.T) {
	f := newFake()
	h := newTestHandler(f)
	post := func(body string) *httptest.ResponseRecorder {
		return serve(h, request(http.MethodPost, "/api/apps", body))
	}

	wantError(t, post(`{"id":"Bad"}`), http.StatusBadRequest, "invalid")
	wantError(t, post(`{"id":"todo"}`), http.StatusBadRequest, "exists")
	// A range the backend refuses maps to invalid too.
	wantError(t, post(`{"id":"notes","history":101}`), http.StatusBadRequest, "invalid")
	wantError(t, post(`{"id":`), http.StatusBadRequest, "invalid")
	wantError(t, post(`{"id":"notes","colour":"red"}`), http.StatusBadRequest, "invalid")
	wantError(t, post(`{"id":"notes"} {"id":"more"}`), http.StatusBadRequest, "invalid")
	wantError(t, post(""), http.StatusBadRequest, "invalid")
	if len(f.added) != 0 {
		t.Fatalf("refused adds reached the backend: %+v", f.added)
	}

	wantStatus(t, post(`{"id":"notes","name":"Notes","history":5,"maxBytes":1048576}`), http.StatusCreated)
	// "Add it?" posts only the id; the registry fills in the defaults.
	wantStatus(t, post(`{"id":"from-phone"}`), http.StatusCreated)
	want := []apps.App{{ID: "notes", Name: "Notes", History: 5, MaxBytes: 1048576}, {ID: "from-phone"}}
	if !reflect.DeepEqual(f.added, want) {
		t.Errorf("added = %+v, want %+v", f.added, want)
	}

	// Any other failure, such as a disk that refuses apps.json, is internal.
	f.addErr = errors.New("write apps.json: disk full")
	wantError(t, post(`{"id":"more"}`), http.StatusInternalServerError, "internal")
}

func TestDeleteAppNeedsConfirm(t *testing.T) {
	f := newFake()
	h := newTestHandler(f)
	del := func(path, body string) *httptest.ResponseRecorder {
		return serve(h, request(http.MethodDelete, path, body))
	}

	wantError(t, del("/api/apps/todo", `{"confirm":"tod"}`), http.StatusBadRequest, "confirmMismatch")
	wantError(t, del("/api/apps/todo", `{"confirm":"TODO"}`), http.StatusBadRequest, "confirmMismatch")
	wantError(t, del("/api/apps/todo", `{}`), http.StatusBadRequest, "confirmMismatch")
	wantError(t, del("/api/apps/todo", ""), http.StatusBadRequest, "invalid")
	if len(f.deleted) != 0 {
		t.Fatalf("DeleteApp called without a matching confirm: %v", f.deleted)
	}

	wantStatus(t, del("/api/apps/todo", `{"confirm":"todo"}`), http.StatusNoContent)
	if !reflect.DeepEqual(f.deleted, []string{"todo"}) {
		t.Errorf("deleted = %v, want [todo]", f.deleted)
	}

	// The backend's ErrNotFound is 404; an id that cannot exist never reaches it.
	wantError(t, del("/api/apps/todo", `{"confirm":"todo"}`), http.StatusNotFound, "notFound")
	wantError(t, del("/api/apps/Bad", `{"confirm":"Bad"}`), http.StatusNotFound, "notFound")
	wantError(t, del("/api/apps/a%2Fb", `{"confirm":"a/b"}`), http.StatusNotFound, "notFound")

	f.deleteErr = errors.New("remove apps/notes: permission denied")
	wantError(t, del("/api/apps/notes", `{"confirm":"notes"}`), http.StatusInternalServerError, "internal")
}

func TestDocumentDownload(t *testing.T) {
	f := newFake()
	f.apps = append(f.apps, AppInfo{App: apps.App{ID: "empty"}})
	h := newTestHandler(f)

	w := serve(h, request(http.MethodGet, "/api/apps/todo/document", ""))
	wantStatus(t, w, http.StatusOK)
	if got := w.Body.String(); got != `{"items":[]}` {
		t.Errorf("body = %q", got)
	}
	want := fmt.Sprintf(`attachment; filename="todo-%s.json"`, testNow.Format("2006-01-02"))
	if got := w.Header().Get("Content-Disposition"); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}

	wantError(t, serve(h, request(http.MethodGet, "/api/apps/empty/document", "")), http.StatusNotFound, "notFound")
	wantError(t, serve(h, request(http.MethodGet, "/api/apps/none/document", "")), http.StatusNotFound, "notFound")
	wantError(t, serve(h, request(http.MethodGet, "/api/apps/Bad/document", "")), http.StatusNotFound, "notFound")
}

// The download is named with the local date, which here is a day after the
// UTC one.
func TestDocumentDownloadUsesTheLocalDate(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC+10", 10*60*60)
	t.Cleanup(func() { time.Local = saved })
	evening := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC) // 06:00 on the 10th, local
	h := newHandler(newFake(), func() int { return testPort }, func() time.Time { return evening })
	w := serve(h, request(http.MethodGet, "/api/apps/todo/document", ""))
	wantStatus(t, w, http.StatusOK)
	if got, want := w.Header().Get("Content-Disposition"), `attachment; filename="todo-2026-11-10.json"`; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
}

func TestConfig(t *testing.T) {
	f := newFake()
	h := newTestHandler(f)
	put := func(body string) *httptest.ResponseRecorder {
		return serve(h, request(http.MethodPut, "/api/config", body))
	}

	wantStatus(t, put(`{"name":"Attic","udpPort":47900}`), http.StatusNoContent)
	wantStatus(t, put(`{"httpPort":8080}`), http.StatusNoContent)
	wantStatus(t, put(`{}`), http.StatusNoContent)
	if len(f.configCalls) != 3 {
		t.Fatalf("SetConfig called %d times, want 3", len(f.configCalls))
	}
	c := f.configCalls[0]
	if c.name == nil || *c.name != "Attic" || c.udp == nil || *c.udp != 47900 || c.http != nil {
		t.Errorf("first call = %s", describe(c))
	}
	c = f.configCalls[1]
	if c.name != nil || c.udp != nil || c.http == nil || *c.http != 8080 {
		t.Errorf("second call = %s", describe(c))
	}
	c = f.configCalls[2]
	if c.name != nil || c.udp != nil || c.http != nil {
		t.Errorf("third call = %s", describe(c))
	}

	f.configErr = errors.New("udpPort and httpPort must differ, both are 47801")
	w := put(`{"udpPort":47801}`)
	wantError(t, w, http.StatusBadRequest, "invalid")
	if !strings.Contains(w.Body.String(), "must differ") {
		t.Errorf("the backend's reason is missing: %s", w.Body.String())
	}

	f.configCalls = nil
	wantError(t, put(`{"udpPort":"x"}`), http.StatusBadRequest, "invalid")
	wantError(t, put(`{"port":1}`), http.StatusBadRequest, "invalid")
	big := `{"name":"` + strings.Repeat("a", 70<<10) + `"}`
	wantError(t, put(big), http.StatusBadRequest, "invalid")
	if len(f.configCalls) != 0 {
		t.Errorf("malformed bodies reached the backend: %d calls", len(f.configCalls))
	}
}

func describe(c configCall) string {
	s := func(p *string) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprintf("%q", *p)
	}
	i := func(p *int) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("{name %s udp %s http %s}", s(c.name), i(c.udp), i(c.http))
}

func TestRemoveDevice(t *testing.T) {
	f := newFake()
	h := newTestHandler(f)
	wantError(t, serve(h, request(http.MethodDelete, "/api/devices/nope", "")), http.StatusNotFound, "notFound")
	wantStatus(t, serve(h, request(http.MethodDelete, "/api/devices/Q2xhdWRlUm9ja3Mx", "")), http.StatusNoContent)
	if !reflect.DeepEqual(f.removed, []string{"Q2xhdWRlUm9ja3Mx"}) {
		t.Errorf("removed = %v", f.removed)
	}
}

func TestJSONShape(t *testing.T) {
	f := newFake()
	f.refusals = []pairing.Refusal{{App: "notes", Device: "Pixel", At: testNow}}
	h := newTestHandler(f)

	getObject := func(path string) map[string]any {
		w := serve(h, request(http.MethodGet, path, ""))
		wantStatus(t, w, http.StatusOK)
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return v
	}
	getList := func(path string) []map[string]any {
		w := serve(h, request(http.MethodGet, path, ""))
		wantStatus(t, w, http.StatusOK)
		var v []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if len(v) != 1 {
			t.Fatalf("GET %s: %d items, want 1", path, len(v))
		}
		return v
	}

	for path, want := range map[string][]string{
		"/api/status": {"addresses", "dataDir", "httpPort", "lastError", "listening", "mainServer", "mdnsName", "mdnsRunning", "name", "program", "serverDir", "serverId", "udpPort"},
	} {
		if got := keysOf(t, getObject(path)); !reflect.DeepEqual(got, want) {
			t.Errorf("GET %s keys = %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string][]string{
		"/api/apps":             {"devices", "history", "id", "maxBytes", "modified", "name", "size", "versions"},
		"/api/devices":          {"app", "connected", "id", "lastSeen", "name", "pairedAt"},
		"/api/pairing/refusals": {"app", "at", "device"},
	} {
		if got := keysOf(t, getList(path)[0]); !reflect.DeepEqual(got, want) {
			t.Errorf("GET %s keys = %v, want %v", path, got, want)
		}
	}
	if got := getList("/api/apps")[0]["modified"]; got != "2026-11-08T10:00:00Z" {
		t.Errorf("modified = %v, want RFC 3339", got)
	}

	// Empty lists are [] rather than null, and so are missing addresses.
	empty := &fakeBackend{}
	h = newTestHandler(empty)
	for _, path := range []string{"/api/apps", "/api/devices", "/api/pairing/refusals"} {
		w := serve(h, request(http.MethodGet, path, ""))
		if got := strings.TrimSpace(w.Body.String()); got != "[]" {
			t.Errorf("GET %s with nothing = %s, want []", path, got)
		}
	}
	w := serve(h, request(http.MethodGet, "/api/status", ""))
	if !strings.Contains(w.Body.String(), `"addresses":[]`) {
		t.Errorf("status without addresses = %s", w.Body.String())
	}
}

func TestRefusalsAreShortenedForDisplay(t *testing.T) {
	f := newFake()
	long := strings.Repeat("я", 100)
	f.refusals = []pairing.Refusal{
		{App: "notes", Device: "  Pixel 8  ", At: testNow},
		{App: strings.Repeat("a", 80), Device: long, At: testNow},
		{App: "<b>Todo</b>", Device: "", At: testNow},
	}
	h := newTestHandler(f)
	w := serve(h, request(http.MethodGet, "/api/pairing/refusals", ""))
	wantStatus(t, w, http.StatusOK)
	var got []struct{ App, Device string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []struct{ App, Device string }{
		{"notes", "Pixel 8"},
		{strings.Repeat("a", 64) + "…", strings.Repeat("я", 64) + "…"},
		{"<b>Todo</b>", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refusals = %q, want %q", got, want)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("<b>")) {
		t.Errorf("JSON carries a raw <: %s", w.Body.String())
	}
}

// The page offers "Add it?" when the shown id matches the pattern, so
// shortening must never turn an invalid id into a valid one.
func TestShortenKeepsIDValidity(t *testing.T) {
	for _, id := range []string{
		"", "todo", strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("a", 200),
		"Todo", " todo", "todo ", "to\x00do", strings.Repeat("a", 70) + "B", "тудо",
	} {
		if got := shorten(id); apps.ValidID(got) != apps.ValidID(id) {
			t.Errorf("shorten(%q) = %q: valid %v, was %v", id, got, apps.ValidID(got), apps.ValidID(id))
		}
	}
}

// --- The page --------------------------------------------------------------

func getPage(t *testing.T, acceptLanguage string) string {
	t.Helper()
	r := request(http.MethodGet, "/", "")
	if acceptLanguage != "" {
		r.Header.Set("Accept-Language", acceptLanguage)
	}
	w := serve(newTestHandler(newFake()), r)
	wantStatus(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	return w.Body.String()
}

func TestLanguage(t *testing.T) {
	for _, tc := range []struct{ header, lang, want string }{
		{"ru-RU,ru;q=0.9", "ru", "Подключить устройство"},
		{"pl", "pl", "Sparuj urządzenie"},
		{"de", "en", "Pair a device"},
		{"", "en", "Pair a device"},
	} {
		page := getPage(t, tc.header)
		if !strings.Contains(page, tc.want) {
			t.Errorf("Accept-Language %q: the page lacks %q", tc.header, tc.want)
		}
		if !strings.Contains(page, `<html lang="`+tc.lang+`">`) {
			t.Errorf("Accept-Language %q: the page is not marked lang=%q", tc.header, tc.lang)
		}
	}
}

var stringsBlock = regexp.MustCompile(`(?s)<script type="application/json" id="strings">(.*?)</script>`)

func TestPageEmbedsTheLanguagesStrings(t *testing.T) {
	for lang, accept := range map[string]string{"en": "en-GB", "ru": "ru", "pl": "pl-PL"} {
		m := stringsBlock.FindStringSubmatch(getPage(t, accept))
		if m == nil {
			t.Fatalf("%s: no strings block in the page", lang)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
			t.Fatalf("%s: strings block is not JSON: %v", lang, err)
		}
		if !reflect.DeepEqual(got, messages[lang]) {
			t.Errorf("%s: the page embeds other strings", lang)
		}
	}
}

func TestPageLoadsNoInlineScript(t *testing.T) {
	page := getPage(t, "en")
	scripts := regexp.MustCompile(`<script[^>]*>`).FindAllString(page, -1)
	want := []string{`<script type="application/json" id="strings">`, `<script src="/assets/app.js" defer>`}
	if !reflect.DeepEqual(scripts, want) {
		t.Errorf("script tags = %q, want %q", scripts, want)
	}
	if strings.Contains(page, " style=") || strings.Contains(page, "<style") {
		t.Error("the page has inline styles, which the CSP blocks")
	}
}

func TestRenderPageEscapesStrings(t *testing.T) {
	hostile := make(map[string]string, len(messages["en"]))
	for k := range messages["en"] {
		hostile[k] = `</script><script>alert(1)</script>"><img src=x onerror=alert(1)>&amp;`
	}
	page, err := renderPage("en", hostile)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script>alert", "<img src=x", "</script><"} {
		if bytes.Contains(page, []byte(bad)) {
			t.Errorf("the page contains %q unescaped", bad)
		}
	}
	m := stringsBlock.FindSubmatch(page)
	if m == nil {
		t.Fatal("no strings block")
	}
	var got map[string]string
	if err := json.Unmarshal(m[1], &got); err != nil || !reflect.DeepEqual(got, hostile) {
		t.Errorf("hostile strings do not round-trip: %v", err)
	}
}

func TestAssets(t *testing.T) {
	h := newTestHandler(newFake())
	for path, ctype := range map[string]string{
		"/assets/app.js":    "text/javascript; charset=utf-8",
		"/assets/style.css": "text/css; charset=utf-8",
	} {
		w := serve(h, request(http.MethodGet, path, ""))
		wantStatus(t, w, http.StatusOK)
		if got := w.Header().Get("Content-Type"); got != ctype {
			t.Errorf("%s: Content-Type = %q, want %q", path, got, ctype)
		}
		if w.Body.Len() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
	for _, path := range []string{"/assets/index.html", "/assets/nope.js", "/index.html", "/nope", "/api/nope", "/api/apps/todo/nope"} {
		t.Run(path, func(t *testing.T) {
			wantError(t, serve(h, request(http.MethodGet, path, "")), http.StatusNotFound, "notFound")
		})
	}
}

// A route asked with a method it does not take answers JSON too, with the
// methods it takes.
func TestWrongMethodIsJSON(t *testing.T) {
	h := newTestHandler(newFake())
	w := serve(h, request(http.MethodPost, "/api/status", ""))
	wantError(t, w, http.StatusMethodNotAllowed, "invalid")
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want GET in it", allow)
	}
	w = serve(h, request(http.MethodDelete, "/api/apps", ""))
	wantError(t, w, http.StatusMethodNotAllowed, "invalid")
}

// The message for an expired code names the button that makes a new one.
func TestExpiredNamesTheButton(t *testing.T) {
	for lang, s := range messages {
		if !strings.Contains(s["expired"], s["newCode"]) {
			t.Errorf("%s: expired %q does not name the button %q", lang, s["expired"], s["newCode"])
		}
	}
}

// --- Strings ---------------------------------------------------------------

func TestPickLanguage(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"", "en"},
		{"ru-RU,ru;q=0.9", "ru"},
		{"pl", "pl"},
		{"de", "en"},
		{"de,ru;q=0.5", "ru"},
		{"en-US,en;q=0.9,ru;q=0.8", "en"},
		{"ru;q=0.5,pl;q=0.8", "pl"},
		{"PL-pl", "pl"},
		{"ru;q=0", "en"},
		{"ru;q=0,pl;q=0.1", "pl"},
		{"*", "en"},
		{"de,*;q=0.5,ru;q=0.4", "en"},
		{"ru;q=abc", "en"},
		{"ru;q=NaN", "en"},
		{"ru;q=2", "en"},
		{" ru ; q=0.7 , pl ; q=0.7", "ru"},
		{"uk-UA, ru;q=0.8, en;q=0.5", "ru"},
		{",,;;", "en"},
	} {
		if got := pickLanguage(tc.header); got != tc.want {
			t.Errorf("pickLanguage(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

var placeholder = regexp.MustCompile(`\{\w+\}`)

func TestStringsComplete(t *testing.T) {
	langs := make([]string, 0, len(messages))
	for lang := range messages {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	if !reflect.DeepEqual(langs, []string{"en", "pl", "ru"}) {
		t.Fatalf("languages = %v, want en, pl, ru", langs)
	}
	en := messages["en"]
	for _, lang := range langs {
		strs := messages[lang]
		for k, v := range en {
			got, ok := strs[k]
			if !ok || strings.TrimSpace(got) == "" {
				t.Errorf("%s: %q is missing", lang, k)
				continue
			}
			p, want := placeholder.FindAllString(got, -1), placeholder.FindAllString(v, -1)
			sort.Strings(p)
			sort.Strings(want)
			if !slices.Equal(p, want) {
				t.Errorf("%s: %q has placeholders %v, English has %v", lang, k, p, want)
			}
		}
		for k := range strs {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: %q is not in English", lang, k)
			}
		}
	}
}

// Every string the script or the page names exists, and every string is used.
func TestStringsUsed(t *testing.T) {
	script, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bS\.(\w+)`).FindAllSubmatch(script, -1) {
		used[string(m[1])] = true
	}
	for _, m := range regexp.MustCompile(`\.T\.(\w+)`).FindAllSubmatch(page, -1) {
		used[string(m[1])] = true
	}
	for k := range used {
		if _, ok := messages["en"][k]; !ok {
			t.Errorf("%q is used but not defined", k)
		}
	}
	for k := range messages["en"] {
		if !used[k] {
			t.Errorf("%q is defined but never used", k)
		}
	}
	if bytes.Contains(script, []byte("innerHTML")) || bytes.Contains(script, []byte("outerHTML")) ||
		bytes.Contains(script, []byte("insertAdjacentHTML")) || bytes.Contains(script, []byte("document.write")) {
		t.Error("app.js parses HTML; build the DOM with createElement and textContent")
	}
}

// The server controls: their strings exist in every language, and every
// language's page holds their markup.
func TestPageHasTheServerControls(t *testing.T) {
	keys := []string{
		"dataDirLabel", "dataDirHint", "dataDirChange", "moveTitle", "moveText", "moveOk",
		"adoptTitle", "adoptText", "adoptOk", "errDataExists", "errDataInvalid", "errDataChanged",
		"restarting", "restartFailed", "restartError", "manageTitle", "manageIntro",
		"stopButton", "resetButton", "deleteServerButton", "stopTitle", "stopText", "stopOk",
		"resetTitle", "resetText", "resetOk", "deleteServerTitle", "deleteServerText", "deleteServerOk",
		"devicesFact", "programFact", "serverDirFact", "dataDirFact",
		"stoppedTitle", "stoppedText", "stoppedDeletedText",
	}
	for lang, strs := range messages {
		for _, k := range keys {
			if strings.TrimSpace(strs[k]) == "" {
				t.Errorf("%s: %q is missing", lang, k)
			}
		}
	}
	for _, accept := range []string{"en", "ru", "pl"} {
		page := getPage(t, accept)
		for _, id := range []string{
			`id="data-form"`, `id="data-dir"`, `id="data-dir-change"`, `id="manage"`,
			`id="btn-stop"`, `id="btn-reset"`, `id="btn-delete-server"`,
			`<dialog id="wipe"`, `id="wipe-ok"`, `id="wipe-cancel"`, `id="stopped"`,
		} {
			if !strings.Contains(page, id) {
				t.Errorf("%s: the page lacks %s", accept, id)
			}
		}
		if !regexp.MustCompile(`<section id="stopped"[^>]*\bhidden\b`).MatchString(page) {
			t.Errorf("%s: the stopped panel is not hidden at first", accept)
		}
	}
}

func withMarker(r *http.Request) *http.Request {
	r.Header.Set("X-Workspace-Server", "1")
	return r
}
