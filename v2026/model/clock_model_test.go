package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func testingResetClock(ctx context.Context) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Del(ctx, clockTransferByteCountRedisKey).Err())
	})
}

func TestClockCounterUsesExactDecimalStrings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingResetClock(ctx)
		defer testingResetClock(ctx)

		if result, ok := GetClock(ctx); ok || result != nil {
			t.Fatalf("GetClock() = (%+v, %t), want uninitialized", result, ok)
		}

		AddClockTransferByteCount(ctx, 10)
		AddClockTransferByteCount(ctx, 15)
		AddClockTransferByteCount(ctx, 0)
		AddClockTransferByteCount(ctx, -1)

		result, ok := GetClock(ctx)
		if !ok {
			t.Fatal("GetClock() was uninitialized after increments")
		}
		if result.TotalTransferByteCount != "25" {
			t.Fatalf("total_transfer_byte_count = %q, want 25", result.TotalTransferByteCount)
		}
		if result.SinceBlock != ClockSinceBlock {
			t.Fatalf("since_block = %d, want %d", result.SinceBlock, ClockSinceBlock)
		}
		if result.SinceTime != ClockSinceTime.Format(time.RFC3339) {
			t.Fatalf("since_time = %q, want %q", result.SinceTime, ClockSinceTime.Format(time.RFC3339))
		}
	})
}

func TestClockAdvancesOnlyWhenContractClaimsFinalOutcome(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		reconcileClockCandidate(ctx, 0)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		Testing_CreateDevice(ctx, sourceNetworkId, server.NewId(), sourceId, "", "")
		Testing_CreateDevice(ctx, destinationNetworkId, server.NewId(), destinationId, "", "")

		contractId, err := CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			1024,
		)
		if err != nil {
			t.Fatalf("CreateContractNoEscrow(): %v", err)
		}

		if err := CloseContract(ctx, contractId, sourceId, 11, false); err != nil {
			t.Fatalf("source CloseContract(): %v", err)
		}
		result, ok := GetClock(ctx)
		if !ok || result.TotalTransferByteCount != "0" {
			t.Fatalf("clock after first party close = (%+v, %t), want 0", result, ok)
		}

		if err := CloseContract(ctx, contractId, destinationId, 13, false); err != nil {
			t.Fatalf("destination CloseContract(): %v", err)
		}
		result, ok = GetClock(ctx)
		if !ok || result.TotalTransferByteCount != "13" {
			t.Fatalf("clock after final outcome = (%+v, %t), want 13", result, ok)
		}

		if err := CloseContract(ctx, contractId, destinationId, 13, false); err == nil {
			t.Fatal("duplicate CloseContract() succeeded")
		}
		result, ok = GetClock(ctx)
		if !ok || result.TotalTransferByteCount != "13" {
			t.Fatalf("clock after duplicate close = (%+v, %t), want 13", result, ok)
		}
	})
}

func TestBackfillClockUsesBlockNineAggregateAndNeverLowersCounter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingResetClock(ctx)
		defer testingResetClock(ctx)

		baseline := clockBackfillCandidate(ctx)
		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		Testing_CreateDevice(ctx, sourceNetworkId, server.NewId(), sourceId, "", "")
		Testing_CreateDevice(ctx, destinationNetworkId, server.NewId(), destinationId, "", "")

		Testing_CreateSettledContract(
			ctx,
			sourceId,
			destinationId,
			ClockSinceTime.Add(-time.Minute),
			ClockSinceTime.Add(-time.Second),
			100,
		)
		Testing_CreateSettledContract(
			ctx,
			sourceId,
			destinationId,
			ClockSinceTime,
			ClockSinceTime,
			20,
		)
		Testing_CreateSettledContract(
			ctx,
			sourceId,
			destinationId,
			ClockSinceTime.Add(time.Minute),
			ClockSinceTime.Add(2*time.Minute),
			30,
		)

		want := baseline + 50
		if got := BackfillClock(ctx); got != want {
			t.Fatalf("BackfillClock() = %d, want %d", got, want)
		}

		AddClockTransferByteCount(ctx, 7)
		if got := BackfillClock(ctx); got != want+7 {
			t.Fatalf("BackfillClock() lowered live counter to %d, want %d", got, want+7)
		}
	})
}

func TestClockContiguousRollupPrefixStopsAtFirstMissingDay(t *testing.T) {
	since := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)
	today := since.Add(5 * 24 * time.Hour)
	rollups := []clockDailyRollup{
		{day: since, byteCount: 10, rowCount: 1},
		{day: since.Add(24 * time.Hour), byteCount: 20, rowCount: 1},
		// Day 2 is deliberately absent. A later row must not hide the gap.
		{day: since.Add(3 * 24 * time.Hour), byteCount: 40, rowCount: 1},
	}

	byteCount, tailStart := clockContiguousRollupPrefix(since, today, rollups)
	if byteCount != 30 {
		t.Fatalf("prefix byte count = %d, want 30", byteCount)
	}
	if want := since.Add(2 * 24 * time.Hour); !tailStart.Equal(want) {
		t.Fatalf("tail start = %s, want %s", tailStart, want)
	}
}

func TestClockContiguousRollupPrefixBoundsRawTailToToday(t *testing.T) {
	since := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)
	today := since.Add(3 * 24 * time.Hour)
	rollups := []clockDailyRollup{
		{day: since, byteCount: 10, rowCount: 1},
		{day: since.Add(24 * time.Hour), byteCount: 20, rowCount: 1},
		{day: since.Add(2 * 24 * time.Hour), byteCount: 30, rowCount: 1},
	}

	byteCount, tailStart := clockContiguousRollupPrefix(since, today, rollups)
	if byteCount != 60 {
		t.Fatalf("prefix byte count = %d, want 60", byteCount)
	}
	if !tailStart.Equal(today) {
		t.Fatalf("tail start = %s, want today %s", tailStart, today)
	}
}

func TestClockContiguousRollupPrefixRejectsDuplicateDay(t *testing.T) {
	since := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)
	rollups := []clockDailyRollup{
		{day: since, byteCount: 10, rowCount: 1},
		{day: since.Add(24 * time.Hour), byteCount: 40, rowCount: 2},
	}

	byteCount, tailStart := clockContiguousRollupPrefix(since, since.Add(3*24*time.Hour), rollups)
	if byteCount != 10 {
		t.Fatalf("prefix byte count = %d, want only the unique day", byteCount)
	}
	if want := since.Add(24 * time.Hour); !tailStart.Equal(want) {
		t.Fatalf("tail start = %s, want duplicate day %s", tailStart, want)
	}
}
