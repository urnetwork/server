package controller

// The ARIN database canary gauges (arindbctl/ARINDB.md, "Signals, alerts and
// dashboards"), read by grafana/dashboards/providers.json, the warp alert rules
// grafana/alerting/arindb-canary.yml and monitor signal §2.9d. Every
// Taskworker's collector exports the indexer's decision from the published
// document rather than recomputing it, so all hosts report the same values:
//
//	urnetwork_stats_arindb_canary_state                  one-hot single, warming, latest or fallback {state}
//	urnetwork_stats_arindb_fallback_active               1 while the breaker serves the old database
//	urnetwork_stats_arindb_decision_flags                1 for each condition of the decision that holds {flag}
//	urnetwork_stats_arindb_providers                     paired providers per bucket under each database {bucket, database}
//	urnetwork_stats_arindb_providers_by_country          the same per country, from the publishing Taskworker only {bucket, database, country_code}
//	urnetwork_stats_arindb_loss_ratio                    net loss of latest against old; negative is a gain {bucket}
//	urnetwork_stats_arindb_gain_ratio                    net gain of latest against old, 0 for a loss {bucket}
//	urnetwork_stats_arindb_churn_ratio                   gross removals and additions against old {bucket}
//	urnetwork_stats_arindb_emptied_countries             countries the latest would empty {bucket}
//	urnetwork_stats_arindb_emptied_country               the old supply of each {bucket, country_code}
//	urnetwork_stats_arindb_emptied_cities                cities the latest would empty, named in the decision {bucket}
//	urnetwork_stats_arindb_coverage_ratio                latest, old and paired coverage of the public cohort {database}
//	urnetwork_stats_arindb_database_build_epoch_seconds  the indexer's pair {database}
//	urnetwork_stats_arindb_database_info                 its state and manifest hash, valued 1 {database, state, sha256}
//	urnetwork_stats_arindb_cohort_providers              the public cohort and its largest size over two hours {reference}
//	urnetwork_stats_arindb_cohort_loss_ratio             the cohort's loss against that size
//	urnetwork_stats_arindb_pair_since_seconds            when the compared pair was first compared
//	urnetwork_stats_arindb_serving_changes               changes of the serving epoch over six hours
//	urnetwork_stats_arindb_latch_passing_runs            consecutive comparisons a latched fallback passed with the release margin
//	urnetwork_stats_arindb_serving_epoch_seconds         the serving epoch, 0 for each row's latest slot
//	urnetwork_stats_arindb_decision_seconds              when the indexer decided
//
// Without a readable decision every reader serves each row's latest slot, so
// the one-hot state, the flags, the fallback gauge and the serving epoch read
// 0, the comparison series are removed, and the decision time keeps the last
// decision it published so that its age keeps growing. The per-country supply,
// about 900 series, is exported only by the Taskworker that published the
// decision.

import (
	"context"
	"strings"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server/model"
)

var statsArindbCanaryStateGauge = newStatsGaugeVec(
	"arindb_canary_state",
	"The ARIN canary state of the latest score publication, one-hot: single, warming, latest or fallback",
	"state",
)

var statsArindbFallbackActiveGauge = newStatsGauge(
	"arindb_fallback_active",
	"1 while the score export serves the old ARIN database's classification because the latest tripped the breaker",
)

var statsArindbDecisionFlagsGauge = newStatsGaugeVec(
	"arindb_decision_flags",
	"1 for each condition of the latest ARIN canary decision that holds: warming_tripped, warming_serves_latest, latched, accepted, latest_unavailable, pair_carried, fallback_dropped",
	"flag",
)

var statsArindbProvidersGauge = newStatsGaugeVec(
	"arindb_providers",
	"Public providers with facts for both ARIN databases in each bucket under the latest and the old database",
	"bucket",
	"database",
)

var statsArindbProvidersByCountryGauge = newStatsGaugeVec(
	"arindb_providers_by_country",
	"Public providers with facts for both ARIN databases in each bucket under each database, per published country",
	"bucket",
	"database",
	"country_code",
)

var statsArindbLossRatioGauge = newStatsGaugeVec(
	"arindb_loss_ratio",
	"Net loss of the latest ARIN database's bucket supply against the old one's, (old - latest) / old; negative is a gain, 0 without old supply",
	"bucket",
)

var statsArindbGainRatioGauge = newStatsGaugeVec(
	"arindb_gain_ratio",
	"Net gain of the latest ARIN database's bucket supply against the old one's, (latest - old) / old; 0 for a loss or without old supply",
	"bucket",
)

var statsArindbChurnRatioGauge = newStatsGaugeVec(
	"arindb_churn_ratio",
	"Gross churn of the latest ARIN database's bucket supply against the old one's, (removed + added) / old; 0 without old supply",
	"bucket",
)

var statsArindbEmptiedCountriesGauge = newStatsGaugeVec(
	"arindb_emptied_countries",
	"Countries with supply in a bucket under the old ARIN database and none under the latest",
	"bucket",
)

