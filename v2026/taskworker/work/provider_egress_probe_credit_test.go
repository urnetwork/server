package work

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

func TestProviderEgressProbeCreditMinimumCoversConcurrentShards(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("probe.example")
	settings.ShardCount = 4
	settings.Full.Limit = 8
	settings.Blackhole.Limit = 1000
	args := providerEgressProbeArgs(settings, 0)
	got, err := providerEgressProbeCreditMinimum(args)
	if err != nil {
		t.Fatal(err)
	}
	if floor := model.ByteCount(4) * model.ProberShardTransferHeadroom; got < floor {
		t.Fatalf("minimum credit=%d, below four independent shard headrooms=%d", got, floor)
	}

	// A much larger full cohort must cover current, announced-ahead and
	// prefetched contracts, in both directions, for every tunnel generation.
	args.Full.Limit = 2000
	got, err = providerEgressProbeCreditMinimum(args)
	if err != nil {
		t.Fatal(err)
	}
	full := int64(connect.DefaultContractManagerSettings().StandardContractTransferByteCount)
	want := model.ByteCount(4 * int64(1+args.TunnelRecreateAttempts) * 6 * (2000*full + providerEgressBlackholeSelectedCohorts*1000*(1024*1024)))
	if got != want {
		t.Fatalf("minimum credit=%d, want planned four-shard two-way exposure=%d", got, want)
	}
}

func TestProviderEgressProbeCreditMinimumCoversAdmittedCohorts(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("probe.example")
	args := providerEgressProbeArgs(settings, 0)
	args.ShardCount = 2
	args.Blackhole.Limit = 2500
	args.Blackhole.Concurrency = 20000
	args.Full.Limit = 8
	args.TunnelRecreateAttempts = 4
	got, err := providerEgressProbeCreditMinimum(args)
	if err != nil {
		t.Fatal(err)
	}
	full := int64(connect.DefaultContractManagerSettings().StandardContractTransferByteCount)
	want := model.ByteCount(2 * 5 * 6 * (8*full + 8*2500*1024*1024))
	if got != want || got <= 2*model.ProberShardTransferHeadroom {
		t.Fatalf("minimum=%d, want concurrent cohorts and lingering tunnel generations=%d", got, want)
	}
}

func TestProviderEgressProbeCreditMinimumRejectsOverflow(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("probe.example")
	args := providerEgressProbeArgs(settings, 0)
	args.Full.Limit = math.MaxInt
	if _, err := providerEgressProbeCreditMinimum(args); err == nil {
		t.Fatal("oversized selected cohort accepted")
	}
	args.Full.Limit = 1
	args.Blackhole.Limit = math.MaxInt
	if _, err := providerEgressProbeCreditMinimum(args); err == nil {
		t.Fatal("oversized blackhole cohort accepted")
	}
	args.Blackhole.Limit = 1
	args.TunnelRecreateAttempts = math.MaxInt
	if _, err := providerEgressProbeCreditMinimum(args); err == nil {
		t.Fatal("overflowing tunnel generation count accepted")
	}
	args.TunnelRecreateAttempts = 1
	args.ShardCount = 1
	args.Full.Limit = 5_000_000_000
	args.Blackhole.Limit = 12_000_000_000
	if _, err := providerEgressProbeCreditMinimum(args); err == nil {
		t.Fatal("overflowing sum of individually representable lane reserves accepted")
	}
	args.Full.Limit, args.Blackhole.Limit = 1, 1
	args.ShardCount = math.MaxInt
	if _, err := providerEgressProbeCreditMinimum(args); err == nil {
		t.Fatal("overflowing combined shard headroom accepted")
	}
}

// Publication granularity is execution-local; it never reduces the worker
// pool, changes full/retry policy, or rewrites the durable scheduler input.
func TestProviderEgressProbeExecutionArgsPreserveOwnerSettings(t *testing.T) {
	for _, c := range []struct {
		selected, workers, guard, want int
	}{
		{selected: 1000, workers: 1000, guard: 10, want: 250},
		{selected: 128, workers: 1000, guard: 10, want: 128},
		{selected: 1000, workers: 250, guard: 10, want: 1000},
		{selected: 1000, workers: 1000, guard: 400, want: 400},
		{selected: 1000, workers: 2401, guard: 10, want: 250},
	} {
		args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("probe.example"), 0)
		args.Blackhole.Limit, args.Blackhole.Concurrency = c.selected, c.workers
		args.DarkBatchGuardMinChecks = c.guard
		stored := *args
		execution := providerEgressProbeExecutionArgs(args)
		if !reflect.DeepEqual(args, &stored) {
			t.Fatalf("%+v: durable args changed", c)
		}
		want := stored
		want.Blackhole.Limit = c.want
		if !reflect.DeepEqual(execution, &want) || !reflect.DeepEqual(providerEgressProbeExecutionArgs(execution), execution) {
			t.Fatalf("%+v: execution settings changed beyond idempotent cohort normalization", c)
		}
		if (execution.Blackhole.Concurrency-1)/providerEgressBlackholeSelectedCohorts >= execution.Blackhole.Limit {
			t.Fatalf("%+v: normalized cohort cannot admit the configured workers", c)
		}
	}
}

func TestProviderEgressProbeCreditMinimumRejectsInvalidGeometry(t *testing.T) {
	if _, err := providerEgressProbeCreditMinimum(nil); err == nil {
		t.Fatal("nil geometry accepted")
	}
	for _, field := range []string{"shards", "full", "blackhole", "full_workers", "blackhole_workers", "recreates"} {
		args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("probe.example"), 0)
		switch field {
		case "shards":
			args.ShardCount = 0
		case "full":
			args.Full.Limit = 0
		case "blackhole":
			args.Blackhole.Limit = 0
		case "full_workers":
			args.Full.Concurrency = 0
		case "blackhole_workers":
			args.Blackhole.Concurrency = 0
		case "recreates":
			args.TunnelRecreateAttempts = 0
		}
		if _, err := providerEgressProbeCreditMinimum(args); err == nil {
			t.Errorf("invalid %s accepted", field)
		}
	}
}

// The compatibility path has no bounded multi-cohort pipeline. Do not reduce
// its selected work or effective concurrency when no owner deadline exists.
func TestProviderEgressProbeExecutionCohortsRequireBoundedOwner(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("probe.example"), 0)
	args.Blackhole.Limit, args.Blackhole.Concurrency = 1000, 1000
	selected := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(_ context.Context, limit int) ([]ingest.DueProvider, error) { selected = limit; return nil, nil },
		fullDue:      func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil },
	}
	if _, err := pass.run(context.Background(), args); err != nil || selected != 1000 {
		t.Fatalf("unbounded compatibility selection=%d error=%v", selected, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, err := pass.run(ctx, args); err != nil || selected != 250 {
		t.Fatalf("bounded publication selection=%d error=%v", selected, err)
	}
}
