// The dual-database ARIN canary of the score export (arindbctl/ARINDB.md,
// "Dual-database canary"). Every process that classifies addresses stamps each
// connection with the facts of the latest and the old database, and the rollup
// carries both slots with their epochs. Once per publication the indexer counts
// public bucket supply under each database over the providers with facts for
// both, and chooses one serving epoch S for the whole publication: the latest
// unless the latest trips the breaker, then the old classification. No
// provider is decided separately.
//
// Quality classification (non-quality and verified) follows S; risk always
// follows each row's latest slot, so a fallback can restore Quality but never
// readmits a provider the latest database calls risky. A slot is used only when
// its epoch is S, so a mixed fleet stays consistent; S = 0 (state single, or no
// readable decision) is each row's latest slot, as before the canary. Facts
// that are unavailable under S count as non-Quality, never clean. The indexer
// publishes the decision before its export, so request-time readers apply the
// same S while the export rewrites targets. A fallback latches on its pair of
// databases until release_passing_runs consecutive comparisons pass with
// release_margin, or the operator accepts the latest in provider.yml, so a
// comparison near a threshold cannot switch the fleet back and forth.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// The state a publication's decision ends in.
const (
	// the indexer loaded no usable old database: S = 0, each row's latest slot
	ArindbCanaryStateSingle = "single"
	// too few providers carry facts for both databases to compare
	ArindbCanaryStateWarming = "warming"
	// compared, and the breaker holds: S is the latest database
	ArindbCanaryStateLatest = "latest"
	// compared, and the breaker tripped: S is the old database
	ArindbCanaryStateFallback = "fallback"
)

// Why a decision serves its epoch.
const (
	// single: no comparison, each row's latest slot
	ArindbCanaryBasisSingle = "single"
	// latest: the comparison passed
	ArindbCanaryBasisPassed = "passed"
	// fallback: the comparison tripped the breaker
	ArindbCanaryBasisTripped = "tripped"
	// warming: the previous decision's database, which the rows still carry
	ArindbCanaryBasisPrevious = "previous"
	// warming: the old database, which the rows carry
	ArindbCanaryBasisOld = "old"
	// warming: the latest database, which the rows carry, unverified
	ArindbCanaryBasisLatest = "latest"
	// warming: no database is carried by min_coverage of the cohort, so each
	// row's latest slot, as before the canary, rather than one most rows lack
	ArindbCanaryBasisUncovered = "uncovered"
	// warming: the paired sample trips or a fallback is latched, so the old
	// database, which the rows carry short of min_coverage but within the loss
	// the breaker tolerates, rather than the latest
	ArindbCanaryBasisLatestBlocked = "latest_blocked"
	// fallback: the comparison has not yet passed with the release margin
	// for release_passing_runs consecutive runs since the pair fell back
	ArindbCanaryBasisLatched = "latched"
	// latest: a latched fallback released after its passing runs
	ArindbCanaryBasisReleased = "released"
	// latest: the operator accepted this latest database although it trips
	// or was latched
	ArindbCanaryBasisAccepted = "accepted"
	// warming: the public cohort is empty, so nothing is compared and the
	// previous choice carries, each row's latest slot without one
	ArindbCanaryBasisEmptyCohort = "empty_cohort"
)

// The conditions of a decision the stats export publishes and the alerts read.
const (
	// warming's paired sample trips, so the latest is not served: page
	ArindbCanaryFlagWarmingTripped = "warming_tripped"
	// warming serves the latest unverified
	ArindbCanaryFlagWarmingServesLatest = "warming_serves_latest"
	// a fallback is latched on this pair
	ArindbCanaryFlagLatched = "latched"
	// the operator's accept key names this latest
	ArindbCanaryFlagAccepted = "accepted"
	// the publishing Taskworker could not open its latest: page
	ArindbCanaryFlagLatestUnavailable = "latest_unavailable"
	// the pair compared is the previous decision's
	ArindbCanaryFlagPairCarried = "pair_carried"
	// a latched fallback's protection is not served: page
	ArindbCanaryFlagFallbackDropped = "fallback_dropped"
)

// Every condition, in the order the stats gauge publishes them.
var ArindbCanaryFlags = []string{
	ArindbCanaryFlagWarmingTripped,
	ArindbCanaryFlagWarmingServesLatest,
	ArindbCanaryFlagLatched,
	ArindbCanaryFlagAccepted,
	ArindbCanaryFlagLatestUnavailable,
	ArindbCanaryFlagPairCarried,
	ArindbCanaryFlagFallbackDropped,
}

// Every state, in the order the stats gauge publishes them.
var ArindbCanaryStates = []string{
	ArindbCanaryStateSingle,
	ArindbCanaryStateWarming,
	ArindbCanaryStateLatest,
	ArindbCanaryStateFallback,
}

// The decision document's version. A reader refuses any other.
const ArindbCanaryDecisionVersion = 1

// One key in its own hash slot, written by the indexer with the export's ttl.
const arindbCanaryDecisionKey = "{arindb_canary_decision}"

// At most this many emptied cities are named per bucket; the count is
// complete.
const arindbCanaryEmptiedCityLimit = 100

// The serving epoch's changes are kept over this window, at most the limit of
// them, so a canary switching the fleet back and forth pages.
const (
	arindbCanaryServingChangeWindow = 6 * time.Hour
	arindbCanaryServingChangeLimit  = 64
)

// The cohort is compared with its largest size over this window, sampled at
// most once per interval, so a latest database that silently drops providers
// from the cohort shows even while a rolling restart drops them gradually.
const (
	arindbCanaryCohortWindow         = 2 * time.Hour
	arindbCanaryCohortSampleInterval = 10 * time.Minute
)

// The tunables of provider.yml `arindb_canary`.
type ArindbCanarySettings struct {
	// the breaker trips when a bucket's net loss is strictly greater
	BucketLossThreshold float64 `json:"bucket_loss_threshold"`
	// the breaker trips when a country has supply in a bucket under the old
	// database and none under the latest
	TripOnEmptiedCountry bool `json:"trip_on_emptied_country"`
	// the share of the public cohort that must carry facts for a database
	MinCoverage float64 `json:"min_coverage"`
	// paired providers a warming comparison needs before its trip blocks the
	// latest database
	MinPairedSample int `json:"min_paired_sample"`
	// the old supply an emptied country needs in a bucket to trip; smaller
	// ones are reported
	MinEmptiedCountrySupply int `json:"min_emptied_country_supply"`
	// consecutive comparisons that must pass with ReleaseMargin to release a
	// latched fallback
	ReleasePassingRuns int `json:"release_passing_runs"`
	// a releasing comparison's Quality loss stays at or below
	// BucketLossThreshold - ReleaseMargin
	ReleaseMargin float64 `json:"release_margin"`
	// the build epoch of a latest database the operator accepts: it is served
	// although it trips, and its latch is released. 0 accepts none
	AcceptLatestBuildEpoch int64 `json:"accept_latest_build_epoch"`
}

func defaultArindbCanarySettings() ArindbCanarySettings {
	return ArindbCanarySettings{
		BucketLossThreshold:     0.10,
		TripOnEmptiedCountry:    true,
		MinCoverage:             0.98,
		MinPairedSample:         50,
		MinEmptiedCountrySupply: 3,
		ReleasePassingRuns:      6,
		ReleaseMargin:           0.02,
	}
}

