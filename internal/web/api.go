package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skip2/go-qrcode"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/devices"
	"github.com/FLighter7/workspace-server/internal/upstream"
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
	// codeUpstream: the upstream server refused the link or could not be
	// reached; the answer's reason says why.
	codeUpstream = "upstream"
	// codeNotMain: a subordinate issues no pairing codes.
	codeNotMain = "notMain"
	// codeNotLinked: there is no link to undo or to ask through.
	codeNotLinked = "notLinked"
	// codeHasSubordinates: a main with subordinates does not link.
	codeHasSubordinates = "hasSubordinates"
	// codeNotInStep: a planned switch found the documents not yet the same
	// on this server and its main.
	codeNotInStep = "notInStep"
	// codeDiverged: a planned switch of the main found an app whose
	// documents diverged from the main's.
	codeDiverged = "diverged"
	// codeNotConnected: the server to hand the role to is not connected.
	codeNotConnected = "notConnected"
	// codeNotStopped: the write guard has not stopped the app to resume.
	codeNotStopped = "notStopped"
	// codeFollowsMain: a subordinate takes the apps' quotas from its main.
	codeFollowsMain = "followsMain"
)

const (
	// linkTimeout bounds linking to an upstream server: reaching it and
	// pairing.
	linkTimeout = 30 * time.Second
	// switchTimeout bounds switching the main: the documents in step, the
	// role taken, and the former main told.
	switchTimeout = 45 * time.Second
)

const (
	maxBodyBytes = 64 << 10 // the largest request body read
	qrPixels     = 512      // the side of the pairing QR code
	displayRunes = 64       // refused app ids and device names are cut to this
)

