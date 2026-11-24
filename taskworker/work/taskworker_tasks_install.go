package work

// What taskworker's tasks run on, installed in each process that runs the worker: taskworker's own, through
// InstallTaskworkerTasks, and all's, which runs the worker beside api's route set and connect's exchange and
// installs the three lists as one (controller.InstallProcessParts, which says how a part two lists name is installed
// once). The parts, in taskworker's order:
//
//	abuse            §21's effects and sealed off-site records for the owed actions the abuse pass finishes, the
//	                 notice pager the clock pass pages through, and §21.7's standing for the reports, record writes
//	                 and grant activations this process runs (controller.AbuseInstallParts; ABUSE-M5-06, ABUSE-M5-07,
//	                 QD-726, QD-1070). api's route set names the same parts, so a process that runs both installs
//	                 them once.
//	breakers         §21.5's outside measures: <root> and the sending addresses from smtpedge.yml, Postmaster Tools
//	                 and SNDS from their vault files, and the listing over this host's copy of the feed mirror; each
//	                 missing one stays unknown and holds the breakers (InstallMailBreakerSources; QD-1069)
//	certs            certs' order, renewal and revocation chains: the engine with its DNS client, ACME client and
//	                 hint writer, while sidecar.yml's certs switch is on and its files are there; otherwise each chain
//	                 runs empty passes (InstallTlsCertChains; QD-846)

import (
	"context"
	"slices"

	"github.com/urnetwork/server/controller"
)

// taskworker's own parts, beside §21's.
const (
	InstallPartMailBreakerSources controller.ProcessInstallPartName = "mail-breaker-sources"
	InstallPartTlsCertChains      controller.ProcessInstallPartName = "tls-cert-chains"
)

// What InstallTaskworkerTasks reads beside config and the vault.
type TaskworkerTasksInstallArgs struct {
	// The breakers' listing feed mirror (MailBreakerSourcesArgs.FeedDirectory). Empty leaves the listing unknown.
	FeedDirectory string
}

// The parts of what taskworker's tasks run on (the file header), in taskworker's order. args may be nil. Nothing is
// installed until a part's Install runs.
func TaskworkerTasksInstallParts(args *TaskworkerTasksInstallArgs) []controller.ProcessInstallPart {
	if args == nil {
		args = &TaskworkerTasksInstallArgs{}
	}
	feedDirectory := args.FeedDirectory
	return slices.Concat(controller.AbuseInstallParts(), []controller.ProcessInstallPart{
		{Name: InstallPartMailBreakerSources, Install: func(context.Context) func() {
			return InstallMailBreakerSources(&MailBreakerSourcesArgs{FeedDirectory: feedDirectory})
		}},
		{Name: InstallPartTlsCertChains, Install: InstallTlsCertChains},
	})
}

// Installs what taskworker's tasks run on (the file header). args may be nil. Answers what removes the parts again,
// the last installed first, each removed before the next.
func InstallTaskworkerTasks(ctx context.Context, args *TaskworkerTasksInstallArgs) func() {
	return controller.InstallProcessParts(ctx, TaskworkerTasksInstallParts(args))
}