func arindbCanarySettings() ArindbCanarySettings {
	return arindbCanarySettingsFromResource(server.Config.SimpleResource(providerConfigResourceName))
}

// Absent keys use the defaults. An unreadable file or an out-of-range value
// keeps the default and logs, so a typo can neither disable nor silently
// tighten the breaker.
func arindbCanarySettingsFromResource(resource *server.SimpleResource, lookupErr error) ArindbCanarySettings {
	settings := defaultArindbCanarySettings()
	if lookupErr != nil || resource == nil {
		if lookupErr != nil && !errors.Is(lookupErr, server.ErrResourceNotFound) {
			glog.Errorf("[arindb]provider config is unavailable (%s); the canary keeps its defaults\n", lookupErr)
		}
		return settings
	}
	var document struct {
		ArindbCanary *struct {
			BucketLossThreshold     *float64 `yaml:"bucket_loss_threshold"`
			TripOnEmptiedCountry    *bool    `yaml:"trip_on_emptied_country"`
			MinCoverage             *float64 `yaml:"min_coverage"`
			MinPairedSample         *int     `yaml:"min_paired_sample"`
			MinEmptiedCountrySupply *int     `yaml:"min_emptied_country_supply"`
			ReleasePassingRuns      *int     `yaml:"release_passing_runs"`
			ReleaseMargin           *float64 `yaml:"release_margin"`
			AcceptLatestBuildEpoch  *int64   `yaml:"accept_latest_build_epoch"`
		} `yaml:"arindb_canary"`
	}
	if err := resource.UnmarshalYamlE(&document); err != nil {
		glog.Errorf("[arindb]provider config arindb_canary is unreadable (%s); the canary keeps its defaults\n", err)
		return settings
	}
	if document.ArindbCanary == nil {
		return settings
	}
	if value := document.ArindbCanary.BucketLossThreshold; value != nil {
		// NaN fails both comparisons
		if 0 <= *value && *value <= 1 {
			settings.BucketLossThreshold = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.bucket_loss_threshold = %v is outside [0, 1]; kept %v\n", *value, settings.BucketLossThreshold)
		}
	}
	if value := document.ArindbCanary.TripOnEmptiedCountry; value != nil {
		settings.TripOnEmptiedCountry = *value
	}
	if value := document.ArindbCanary.MinCoverage; value != nil {
		if 0 < *value && *value <= 1 {
			settings.MinCoverage = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.min_coverage = %v is outside (0, 1]; kept %v\n", *value, settings.MinCoverage)
		}
	}
	if value := document.ArindbCanary.MinPairedSample; value != nil {
		if 1 <= *value {
			settings.MinPairedSample = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.min_paired_sample = %d is below 1; kept %d\n", *value, settings.MinPairedSample)
		}
	}
	if value := document.ArindbCanary.MinEmptiedCountrySupply; value != nil {
		if 1 <= *value {
			settings.MinEmptiedCountrySupply = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.min_emptied_country_supply = %d is below 1; kept %d\n", *value, settings.MinEmptiedCountrySupply)
		}
	}
	if value := document.ArindbCanary.ReleasePassingRuns; value != nil {
		if 1 <= *value {
			settings.ReleasePassingRuns = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.release_passing_runs = %d is below 1; kept %d\n", *value, settings.ReleasePassingRuns)
		}
	}
	if value := document.ArindbCanary.ReleaseMargin; value != nil {
		// NaN fails both comparisons
		if 0 <= *value && *value <= 1 {
			settings.ReleaseMargin = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.release_margin = %v is outside [0, 1]; kept %v\n", *value, settings.ReleaseMargin)
		}
	}
	if value := document.ArindbCanary.AcceptLatestBuildEpoch; value != nil {
		if 0 <= *value {
			settings.AcceptLatestBuildEpoch = *value
		} else {
			glog.Errorf("[arindb]provider config arindb_canary.accept_latest_build_epoch = %d is negative; accepting none\n", *value)
		}
	}
	return settings
}

// The two ARIN slots of one provider's rollup row. Non-quality already
// includes the policy-two unverified rule of its slot.
type arindbRollupSlots struct {
	risk          bool
	nonQuality    bool
	oldRisk       bool
	oldNonQuality bool
	epoch         int64
	oldEpoch      int64
}

// The provider's facts under the database built at epoch: the slot whose
// epoch it is, or unavailable. Epoch 0 names no database.
func (self arindbRollupSlots) factsUnder(epoch int64) (risk bool, nonQuality bool, ok bool) {
	switch {
	case epoch == 0:
		return false, false, false
	case self.epoch == epoch:
		return self.risk, self.nonQuality, true
	case self.oldEpoch == epoch:
		return self.oldRisk, self.oldNonQuality, true
	default:
		return false, false, false
	}
}

// Non-quality under serving epoch S. S = 0 is the latest slot; facts
// unavailable under S are non-Quality, never clean.
func (self arindbRollupSlots) servingNonQuality(servingEpoch int64) bool {
	if servingEpoch == 0 {
		return self.nonQuality
	}
	_, nonQuality, ok := self.factsUnder(servingEpoch)
	return !ok || nonQuality
}

// The rollup's two slots for clientIds, read by primary key in batches.
func loadArindbRollupSlots(ctx context.Context, clientIds []server.Id) map[server.Id]arindbRollupSlots {
	slots := map[server.Id]arindbRollupSlots{}
	if len(clientIds) == 0 {
		return slots
	}
	server.Db(ctx, func(conn server.PgConn) {
		const batchSize = 1024
		for start := 0; start < len(clientIds); start += batchSize {
			batch := clientIds[start:min(start+batchSize, len(clientIds))]
			rows, err := conn.Query(
				ctx,
				`
				SELECT
					client_id,
					arin_risk,
					arin_non_quality,
					arin_old_risk,
					arin_old_non_quality,
					arin_epoch,
					arin_old_epoch
				FROM network_client_location_reliability
				WHERE client_id = ANY($1)
				`,
				batch,
			)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					var s arindbRollupSlots
					server.Raise(rows.Scan(&clientId, &s.risk, &s.nonQuality, &s.oldRisk, &s.oldNonQuality, &s.epoch, &s.oldEpoch))
					slots[clientId] = s
				}
			})
		}
	})
	return slots
}

// The pair the indexer compares, as its own loader opened it.
type arindbCanaryPair struct {
	latest server.ArinDatabaseIdentity
	old    server.ArinDatabaseIdentity
	// the latest could not be opened
	err error
}

// The pair of this process. Taskworker opens it at its first score
// publication and performs no lookups. Replaced in tests.
var arindbCanaryPairIdentities = server.ArinDatabaseIdentities

func currentArindbCanaryPair() arindbCanaryPair {
	latest, old, err := arindbCanaryPairIdentities()
	return arindbCanaryPair{latest: latest, old: old, err: err}
}

// The latest epoch L, 0 when the latest is not open.
func (self arindbCanaryPair) latestEpoch() int64 {
	if self.err != nil || self.latest.State != server.ArinDatabaseLoaded {
		return 0
	}
	return self.latest.BuildEpoch
}

// The old epoch O, 0 unless a strictly older old database is loaded beside an
// open latest.
func (self arindbCanaryPair) oldEpoch() int64 {
	latestEpoch := self.latestEpoch()
	if latestEpoch == 0 || self.old.State != server.ArinDatabaseLoaded || self.old.BuildEpoch <= 0 || latestEpoch <= self.old.BuildEpoch {
		return 0
	}
	return self.old.BuildEpoch
}

