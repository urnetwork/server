// Overload windows: URL outcomes measured while our own platform was
// overloaded are recorded but not scored (owner, 2026-11-25).
//
// # Why
//
// From 00:00 to about 00:19 UTC on 2026-11-25 Main's database was overloaded:
// cached plans read a new day's partitions by sequential scan, the api
// answered in about 2.8 s and PgBouncer queued. In that window 88% of the URL
// outcomes scored against providers were failures, against about a third
// normally, and the next pool pass took 283 providers out of Quality (2,577 to
// 2,294) and about 1.1k out of Speed. The failures were the platform's, not
// the providers', yet they stayed in the eight-hour window and Quality was
// still short four hours later.
//
// # The signal
//
// The fleet's own scored URL outcomes, by bucket of measured time
// (BucketDuration, five minutes). A taskworker judge
// (JudgeProviderEgressUrlOverload) tallies each recent bucket from the
// history, every provider's outcomes together, and stores the tally in
// provider_egress_url_fleet_bucket. A bucket is an overload, and discounted,
// when all of these hold:
//
//   - it holds at least MinOutcomeCount scored outcomes, so a thin bucket is
//     no verdict;
//   - their failure share is above ErrorShare (0.6), against about 0.33 on a
//     normal day;
//   - and at least BaselineMargin (0.25) above the baseline: the failure share
//     of the stored buckets of the BaselineDuration (eight hours) before it,
//     discounted ones included, so a lasting rise of the fleet's failure share,
//     a stricter pass criterion for example, becomes the baseline within hours
//     instead of being discounted for good;
//   - and both still hold with the bucket's country with the most failures
//     left out, and again with its network with the most failures left out.
//
// The last condition keeps the signal fleet-wide. A real provider-side outage,
// such as a national block that fails every provider in one large country, or
// one operator's network of many providers failing together, is left out by
// it and still counts against those providers: what remains of the fleet fails
// at its usual share. An overload of our platform fails providers in every
// country and network alike.
//
// This signal needs no new infrastructure: the judge reads the history the
// window reads, with the same row predicate. It cannot be gamed by a provider
// or a group of them: a bucket holds every provider's outcomes, so moving its
// share to the line takes failing on purpose across a large part of the whole
// fleet, from many countries and networks at once, and the failures would be
// the group's own. The alternatives were weaker. The prober's local failures
// (turns that measured nothing) are already never scored, and a provider that
// refuses its tunnel makes them too. A database latency signal would be one
// process's view of one pool, measured only where it runs, while the failures
// it should explain show up whatever path the overload took (database, api,
// connect servers).
//
// # What is discounted
//
// Every scored outcome measured in a discounted bucket, its successes as well
// as its failures: keeping the successes while dropping the failures would lift
// every provider measured in the window, while dropping the bucket whole leaves
// each provider as its other outcomes have it, as if the probes had not run.
// The outcomes stay in the history, with their outcome, and in the rolling
// quota, which paces turns by measured runs (an overload costs no extra turns);
// what leaves them out of every score is the window's row predicate
// (providerEgressHealthWindowRowsSql), which Quality, Speed, the index, the
// Online rule, the zero-carry judge, the rollup and the provider status all
// read. A judged bucket names its range, so the predicate never needs the grid.
//
// # Timing
//
// The judge runs about once a minute and tallies the open bucket too, so an
// overload is discounted once its bucket holds enough outcomes, before the
// bucket ends. Each run tallies the RetallyDuration before it again, so
// outcomes that arrive late, as they do while the platform is overloaded, are
// judged with their bucket, and an overload the judge itself could not run
// through is judged once it can, within RetallyDuration. A pass that read the
// window before the judgement counts the overload until the next pass. A
// bucket older than RetallyDuration keeps its last verdict, which also holds
// for its late outcomes. The judge works through the maintenance pool, a
// direct session past PgBouncer, so it does not wait in PgBouncer's queue
// behind the overload it judges.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The bucket grid starts at midnight UTC, in Go (Time.Truncate, whose zero time
// is a midnight) and in SQL (date_bin from this origin) alike, for a bucket
// duration that divides a day.
const providerEgressUrlFleetBucketOrigin = "2000-01-01 00:00:00"

// The judge's lock: concurrent runs serialize, so each bucket's discounted
// counts, and the counter that follows them, move once.
const providerEgressUrlOverloadLockKey = "provider-egress-url-overload-v1"