type api struct {
	b Backend
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

// readOptionalJSON reads the body into v as readJSON does, and takes an
// empty body as v left as it is.
func readOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalid, "request body: "+err.Error())
		return false
	}
	if strings.TrimSpace(string(body)) == "" {
		return true
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	return readJSON(w, r, v)
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
	for i := range list {
		if list[i].Diverged == nil {
			list[i].Diverged = []Divergence{}
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// addAppRequest is an app to add: a zero quota is the default, a blank
// name the id.
type addAppRequest struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Quota int64  `json:"quota"`
}

func (a *api) addApp(w http.ResponseWriter, r *http.Request) {
	var req addAppRequest
	if !readJSON(w, r, &req) {
		return
	}
	app := apps.App{ID: req.ID, Name: req.Name, Quota: req.Quota}
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

// setAppRequest is the body of PUT /api/apps/{id}: the app's quota in
// bytes, zero for the default. It is the only field that changes.
type setAppRequest struct {
	Quota *int64 `json:"quota"`
}

// setApp changes an app's quota. A main's subordinates take it with the app
// list; a subordinate refuses, since it takes its main's.
func (a *api) setApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !apps.ValidID(id) {
		notFound(w, "app")
		return
	}
	var req setAppRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Quota == nil {
		writeError(w, http.StatusBadRequest, codeInvalid, "quota is missing")
		return
	}
	switch err := a.b.SetQuota(id, *req.Quota); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, apps.ErrNotFound):
		notFound(w, "app")
	case errors.Is(err, ErrFollowsMain):
		writeError(w, http.StatusConflict, codeFollowsMain, err.Error())
	case errors.Is(err, apps.ErrInvalid):
		writeError(w, http.StatusBadRequest, codeInvalid, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// openApp shows the app's folder in the system's file manager, on the
// computer the server runs on, which is the one the page is open on.
func (a *api) openApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !apps.ValidID(id) {
		notFound(w, "app")
		return
	}
	switch err := a.b.OpenFolder(id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, apps.ErrNotFound):
		notFound(w, "app")
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
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

// moveRequest is the body of POST /api/devices/{id}/move: the machine the
// device goes on, "" for a new one.
type moveRequest struct {
	Machine *string `json:"machine"`
}

// moveResponse names the machine the device is on now.
type moveResponse struct {
	Machine string `json:"machine"`
}

// moveDevice puts a device of an app on another machine: two machines
// merge when every device of one moves onto the other, and one splits off
// when it moves to a new machine.
func (a *api) moveDevice(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Machine == nil {
		writeError(w, http.StatusBadRequest, codeInvalid, "machine is missing")
		return
	}
	machine, err := a.b.MoveDevice(r.PathValue("id"), *req.Machine)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, moveResponse{Machine: machine})
	case errors.Is(err, devices.ErrNoDevice):
		notFound(w, "device")
	case errors.Is(err, devices.ErrNoMachine):
		notFound(w, "machine")
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

func (a *api) listMachines(w http.ResponseWriter, r *http.Request) {
	list := a.b.Machines()
	if list == nil {
		list = []MachineInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

// renameRequest is the body of PUT /api/machines/{id}.
type renameRequest struct {
	Name *string `json:"name"`
}

func (a *api) renameMachine(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == nil {
		writeError(w, http.StatusBadRequest, codeInvalid, "name is missing")
		return
	}
	switch err := a.b.RenameMachine(r.PathValue("id"), *req.Name); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, devices.ErrNoMachine):
		notFound(w, "machine")
	case errors.Is(err, devices.ErrInvalidName):
		writeError(w, http.StatusBadRequest, codeInvalid, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

func (a *api) removeMachine(w http.ResponseWriter, r *http.Request) {
	if !a.b.RemoveMachine(r.PathValue("id")) {
		notFound(w, "machine")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pairingRequest is the optional body of POST /api/pairing and POST
// /api/upstream/invite: the machine the device that pairs joins, absent or
// "" for a new one.
type pairingRequest struct {
	Machine string `json:"machine"`
}

type pairingResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
	QR        string    `json:"qr"` // a data: URL of a PNG
}

func (a *api) issuePairing(w http.ResponseWriter, r *http.Request) {
	var req pairingRequest
	if !readOptionalJSON(w, r, &req) {
		return
	}
	code, expires, err := a.b.IssuePairing(req.Machine)
	switch {
	case errors.Is(err, ErrNotMain):
		writeError(w, http.StatusConflict, codeNotMain, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	default:
		writePairing(w, code, expires)
	}
}

// writePairing answers with a pairing code, as text and as a QR code.
func writePairing(w http.ResponseWriter, code string, expires time.Time) {
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

func (a *api) upstreamStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.upstream())
}

// upstream is the link's status, its app list [] rather than null.
func (a *api) upstream() UpstreamStatus {
	st := a.b.Upstream()
	if st.Apps == nil {
		st.Apps = []string{}
	}
	return st
}

// linkRequest is the body of POST /api/upstream: the main's pairing code.
type linkRequest struct {
	Code string `json:"code"`
}

// refusedBody is the answer when the upstream server refused the link or
// could not be reached. Reason is a protocol error code (badSecret,
// blocked, connectTimeout, closed, ...) or notMain or serverChanged.
type refusedBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// linkUpstream links this server to a main and answers with the link's
// status. It returns once the pairing is done, which can take a while.
func (a *api) linkUpstream(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), linkTimeout)
	defer cancel()
	err := a.b.LinkUpstream(ctx, req.Code)
	var refused *upstream.RefusedError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, a.upstream())
	case errors.Is(err, upstream.ErrBadCode), errors.Is(err, upstream.ErrOwnCode):
		writeError(w, http.StatusBadRequest, codeInvalid, err.Error())
	case errors.Is(err, upstream.ErrLinked):
		writeError(w, http.StatusConflict, codeExists, err.Error())
	case errors.Is(err, upstream.ErrBusy):
		writeError(w, http.StatusConflict, codeBusy, err.Error())
	case errors.Is(err, ErrHasSubordinates):
		writeError(w, http.StatusConflict, codeHasSubordinates, err.Error())
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadGateway, refusedBody{Error: codeUpstream, Message: err.Error(), Reason: refused.Reason})
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// unlinkUpstream ends the link and answers whether the main no longer knows
// this server: {"forgotten": bool}.
func (a *api) unlinkUpstream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), linkTimeout)
	defer cancel()
	forgotten, err := a.b.UnlinkUpstream(ctx)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"forgotten": forgotten})
	case errors.Is(err, upstream.ErrNotLinked):
		writeError(w, http.StatusConflict, codeNotLinked, err.Error())
	case errors.Is(err, upstream.ErrBusy):
		writeError(w, http.StatusConflict, codeBusy, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// inviteUpstream answers with a pairing code of the main, which it issued
// over the link for a browser on this computer, as POST /api/pairing
// answers with this server's own.
func (a *api) inviteUpstream(w http.ResponseWriter, r *http.Request) {
	var req pairingRequest
	if !readOptionalJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), linkTimeout)
	defer cancel()
	code, expires, err := a.b.InviteUpstream(ctx, req.Machine)
	var refused *upstream.RefusedError
	switch {
	case err == nil:
		writePairing(w, code, expires)
	case errors.Is(err, upstream.ErrNotLinked):
		writeError(w, http.StatusConflict, codeNotLinked, err.Error())
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadGateway, refusedBody{Error: codeUpstream, Message: err.Error(), Reason: refused.Reason})
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// promoteRequest is the body of POST /api/upstream/promote: force for a
// switch while the main is out of reach.
type promoteRequest struct {
	Force bool `json:"force"`
}

