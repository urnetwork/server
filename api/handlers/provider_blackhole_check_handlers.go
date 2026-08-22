package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	// defaultProviderBlackholeDueLimit is a batch when the caller names none.
	// Larger than the egress-location default because a blackhole check is a
	// single request through an already-required tunnel rather than a ~131
	// destination sweep -- the whole point is that a batch of these is cheap.
	defaultProviderBlackholeDueLimit = 500
	// maxProviderBlackholeDueLimit caps one batch. The sweep is meant to cover
	// the fleet hourly, so this is sized to let a deployment do that in a few
	// requests rather than hundreds.
	maxProviderBlackholeDueLimit = 5000
)

// ProviderBlackholeCheckDueResult is the response body of
// ProviderBlackholeCheckDue.
type ProviderBlackholeCheckDueResult struct {
	ClientIds []server.Id `json:"client_ids"`
}

// ProviderBlackholeCheckDue serves the sweep: which providers to check next,
// never-checked first, then least recently checked.
//
// Authenticated with the same operator secret as the egress-location
// endpoints -- one secret, one mechanism, one thing for a deployment to get
// right.
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
		if err != nil || parsed < 1 {
			// not clamped up to 1: limit=0 returns an empty list, which the
			// caller cannot tell apart from "nothing is due"
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		limit = min(parsed, maxProviderBlackholeDueLimit)
	}

	shardCount := 1
	shardIndex := 0
	if raw := r.URL.Query().Get("shard_count"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		shardCount = parsed
	}
	if raw := r.URL.Query().Get("shard_index"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		// an out-of-range index returns nothing forever, which looks exactly
		// like "the fleet is fully checked" -- reject it instead
		if err != nil || parsed < 0 || shardCount <= parsed {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		shardIndex = parsed
	}

	// computed here and passed in: checked_at is a naive timestamp holding utc,
	// so comparing it to sql now() in the query would cast through the session
	// timezone
	minCheckedAt := server.NowUtc().Add(-model.ProviderBlackholeCheckDueAge)

	result := &ProviderBlackholeCheckDueResult{
		ClientIds: model.GetProviderBlackholeCheckDue(
			r.Context(),
			minCheckedAt,
			limit,
			shardIndex,
			shardCount,
		),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// SubmitProviderBlackholeCheckArgs is one provider's result. Kept to the single
// question the check answers: did anything get through.
type SubmitProviderBlackholeCheckArgs struct {
	ClientId server.Id `json:"client_id"`
	OK       bool      `json:"ok"`
	// Failure is a short class when OK is false: tunnel_failed,
	// all_destinations_failed, and so on. Ignored when OK.
	Failure   string    `json:"failure,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// SubmitProviderBlackholeChecks accepts a BATCH of results.
//
// Batched, unlike the egress-location submission, because the sweep produces
// hundreds of one-bit answers per pass and a request each would spend more on
// http than on the checks themselves.
type SubmitProviderBlackholeChecksArgs struct {
	Checks []SubmitProviderBlackholeCheckArgs `json:"checks"`
}

// maxProviderBlackholeChecksPerRequest bounds one batch, so a malformed or
// hostile body cannot make the server hold an unbounded slice. It is above
// maxProviderBlackholeDueLimit so a caller can always report a full batch it
// was legitimately handed.
const maxProviderBlackholeChecksPerRequest = 10000

func SubmitProviderBlackholeChecks(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args SubmitProviderBlackholeChecksArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(args.Checks) == 0 {
		http.Error(w, "Missing checks.", http.StatusBadRequest)
		return
	}
	if maxProviderBlackholeChecksPerRequest < len(args.Checks) {
		http.Error(w, "Too many checks.", http.StatusBadRequest)
		return
	}

	now := server.NowUtc()

	// Validate the WHOLE batch before writing any of it. A partial write would
	// leave the caller unable to tell which results landed, and it reports
	// fire-and-forget.
	for _, check := range args.Checks {
		if check.ClientId == (server.Id{}) {
			http.Error(w, "Missing client_id.", http.StatusBadRequest)
			return
		}
		if check.CheckedAt.IsZero() {
			// never fabricated here: an "as of now" timestamp would defeat the
			// freshness bound the gate depends on, and could pin a stale answer
			http.Error(w, "Missing checked_at.", http.StatusBadRequest)
			return
		}
		// a future-dated check would outlive every real one under the monotonic
		// upsert and could never be corrected
		if now.Add(time.Minute).Before(check.CheckedAt) {
			http.Error(w, "checked_at is in the future.", http.StatusBadRequest)
			return
		}
		if model.MaxProviderBlackholeFailureLen < len(check.Failure) {
			http.Error(w, "Failure class too long.", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(check.Failure) == "" && !check.OK {
			http.Error(w, "A failed check must name a failure class.", http.StatusBadRequest)
			return
		}
	}

	for _, check := range args.Checks {
		model.SetProviderBlackholeCheck(r.Context(), &model.ProviderBlackholeCheck{
			ClientId:  check.ClientId,
			CheckedAt: check.CheckedAt,
			OK:        check.OK,
			Failure:   check.Failure,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct{}{})
}