// The pair a decision compares, and why.
type arindbCanaryResolvedPair struct {
	latest      ArindbCanaryDatabase
	old         ArindbCanaryDatabase
	latestEpoch int64
	// 0 when nothing can be compared: the canary is single
	oldEpoch int64
	// why this process's own pair cannot compare, "" when it can
	reason string
	// reason, when the previous decision's epochs stand in for this process's
	carriedReason     string
	latestUnavailable bool
}

// The comparison needs only the pair's epochs, since the rows carry the facts.
// A Taskworker that cannot use its own pair compares the previous decision's
// instead: both epochs when it cannot open its latest, the old one beside the
// same latest otherwise. A damaged file on the publishing host therefore
// cannot silently drop a fallback.
func resolveArindbCanaryPair(pair arindbCanaryPair, previous *ArindbCanaryDecision) arindbCanaryResolvedPair {
	resolved := arindbCanaryResolvedPair{
		latest:      ArindbCanaryDatabase{State: pair.latest.State, BuildEpoch: pair.latest.BuildEpoch, Sha256: pair.latest.Sha256},
		old:         ArindbCanaryDatabase{State: pair.old.State, BuildEpoch: pair.old.BuildEpoch, Sha256: pair.old.Sha256},
		latestEpoch: pair.latestEpoch(),
		oldEpoch:    pair.oldEpoch(),
	}
	switch {
	case pair.err != nil:
		resolved.reason = "latest_unopened"
	case resolved.latestEpoch == 0:
		resolved.reason = "latest_" + pair.latest.State
	case resolved.oldEpoch == 0:
		resolved.reason = "old_" + pair.old.State
	}
	resolved.latestUnavailable = resolved.latestEpoch == 0
	if resolved.reason != "" && previous != nil && previous.State != ArindbCanaryStateSingle &&
		0 < previous.Old.BuildEpoch && previous.Old.BuildEpoch < previous.Latest.BuildEpoch &&
		(resolved.latestEpoch == 0 || resolved.latestEpoch == previous.Latest.BuildEpoch) {
		if resolved.latestEpoch == 0 {
			resolved.latest = previous.Latest
			resolved.latestEpoch = previous.Latest.BuildEpoch
		}
		resolved.old = previous.Old
		resolved.oldEpoch = previous.Old.BuildEpoch
		resolved.carriedReason = resolved.reason
	}
	return resolved
}

// One public provider of the cohort as the canary compares it.
type arindbCanaryProvider struct {
	clientId server.Id
	// the published country, normalized; "" when unknown
	countryCode string
	// the published city; nil when none
	cityId *server.Id
	slots  arindbRollupSlots
	// what the unchanged rules read besides ARIN
	egress  *clientScoreEgress
	weights map[int]float64
}

// Bucket membership under one database's ARIN facts.
type arindbCanaryMembership struct {
	quality bool
	speed   bool
	online  bool
}

func (self arindbCanaryMembership) in(bucket string) bool {
	switch bucket {
	case RankModeQuality:
		return self.quality
	case RankModeSpeed:
		return self.speed
	case ProviderEgressBucketOnline:
		return self.online
	default:
		return false
	}
}

// One database of the decision.
type ArindbCanaryDatabase struct {
	State      string `json:"state"`
	BuildEpoch int64  `json:"build_epoch"`
	// "" when no manifest names one
	Sha256 string `json:"sha256"`
}

// How many public providers of the cohort carry facts for each database.
type ArindbCanaryCoverage struct {
	Providers int `json:"providers"`
	Latest    int `json:"latest"`
	Old       int `json:"old"`
	Paired    int `json:"paired"`
}

// The share of the cohort count covers; an empty cohort is fully covered,
// though it compares nothing and the canary warms.
func (self ArindbCanaryCoverage) Ratio(count int) float64 {
	if self.Providers == 0 {
		return 1
	}
	return float64(count) / float64(self.Providers)
}

// Paired providers in one bucket or one country of it, under each database.
type ArindbCanaryCount struct {
	Latest int `json:"latest"`
	Old    int `json:"old"`
}

// A country with supply under the old database and none under the latest.
type ArindbCanaryEmptiedCountry struct {
	CountryCode string `json:"country_code"`
	Old         int    `json:"old"`
	// the old supply reaches min_emptied_country_supply, so the country trips
	// when trip_on_emptied_country is set
	Trips bool `json:"trips"`
}

// A city with supply under the old database and none under the latest. Cities
// are reported, never tripped on.
type ArindbCanaryEmptiedCity struct {
	CityLocationId server.Id `json:"city_location_id"`
	CountryCode    string    `json:"country_code"`
	Name           string    `json:"name"`
	Old            int       `json:"old"`
}

// One bucket of the comparison, over the paired providers.
type ArindbCanaryBucket struct {
	ArindbCanaryCount
	// the net loss (old - latest) / old; negative is a gain, 0 without old supply
	LossRatio float64 `json:"loss_ratio"`
	// the net gain (latest - old) / old, 0 for a loss or without old supply: a
	// flood the breaker does not trip on, which pages
	GainRatio float64 `json:"gain_ratio"`
	// the gross churn (removed + added) / old, 0 without old supply: a
	// reshuffle the net loss hides, which pages
	ChurnRatio float64 `json:"churn_ratio"`
	// gross removals and additions of latest against old
	Removed int `json:"removed"`
	Added   int `json:"added"`
	// keyed by country code; providers without a published country count only
	// in the bucket
	Countries        map[string]ArindbCanaryCount `json:"countries"`
	EmptiedCountries []ArindbCanaryEmptiedCountry `json:"emptied_countries"`
	EmptiedCityCount int                          `json:"emptied_city_count"`
	EmptiedCities    []ArindbCanaryEmptiedCity    `json:"emptied_cities"`
	// the net loss is over the threshold, or an emptied country trips. Only a
	// Quality trip falls back; a Speed or Online trip is risk, which the
	// fallback cannot repair, and pages
	Tripped bool `json:"tripped"`
}

// One sample of the public cohort's size.
type ArindbCanaryCohortSample struct {
	At        time.Time `json:"at"`
	Providers int       `json:"providers"`
}

// The public cohort against its recent size. Providers whose latest lookup
// fails never reach the cohort, so the comparison cannot see them; a cohort
// that shrinks against its recent size can.
type ArindbCanaryCohort struct {
	// at most one sample per ten minutes over the last two hours, oldest first
	History []ArindbCanaryCohortSample `json:"history"`
	// the largest cohort of the history before this decision, 0 without one
	ReferenceProviders int `json:"reference_providers"`
	// (reference - providers) / reference, 0 when the cohort did not shrink
	LossRatio float64 `json:"loss_ratio"`
}

// The serving epoch's changes over the window, this decision's included: a
// decision serving another epoch than the previous one is a change.
func arindbCanaryServingChangesAfter(previous *ArindbCanaryDecision, servingEpoch int64, decidedAt time.Time) []time.Time {
	changes := []time.Time{}
	if previous == nil {
		return changes
	}
	for _, at := range previous.ServingChanges {
		if !at.After(decidedAt) && decidedAt.Sub(at) <= arindbCanaryServingChangeWindow {
			changes = append(changes, at)
		}
	}
	if previous.ServingEpoch != servingEpoch {
		changes = append(changes, decidedAt)
	}
	return changes[max(0, len(changes)-arindbCanaryServingChangeLimit):]
}

