package controller

// What connect's exchange runs on, installed in each process that runs the exchange: connect's own, through
// InstallConnectExchange, and all's, which runs it beside api's route set and taskworker's tasks and installs the
// three lists as one (InstallProcessParts). Its residents' CreateContract and §8.19 peer lists read two parts, in
// connect's order:
//
//	hard deny        §21's hard deny, on a network_abuse_state snapshot of the process's own (InstallAbuseHardDeny;
//	                 ABUSE-M5-07)
//	authorization    the contract gate while sidecar.yml's acl_contract_gate is on, over a gate cache that follows
//	                 api's internal gate surface, since connect holds no metadata credentials
//	                 (InstallConnectAuthorization)
//
// In a process whose lists hold api's route set, api's parts cover both: api's standing's snapshot is the process's
// hard deny, and api's projection installs the contract gate over api's own gate cache, read from the first metadata
// cluster. Such a process has no other gate surface to follow either: its api's internal gate route answers from
// that same cache, unavailable until it loads. alt, which runs a connect node beside api's route set, installs api's
// alone for the same reason.

import (
	"context"

	"github.com/urnetwork/sidecar"
)

// connect's parts.
const (
	InstallPartAbuseHardDeny        ProcessInstallPartName = "abuse-hard-deny"
	InstallPartConnectAuthorization ProcessInstallPartName = "connect-authorization"
)

// The parts of what connect's exchange runs on (the file header), in connect's order, over host, the ServiceHost the
// process set its Env with. Nothing is installed until a part's Install runs.
func ConnectExchangeInstallParts(host *sidecar.ServiceHost) []ProcessInstallPart {
	return []ProcessInstallPart{
		{Name: InstallPartAbuseHardDeny, Install: InstallAbuseHardDeny},
		{Name: InstallPartConnectAuthorization, Install: func(ctx context.Context) func() {
			return InstallConnectAuthorization(ctx, host)
		}},
	}
}

// Installs what connect's exchange runs on (the file header) over host, the ServiceHost the process set its Env
// with. Answers what removes the parts again, the last installed first, each removed before the next.
func InstallConnectExchange(ctx context.Context, host *sidecar.ServiceHost) func() {
	return InstallProcessParts(ctx, ConnectExchangeInstallParts(host))
}
