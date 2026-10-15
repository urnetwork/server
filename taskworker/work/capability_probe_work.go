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

// How long a switched-off probe waits before it reads its settings again.
const capabilityProbeDisabledRecheck = 10 * time.Minute

// The retry delay of a failed pass.
const capabilityProbeRetryDelay = time.Minute

// The sweep's place, all a pass needs that its settings do not hold.
type CapabilityProbeArgs struct {
	Sweep capprobe.Sweep `json:"sweep"`
}

// What one pass did, and the sweep's place after it.
type CapabilityProbeResult struct {
	// the settings switched the probe off; the sweep stays where it was
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

// Ensures the probe's task row. A row that exists keeps its arguments, so a
// restart keeps the sweep's place; its run time only moves earlier, and a
// pass that runs before its sweep's next start waits again.
func ScheduleCapabilityProbe(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleCapabilityProbeAt(clientSession, tx, &CapabilityProbeArgs{}, server.NowUtc(), capabilityProbeSettingsOrDefault())
}

// Schedules a pass, merging into the pending row of the probe's key. A
// switched-off probe's settings are not validated, so its row takes the
// default max time.
func scheduleCapabilityProbeAt(clientSession *session.ClientSession, tx server.PgTx, args *CapabilityProbeArgs, runAt time.Time, settings capprobe.Settings) {
	maxTime := settings.MaxTime()
	if !settings.Enabled {
		maxTime = capprobe.DefaultSettings().MaxTime()
	}
	task.ScheduleTaskInTx(
		tx,
		CapabilityProbe,
		args,
		clientSession,
		capabilityProbeRunOnce(),
		task.RunAt(runAt),
		task.MaxTime(maxTime),
	)
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
// change applies at the next one; a switched-off probe runs nothing and
// keeps its sweep's place. A failed pass is retried after a minute with the
// same sweep.
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
	if args != nil {
		sweep = args.Sweep
	}
	if !settings.Enabled {
		capprobe.ObservePass(capprobe.PassResultDisabled)
		return &CapabilityProbeResult{Disabled: true, Sweep: sweep}, nil
	}
	// Task MaxTime cancels the session with a timer but sets no deadline;
	// the pass's admission needs the deadline to finish what it starts.
	probeCtx, cancel := context.WithTimeout(clientSession.Ctx, settings.MaxTime())
	defer cancel()
	next, pass, err := executeCapabilityProbe(probeCtx, settings, sweep)
	if err != nil {
		return nil, task.WithRetryDelay(err, capabilityProbeRetryDelay)
	}
	return &CapabilityProbeResult{Sweep: next, Pass: pass}, nil
}

// Schedules the next pass: within a sweep after the pass interval, between
// sweeps at the next sweep's start, and a switched-off probe after its
// recheck interval.
func CapabilityProbePost(
	args *CapabilityProbeArgs,
	result *CapabilityProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := capabilityProbeSettingsOrDefault()
	nextArgs, runAt := capabilityProbeSuccessor(args, result, settings, server.NowUtc())
	scheduleCapabilityProbeAt(clientSession, tx, nextArgs, runAt, settings)
	return nil
}

// The next pass's arguments and run time after a pass finished at now.
func capabilityProbeSuccessor(args *CapabilityProbeArgs, result *CapabilityProbeResult, settings capprobe.Settings, now time.Time) (*CapabilityProbeArgs, time.Time) {
	switch {
	case result == nil:
		sweep := capprobe.Sweep{}
		if args != nil {
			sweep = args.Sweep
		}
		return &CapabilityProbeArgs{Sweep: sweep}, now.Add(settings.PassInterval())
	case result.Disabled:
		return &CapabilityProbeArgs{Sweep: result.Sweep}, now.Add(capabilityProbeDisabledRecheck)
	default:
		return &CapabilityProbeArgs{Sweep: result.Sweep}, settings.NextPassAt(result.Sweep, now)
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

// What one pass of the production probe produced.
type capabilityProbeOutcome struct {
	sweep capprobe.Sweep
	pass  *capprobe.PassResult
}

// The production pass. It selects first, from the database alone; a pass that
// waits for the sweep's next start or finds nothing to measure finishes
// there and costs no prober shard. Otherwise it measures in a fresh prober
// shard for this task execution (runCapabilityProbeShard).
func runCapabilityProbe(ctx context.Context, settings capprobe.Settings, sweep capprobe.Sweep) (capprobe.Sweep, *capprobe.PassResult, error) {
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
	return runCapabilityProbeShard(ctx, key, settings, sweep, urlProbeSettings, func(tunnelConfig providertunnel.Config) (capprobe.Sweep, *capprobe.PassResult, error) {
		return capprobe.FinishPass(ctx, settings, selected, tunnelConfig)
	})
}

// One measuring pass of sweep in the prober shard key: the shard's private
// credit for settings, its identity, derived-client credentials and
// in-process control, the funding readiness check, and then finish with the
// pass's tunnel configuration and the URL probe's control-plane endpoints.
// The shard is drained and reaped when finish returns, after every tunnel
// has joined; capabilityProbeShardPassResult says what the pass returns.
func runCapabilityProbeShard(
	ctx context.Context,
	key model.ProberShardKey,
	settings capprobe.Settings,
	sweep capprobe.Sweep,
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
	outcome, err := runWithProberShard(ctx, "capability probe", key, credit, settings.MaxTime(), func(identity *model.ProberIdentity) (capabilityProbeOutcome, error) {
		credentials, err := newProviderEgressCredentials(identity)
		if err != nil {
			return capabilityProbeOutcome{}, err
		}
		return runWithProviderEgressControl(ctx, credentials, func(localControl connect.NetworkClientControl) (capabilityProbeOutcome, error) {
			if err := newProviderEgressProbeReadiness(*identity.NetworkId).check(ctx); err != nil {
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
// and failed. Every cleanup failure is counted apart and logged. Any other
// failure leaves the sweep where it was, counted once: finish counts its
// own, this the ones before it.
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
	default:
		if !finished {
			capprobe.ObservePass(capprobe.PassResultFailed)
		}
		return sweep, outcome.pass, err
	}
}
