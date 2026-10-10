// URL probes have one worker pool and one request per provider turn.
package work

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
)

func providerUrlProbeBatch(args *ProviderEgressProbeArgs) ProviderEgressProbeBatchArgs {
	if args.UrlProbe != nil {
		return *args.UrlProbe
	}
	return args.Full
}

func providerUrlProbeBatchMatches(left, right *ProviderEgressProbeBatchArgs) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// One URL has one attempt. Failed measured requests are requeued durably;
// the worker never retains its tunnel for a retry timer.
func providerUrlProbeRunBudget(args *ProviderEgressProbeArgs) time.Duration {
	batch := providerUrlProbeBatch(args)
	timeout := time.Duration(batch.ProbeTimeoutSeconds) * time.Second
	options := fleetprobe.EgressHealthOptions(timeout, false)
	options.UrlProbe = true
	options.LoadAttempts = 1
	options.TunnelRecreateAttempts = args.TunnelRecreateAttempts
	return timeout + options.RunBudget(1)
}

func validateProviderUrlProbeArgs(args *ProviderEgressProbeArgs) error {
	if args.ShardCount < 1 || maxProviderEgressProbeShardCount < args.ShardCount {
		return fmt.Errorf("URL probe shard_count must be in [1,%d]", maxProviderEgressProbeShardCount)
	}
	if args.IdleDelaySeconds < 1 || args.MaxTimeSeconds < 1 {
		return fmt.Errorf("URL probe idle delay and max time must be positive")
	}
	if strings.TrimSpace(args.APIURL) == "" || strings.TrimSpace(args.PlatformURL) == "" {
		return fmt.Errorf("URL probe api_url and platform_url are required")
	}
	batch := providerUrlProbeBatch(args)
	if err := validateProviderEgressProbeBatchArgs("url_probe", batch); err != nil {
		return err
	}
	if batch.AllDestinations || batch.Bandwidth || batch.IpEchoTimeoutSeconds != 0 {
		return fmt.Errorf("URL probes accept only one randomized URL per turn")
	}
	if batch.ProbeTimeoutSeconds < int(egresshealth.DefaultPerRequestTimeout/time.Second) ||
		batch.ProbeTimeoutSeconds >= int(model.ProviderEgressProbeAttemptBackoff/time.Second) ||
		args.TunnelRecreateAttempts < 1 || args.TunnelRecreateAttempts > int(model.ProviderEgressProbeAttemptBackoff/egresshealth.DefaultPerRequestTimeout) {
		return fmt.Errorf("URL probe request timeout or tunnel recreation bound is invalid")
	}
	if err := args.ProviderEgressRules.Validate(); err != nil {
		return err
	}
	minimum := providerUrlProbeRunBudget(args) + 3*providerEgressControlPlaneTimeout
	if minimum >= model.ProviderEgressProbeAttemptBackoff {
		return fmt.Errorf("URL probe turn and publication must finish before its durable claim expires")
	}
	if time.Duration(args.MaxTimeSeconds)*time.Second < minimum {
		return fmt.Errorf("URL probe max_time_seconds must cover one request and publication (%d)", int64(minimum/time.Second))
	}
	return nil
}

// Plan credit only for the active URL pool, including announced/prefetched
// contracts in both directions and possible tunnel recreations. Old cheap
// cohort settings cannot reserve credit or change URL admission.
func providerUrlProbeCreditMinimum(args *ProviderEgressProbeArgs) (model.ByteCount, error) {
	if args == nil || args.ShardCount < 1 || args.TunnelRecreateAttempts < 1 || args.TunnelRecreateAttempts == math.MaxInt {
		return 0, fmt.Errorf("invalid URL probe credit geometry")
	}
	batch := providerUrlProbeBatch(args)
	if batch.Limit < 1 || batch.Concurrency < 1 {
		return 0, fmt.Errorf("invalid URL probe credit geometry")
	}
	amount := int64(connect.DefaultContractManagerSettings().StandardContractTransferByteCount)
	for _, factor := range []int64{int64(batch.Concurrency), 6, int64(args.TunnelRecreateAttempts) + 1} {
		if factor < 1 || amount > math.MaxInt64/factor {
			return 0, fmt.Errorf("URL probe credit geometry overflows")
		}
		amount *= factor
	}
	amount = max(amount, int64(model.ProberShardTransferHeadroom))
	if amount > math.MaxInt64/int64(args.ShardCount) {
		return 0, fmt.Errorf("URL probe credit geometry overflows")
	}
	return model.ByteCount(amount * int64(args.ShardCount)), nil
}

const providerUrlProbeFundingMultiplier = int64(10)

// One private allocation funds at least ten times the full pass's anticipated
// contract exposure, without assuming any reclamation before the pass ends.
// Each direction reserves the whole initial-to-standard renewal ramp plus
// current, announced-ahead and prefetched standard contracts. Charging every
// slot at the standard ceiling is deliberately more conservative than the URL
// body cap or the actual ramp. Include concurrent headroom and every possible
// tunnel generation; no old generation's unused reservation is borrowed back.
// This is not multiplied by ShardCount or replenished from the shared account.
// Unexpected extra consumption fails normal available-credit admission rather
// than silently creating another grant or borrowing another shard's credit.
func providerUrlProbeShardCredit(args *ProviderEgressProbeArgs) (model.ByteCount, error) {
	if _, err := providerUrlProbeCreditMinimum(args); err != nil {
		return 0, err
	}
	batch := providerUrlProbeBatch(args)
	contracts := connect.DefaultContractManagerSettings()
	if int64(batch.Concurrency) > math.MaxInt64-int64(providerEgressFullSelectedLimit) ||
		contracts.ContractTransferByteSeqScale > uint64(math.MaxInt64-3) {
		return 0, fmt.Errorf("URL probe pass credit geometry overflows")
	}
	amount := int64(contracts.StandardContractTransferByteCount)
	for _, factor := range []int64{
		int64(providerEgressFullSelectedLimit) + int64(batch.Concurrency),
		2, // origin and companion both charge the private payer
		int64(contracts.ContractTransferByteSeqScale) + 3,
		int64(args.TunnelRecreateAttempts) + 1,
		providerUrlProbeFundingMultiplier,
	} {
		if amount < 1 || factor < 1 || amount > math.MaxInt64/factor {
			return 0, fmt.Errorf("URL probe pass credit geometry overflows")
		}
		amount *= factor
	}
	return model.ByteCount(max(amount, int64(model.ProberShardTransferHeadroom))), nil
}
