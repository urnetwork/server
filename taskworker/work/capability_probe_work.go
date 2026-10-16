// The capability probe's task (server/capprobe): the FP2 modern provider
// gate's evidence, end-to-end post-quantum encryption and resizable transfer
// windows, measured in short tunnels of its own and recorded straight into
// provider_modern_evidence. One task row at a time runs one pass of the
// probe's sweep; its post-step schedules the next pass with the sweep's
// place. Each pass that measures runs in a prober shard of its own, a private
// network, credential parent and balance separate from every URL probe shard,
// so its load, funding and contracts never draw on the URL probe's quota or
// budget; a pass with nothing to measure takes no shard.
package work

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The prober shard slot the capability probe owns: the registry's last. The
// URL probe's shards are slots [0, shard_count), and its settings and
// arguments refuse a shard_count past maxProviderUrlProbeShardCount, which
// stops before this slot. The capability probe still refuses to run while
// the URL probe's shard count reaches it, rather than retire a URL shard's
// owner or have its own retired by one.
const (
	capabilityProbeShardIndex = maxProviderEgressProbeShardCount - 1
	capabilityProbeShardCount = maxProviderEgressProbeShardCount
)

// The retry delay of a failed pass.
const capabilityProbeRetryDelay = time.Minute

// The sweep's place and the pass's budget, all a pass needs that its
// settings do not hold.
type CapabilityProbeArgs struct {
	Sweep capprobe.Sweep `json:"sweep"`
	// the max time the pass's task row was scheduled with, the pass's whole
	// budget; zero for a row scheduled without one
	MaxTimeSeconds int `json:"max_time_seconds,omitempty"`
}

// What one pass did, and the sweep's place after it.
type CapabilityProbeResult struct {
	// the settings switched the probe off: the pass ran nothing, and no
	// successor is scheduled
	Disabled bool                 `json:"disabled,omitempty"`
	Sweep    capprobe.Sweep       `json:"sweep"`
	Pass     *capprobe.PassResult `json:"pass,omitempty"`
}

// Package-level seams like the URL probe's: immutable in production,
// replaced by tests that must not open tunnels or read the deployment.
var getCapabilityProbeSettings = capprobe.LoadSettings
var executeCapabilityProbe = runCapabilityProbe

// One task row for the whole probe.
func capabilityProbeRunOnce() *task.RunOnceOption {
	return task.RunOnce("capability_probe")
}

// At a taskworker's start, ensures the probe's task row while
// capability_probe.yml switches the probe on, due at once, and does nothing
// else. The probe ships switched off, and a switched-off probe schedules
// nothing, so it is turned on by a taskworker starting with settings that
// enable it. A row that exists keeps its arguments, so a restart keeps the
// sweep's place; its run time only moves earlier, and a pass that runs
// before its sweep's next start waits again. Settings that cannot be read
// switch nothing on, and are logged.
func ScheduleCapabilityProbe(clientSession *session.ClientSession, tx server.PgTx) {
	settings, err := getCapabilityProbeSettings()
	if err != nil {
		glog.Errorf("[capprobe]not scheduled: %s cannot be used: %s\n", capprobe.SettingsResourceName, err)
		return
	}
	if !settings.Enabled {
		return
	}
	scheduleCapabilityProbeAt(clientSession, tx, &CapabilityProbeArgs{}, server.NowUtc(), settings)
}

// While capability_probe.yml switches the probe on, schedules a pass due at
// once when the probe's chain has no task row, and reports whether it did. A
// row that exists, waiting or claimed, keeps its arguments, run time and
// backoff. A chain ends at a pass that reads the probe as off, as one on a
// taskworker still running an older config does after the switch-on, or
// when maintenance removes its row; a taskworker that reads the probe as on
// calls this every few minutes (taskworker's ensureCapabilityProbeChain), so
// such a chain returns, starting a new sweep. Settings that cannot be read
// schedule nothing, and are logged.
func EnsureCapabilityProbe(ctx context.Context) bool {
	settings, err := getCapabilityProbeSettings()
	if err != nil {
		glog.Errorf("[capprobe]chain not checked: %s cannot be used: %s\n", capprobe.SettingsResourceName, err)
		return false
	}
	if !settings.Enabled {
		return false
	}
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	args, maxTime := capabilityProbeScheduled(&CapabilityProbeArgs{}, settings)
	scheduled, _ := task.ScheduleTaskIfAbsent(
		CapabilityProbe,
		args,
		clientSession,
		capabilityProbeRunOnce(),
		task.RunAt(server.NowUtc()),
		task.MaxTime(maxTime),
	)
	if scheduled {
		capprobe.ObserveChainRestart()
		glog.Infof("[capprobe]the probe's chain had no task row; scheduled a pass due at once\n")
	}
	return scheduled
}

