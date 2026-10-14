package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/urnetwork/server/controller"
)

const (
	// defaultProviderBlackholeDueLimit is a batch when the caller names none.
	defaultProviderBlackholeDueLimit = controller.DefaultProviderBlackholeDueLimit
	// maxProviderBlackholeDueLimit caps one batch.
	maxProviderBlackholeDueLimit = controller.MaxProviderBlackholeDueLimit
)

// ProviderBlackholeCheckDue serves the sweep: which providers to check next,
// each with the place it is published under (controller.
// ProviderBlackholeCheckDue). The body is ProviderEgressLocationDueResult, the
// full probe's shape.
//
// Authenticated with the same operator secret as the egress-location
// endpoints -- one secret, one mechanism, one thing for a deployment to get
// right. The limit and shard rules are controller's, shared with the
// taskworker's in-process operator.
func ProviderBlackholeCheckDue(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	limit := defaultProviderBlackholeDueLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		limit = parsed
	}

	shardCount := 1
	shardIndex := 0
	if raw := r.URL.Query().Get("shard_count"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		shardCount = parsed
	}
	if raw := r.URL.Query().Get("shard_index"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		shardIndex = parsed
	}

	result, err := controller.ProviderBlackholeCheckDue(r.Context(), limit, shardIndex, shardCount)
	if err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// The batch shapes live in controller, which the in-process operator shares.
type SubmitProviderBlackholeCheckArgs = controller.SubmitProviderBlackholeCheckArgs

type SubmitProviderBlackholeChecksArgs = controller.SubmitProviderBlackholeChecksArgs

// maxProviderBlackholeChecksPerRequest bounds one batch (see controller).
const maxProviderBlackholeChecksPerRequest = controller.MaxProviderBlackholeChecksPerRequest

// maxProviderBlackholeChecksBody bounds the body read: a full batch of checks
// at about 150 bytes each, with room.
const maxProviderBlackholeChecksBody = 4 * 1024 * 1024

// SubmitProviderBlackholeChecks accepts a batch of results. controller.
// SubmitProviderBlackholeChecks validates the whole batch before writing any
// of it.
func SubmitProviderBlackholeChecks(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// strict: a field this server does not know decodes to nothing, and a
	// check whose not_measured flag was dropped would be stored as a failure
	// counted against the provider
	var args SubmitProviderBlackholeChecksArgs
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxProviderBlackholeChecksBody+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if err := controller.SubmitProviderBlackholeChecks(r.Context(), &args); err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct{}{})
}