// A latch the decision can no longer honour. A held latch is dropped when the
// canary goes single after it, or when the pair changes to one whose old
// database is the latest the latch held out, so the comparison's baseline is
// the rejected database. A latch released by passing comparisons, or a new
// pair whose old database is the latch's own, drops nothing.
//
// Once dropped, the latch lasts however the pairs move on, since every later
// baseline descends from the rejected latest, until its latest is accepted or
// the canary compares against the latch's old database again: a latest
// decision verifies a release against it, and a latch on such a pair protects
// it anew.
func arindbCanaryDroppedLatch(previous *ArindbCanaryDecision, decision *ArindbCanaryDecision, settings ArindbCanarySettings) *ArindbCanaryLatch {
	if previous == nil {
		return nil
	}
	var candidate *ArindbCanaryLatch
	switch {
	case previous.DroppedLatch != nil:
		candidate = previous.DroppedLatch
	case previous.Latch != nil && decision.Latch == nil &&
		(decision.State == ArindbCanaryStateSingle || decision.Old.BuildEpoch == previous.Latch.LatestEpoch):
		candidate = previous.Latch
	default:
		return nil
	}
	if settings.AcceptLatestBuildEpoch != 0 && settings.AcceptLatestBuildEpoch == candidate.LatestEpoch {
		return nil
	}
	if decision.State != ArindbCanaryStateSingle && decision.Old.BuildEpoch == candidate.OldEpoch &&
		(decision.State == ArindbCanaryStateLatest || decision.Latch != nil) {
		return nil
	}
	dropped := *candidate
	return &dropped
}

// The cohort of this decision against the previous decision's history.
func arindbCanaryCohortAfter(previous *ArindbCanaryDecision, providers int, decidedAt time.Time) ArindbCanaryCohort {
	cohort := ArindbCanaryCohort{History: []ArindbCanaryCohortSample{}}
	if previous != nil {
		for _, sample := range previous.Cohort.History {
			if !sample.At.After(decidedAt) && decidedAt.Sub(sample.At) <= arindbCanaryCohortWindow {
				cohort.History = append(cohort.History, sample)
				cohort.ReferenceProviders = max(cohort.ReferenceProviders, sample.Providers)
			}
		}
	}
	if 0 < cohort.ReferenceProviders && providers < cohort.ReferenceProviders {
		cohort.LossRatio = float64(cohort.ReferenceProviders-providers) / float64(cohort.ReferenceProviders)
	}
	if count := len(cohort.History); count == 0 || arindbCanaryCohortSampleInterval <= decidedAt.Sub(cohort.History[count-1].At) {
		cohort.History = append(cohort.History, ArindbCanaryCohortSample{At: decidedAt, Providers: providers})
	}
	return cohort
}

// A fallback latched on one pair of databases. It holds, and is carried
// through warming, until release_passing_runs consecutive comparisons of the
// pair pass with release_margin, or the operator accepts the latest.
type ArindbCanaryLatch struct {
	LatestEpoch int64     `json:"latest_epoch"`
	OldEpoch    int64     `json:"old_epoch"`
	Since       time.Time `json:"since"`
	// consecutive comparisons since the last that did not pass with the margin
	PassingRuns int `json:"passing_runs"`
}

// One publication's choice, as the indexer published it.
type ArindbCanaryDecision struct {
	Version      int    `json:"version"`
	DecisionId   string `json:"decision_id"`
	State        string `json:"state"`
	ServingEpoch int64  `json:"serving_epoch"`
	// why the state serves its epoch, one of the ArindbCanaryBasis values
	ServingBasis string `json:"serving_basis"`
	// the decision this one followed, "" without one
	PreviousDecisionId string `json:"previous_decision_id,omitempty"`
	// the process that published it, as ArindbCanaryPublisher names it. Only
	// that process's stats collector exports the per-country supply, so the
	// largest series is exported once rather than by every Taskworker
	Publisher string `json:"publisher"`
	// when the serving epoch changed over the last six hours, this decision
	// included, oldest first
	ServingChanges []time.Time `json:"serving_changes"`
	// warming: the paired sample reached min_paired_sample and its Quality
	// comparison trips, so the latest is not served
	WarmingTripped bool `json:"warming_tripped"`
	// warming serves the latest database, unverified by a comparison
	WarmingServesLatest bool `json:"warming_serves_latest"`
	// the fallback latched on this pair, nil without one
	Latch *ArindbCanaryLatch `json:"latch,omitempty"`
	// this Taskworker could not open its latest database
	LatestUnavailable bool `json:"latest_unavailable"`
	// the pair compared is the previous decision's because this Taskworker
	// could not use its own: latest_<state> or old_<state>; "" otherwise
	PairCarriedReason string `json:"pair_carried_reason,omitempty"`
	// a latched fallback's protection is not served: a held latch while S is
	// not its old database, or a dropped latch
	FallbackDropped bool `json:"fallback_dropped"`
	// when this pair of databases was first compared: the previous decision's
	// while the pair is unchanged, this decision's otherwise. A cohort that
	// shrinks soon after it is attributable to the release
	PairSince time.Time `json:"pair_since"`
	// a latch the decision can no longer honour: the canary went single after
	// it, or the pair changed to one whose old database is the latest the latch
	// held out. Carried until its latest is accepted or the canary compares
	// against its old database again; nil otherwise
	DroppedLatch *ArindbCanaryLatch `json:"dropped_latch,omitempty"`
	// provider.yml accept_latest_build_epoch names this latest database
	Accepted bool `json:"accepted"`
	// why the state is single, "" otherwise
	SingleReason string                         `json:"single_reason,omitempty"`
	Latest       ArindbCanaryDatabase           `json:"latest"`
	Old          ArindbCanaryDatabase           `json:"old"`
	Coverage     ArindbCanaryCoverage           `json:"coverage"`
	Cohort       ArindbCanaryCohort             `json:"cohort"`
	Settings     ArindbCanarySettings           `json:"settings"`
	Buckets      map[string]*ArindbCanaryBucket `json:"buckets"`
	// the start of the score source read the decision counts
	SourceStartedAt time.Time `json:"source_started_at"`
	DecidedAt       time.Time `json:"decided_at"`
}

// The providers of the cohort with facts under the database built at epoch.
func arindbCanaryCoverageOf(providers []*arindbCanaryProvider, epoch int64) int {
	count := 0
	for _, provider := range providers {
		if _, _, ok := provider.slots.factsUnder(epoch); ok {
			count += 1
		}
	}
	return count
}

