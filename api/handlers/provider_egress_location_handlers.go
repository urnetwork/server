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

// ProviderEgressLocationSubmit ingests a probed provider egress location from
// the operator's prober. The prober routes geolocation lookups through a
// provider's own egress -- rather than relying on a lookup against the
// provider's control-connection ip -- and submits the result here so the
// server can prefer it over the built-in mmdb lookup. The route is
// operator-to-server, authenticated by the shared secret above rather than a
// network jwt.
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

// ProviderEgressLocationAttempt records that the operator's prober tried to
// probe a provider, whether or not the try produced a location.
//
// The prober reports a *failure* here; a success is reported by
// ProviderEgressLocationSubmit above, whose provider_egress_location row
// already defers the provider for the full staleness window. Reporting a
// success here as well is harmless -- the attempt backoff is far shorter than
// that window -- but redundant.
//
// This exists because ProviderEgressLocationDue would otherwise be starved by
// providers that can never be probed successfully: they never get an egress
// row, so they sort to the head of the queue on every poll forever. See
// model.GetProviderEgressLocationDue.
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
var maxProviderEgressDueLimit = sync.OnceValue(func() int {
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
})

// providerEgressDueAge is how stale a stored probe must be before its provider
// is offered up for re-probing. It is deliberately shorter than
// model.ProviderEgressLocationMaxAge -- the age past which a stored location
// stops being trusted at all. If the two were equal, every location would lapse
// to the mmdb fallback at the exact moment it became due and stay lapsed until
// the prober worked its way around to it; at half the max age the prober has a
// full max-age/2 window to refresh a location before it expires.
const providerEgressDueAge = model.ProviderEgressLocationMaxAge / 2

// ProviderEgressLocationDueResult is the response body of
// ProviderEgressLocationDue.
type ProviderEgressLocationDueResult struct {
	ClientIds []server.Id `json:"client_ids"`
}

// ProviderEgressLocationDue tells the operator's prober which providers to
// probe next: those whose egress location has gone stale, and those that have
// never been probed at all, oldest first.
//
// This moves the probe schedule from the prober's memory into the database.
// The prober used to decide what to probe from an in-memory ttl cache, so a
// restart re-probed the whole population and nothing durable recorded what was
// actually due; observed_at already carries that information server-side, and
// this exposes it.
//
// A provider is skipped if it has a fresh success *or* a recent attempt. The
// second cutoff, ProviderEgressProbeAttemptBackoff, is much shorter than the
// first: a provider that failed to probe should be retried within hours, but
// must not be handed back on every poll, which is what would starve the rest of
// the queue (see ProviderEgressLocationAttempt above).
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
	// Absent (or shard_count=1) is the single-prober case and behaves exactly as
	// before, so an existing prober needs no change.
	//
	// Without this, every worker polling inside the attempt backoff gets the
	// same rows -- the queue hands work out but never claims it -- so N workers
	// repeat one shard's work instead of dividing it. Main assigns these slices
	// to durable task rows rather than to hosts, so any taskworker can execute
	// any slice.
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
		// an out-of-range index would silently return nothing forever, which
		// looks identical to "the fleet is fully probed" -- reject it instead
		if err != nil || parsed < 0 || shardCount <= parsed {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		shardIndex = parsed
	}

	// both cutoffs are computed here and passed as arguments; observed_at and
	// attempt_at are naive timestamps holding utc, so comparing them to sql
	// now() in the query would cast through the session timezone
	now := server.NowUtc()
	minObservedAt := now.Add(-providerEgressDueAge)
	minAttemptAt := now.Add(-model.ProviderEgressProbeAttemptBackoff)

	result := &ProviderEgressLocationDueResult{
		ClientIds: model.GetProviderEgressLocationDueSharded(
			r.Context(),
			minObservedAt,
			minAttemptAt,
			limit,
			shardIndex,
			shardCount,
		),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[pegl]could not write response. err = %s\n", err)
	}
}
