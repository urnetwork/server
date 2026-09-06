// The public typed endpoint streams exact immutable attempt bytes. Upload
// authorization and complete validator-policy replay are separate boundaries.
package handlers

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// Transport chunks fit these independent service bounds; increasing a complete
// campaign population never increases one object or ordinary evidence limits.
const (
	maximumSnAttemptMetadataBytes = 2 * 1024 * 1024
	maximumSnAttemptRecordBytes   = 32 * 1024 * 1024
	maximumSnAttemptProofBytes    = 32 * 1024 * 1024
	maximumSnAttemptReaders       = 16
)

// Refuse excess public work immediately; each admitted request uses one fixed
// 32-KiB copy buffer, with a deadline shorter than the API's write deadline.
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
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		http.Error(w, "Request canceled.", http.StatusRequestTimeout)
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
	count, err := startifact.ReadAttemptObjectTo(ctx, store, bounds, kind, contentHash, w)
	if err == nil {
		return
	}
	if count != 0 {
		// net/http must terminate the response without a successful body EOF.
		panic(http.ErrAbortHandler)
	}
	w.Header().Del("ETag")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "Artifact unavailable or failed integrity.", http.StatusBadGateway)
}
