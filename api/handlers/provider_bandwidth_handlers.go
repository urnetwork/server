package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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

// SubmitProviderBandwidthArgs is an active bandwidth measurement taken by the
// prober over a provider's tunnel.
type SubmitProviderBandwidthArgs struct {
	ClientId server.Id `json:"client_id"`
	// Source names which target produced this figure
	// (model.ProviderBandwidthSourceActiveOperator or ...ActiveCDN). It is
	// part of the storage key, so it is what keeps the two targets' figures in
	// separate rows instead of overwriting each other.
	Source          string  `json:"source"`
	BytesPerSecond  float64 `json:"bytes_per_second"`
	SampleByteCount int64   `json:"sample_byte_count"`
}

// ProviderBandwidthResult stores an active bandwidth measurement. An active
// probe is a point measurement rather than an aggregate over a window, so
// window_start and window_end are both the arrival time.
//
// A non-positive rate or sample size is not a usable measurement, and storing
// one would overwrite a real figure with a meaningless one -- so those are
// rejected before anything is written.
//
// The source is validated against the known ACTIVE set rather than merely
// stored. Two distinct failures are being closed off. An unrecognised source
// is not a harmless label: the row is keyed on (client_id, source), so it
// creates a row nothing will ever read or replace, and a prober with a typo'd
// tag would look like it was working while writing to a tag no consumer knows.
// And "passive" is refused specifically: that figure is derived server-side
// from bytes the provider has already been paid to carry, which is exactly
// what makes it ungameable, so accepting a submitted one would let this
// endpoint overwrite the derived figure with an asserted one.
func ProviderBandwidthResult(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args SubmitProviderBandwidthArgs
	if !readOperatorRequestBody(w, r, &args) {
		return
	}

	if args.ClientId == (server.Id{}) {
		http.Error(w, "Missing client id.", http.StatusBadRequest)
		return
	}
	if args.BytesPerSecond <= 0 {
		http.Error(w, "bytes_per_second must be positive.", http.StatusBadRequest)
		return
	}
	if args.SampleByteCount <= 0 {
		http.Error(w, "sample_byte_count must be positive.", http.StatusBadRequest)
		return
	}
	if !model.IsSubmittableProviderBandwidthSource(args.Source) {
		http.Error(w, fmt.Sprintf(
			"source must be one of %s, %s.",
			model.ProviderBandwidthSourceActiveOperator,
			model.ProviderBandwidthSourceActiveCDN,
		), http.StatusBadRequest)
		return
	}

	now := server.NowUtc()
	model.StoreProviderBandwidth(r.Context(), &model.ProviderBandwidth{
		ClientId:        args.ClientId,
		BytesPerSecond:  args.BytesPerSecond,
		Source:          args.Source,
		SampleByteCount: args.SampleByteCount,
		WindowStart:     now,
		WindowEnd:       now,
	})

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{}); err != nil {
		glog.Infof("[pbw]could not write response. err = %s\n", err)
	}
}

// ReserveProviderBandwidthArgs asks for budget to run one active probe.
type ReserveProviderBandwidthArgs struct {
	ClientId  server.Id `json:"client_id"`
	ByteCount int64     `json:"byte_count"`
}

// ReserveProviderBandwidthResult carries the reservation the prober just took.
// BucketStart is always the current hourly bucket (see ProviderBandwidthReserve).
type ReserveProviderBandwidthResult struct {
	ReservationId server.Id `json:"reservation_id"`
	BucketStart   time.Time `json:"bucket_start"`
}

// ProviderBandwidthReserve reserves deployment-wide byte budget for one active
// bandwidth probe. Active probing pulls real data through a provider's tunnel,
// which is real paid contract traffic, so it is rationed
// (model.ReserveProviderBandwidthSlot).
//
// The prober measures over a tunnel it already has open, right now: it has no
// use for budget in a later hour. model.ReserveProviderBandwidthSlot will
// happily defer a reservation into a future bucket for callers that can
// schedule a RunAt, so when it does that here the reservation is cancelled
// again and the request answered 429 -- the hourly ceiling would otherwise be
// decorative, since the prober could spend the whole daily budget inside one
// hour. Retry-After points at the bucket that does have room. (The plan
// specifies 429 "when every lookahead bucket is full"; this returns 429 on a
// strict superset of that, for the same "skip this provider cleanly" reason.)
func ProviderBandwidthReserve(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args ReserveProviderBandwidthArgs
	if !readOperatorRequestBody(w, r, &args) {
		return
	}

	if args.ClientId == (server.Id{}) {
		http.Error(w, "Missing client id.", http.StatusBadRequest)
		return
	}
	if args.ByteCount <= 0 {
		http.Error(w, "byte_count must be positive.", http.StatusBadRequest)
		return
	}
	// the byte count is caller-supplied; a probe never legitimately needs more
	// than the per-probe figure, and an oversized request must not be able to
	// swallow a large slice of a bucket in one reservation
	byteCount := args.ByteCount
	if model.MaxProviderBandwidthBytesPerProbe < byteCount {
		byteCount = model.MaxProviderBandwidthBytesPerProbe
	}

	ctx := r.Context()
	now := server.NowUtc()
	currentBucketStart := now.UTC().Truncate(model.ProviderBandwidthBucketDuration)

	reservationId, bucketStart, err := model.ReserveProviderBandwidthSlot(ctx, args.ClientId, byteCount)
	if err != nil {
		// every bucket in the lookahead window is full: the deployment's daily
		// budget is exhausted
		writeProviderBandwidthBudgetExhausted(w, currentBucketStart.Add(model.ProviderBandwidthBucketDuration).Sub(now), err.Error())
		return
	}
	if bucketStart.After(currentBucketStart) {
		// budget exists, but not until a later hour -- of no use to a probe
		// that runs now, so give it back rather than burning it on a request
		// that is about to be skipped
		model.CancelProviderBandwidthReservation(ctx, reservationId)
		writeProviderBandwidthBudgetExhausted(w, bucketStart.Sub(now), "The active bandwidth probe budget for this hour has been reached.")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	result := &ReserveProviderBandwidthResult{
		ReservationId: reservationId,
		BucketStart:   bucketStart,
	}
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