var statsArindbEmptiedCountryGauge = newStatsGaugeVec(
	"arindb_emptied_country",
	"The old ARIN database's supply in each country the latest would empty, per bucket",
	"bucket",
	"country_code",
)

var statsArindbEmptiedCitiesGauge = newStatsGaugeVec(
	"arindb_emptied_cities",
	"Cities with supply in a bucket under the old ARIN database and none under the latest; reported, never tripped on",
	"bucket",
)

var statsArindbCoverageRatioGauge = newStatsGaugeVec(
	"arindb_coverage_ratio",
	"Share of the public cohort with facts for the latest, the old, or both (paired) ARIN databases",
	"database",
)

var statsArindbDatabaseBuildEpochGauge = newStatsGaugeVec(
	"arindb_database_build_epoch_seconds",
	"Build epoch of the latest and old ARIN databases the score export compared",
	"database",
)

var statsArindbDatabaseInfoGauge = newStatsGaugeVec(
	"arindb_database_info",
	"1 for each ARIN database the score export compared, with its state and manifest SHA-256 (none when unknown)",
	"database",
	"state",
	"sha256",
)

var statsArindbCohortProvidersGauge = newStatsGaugeVec(
	"arindb_cohort_providers",
	"Public providers of the score cohort the ARIN canary compared (current) and its largest size over the last two hours (reference)",
	"reference",
)

var statsArindbCohortLossRatioGauge = newStatsGauge(
	"arindb_cohort_loss_ratio",
	"Loss of the ARIN canary's public cohort against its largest size over the last two hours; providers whose latest lookup fails never reach the cohort",
)

var statsArindbPairSinceGauge = newStatsGauge(
	"arindb_pair_since_seconds",
	"Unix time the ARIN canary first compared its current pair of databases; 0 without a decision",
)

var statsArindbServingChangesGauge = newStatsGauge(
	"arindb_serving_changes",
	"Changes of the ARIN serving epoch over the last six hours, as of the latest decision",
)

var statsArindbLatchPassingRunsGauge = newStatsGauge(
	"arindb_latch_passing_runs",
	"Consecutive comparisons a latched ARIN fallback passed with the release margin; it releases at release_passing_runs. 0 without a latch",
)

var statsArindbServingEpochGauge = newStatsGauge(
	"arindb_serving_epoch_seconds",
	"Build epoch of the ARIN database whose Quality classification the score export serves; 0 for each provider's latest slot",
)

var statsArindbDecisionSecondsGauge = newStatsGauge(
	"arindb_decision_seconds",
	"Unix time of the latest published ARIN canary decision",
)

// Publishes the decision of the latest score publication. A failed read is
// logged and leaves the gauges as no decision.
func statsRefreshArindbCanary(ctx context.Context) {
	decision, err := model.GetArindbCanaryDecision(ctx)
	if err != nil {
		glog.Infof("[stats]arindb canary decision unreadable: %s\n", err)
		decision = nil
	}
	statsPublishArindbCanary(decision)
}

