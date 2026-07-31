package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// A deployment-wide budget on the bytes active bandwidth probing is allowed
// to spend, counted across all providers together -- this does not
// distinguish which provider is being probed, only how much total probe
// traffic has been admitted. Active probing pulls real data through a
// provider's tunnel, which is a real paid contract: a zero-cost balance code
// does not make it free, because the payout planner sums paid and unpaid
// traffic identically before computing payouts
// (account_payment_model_plan.go). This is therefore a spend limit
// unconditionally, on every deployment, not only where payouts happen to be
// live.
//
// The budget is split into fixed, non-overlapping hourly buckets (UTC),
// rather than a rolling trailing window: a reservation is made against the
// earliest bucket -- starting with the current hour -- that has room for it.
// If the current hour is full, the probe is deferred to the next hour instead
// of rejected, and so on, up to MaxProviderBandwidthLookaheadBuckets hours
// out. Only once no bucket in that whole lookahead window has room -- meaning
// the deployment-wide daily budget (MaxProviderBandwidthBytesPerDay) is
// genuinely exhausted -- is the probe rejected. Fixed buckets (rather than a
// rolling window) are what make "defer to the next hour" well-defined: there
// needs to be a discrete boundary to wait for.
//
// This intentionally does NOT apply to the passive bandwidth signal, which is
// aggregated from already-settled transfer_escrow bytes and costs nothing
// additional -- there is no spend to budget there.
const ProviderBandwidthBucketDuration = time.Hour
const MaxProviderBandwidthLookaheadBuckets = 24

// MaxActiveBandwidthProbesPerBucket is derived from the population this
// probes, not picked arbitrarily. Active sampling only ever runs against
// providers with no passive history -- on beta today that is the entire fleet
// (nothing has settled a contract yet), and on a mature deployment it is the
// trickle of newly-joined providers before their first settled contract. 40
// per hour comfortably covers a beta-sized fleet in a single pass, and stays a
// small, bounded spend on a mature deployment where the no-passive-history
// population is naturally small. It is one value chosen to behave sensibly at
// both scales, not a per-environment knob.
//
// This is a tuned value, not a structural one: revisit it against real
// production data once active probing has run for a week.
const MaxActiveBandwidthProbesPerBucket = 40

// MaxProviderBandwidthBytesPerProbe is what ONE reservation admits, and it
// must equal what one measurement actually transfers -- a budget that
// under-counts is worse than no budget, because it reports a spend ceiling the
// deployment is quietly exceeding.
//
// A measurement is 8 parallel streams of 2 MiB = 16 MiB, per target. It is
// parallel because it has to be: a single TCP flow cannot exceed (send window
// / RTT), connect's MaxWindowSize is scaledPow2WindowSize(mib(1), ...), and
// the single-stream probe therefore measured 1 MiB / RTT for every provider on
// the fleet rather than the provider. Eleven of twelve beta providers came
// back with a bandwidth-delay product of ~1 MiB -- exactly one window -- and a
// provider independently measured at 79 MB/s on its own host reported
// 4.8 MB/s through the tunnel. N flows get N windows; the prober's
// bandwidth.MaxSampleBytes is the other half of this figure and the two must
// be changed together.
//
// So the hourly budget is 40 probes x 16 MiB = 640 MiB/hour, and the daily cap
// is 24 x 640 MiB = 15 GiB worst case -- the ceiling only reached if every
// bucket in the lookahead window fills. Note that a probe reserves per TARGET
// and there are two targets measured separately, so 40 reservations is 20
// providers per hour, not 40. That is unchanged by this commit and is a
// property of MaxActiveBandwidthProbesPerBucket, which is left alone here.
//
// Explicitly int64: a byte budget is not a row count, and callers pass an
// int64 byteCount.
const MaxProviderBandwidthBytesPerProbe int64 = 16 * 1024 * 1024
const MaxProviderBandwidthBytesPerBucket int64 = MaxActiveBandwidthProbesPerBucket * MaxProviderBandwidthBytesPerProbe
const MaxProviderBandwidthBytesPerDay int64 = MaxProviderBandwidthLookaheadBuckets * MaxProviderBandwidthBytesPerBucket

func maxProviderBandwidthError() error {
	return fmt.Errorf(
		"The active bandwidth probe budget (%d bytes per hour, %d bytes per day) has been reached for this deployment. Please try again later.",
		MaxProviderBandwidthBytesPerBucket,
		MaxProviderBandwidthBytesPerDay,
	)
}

// providerBandwidthBucketStart truncates t to the start of its UTC hourly
// bucket. Truncate operates on the absolute duration since the zero time,
// so this only aligns to true UTC hour boundaries when t is already UTC.
func providerBandwidthBucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(ProviderBandwidthBucketDuration)
}

