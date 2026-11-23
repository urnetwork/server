package controller

// What api's route set runs on, installed in each process that serves the set: api's behind the lb, and alt's, which
// serves the same set over HTTP/3 on the proxy hosts with no lb in front (alt.Run mounts api.NewRouter; EXTENDER.md
// L1). Each route reads what it runs on from what its process installed, so both mains call InstallApiRoutes once,
// over the ServiceHost they set the process's Env with and before they serve, and a route answers alike through
// either front (§19.4's api row). A process that serves the set without it answers each route as a process with
// nothing installed does, every one of them closed: the smtp routes, the approval cancel, the authorization log's
// reads and writes, the exchange and the /tls routes 503, the device grant temporarily_unavailable, a staff action
// owed to the worker's pass, and §21's hard deny held by no network, so a contract a suspended network's sidecar asks
// alt for, through its connect node or its control route, is not refused. The parts, in api's order, each described
// further where it is built:
//
//	abuse effects    §21's effects for the actions this process applies (InstallAbuseEffects; ABUSE-M5-06)
//	record mailers   the sealed off-site record of each action and each hold (InstallAbuseRecordMailer,
//	                 InstallLegalHoldRecordMailer; QD-726)
//	hold dispatch    a hold's placement handed to the worker, whose cluster user reads GroupForms
//	                 (InstallLegalHoldDispatch)
//	notice pager     §21.8's pages through the monitor's webhook (InstallAbuseNoticePager; QD-1070)
//	standing         §21.7's standing and network_verified, and the hard deny CreateContract reads
//	                 (InstallAbuseStanding; ABUSE-M5-07)
//	authorization    rev/'s store and the issuer's worker, and the projection: the authorization log's sequencer, the
//	                 gate cache, the approval gate's states, the dns scopes' labels, the smtp routes' §8 checks, the
//	                 edge logins, the contract gate while sidecar.yml's acl_contract_gate is on and the exchange's
//	                 scopes (installApiAuthorization, installApiProjection)
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

// What InstallApiRoutes is built from. Each nil takes what api's block holds.
type ApiRoutesInstallDeps struct {
	// The first metadata cluster the issuer's services read and write, and what releases it. nil: vault's
	// file-clusters-api.yml (loadApiFileClusters).
	Clusters func(ctx context.Context) (sidecar.BlobStore, func(), error)
}

// Installs what api's route set runs on (the file header) over host, the ServiceHost the process set its Env with.
// deps may be nil. Answers what removes the parts again, the last installed first, each removed before the next.
func InstallApiRoutes(ctx context.Context, host *sidecar.ServiceHost, deps *ApiRoutesInstallDeps) func() {
	if deps == nil {
		deps = &ApiRoutesInstallDeps{}
	}
	loadClusters := deps.Clusters
	if loadClusters == nil {
		loadClusters = loadApiFileClusters
	}
	undos := []func(){}
	undos = append(undos, InstallAbuseEffects())
	undos = append(undos, InstallAbuseRecordMailer())
	undos = append(undos, InstallLegalHoldRecordMailer())
	undos = append(undos, InstallLegalHoldDispatch())
	undos = append(undos, InstallAbuseNoticePager())
	// before certs, whose gate reads the hard deny this installs
	undos = append(undos, InstallAbuseStanding(ctx))
	undos = append(undos, installApiAuthorization(ctx, host, loadClusters))
	undos = append(undos, oauth.InstallDeviceAuthorizationStore())
	undos = append(undos, InstallTlsCerts(ctx))
	return func() {
		for _, undo := range slices.Backward(undos) {
			undo()
		}
	}
}
