// A URL probe shard's arguments carry the config stamp of the taskworker that
// wrote them (config_generation.go). An instance whose config is older than a
// newer, live geometry hands that shard back unchanged, instead of retiring a
// shard it does not know or rewriting the arguments to its own older
// geometry. It also leaves the shard set to that geometry: its startup reap
// does not disable it, and neither its startup schedule nor its post-steps
// create shards.
package work

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
)

// The bounds of providerEgressProbeNewerGenerationTimeout.
const (
	providerEgressProbeNewerGenerationMinTimeout = 30 * time.Minute
	providerEgressProbeNewerGenerationMaxTimeout = 24 * time.Hour
)

// How long after its last write a newer config's geometry still counts as
// live, from that config's own arguments. Its instances restamp a shard at the
// end of each pass, and a pass ends within its max time, so while one of them
// holds any pass, some shard of the geometry is restamped at least once per
// max time and idle delay. Twice that covers claiming the next pass, a claim
// handed back in between, and a restart of the instance. The minimum covers a
// restart under a short max time, and the maximum bounds how long a withdrawn
// geometry, for example after an older config is redeployed, keeps older
// instances from converging it. One shard's stamp can age far longer while
// its config is live: older instances hand the shard back without restamping
// it while the newer instances' passes are all busy. Liveness is therefore
// judged across the geometry's shards before a shard is converged.
func providerEgressProbeNewerGenerationTimeout(args *ProviderEgressProbeArgs) time.Duration {
	cycle := time.Duration(max(0, args.MaxTimeSeconds)+max(0, args.IdleDelaySeconds)) * time.Second
	return min(max(providerEgressProbeNewerGenerationMinTimeout, 2*cycle), providerEgressProbeNewerGenerationMaxTimeout)
}

// Whether a snapshot was written by a newer config than this process's within
// that config's timeout.
func providerEgressProbeStampLive(args *ProviderEgressProbeArgs, now time.Time) bool {
	return args != nil && args.ConfigStamp.newerWithin(now, providerEgressProbeNewerGenerationTimeout(args))
}

// A copy of args written by this process at now.
func stampProviderEgressProbeArgs(args *ProviderEgressProbeArgs, now time.Time) *ProviderEgressProbeArgs {
	stamped := *args
	stamped.ConfigStamp = newConfigStamp(now)
	return &stamped
}

// Whether a snapshot comes from a newer config than this process's and this
// instance would retire or rewrite it, including because its own config
// disables probing. A newer snapshot of this instance's own geometry still
// runs here.
func providerEgressProbeNewerForeignGeometry(args *ProviderEgressProbeArgs, settings providerEgressProbeSettings) bool {
	if args == nil || !configGenerationNewer(args.ConfigGeneration, currentConfigGeneration()) {
		return false
	}
	return !settings.Enabled || args.ShardIndex < 0 || settings.ShardCount <= args.ShardIndex ||
		!providerEgressProbeArgsMatchSettings(args, settings)
}

// An older config hands back a newer config's snapshot that it would retire
// or rewrite while that config is live, by the shard's own stamp or by the
// stamp of any other shard of either geometry. Only a stale own stamp costs a
// read of the shard rows. A config that still enables probing also hands back
// its own snapshots while a newer config's disabled marker is live, since that
// config writes no shard rows to show it.
func providerEgressProbeHandsBack(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	settings providerEgressProbeSettings,
	now time.Time,
) bool {
	// Nil arguments name no shard; validation fails them.
	if args == nil {
		return false
	}
	if !providerEgressProbeNewerForeignGeometry(args, settings) {
		return settings.Enabled && providerEgressProbeDisabledByNewerConfig(ctx, now)
	}
	if providerEgressProbeStampLive(args, now) {
		return true
	}
	shardCount := min(max(args.ShardCount, settings.ShardCount, 0), maxProviderEgressProbeShardCount)
	newerGenerationLive := false
	server.Tx(ctx, func(tx server.PgTx) {
		_, newerGenerationLive = readProviderEgressProbeShardRowsInTx(ctx, tx, shardCount, now)
	}, server.TxReadCommitted)
	return newerGenerationLive
}

// The pending rows of the first shardCount shard indexes, read as a plain
// snapshot that takes no lock and waits on no transaction: the indexes that
// have a row, and whether any row carries a newer config's live geometry, or
// a newer config's live disabled marker shows it retired the shard set. A row
// whose arguments do not decode counts as present and not newer.
func readProviderEgressProbeShardRowsInTx(
	ctx context.Context,
	tx server.PgTx,
	shardCount int,
	now time.Time,
) (presentShardIndexes map[int]bool, newerGenerationLive bool) {
	runOnceKeyShardIndexes := make(map[string]int, shardCount)
	runOnceKeys := make([]string, 0, shardCount)
	for shardIndex := range shardCount {
		runOnceKey := providerEgressProbeRunOnce(shardIndex).String()
		runOnceKeyShardIndexes[runOnceKey] = shardIndex
		runOnceKeys = append(runOnceKeys, runOnceKey)
	}
	presentShardIndexes = map[int]bool{}
	result, err := tx.Query(
		ctx,
		`SELECT run_once_key, args_json FROM pending_task WHERE run_once_key = ANY($1)`,
		runOnceKeys,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var runOnceKey string
			var argsJson string
			server.Raise(result.Scan(&runOnceKey, &argsJson))
			presentShardIndexes[runOnceKeyShardIndexes[runOnceKey]] = true
			var args ProviderEgressProbeArgs
			if json.Unmarshal([]byte(argsJson), &args) == nil && providerEgressProbeStampLive(&args, now) {
				newerGenerationLive = true
			}
		}
	})
	if providerEgressProbeDisabledMarkerLiveInTx(ctx, tx, now) {
		newerGenerationLive = true
	}
	return
}
