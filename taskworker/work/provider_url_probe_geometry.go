// Taskworkers run different config versions during a rollout, and a block
// started before its host received a new config keeps the older one until its
// next deploy. Each shard's arguments name the config version that wrote them
// and when. An instance whose config is older than a newer, live geometry
// hands that shard back unchanged, instead of retiring a shard it does not
// know or rewriting the arguments to its own older geometry, and its startup
// reap does not disable a geometry it is older than. Versions are ordered as
// warp selects the latest config: release, then build metadata. A process
// without a config version orders nothing and keeps the previous behavior.
package work

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/coreos/go-semver/semver"

	"github.com/urnetwork/server"
)

// A newer config's geometry that no instance has rewritten for this long is
// treated as withdrawn, for example by redeploying an older config, and older
// instances converge it to their own settings again. Instances on the newer
// config rewrite a shard's arguments at the end of each of its passes, so in a
// rollout a live newer geometry is far younger than this.
const providerEgressProbeNewerGenerationTimeout = 30 * time.Minute

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

// Whether a snapshot stamped with generation at writtenAt belongs to a config
// newer than this process's that instances still rewrite.
func providerEgressProbeGenerationLive(generation string, writtenAt time.Time, now time.Time) bool {
	return providerEgressProbeGenerationNewer(generation, providerEgressProbeConfigVersion()) &&
		!writtenAt.IsZero() && now.Sub(writtenAt) < providerEgressProbeNewerGenerationTimeout
}

// A copy of args written by this process at now.
func stampProviderEgressProbeArgs(args *ProviderEgressProbeArgs, now time.Time) *ProviderEgressProbeArgs {
	stamped := *args
	stamped.GeometryGeneration = providerEgressProbeConfigVersion()
	stamped.GeometryWrittenAt = now.UTC()
	return &stamped
}

// An older config hands back a newer, live snapshot that it would otherwise
// retire or rewrite, including when its own config disables probing. A newer
// snapshot of this instance's own geometry still runs here.
func providerEgressProbeHandsBack(args *ProviderEgressProbeArgs, settings providerEgressProbeSettings, now time.Time) bool {
	if args == nil || !providerEgressProbeGenerationLive(args.GeometryGeneration, args.GeometryWrittenAt, now) {
		return false
	}
	return !settings.Enabled || args.ShardIndex < 0 || settings.ShardCount <= args.ShardIndex ||
		!providerEgressProbeArgsMatchSettings(args, settings)
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
			if json.Unmarshal([]byte(argsJson), &args) == nil &&
				providerEgressProbeGenerationLive(args.GeometryGeneration, args.GeometryWrittenAt, now) {
				newerGenerationLive = true
			}
		}
	})
	return
}
