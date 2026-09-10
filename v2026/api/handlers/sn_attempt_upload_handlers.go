// Authenticated clients may stage exact typed bytes, not approve validator
// evidence. The actual public reader and both-origin replay remain mandatory.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/startifact"
)

// Fixed active memory/I/O ceiling is separate from the deployment/account
// hourly byte/count budgets. Waiting for a free upload slot is not allowed.
var snAttemptUploadSlots = make(chan struct{}, 8)

// Production and sim-testnet use this same route and real client-auth wrapper.
func SnUploadAttemptArtifact(w http.ResponseWriter, r *http.Request) {
	serveSnUploadAttemptArtifact(w, r, server.LoadBlobStore, controller.StReserveAttemptUpload,
		startifact.AttemptObjectBounds{MetadataBytes: maximumSnAttemptMetadataBytes, RecordBytes: maximumSnAttemptRecordBytes, ProofBytes: maximumSnAttemptProofBytes}, snAttemptUploadSlots)
}

// The production router captures the one API-owned admission lifecycle. A
// missing cache never turns a supplied reserved header into ordinary traffic.
func SnUploadAttemptArtifactWithReserved(reserved *controller.StReservedAttemptUpload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveSnAttemptUpload(w, r, server.LoadBlobStore, controller.StReserveAttemptUpload,
			startifact.AttemptObjectBounds{MetadataBytes: maximumSnAttemptMetadataBytes, RecordBytes: maximumSnAttemptRecordBytes, ProofBytes: maximumSnAttemptProofBytes}, snAttemptUploadSlots, reserved)
	}
}

// Dependencies are per-call: tests cannot replace shared authentication,
// storage or quota state under concurrent production requests.
func serveSnUploadAttemptArtifact(w http.ResponseWriter, r *http.Request, loadStore func() (server.BlobStore, bool), reserve func(context.Context, server.Id, uint64) error, bounds startifact.AttemptObjectBounds, slots chan struct{}) {
	serveSnAttemptUpload(w, r, loadStore, reserve, bounds, slots, nil)
}

// Both lanes retain exact JWT/body/Close/storage semantics; only the protected
// cache/Redis/slot admission differs. Reserved headers never bypass client auth.
func serveSnAttemptUpload(w http.ResponseWriter, r *http.Request, loadStore func() (server.BlobStore, bool), reserve func(context.Context, server.Id, uint64) error, bounds startifact.AttemptObjectBounds, slots chan struct{}, reserved *controller.StReservedAttemptUpload) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		http.Error(w, "Client authentication required.", http.StatusUnauthorized)
		return
	}
	router.WrapRequireClient(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		ctx, cancel := context.WithTimeout(clientSession.Ctx, 25*time.Second)
		defer cancel()
		if clientSession.ByJwt == nil || clientSession.ByJwt.UserId == (server.Id{}) || clientSession.ByJwt.NetworkId == (server.Id{}) || clientSession.ByJwt.ClientId == nil || *clientSession.ByJwt.ClientId == (server.Id{}) {
			return "", errors.New("401 Client authentication required.")
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) != 2 || len(query["kind"]) != 1 || len(query["hash"]) != 1 || startifact.ValidateAttemptObject(query.Get("kind"), query.Get("hash")) != nil {
			return "", errors.New("400 Invalid typed upload identity.")
		}
		if bounds.Validate() != nil || loadStore == nil || reserve == nil || slots == nil || cap(slots) == 0 {
			return "", errors.New("503 Attempt upload unavailable.")
		}
		kind, contentHash := query.Get("kind"), query.Get("hash")
		limit, contentType := bounds.MetadataBytes, "application/json"
		if kind == "records" {
			limit, contentType = bounds.RecordBytes, "application/x-ndjson"
		}
		if kind == "proofs" {
			limit, contentType = bounds.ProofBytes, "application/x-ndjson"
		}
		mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != contentType || len(parameters) != 0 || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.Header.Values("Content-Range")) != 0 || len(r.Header.Values("Range")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
			return "", errors.New("400 Transformed or partial upload is forbidden.")
		}
		if r.Body == nil || r.ContentLength <= 0 || uint64(r.ContentLength) > limit {
			return "", errors.New("413 Upload requires a positive bounded Content-Length.")
		}
		size, body := uint64(r.ContentLength), r.Body
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("408 Upload canceled: %w", err)
		}
		reservedHeaders := r.Header.Values(protocol.ValidatorAttemptUploadHeader)
		if len(reservedHeaders) != 0 {
			if len(reservedHeaders) != 1 || reservedHeaders[0] == "" || reserved == nil {
				return "", errors.New("403 Validator reserved upload is unavailable or ambiguous.")
			}
			// The existing wrapper already authenticated this exact bearer. Do
			// not accept a differently normalized or second credential here.
			authorization := r.Header.Get("Authorization")
			if !strings.HasPrefix(authorization, "Bearer ") {
				return "", errors.New("401 Exact bearer authentication required.")
			}
			var digest [32]byte
			if _, err := hex.Decode(digest[:], []byte(contentHash[2:])); err != nil {
				return "", errors.New("400 Invalid reserved object digest.")
			}
			lease, err := reserved.Begin(ctx, strings.TrimPrefix(authorization, "Bearer "), reservedHeaders[0], kind, digest, size)
			if err != nil {
				return "", err
			}
			defer lease.Close()
			ctx = lease.Context()
			body, err = ownSnAttemptUploadBody(ctx, w, body)
			if err != nil {
				return "", fmt.Errorf("503 Upload read cancellation unavailable: %w", err)
			}
			if err := reserved.Reserve(lease); err != nil {
				return "", err
			}
		} else {
			body, err = ownSnAttemptUploadBody(ctx, w, body)
			if err != nil {
				return "", fmt.Errorf("503 Upload read cancellation unavailable: %w", err)
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				w.Header().Set("Retry-After", "1")
				return "", errors.New("429 Attempt upload busy.")
			}
			if err := reserve(ctx, clientSession.ByJwt.UserId, size); err != nil {
				return "", err
			}
		}
		data, err := readSnAttemptUploadBody(ctx, body, size)
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("408 Upload canceled: %w", err)
			}
			return "", fmt.Errorf("400 Incomplete upload: %w", err)
		}
		if fmt.Sprintf("0x%x", sha256.Sum256(data)) != contentHash {
			return "", errors.New("400 Upload hash differs from body.")
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("408 Upload canceled: %w", err)
		}
		store, ok := loadStore()
		if !ok || store == nil {
			return "", errors.New("503 Attempt store unavailable.")
		}
		if err := startifact.PublishAttemptObject(ctx, store, bounds, kind, contentHash, data); err != nil {
			return "", fmt.Errorf("502 Immutable upload or readback failed: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("408 Upload canceled: %w", err)
		}
		return contentHash, nil
	}, w, r, func(contentHash string) bool {
		w.Header().Set("ETag", `"`+contentHash+`"`)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return true
	})
}

