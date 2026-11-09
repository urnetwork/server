package handlers

import (
	"encoding/base64"
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §6.8's sync routes (PROTO-M1-10), Client class on the strict_exp allowlist
// (api/route_authz.go). poll and versions answer a PollResponse, binary,
// application/octet-stream, which WrapRequireClient's JSON formatter cannot
// write, so each passes a formatter of its own; core answers JSON. A cursor
// travels in the query as base64url without padding, since PollResponse's is
// opaque bytes.

// Writes a PollResponse as its 200 body, never 204, which carries no body
// (§6.8).
func sidecarSyncPollResponseFormatter(w http.ResponseWriter) router.FormatFunction[*protocol.PollResponse] {
	return func(_ *session.ClientSession, response *protocol.PollResponse) bool {
		body, err := proto.Marshal(response)
		if err != nil {
			http.Error(w, router.UnexpectedErrorMessage, http.StatusInternalServerError)
			return true
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return true
	}
}

// The cursor the query carries, nil when it carries none, or false when it
// does not decode.
func sidecarSyncCursor(r *http.Request) ([]byte, bool) {
	encoded := r.URL.Query().Get("cursor")
	if encoded == "" {
		return nil, true
	}
	cursor, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	return cursor, true
}

// GET /sidecar/sync/poll?cursor=: held up to 25 s. While sidecar.yml's push
// switch is off it answers 503 with Retry-After before it authenticates, as a
// closed gate does.
func GetSidecarSyncPoll(w http.ResponseWriter, r *http.Request) {
	if !server.CurrentSidecarConfig().Push() {
		server.WriteGateClosed(w, 0)
		return
	}
	cursor, ok := sidecarSyncCursor(r)
	if !ok {
		http.Error(w, "Invalid cursor.", http.StatusBadRequest)
		return
	}
	impl := func(clientSession *session.ClientSession) (*protocol.PollResponse, error) {
		return controller.SidecarSyncPoll(cursor, clientSession)
	}
	router.WrapRequireClient(impl, w, r, sidecarSyncPollResponseFormatter(w))
}

// GET /sidecar/sync/versions?cursor=: one page of the client's versions.
func GetSidecarSyncVersions(w http.ResponseWriter, r *http.Request) {
	cursor, ok := sidecarSyncCursor(r)
	if !ok {
		http.Error(w, "Invalid cursor.", http.StatusBadRequest)
		return
	}
	impl := func(clientSession *session.ClientSession) (*protocol.PollResponse, error) {
		return controller.SidecarSyncVersions(cursor, clientSession)
	}
	router.WrapRequireClient(impl, w, r, sidecarSyncPollResponseFormatter(w))
}

// GET /sidecar/sync/<topic>?since=: core alone, as JSON; any other topic is
// 404.
func GetSidecarSyncTopic(w http.ResponseWriter, r *http.Request) {
	topic := router.GetPathValues(r)[0]
	since := r.URL.Query().Get("since")
	impl := func(clientSession *session.ClientSession) (*controller.SidecarSyncCoreResult, error) {
		return controller.SidecarSyncTopic(topic, since, clientSession)
	}
	router.WrapRequireClient(impl, w, r)
}
