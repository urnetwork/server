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

// POST /network/dns/label-reclaim: re-binds an ownerless retiring name after a restore.
func ReclaimNetworkDnsLabel(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.ReclaimNetworkDnsLabel, w, r)
}

// POST /network/dns/alias-claim: the network's name as its alias, public forever (CT).
func ClaimNetworkDnsAlias(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.ClaimNetworkDnsAlias, w, r)
}

// POST /network/dns/alias-release: the live alias retires for 90 days, then tombstones for good.
func ReleaseNetworkDnsAlias(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.ReleaseNetworkDnsAlias, w, r)
}

// POST /network/dns/custom-host-add: maps a verified domain's host to a host owner of the network, Team only.
// Naming the network's mail host maps a strict-mode MX host (§12.8 step 4), and that moves the envelope of mail
// sent From each verified domain it stands at or under to b.<domain> at once, whether or not mail.strict is signed
// yet, since api never reads the config repo; removing it moves the envelope back to b.<net>.<root> (QD-1129;
// GET /mail/status's strict_domains).
func AddNetworkDnsCustomHost(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.AddNetworkDnsCustomHost, w, r)
}

// POST /network/dns/custom-host-remove: withdraws a custom host.
func RemoveNetworkDnsCustomHost(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RemoveNetworkDnsCustomHost, w, r)
}