// Schedules a pass, merging into the pending row of the probe's key.
func scheduleCapabilityProbeAt(clientSession *session.ClientSession, tx server.PgTx, args *CapabilityProbeArgs, runAt time.Time, settings capprobe.Settings) {
	scheduledArgs, maxTime := capabilityProbeScheduled(args, settings)
	task.ScheduleTaskInTx(
		tx,
		CapabilityProbe,
		scheduledArgs,
		clientSession,
		capabilityProbeRunOnce(),
		task.RunAt(runAt),
		task.MaxTime(maxTime),
	)
}

// The arguments a pass is scheduled with, and its task row's max time: the
// settings', or the default one for settings that switch the probe off,
// which are not validated, as a post-step that could not read the file has. The max time rides in the arguments too, so the pass's
// budget is its row's whatever the settings say when it runs. A row that
// merges into a pending one keeps that one's arguments, and the row's max
// time is then the greater of the two, never less than the arguments'.
func capabilityProbeScheduled(args *CapabilityProbeArgs, settings capprobe.Settings) (*CapabilityProbeArgs, time.Duration) {
	maxTime := settings.MaxTime()
	if !settings.Enabled {
		maxTime = capprobe.DefaultSettings().MaxTime()
	}
	scheduledArgs := CapabilityProbeArgs{}
	if args != nil {
		scheduledArgs = *args
	}
	scheduledArgs.MaxTimeSeconds = int(maxTime / time.Second)
	return &scheduledArgs, maxTime
}

// The deployment's settings, or the defaults when they cannot be read: the
// pass itself reports an unusable file, and the chain keeps the default
// cadence until it is fixed.
func capabilityProbeSettingsOrDefault() capprobe.Settings {
	settings, err := getCapabilityProbeSettings()
	if err != nil {
		return capprobe.DefaultSettings()
	}
	return settings
}

// One pass of the capability probe. Settings are read at every pass, so a
// change applies at the next one; a switched-off probe runs nothing, and its
// chain ends there (CapabilityProbePost). The pass's budget is the max time its task row
// was scheduled with, carried in its arguments: the task cancels the pass at
// that time whatever the settings say now. A failed pass is retried after a
// minute with the same sweep.
func CapabilityProbe(
	args *CapabilityProbeArgs,
	clientSession *session.ClientSession,
) (*CapabilityProbeResult, error) {
	settings, err := getCapabilityProbeSettings()
	if err != nil {
		capprobe.ObservePass(capprobe.PassResultFailed)
		return nil, task.WithRetryDelay(fmt.Errorf("capability probe settings: %w", err), capabilityProbeRetryDelay)
	}
	sweep := capprobe.Sweep{}
	scheduledMaxTimeSeconds := 0
	if args != nil {
		sweep = args.Sweep
		scheduledMaxTimeSeconds = args.MaxTimeSeconds
	}
	if !settings.Enabled {
		capprobe.ObservePass(capprobe.PassResultDisabled)
		return &CapabilityProbeResult{Disabled: true, Sweep: sweep}, nil
	}
	maxTime := settings.PassMaxTime(scheduledMaxTimeSeconds)
	// Task MaxTime cancels the session with a timer but sets no deadline;
	// the pass's admission needs the deadline to finish what it starts.
	probeCtx, cancel := context.WithTimeout(clientSession.Ctx, maxTime)
	defer cancel()
	next, pass, err := executeCapabilityProbe(probeCtx, settings, sweep, maxTime)
	if err != nil {
		// The retry runs from the sweep the pass returned: the same place,
		// and the held downgrades as a measuring pass left them. The task
		// keeps the arguments it ran with when it refuses the checkpoint:
		// for a failure with a cancellation among its causes, one joined
		// with a prober shard cleanup failure, or arguments past its 4 KiB
		// bound, which about 38 held downgrades reach.
		return nil, task.WithRetryDelayAndArgs(err, capabilityProbeRetryDelay, &CapabilityProbeArgs{Sweep: next, MaxTimeSeconds: scheduledMaxTimeSeconds})
	}
	return &CapabilityProbeResult{Sweep: next, Pass: pass}, nil
}

// Schedules the next pass: within a sweep after the pass interval, between
// sweeps a waiting pass or the next sweep's start, whichever comes first. A
// switched-off probe's pass schedules nothing, which ends the chain; a
// taskworker that starts with the probe switched on schedules it again
// (ScheduleCapabilityProbe).
func CapabilityProbePost(
	args *CapabilityProbeArgs,
	result *CapabilityProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := capabilityProbeSettingsOrDefault()
	nextArgs, runAt, scheduled := capabilityProbeSuccessor(args, result, settings, server.NowUtc())
	if scheduled {
		scheduleCapabilityProbeAt(clientSession, tx, nextArgs, runAt, settings)
	}
	return nil
}

