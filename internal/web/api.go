package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skip2/go-qrcode"

	"github.com/FLighter7/workspace-server/internal/apps"
)

// Error codes in {"error": code, "message": ...}. The message is English, for
// a developer; the page shows its own text for each code.
const (
	codeInvalid         = "invalid"
	codeExists          = "exists"
	codeConfirmMismatch = "confirmMismatch"
	codeNotFound        = "notFound"
	codeForbidden       = "forbidden"
	codeInternal        = "internal"
	codeDataExists      = "dataExists"
	codeChanged         = "changed"
	codeBusy            = "busy"
)

const (
	maxBodyBytes = 64 << 10 // the largest request body read
	qrPixels     = 512      // the side of the pairing QR code
	displayRunes = 64       // refused app ids and device names are cut to this
)

type api struct {
	b   Backend
	now func() time.Time
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{code, message})
}

// readJSON decodes the request body into v: one JSON value, at most
// maxBodyBytes, with no unknown fields. Otherwise it answers 400 invalid and
// returns false.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("more than one JSON value")
	}
	if errors.Is(err, io.EOF) {
		err = errors.New("empty")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalid, "request body: "+err.Error())
		return false
	}
	return true
}

func notFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, codeNotFound, what+" not found")
}

func (a *api) status(w http.ResponseWriter, r *http.Request) {
	st := a.b.Status()
	if st.Addresses == nil {
		st.Addresses = []string{}
	}
	writeJSON(w, http.StatusOK, st)
}

// configRequest holds the fields to change; an absent field is unchanged.
// The data folder changes on its own, since it restarts the server, and with
// the verdict the page saw when it checked the folder.
type configRequest struct {
	Name     *string `json:"name"`
	UDPPort  *int    `json:"udpPort"`
	HTTPPort *int    `json:"httpPort"`
	DataDir  *string `json:"dataDir"`
	Verdict  *string `json:"verdict"`
}