// Tunables of the overload judge.
type ProviderEgressUrlOverloadSettings struct {
	// BucketDuration is the length of one bucket of measured time, the unit a
	// verdict covers. It must divide a day, so buckets start on the same grid
	// in Go and in SQL; one that is non-positive or does not judges nothing.
	BucketDuration time.Duration
	// RetallyDuration is how far back each run tallies the history again:
	// outcomes arrive after they are measured, later still while the platform
	// is overloaded. A bucket older than this keeps its last verdict.
	// Non-positive judges nothing.
	RetallyDuration time.Duration
	// MinOutcomeCount is the fewest scored outcomes a bucket needs for a
	// verdict, and so does what is left of it with its worst country or
	// network left out, and its baseline. Fewer are no overload.
	MinOutcomeCount int
	// ErrorShare is the failure share a bucket must exceed. A normal day's
	// share is about a third; the 2026-11-25 overload's was 0.88.
	ErrorShare float64
	// BaselineMargin is how far above its baseline's failure share a bucket's
	// must be.
	BaselineMargin float64
	// BaselineDuration is how far before a bucket its baseline reaches: the
	// stored buckets that start in it, discounted or not. The scoring window's
	// eight hours.
	BaselineDuration time.Duration
	// Retention is how long a judged bucket is kept after it ends: the
	// history's own retention, as long as the outcomes it judged.
	Retention time.Duration
	// JudgeTimeout bounds one run's transaction. A run that cannot finish in
	// it changes nothing, and the next run judges the same buckets.
	// Non-positive leaves the transaction to its role's own bound.
	JudgeTimeout time.Duration
	// JudgeInterval is how long after one run the taskworker runs the next:
	// how soon an overload is discounted once its bucket holds enough
	// outcomes.
	JudgeInterval time.Duration
	// JudgeMaxTime is the task's bound on one run, past its transaction's.
	JudgeMaxTime time.Duration
}

// Five-minute buckets judged against the eight hours before them.
func DefaultProviderEgressUrlOverloadSettings() *ProviderEgressUrlOverloadSettings {
	return &ProviderEgressUrlOverloadSettings{
		BucketDuration: 5 * time.Minute,
		// the 2026-11-25 overload lasted about nineteen minutes
		RetallyDuration: 30 * time.Minute,
		// the 2026-11-25 overload's buckets held about four thousand scored
		// outcomes each (14.5k in under twenty minutes); at 200 a share of
		// 0.6 against a third is many standard errors beyond sampling noise
		MinOutcomeCount:  200,
		ErrorShare:       0.6,
		BaselineMargin:   0.25,
		BaselineDuration: ProviderEgressHealthMaxAge,
		Retention:        DefaultProviderEgressHealthHistorySettings().Retention,
		JudgeTimeout:     time.Minute,
		JudgeInterval:    time.Minute,
		JudgeMaxTime:     5 * time.Minute,
	}
}

// Whether the settings can judge at all.
func (self *ProviderEgressUrlOverloadSettings) judges() bool {
	return 0 < self.BucketDuration && (24*time.Hour)%self.BucketDuration == 0 && 0 < self.RetallyDuration
}

// One bucket of measured time, as the judge stores it.
type ProviderEgressUrlFleetBucket struct {
	BucketStart time.Time
	BucketEnd   time.Time
	// the bucket's scored URL outcomes, every provider's, and the failures
	OutcomeCount int
	ErrorCount   int
	// the outcomes and failures of the bucket's country with the most
	// failures (its providers' published country), and of its network with
	// the most failures
	CountryOutcomeCount int
	CountryErrorCount   int
	NetworkOutcomeCount int
	NetworkErrorCount   int
	// the stored buckets of the baseline, summed
	BaselineOutcomeCount int64
	BaselineErrorCount   int64
	Discounted           bool
	// the outcomes and failures the judge has counted as discounted so far,
	// which never decrease
	DiscountedOutcomeCount int
	DiscountedErrorCount   int
	JudgeTime              time.Time
}

// The bucket's failure share, 0 for none.
func (self *ProviderEgressUrlFleetBucket) ErrorShare() float64 {
	if self.OutcomeCount <= 0 {
		return 0
	}
	return float64(self.ErrorCount) / float64(self.OutcomeCount)
}

