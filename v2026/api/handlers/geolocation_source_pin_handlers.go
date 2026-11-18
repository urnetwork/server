package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// GeolocationSourcePin is one host's observed certificate pin as served to the
// prober: the SPKI hash of the leaf certificate and of its issuing
// intermediate, both base64 sha-256, exactly as the observation job recorded
// them from a DIRECT, WebPKI-validated connection on this server's own network.
//
// Both are served, not just the leaf, because the prober's check
// (providertunnel.checkPin) accepts a match anywhere in the verified chain: the
// intermediate is what absorbs routine leaf renewal between two observations,
// and the leaf is the tighter of the two while it lasts.
type GeolocationSourcePin struct {
	Leaf         string `json:"leaf"`
	Intermediate string `json:"intermediate"`
}

// GeolocationSourcePinsResult is the response body: a BARE map from host to its
// pin, `{"ipinfo.io": {"leaf": "...", "intermediate": "..."}}`.
//
// The other operator endpoints wrap their payload in a named field
// (`{"client_ids": [...]}`); this one deliberately does not, because the plan
// specifies this shape and because the map IS the whole answer -- there is no
// second field this response could ever grow that would not be better as its
// own endpoint. A host absent from the map has never been successfully
// observed, and the prober's correct response to that is to refuse to probe,
// so the absence has to survive the wire rather than being padded out to a
// placeholder entry here.
type GeolocationSourcePinsResult map[string]GeolocationSourcePin

// GeolocationSourcePins serves the certificate pins this server has observed
// for the geolocation source hosts, so the prober does not have to carry them
// as a compile-time constant.
//
// # Why serving pins is safe, and where the line is
//
// The geolocation lookup the prober makes is issued THROUGH the provider under
// test. The pin is what stops that provider substituting a certificate and
// forging its own apparent location, which is the entire point of the probe.
// Handing the prober a pin the SERVER chose is therefore only sound because the
// server observed it directly, on its own network, with no provider anywhere in
// the path and full chain validation (see work.RefreshGeolocationSourcePins).
// A provider cannot influence what this server saw, so it cannot influence what
// this endpoint says. Nothing in this file may ever accept a pin from a
// request: this endpoint is read-only, and the table it reads has exactly one
// writer, the observation job.
//
// # It serves what was observed, and nothing else
//
// It does not synthesize a row for a source host that has not been observed,
// and it does not fall back to any built-in default. An empty or partial answer
// is a truthful one, and the prober treats it as a hard stop rather than
// probing unpinned -- which is the whole reason the shortfall must be visible
// rather than papered over. That is also why an empty table returns `{}` with
// 200 rather than 404: 404 would be indistinguishable from "this server does
// not implement the endpoint", and the prober does distinguish those in its
// message even though both are fatal.
//
// Same auth as the operator endpoints beside it: the shared secret header
// rather than a network jwt, fail-closed when the vault resource is missing.
// The pins are not secret -- anyone can open a TLS connection to ipinfo.io and
// compute them -- but the endpoint is operator-to-server like the rest of the
// probe control plane, and there is no reason to give it a wider door than the
// due list it is fetched alongside.
func GeolocationSourcePins(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	pins := model.GetGeolocationSourcePins(r.Context())
	result := GeolocationSourcePinsResult{}
	for host, pin := range pins {
		result[host] = GeolocationSourcePin{
			Leaf:         pin.LeafSpki,
			Intermediate: pin.IntermediateSpki,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[gsp]could not write response. err = %s\n", err)
	}
}