// Decides one publication. membership applies the unchanged rules
// (decideProviderEgress and the reliability minimums) with only the ARIN
// facts substituted; under them risk can remove a provider from every bucket
// and non-quality only from Quality. previous is the decision the last
// publication wrote, nil without a readable one.
func decideArindbCanary(
	pair arindbCanaryPair,
	providers []*arindbCanaryProvider,
	membership func(provider *arindbCanaryProvider, risk bool, nonQuality bool) arindbCanaryMembership,
	settings ArindbCanarySettings,
	previous *ArindbCanaryDecision,
	sourceStartedAt time.Time,
	decidedAt time.Time,
) (decision *ArindbCanaryDecision) {
	decision = &ArindbCanaryDecision{
		Version:         ArindbCanaryDecisionVersion,
		DecisionId:      server.NewId().String(),
		Settings:        settings,
		Buckets:         map[string]*ArindbCanaryBucket{},
		SourceStartedAt: sourceStartedAt,
		DecidedAt:       decidedAt,
	}
	if previous != nil {
		decision.PreviousDecisionId = previous.DecisionId
	}
	// every state below counts a change of the serving epoch, and says
	// whether a latched fallback's protection is still served
	defer func() {
		decision.ServingChanges = arindbCanaryServingChangesAfter(previous, decision.ServingEpoch, decidedAt)
		decision.DroppedLatch = arindbCanaryDroppedLatch(previous, decision, settings)
		decision.FallbackDropped = decision.DroppedLatch != nil ||
			(decision.Latch != nil && decision.ServingEpoch != decision.Latch.OldEpoch)
	}()
	for _, bucket := range ProviderEgressBuckets {
		decision.Buckets[bucket] = &ArindbCanaryBucket{
			Countries:        map[string]ArindbCanaryCount{},
			EmptiedCountries: []ArindbCanaryEmptiedCountry{},
			EmptiedCities:    []ArindbCanaryEmptiedCity{},
		}
	}

	resolved := resolveArindbCanaryPair(pair, previous)
	latestEpoch, oldEpoch := resolved.latestEpoch, resolved.oldEpoch
	decision.Latest, decision.Old = resolved.latest, resolved.old
	decision.LatestUnavailable = resolved.latestUnavailable
	decision.PairCarriedReason = resolved.carriedReason
	pairReason := resolved.reason
	decision.Coverage.Providers = len(providers)
	decision.Cohort = arindbCanaryCohortAfter(previous, len(providers), decidedAt)
	decision.PairSince = decidedAt
	if previous != nil && !previous.PairSince.IsZero() &&
		previous.Latest.BuildEpoch == decision.Latest.BuildEpoch && previous.Old.BuildEpoch == decision.Old.BuildEpoch {
		decision.PairSince = previous.PairSince
	}
	for _, provider := range providers {
		_, _, latestOk := provider.slots.factsUnder(latestEpoch)
		_, _, oldOk := provider.slots.factsUnder(oldEpoch)
		if latestOk {
			decision.Coverage.Latest += 1
		}
		if oldOk {
			decision.Coverage.Old += 1
		}
		if latestOk && oldOk {
			decision.Coverage.Paired += 1
		}
	}
	if oldEpoch == 0 {
		decision.State = ArindbCanaryStateSingle
		decision.ServingEpoch = 0
		decision.ServingBasis = ArindbCanaryBasisSingle
		decision.SingleReason = pairReason
		return decision
	}

	// compare the providers with facts for both databases
	type cityCount struct {
		countryCode string
		count       ArindbCanaryCount
	}
	bucketCityCounts := map[string]map[server.Id]*cityCount{}
	for _, bucket := range ProviderEgressBuckets {
		bucketCityCounts[bucket] = map[server.Id]*cityCount{}
	}
	for _, provider := range providers {
		latestRisk, latestNonQuality, latestOk := provider.slots.factsUnder(latestEpoch)
		oldRisk, oldNonQuality, oldOk := provider.slots.factsUnder(oldEpoch)
		if !latestOk || !oldOk {
			continue
		}
		underLatest := membership(provider, latestRisk, latestNonQuality)
		underOld := membership(provider, oldRisk, oldNonQuality)
		for _, bucket := range ProviderEgressBuckets {
			inLatest, inOld := underLatest.in(bucket), underOld.in(bucket)
			if !inLatest && !inOld {
				continue
			}
			add := func(count *ArindbCanaryCount) {
				if inLatest {
					count.Latest += 1
				}
				if inOld {
					count.Old += 1
				}
			}
			b := decision.Buckets[bucket]
			add(&b.ArindbCanaryCount)
			if inOld && !inLatest {
				b.Removed += 1
			}
			if inLatest && !inOld {
				b.Added += 1
			}
			if provider.countryCode != "" {
				count := b.Countries[provider.countryCode]
				add(&count)
				b.Countries[provider.countryCode] = count
			}
			if provider.cityId != nil {
				city, ok := bucketCityCounts[bucket][*provider.cityId]
				if !ok {
					city = &cityCount{countryCode: provider.countryCode}
					bucketCityCounts[bucket][*provider.cityId] = city
				}
				add(&city.count)
			}
		}
	}

	for _, bucket := range ProviderEgressBuckets {
		b := decision.Buckets[bucket]
		if 0 < b.Old {
			b.LossRatio = float64(b.Old-b.Latest) / float64(b.Old)
			b.GainRatio = max(0, -b.LossRatio)
			b.ChurnRatio = float64(b.Removed+b.Added) / float64(b.Old)
		}
		for countryCode, count := range b.Countries {
			if 0 < count.Old && count.Latest == 0 {
				b.EmptiedCountries = append(b.EmptiedCountries, ArindbCanaryEmptiedCountry{
					CountryCode: countryCode,
					Old:         count.Old,
					Trips:       settings.MinEmptiedCountrySupply <= count.Old,
				})
			}
		}
		slices.SortFunc(b.EmptiedCountries, func(x, y ArindbCanaryEmptiedCountry) int {
			if x.Old != y.Old {
				return y.Old - x.Old
			}
			return strings.Compare(x.CountryCode, y.CountryCode)
		})
		for cityId, city := range bucketCityCounts[bucket] {
			if 0 < city.count.Old && city.count.Latest == 0 {
				b.EmptiedCities = append(b.EmptiedCities, ArindbCanaryEmptiedCity{CityLocationId: cityId, CountryCode: city.countryCode, Old: city.count.Old})
			}
		}
		slices.SortFunc(b.EmptiedCities, func(x, y ArindbCanaryEmptiedCity) int {
			if x.Old != y.Old {
				return y.Old - x.Old
			}
			return strings.Compare(x.CityLocationId.String(), y.CityLocationId.String())
		})
		b.EmptiedCityCount = len(b.EmptiedCities)
		b.EmptiedCities = b.EmptiedCities[:min(len(b.EmptiedCities), arindbCanaryEmptiedCityLimit)]
		// net loss, so a release that moves as many providers in as out passes.
		// A country trips only with min_emptied_country_supply under the old
		// database, so one provider connecting and disconnecting cannot.
		lossTrip := 0 < b.Old && settings.BucketLossThreshold < b.LossRatio
		countryTrip := settings.TripOnEmptiedCountry && slices.ContainsFunc(b.EmptiedCountries, func(country ArindbCanaryEmptiedCountry) bool {
			return country.Trips
		})
		b.Tripped = lossTrip || countryTrip
	}
	// Only a Quality trip falls back: the serving epoch changes only Quality
	// classification, since risk always comes from the latest slot. A Speed
	// or Online trip, which only risk can cause, is reported and pages
	// without changing S, as falling back would repair none of it.
	tripped := decision.Buckets[RankModeQuality].Tripped

	// Hysteresis: a fallback latches on its pair. The operator's accept key
	// overrides a trip and releases the latch.
	accepted := settings.AcceptLatestBuildEpoch != 0 && settings.AcceptLatestBuildEpoch == latestEpoch
	decision.Accepted = accepted
	var latch *ArindbCanaryLatch
	if !accepted && previous != nil && previous.Latch != nil &&
		previous.Latch.LatestEpoch == latestEpoch && previous.Latch.OldEpoch == oldEpoch {
		carriedLatch := *previous.Latch
		latch = &carriedLatch
	}
	// a releasing comparison passes with the margin: Quality's loss stays at
	// or below threshold - margin and no country with min supply empties
	quality := decision.Buckets[RankModeQuality]
	passesWithMargin := (quality.Old == 0 || quality.LossRatio <= settings.BucketLossThreshold-settings.ReleaseMargin) &&
		!(settings.TripOnEmptiedCountry && slices.ContainsFunc(quality.EmptiedCountries, func(country ArindbCanaryEmptiedCountry) bool {
			return country.Trips
		}))

	if decision.Coverage.Providers == 0 {
		// An empty cohort compares nothing and verifies no latest: warming
		// carries the previous choice and any latch, or serves each row's
		// latest slot without a previous decision.
		decision.State = ArindbCanaryStateWarming
		decision.ServingBasis = ArindbCanaryBasisEmptyCohort
		decision.Latch = latch
		if previous != nil {
			decision.ServingEpoch = previous.ServingEpoch
		}
		decision.WarmingServesLatest = decision.ServingEpoch != 0 && decision.ServingEpoch == latestEpoch
		return decision
	}

	minimum := settings.MinCoverage * float64(decision.Coverage.Providers)
	// a database is carried when min_coverage of the cohort has its facts;
	// epoch 0 names none
	carried := func(epoch int64, coverage int) bool {
		return epoch != 0 && minimum <= float64(coverage)
	}
	switch {
	case float64(decision.Coverage.Paired) < minimum:
		// serve a database the rows carry while processes restart onto the
		// pair and the rollup refreshes. Serving one most rows lack would make
		// them unavailable, so non-Quality, so with none carried every row
		// serves its latest slot, as before the canary. The previous choice
		// is kept while the rows still carry it, so coverage hovering near
		// min_coverage cannot switch the fleet between the databases.
		// A paired sample of min_paired_sample whose Quality comparison trips
		// blocks the latest: it is evidence enough not to serve a database
		// that would fall back once covered. Then the old serves while the
		// providers it lacks are within the loss the breaker tolerates.
		decision.State = ArindbCanaryStateWarming
		decision.WarmingTripped = settings.MinPairedSample <= decision.Coverage.Paired && tripped && !accepted
		// a latch holds through warming unchanged: warming neither passes nor
		// fails a comparison
		decision.Latch = latch
		latestBlocked := decision.WarmingTripped || latch != nil
		switch {
		case previous != nil && !(latestBlocked && previous.ServingEpoch == latestEpoch) &&
			carried(previous.ServingEpoch, arindbCanaryCoverageOf(providers, previous.ServingEpoch)):
			decision.ServingEpoch = previous.ServingEpoch
			decision.ServingBasis = ArindbCanaryBasisPrevious
		case carried(oldEpoch, decision.Coverage.Old):
			decision.ServingEpoch = oldEpoch
			decision.ServingBasis = ArindbCanaryBasisOld
		case !latestBlocked && carried(latestEpoch, decision.Coverage.Latest):
			decision.ServingEpoch = latestEpoch
			decision.ServingBasis = ArindbCanaryBasisLatest
		case latestBlocked && (1-settings.BucketLossThreshold)*float64(decision.Coverage.Providers) <= float64(decision.Coverage.Old):
			decision.ServingEpoch = oldEpoch
			decision.ServingBasis = ArindbCanaryBasisLatestBlocked
		default:
			decision.ServingEpoch = 0
			decision.ServingBasis = ArindbCanaryBasisUncovered
		}
		decision.WarmingServesLatest = decision.ServingEpoch == latestEpoch
	case accepted:
		decision.State = ArindbCanaryStateLatest
		decision.ServingEpoch = latestEpoch
		decision.ServingBasis = ArindbCanaryBasisPassed
		if tripped || (previous != nil && previous.Latch != nil && previous.Latch.LatestEpoch == latestEpoch && previous.Latch.OldEpoch == oldEpoch) {
			decision.ServingBasis = ArindbCanaryBasisAccepted
		}
	case latch != nil:
		if passesWithMargin {
			latch.PassingRuns += 1
		} else {
			latch.PassingRuns = 0
		}
		if settings.ReleasePassingRuns <= latch.PassingRuns {
			decision.State = ArindbCanaryStateLatest
			decision.ServingEpoch = latestEpoch
			decision.ServingBasis = ArindbCanaryBasisReleased
		} else {
			decision.State = ArindbCanaryStateFallback
			decision.ServingEpoch = oldEpoch
			decision.ServingBasis = ArindbCanaryBasisLatched
			decision.Latch = latch
		}
	case tripped:
		decision.State = ArindbCanaryStateFallback
		decision.ServingEpoch = oldEpoch
		decision.ServingBasis = ArindbCanaryBasisTripped
		decision.Latch = &ArindbCanaryLatch{LatestEpoch: latestEpoch, OldEpoch: oldEpoch, Since: decidedAt}
	default:
		decision.State = ArindbCanaryStateLatest
		decision.ServingEpoch = latestEpoch
		decision.ServingBasis = ArindbCanaryBasisPassed
	}
	return decision
}

