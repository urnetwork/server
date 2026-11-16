package taskworker

// The block's config for evs's jobs (QD-854; §11.6's format row, D-PHASE-10, FS-72): config/<env>/evs.yml as
// evshost.LoadBlockConfig reads it, the file evs's own block reads, taken once when a runtime builds its evs host.
// Its instance_format becomes the EvsSettings.InstanceFormat every evs job of that runtime runs with: 2 where the file
// is absent, and 1 in a rollback's, under which a job keeps format 1's rules and its guard over format-2 data
// (evsservice/format1_guard.go): evacuation leaves format-2 batches to a format-2 evacuator, alarming, and no fold,
// page-out, path-epoch copy or marker collection writes to a format-2 repo.
//
// A config that does not read, or names a format this code does not know, builds no host: every evs job then fails
// and waits on server/task's backoff, as in a process with no file clusters, and none runs under a format its operator
// did not deploy.
//
// The settings reach a job bound to its run: the registry binds the settings read for the host the run is on to the
// run's context (registerEvsJobs), so a run never mixes one runtime's host with another's settings, and a run on a
// host no settings were read for fails and waits. A job's function called outside the registry, as a test calls one,
// finds none bound and runs this code's defaults.
//
// Format 2 added jobs a format-1 binary does not hold: the drop job and the one-off sweeps, which delete what format
// 2's rules name. A taskworker at instance format 1 runs none of them (evsJob.format2): each run fails and waits for a
// format-2 taskworker, so a rollback purges nothing.

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server/evshost"
)

// A run on a host no block config was read for: retried on server/task's backoff, never run under settings of
// another runtime's.
var errNoEvsJobSettings = errors.New("this taskworker's evs host is not the one its block config was read for, so evs's jobs wait")

// A run of a job format 2 added, in a taskworker at instance format 1: retried on server/task's backoff until a
// format-2 taskworker takes it.
var errEvsJobNeedsFormat2 = errors.New("this taskworker runs as instance format 1, which holds no format-2 job, so the run waits for a format-2 taskworker")

// The engine settings a runtime's evs host runs its jobs with.
type evsJobHostSettings struct {
	host        *sidecar.ServiceHost
	evsSettings *evsservice.EvsSettings
}

// The newest runtime's, stored before its host is the jobs' and removed after.
var evsJobHostSettingsCurrent atomic.Pointer[evsJobHostSettings]

// The engine settings evs's jobs run with in a process deployed with the block's config: this code's defaults at the
// config's instance format. An error for a config that does not read or names a format this code does not know.
func LoadEvsJobSettings() (*evsservice.EvsSettings, error) {
	blockConfig, err := evshost.LoadBlockConfig()
	if err != nil {
		return nil, err
	}
	evsSettings := blockConfig.EvsSettings(nil)
	if err := evsSettings.Validate(); err != nil {
		return nil, err
	}
	return evsSettings, nil
}

// The settings read for host, or errNoEvsJobSettings when host is not the one the newest settings were read for.
func evsJobSettingsOfHost(host *sidecar.ServiceHost) (*evsservice.EvsSettings, error) {
	installed := evsJobHostSettingsCurrent.Load()
	if installed == nil || installed.host != host {
		return nil, errNoEvsJobSettings
	}
	return installed.evsSettings, nil
}

type evsJobSettingsContextKey struct{}

// ctx with the engine settings one run takes.
func withEvsJobSettings(ctx context.Context, evsSettings *evsservice.EvsSettings) context.Context {
	return context.WithValue(ctx, evsJobSettingsContextKey{}, evsSettings)
}

// The engine settings a run takes: the ones the registry bound to it, or this code's defaults for a run nothing bound,
// a job's function a test calls directly.
func evsJobSettings(ctx context.Context) *evsservice.EvsSettings {
	if evsSettings, ok := ctx.Value(evsJobSettingsContextKey{}).(*evsservice.EvsSettings); ok && evsSettings != nil {
		return evsSettings
	}
	return evsservice.DefaultEvsSettings()
}
