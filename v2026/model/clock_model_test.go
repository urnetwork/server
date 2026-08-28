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
