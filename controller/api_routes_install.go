package controller

// What api's route set runs on, installed in each process that serves the set: api's behind the lb, alt's, which
// serves the same set over HTTP/3 on the proxy hosts with no lb in front (alt.Run mounts api.NewRouter; EXTENDER.md
// L1), and all's, which runs api beside connect and taskworker in one process. Each route reads what it runs on from
// what its process installed, so api's and alt's mains call InstallApiRoutes once, and all's main installs the same
// parts with connect's and taskworker's (ApiRoutesInstallParts, InstallProcessParts), each over the ServiceHost it
// set the process's Env with and before it serves, and a route answers alike through each front (§19.4's api row).
// A process that serves the set without it answers each route as a process with nothing installed does, every one
// of them closed: the smtp routes, the approval cancel, the authorization log's reads and writes, the exchange and
// the /tls routes 503, the device grant temporarily_unavailable, a staff action owed to the worker's pass, and §21's
// hard deny held by no network, so a contract a suspended network's sidecar asks for, through the process's connect
// node or its control route, is not refused. The parts, in api's order, each described further where it is built:
//
//	abuse            §21's effects, record mailers, notice pager and standing, which taskworker runs on as well
//	                 (AbuseInstallParts)
//	hold dispatch    a hold's placement handed to the worker, whose cluster user reads GroupForms
//	                 (InstallLegalHoldDispatch)
//	authorization    rev/'s store, hold/'s writer that renewal writes through (installApiLegalHolds; QD-1399) and the
//	                 issuer's worker, and the projection: the authorization log's sequencer, the
//	                 gate cache, the approval gate's states, the dns scopes' labels, the smtp routes' §8 checks, the
//	                 report intake's seal key (SmtpReportKeySource, gkm at $UR_GKM_URL), the edge logins, the contract
//	                 gate while sidecar.yml's acl_contract_gate is on and the exchange's scopes
//	                 (installApiAuthorization, installApiProjection); it covers connect's authorization
//	device grant     ur-cli's device authorizations in Redis (oauth.InstallDeviceAuthorizationStore; QD-830)
//	certs            §13.15's /tls routes while sidecar.yml's certs switch is on (InstallTlsCerts; QD-846)
//
// Each part that needs a file the process does not hold stays uninstalled, as its own install says, and the routes
// it serves answer as above; none stops the process.

import (
	"context"
	"slices"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/oauth"
)

// api's own parts, beside §21's.
const (
	InstallPartLegalHoldDispatch        ProcessInstallPartName = "legal-hold-dispatch"
	InstallPartApiAuthorization         ProcessInstallPartName = "api-authorization"
	InstallPartDeviceAuthorizationStore ProcessInstallPartName = "device-authorization-store"
	InstallPartTlsCerts                 ProcessInstallPartName = "tls-certs"
)

// What InstallApiRoutes is built from. Each nil takes what api's block holds.
type ApiRoutesInstallDeps struct {
	// The first metadata cluster the issuer's services read and write, and what releases it. nil: vault's
	// file-clusters-api.yml (loadApiFileClusters).
	Clusters func(ctx context.Context) (sidecar.BlobStore, func(), error)
}

// The parts of what api's route set runs on (the file header), in api's order, over host, the ServiceHost the
// process set its Env with. deps may be nil. Nothing is installed until a part's Install runs.
func ApiRoutesInstallParts(host *sidecar.ServiceHost, deps *ApiRoutesInstallDeps) []ProcessInstallPart {
	if deps == nil {
		deps = &ApiRoutesInstallDeps{}
	}
	loadClusters := deps.Clusters
	if loadClusters == nil {
		loadClusters = loadApiFileClusters
	}
	// §21's first: standing before certs, whose gate reads the hard deny standing installs
	return slices.Concat(AbuseInstallParts(), []ProcessInstallPart{
		{Name: InstallPartLegalHoldDispatch, Install: func(context.Context) func() {
			return InstallLegalHoldDispatch()
		}},
		{Name: InstallPartApiAuthorization, Covers: []ProcessInstallPartName{InstallPartConnectAuthorization}, Install: func(ctx context.Context) func() {
			return installApiAuthorization(ctx, host, loadClusters)
		}},
		{Name: InstallPartDeviceAuthorizationStore, Install: func(context.Context) func() {
			return oauth.InstallDeviceAuthorizationStore()
		}},
		{Name: InstallPartTlsCerts, Install: InstallTlsCerts},
	})
}

// Installs what api's route set runs on (the file header) over host, the ServiceHost the process set its Env with.
// deps may be nil. Answers what removes the parts again, the last installed first, each removed before the next.
func InstallApiRoutes(ctx context.Context, host *sidecar.ServiceHost, deps *ApiRoutesInstallDeps) func() {
	return InstallProcessParts(ctx, ApiRoutesInstallParts(host, deps))
}
