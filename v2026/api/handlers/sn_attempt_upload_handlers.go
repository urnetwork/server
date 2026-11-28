// Authenticated clients may stage exact typed bytes, not approve validator
// evidence. The actual public reader and both-origin replay remain mandatory.
//
// A body may travel as zstd, with the object's own length in
// X-Decoded-Content-Length. Every bound, the reserved consent, the budgets and
// the hash apply to the decoded object; the wire bytes may not exceed it, and
// decoding stops at its declared length. Acknowledgements, refusals of an
// encoding and object heads advertise zstd with Accept-Encoding (RFC 7694), so
// an uploader sends it only to an api that takes it. Validators uploading over
// a lossy path (2026-11-16) send a fraction of each records object's bytes.
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
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

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

// The request header that carries an encoded body's decoded object length.
const snAttemptUploadDecodedLengthHeader = "X-Decoded-Content-Length"

// The one request body encoding taken, as Accept-Encoding advertises it.
const snAttemptUploadEncodingZstd = "zstd"

// The answer to any other encoding, which carries the advertisement.
var errSnAttemptUploadEncodingUnsupported = errors.New("415 Upload encoding is unsupported.")

// Tunables of encoded upload bodies and of an object's head.
type snAttemptEncodingSettings struct {
	// The largest zstd window a body may declare. Twice the largest object, so
	// any encoder's choice for one object fits and a hostile frame cannot make
	// the decoder reserve more.
	ZstdWindowBytes uint64
	// The fewest decoded bytes a decoder may hold, for an object smaller than
	// zstd's smallest window, which the decoder bounds by its memory.
	ZstdMemoryFloorBytes uint64
	// How long one head may list its object's address; it never streams.
	HeadDuration time.Duration
}

// The route's values, a new copy on every call.
func defaultSnAttemptEncodingSettings() snAttemptEncodingSettings {
	return snAttemptEncodingSettings{
		ZstdWindowBytes:      8 * 1024 * 1024,
		ZstdMemoryFloorBytes: 64 * 1024,
		HeadDuration:         10 * time.Second,
	}
}

// Production and sim-testnet use this same route and real client-auth wrapper.
func SnUploadAttemptArtifact(w http.ResponseWriter, r *http.Request) {
	serveSnUploadAttemptArtifact(w, r, server.LoadBlobStore, controller.StReserveAttemptUpload,
		startifact.AttemptObjectBounds{MetadataBytes: maximumSnAttemptMetadataBytes, RecordBytes: maximumSnAttemptRecordBytes, ProofBytes: maximumSnAttemptProofBytes}, snAttemptUploadSlots)
}

// The API lifecycle's reserved upload lane as the route sees it, asked on each
// request because a lane whose startup failed can come up later in the process.
type SnReservedAttemptUploadLane interface {
	// The running admission, or nil. A nil admission with starting set is a
	// configured lane that is not running on this process. With starting clear,
	// no configuration asks this process for a lane.
	Admission() (admission *controller.StReservedAttemptUpload, starting bool)
}