func (a *api) setConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.DataDir != nil {
		switch {
		case req.Name != nil || req.UDPPort != nil || req.HTTPPort != nil:
			writeError(w, http.StatusBadRequest, codeInvalid, "change the data folder on its own")
		case req.Verdict == nil:
			writeError(w, http.StatusBadRequest, codeInvalid, "verdict is missing: send the one GET /api/data-dir gave")
		default:
			a.setDataDir(w, *req.DataDir, *req.Verdict)
		}
		return
	}
	if req.Verdict != nil {
		writeError(w, http.StatusBadRequest, codeInvalid, "verdict goes with dataDir only")
		return
	}
	if err := a.b.SetConfig(req.Name, req.UDPPort, req.HTTPPort); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalid, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) setDataDir(w http.ResponseWriter, path, verdict string) {
	restart, err := a.b.SetDataDir(path, verdict)
	switch {
	case errors.Is(err, ErrDataConflict):
		writeError(w, http.StatusConflict, codeDataExists, err.Error())
	case errors.Is(err, ErrDataChanged):
		writeError(w, http.StatusConflict, codeChanged, err.Error())
	case errors.Is(err, ErrBusy):
		writeError(w, http.StatusConflict, codeBusy, err.Error())
	case errors.Is(err, ErrDataInvalid):
		writeError(w, http.StatusBadRequest, codeInvalid, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	case !restart:
		w.WriteHeader(http.StatusNoContent)
	default:
		answerThen(w, map[string]bool{"restarting": true}, a.b.Restart)
	}
}

// answerThen writes 202 with body and flushes it before it calls then, which
// stops or restarts the server: the caller learns that its request was taken
// even if the connection closes right after. The web server's shutdown also
// waits for requests in flight, this one included.
func answerThen(w http.ResponseWriter, body any, then func()) {
	writeJSON(w, http.StatusAccepted, body)
	_ = http.NewResponseController(w).Flush()
	then()
}

// stopRequest is the body of POST /api/server/stop.
type stopRequest struct {
	RemoveAutostart bool `json:"removeAutostart"`
}

func (a *api) stopServer(w http.ResponseWriter, r *http.Request) {
	var req stopRequest
	if !readJSON(w, r, &req) {
		return
	}
	answerThen(w, map[string]bool{"stopping": true}, func() { a.b.Stop(req.RemoveAutostart) })
}

func (a *api) deleteServer(w http.ResponseWriter, r *http.Request) {
	answerThen(w, map[string]bool{"stopping": true}, a.b.DeleteServer)
}

func (a *api) resetData(w http.ResponseWriter, r *http.Request) {
	answerThen(w, map[string]bool{"restarting": true}, a.b.Reset)
}

func (a *api) checkDataDir(w http.ResponseWriter, r *http.Request) {
	// A GET passes the guard without the marker header, and another site
	// can send one with an <img> and no Origin. This one writes a probe file
	// and tells which paths exist, so it needs the header too.
	if r.Header.Get(markerHeader) != "1" {
		writeError(w, http.StatusForbidden, codeForbidden, "this request must carry the header "+markerHeader+": 1")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, codeInvalid, "the query parameter path is missing")
		return
	}
	c, err := a.b.CheckDataDir(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	if c.Current == nil {
		c.Current = []AppData{}
	}
	if c.Target == nil {
		c.Target = []AppData{}
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *api) listApps(w http.ResponseWriter, r *http.Request) {
	list := a.b.Apps()
	if list == nil {
		list = []AppInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *api) addApp(w http.ResponseWriter, r *http.Request) {
	var app apps.App
	if !readJSON(w, r, &app) {
		return
	}
	if !apps.ValidID(app.ID) {
		writeError(w, http.StatusBadRequest, codeInvalid, fmt.Sprintf("id %q must match ^[a-z0-9-]{1,64}$", app.ID))
		return
	}
	switch err := a.b.AddApp(app); {
	case err == nil:
		w.WriteHeader(http.StatusCreated)
	case errors.Is(err, apps.ErrInvalid):
		writeError(w, http.StatusBadRequest, codeInvalid, err.Error())
	case errors.Is(err, apps.ErrExists):
		writeError(w, http.StatusBadRequest, codeExists, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// deleteRequest repeats the app's id, typed by the user.
type deleteRequest struct {
	Confirm string `json:"confirm"`
}

func (a *api) deleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !apps.ValidID(id) {
		notFound(w, "app")
		return
	}
	var req deleteRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != id {
		writeError(w, http.StatusBadRequest, codeConfirmMismatch, "confirm must equal the app id")
		return
	}
	switch err := a.b.DeleteApp(id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, apps.ErrNotFound):
		notFound(w, "app")
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// document sends the app's current document as a download named after the
// app and the day the copy was taken, in the server's local time.
func (a *api) document(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !apps.ValidID(id) {
		notFound(w, "app")
		return
	}
	data, ok := a.b.Document(id)
	if !ok {
		notFound(w, "document")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.json"`, id, a.now().Local().Format("2006-01-02")))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (a *api) listDevices(w http.ResponseWriter, r *http.Request) {
	list := a.b.Devices()
	if list == nil {
		list = []DeviceInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *api) removeDevice(w http.ResponseWriter, r *http.Request) {
	if !a.b.RemoveDevice(r.PathValue("id")) {
		notFound(w, "device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pairingResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
	QR        string    `json:"qr"` // a data: URL of a PNG
}

func (a *api) issuePairing(w http.ResponseWriter, r *http.Request) {
	code, expires, err := a.b.IssuePairing()
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	png, err := qrcode.Encode(code, qrcode.Low, qrPixels)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "QR code: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pairingResponse{
		Code:      code,
		ExpiresAt: expires,
		QR:        "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

// refusalJSON is a refused pairing as the page shows it. Both strings came
// from a remote device, so they are cut for display; the page escapes them.
type refusalJSON struct {
	App    string    `json:"app"`
	Device string    `json:"device"`
	At     time.Time `json:"at"`
}

func (a *api) refusals(w http.ResponseWriter, r *http.Request) {
	list := a.b.Refusals()
	out := make([]refusalJSON, 0, len(list))
	for _, f := range list {
		out = append(out, refusalJSON{
			App:    shorten(f.App),
			Device: shorten(strings.TrimSpace(f.Device)),
			At:     f.At,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// shorten cuts s to displayRunes runes and marks the cut with an ellipsis.
// The page offers "Add it?" for an app id that matches the id pattern, so
// shorten never makes an invalid id valid: it changes only strings longer
// than any valid id, and the ellipsis is not allowed in one.
func shorten(s string) string {
	if utf8.RuneCountInString(s) <= displayRunes {
		return s
	}
	return string([]rune(s)[:displayRunes]) + "…"
}