// The baseline's failure share, 0 for none.
func (self *ProviderEgressUrlFleetBucket) BaselineErrorShare() float64 {
	if self.BaselineOutcomeCount <= 0 {
		return 0
	}
	return float64(self.BaselineErrorCount) / float64(self.BaselineOutcomeCount)
}

// Whether the tallied bucket is an overload of the platform (see the file
// header): enough outcomes, a failure share above ErrorShare and BaselineMargin
// above the baseline's, and the same again with the bucket's worst country,
// then its worst network, left out. A baseline thinner than MinOutcomeCount is
// no verdict.
func (self *ProviderEgressUrlOverloadSettings) overloaded(bucket *ProviderEgressUrlFleetBucket) bool {
	if bucket.BaselineOutcomeCount <= 0 || bucket.BaselineOutcomeCount < int64(self.MinOutcomeCount) {
		return false
	}
	baselineShare := bucket.BaselineErrorShare()
	over := func(outcomeCount int, errorCount int) bool {
		if outcomeCount <= 0 || outcomeCount < self.MinOutcomeCount {
			return false
		}
		share := float64(errorCount) / float64(outcomeCount)
		return self.ErrorShare < share && baselineShare+self.BaselineMargin <= share
	}
	return over(bucket.OutcomeCount, bucket.ErrorCount) &&
		over(bucket.OutcomeCount-bucket.CountryOutcomeCount, bucket.ErrorCount-bucket.CountryErrorCount) &&
		over(bucket.OutcomeCount-bucket.NetworkOutcomeCount, bucket.ErrorCount-bucket.NetworkErrorCount)
}

// What one judge run did.
type ProviderEgressUrlOverloadJudgement struct {
	// the buckets the run tallied, oldest first, as stored
	Buckets []*ProviderEgressUrlFleetBucket
	// those the run discounted that were not before, and those it no longer
	// discounts
	Entered  []*ProviderEgressUrlFleetBucket
	Released []*ProviderEgressUrlFleetBucket
	// scored outcomes newly counted as discounted, and the failures among them
	DiscountedOutcomeCount int
	DiscountedErrorCount   int
	// judged buckets removed past their retention
	RemovedCount int64
}

// The scored history rows the judge tallies by bucket, over [$1, $2), each
// bucket $3 seconds from the grid's origin: the window's rows before the
// discount (providerEgressHealthScoredRowsSql), with each outcome's network and
// its provider's published country. Per bucket, the outcomes and failures,
// then those of the country and of the network with the most failures (a
// provider without either is one more group). One range of the history's
// measured_at index and a key lookup per row.
func providerEgressUrlFleetBucketTallySql() string {
	return fmt.Sprintf(`
		WITH outcome AS MATERIALIZED (
			SELECT
				date_bin(make_interval(secs => $3::double precision), history.measured_at, TIMESTAMP '%[1]s') AS bucket_start,
				history.ok_count = 0 AS error,
				client.network_id,
				country_location.country_code
			FROM provider_egress_health_history AS history
			LEFT JOIN network_client AS client ON client.client_id = history.client_id
			LEFT JOIN network_client_location_reliability AS reliability ON reliability.client_id = history.client_id
			LEFT JOIN location AS country_location ON country_location.location_id = reliability.country_location_id
			WHERE history.measured_at >= $1 AND history.measured_at < $2 AND %[2]s
		), bucket AS (
			SELECT bucket_start, count(*) AS outcome_count, count(*) FILTER (WHERE error) AS error_count
			FROM outcome GROUP BY bucket_start
		), country AS (
			SELECT DISTINCT ON (bucket_start) bucket_start, outcome_count, error_count
			FROM (
				SELECT bucket_start, country_code, count(*) AS outcome_count, count(*) FILTER (WHERE error) AS error_count
				FROM outcome GROUP BY bucket_start, country_code
			) AS grouped
			ORDER BY bucket_start, error_count DESC, outcome_count DESC
		), network AS (
			SELECT DISTINCT ON (bucket_start) bucket_start, outcome_count, error_count
			FROM (
				SELECT bucket_start, network_id, count(*) AS outcome_count, count(*) FILTER (WHERE error) AS error_count
				FROM outcome GROUP BY bucket_start, network_id
			) AS grouped
			ORDER BY bucket_start, error_count DESC, outcome_count DESC
		)
		SELECT bucket.bucket_start, bucket.outcome_count, bucket.error_count,
			country.outcome_count, country.error_count, network.outcome_count, network.error_count
		FROM bucket
		INNER JOIN country ON country.bucket_start = bucket.bucket_start
		INNER JOIN network ON network.bucket_start = bucket.bucket_start
		ORDER BY bucket.bucket_start`,
		providerEgressUrlFleetBucketOrigin, providerEgressHealthScoredRowsSql("history"))
}