// Quality facts follow the serving epoch for every client of the
// publication, Network-only included; risk keeps each row's latest slot. A
// client the slot read did not return, such as a rollup row replaced between
// the source read and the slot read, has no facts under S and is non-Quality.
// S = 0 leaves the filter as the rollup's latest slots loaded it.
func applyArindbServingEpoch(filter *providerCountFilter, clientIds []server.Id, slots map[server.Id]arindbRollupSlots, servingEpoch int64) {
	filter.arindbServingEpoch = servingEpoch
	if servingEpoch == 0 {
		return
	}
	for _, clientId := range clientIds {
		if s, ok := slots[clientId]; !ok || s.servingNonQuality(servingEpoch) {
			filter.arinNonQuality[clientId] = true
		} else {
			delete(filter.arinNonQuality, clientId)
		}
	}
}

// Reads the rollup's two slots; replaced in tests.
var arindbCanaryLoadSlots = loadArindbRollupSlots

// One comparison provider per public client, placed by its location row,
// with every lookback weight its rows carry; and every client of the cohort,
// group-only and Network-only clients included.
func arindbCanaryProvidersFromRows(rows []clientScoreSourceRow) (providers []*arindbCanaryProvider, cohortIds []server.Id) {
	weights := map[server.Id]map[int]float64{}
	placedClientIds := map[server.Id]bool{}
	for _, row := range rows {
		clientId := row.score.ClientId
		clientWeights, ok := weights[clientId]
		if !ok {
			clientWeights = map[int]float64{}
			weights[clientId] = clientWeights
			cohortIds = append(cohortIds, clientId)
		}
		clientWeights[row.score.LookbackIndex] = row.score.IndependentReliabilityWeight
		if row.locationGroup || row.score.NetworkOnly || placedClientIds[clientId] {
			continue
		}
		placedClientIds[clientId] = true
		providers = append(providers, &arindbCanaryProvider{
			clientId:    clientId,
			countryCode: normalizeCountryCode(row.egress.publishedCountryCode),
			cityId:      row.cityId,
			egress:      row.egress,
			weights:     clientWeights,
		})
	}
	return
}