// The production router captures the one API-owned admission lifecycle. A
// missing cache never turns a supplied reserved header into ordinary traffic.
func SnUploadAttemptArtifactWithReserved(reserved SnReservedAttemptUploadLane) http.HandlerFunc {
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
// Every refusal names only its declared answer: a cause, which can quote a
// socket, a temp path, a store endpoint or a Redis script, stays in the log
// (router.PrivateCause), and an undeclared failure is a 503 (PrivateErrors).
func serveSnAttemptUpload(w http.ResponseWriter, r *http.Request, loadStore func() (server.BlobStore, bool), reserve func(context.Context, server.Id, uint64) error, bounds startifact.AttemptObjectBounds, slots chan struct{}, reservedLane SnReservedAttemptUploadLane) {
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
	router.WrapRequireClient(router.PrivateErrors(func(clientSession *session.ClientSession) (string, error) {
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
		if err != nil || mediaType != contentType || len(parameters) != 0 || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Range")) != 0 || len(r.Header.Values("Range")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
			return "", errors.New("400 Transformed or partial upload is forbidden.")
		}
		// the object's own length, and its bytes on the wire
		size, wireSize, encoded, err := snAttemptUploadLengths(r, limit)
		if err != nil {
			if err == errSnAttemptUploadEncodingUnsupported {
				w.Header().Set("Accept-Encoding", snAttemptUploadEncodingZstd)
			}
			return "", err
		}
		body := r.Body
		if err := ctx.Err(); err != nil {
			return "", router.PrivateCause(http.StatusRequestTimeout, "Upload canceled.", err)
		}
		reservedHeaders := r.Header.Values(protocol.ValidatorAttemptUploadHeader)
		if len(reservedHeaders) != 0 {
			if len(reservedHeaders) != 1 || reservedHeaders[0] == "" {
				return "", errors.New("403 Validator reserved upload is unavailable or ambiguous.")
			}
			var reserved *controller.StReservedAttemptUpload
			laneStarting := false
			if reservedLane != nil {
				reserved, laneStarting = reservedLane.Admission()
			}
			// A lane that is not running here says nothing about the validator. A
			// 503 lets the lb try a sibling block (warp proxy_next_upstream), and
			// the validator retries it without spending its failure budget
			// (sn validator classifyReleaseRetryBounded). It counted a 403 as an
			// authorization refusal, toward the panic that restarts it.
			if reserved == nil && laneStarting {
				return "", errors.New("503 Validator reserved upload is not running on this instance.")
			}
			if reserved == nil {
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
				return "", router.PrivateCause(http.StatusServiceUnavailable, "Upload read cancellation unavailable.", err)
			}
			if err := reserved.Reserve(lease); err != nil {
				return "", err
			}
		} else {
			body, err = ownSnAttemptUploadBody(ctx, w, body)
			if err != nil {
				return "", router.PrivateCause(http.StatusServiceUnavailable, "Upload read cancellation unavailable.", err)
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
		data, err := readSnAttemptUploadBody(ctx, body, wireSize)
		if err != nil {
			if ctx.Err() != nil {
				return "", router.PrivateCause(http.StatusRequestTimeout, "Upload canceled.", err)
			}
			return "", router.PrivateCause(http.StatusBadRequest, "Incomplete upload.", err)
		}
		if encoded {
			data, err = decodeSnAttemptUploadBody(data, size)
			if err != nil {
				return "", router.PrivateCause(http.StatusBadRequest, "Upload encoding is invalid or differs from its declared length.", err)
			}
		}
		if fmt.Sprintf("0x%x", sha256.Sum256(data)) != contentHash {
			return "", errors.New("400 Upload hash differs from body.")
		}
		if err := ctx.Err(); err != nil {
			return "", router.PrivateCause(http.StatusRequestTimeout, "Upload canceled.", err)
		}
		store, ok := loadStore()
		if !ok || store == nil {
			return "", errors.New("503 Attempt store unavailable.")
		}
		if err := startifact.PublishAttemptObject(ctx, store, bounds, kind, contentHash, data); err != nil {
			return "", router.PrivateCause(http.StatusBadGateway, "Immutable upload or readback failed.", err)
		}
		if err := ctx.Err(); err != nil {
			return "", router.PrivateCause(http.StatusRequestTimeout, "Upload canceled.", err)
		}
		return contentHash, nil
	}), w, r, func(_ *session.ClientSession, contentHash string) bool {
		w.Header().Set("ETag", `"`+contentHash+`"`)
		w.Header().Set("Accept-Encoding", snAttemptUploadEncodingZstd)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return true
	})
}

// The object's own length and the body's length on the wire, and whether the
// body is zstd. Without an encoding they are one positive bounded
// Content-Length. With zstd the object's length comes from
// X-Decoded-Content-Length, written exactly as a positive decimal within the
// type's bound, and the wire may not carry more than that. Any other encoding
// is answered 415.
func snAttemptUploadLengths(r *http.Request, limit uint64) (size uint64, wireSize uint64, encoded bool, err error) {
	if r.Body == nil || r.ContentLength <= 0 {
		return 0, 0, false, errors.New("413 Upload requires a positive bounded Content-Length.")
	}
	wireSize = uint64(r.ContentLength)
	encodings := r.Header.Values("Content-Encoding")
	declared := r.Header.Values(snAttemptUploadDecodedLengthHeader)
	if len(encodings) == 0 {
		if len(declared) != 0 {
			return 0, 0, false, errors.New("400 Decoded length requires an encoded upload.")
		}
		if limit < wireSize {
			return 0, 0, false, errors.New("413 Upload requires a positive bounded Content-Length.")
		}
		return wireSize, wireSize, false, nil
	}
	if len(encodings) != 1 || !strings.EqualFold(strings.TrimSpace(encodings[0]), snAttemptUploadEncodingZstd) {
		return 0, 0, false, errSnAttemptUploadEncodingUnsupported
	}
	if len(declared) != 1 {
		return 0, 0, false, errors.New("400 Encoded upload requires its decoded length.")
	}
	size, parseErr := strconv.ParseUint(declared[0], 10, 64)
	if parseErr != nil || size == 0 || strconv.FormatUint(size, 10) != declared[0] {
		return 0, 0, false, errors.New("400 Encoded upload requires its decoded length.")
	}
	if limit < size {
		return 0, 0, false, errors.New("413 Upload exceeds its typed byte bound.")
	}
	if size < wireSize {
		return 0, 0, false, errors.New("400 Encoded upload is larger than its object.")
	}
	return size, wireSize, true, nil
}

// The object a zstd body decodes to, which must be exactly size bytes. The
// decoder holds no more than size decoded bytes, or the small floor zstd's
// smallest window needs, and its window is bounded too, so a hostile frame
// fails instead of growing. A frame whose window exceeds its object (one an
// encoder wrote in segments) is refused; the uploader then sends the object
// as it is. A single-segment frame, as an encoder writes for one whole object,
// has a window of the object's own length.
func decodeSnAttemptUploadBody(wire []byte, size uint64) ([]byte, error) {
	if size == 0 || max(maximumSnAttemptMetadataBytes, maximumSnAttemptRecordBytes, maximumSnAttemptProofBytes) < size {
		return nil, errors.New("decoded upload length is outside every typed bound")
	}
	settings := defaultSnAttemptEncodingSettings()
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(max(size, settings.ZstdMemoryFloorBytes)), zstd.WithDecoderMaxWindow(settings.ZstdWindowBytes))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	data, err := decoder.DecodeAll(wire, make([]byte, 0, size))
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != size {
		return nil, errors.New("decoded upload differs from its declared length")
	}
	return data, nil
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
