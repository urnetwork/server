// Package sidecarhost is server's half of §4.5's ServiceHost: the sidecar engines' interfaces on server's
// stores and libraries, and the run of one engine as a warp block (D-OPS-1, D-REPO-3). Engines live in
// sidecar's <svc>service packages and never import server, so go-redis, prometheus and Postgres reach
// them only through what this package builds. Each server/cli/<svc> host imports it, and server itself
// never does.
//
//   - host.go assembles a sidecar.ServiceHost: NewServiceHost.
//   - redis*.go: ServiceHost.Redis and §22.5's op-counting wrapper.
//   - metrics.go, tasks.go, auth.go, gates.go: Metrics, Tasks, Authenticator and Gates.
//   - run.go, route_metrics.go: Run, the block's lifecycle around one engine.
//
// Blobs, Push, Quota, ServiceClient, ClockCheck, Budget and Abuse come from their own plans' packages,
// so NewServiceHost takes each as an option and leaves the field nil when it is not given, which every
// engine reads as unavailable.
package sidecarhost

import (
	"context"

	"github.com/urnetwork/sidecar"
)

// What a block's main gives NewServiceHost.
type ServiceHostOptions struct {
	// Whether this process holds a Postgres pool, §19.4's column. Without one Tasks stays nil, since
	// scheduling writes Postgres (plan-OPS-small-readings #8).
	Postgres bool
	// The registry taskworker adds as a target. nil, with Postgres, makes one.
	Tasks *Tasks

	// The ServiceHost fields other plans' packages implement (EVSB, PROTO, QUOTA, TLS, ABUSE). Each nil
	// leaves its field nil.
	Blobs         sidecar.BlobStore
	Push          sidecar.Push
	Quota         sidecar.Quota
	ServiceClient sidecar.ServiceClientSource
	ClockCheck    sidecar.ClockCheck
	Budget        sidecar.WriteBudget
	// §21.7's standing and network_verified from memory: model.NewAbuseGate where the process holds
	// Postgres (ABUSE-M5-07), plan-SOCK-gate-data's route where it does not. nil has loaded no
	// network, so every gate reading it refuses.
	Abuse sidecar.AbuseGate
	// The engines' clock. nil is real time.
	Clock sidecar.Clock

	// nil takes each one's defaults.
	Redis *RedisSettings
	Gates *GatesSettings
}

// The ServiceHost a block's engine takes: Redis on server's pools, Metrics on the default registry,
// the service token Authenticator, the gates read from sidecar.yml until ctx ends, Tasks for a process
// with Postgres, and the rest as options gave them.
func NewServiceHost(ctx context.Context, options *ServiceHostOptions) *sidecar.ServiceHost {
	if options == nil {
		options = &ServiceHostOptions{}
	}
	host := &sidecar.ServiceHost{
		Authenticator: NewAuthenticator(),
		Quota:         options.Quota,
		Metrics:       NewMetrics(),
		Redis:         NewRedis(options.Redis),
		Blobs:         options.Blobs,
		ClockCheck:    options.ClockCheck,
		Push:          options.Push,
		ServiceClient: options.ServiceClient,
		Clock:         options.Clock,
		Gates:         NewGates(ctx, options.Gates),
		Budget:        options.Budget,
		Abuse:         options.Abuse,
	}
	if options.Postgres {
		tasks := options.Tasks
		if tasks == nil {
			tasks = NewTasks(nil)
		}
		host.Tasks = tasks
	}
	return host
}
