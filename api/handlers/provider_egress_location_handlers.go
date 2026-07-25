package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
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
// (beta-vault/vault/provider_egress.yml, key "ingest_secret"). It returns ""
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
// the operator's prober. See
// docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md.
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