// The next pass's arguments and run time after a pass finished at now, and
// false for none: a switched-off probe's pass ends the chain.
func capabilityProbeSuccessor(args *CapabilityProbeArgs, result *CapabilityProbeResult, settings capprobe.Settings, now time.Time) (*CapabilityProbeArgs, time.Time, bool) {
	switch {
	case result == nil:
		sweep := capprobe.Sweep{}
		if args != nil {
			sweep = args.Sweep
		}
		return &CapabilityProbeArgs{Sweep: sweep}, now.Add(settings.PassInterval()), true
	case result.Disabled:
		return nil, time.Time{}, false
	default:
		return &CapabilityProbeArgs{Sweep: result.Sweep}, settings.NextPassAt(result.Sweep, now), true
	}
}

// The probe's task target as the taskworker registers it.
func NewCapabilityProbeTaskTarget() *task.TaskTarget[*CapabilityProbeArgs, *CapabilityProbeResult] {
	return task.NewTaskTargetWithPost(CapabilityProbe, CapabilityProbePost)
}

// The capability probe's shard slot is free of the URL probe's geometry.
func capabilityProbeShardAvailable(urlProbeShardCount int) error {
	if capabilityProbeShardIndex < urlProbeShardCount {
		return fmt.Errorf("the URL probe's shard_count %d reaches the capability probe's prober shard %d", urlProbeShardCount, capabilityProbeShardIndex)
	}
	return nil
}

// The private balance of one pass's shard: the standard contract ceiling of
// every capability tunnel a pass can open (each provider of its limit, with
// a window re-dial apiece), origin and companion both, every renewal stage,
// ten times over as the URL probe's funding is, and never under the shared
// shard headroom. This is accounting credit in the shard's own network, not
// a traffic quota; a capability tunnel reserves far less.
func capabilityProbeShardCredit(settings capprobe.Settings) (model.ByteCount, error) {
	contracts := connect.DefaultContractManagerSettings()
	if contracts.ContractTransferByteSeqScale > uint64(math.MaxInt64-3) {
		return 0, fmt.Errorf("capability probe credit geometry overflows")
	}
	amount := int64(contracts.StandardContractTransferByteCount)
	for _, factor := range []int64{
		int64(settings.Limit) + int64(settings.Concurrency),
		2, // a window re-dial
		2, // origin and companion both charge the private payer
		int64(contracts.ContractTransferByteSeqScale) + 3,
		providerUrlProbeFundingMultiplier,
	} {
		if amount < 1 || factor < 1 || amount > math.MaxInt64/factor {
			return 0, fmt.Errorf("capability probe credit geometry overflows")
		}
		amount *= factor
	}
	return model.ByteCount(max(amount, int64(model.ProberShardTransferHeadroom))), nil
}

// A capability pass's funding readiness over its shard's network: the URL
// probe's check, whose failures count in the capability probe's own metrics,
// never in the URL probe's pass errors.
func newCapabilityProbeReadiness(networkId server.Id) *providerEgressProbeReadiness {
	readiness := newProviderEgressProbeReadiness(networkId)
	readiness.observeFailure = capprobe.ObserveReadinessFailure
	return readiness
}

// What one pass of the production probe produced.
type capabilityProbeOutcome struct {
	sweep capprobe.Sweep
	pass  *capprobe.PassResult
}

// The production pass, whose whole budget is maxTime. It selects first, from
// the database alone; a pass that waits for the sweep's next start or finds
// nothing to measure finishes there and costs no prober shard. Otherwise it
// measures in a fresh prober shard for this task execution
// (runCapabilityProbeShard).
func runCapabilityProbe(ctx context.Context, settings capprobe.Settings, sweep capprobe.Sweep, maxTime time.Duration) (capprobe.Sweep, *capprobe.PassResult, error) {
	fail := func(err error) (capprobe.Sweep, *capprobe.PassResult, error) {
		capprobe.ObservePass(capprobe.PassResultFailed)
		return sweep, nil, err
	}
	execution, ok := task.ExecutionIdentityFromContext(ctx)
	if !ok {
		return fail(errors.New("capability probe requires a durable task execution owner"))
	}
	urlProbeSettings, err := getProviderEgressProbeSettings()
	if err != nil {
		return fail(fmt.Errorf("capability probe control-plane endpoints: %w", err))
	}
	if err := capabilityProbeShardAvailable(urlProbeSettings.ShardCount); err != nil {
		return fail(err)
	}
	// SelectPass and FinishPass count the failures they return
	selected, err := capprobe.SelectPass(ctx, settings, sweep)
	if err != nil {
		return sweep, nil, err
	}
	if !selected.NeedsTunnels() {
		return capprobe.FinishPass(ctx, settings, selected, providertunnel.Config{})
	}
	key := model.ProberShardKey{
		TaskId:     execution.TaskId,
		Epoch:      execution.Epoch,
		ShardIndex: capabilityProbeShardIndex,
		ShardCount: capabilityProbeShardCount,
	}
	return runCapabilityProbeShard(ctx, key, settings, sweep, maxTime, urlProbeSettings, func(tunnelConfig providertunnel.Config) (capprobe.Sweep, *capprobe.PassResult, error) {
		return capprobe.FinishPass(ctx, settings, selected, tunnelConfig)
	})
}

