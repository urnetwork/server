// A config that disables URL probing writes no shard rows, so an older
// taskworker that still enables probing could not tell that the shard set was
// retired: it found the shards missing and recreated them after every pass,
// and probed on until it restarted. While a config disables probing, its
// taskworkers keep one marker chain instead, ProviderEgressProbeDisabled,
// stamped with their config version (config_generation.go) and restamped at
// every run. An older config treats a newer config's live marker as that
// config's live geometry: it schedules and recreates no shard and hands back
// every pass it claims, and the disabling config retires those passes. A
// taskworker whose config enables probing retires a marker that is not a
// newer config's live one, and then recreates the shards its config is
// missing, so a disable that was withdrawn while the shard set was retired
// brings the shards back without a restart. The marker lives under its own
// function, so probe row counts and the disabled reap see no probe rows.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The marker runs this often while its config disables probing. It counts as
// live for the minimum newer-generation timeout, six runs.
const providerEgressProbeDisabledInterval = 5 * time.Minute

// A run reads settings and schedules one row.
const providerEgressProbeDisabledMaxTime = time.Minute

type ProviderEgressProbeDisabledArgs struct {
	// The config version that wrote the marker, and when.
	ConfigStamp
}

type ProviderEgressProbeDisabledResult struct {
	// This config disables probing and the marker is not a newer config's
	// live one: the post-step restamps it.
	Restamp bool `json:"restamp,omitempty"`
	// A newer config's live marker: the post-step reschedules it unchanged.
	HandedBack bool `json:"handed_back,omitempty"`
}

// The single run-once key of the marker.
func providerEgressProbeDisabledRunOnce() *task.RunOnceOption {
	return task.RunOnce("provider_egress_probe_disabled")
}

// Whether a marker was written by a newer config than this process's within
// the minimum newer-generation timeout.
func providerEgressProbeDisabledMarkerLive(stamp ConfigStamp, now time.Time) bool {
	return stamp.newerWithin(now, providerEgressProbeNewerGenerationMinTimeout)
}

// Whether the pending marker in tx is a newer config's live one.
func providerEgressProbeDisabledMarkerLiveInTx(ctx context.Context, tx server.PgTx, now time.Time) bool {
	for _, stamp := range readConfigStampsInTx(ctx, tx, []string{providerEgressProbeDisabledRunOnce().String()}) {
		if providerEgressProbeDisabledMarkerLive(stamp, now) {
			return true
		}
	}
	return false
}

// Whether a newer config's live marker shows probing disabled. A process
// without a config version orders nothing and reads nothing.
func providerEgressProbeDisabledByNewerConfig(ctx context.Context, now time.Time) (disabled bool) {
	if currentConfigGeneration() == "" {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		disabled = providerEgressProbeDisabledMarkerLiveInTx(ctx, tx, now)
	}, server.TxReadCommitted)
	return
}

// Schedules the marker, merging into a pending marker, which keeps its stamp
// and moves only earlier.
func scheduleProviderEgressProbeDisabledAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	providerEgressProbeDisabled *ProviderEgressProbeDisabledArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		ProviderEgressProbeDisabled,
		providerEgressProbeDisabled,
		clientSession,
		providerEgressProbeDisabledRunOnce(),
		task.RunAt(runAt),
		task.MaxTime(providerEgressProbeDisabledMaxTime),
	)
}

// One run of the marker. It reads only settings: a newer config's live marker
// is handed back, a disabling config restamps the marker, and an enabling
// config retires it.
func ProviderEgressProbeDisabled(
	providerEgressProbeDisabled *ProviderEgressProbeDisabledArgs,
	clientSession *session.ClientSession,
) (*ProviderEgressProbeDisabledResult, error) {
	settings, err := getProviderEgressProbeSettings()
	if err != nil {
		return nil, err
	}
	if providerEgressProbeDisabled != nil && providerEgressProbeDisabledMarkerLive(providerEgressProbeDisabled.ConfigStamp, server.NowUtc()) {
		configHandbacks.WithLabelValues("provider_egress_probe_disabled").Inc()
		return &ProviderEgressProbeDisabledResult{HandedBack: true}, nil
	}
	return &ProviderEgressProbeDisabledResult{Restamp: !settings.Enabled}, nil
}

// Restamps the marker one interval later, reschedules a handed-back marker
// with its arguments as stored, or schedules nothing, which retires it.
func ProviderEgressProbeDisabledPost(
	providerEgressProbeDisabled *ProviderEgressProbeDisabledArgs,
	providerEgressProbeDisabledResult *ProviderEgressProbeDisabledResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if providerEgressProbeDisabledResult == nil {
		return nil
	}
	now := server.NowUtc()
	switch {
	case providerEgressProbeDisabledResult.HandedBack:
		runAt := now.Add(configHandbackDelay)
		rescheduleHandedBackTaskInTx(clientSession, tx, providerEgressProbeDisabledRunOnce(), runAt, func() {
			scheduleProviderEgressProbeDisabledAt(clientSession, tx, providerEgressProbeDisabled, runAt)
		})
	case providerEgressProbeDisabledResult.Restamp:
		scheduleProviderEgressProbeDisabledAt(clientSession, tx, &ProviderEgressProbeDisabledArgs{ConfigStamp: newConfigStamp(now)},
			now.Add(providerEgressProbeDisabledInterval))
	}
	return nil
}

// The registered post-step: ProviderEgressProbeDisabledPost in the finishing
// transaction, then, when a config that enables probing has retired the
// marker, the probe's recreation of the shards that config is missing, after
// the commit and through the same read committed, shard-ordered transaction
// as after a probe pass.
func ProviderEgressProbeDisabledCommitPost(
	providerEgressProbeDisabled *ProviderEgressProbeDisabledArgs,
	providerEgressProbeDisabledResult *ProviderEgressProbeDisabledResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) ([]server.PostFunction, error) {
	if err := ProviderEgressProbeDisabledPost(providerEgressProbeDisabled, providerEgressProbeDisabledResult, clientSession, tx); err != nil {
		return nil, err
	}
	if providerEgressProbeDisabledResult == nil || providerEgressProbeDisabledResult.HandedBack || providerEgressProbeDisabledResult.Restamp {
		return nil, nil
	}
	// The post's session ends with the post; the recreation keeps only its values.
	ctx := context.WithoutCancel(clientSession.Ctx)
	return []server.PostFunction{func() any {
		recreateMissingProviderEgressProbeShards(ctx)
		return nil
	}}, nil
}

// The marker's task target as the taskworker registers it.
func NewProviderEgressProbeDisabledTaskTarget() *task.TaskTarget[*ProviderEgressProbeDisabledArgs, *ProviderEgressProbeDisabledResult] {
	return task.NewTaskTargetWithCommitPost(ProviderEgressProbeDisabled, ProviderEgressProbeDisabledCommitPost)
}