// ReserveProviderBandwidthSlot finds the earliest hourly bucket -- starting
// with the current hour -- that has room for `byteCount` more probe bytes,
// and reserves them there. It returns the reservation's id (for
// CancelProviderBandwidthReservation, if the caller can't ultimately use the
// budget it just reserved -- e.g. the provider went offline between reserving
// and dialing) and the bucket's start time, which the caller should use as
// the probe's RunAt when the bucket isn't the current hour. A probe deferred
// to a future hour should have its RunAt jittered randomly across that hour
// rather than firing at the hour's top: otherwise every probe pushed into the
// same future bucket becomes eligible at the identical instant, converting a
// budget that was meant to spread load into an hourly thundering herd.
//
// If no bucket within MaxProviderBandwidthLookaheadBuckets hours has room,
// nothing is reserved and an error is returned -- this is the deployment's
// daily cap. A single probe can never itself be too large to fit an empty
// bucket (one probe is MaxProviderBandwidthBytesPerProbe, a fortieth of a
// bucket), so this only happens under genuine contention from other probes.
//
// Concurrency: this is a plain read-then-insert in one transaction, with no
// row locking. `SELECT ... FOR UPDATE` would not help here -- the thing being
// contended is a bucket's *aggregate*, and an empty or partly-filled bucket
// has no single row to lock; serializing reservations properly would need a
// materialized per-bucket row or an advisory lock, which this ledger shape
// deliberately doesn't have. Under the default RepeatableRead isolation two
// concurrent reservations can therefore both read the same bucket total and
// both insert (their rows are distinct, so there's no write-write conflict
// and no serialization failure), overshooting the ceiling by at most
// (concurrent reservers x their byte counts). That is acceptable: this is a
// coarse deployment-wide spend cap, not an accounting invariant, and probe
// reservations arrive at a low rate from a small number of prober processes.
func ReserveProviderBandwidthSlot(
	ctx context.Context,
	clientId server.Id,
	byteCount int64,
) (reservationId server.Id, bucketStart time.Time, err error) {
	windowStart := providerBandwidthBucketStart(server.NowUtc())
	windowEnd := windowStart.Add(MaxProviderBandwidthLookaheadBuckets * ProviderBandwidthBucketDuration)

	server.Tx(ctx, func(tx server.PgTx) {
		usedByBucket := map[time.Time]int64{}
		result, qerr := tx.Query(
			ctx,
			`
			SELECT bucket_start, SUM(byte_count) FROM provider_bandwidth_quota
			WHERE $1 <= bucket_start AND bucket_start < $2
			GROUP BY bucket_start
			`,
			windowStart, windowEnd,
		)
		server.WithPgResult(result, qerr, func() {
			for result.Next() {
				var bucket time.Time
				var used int64
				server.Raise(result.Scan(&bucket, &used))
				usedByBucket[bucket] = used
			}
		})

		for i := 0; i < MaxProviderBandwidthLookaheadBuckets; i++ {
			candidate := windowStart.Add(time.Duration(i) * ProviderBandwidthBucketDuration)
			if usedByBucket[candidate]+byteCount <= MaxProviderBandwidthBytesPerBucket {
				id := server.NewId()
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_bandwidth_quota (provider_bandwidth_quota_id, client_id, byte_count, bucket_start, create_time)
					VALUES ($1, $2, $3, $4, $5)
					`,
					id, clientId, byteCount, candidate, server.NowUtc(),
				))
				reservationId = id
				bucketStart = candidate
				return
			}
		}
		err = maxProviderBandwidthError()
	})
	return reservationId, bucketStart, err
}

// CancelProviderBandwidthReservation releases a reservation made by
// ReserveProviderBandwidthSlot that the caller ultimately couldn't use -- e.g.
// the provider turned out to be unreachable, discovered only after the budget
// was reserved (the target bucket has to be known before the probe can be
// scheduled, so reservation necessarily happens before that check).
func CancelProviderBandwidthReservation(ctx context.Context, reservationId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_bandwidth_quota WHERE provider_bandwidth_quota_id = $1`,
			reservationId,
		))
	})
}

// RemoveExpiredProviderBandwidthQuota deletes quota ledger rows whose bucket
// is entirely in the past relative to minBucketStart. Callers should pass a
// cutoff safely behind the lookahead window (e.g. now minus a couple of
// days), since a bucket up to MaxProviderBandwidthLookaheadBuckets hours in
// the future can still be actively reserved against.
func RemoveExpiredProviderBandwidthQuota(ctx context.Context, minBucketStart time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_bandwidth_quota WHERE bucket_start < $1`,
			minBucketStart.UTC(),
		))
	})
}
