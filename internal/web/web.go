// Package web serves the page on 127.0.0.1 that manages the server: its name
// and ports, the apps it serves, the paired devices, and pairing. The page and
// its JSON API answer only requests that come from the page itself.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/pairing"
)

// Status is what the Server section shows.
type Status struct {
	Name        string   `json:"name"`
	ServerID    string   `json:"serverId"`
	MDNSName    string   `json:"mdnsName"`
	MDNSRunning bool     `json:"mdnsRunning"`
	Listening   bool     `json:"listening"` // the UDP port is open; false: devices cannot connect
	Addresses   []string `json:"addresses"`
	UDPPort     int      `json:"udpPort"`
	HTTPPort    int      `json:"httpPort"`
	Program     string   `json:"program"`    // the program file
	ServerDir   string   `json:"serverDir"`  // settings, identity, devices, log
	DataDir     string   `json:"dataDir"`    // the apps' data; never empty
	MainServer  bool     `json:"mainServer"` // the default server folder: owns autostart and the program file
	LastError   string   `json:"lastError"`  // why the last reset or data move failed; "" when none did
}

// DataDirCheck is what changing the data folder to Path would do. Verdict is
// one of same, create, move, switch, adopt, conflict, unavailable.
type DataDirCheck struct {
	Path    string    `json:"path"`
	Verdict string    `json:"verdict"`
	Message string    `json:"message"`
	Current []AppData `json:"current"` // the apps' data in the current folder
	Target  []AppData `json:"target"`  // the apps' data already in Path
}

// AppData is an app's data in a data folder.
type AppData struct {
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Versions int    `json:"versions"`
}

// SetDataDir's errors, which the page shows differently.
var (
	// ErrDataConflict: both the current and the new folder hold data.
	ErrDataConflict = errors.New("the folder already holds data of a workspace-server")
	// ErrDataChanged: the new folder changed since the page checked it.
	ErrDataChanged = errors.New("the folder changed since it was checked")
	// ErrDataInvalid: the folder cannot be used.
	ErrDataInvalid = errors.New("the folder cannot be used")
	// ErrBusy: the server is already changing its data folder or stopping.
	ErrBusy = errors.New("the server is already being changed or stopped")
)

// AppInfo is a served app and the state of its data.
type AppInfo struct {
	apps.App
	Size     int64      `json:"size"`     // bytes of the current document; 0 when there is none
	Modified *time.Time `json:"modified"` // the last write; nil when there is no document
	Versions int        `json:"versions"` // previous documents kept in history
	Devices  int        `json:"devices"`  // devices paired with the app
}

// DeviceInfo is one paired device.
type DeviceInfo struct {
	ID        string    `json:"id"`
	App       string    `json:"app"`
	Name      string    `json:"name"`
	PairedAt  time.Time `json:"pairedAt"`
	LastSeen  time.Time `json:"lastSeen"`
	Connected bool      `json:"connected"` // a session is open now
}

// Backend is the server as the page manages it.
type Backend interface {
	Status() Status
	// SetConfig changes the fields that are not nil. Its error is shown to
	// the user as the reason the values were refused.
	SetConfig(name *string, udpPort, httpPort *int) error
	Apps() []AppInfo
	// AddApp applies the defaults to zero fields and adds the app. Its
	// errors wrap apps.ErrInvalid or apps.ErrExists.
	AddApp(a apps.App) error
	// DeleteApp removes the app, its folder and its devices, closing their
	// sessions. An unknown app is an error wrapping apps.ErrNotFound.
	DeleteApp(id string) error
	// Document returns the app's current document, or false when there is
	// none.
	Document(id string) ([]byte, bool)
	Devices() []DeviceInfo
	// RemoveDevice removes the device and closes its session. It reports
	// false when there is no such device.
	RemoveDevice(id string) bool
	// IssuePairing creates a pairing code with a new secret, revoking the
	// previous one.
	IssuePairing() (code string, expires time.Time, err error)
	// Refusals returns the recent pairings refused for an unknown app,
	// newest first.
	Refusals() []pairing.Refusal

	// The server controls. The handlers answer first and call these after:
	// each makes the server stop or restart once the answer is out. A call
	// while one is pending is ignored.

	// Stop stops the server; removeAutostart also removes it from the login
	// items as it exits.
	Stop(removeAutostart bool)
	// Reset restarts the server as if new, keeping its settings.
	Reset()
	// DeleteServer stops the server and removes it from the machine.
	DeleteServer()
	// CheckDataDir tells what changing the data folder to path would do. Its
	// error is a failure to read the current data.
	CheckDataDir(path string) (DataDirCheck, error)
	// SetDataDir checks path again and records the change; restart reports
	// whether Restart must follow (false: path is the current folder).
	// verdict is the one the page saw from CheckDataDir; a different one now
	// is ErrDataChanged, so that the user never gets a move they did not
	// see. Its errors wrap ErrDataConflict, ErrDataChanged, ErrDataInvalid
	// or ErrBusy, or are internal.
	SetDataDir(path, verdict string) (restart bool, err error)
	// Restart restarts the server to apply what SetDataDir recorded.
	Restart()
}

