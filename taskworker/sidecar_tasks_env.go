package taskworker

import (
	"github.com/urnetwork/server/sidecarhost"
)

// The process's sidecar task registry, the one whose target the production worker adds (evs_jobs.go),
// for the ServiceHost a main gives its process's Env (core.SetProcessEnv, QD-1027): a job bound through
// that Env's Tasks is then a job this process's worker claims and runs, rather than a name in a registry
// no worker reads.
func SidecarTasks() *sidecarhost.Tasks {
	return sidecarTasks
}
