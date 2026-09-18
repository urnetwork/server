package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// ProberCredentialResult is the response body of ProberCredential.
//
// It is NARROWER than the prober_identity row behind it. Exactly two things
// leave this server:
//
//   - by_client_jwt, the *delivery* credential. It is revocable and re-mintable:
//     model.clearProberIdentityClient drops it and the bootstrap task mints
//     another against the same network. A leaked one is recovered by forcing a
//     re-mint, and costs nothing else.
//   - client_id, which names the client the jwt already names. It discloses
//     nothing the jwt does not, and the prober needs it to identify itself in
//     the routes it goes on to call.
//
// Do NOT read the omission of network_id, user_id and network_name as a
// security measure. An earlier version of this comment claimed they were left
// out ON PURPOSE, as a "root identity" that a holder could otherwise re-derive
// the account from. That claim was false. ByJwt.Client copies all three into
// the very token this endpoint hands out (jwt/by_jwt.go:658-662), and the
// struct tags serialise them as network_id/user_id/network_name
// (jwt/by_jwt.go:188-190), so anyone holding this response can base64-decode
// the token's payload segment and read them. Keeping them out of the JSON body
// hides nothing from the only party that ever sees it.
//
// The ids are inert in any case. /auth/regenerate-seedphrase takes an EMPTY
// args struct (controller/seedphrase_controller.go:8-9) and keys on
// session.ByJwt.UserId (:36 and :62) -- the id from the authenticated token,
// never one the caller supplies. Knowing a user_id buys nothing.
//
// Holding the TOKEN is the whole capability, and it reaches further than
// "delivery credential" suggests. session.Auth parses it for the api audience
// (session/client_session.go:139), which a client jwt carries
// (jwt/by_jwt.go:670, via newRegisteredClaims at :209), then calls
// jwt.ValidateByJwtState(ctx, byJwt, false) -- requireClient=false
// (session/client_session.go:143) -- so a CLIENT jwt authenticates as the
// account. POST /auth/regenerate-seedphrase (routed at api/api.go:68) is
// WrapWithInputRequireAuth (api/handlers/seedphrase_handlers.go:11), and that
// wrapper's auth is the same session.Auth (router/handler_utils.go:307). So
// whoever holds this response can mint the prober network a fresh seedphrase.
// That leaves them a login credential this server cannot read back, because
// only a salted hash is stored (model/seedphrase_auth_model.go:139-141,
// model/auth_model_identity.go:136), and cannot revoke, because
// RegenerateSeedphrase does not touch credential_change_time. Re-minting or
// dropping the jwt afterwards does not take it back, and the same call
// invalidates whatever phrase an operator had recorded.
//
// The real gate is therefore the operator secret checked in ProberCredential
// below. It is the only thing standing between a caller and that capability;
// the shape of this struct is not part of it. Keep the response narrow anyway
// -- sending the minimum is right on its own terms, and a field set that is
// pinned by test cannot drift into "whatever the model struct happens to have"
// -- but do not mistake it for the protection.
//
// A seedphrase cannot appear here even by accident: it is never persisted.
// createProberNetwork calls NetworkCreate and keeps only the network id, the
// admin user id and the name (model/prober_identity_model.go:502, and the note
// above that call explaining why the phrase is dropped), so there is no column
// on prober_identity that could carry one. That is a property worth preserving
// -- do not add one.
type ProberCredentialResult struct {
	ByClientJwt string    `json:"by_client_jwt"`
	ClientId    server.Id `json:"client_id"`
}

// ProberCredential hands the operator's prober the network client jwt that the
// bootstrap task minted for it (see model/prober_identity_model.go and
// taskworker/work/prober_bootstrap_work.go).
//
// This is the last leg of that bootstrap. The task already creates the prober's
// account, funds it and mints the credential into prober_identity, but nothing
// read that column, so the credential still had to reach the prober process by
// hand -- an env file written by an operator. A prober that can fetch its own
// credential closes the loop: no human step remains between a fresh deployment
// and a probing prober.
//
// Same auth as the provider-egress endpoints it sits beside: operator-to-server,
// the shared X-UR-Operator-Secret header rather than a network jwt, fail-closed
// when the vault resource is missing. One secret, one mechanism -- this route
// hands out a credential, which is the strongest possible reason not to invent
// a second, less-examined way in.
func ProberCredential(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)

	// The two failure branches are logged SEPARATELY, and loudly, which is the
	// one place this deviates from the endpoints next door (they 401 in
	// silence). The deviation is the point: a credential endpoint that rejects
	// everything produces a prober that probes nothing, and a fleet that probes
	// nothing is indistinguishable from a fleet of unhealthy providers. That
	// exact misreading has already cost this system eight hours. Which side is
	// misconfigured is the first question anyone asks, so the log answers it
	// before it is asked.
	if secret == "" {
		// SERVER side: no vault resource, or no ingest_secret in it. Every
		// request is rejected regardless of what the caller sends.
		glog.Errorf(
			"[probercred]this server has no operator ingest secret " +
				"(provider_egress.yml/ingest_secret); the prober cannot fetch its " +
				"credential, so egress probing will not run at all\n",
		)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		// CALLER side: the prober's configured secret is absent or does not
		// match this server's. Neither the provided value nor any prefix of it
		// is logged -- a rejected secret is still a secret, and log shipping is
		// a wider audience than the vault.
		glog.Errorf(
			"[probercred]rejected a prober credential request: missing or wrong %s header; "+
				"the caller's operator secret does not match this server's\n",
			operatorSecretHeader,
		)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	identity := model.GetProberIdentity(r.Context())

	// 404 -- not a 500, and not an empty 200. The prober polls this before the
	// bootstrap task has necessarily finished, and it must be able to tell "not
	// ready yet, keep polling" from "broken, wake someone". An empty 200 makes
	// those two the same response.
	//
	// A missing row is NOT the only not-ready state, and checking only for it
	// would produce exactly that empty 200. The row is committed by
	// createProberNetwork before mintProberClientJwt runs, so it legitimately
	// exists with by_client_jwt still NULL; clearProberIdentityClient also
	// returns it to that state when a client has to be re-provisioned. All
	// three are the same answer to this caller: there is no credential yet.
	//
	// Not logged: the bootstrap task already reports its own failures at
	// Errorf, and a poll during normal startup is not an error.
	if identity == nil || identity.ClientId == nil || identity.ByClientJwt == "" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	result := &ProberCredentialResult{
		ByClientJwt: identity.ByClientJwt,
		ClientId:    *identity.ClientId,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[probercred]could not write response. err = %s\n", err)
	}
}
