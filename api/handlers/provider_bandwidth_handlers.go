package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
)

// maxProviderBandwidthTestBytes bounds a single REQUEST. Without a clamp the
// endpoint is an open-ended resource commitment per request: a caller could
// ask for gigabytes and hold a connection (and the egress bytes that go with
// it) for as long as it liked.
//
// This is no longer the same thing as the per-probe figure. One probe is
// 8 parallel streams -- it has to be parallel, because a single TCP flow
// cannot exceed (connect's 1 MiB window / RTT) and the single-stream probe
// measured that ceiling rather than the provider -- so one probe is 8 requests
// of 2 MiB each, and the aggregate is bounded by the RESERVATION
// (model.MaxProviderBandwidthBytesPerProbe, 16 MiB), not by this clamp.
//
// The invariant to keep: the prober's per-stream byte count
// (bandwidth.StreamBytes, 2 MiB) must stay at or below this value. If it ever
// exceeds it, the endpoint silently truncates each stream and the probe
// transfers less than it reserved, understating every provider it measures.
const maxProviderBandwidthTestBytes = 5 * 1024 * 1024

// defaultProviderBandwidthTestBytes is used when `bytes` is absent, malformed,
// or non-positive. `bytes=0` and `bytes=-1` parse cleanly, so they are not
// caught by a malformed-input check -- and streaming an empty body for them
// would hand the prober a zero-byte sample to divide by.
const defaultProviderBandwidthTestBytes = 1024 * 1024

// maxProviderBandwidthBody bounds the request body of the two POST endpoints.
// Both carry a fixed handful of scalars.
const maxProviderBandwidthBody = 4 * 1024

// providerBandwidthTestBlock is the unit the download endpoint repeats. The
// content is irrelevant -- only the byte count is measured -- so this is one
// small shared block streamed over and over rather than a per-request
// allocation of the full byte count.
var providerBandwidthTestBlock = make([]byte, 32*1024)

// repeatingReader yields providerBandwidthTestBlock endlessly. Bounded by an
// io.LimitReader at the call site, so it never needs an end of its own.
type repeatingReader struct {
	block  []byte
	offset int
}

func (self *repeatingReader) Read(p []byte) (int, error) {
	n := copy(p, self.block[self.offset:])
	self.offset = (self.offset + n) % len(self.block)
	return n, nil
}

// authorizeOperator applies the same operator-secret check the provider egress
// location ingest endpoint uses: the shared secret from the vault
// (operatorIngestSecret, memoized and fail-closed) compared in constant time
// against the X-UR-Operator-Secret header. These are operator-to-server
// routes, not client routes -- there is no network jwt involved.
//
// An unconfigured vault leaves the secret empty, which rejects every request
// rather than accepting every request.
func authorizeOperator(r *http.Request) bool {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	return secret != "" && provided != "" && hmac.Equal([]byte(secret), []byte(provided))
}

// readOperatorRequestBody reads a bounded operator request body, writing the
// error response itself and reporting whether the caller should continue.
func readOperatorRequestBody(w http.ResponseWriter, r *http.Request, out any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderBandwidthBody+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return false
	}
	if maxProviderBandwidthBody < len(body) {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// ProviderBandwidthTest streams a bounded number of bytes. The active
// bandwidth probe needs something to download *through* a provider's tunnel to
// measure that tunnel's throughput; this is that target. It is
// operator-to-server, gated by the operator secret, so ordinary clients cannot
// use the deployment as a free speed-test target.
//
// The content is arbitrary -- only the byte count matters -- and it is streamed
// from a small repeating block through an io.LimitReader, never materialized
// in full.
func ProviderBandwidthTest(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	byteCount := int64(defaultProviderBandwidthTestBytes)
	if requested, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64); err == nil && 0 < requested {
		byteCount = requested
	}
	if maxProviderBandwidthTestBytes < byteCount {
		byteCount = maxProviderBandwidthTestBytes
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(byteCount, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	source := &repeatingReader{block: providerBandwidthTestBlock}
	if _, err := io.Copy(w, io.LimitReader(source, byteCount)); err != nil {
		// the prober hanging up mid-download is ordinary (it stops at its own
		// time or byte cap), so this is not an error worth escalating
		glog.Infof("[pbw]bandwidth test stream ended early. err = %s\n", err)
	}
}

// The submission and reservation shapes live in controller, which validates
// and stores them for this handler and for the taskworker's in-process
// operator alike.
type SubmitProviderBandwidthArgs = controller.SubmitProviderBandwidthArgs

type ReserveProviderBandwidthArgs = controller.ReserveProviderBandwidthArgs

type ReserveProviderBandwidthResult = controller.ReserveProviderBandwidthResult

// ProviderBandwidthResult stores an active bandwidth measurement
// (controller.SubmitProviderBandwidth).
func ProviderBandwidthResult(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args SubmitProviderBandwidthArgs
	if !readOperatorRequestBody(w, r, &args) {
		return
	}

	if err := controller.SubmitProviderBandwidth(r.Context(), &args); err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{}); err != nil {
		glog.Infof("[pbw]could not write response. err = %s\n", err)
	}
}

// ProviderBandwidthReserve reserves deployment-wide byte budget for one active
// bandwidth probe (controller.ReserveProviderBandwidth). Budget that exists
// only in a later hour is given back and answered 429, with Retry-After
// pointing at the bucket that does have room.
func ProviderBandwidthReserve(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args ReserveProviderBandwidthArgs
	if !readOperatorRequestBody(w, r, &args) {
		return
	}

	result, err := controller.ReserveProviderBandwidth(r.Context(), &args)
	if err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[pbw]could not write response. err = %s\n", err)
	}
}

func writeProviderBandwidthBudgetExhausted(w http.ResponseWriter, retryAfter time.Duration, message string) {
	retryAfterSeconds := int64(retryAfter.Seconds())
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	http.Error(w, message, http.StatusTooManyRequests)
}
