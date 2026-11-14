//go:build sidecar_pending

package quotahost

// FS-60's count clause (§20.7's count row, FI-13 (c); QUOTA-M2-13) on T3: the nightly pass's sums written
// through model as taskworker's closing job writes them (server/taskworker/evs_nightly.go: an absolute
// SetNetworkStoredByteCount for each network a sum names, and zero for each network holding a count that no
// sum names), with FI-13's oracle reading network_quota_state against the listing: the seg/, pack/ and hp/
// objects' sizes plus the lengths of records still in batches. Usage batches' deltas move the count between
// passes, once each, and the next pass's write replaces whatever they left; a container the pass deleted
// counts no more; the storage refusal follows the count.
//
// The sums here are a night's evsservice.NightlyBytes given by hand: the pass that makes them from LISTs and
// head windows runs on T2 in sidecartest/fault's fs60_nightly_test.go, and its run on the real object store
// is EVSB's.

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The closing job's writes for one night's sums.
func fs60T3CloseNight(t *testing.T, ctx context.Context, networkBytes map[connect.Id]*evsservice.NightlyBytes) {
	t.Helper()
	for networkId, stored := range networkBytes {
		if err := model.SetNetworkStoredByteCount(ctx, server.Id(networkId), stored.StoredByteCount()); err != nil {
			t.Fatal(err)
		}
	}
	var after *server.Id
	for {
		networkIds, err := model.NetworkIdsWithStoredByteCount(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(networkIds) == 0 {
			return
		}
		for _, networkId := range networkIds {
			if _, ok := networkBytes[connect.Id(networkId)]; !ok {
				if err := model.SetNetworkStoredByteCount(ctx, networkId, 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		after = &networkIds[len(networkIds)-1]
	}
}

func fs60T3Listing(stored *evsservice.NightlyBytes) *sidecartest.QuotaStoredListing {
	return &sidecartest.QuotaStoredListing{
		SegmentByteCount:     stored.SegmentByteCount,
		PackByteCount:        stored.PackByteCount,
		ParcelByteCount:      stored.ParcelByteCount,
		BatchRecordByteCount: stored.BatchRecordByteCount,
	}
}

func TestFs60T3NightlyCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"storage": 10000})()
		ctx := t.Context()
		target := newT3Target(ctx)(t)
		oracle := newT3Oracle(ctx)
		checkStored := func(what string, networkId connect.Id, listing *sidecartest.QuotaStoredListing) {
			t.Helper()
			if err := oracle.CheckStored(ctx, networkId, listing); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		storageRefused := func(networkId connect.Id) bool {
			t.Helper()
			overLimit, err := target.Quota.OverLimit(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return overLimit.StorageNetworkIdRefusals[networkId] != nil
		}

		networkIds := []connect.Id{}
		for range 3 {
			networkId := connect.NewId()
			if err := target.SetBalances(networkId, []*sidecartest.QuotaBalance{{ByteCount: 1 << 30}}); err != nil {
				t.Fatal(err)
			}
			networkIds = append(networkIds, networkId)
		}
		kept, emptied, full := networkIds[0], networkIds[1], networkIds[2]

		// the first night: LIST sizes plus batch-resident lengths, for each network
		first := map[connect.Id]*evsservice.NightlyBytes{
			kept:    {SegmentByteCount: 4000, PackByteCount: 2500, ParcelByteCount: 300, BatchRecordByteCount: 120, RepoCount: 2},
			emptied: {SegmentByteCount: 900, BatchRecordByteCount: 50, RepoCount: 1},
			full:    {SegmentByteCount: 9000, PackByteCount: 1000, RepoCount: 1},
		}
		fs60T3CloseNight(t, ctx, first)
		for networkId, stored := range first {
			checkStored("the first night", networkId, fs60T3Listing(stored))
		}
		if storageRefused(kept) || !storageRefused(full) {
			t.Fatal("the storage refusal did not follow the first night's counts")
		}
		// the job rerun writes the same counts
		fs60T3CloseNight(t, ctx, first)
		checkStored("the first night rerun", kept, fs60T3Listing(first[kept]))

		// between passes a usage batch's delta moves the count, once however often it arrives
		batch := &sidecar.UsageBatch{
			BatchId:  connect.NewId(),
			Producer: sidecar.QuotaProducerEvs,
			Events:   []*sidecar.UsageEvent{{NetworkId: kept, Meter: sidecar.QuotaMeterEvs, StoredByteDelta: 700}},
		}
		for range 2 {
			if err := target.Quota.ReportUsage(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}
		moved := fs60T3Listing(first[kept])
		moved.SegmentByteCount += 700
		checkStored("after the day's delta", kept, moved)

		// the second night: the new segment's 700 listed with 60 the delta never named, a pack deleted once
		// its records moved, the batch-resident record now in a pack; one network's repos gone; one under
		// its limit again
		second := map[connect.Id]*evsservice.NightlyBytes{
			kept: {SegmentByteCount: 4760, PackByteCount: 820, ParcelByteCount: 300, RepoCount: 2},
			full: {SegmentByteCount: 9000, RepoCount: 1},
		}
		fs60T3CloseNight(t, ctx, second)
		for networkId, stored := range second {
			checkStored("the second night", networkId, fs60T3Listing(stored))
		}
		checkStored("a network no sum names", emptied, &sidecartest.QuotaStoredListing{})
		if storageRefused(full) {
			t.Fatal("the storage refusal outlived the count that made it")
		}
		left, err := model.NetworkIdsWithStoredByteCount(ctx, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 2 {
			t.Fatalf("%d networks hold a count after the second night, want the two its sums name", len(left))
		}
	})
}