// The unchanged rules of the export, decideProviderEgress and the reliability
// minimums, with only the ARIN facts substituted.
func arindbCanaryMembershipRule(
	countFilter *providerCountFilter,
	egressSettings *EgressIndexSettings,
	egressTestEnabled bool,
	minIndependentReliabilityWeights map[int]float64,
) func(provider *arindbCanaryProvider, risk bool, nonQuality bool) arindbCanaryMembership {
	return func(provider *arindbCanaryProvider, risk bool, nonQuality bool) arindbCanaryMembership {
		facts := countFilter.egressFacts(
			provider.clientId,
			provider.egress.publishedCountryCode,
			provider.egress.egressIndex,
			provider.egress.egressQuality,
			egressSettings,
		)
		facts.arinRisk = risk
		facts.arinNonQuality = nonQuality
		decision := decideProviderEgress(facts, egressTestEnabled)
		passes := providerReliabilityPasses(provider.weights, minIndependentReliabilityWeights)
		return arindbCanaryMembership{
			quality: decision.quality && passes,
			speed:   decision.speed && passes,
			online:  decision.online && passes,
		}
	}
}

// Decides this publication from its staged source rows and applies the
// serving epoch to the export's filter, before any provider is decided. The
// filter must hold the cohort's common gates and the URL-success guard. The
// slots are read in their own primary-key query beside the source rows; the
// pinned source and filter statements are unchanged. The decision follows the
// previous one, so a failed read of it stops the export, like a failed write.
func decideArindbCanaryForPublication(
	ctx context.Context,
	sourceStartedAt time.Time,
	rows []clientScoreSourceRow,
	countFilter *providerCountFilter,
	egressSettings *EgressIndexSettings,
	egressTestEnabled bool,
	minIndependentReliabilityWeights map[int]float64,
) (*ArindbCanaryDecision, error) {
	previous, err := readPreviousArindbCanaryDecision(ctx)
	if err != nil {
		return nil, err
	}
	pair := currentArindbCanaryPair()
	if pair.err != nil {
		glog.Errorf("[arindb]the latest ARIN database is not open in this process (%s); the canary is single\n", pair.err)
	}
	providers, cohortIds := arindbCanaryProvidersFromRows(rows)
	// single compares nothing and serves each row's latest slot, so it skips
	// the slot read
	slots := map[server.Id]arindbRollupSlots{}
	if resolveArindbCanaryPair(pair, previous).oldEpoch != 0 {
		slots = arindbCanaryLoadSlots(ctx, cohortIds)
	}
	for _, provider := range providers {
		provider.slots = slots[provider.clientId]
	}
	membership := arindbCanaryMembershipRule(countFilter, egressSettings, egressTestEnabled, minIndependentReliabilityWeights)
	decision := decideArindbCanary(pair, providers, membership, arindbCanarySettings(), previous, sourceStartedAt, server.NowUtc())
	// names are best effort; a failed read leaves the ids and counts standing
	if r := server.HandleError(func() {
		nameArindbCanaryCities(ctx, decision)
	}); r != nil {
		glog.Errorf("[arindb]emptied city names unavailable: %v\n", r)
	}
	decision.Publisher = ArindbCanaryPublisher()
	applyArindbServingEpoch(countFilter, cohortIds, slots, decision.ServingEpoch)
	glog.Infof("[arindb]%s\n", decision.logSummary())
	return decision, nil
}

// The decision the last publication wrote, nil when none is published. A
// document this binary cannot read is logged and treated as absent, so it
// cannot stop every export until it expires; a failed read is an error.
func readPreviousArindbCanaryDecision(ctx context.Context) (*ArindbCanaryDecision, error) {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := arindbCanaryDecisions.get(readCtx)
	if err != nil {
		return nil, fmt.Errorf("read the previous arindb canary decision: %w", err)
	}
	if data == nil {
		return nil, nil
	}
	previous, err := parseArindbCanaryDecision(data)
	if err != nil {
		glog.Errorf("[arindb]the previous canary decision is unreadable (%s); deciding without it\n", err)
		return nil, nil
	}
	return previous, nil
}

