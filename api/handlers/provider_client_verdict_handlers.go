package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// ProviderClientVerdictSubmit receives one client-reported blackhole verdict.
//
// Unlike the other provider-probing ingest endpoints in this package, this one
// is NOT operator-secret authed: the reporter is a real client network, and the
// network is the unit the quorum counts. WrapWithInputRequireAuth fails closed
// with a 401 when the jwt is missing or unparseable, and hands the impl a
// session whose ByJwt.NetworkId is the reporter -- which is the only place the
// reporter identity may come from.
//
// All validation, the append-only store and the quorum aggregation live in
// model.SubmitProviderClientVerdict; the strict (unknown-field-rejecting)
// decode is attached to the args type, because the router's decoder is shared
// by every endpoint and must not be tightened globally.
func ProviderClientVerdictSubmit(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SubmitProviderClientVerdict, w, r)
}
