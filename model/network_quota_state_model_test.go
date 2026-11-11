//go:build sidecar_pending

package model

// QUOTA-M2-04: network_quota_state's stored count, storage limit, spent marks and Quota.OverLimit's
// answer, under -tags sidecar_pending through .stack/server-test.sh, since the table is MG2's.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

func testingNetworkQuotaState(t testing.TB, ctx context.Context, networkId server.Id) *NetworkQuotaState {
	state, err := GetNetworkQuotaState(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatalf("no quota state for %s", networkId)
	}
	return state
}

func testingApplyStoredByteDelta(t testing.TB, ctx context.Context, networkId server.Id, storedByteDelta int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		if err := ApplyStoredByteDeltaInTx(ctx, tx, networkId, storedByteDelta); err != nil {
			t.Fatal(err)
		}
	})
}

func testingOverLimit(t testing.TB, ctx context.Context) *sidecar.QuotaOverLimit {
	overLimit, err := GetQuotaOverLimit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return overLimit
}

// A service debit of byteCount against a balance, journaled and not yet applied.
func testingJournalServiceDebit(ctx context.Context, balanceId server.Id, meter string, byteCount int64) server.Id {
	usageId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO service_debit_journal (usage_id, balance_id, meter, debit_byte_count, batch_id)
				VALUES ($1, $2, $3, $4, $5)
			`,
			usageId,
			balanceId,
			meter,
			byteCount,
			server.NewId(),
		))
	})
	return usageId
}

// The network's one balance, by id.
func testingNetworkBalanceIds(ctx context.Context, networkId server.Id) (balanceIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT balance_id FROM transfer_balance WHERE network_id = $1 ORDER BY balance_id`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})
	})
	return
}

// One byte under the limit is not over; the limit itself and one byte past are, refused by evs with
// the limit, the count and whether a higher tier lifts it.
func TestNetworkStorageOverAtTheExactLimit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1000})()
		defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"storage": 20000})()
		under := server.NewId()
		at := server.NewId()
		// a network row, which a limit row needs
		past := testingSidecarLimitNetwork(t, ctx)
		for networkId, storedByteCount := range map[server.Id]int64{under: 999, at: 1000, past: 1001} {
			connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, storedByteCount), nil)
		}
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, under).StorageOver, false)
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, at).StorageOver, true)
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, past).StorageOver, true)

		overLimit := testingOverLimit(t, ctx)
		_, ok := overLimit.StorageNetworkIdRefusals[connect.Id(under)]
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, *overLimit.StorageNetworkIdRefusals[connect.Id(at)], sidecar.QuotaRefusal{LimitKey: "storage", Limit: 1000, Used: 1000, Upgrade: true})
		connect.AssertEqual(t, *overLimit.StorageNetworkIdRefusals[connect.Id(past)], sidecar.QuotaRefusal{LimitKey: "storage", Limit: 1000, Used: 1001, Upgrade: true})
		// over storage with balance is not spent (SOCK reading 9)
		connect.AssertEqual(t, len(overLimit.SpentNetworkIdRefusals), 0)

		// a row holds whatever the tier, so no tier lifts it
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, past, "storage", 500, "ops change 1"), nil)
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, past, 1001), nil)
		overLimit = testingOverLimit(t, ctx)
		connect.AssertEqual(t, *overLimit.StorageNetworkIdRefusals[connect.Id(past)], sidecar.QuotaRefusal{LimitKey: "storage", Limit: 500, Used: 1001, Upgrade: false})
	})
}

// While limits ship dark the storage map stays empty and the spent map does not (§20.5).
func TestQuotaOverLimitDarkKeepsSpent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1000})()
		over := server.NewId()
		spent := server.NewId()
		both := server.NewId()
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, over, 2000), nil)
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, both, 2000), nil)
		now := server.NowUtc()
		connect.AssertEqual(t, MarkNetworkSpent(ctx, spent, now), nil)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, both, now), nil)

		restore := Testing_SetEnforceSidecarLimits(false)
		overLimit := testingOverLimit(t, ctx)
		restore()
		connect.AssertEqual(t, len(overLimit.StorageNetworkIdRefusals), 0)
		connect.AssertEqual(t, len(overLimit.SpentNetworkIdRefusals), 2)
		connect.AssertEqual(t, *overLimit.SpentNetworkIdRefusals[connect.Id(spent)], sidecar.QuotaRefusal{LimitKey: "data", Upgrade: true})
		connect.AssertEqual(t, overLimit.SpentNetworkIdRefusals[connect.Id(both)] != nil, true)

		defer Testing_SetEnforceSidecarLimits(true)()
		overLimit = testingOverLimit(t, ctx)
		connect.AssertEqual(t, len(overLimit.StorageNetworkIdRefusals), 2)
		connect.AssertEqual(t, len(overLimit.SpentNetworkIdRefusals), 2)
		_, ok := overLimit.SpentNetworkIdRefusals[connect.Id(over)]
		connect.AssertEqual(t, ok, false)
	})
}