// Real HTTP/1 bodies serialize Read and Close with the same mutex. Interrupt
// native I/O before joining Close; HTTP/2 applies this deadline to one stream.
type snAttemptUploadBody struct {
	io.ReadCloser
	ctx             context.Context
	setReadDeadline func(time.Time) error
}

// Never extend or clear the server's existing read deadline. A canceled owner
// only moves it into the past, while the response writer still belongs to us.
func (self *snAttemptUploadBody) Close() error {
	var interruptErr error
	if self.ctx.Err() != nil {
		interruptErr = self.setReadDeadline(time.Now())
	}
	return errors.Join(interruptErr, self.ReadCloser.Close())
}

// Capability admission performs no I/O and cannot reset an earlier server
// deadline. Opaque, cyclic or excessively nested wrappers fail before quota.
// The production router and drain handler retain the native writer; a wrapper
// which interposes must expose the same bounded Unwrap chain.
func ownSnAttemptUploadBody(ctx context.Context, writer http.ResponseWriter, body io.ReadCloser) (io.ReadCloser, error) {
	for range 8 {
		switch value := writer.(type) {
		case interface{ SetReadDeadline(time.Time) error }:
			return &snAttemptUploadBody{ReadCloser: body, ctx: ctx, setReadDeadline: value.SetReadDeadline}, nil
		case interface{ Unwrap() http.ResponseWriter }:
			writer = value.Unwrap()
		default:
			return nil, http.ErrNotSupported
		}
	}
	return nil, errors.New("upload response writer unwrap bound exceeded")
}

// The acquired body has a native I/O interruption owner (or the independently
// closable pipe used by unit controls). Join cancellation and the actual Close
// before returning any bytes or admitting storage work.
func readSnAttemptUploadBody(ctx context.Context, body io.ReadCloser, size uint64) (data []byte, resultErr error) {
	if ctx == nil || body == nil || size == 0 || size > max(maximumSnAttemptMetadataBytes, maximumSnAttemptRecordBytes, maximumSnAttemptProofBytes) {
		return nil, errors.New("upload body owner is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	closed := make(chan struct{})
	closeErr := errors.New("upload cancellation close did not complete")
	stop := context.AfterFunc(ctx, func() { defer close(closed); closeErr = body.Close() })
	defer func() {
		if stop() {
			closeErr = body.Close()
		} else {
			<-closed
		}
		resultErr = errors.Join(resultErr, closeErr, ctx.Err())
		if resultErr != nil {
			data = nil
		}
	}()
	data, resultErr = io.ReadAll(io.LimitReader(body, int64(size)+1))
	if uint64(len(data)) != size {
		resultErr = errors.Join(resultErr, errors.New("upload body differs from Content-Length"))
	}
	return
}
