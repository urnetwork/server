package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// Verify backs `POST /verify` (sn/VALIDATOR.md §4). No JWT by design
// (PLAN.md D-7): the protocol is self-authenticating — the body carries the
// validator's client_id and an Ed25519 signature under that client's
// registered key — and unknown callers are answered with indistinguishable
// poisoned trails (§9).
func Verify(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.Verify, w, r)
}

// GetVerifyKeys backs `GET /verify/keys`. Unauthenticated by design: the
// values are the published server Ed25519 public keys, by server_key_id, so
// any third party can verify published trail proofs (VALIDATOR.md §3.5).
func GetVerifyKeys(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.GetVerifyKeys, w, r)
}