// A delta moves the count between passes and storage_over with it; one below zero clamps there and
// counts the drift.
func TestStoredByteDeltaClampsAndCountsTheDrift(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1000})()
		networkId := server.NewId()
		testingApplyStoredByteDelta(t, ctx, networkId, 600)
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, networkId), NetworkQuotaState{NetworkId: networkId, StoredByteCount: 600})
		testingApplyStoredByteDelta(t, ctx, networkId, 500)
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, networkId), NetworkQuotaState{NetworkId: networkId, StoredByteCount: 1100, StorageOver: true})

		driftBefore := testutil.ToFloat64(quotaStoredDriftByteCounter)
		testingApplyStoredByteDelta(t, ctx, networkId, -1400)
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, networkId), NetworkQuotaState{NetworkId: networkId, StoredByteCount: 0})
		connect.AssertEqual(t, testutil.ToFloat64(quotaStoredDriftByteCounter)-driftBefore, float64(300))
	})
}

// FS-47's "the nightly count corrects an injected drift", and FS-60's api half: what a nightly run writes
// is what the network then reports, and storage_over follows it.
func TestNightlyCountCorrectsAnInjectedDrift(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1000})()
		networkId := server.NewId()
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, 900), nil)
		// a freed object reported twice: the count drifts below the bytes stored
		testingApplyStoredByteDelta(t, ctx, networkId, -300)
		testingApplyStoredByteDelta(t, ctx, networkId, -300)
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, networkId).StoredByteCount, int64(300))
		// the pass lists 600 stored, and an upload of 450 lands after it
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, 600), nil)
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, networkId).StoredByteCount, int64(600))
		testingApplyStoredByteDelta(t, ctx, networkId, 450)
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, networkId), NetworkQuotaState{NetworkId: networkId, StoredByteCount: 1050, StorageOver: true})
		// the next pass lists 990: under the limit again
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, 990), nil)
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, networkId), NetworkQuotaState{NetworkId: networkId, StoredByteCount: 990})
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, -1) != nil, true)
	})
}

// The closing job's zero write reads every network holding a count above zero, a page at a time in id order: a
// count of zero, and a row that only a spent mark made, are not listed (owner decision 198).
func TestNetworkIdsWithStoredByteCountPagesThroughTheCounts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		counted := []server.Id{}
		for i := range 5 {
			networkId := server.NewId()
			connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, int64(100+i)), nil)
			counted = append(counted, networkId)
		}
		// written back to zero by an earlier night, and marked spent with no count
		zeroed := server.NewId()
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, zeroed, 10), nil)
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, zeroed, 0), nil)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, server.NewId(), server.NowUtc()), nil)

		listed := []server.Id{}
		var after *server.Id
		pageCount := 0
		for {
			page, err := NetworkIdsWithStoredByteCount(ctx, after, 2)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(page) <= 2, true)
			pageCount += 1
			if len(page) == 0 {
				break
			}
			listed = append(listed, page...)
			last := page[len(page)-1]
			after = &last
		}
		slices.SortFunc(counted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, listed, counted)
		connect.AssertEqual(t, pageCount, 4)

		_, err := NetworkIdsWithStoredByteCount(ctx, nil, 0)
		connect.AssertEqual(t, err != nil, true)
	})
}

// A spent mark keeps its earliest time however often it is marked again.
func TestMarkNetworkSpentKeepsTheEarliest(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		first := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, networkId, first.Add(time.Minute)), nil)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, networkId, first), nil)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, networkId, first.Add(time.Hour)), nil)
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, MarkNetworkSpentInTx(ctx, tx, networkId, first.Add(2*time.Hour)), nil)
		})
		state := testingNetworkQuotaState(t, ctx, networkId)
		connect.AssertEqual(t, state.SpentTime != nil && state.SpentTime.Equal(first), true)
		// a mark leaves the stored count as it was
		connect.AssertEqual(t, state.StoredByteCount, int64(0))
	})
}

