package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// operatorSecretHeader carries the operator ingest secret. This endpoint is
// operator-to-server, not a client route: it is authenticated by a shared
// secret from the vault, not by a network jwt.
const operatorSecretHeader = "X-UR-Operator-Secret"

// maxProviderEgressLocationBody bounds the request body.
const maxProviderEgressLocationBody = 16 * 1024

// operatorIngestSecret memoizes readOperatorIngestSecret for the life of the
// process. It is a package-level var (not a plain sync.OnceValue call site)
// so tests can swap it for a stub and restore it with defer; production code
// never reassigns it.
var operatorIngestSecret func() string = sync.OnceValue(readOperatorIngestSecret)

// readOperatorIngestSecret reads the operator ingest secret from the vault
// resource "provider_egress.yml", key "ingest_secret". It returns ""
// when the vault resource is absent, the key is absent, or the key is empty,
// which makes the endpoint fail closed (every request is rejected) rather
// than open. `SimpleResource`/`String` are the non-panicking lookups (unlike
// `RequireSimpleResource`/`RequireString`), so a missing vault resource
// disables the endpoint instead of panicking the api process at startup or
// per-request.
func readOperatorIngestSecret() string {
	res, err := server.Vault.SimpleResource("provider_egress.yml")
	if err != nil {
		glog.Infof("[pegl]no provider_egress.yml in the vault; ingest endpoint disabled\n")
		return ""
	}
	values := res.String("ingest_secret")
	if len(values) != 1 || values[0] == "" {
		glog.Infof("[pegl]no ingest_secret in provider_egress.yml; ingest endpoint disabled\n")
		return ""
	}
	return values[0]
}

// ProviderEgressLocationSubmit ingests where a provider's traffic exits: the
// address the operator's own /ip echo saw through the provider's tunnel, which
// the server places with its own GeoLite2 (connect/GEOMAP.md §11.3) and
// prefers over the lookup on the provider's control-connection ip. The route
// is operator-to-server, authenticated by the shared secret above rather than
// a network jwt.
func ProviderEgressLocationSubmit(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderEgressLocationBody+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxProviderEgressLocationBody {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}

	var args controller.SubmitProviderEgressLocationArgs
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	result, err := controller.SubmitProviderEgressLocation(r.Context(), &args)
	if err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[pegl]could not write response. err = %s\n", err)
	}
}

// ProviderEgressLocationAttempt retains attempt diagnostics and reports local
// setup/publication failures. The model releases an incomplete URL claim into
// its bounded failure retry without inventing a measured URL error or success.
// A stored location or legacy aggregate result does not satisfy the URL quota;
// the due route uses its durable deadline and rolling selected-policy history.
//
// Same auth as the two endpoints around it: operator-to-server, the shared
// secret header rather than a network jwt, fail-closed when the vault resource
// is missing.
func ProviderEgressLocationAttempt(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderEgressLocationBody+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxProviderEgressLocationBody {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}

	var args controller.RecordProviderEgressProbeAttemptArgs
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	result, err := controller.RecordProviderEgressProbeAttempt(r.Context(), &args)
	if err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[pegl]could not write response. err = %s\n", err)
	}
}

const (
	// defaultProviderEgressDueLimit is the batch size when the caller does not
	// ask for one.
	defaultProviderEgressDueLimit = controller.DefaultProviderEgressDueLimit
	// defaultMaxProviderEgressDueLimit bounds the batch size regardless of what
	// the caller asks for. This is the fallback; the effective value comes from
	// provider_egress_due.yml -- see controller.ProviderEgressDueLimitCeiling.
	defaultMaxProviderEgressDueLimit = controller.DefaultMaxProviderEgressDueLimit
)

// The uncached reader of provider_egress_due.yml's max_due_limit; each process
// memoizes it once in controller.ProviderEgressDueLimitCeiling, which the
// taskworker's in-process operator shares.
func readMaxProviderEgressDueLimit() int {
	return controller.ReadMaxProviderEgressDueLimit()
}

// providerEgressDueAge remains the legacy location-refresh cutoff for retained
// diagnostic callers. ProviderEgressLocationDue does not use location age: its
// URL quota and pacing are independent of the location's trust lifetime.
const providerEgressDueAge = model.ProviderEgressLocationMaxAge / 2

// The place-bearing due list shapes live in controller, which the blackhole
// due route and the in-process operator share.
type ProviderEgressDueProvider = controller.ProviderEgressDueProvider

type ProviderEgressLocationDueResult = controller.ProviderEgressLocationDueResult

// ProviderEgressLocationDue retains its operator route while atomically
// claiming paced URL turns. It returns the durable cycle token, remaining
// success target and accepted outcome ordinal along with each provider place.
// Reliability and ARIN risk determine eligibility; no independent cheap probe
// gates admission. Unacknowledged claims expire without changing evidence.
//
// Same auth as ProviderEgressLocationSubmit above: operator-to-server, the
// shared secret header rather than a network jwt, fail-closed when the vault
// resource is missing. The limit and shard rules are controller's, shared with
// the taskworker's in-process operator.
func ProviderEgressLocationDue(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	limit := defaultProviderEgressDueLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		limit, err = controller.ProviderUrlProbeDueLimit(parsed)
		if err != nil {
			writeProviderEgressIngestError(w, err)
			return
		}
	}

	// shard_index / shard_count partition the queue across independent workers.
	// Absent (or shard_count=1) selects the single-shard URL queue. Retaining
	// the route name does not make legacy aggregate results satisfy its quota.
	//
	// Durable task rows own these slices rather than particular hosts. Atomic
	// row claims also exclude duplicate work while an earlier turn is active.
	shardCount := 1
	shardIndex := 0
	if raw := r.URL.Query().Get("shard_count"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || model.ProviderUrlProbeSlotCount < parsed {
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
	if err := controller.ValidateProviderUrlProbeDueShard(shardIndex, shardCount); err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	respondProviderUrlProbeDue(w, r, limit, shardIndex, shardCount,
		model.ClaimProviderUrlProbeDueWithObservation, providerUrlProbeDueTiming)
}

// Writes a controller refusal exactly as the handlers always wrote it: its
// status and message, and Retry-After for an exhausted bandwidth budget. Any
// other error is the 400 these routes answer a controller failure with, with
// a fixed message; its own text stays in the log.
func writeProviderEgressIngestError(w http.ResponseWriter, err error) {
	if refusal := controller.ProviderEgressIngestRefusalOf(err); refusal != nil {
		if refusal.Status == http.StatusTooManyRequests {
			writeProviderBandwidthBudgetExhausted(w, refusal.RetryAfter, refusal.Message)
			return
		}
		http.Error(w, refusal.Message, refusal.Status)
		return
	}
	glog.Infof("[pegl]ingest error: %s\n", err)
	http.Error(w, controller.ProviderEgressIngestFailedMessage, http.StatusBadRequest)
}
