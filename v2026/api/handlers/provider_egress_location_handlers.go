package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	defaultProviderEgressDueLimit = 100
	// defaultMaxProviderEgressDueLimit bounds the batch size regardless of what
	// the caller asks for, so one request cannot ask the database for the
	// entire provider population.
	//
	// This is the FALLBACK. The effective value comes from
	// provider_egress_due.yml -- see maxProviderEgressDueLimit below.
	defaultMaxProviderEgressDueLimit = 500
)

// maxProviderEgressDueLimit is the ceiling on one due batch, read from
// provider_egress_due.yml.
//
// It is configuration, not a constant, for the same reason the bandwidth budget
// is (see provider_bandwidth.yml): capacity is a property of the deployment.
// The 500 fallback is sized for a beta-scale fleet. A deployment holding ~100k
// providers needs a different regime entirely -- at a 6h re-probe backoff that
// is ~16,700 probes/hour, and a 500-per-request ceiling caps the whole fleet at
// 500/hour no matter how much prober capacity exists behind it.
//
// OPTIONAL, exactly like provider_bandwidth.yml and pro.yml: a deployment
// without the file must not fail to boot, it falls back to the conservative
// default.
var maxProviderEgressDueLimit = sync.OnceValue(readMaxProviderEgressDueLimit)

// The uncached reader gives synthetic configuration tests the same parsing
// path that each API process memoizes at startup.
func readMaxProviderEgressDueLimit() int {
	resource, err := server.Config.SimpleResource("provider_egress_due.yml")
	if err != nil {
		glog.Infof(
			"[pegl]provider_egress_due.yml not present; using the default due limit of %d\n",
			defaultMaxProviderEgressDueLimit,
		)
		return defaultMaxProviderEgressDueLimit
	}
	var y struct {
		MaxDueLimit int `yaml:"max_due_limit"`
	}
	resource.UnmarshalYaml(&y)
	if y.MaxDueLimit <= 0 {
		glog.Errorf(
			"[pegl]provider_egress_due.yml has max_due_limit=%d, which is not usable; using the default %d\n",
			y.MaxDueLimit,
			defaultMaxProviderEgressDueLimit,
		)
		return defaultMaxProviderEgressDueLimit
	}
	glog.Infof("[pegl]max due limit: %d from provider_egress_due.yml\n", y.MaxDueLimit)
	return y.MaxDueLimit
}

// providerEgressDueAge remains the legacy location-refresh cutoff for retained
// diagnostic callers. ProviderEgressLocationDue does not use location age: its
// URL quota and pacing are independent of the location's trust lifetime.
const providerEgressDueAge = model.ProviderEgressLocationMaxAge / 2

// One provider of a due list, with the place it is published under: the prober
// draws a provider's sample only from the destinations compatible with that
// place, so a site blocked in a country never counts against that country's
// exits (connect/GEOMAP.md §11.3). Both place fields are empty for a provider
// the reliability rollup has not placed, which excludes nothing.
type ProviderEgressDueProvider struct {
	ClientId server.Id `json:"client_id"`
	// CountryCode is lowercase alpha-2.
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
}

// ProviderEgressLocationDueResult is the retained diagnostic due-list shape.
// The URL due route additionally returns the token, quota deficit and ordinal
// through model.ProviderUrlProbeDue; this shape can read only its place subset.
type ProviderEgressLocationDueResult struct {
	Providers []ProviderEgressDueProvider `json:"providers"`
}

// Joins a due list, in its order, to the places its providers are published
// under.
func providerEgressDueProviders(r *http.Request, clientIds []server.Id) []ProviderEgressDueProvider {
	places := model.GetProviderEgressPlaces(r.Context(), clientIds)
	providers := make([]ProviderEgressDueProvider, 0, len(clientIds))
	for _, clientId := range clientIds {
		place := places[clientId]
		providers = append(providers, ProviderEgressDueProvider{
			ClientId:    clientId,
			CountryCode: place.CountryCode,
			Region:      place.Region,
		})
	}
	return providers
}

// ProviderEgressLocationDue retains its operator route while atomically
// claiming paced URL turns. It returns the durable cycle token, remaining
// success target and accepted outcome ordinal along with each provider place.
// Reliability and ARIN risk determine eligibility; no independent cheap probe
// gates admission. Unacknowledged claims expire without changing evidence.
//
// Same auth as ProviderEgressLocationSubmit above: operator-to-server, the
// shared secret header rather than a network jwt, fail-closed when the vault
// resource is missing.
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
		if err != nil || parsed < 1 {
			// not clamped up to 1: `limit=0` would come back as an empty list,
			// which the prober cannot distinguish from "nothing is due"
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		limit = min(parsed, maxProviderEgressDueLimit())
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
		// an out-of-range index would silently return nothing forever, which
		// looks identical to "the fleet is fully probed" -- reject it instead
		if err != nil || parsed < 0 || shardCount <= parsed {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		shardIndex = parsed
	}

	respondProviderUrlProbeDue(w, r, limit, shardIndex, shardCount,
		model.ClaimProviderUrlProbeDueWithObservation, providerUrlProbeDueTiming)
}