// The pass clears a spent network once it holds an in-window balance with room at the pass's time, and
// not before: no balance, one not yet started, or one whose room journaled debits already hold.
func TestClearSpentNetworksOnANewBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		connect.AssertEqual(t, MarkNetworkSpent(ctx, networkId, now), nil)
		spentNow := func() bool {
			return testingNetworkQuotaState(t, ctx, networkId).SpentTime != nil
		}
		clear := func(at time.Time) {
			_, err := ClearSpentNetworks(ctx, at, nil)
			connect.AssertEqual(t, err, nil)
		}
		clear(now)
		connect.AssertEqual(t, spentNow(), true)

		// a grant that starts in an hour
		connect.AssertEqual(t, AddBasicTransferBalance(ctx, networkId, ByteCount(1000), now.Add(time.Hour), now.Add(25*time.Hour)), nil)
		clear(now)
		connect.AssertEqual(t, spentNow(), true)

		// its room taken by debits not yet applied
		balanceIds := testingNetworkBalanceIds(ctx, networkId)
		connect.AssertEqual(t, len(balanceIds), 1)
		usageId := testingJournalServiceDebit(ctx, balanceIds[0], sidecar.QuotaMeterEvs, 1000)
		clear(now.Add(2 * time.Hour))
		connect.AssertEqual(t, spentNow(), true)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM service_debit_journal WHERE balance_id = $1 AND usage_id = $2`, balanceIds[0], usageId))
		})
		clear(now.Add(2 * time.Hour))
		connect.AssertEqual(t, spentNow(), false)
	})
}

// A run looks at a bounded number of pages and hands its cursor on; the runs together clear every
// spent network with room and none without.
func TestClearSpentNetworksPagesThroughTheMarks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkQuotaStateSettings()
		settings.SpentPassPageSize = 2
		settings.SpentPassMaxPageCount = 1
		defer Testing_SetNetworkQuotaStateSettings(settings)()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		withRoom := map[server.Id]bool{}
		for i := range 5 {
			networkId := server.NewId()
			connect.AssertEqual(t, MarkNetworkSpent(ctx, networkId, now), nil)
			withRoom[networkId] = i%2 == 0
			if withRoom[networkId] {
				connect.AssertEqual(t, AddBasicTransferBalance(ctx, networkId, ByteCount(10), now.Add(-time.Hour), now.Add(time.Hour)), nil)
			}
		}
		var after *server.Id
		runCount := 0
		clearedCount := 0
		for {
			runCount += 1
			result, err := ClearSpentNetworks(ctx, now, after)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.LookedCount <= 2, true)
			clearedCount += result.ClearedCount
			if result.LastNetworkId == nil {
				break
			}
			after = result.LastNetworkId
		}
		connect.AssertEqual(t, runCount, 3)
		connect.AssertEqual(t, clearedCount, 3)
		for networkId, room := range withRoom {
			connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, networkId).SpentTime == nil, room)
		}
	})
}

// Team's limit is storage while storage_gib_month is unset, and storage_overage_max once it is priced
// (owner decision 122); other tiers read storage, and a row past the overage max raises it.
func TestNetworkStorageLimitForTeam(t *testing.T) {
	limits := ProSidecarLimits{}.withLimit("storage", 200*1024*1024*1024)
	team := &ProTeamConfig{StorageOverageMax: ByteCount(2 * 1024 * 1024 * 1024 * 1024)}
	connect.AssertEqual(t, networkStorageLimitByteCount(NetworkTierTeam, &limits, team), int64(200*1024*1024*1024))
	team.overageStorageGibMonthUsd = 0.05
	connect.AssertEqual(t, networkStorageLimitByteCount(NetworkTierTeam, &limits, team), int64(2*1024*1024*1024*1024))
	connect.AssertEqual(t, networkStorageLimitByteCount(NetworkTierPro, &limits, team), int64(200*1024*1024*1024))
	raised := limits.withLimit("storage", 3*1024*1024*1024*1024)
	connect.AssertEqual(t, networkStorageLimitByteCount(NetworkTierTeam, &raised, team), int64(3*1024*1024*1024*1024))
	none := limits.withLimit("storage", -1)
	connect.AssertEqual(t, networkStorageLimitByteCount(NetworkTierTeam, &none, team), int64(-1))
	connect.AssertEqual(t, networkStorageOver(5, -1), false)
	connect.AssertEqual(t, networkStorageOver(0, 0), true)
}
