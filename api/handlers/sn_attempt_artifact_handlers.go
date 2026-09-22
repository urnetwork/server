// The public typed endpoint streams exact immutable attempt bytes. Upload
// authorization and complete validator-policy replay are separate boundaries.
package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// Transport chunks fit these independent service bounds; increasing a complete
// campaign population never increases one object or ordinary evidence limits.
const (
	maximumSnAttemptMetadataBytes = 2 * 1024 * 1024
	maximumSnAttemptRecordBytes   = model.StAttemptUploadMaximumObjectBytes
	maximumSnAttemptProofBytes    = model.StAttemptUploadMaximumObjectBytes
	maximumSnAttemptReaders       = 16
	maximumSnAttemptReadDuration  = 10 * time.Minute
)

// Refuse excess public work immediately; each admitted request uses one fixed
// 32-KiB copy buffer. The finite route deadline allows signed-row replay and
// synchronous scratch writes between client reads of a bounded data chunk.
var snAttemptReaderSlots = make(chan struct{}, maximumSnAttemptReaders)

// The same route is used by the production API and sim-testnet module runner.
func SnAttemptArtifact(w http.ResponseWriter, r *http.Request) {
	serveSnAttemptArtifact(w, r, server.LoadBlobStore, startifact.AttemptObjectBounds{
		MetadataBytes: maximumSnAttemptMetadataBytes,
		RecordBytes:   maximumSnAttemptRecordBytes,
		ProofBytes:    maximumSnAttemptProofBytes,
	}, snAttemptReaderSlots)
}

// Dependency injection is call-local; tests do not replace process-global
// stores, configuration or cancellation while other requests are running.
func serveSnAttemptArtifact(w http.ResponseWriter, r *http.Request, loadStore func() (server.BlobStore, bool), bounds startifact.AttemptObjectBounds, slots chan struct{}) {
	serveSnAttemptArtifactWithReporter(w, r, loadStore, bounds, slots, func(ctx context.Context, kind, contentHash string, count uint64, elapsed time.Duration, failure error) {
		// Quote the whole cause so joined errors remain one classifiable line.
		glog.Warningf("[sn-attempt] artifact stream failed: kind=%s hash=%s bytes=%d elapsed=%s context=%v error=%q", kind, contentHash, count, elapsed, ctx.Err(), failure.Error())
	})
}

// A request owns its reporter; cancellation tests never replace global logs.
func serveSnAttemptArtifactWithReporter(w http.ResponseWriter, r *http.Request, loadStore func() (server.BlobStore, bool), bounds startifact.AttemptObjectBounds, slots chan struct{}, report func(context.Context, string, string, uint64, time.Duration, error)) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 2 || len(query["kind"]) != 1 || len(query["hash"]) != 1 ||
		startifact.ValidateAttemptObject(query.Get("kind"), query.Get("hash")) != nil || len(r.Header.Values("Range")) != 0 {
		http.Error(w, "Invalid typed artifact request.", http.StatusBadRequest)
		return
	}
	if r.Context().Err() != nil {
		http.Error(w, "Request canceled.", http.StatusRequestTimeout)
		return
	}
	if loadStore == nil || bounds.Validate() != nil || slots == nil || cap(slots) == 0 {
		http.Error(w, "Artifact reader unavailable.", http.StatusServiceUnavailable)
		return
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Artifact reader busy.", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maximumSnAttemptReadDuration)
	defer cancel()
	if ctx.Err() != nil {
		http.Error(w, "Request canceled.", http.StatusRequestTimeout)
		return
	}
	deadline, _ := ctx.Deadline()
	// Override the API's ordinary 30-second response deadline for this bounded
	// streaming route. Non-network writers may lack this capability; an opaque
	// network wrapper then retains the server's shorter, still finite deadline.
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		http.Error(w, "Artifact write deadline unavailable.", http.StatusServiceUnavailable)
		return
	}
	store, ok := loadStore()
	if !ok || store == nil {
		http.Error(w, "Artifact store unavailable.", http.StatusServiceUnavailable)
		return
	}
	kind, contentHash := query.Get("kind"), query.Get("hash")
	contentType := "application/x-ndjson"
	if kind == "metadata" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+contentHash+`"`)
	w.Header().Set("Cache-Control", "public, immutable, max-age=31536000, no-transform")
	started := time.Now()
	count, err := startifact.ReadAttemptObjectTo(ctx, store, bounds, kind, contentHash, w)
	if err == nil {
		return
	}
	// A reader may stop an immutable download after its owning operation ends.
	// Preserve every independent deadline, storage, integrity or write failure.
	canceled := snAttemptArtifactClientCanceled(ctx, err)
	if !canceled {
		report(ctx, kind, contentHash, count, time.Since(started), err)
	}
	if count != 0 {
		// net/http must terminate the response without a successful body EOF.
		panic(http.ErrAbortHandler)
	}
	w.Header().Del("ETag")
	w.Header().Set("Cache-Control", "no-store")
	if canceled {
		http.Error(w, "Request canceled.", http.StatusRequestTimeout)
		return
	}
	http.Error(w, "Artifact unavailable or failed integrity.", http.StatusBadGateway)
}

// Cancellation is expected only when every leaf is the canceled request or
// its ordinary end-of-input. A joined close/write failure is never waived.
func snAttemptArtifactClientCanceled(ctx context.Context, failure error) bool {
	if ctx == nil || ctx.Err() != context.Canceled || failure == nil {
		return false
	}
	var canceled func(error) bool
	canceled = func(cause error) bool {
		if cause == context.Canceled || cause == io.EOF {
			return true
		}
		switch cause := cause.(type) {
		case interface{ Unwrap() []error }:
			causes := cause.Unwrap()
			if len(causes) == 0 {
				return false
			}
			for _, child := range causes {
				if !canceled(child) {
					return false
				}
			}
			return true
		case interface{ Unwrap() error }:
			return canceled(cause.Unwrap())
		default:
			return false
		}
	}
	return canceled(failure)
}
