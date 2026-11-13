package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// §12.8's record API for the console and network credentials (DNS-M5-12): Network routes, GET for read roles and the
// write for the DNS permission (route_authz.go, route_permission.go). The dns block serves the sidecar routes,
// POST /dns/records and GET /dns/zone, beside them.

// GET /network/dns: label, alias, records and quota.
func GetNetworkDns(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetNetworkDns, w, r)
}

// POST /network/dns/records: {expect_version, upserts, deletes}, compare-and-swap.
func WriteNetworkDnsRecords(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.WriteNetworkDnsRecords, w, r)
}
