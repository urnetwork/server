// Plural observations retain the singleton's authenticated, bounded, charged
// publication contract. One Http body never becomes one logical quota item.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Authentication and finite framing precede body ownership. Logical quota
// reservations precede all database, chain, signing and publication work.
func SnClientKeyObservations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Client-key observations require POST.", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		http.Error(w, "Client authentication required.", http.StatusUnauthorized)
		return
	}
	if r.Context().Err() != nil {
		http.Error(w, "Client-key batch request is cancelled.", http.StatusRequestTimeout)
		return
	}
	router.WrapRequireClient(func(clientSession *session.ClientSession) (*protocol.ClientKeyObservationBatchResponse, error) {
		defer clientSession.Cancel()
		operationCtx, cancel := context.WithTimeout(clientSession.Ctx, time.Duration(protocol.ClientKeyObservationBatchOperationSeconds)*time.Second)
		defer cancel()
		clientSession.Ctx = operationCtx
		if err := operationCtx.Err(); err != nil {
			return nil, err
		}
		deadline, ok := operationCtx.Deadline()
		if !ok || http.NewResponseController(w).SetWriteDeadline(deadline) != nil {
			return nil, errors.New("503 Client-key batch write deadline unavailable.")
		}
		if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > protocol.MaxClientKeyObservationBatchRequestBytes || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
			return nil, errors.New("400 Client-key batch framing is invalid.")
		}
		select {
		case snClientKeyObservationSlots <- struct{}{}:
			defer func() { <-snClientKeyObservationSlots }()
		default:
			return nil, errors.New("429 Client-key observation busy.")
		}
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(25 * time.Second)); err != nil {
			return nil, errors.New("503 Client-key batch read deadline unavailable.")
		}
		body, err := ownSnAttemptUploadBody(clientSession.Ctx, w, r.Body)
		if err != nil {
			return nil, err
		}
		encoded, err := readSnAttemptUploadBody(clientSession.Ctx, body, uint64(r.ContentLength))
		if err != nil {
			return nil, err
		}
		request, err := protocol.DecodeClientKeyObservationBatchRequest(encoded)
		if err != nil {
			return nil, errors.New("400 Client-key batch request is invalid.")
		}
		for range request.Requests {
			if err := controller.StReserveAttemptUpload(clientSession.Ctx, clientSession.ByJwt.UserId, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
				return nil, err
			}
		}
		result, err := controller.SnClientKeyObservations(&request, clientSession)
		if errors.Is(err, stabi.ErrClientKeyAuthorityRpcWork) {
			w.Header().Set("X-Ur-Client-Key-Batch-Admission", "work")
			return nil, errors.New("413 Client-key batch work admission refused.")
		}
		return result, err
	}, w, r)
}
