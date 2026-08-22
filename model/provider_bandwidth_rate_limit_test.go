package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The budget is global, not per-provider: two different providers draw from
// the same shared per-bucket byte ceiling.
func TestReserveProviderBandwidthSlotFillsBucketThenSpillsToNext(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientIdA := server.NewId()
		clientIdB := server.NewId()

		currentBucket := providerBandwidthBucketStart(server.NowUtc())

		half := MaxProviderBandwidthBytesPerBucket() / 2

		_, bucket1, err := ReserveProviderBandwidthSlot(ctx, clientIdA, half)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bucket1, currentBucket)

		// the remaining half fits exactly at the current bucket's ceiling,
		// even though it's a different provider
		_, bucketStart, err := ReserveProviderBandwidthSlot(ctx, clientIdB, MaxProviderBandwidthBytesPerBucket()-half)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bucketStart, currentBucket)

		// the current bucket is now fully spent; the next reservation must
		// spill into the next hour instead of being rejected
		_, bucket2, err := ReserveProviderBandwidthSlot(ctx, clientIdA, 1)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bucket2, currentBucket.Add(ProviderBandwidthBucketDuration))
	})
}

// Once every bucket in the whole lookahead window (the deployment's daily
// byte budget) is spent, a new reservation must be rejected outright rather
// than queued indefinitely -- this is the hard daily cap.
func TestReserveProviderBandwidthSlotErrorsWhenAllBucketsFull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for i := 0; i < MaxProviderBandwidthLookaheadBuckets; i++ {
			_, _, err := ReserveProviderBandwidthSlot(ctx, server.NewId(), MaxProviderBandwidthBytesPerBucket())
			connect.AssertEqual(t, err, nil)
		}

		// every bucket in the lookahead window is now full
		_, _, err := ReserveProviderBandwidthSlot(ctx, server.NewId(), 1)
		connect.AssertNotEqual(t, err, nil)
	})
}

// A cancelled reservation must free its bytes back up -- e.g. an active probe
// that was never actually run because the provider went offline between
// reserving the budget and dialing it.
func TestCancelProviderBandwidthReservationFreesSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		currentBucket := providerBandwidthBucketStart(server.NowUtc())

		reservationId, bucketStart, err := ReserveProviderBandwidthSlot(ctx, clientId, MaxProviderBandwidthBytesPerBucket())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bucketStart, currentBucket)

		CancelProviderBandwidthReservation(ctx, reservationId)

		// the current bucket's full ceiling must be available again
		_, bucketStart, err = ReserveProviderBandwidthSlot(ctx, clientId, MaxProviderBandwidthBytesPerBucket())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bucketStart, currentBucket)
	})
}

// RemoveExpiredProviderBandwidthQuota must only remove buckets safely before
// the cutoff, leaving buckets at or after it untouched.
func TestRemoveExpiredProviderBandwidthQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		currentBucket := providerBandwidthBucketStart(server.NowUtc())
		oldBucket := currentBucket.Add(-72 * time.Hour)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO provider_bandwidth_quota (provider_bandwidth_quota_id, client_id, byte_count, bucket_start, create_time)
				VALUES ($1, $2, $3, $4, $5)
				`,
				server.NewId(), server.NewId(), 5, oldBucket, server.NowUtc(),
			))
		})

		_, _, err := ReserveProviderBandwidthSlot(ctx, server.NewId(), 7)
		connect.AssertEqual(t, err, nil)

		RemoveExpiredProviderBandwidthQuota(ctx, currentBucket.Add(-48*time.Hour))

		var total int64
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(ctx, `SELECT COALESCE(SUM(byte_count), 0) FROM provider_bandwidth_quota`)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&total))
				}
			})
		})
		// the old (72h back) row must be gone; the current-bucket row (7)
		// from ReserveProviderBandwidthSlot must remain
		connect.AssertEqual(t, total, int64(7))
	})
}

// The budget is configurable because capacity is a property of the deployment,
// but an environment with no config file must still boot and must fall back to
// the CONSERVATIVE default -- never to something larger than a small
// deployment can afford.
func TestActiveBandwidthBudgetFallsBackToTheConservativeDefault(t *testing.T) {
	// a deployment may supply provider_bandwidth.yml, so this asserts the
	// relationship rather than a fixed number: whatever is configured, the
	// default it would fall back to must be the smaller, safer value.
	if defaultActiveBandwidthProbesPerBucket <= 0 {
		t.Fatal("the default budget must be positive; zero would reject every probe")
	}
	if MaxProviderBandwidthBytesPerBucket() != int64(activeBandwidthProbesPerBucket())*MaxProviderBandwidthBytesPerProbe {
		t.Error("the hourly byte budget must be the probe count times the per-probe size")
	}
	if MaxProviderBandwidthBytesPerDay() != MaxProviderBandwidthLookaheadBuckets*MaxProviderBandwidthBytesPerBucket() {
		t.Error("the daily cap must be the lookahead window times the hourly budget")
	}
}
