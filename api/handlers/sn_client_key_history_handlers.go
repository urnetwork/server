// A key observation creates a durable signed public statement, so it is an
// explicit POST owned by an authenticated client session, not a side effect of
// the legacy public GET /key route. The request cannot supply a key to sign.
package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A fixed active ceiling complements the real configured account/deployment
// request/byte quota; waiting clients cannot retain an unbounded signing queue.
var snClientKeyObservationSlots = make(chan struct{}, protocol.MaxClientKeyObservationActiveOperations)

// Authentication precedes body consumption, quota, chain reads and storage.
// The same retained-body owner used by artifact upload joins deadlines/Close.
func SnClientKeyObservation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Client-key observation requires POST.", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		http.Error(w, "Client authentication required.", http.StatusUnauthorized)
		return
	}
	router.WrapRequireClient(func(clientSession *session.ClientSession) (*controller.SnClientKeyObservationResult, error) {
		defer clientSession.Cancel()
		if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > 16*1024 || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
			return nil, errors.New("400 Client-key observation framing is invalid.")
		}
		select {
		case snClientKeyObservationSlots <- struct{}{}:
			defer func() { <-snClientKeyObservationSlots }()
		default:
			return nil, errors.New("429 Client-key observation busy.")
		}
		// Charge the complete maximum response before any operator signature
		// or public write. Failures retain their configured admission charge.
		if err := controller.StReserveAttemptUpload(clientSession.Ctx, clientSession.ByJwt.UserId, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
			return nil, err
		}
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(25 * time.Second)); err != nil {
			return nil, errors.New("503 Client-key request read deadline unavailable.")
		}
		body, err := ownSnAttemptUploadBody(clientSession.Ctx, w, r.Body)
		if err != nil {
			return nil, err
		}
		encoded, err := readSnAttemptUploadBody(clientSession.Ctx, body, uint64(r.ContentLength))
		if err != nil {
			return nil, err
		}
		var args controller.SnClientKeyObservationArgs
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&args); err != nil {
			return nil, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return nil, errors.New("400 Client-key observation contains trailing JSON.")
		}
		canonical, err := json.Marshal(args)
		if err != nil || !bytes.Equal(canonical, encoded) {
			return nil, errors.New("400 Client-key observation is noncanonical.")
		}
		return controller.SnClientKeyObservation(&args, clientSession)
	}, w, r)
}
