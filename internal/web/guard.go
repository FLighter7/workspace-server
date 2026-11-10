package web

import (
	"net/http"
	"strconv"
)

// markerHeader must carry "1" on every request other than GET and HEAD. A page
// on another site cannot add a custom header without a CORS preflight, and
// the server never grants one.
const markerHeader = "X-Workspace-Server"

// csp lets the page load only its own files, plus the QR code as a data: URL,
// and forbids framing it, so no other site can show it and click its buttons.
const csp = "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// guard answers 403 to any request that did not come from the page itself:
//   - Host must be 127.0.0.1:<port> or localhost:<port>, against DNS
//     rebinding; this covers the page and its assets too;
//   - an Origin, when present, must be the page's own;
//   - every method other than GET and HEAD must carry markerHeader: 1.
//
// It sends no CORS headers, ever. Every response, a refusal included, forbids
// framing and caching.
func guard(port func() int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		// Not "no-referrer": under it a browser may send "Origin: null" on
		// the page's own PUT/POST/DELETE (Fetch, "append a request Origin
		// header"), which the Origin check below refuses. "same-origin"
		// keeps the real Origin on same-origin requests.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if reason := forbiddenReason(r, port()); reason != "" {
			writeError(w, http.StatusForbidden, codeForbidden, reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// forbiddenReason returns why r is refused, or "" when it may proceed.
func forbiddenReason(r *http.Request, port int) string {
	p := strconv.Itoa(port)
	loopback, localhost := "127.0.0.1:"+p, "localhost:"+p
	if r.Host != loopback && r.Host != localhost {
		return "the Host header must be " + loopback + " or " + localhost
	}
	if origins, ok := r.Header["Origin"]; ok {
		if len(origins) != 1 || origins[0] != "http://"+loopback && origins[0] != "http://"+localhost {
			return "requests from other sites are not allowed"
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(markerHeader) != "1" {
		return r.Method + " requests must carry the header " + markerHeader + ": 1"
	}
	return ""
}
