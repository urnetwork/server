package handlers

import (
	"io"
	"net/http"
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The longest body POST /sidecar/close reads: a capability is 22 characters, so
// this leaves room for a trailing newline and nothing a client would send.
const sidecarCloseMaxByteCount = 64

// POST /sidecar/close: an ephemeral page's best-effort unload beacon (§18.8,
// D-SDK-17; §6.16). Public, a credential of its own kind: navigator.sendBeacon
// sets no Authorization header, so the key-origin document sends, as a
// text/plain body, the close capability its client's mint returned once, which
// reaps that client at once and authorizes nothing else. Answers 204 whether or
// not the capability matched, since a beacon reads no answer, and never echoes
// or logs the body.
func SidecarClose(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sidecarCloseMaxByteCount+1))
	if err != nil || sidecarCloseMaxByteCount < len(body) {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if _, err := model.CloseEphemeralClient(r.Context(), strings.TrimSpace(string(body))); err != nil {
		glog.Infof("[sidecar]close beacon failed: %s\n", err)
		http.Error(w, "Close is unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