//go:embed assets
var assetFS embed.FS

// assetTypes lists the files served under /assets/ with their types. The
// page's template is not among them.
var assetTypes = map[string]string{
	"app.js":    "text/javascript; charset=utf-8",
	"style.css": "text/css; charset=utf-8",
}

// NewHandler returns the page and its API. port reports the HTTP port the
// page is served on; it is read on every request, since the port can change
// while the server runs.
func NewHandler(b Backend, port func() int) http.Handler {
	return newHandler(b, port, time.Now)
}

func newHandler(b Backend, port func() int, now func() time.Time) http.Handler {
	a := &api{b: b, now: now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", servePage)
	mux.HandleFunc("GET /assets/{file}", serveAsset)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("PUT /api/config", a.setConfig)
	mux.HandleFunc("GET /api/apps", a.listApps)
	mux.HandleFunc("POST /api/apps", a.addApp)
	mux.HandleFunc("DELETE /api/apps/{id}", a.deleteApp)
	mux.HandleFunc("GET /api/apps/{id}/document", a.document)
	mux.HandleFunc("GET /api/devices", a.listDevices)
	mux.HandleFunc("DELETE /api/devices/{id}", a.removeDevice)
	mux.HandleFunc("POST /api/pairing", a.issuePairing)
	mux.HandleFunc("GET /api/pairing/refusals", a.refusals)
	mux.HandleFunc("POST /api/server/stop", a.stopServer)
	mux.HandleFunc("DELETE /api/server", a.deleteServer)
	mux.HandleFunc("DELETE /api/data", a.resetData)
	mux.HandleFunc("GET /api/data-dir", a.checkDataDir)
	return guard(port, jsonErrors(mux))
}

// jsonErrors answers what the mux itself would answer in plain text, a path
// with no route or a route for other methods, with the API's JSON error
// instead, keeping the mux's Allow header.
func jsonErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			// Through the mux, which sets the path values the handlers read.
			mux.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{header: http.Header{}}
		h.ServeHTTP(rec, r)
		if rec.status == http.StatusMethodNotAllowed {
			allow := rec.header.Get("Allow")
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, codeInvalid, fmt.Sprintf("method %s is not allowed here; allowed: %s", r.Method, allow))
			return
		}
		notFound(w, "path "+r.URL.Path)
	})
}

// statusRecorder takes what the mux's own error handler writes, to learn
// its status and Allow header.
type statusRecorder struct {
	header http.Header
	status int
}

func (s *statusRecorder) Header() http.Header         { return s.header }
func (s *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusRecorder) WriteHeader(status int)      { s.status = status }

var pageTemplate = template.Must(template.New("index.html").
	Option("missingkey=error").
	ParseFS(assetFS, "assets/index.html"))

// pageData is what the page's template reads. T holds the strings; the
// template also embeds all of them as JSON for the script.
type pageData struct {
	Lang     string
	T        map[string]string
	History  int
	MaxBytes int64
}

// renderPage renders the page in one language. html/template escapes every
// string for where it lands, the JSON block for the script included.
func renderPage(lang string, strs map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	err := pageTemplate.Execute(&buf, pageData{
		Lang: lang, T: strs, History: apps.DefaultHistory, MaxBytes: apps.DefaultMaxBytes,
	})
	return buf.Bytes(), err
}

// pages holds the page rendered once in each language.
var pages = func() map[string][]byte {
	out := make(map[string][]byte, len(messages))
	for lang, strs := range messages {
		page, err := renderPage(lang, strs)
		if err != nil {
			panic(fmt.Sprintf("web: render the %s page: %v", lang, err))
		}
		out[lang] = page
	}
	return out
}()

func servePage(w http.ResponseWriter, r *http.Request) {
	lang := pickLanguage(strings.Join(r.Header.Values("Accept-Language"), ","))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	w.Write(pages[lang])
}

func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ctype, ok := assetTypes[name]
	if !ok {
		notFound(w, "asset "+name)
		return
	}
	data, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Write(data)
}