// Names the emptied cities of every bucket from their location rows.
func nameArindbCanaryCities(ctx context.Context, decision *ArindbCanaryDecision) {
	cityIds := []server.Id{}
	for _, bucket := range decision.Buckets {
		for _, city := range bucket.EmptiedCities {
			cityIds = append(cityIds, city.CityLocationId)
		}
	}
	if len(cityIds) == 0 {
		return
	}
	names := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT location_id, location_name FROM location WHERE location_id = ANY($1)`, cityIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var locationId server.Id
				var name string
				server.Raise(rows.Scan(&locationId, &name))
				names[locationId] = name
			}
		})
	})
	for _, bucket := range decision.Buckets {
		for i := range bucket.EmptiedCities {
			bucket.EmptiedCities[i].Name = names[bucket.EmptiedCities[i].CityLocationId]
		}
	}
}

// One line for the indexer log, emptied countries and cities named.
func (self *ArindbCanaryDecision) logSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "canary state=%s serving=%d basis=%s latest=%d(%s) old=%d(%s)", self.State, self.ServingEpoch, self.ServingBasis,
		self.Latest.BuildEpoch, self.Latest.State, self.Old.BuildEpoch, self.Old.State)
	if self.SingleReason != "" {
		fmt.Fprintf(&b, " reason=%s", self.SingleReason)
	}
	if self.PairCarriedReason != "" {
		fmt.Fprintf(&b, " pair_carried=%s", self.PairCarriedReason)
	}
	if self.LatestUnavailable {
		b.WriteString(" latest_unavailable")
	}
	if self.FallbackDropped {
		b.WriteString(" fallback_dropped")
	}
	if self.DroppedLatch != nil {
		fmt.Fprintf(&b, " dropped_latch=%d/%d", self.DroppedLatch.LatestEpoch, self.DroppedLatch.OldEpoch)
	}
	fmt.Fprintf(&b, " coverage latest=%d old=%d paired=%d of %d", self.Coverage.Latest, self.Coverage.Old, self.Coverage.Paired, self.Coverage.Providers)
	if 0 < self.Cohort.LossRatio {
		fmt.Fprintf(&b, " cohort_loss=%.4f of %d", self.Cohort.LossRatio, self.Cohort.ReferenceProviders)
	}
	if self.WarmingTripped {
		b.WriteString(" warming_tripped")
	}
	if self.WarmingServesLatest {
		b.WriteString(" warming_serves_latest")
	}
	if self.Latch != nil {
		fmt.Fprintf(&b, " latched_since=%s passing_runs=%d", self.Latch.Since.Format(time.RFC3339), self.Latch.PassingRuns)
	}
	if self.Accepted {
		b.WriteString(" accepted")
	}
	for _, bucket := range ProviderEgressBuckets {
		b2 := self.Buckets[bucket]
		fmt.Fprintf(&b, "; %s old=%d latest=%d loss=%.4f churn=%.4f removed=%d added=%d tripped=%t", bucket, b2.Old, b2.Latest, b2.LossRatio, b2.ChurnRatio, b2.Removed, b2.Added, b2.Tripped)
		if 0 < len(b2.EmptiedCountries) {
			countries := []string{}
			for _, country := range b2.EmptiedCountries {
				countries = append(countries, fmt.Sprintf("%s(%d)", country.CountryCode, country.Old))
			}
			fmt.Fprintf(&b, " emptied_countries=%s", strings.Join(countries, ","))
		}
		if 0 < b2.EmptiedCityCount {
			cities := []string{}
			for _, city := range b2.EmptiedCities {
				cities = append(cities, fmt.Sprintf("%s/%s(%d)", city.CountryCode, city.Name, city.Old))
			}
			fmt.Fprintf(&b, " emptied_cities=%d[%s]", b2.EmptiedCityCount, strings.Join(cities, ","))
		}
	}
	return b.String()
}

// Where the decision lives. Redis in production; tests substitute a store.
type arindbCanaryDecisionStore interface {
	set(ctx context.Context, value []byte, ttl time.Duration) error
	// absent is (nil, nil)
	get(ctx context.Context) ([]byte, error)
}

type redisArindbCanaryDecisionStore struct{}

func (redisArindbCanaryDecisionStore) set(ctx context.Context, value []byte, ttl time.Duration) (returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Set(ctx, arindbCanaryDecisionKey, value, ttl).Err()
	})
	return
}

func (redisArindbCanaryDecisionStore) get(ctx context.Context) (value []byte, returnErr error) {
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		data, err := r.Get(ctx, arindbCanaryDecisionKey).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		value = data
		return nil
	})
	return
}

var arindbCanaryDecisions arindbCanaryDecisionStore = redisArindbCanaryDecisionStore{}

// Publishes the decision as versioned JSON with the export's ttl. Written
// immediately after deciding and before the export, so readers switch while
// the export rewrites targets; a stalled indexer's decision expires with its
// publication.
func writeArindbCanaryDecision(ctx context.Context, decision *ArindbCanaryDecision, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	return arindbCanaryDecisions.set(ctx, data, ttl)
}

// The published decision, nil when none is published. A decision of another
// version or with inconsistent provenance is an error, never a default.
// Bounded by a 2 second deadline unless ctx is shorter.
func GetArindbCanaryDecision(ctx context.Context) (*ArindbCanaryDecision, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := arindbCanaryDecisions.get(ctx)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	return parseArindbCanaryDecision(data)
}

func parseArindbCanaryDecision(data []byte) (*ArindbCanaryDecision, error) {
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return nil, fmt.Errorf("unreadable arindb canary decision: %w", err)
	}
	if version.Version != ArindbCanaryDecisionVersion {
		return nil, fmt.Errorf("arindb canary decision version %d is not %d", version.Version, ArindbCanaryDecisionVersion)
	}
	decision := &ArindbCanaryDecision{}
	if err := json.Unmarshal(data, decision); err != nil {
		return nil, fmt.Errorf("unreadable arindb canary decision: %w", err)
	}
	if decision.DecisionId == "" || decision.DecidedAt.IsZero() || decision.SourceStartedAt.IsZero() || decision.DecidedAt.Before(decision.SourceStartedAt) {
		return nil, fmt.Errorf("arindb canary decision has incomplete provenance")
	}
	for _, bucket := range ProviderEgressBuckets {
		if decision.Buckets[bucket] == nil {
			return nil, fmt.Errorf("arindb canary decision has no %s bucket", bucket)
		}
	}
	// a latch names the pair it latched on
	if latch := decision.Latch; latch != nil && (latch.LatestEpoch != decision.Latest.BuildEpoch || latch.OldEpoch != decision.Old.BuildEpoch || latch.PassingRuns < 0) {
		return nil, fmt.Errorf("arindb canary decision latch %+v is not its pair's", *latch)
	}
	if dropped := decision.DroppedLatch; dropped != nil && (dropped.LatestEpoch <= dropped.OldEpoch || dropped.OldEpoch <= 0) {
		return nil, fmt.Errorf("arindb canary decision dropped latch %+v names no pair", *dropped)
	}
	// the serving epoch must be one the state can choose
	switch decision.State {
	case ArindbCanaryStateSingle:
		if decision.ServingEpoch != 0 {
			return nil, fmt.Errorf("single arindb canary decision serves epoch %d", decision.ServingEpoch)
		}
	case ArindbCanaryStateLatest:
		if decision.ServingEpoch <= 0 || decision.ServingEpoch != decision.Latest.BuildEpoch {
			return nil, fmt.Errorf("latest arindb canary decision serves epoch %d, not the latest", decision.ServingEpoch)
		}
	case ArindbCanaryStateFallback:
		if decision.ServingEpoch <= 0 || decision.ServingEpoch != decision.Old.BuildEpoch {
			return nil, fmt.Errorf("fallback arindb canary decision serves epoch %d, not the old", decision.ServingEpoch)
		}
	case ArindbCanaryStateWarming:
		// the basis names which epoch warming may serve: either database, the
		// previous decision's, or each row's latest slot
		valid := false
		switch decision.ServingBasis {
		case ArindbCanaryBasisOld, ArindbCanaryBasisLatestBlocked:
			valid = 0 < decision.ServingEpoch && decision.ServingEpoch == decision.Old.BuildEpoch
		case ArindbCanaryBasisLatest:
			valid = 0 < decision.ServingEpoch && decision.ServingEpoch == decision.Latest.BuildEpoch
		case ArindbCanaryBasisPrevious:
			valid = 0 < decision.ServingEpoch
		case ArindbCanaryBasisUncovered:
			valid = decision.ServingEpoch == 0
		case ArindbCanaryBasisEmptyCohort:
			valid = 0 <= decision.ServingEpoch
		}
		if !valid {
			return nil, fmt.Errorf("warming arindb canary decision serves epoch %d on basis %q", decision.ServingEpoch, decision.ServingBasis)
		}
	default:
		return nil, fmt.Errorf("unknown arindb canary state %q", decision.State)
	}
	return decision, nil
}

// Which conditions of the decision hold, keyed by ArindbCanaryFlags.
func (self *ArindbCanaryDecision) Flags() map[string]bool {
	return map[string]bool{
		ArindbCanaryFlagWarmingTripped:      self.WarmingTripped,
		ArindbCanaryFlagWarmingServesLatest: self.WarmingServesLatest,
		ArindbCanaryFlagLatched:             self.Latch != nil,
		ArindbCanaryFlagAccepted:            self.Accepted,
		ArindbCanaryFlagLatestUnavailable:   self.LatestUnavailable,
		ArindbCanaryFlagPairCarried:         self.PairCarriedReason != "",
		ArindbCanaryFlagFallbackDropped:     self.FallbackDropped,
	}
}

// This process as the publisher of a decision: its host, block and process id.
func ArindbCanaryPublisher() string {
	host, _ := server.Host()
	block, _ := server.Block()
	return fmt.Sprintf("%s/%s/%d", host, block, os.Getpid())
}

// The decision's summary in the native census. Older readers ignore it.
type ClientScoreNativeArindb struct {
	DecisionId   string                       `json:"decision_id"`
	State        string                       `json:"state"`
	ServingEpoch int64                        `json:"serving_epoch"`
	Latest       ArindbCanaryDatabase         `json:"latest"`
	Old          ArindbCanaryDatabase         `json:"old"`
	Coverage     ArindbCanaryCoverage         `json:"coverage"`
	Buckets      map[string]ArindbCanaryCount `json:"buckets"`
}

func (self *ArindbCanaryDecision) nativeSummary() *ClientScoreNativeArindb {
	summary := &ClientScoreNativeArindb{
		DecisionId:   self.DecisionId,
		State:        self.State,
		ServingEpoch: self.ServingEpoch,
		Latest:       self.Latest,
		Old:          self.Old,
		Coverage:     self.Coverage,
		Buckets:      map[string]ArindbCanaryCount{},
	}
	for bucket, count := range self.Buckets {
		summary.Buckets[bucket] = count.ArindbCanaryCount
	}
	return summary
}
