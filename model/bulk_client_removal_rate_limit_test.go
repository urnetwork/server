package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// The budget is global, not per-network: two different networks draw from
// the same shared per-bucket ceiling.
func TestReserveBulkClientRemovalSlotFillsCurrentBucketThenQueuesNextHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkIdA := server.NewId()
		networkIdB := server.NewId()

		currentBucket := bulkClientRemovalBucketStart(server.NowUtc())

		_, bucketStart, err := ReserveBulkClientRemovalSlot(ctx, networkIdA, MaxBulkClientRemovalsPerBucket-1)
		assert.Equal(t, err, nil)
		assert.Equal(t, bucketStart, currentBucket)

		// 1 more from a different network fits exactly at the current
		// bucket's ceiling
		_, bucketStart, err = ReserveBulkClientRemovalSlot(ctx, networkIdB, 1)
		assert.Equal(t, err, nil)
		assert.Equal(t, bucketStart, currentBucket)

		// the current bucket is now fully spent; the next reservation must
		// be queued for the next hour instead of being rejected
		_, bucketStart, err = ReserveBulkClientRemovalSlot(ctx, networkIdA, 1)
		assert.Equal(t, err, nil)
		assert.Equal(t, bucketStart, currentBucket.Add(BulkClientRemovalBucketDuration))
	})
}

// Once every bucket in the whole lookahead window (the deployment's daily
// budget) is spent, a new reservation must be rejected outright, not queued
// indefinitely -- this is the hard daily cap.
func TestReserveBulkClientRemovalSlotRejectsWhenDailyBudgetExhausted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for i := 0; i < MaxBulkClientRemovalLookaheadBuckets; i++ {
			_, _, err := ReserveBulkClientRemovalSlot(ctx, server.NewId(), MaxBulkClientRemovalsPerBucket)
			assert.Equal(t, err, nil)
		}

		// every bucket in the lookahead window is now full
		_, _, err := ReserveBulkClientRemovalSlot(ctx, server.NewId(), 1)
		assert.NotEqual(t, err, nil)
	})
}

// A cancelled reservation must free its slot back up -- e.g. for a request
// that turns out to be rejected for an unrelated reason (AlreadyInProgress)
// after the slot was already reserved.
func TestCancelBulkClientRemovalReservationFreesSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		currentBucket := bulkClientRemovalBucketStart(server.NowUtc())

		reservationId, bucketStart, err := ReserveBulkClientRemovalSlot(ctx, networkId, MaxBulkClientRemovalsPerBucket)
		assert.Equal(t, err, nil)
		assert.Equal(t, bucketStart, currentBucket)

		CancelBulkClientRemovalReservation(ctx, reservationId)

		// the current bucket's full ceiling must be available again
		_, bucketStart, err = ReserveBulkClientRemovalSlot(ctx, networkId, MaxBulkClientRemovalsPerBucket)
		assert.Equal(t, err, nil)
		assert.Equal(t, bucketStart, currentBucket)
	})
}

// RemoveExpiredBulkClientRemovalQuota must only remove buckets safely
// before the cutoff, leaving buckets at or after it untouched.
func TestRemoveExpiredBulkClientRemovalQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		currentBucket := bulkClientRemovalBucketStart(server.NowUtc())
		oldBucket := currentBucket.Add(-72 * time.Hour)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO bulk_client_removal_quota (bulk_client_removal_quota_id, network_id, client_count, bucket_start, create_time)
				VALUES ($1, $2, $3, $4, $5)
				`,
				server.NewId(), server.NewId(), 5, oldBucket, server.NowUtc(),
			))
		})

		_, _, err := ReserveBulkClientRemovalSlot(ctx, server.NewId(), 7)
		assert.Equal(t, err, nil)

		RemoveExpiredBulkClientRemovalQuota(ctx, currentBucket.Add(-48*time.Hour))

		var total int
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(ctx, `SELECT COALESCE(SUM(client_count), 0) FROM bulk_client_removal_quota`)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&total))
				}
			})
		})
		// the old (72h back) row must be gone; the current-bucket row (7)
		// from ReserveBulkClientRemovalSlot must remain
		assert.Equal(t, total, 7)
	})
}

// RemoveNetworkClients must actually surface the daily-cap rejection as an
// error to the caller, not just leave the reservation mechanism correct in
// isolation. Filling every bucket in the lookahead window directly (cheap)
// avoids needing to construct 24 million-id requests just to exercise this
// wiring.
func TestRemoveNetworkClientsSurfacesDailyBudgetRejection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for i := 0; i < MaxBulkClientRemovalLookaheadBuckets; i++ {
			_, _, err := ReserveBulkClientRemovalSlot(ctx, server.NewId(), MaxBulkClientRemovalsPerBucket)
			assert.Equal(t, err, nil)
		}

		sess := &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &jwt.ByJwt{NetworkId: server.NewId()},
		}

		_, err := RemoveNetworkClients(&RemoveNetworkClientsArgs{
			ClientIds: []server.Id{server.NewId(), server.NewId()},
		}, sess)
		assert.NotEqual(t, err, nil)
	})
}

// When the current hour's budget is already spent, RemoveNetworkClients
// must queue the request for the next hour -- even a small request that
// would normally run synchronously -- rather than reject it. This is the
// behavior the user asked for: wait for the next hour instead of failing.
func TestRemoveNetworkClientsQueuesSmallRequestWhenCurrentHourIsFull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		currentBucket := bulkClientRemovalBucketStart(server.NowUtc())

		_, _, err := ReserveBulkClientRemovalSlot(ctx, server.NewId(), MaxBulkClientRemovalsPerBucket)
		assert.Equal(t, err, nil)

		networkId := server.NewId()
		deviceId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")

		sess := &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &jwt.ByJwt{NetworkId: networkId},
		}

		result, err := RemoveNetworkClients(&RemoveNetworkClientsArgs{
			ClientIds: []server.Id{clientId},
		}, sess)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Scheduled, true)
		assert.NotEqual(t, result.ScheduledFor, nil)
		assert.Equal(t, *result.ScheduledFor, currentBucket.Add(BulkClientRemovalBucketDuration))

		// not applied yet -- it's queued for the next hour, not run now
		assert.NotEqual(t, GetNetworkClient(ctx, clientId), nil)
	})
}
