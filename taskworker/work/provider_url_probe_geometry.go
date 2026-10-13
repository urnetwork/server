// Taskworkers run different config versions during a rollout, and a block
// started before its host received a new config keeps the older one until its
// next deploy. Each shard's arguments name the config version that wrote them
// and when. An instance whose config is older than a newer, live geometry
// hands that shard back unchanged, instead of retiring a shard it does not
// know or rewriting the arguments to its own older geometry. It also leaves
// the shard set to that geometry: its startup reap does not disable it, and
// neither its startup schedule nor its post-steps create shards. Versions are
// ordered as warp selects the latest config: release, then build metadata. A
// process without a config version orders nothing and keeps the previous
// behavior.
package work

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/coreos/go-semver/semver"

	"github.com/urnetwork/server"
)

// The bounds of providerEgressProbeNewerGenerationTimeout.
const (
	providerEgressProbeNewerGenerationMinTimeout = 30 * time.Minute
	providerEgressProbeNewerGenerationMaxTimeout = 24 * time.Hour
)

// The config version of this process. It is read directly rather than through
// server.ConfigVersion, which falls back to the service version: only config
// versions order geometries.
var providerEgressProbeConfigVersion = func() string {
	return os.Getenv("WARP_CONFIG_VERSION")
}

// Whether candidate is a strictly newer config version than reference. An
// empty or unparsable version on either side has no order.
func providerEgressProbeGenerationNewer(candidate string, reference string) bool {
	if candidate == "" || reference == "" {
		return false
	}
	candidateVersion, err := semver.NewVersion(candidate)
	if err != nil {
		return false
	}
	referenceVersion, err := semver.NewVersion(reference)
	if err != nil {
		return false
	}
	if candidateVersion.Major == referenceVersion.Major && candidateVersion.Minor == referenceVersion.Minor &&
		candidateVersion.Patch == referenceVersion.Patch {
		return referenceVersion.Metadata < candidateVersion.Metadata
	}
	return referenceVersion.LessThan(*candidateVersion)
}

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
	return args != nil && providerEgressProbeGenerationNewer(args.GeometryGeneration, providerEgressProbeConfigVersion()) &&
		!args.GeometryWrittenAt.IsZero() && now.Sub(args.GeometryWrittenAt) < providerEgressProbeNewerGenerationTimeout(args)
}

// A copy of args written by this process at now.
func stampProviderEgressProbeArgs(args *ProviderEgressProbeArgs, now time.Time) *ProviderEgressProbeArgs {
	stamped := *args
	stamped.GeometryGeneration = providerEgressProbeConfigVersion()
	stamped.GeometryWrittenAt = now.UTC()
	return &stamped
}

// Whether a snapshot comes from a newer config than this process's and this
// instance would retire or rewrite it, including because its own config
// disables probing. A newer snapshot of this instance's own geometry still
// runs here.
func providerEgressProbeNewerForeignGeometry(args *ProviderEgressProbeArgs, settings providerEgressProbeSettings) bool {
	if args == nil || !providerEgressProbeGenerationNewer(args.GeometryGeneration, providerEgressProbeConfigVersion()) {
		return false
	}
	return !settings.Enabled || args.ShardIndex < 0 || settings.ShardCount <= args.ShardIndex ||
		!providerEgressProbeArgsMatchSettings(args, settings)
}

// An older config hands back a newer config's snapshot that it would retire
// or rewrite while that config is live, by the shard's own stamp or by the
// stamp of any other shard of either geometry. Only a stale own stamp costs a
// read of the shard rows.
func providerEgressProbeHandsBack(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	settings providerEgressProbeSettings,
	now time.Time,
) bool {
	if !providerEgressProbeNewerForeignGeometry(args, settings) {
		return false
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
// have a row, and whether any row carries a newer config's live geometry. A
// row whose arguments do not decode counts as present and not newer.
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
	return
}