func statsPublishArindbCanary(decision *model.ArindbCanaryDecision) {
	state := ""
	flags := map[string]bool{}
	if decision != nil {
		state = decision.State
		flags = decision.Flags()
	}
	stateValues := []statsLabeledValue{}
	for _, s := range model.ArindbCanaryStates {
		value := 0.0
		if s == state {
			value = 1
		}
		stateValues = append(stateValues, statsLabeledValue{labelValues: []string{s}, value: value})
	}
	statsArindbCanaryStateGauge.replace(stateValues)
	flagValues := []statsLabeledValue{}
	for _, flag := range model.ArindbCanaryFlags {
		value := 0.0
		if flags[flag] {
			value = 1
		}
		flagValues = append(flagValues, statsLabeledValue{labelValues: []string{flag}, value: value})
	}
	statsArindbDecisionFlagsGauge.replace(flagValues)

	if decision == nil {
		statsArindbFallbackActiveGauge.set(0)
		statsArindbServingEpochGauge.set(0)
		statsArindbCohortLossRatioGauge.set(0)
		statsArindbServingChangesGauge.set(0)
		statsArindbLatchPassingRunsGauge.set(0)
		statsArindbPairSinceGauge.set(0)
		for _, gauge := range []*statsGaugeVec{
			statsArindbProvidersGauge,
			statsArindbProvidersByCountryGauge,
			statsArindbLossRatioGauge,
			statsArindbGainRatioGauge,
			statsArindbChurnRatioGauge,
			statsArindbCohortProvidersGauge,
			statsArindbEmptiedCountriesGauge,
			statsArindbEmptiedCountryGauge,
			statsArindbEmptiedCitiesGauge,
			statsArindbCoverageRatioGauge,
			statsArindbDatabaseBuildEpochGauge,
			statsArindbDatabaseInfoGauge,
		} {
			gauge.replace(nil)
		}
		return
	}

	fallback := 0.0
	if decision.State == model.ArindbCanaryStateFallback {
		fallback = 1
	}
	statsArindbFallbackActiveGauge.set(fallback)
	statsArindbServingEpochGauge.set(float64(decision.ServingEpoch))
	statsArindbDecisionSecondsGauge.set(statsSourceTimestampSeconds(decision.DecidedAt))
	statsArindbCohortProvidersGauge.replace([]statsLabeledValue{
		{labelValues: []string{"current"}, value: float64(decision.Coverage.Providers)},
		{labelValues: []string{"reference"}, value: float64(decision.Cohort.ReferenceProviders)},
	})
	statsArindbCohortLossRatioGauge.set(decision.Cohort.LossRatio)
	statsArindbServingChangesGauge.set(float64(len(decision.ServingChanges)))
	passingRuns := 0
	if decision.Latch != nil {
		passingRuns = decision.Latch.PassingRuns
	}
	statsArindbLatchPassingRunsGauge.set(float64(passingRuns))
	statsArindbPairSinceGauge.set(statsSourceTimestampSeconds(decision.PairSince))
	// the publishing Taskworker alone exports the per-country supply
	publisher := decision.Publisher == model.ArindbCanaryPublisher()

	providers := []statsLabeledValue{}
	byCountry := []statsLabeledValue{}
	losses := []statsLabeledValue{}
	gains := []statsLabeledValue{}
	churns := []statsLabeledValue{}
	emptiedCountries := []statsLabeledValue{}
	emptiedCountry := []statsLabeledValue{}
	emptiedCities := []statsLabeledValue{}
	for _, bucketName := range model.ProviderEgressBuckets {
		bucket := decision.Buckets[bucketName]
		if bucket == nil {
			continue
		}
		providers = append(providers,
			statsLabeledValue{labelValues: []string{bucketName, "latest"}, value: float64(bucket.Latest)},
			statsLabeledValue{labelValues: []string{bucketName, "old"}, value: float64(bucket.Old)},
		)
		if publisher {
			for countryCode, count := range bucket.Countries {
				countryLabel := statsArindbCountryLabel(countryCode)
				byCountry = append(byCountry,
					statsLabeledValue{labelValues: []string{bucketName, "latest", countryLabel}, value: float64(count.Latest)},
					statsLabeledValue{labelValues: []string{bucketName, "old", countryLabel}, value: float64(count.Old)},
				)
			}
		}
		losses = append(losses, statsLabeledValue{labelValues: []string{bucketName}, value: bucket.LossRatio})
		gains = append(gains, statsLabeledValue{labelValues: []string{bucketName}, value: bucket.GainRatio})
		churns = append(churns, statsLabeledValue{labelValues: []string{bucketName}, value: bucket.ChurnRatio})
		emptiedCountries = append(emptiedCountries, statsLabeledValue{labelValues: []string{bucketName}, value: float64(len(bucket.EmptiedCountries))})
		for _, country := range bucket.EmptiedCountries {
			emptiedCountry = append(emptiedCountry, statsLabeledValue{
				labelValues: []string{bucketName, statsArindbCountryLabel(country.CountryCode)},
				value:       float64(country.Old),
			})
		}
		emptiedCities = append(emptiedCities, statsLabeledValue{labelValues: []string{bucketName}, value: float64(bucket.EmptiedCityCount)})
	}
	statsArindbProvidersGauge.replace(providers)
	statsArindbProvidersByCountryGauge.replace(byCountry)
	statsArindbLossRatioGauge.replace(losses)
	statsArindbGainRatioGauge.replace(gains)
	statsArindbChurnRatioGauge.replace(churns)
	statsArindbEmptiedCountriesGauge.replace(emptiedCountries)
	statsArindbEmptiedCountryGauge.replace(emptiedCountry)
	statsArindbEmptiedCitiesGauge.replace(emptiedCities)

	statsArindbCoverageRatioGauge.replace([]statsLabeledValue{
		{labelValues: []string{"latest"}, value: decision.Coverage.Ratio(decision.Coverage.Latest)},
		{labelValues: []string{"old"}, value: decision.Coverage.Ratio(decision.Coverage.Old)},
		{labelValues: []string{"paired"}, value: decision.Coverage.Ratio(decision.Coverage.Paired)},
	})
	statsArindbDatabaseBuildEpochGauge.replace([]statsLabeledValue{
		{labelValues: []string{"latest"}, value: float64(decision.Latest.BuildEpoch)},
		{labelValues: []string{"old"}, value: float64(decision.Old.BuildEpoch)},
	})
	statsArindbDatabaseInfoGauge.replace([]statsLabeledValue{
		{labelValues: []string{"latest", statsArindbLabel(decision.Latest.State), statsArindbHashLabel(decision.Latest.Sha256)}, value: 1},
		{labelValues: []string{"old", statsArindbLabel(decision.Old.State), statsArindbHashLabel(decision.Old.Sha256)}, value: 1},
	})
}

// A country code as a label: lower case, "unknown" for none.
func statsArindbCountryLabel(countryCode string) string {
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	if countryCode == "" {
		return "unknown"
	}
	return countryCode
}

// A state as a label, "unknown" for none.
func statsArindbLabel(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// A manifest hash as a label, "none" when the manifest names none.
func statsArindbHashLabel(hash string) string {
	if hash == "" {
		return "none"
	}
	return hash
}