// promoteUpstream makes this server the main of its family, and answers
// whether the former main followed (always false for a forced switch, which
// tells it once the two meet): {"followed": bool}.
func (a *api) promoteUpstream(w http.ResponseWriter, r *http.Request) {
	var req promoteRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), switchTimeout)
	defer cancel()
	followed, err := a.b.PromoteUpstream(ctx, req.Force)
	var refused *upstream.RefusedError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"followed": followed})
	case errors.Is(err, upstream.ErrNotLinked):
		writeError(w, http.StatusConflict, codeNotLinked, err.Error())
	case errors.Is(err, upstream.ErrBusy):
		writeError(w, http.StatusConflict, codeBusy, err.Error())
	case errors.Is(err, upstream.ErrNotInStep):
		writeError(w, http.StatusConflict, codeNotInStep, err.Error())
	case errors.Is(err, upstream.ErrDiverged):
		writeError(w, http.StatusConflict, codeDiverged, err.Error())
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadGateway, refusedBody{Error: codeUpstream, Message: err.Error(), Reason: refused.Reason})
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// handOverRequest is the body of POST /api/upstream/hand-over: the server
// id of the subordinate to take the role of main.
type handOverRequest struct {
	ServerID string `json:"serverId"`
}

// handOver asks a subordinate to take the role of main over and answers
// whether this server stepped down to it in time: {"done": bool}.
func (a *api) handOver(w http.ResponseWriter, r *http.Request) {
	var req handOverRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), switchTimeout)
	defer cancel()
	done, err := a.b.HandOver(ctx, req.ServerID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"done": done})
	case errors.Is(err, ErrNotMain):
		writeError(w, http.StatusConflict, codeNotMain, err.Error())
	case errors.Is(err, ErrNoSuchServer):
		notFound(w, "server")
	case errors.Is(err, ErrNotConnected):
		writeError(w, http.StatusConflict, codeNotConnected, err.Error())
	case errors.Is(err, upstream.ErrDiverged):
		writeError(w, http.StatusConflict, codeDiverged, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}

// resumeRequest is the body of POST /api/upstream/resume: an app the write
// guard stopped.
type resumeRequest struct {
	App string `json:"app"`
}

// resumeUpstream resumes an app the write guard stopped and answers with the
// link's status, which lists the app as synced again. It answers at once:
// the app syncs with the main right after, or once the main is reachable.
func (a *api) resumeUpstream(w http.ResponseWriter, r *http.Request) {
	var req resumeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !apps.ValidID(req.App) {
		writeError(w, http.StatusBadRequest, codeInvalid, fmt.Sprintf("app %q must match ^[a-z0-9-]{1,64}$", req.App))
		return
	}
	err := a.b.ResumeUpstream(req.App)
	var refused *upstream.RefusedError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, a.upstream())
	case errors.Is(err, upstream.ErrNotLinked):
		writeError(w, http.StatusConflict, codeNotLinked, err.Error())
	case errors.Is(err, apps.ErrNotFound):
		notFound(w, "app")
	case errors.Is(err, upstream.ErrNotStopped):
		writeError(w, http.StatusConflict, codeNotStopped, err.Error())
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadGateway, refusedBody{Error: codeUpstream, Message: err.Error(), Reason: refused.Reason})
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
	}
}