// The window's discount over the history rows aliased alias ("" for the bare
// table): not measured in a discounted bucket. The discounted buckets' ranges
// are one multirange, read once per statement, so the test per row is a search
// of a few ranges, and a statement with none to read keeps every row.
func providerEgressUrlOverloadDiscountSql(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return fmt.Sprintf(`NOT COALESCE(%smeasured_at <@ (
		SELECT range_agg(tsrange(discount.bucket_start, discount.bucket_end, '[)'))
		FROM provider_egress_url_fleet_bucket AS discount WHERE discount.discounted
	), false)`, prefix)
}

// One run of the judge at now (see the file header): tally the buckets of the
// RetallyDuration before now through the open bucket, judge each against the
// stored baseline before it, store them, and remove the judged buckets past
// their retention.
func JudgeProviderEgressUrlOverload(ctx context.Context, now time.Time, settings *ProviderEgressUrlOverloadSettings) *ProviderEgressUrlOverloadJudgement {
	if settings == nil {
		settings = DefaultProviderEgressUrlOverloadSettings()
	}
	judgement := &ProviderEgressUrlOverloadJudgement{}
	if !settings.judges() {
		return judgement
	}
	now = now.UTC()
	spanStart := now.Add(-settings.RetallyDuration).Truncate(settings.BucketDuration)
	spanEnd := now.Truncate(settings.BucketDuration).Add(settings.BucketDuration)

	judgeCtx := ctx
	if 0 < settings.JudgeTimeout {
		var cancel context.CancelFunc
		judgeCtx, cancel = context.WithTimeout(ctx, settings.JudgeTimeout)
		defer cancel()
	}
	server.MaintenanceTx(judgeCtx, func(tx server.PgTx) {
		// a retried transaction discards what the rolled-back one decided
		*judgement = ProviderEgressUrlOverloadJudgement{}
		if 0 < settings.JudgeTimeout {
			server.SetTransactionTimeoutInTx(judgeCtx, tx, settings.JudgeTimeout)
		}
		server.RaisePgResult(tx.Exec(judgeCtx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, providerEgressUrlOverloadLockKey))

		// the stored buckets of every tallied bucket's baseline, and the
		// tallied ones as stored before this run
		stored := map[int64]*ProviderEgressUrlFleetBucket{}
		rows, err := tx.Query(judgeCtx, `
			SELECT bucket_start, bucket_end, outcome_count, error_count, discounted, discounted_outcome_count, discounted_error_count
			FROM provider_egress_url_fleet_bucket
			WHERE bucket_start >= $1 AND bucket_start < $2`,
			spanStart.Add(-settings.BaselineDuration), spanEnd)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				bucket := &ProviderEgressUrlFleetBucket{}
				server.Raise(rows.Scan(
					&bucket.BucketStart,
					&bucket.BucketEnd,
					&bucket.OutcomeCount,
					&bucket.ErrorCount,
					&bucket.Discounted,
					&bucket.DiscountedOutcomeCount,
					&bucket.DiscountedErrorCount,
				))
				bucket.BucketStart = bucket.BucketStart.UTC()
				stored[bucket.BucketStart.UnixMicro()] = bucket
			}
		})

		tallied := []*ProviderEgressUrlFleetBucket{}
		rows, err = tx.Query(judgeCtx, providerEgressUrlFleetBucketTallySql(), spanStart, spanEnd, settings.BucketDuration.Seconds())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				bucket := &ProviderEgressUrlFleetBucket{}
				server.Raise(rows.Scan(
					&bucket.BucketStart,
					&bucket.OutcomeCount,
					&bucket.ErrorCount,
					&bucket.CountryOutcomeCount,
					&bucket.CountryErrorCount,
					&bucket.NetworkOutcomeCount,
					&bucket.NetworkErrorCount,
				))
				bucket.BucketStart = bucket.BucketStart.UTC()
				bucket.BucketEnd = bucket.BucketStart.Add(settings.BucketDuration)
				bucket.JudgeTime = now
				tallied = append(tallied, bucket)
			}
		})

		// a bucket's baseline is the buckets that start in the BaselineDuration
		// before it, this run's tallies where it has them
		baseline := map[int64]*ProviderEgressUrlFleetBucket{}
		for start, bucket := range stored {
			baseline[start] = bucket
		}
		for _, bucket := range tallied {
			baseline[bucket.BucketStart.UnixMicro()] = bucket
		}
		for _, bucket := range tallied {
			from, to := bucket.BucketStart.Add(-settings.BaselineDuration).UnixMicro(), bucket.BucketStart.UnixMicro()
			for start, other := range baseline {
				if from <= start && start < to {
					bucket.BaselineOutcomeCount += int64(other.OutcomeCount)
					bucket.BaselineErrorCount += int64(other.ErrorCount)
				}
			}
		}

		for _, bucket := range tallied {
			previous := stored[bucket.BucketStart.UnixMicro()]
			if previous == nil {
				previous = &ProviderEgressUrlFleetBucket{}
			}
			bucket.Discounted = settings.overloaded(bucket)
			// what the counter has counted of the bucket never goes back, so a
			// bucket that leaves the discount and enters it again counts only
			// its outcomes beyond those
			bucket.DiscountedOutcomeCount = previous.DiscountedOutcomeCount
			bucket.DiscountedErrorCount = previous.DiscountedErrorCount
			if bucket.Discounted {
				bucket.DiscountedOutcomeCount = max(previous.DiscountedOutcomeCount, bucket.OutcomeCount)
				bucket.DiscountedErrorCount = max(previous.DiscountedErrorCount, bucket.ErrorCount)
				judgement.DiscountedOutcomeCount += bucket.DiscountedOutcomeCount - previous.DiscountedOutcomeCount
				judgement.DiscountedErrorCount += bucket.DiscountedErrorCount - previous.DiscountedErrorCount
			}
			switch {
			case bucket.Discounted && !previous.Discounted:
				judgement.Entered = append(judgement.Entered, bucket)
			case !bucket.Discounted && previous.Discounted:
				judgement.Released = append(judgement.Released, bucket)
			}
			server.RaisePgResult(tx.Exec(judgeCtx, `
				INSERT INTO provider_egress_url_fleet_bucket (
					bucket_start, bucket_end, outcome_count, error_count,
					country_outcome_count, country_error_count, network_outcome_count, network_error_count,
					baseline_outcome_count, baseline_error_count,
					discounted, discounted_outcome_count, discounted_error_count, judge_time
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
				ON CONFLICT (bucket_start) DO UPDATE SET
					bucket_end = EXCLUDED.bucket_end,
					outcome_count = EXCLUDED.outcome_count,
					error_count = EXCLUDED.error_count,
					country_outcome_count = EXCLUDED.country_outcome_count,
					country_error_count = EXCLUDED.country_error_count,
					network_outcome_count = EXCLUDED.network_outcome_count,
					network_error_count = EXCLUDED.network_error_count,
					baseline_outcome_count = EXCLUDED.baseline_outcome_count,
					baseline_error_count = EXCLUDED.baseline_error_count,
					discounted = EXCLUDED.discounted,
					discounted_outcome_count = EXCLUDED.discounted_outcome_count,
					discounted_error_count = EXCLUDED.discounted_error_count,
					judge_time = EXCLUDED.judge_time`,
				bucket.BucketStart, bucket.BucketEnd, bucket.OutcomeCount, bucket.ErrorCount,
				bucket.CountryOutcomeCount, bucket.CountryErrorCount, bucket.NetworkOutcomeCount, bucket.NetworkErrorCount,
				bucket.BaselineOutcomeCount, bucket.BaselineErrorCount,
				bucket.Discounted, bucket.DiscountedOutcomeCount, bucket.DiscountedErrorCount, bucket.JudgeTime,
			))
		}
		judgement.Buckets = tallied

		if 0 < settings.Retention {
			removed, err := tx.Exec(judgeCtx, `DELETE FROM provider_egress_url_fleet_bucket WHERE bucket_end < $1`, now.Add(-settings.Retention))
			server.Raise(err)
			judgement.RemovedCount = removed.RowsAffected()
		}
	}, server.TxReadCommitted)
	return judgement
}
