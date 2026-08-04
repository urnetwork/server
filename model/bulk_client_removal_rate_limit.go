package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// A deployment-wide budget on the bulk removal API (RemoveNetworkClients),
// counted across all networks and accounts together -- this does not
// distinguish who is calling it, only how much total bulk-removal work has
// been admitted. It guards against many networks each triggering large
// bulk-deletes at once placing an unbounded aggregate load on the database
// in a short window.
//
// The budget is split into fixed, non-overlapping hourly buckets (UTC),
// rather than a rolling trailing window: a request is reserved against the
// earliest bucket -- starting with the current hour -- that has room for
// it. If the current hour is full, the request is queued for the next hour
// instead of rejected, and so on, up to MaxBulkClientRemovalLookaheadBuckets
// hours out. Only once no bucket in that whole lookahead window has room --
// meaning the deployment-wide daily budget (MaxBulkClientRemovalsPerDay) is
// genuinely exhausted -- is the request rejected and shown to the caller.
// Fixed buckets (rather than a rolling window) are what make "queue for the
// next hour" well-defined: there needs to be a discrete boundary to wait
// for.
//
// This intentionally does NOT apply to the single-client removal API
// (RemoveNetworkClient) -- that path is a single-row update with no
// comparable blast radius, and callers should not have their normal,
// incremental client cleanup throttled by bulk-API abuse elsewhere.
const BulkClientRemovalBucketDuration = time.Hour
const MaxBulkClientRemovalsPerBucket = 1000000
const MaxBulkClientRemovalLookaheadBuckets = 24
const MaxBulkClientRemovalsPerDay = MaxBulkClientRemovalLookaheadBuckets * MaxBulkClientRemovalsPerBucket

func maxBulkClientRemovalsError() error {
	return fmt.Errorf(
		"The bulk client removal limit (%d per hour, %d per day) has been reached for this deployment. Please try again later.",
		MaxBulkClientRemovalsPerBucket,
		MaxBulkClientRemovalsPerDay,
	)
}

// bulkClientRemovalBucketStart truncates t to the start of its UTC hourly
// bucket. Truncate operates on the absolute duration since the zero time,
// so this only aligns to true UTC hour boundaries when t is already UTC.
func bulkClientRemovalBucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(BulkClientRemovalBucketDuration)
}

// ReserveBulkClientRemovalSlot finds the earliest hourly bucket -- starting
// with the current hour -- that has room for `count` more removals, and
// reserves it there. It returns the reservation's id (for
// CancelBulkClientRemovalReservation, if the caller can't ultimately use the
// slot it just reserved -- e.g. a duplicate in-flight request for the same
// network) and the bucket's start time, which the caller should use as the
// task's RunAt when the bucket isn't the current hour.
//
// If no bucket within MaxBulkClientRemovalLookaheadBuckets hours has room,
// nothing is reserved and an error is returned -- this is the deployment's
// daily cap. A single request can never itself be too large to fit an empty
// bucket (RemoveNetworkClients already caps a request at
// MaxRemoveNetworkClientsCount, which equals MaxBulkClientRemovalsPerBucket),
// so this only happens under genuine contention from other reservations.
func ReserveBulkClientRemovalSlot(
	ctx context.Context,
	networkId server.Id,
	count int,
) (reservationId server.Id, bucketStart time.Time, err error) {
	windowStart := bulkClientRemovalBucketStart(server.NowUtc())
	windowEnd := windowStart.Add(MaxBulkClientRemovalLookaheadBuckets * BulkClientRemovalBucketDuration)

	server.Tx(ctx, func(tx server.PgTx) {
		usedByBucket := map[time.Time]int{}
		result, qerr := tx.Query(
			ctx,
			`
			SELECT bucket_start, SUM(client_count) FROM bulk_client_removal_quota
			WHERE $1 <= bucket_start AND bucket_start < $2
			GROUP BY bucket_start
			`,
			windowStart, windowEnd,
		)
		server.WithPgResult(result, qerr, func() {
			for result.Next() {
				var bucket time.Time
				var used int
				server.Raise(result.Scan(&bucket, &used))
				usedByBucket[bucket] = used
			}
		})

		for i := 0; i < MaxBulkClientRemovalLookaheadBuckets; i++ {
			candidate := windowStart.Add(time.Duration(i) * BulkClientRemovalBucketDuration)
			if usedByBucket[candidate]+count <= MaxBulkClientRemovalsPerBucket {
				id := server.NewId()
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO bulk_client_removal_quota (bulk_client_removal_quota_id, network_id, client_count, bucket_start, create_time)
					VALUES ($1, $2, $3, $4, $5)
					`,
					id, networkId, count, candidate, server.NowUtc(),
				))
				reservationId = id
				bucketStart = candidate
				return
			}
		}
		err = maxBulkClientRemovalsError()
	})
	return reservationId, bucketStart, err
}

// CancelBulkClientRemovalReservation releases a reservation made by
// ReserveBulkClientRemovalSlot that the caller ultimately couldn't use --
// e.g. the network turned out to already have a bulk-delete run in
// progress, discovered only after the slot was reserved (the target bucket
// has to be known before scheduling the task, so reservation necessarily
// happens before that check).
func CancelBulkClientRemovalReservation(ctx context.Context, reservationId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM bulk_client_removal_quota WHERE bulk_client_removal_quota_id = $1`,
			reservationId,
		))
	})
}

// RemoveExpiredBulkClientRemovalQuota deletes quota ledger rows whose bucket
// is entirely in the past relative to minBucketStart. Callers should pass a
// cutoff safely behind the lookahead window (e.g. now minus a couple of
// days), since a bucket up to MaxBulkClientRemovalLookaheadBuckets hours in
// the future can still be actively reserved against.
func RemoveExpiredBulkClientRemovalQuota(ctx context.Context, minBucketStart time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM bulk_client_removal_quota WHERE bucket_start < $1`,
			minBucketStart.UTC(),
		))
	})
}