// One measuring pass of sweep in the prober shard key, which lives for the
// pass's budget maxTime: the shard's private credit for settings, its
// identity, derived-client credentials and in-process control, the funding
// readiness check, and then finish with the pass's tunnel configuration and
// the URL probe's control-plane endpoints. The shard is drained and reaped
// when finish returns, after every tunnel has joined;
// capabilityProbeShardPassResult says what the pass returns.
func runCapabilityProbeShard(
	ctx context.Context,
	key model.ProberShardKey,
	settings capprobe.Settings,
	sweep capprobe.Sweep,
	maxTime time.Duration,
	urlProbeSettings providerEgressProbeSettings,
	finish func(providertunnel.Config) (capprobe.Sweep, *capprobe.PassResult, error),
) (capprobe.Sweep, *capprobe.PassResult, error) {
	credit, err := capabilityProbeShardCredit(settings)
	if err != nil {
		capprobe.ObservePass(capprobe.PassResultFailed)
		return sweep, nil, err
	}
	// whether finish ran, and what it returned: it counts its own failures
	finished := false
	var finishErr error
	outcome, err := runWithProberShard(ctx, "capability probe", key, credit, maxTime, func(identity *model.ProberIdentity) (capabilityProbeOutcome, error) {
		credentials, err := newProviderEgressCredentials(identity)
		if err != nil {
			return capabilityProbeOutcome{}, err
		}
		return runWithProviderEgressControl(ctx, credentials, func(localControl connect.NetworkClientControl) (capabilityProbeOutcome, error) {
			if err := newCapabilityProbeReadiness(*identity.NetworkId).check(ctx); err != nil {
				return capabilityProbeOutcome{}, err
			}
			finished = true
			next, pass, err := finish(providertunnel.Config{
				ApiUrl:            urlProbeSettings.APIURL,
				PlatformUrl:       urlProbeSettings.PlatformURL,
				ByJwt:             identity.ByClientJwt,
				ClientId:          connect.Id(*identity.ClientId),
				DeviceDescription: model.ProberClientDescription,
				DeviceSpec:        model.ProberClientDeviceSpec,
				Version:           server.RequireVersion(),
				ClientCredentials: credentials,
				ClientControl:     localControl,
			})
			finishErr = err
			return capabilityProbeOutcome{sweep: next, pass: pass}, err
		})
	})
	return capabilityProbeShardPassResult(sweep, outcome, finished, finishErr, err)
}

// What a measuring pass returns once its prober shard is released, from what
// the shard's run returned (outcome, err) and whether finish ran and what it
// returned (finished, finishErr).
//
// A pass that finish finished stands even when the shard's cleanup failed
// afterwards: the prober reaper retries the cleanup, and returning the old
// sweep would have the retry a minute later measure the pass's providers
// again, those without evidence at once, and count the pass both measured
// and failed. Every cleanup failure is counted apart and logged. A pass that
// finish failed returns finish's sweep, the pass's place with the held
// downgrades as it left them; a failure before finish leaves the sweep where
// it was. Each is counted once: finish counts its own, this the ones before
// it.
func capabilityProbeShardPassResult(
	sweep capprobe.Sweep,
	outcome capabilityProbeOutcome,
	finished bool,
	finishErr error,
	err error,
) (capprobe.Sweep, *capprobe.PassResult, error) {
	var cleanupErr *proberShardCleanupError
	if errors.As(err, &cleanupErr) {
		capprobe.ObserveShardCleanupFailure()
		glog.Errorf("[capprobe]prober shard cleanup failed, left to the prober reaper: %s\n", cleanupErr)
	}
	switch {
	case err == nil:
		return outcome.sweep, outcome.pass, nil
	case finished && finishErr == nil:
		// the only failure is the cleanup's
		return outcome.sweep, outcome.pass, nil
	case finished:
		// finish failed, and counted it; its sweep keeps the pass's place
		// and carries the held downgrades as the pass left them
		return outcome.sweep, outcome.pass, err
	default:
		capprobe.ObservePass(capprobe.PassResultFailed)
		return sweep, outcome.pass, err
	}
}
