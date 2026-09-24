package model

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The egress index (connect/GEOMAP.md §10, D23) replaces the net-type score as
// the quality base of FindProviders2. It is what the egress probe found wrong
// with a provider, as one small number: every scored load of its latest health
// run that failed each of its retries costs its class's weight, and the sum is
// capped. The reliability rollup computes it per provider, with the run's 90 %
// verdict and the time of the evidence, onto network_client_location_reliability
// (UpdateClientLocationReliabilities); a provider without a usable run gets an
// index of 0 and no verdict, and the NULL verdict is what routes it to the
// online bucket. The client-score job reads the three into the tiers
// (UpdateClientScores).
//
// The rules that decide which bucket a provider is in at all live here too
// (decideProviderEgress), so the score cache, the location counts, the
// dashboard and `bringyourctl provider inspect` read one decision:
//
//   - a current blackhole verdict or a TLS-authentication failure is a hard
//     exclusion: the provider is absent everywhere, force_minimum and an
//     explicit client id included;
//   - a fresh probe that observed the exit outside the country the provider is
//     published under is a gate on both buckets and the counts, as a minimum
//     that force_minimum and an explicit client id still pass;
//   - quality is the probed providers whose run passes the 90 % rule, speed
//     every probed provider past the two above, and online, which no request
//     names, every unprobed one that passes the minimums the other buckets
//     apply apart from the probe's -- the reliability floors and the
//     speed-mode score maximum, so a provider no client has measured stays
//     out; a short bucket borrows from the others (FindProviders2), and the
//     rollout flag speaks only for a rollup row written before the index.

// The weights of the index and the bounds of its evidence, with the other
// tunables of the egress rules. The `egress_index` block of provider.yml,
// beside the rollout flag, overrides any of the index's (egressIndexSettings).
type EgressIndexSettings struct {
	// what one scored load that failed every one of its retries costs, per
	// class, so a dns failure can be made to cost more than a cdn one once
	// real data says it should
	ClassWeights map[string]int
	// the cost of a failed load in a class ClassWeights does not name, and of
	// a failure the run's class tally does not account for: a class the prober
	// adds later is scored before it is weighted, and a run without a class
	// breakdown still pays for its failures
	DefaultClassWeight int
	// caps the weighted failures, so a broken run cannot bury the performance
	// adjustment under it
	MaxFailureIndex int
	// how long a health run stays evidence. A provider without a run within
	// it, or with a run of fewer than MinScoredLoads loads, has no evidence:
	// it is in the online bucket, not ranked by an index.
	EvidenceMaxAge time.Duration
	// The 90 % rule: a run passes when
	// QualityOkNumerator·total ≤ QualityOkDenominator·ok over all its scored
	// loads, compared in integers so the boundary is exact for every total.
	QualityOkNumerator   int
	QualityOkDenominator int
	// the fewest scored loads a run must have to be evidence. Over a sample of
	// 26 the one-in-ten line is a two-failure line (GEOMAP §10.5), which says
	// more about the sample than about the exit.
	MinScoredLoads int
	// the gate of a fresh probe that observed the exit in a country other than
	// the one the provider is published under
	CountryGate bool
	// added to the tier of a provider FindProviders2
	// borrows from the other bucket when a request's own bucket comes up short
	// (GEOMAP §10.3 "Backfill"); a provider borrowed from the online bucket
	// carries twice the offset. The client ranks on the tier plus demerits of
	// its own, up to MaxClientTierDemerit, which the server never sees, so the
	// default holds both edges of the borrowed band with every demerit added
	// on the better side and none on the worse:
	//
	//   - a native carries at most MaxNativeClientScoreTier (2), so at most 9
	//     demerited, and a borrowed provider at least the offset: a demerited
	//     native ranks ahead of every borrowed provider from an offset of
	//     2 + 1 + 7 = 10;
	//   - a provider borrowed from the other bucket carries at most
	//     ClientScoreCutoffTier (3) plus the offset, so at most the offset
	//     plus 10 demerited, and an online one twice the offset: a demerited
	//     borrowed provider ranks ahead of the online bucket from an offset of
	//     3 + 1 + 7 = 11.
	//
	// The default is the larger, 11, with online at 22, and the borrowed keep
	// their order among themselves. The client bounds no tier -- a larger one
	// only ranks later -- so it can be raised. It is never set below
	// MaxNativeClientScoreTier + 1 (3), the least that keeps every native
	// ahead of every borrowed provider before demerits.
	BackfillTierOffset int
	// how old the settings a request path reads may be. The passes read
	// provider.yml on every call; a request path runs far too often to read
	// and parse a file each time.
	RequestSettingsMaxAge time.Duration
}

// The most a connect client adds to a provider's tier from what it sees
// itself (connect ip_remote_multi_client.go, effectiveTier): +1 while no probe
// or traffic has proven the provider, +2 while its dials are starved, +2 while
// quarantined or remembering a quarantine, +1 for an unhealthy stats window,
// and +1 while a busy-flow probe is unanswered. It is the client's number,
// mirrored here only to size the default backfill offset.
const MaxClientTierDemerit = 7

// The rules as connect/GEOMAP.md §10.3 sets them, before any provider.yml
// override.
func DefaultEgressIndexSettings() *EgressIndexSettings {
	return &EgressIndexSettings{
		ClassWeights: map[string]int{
			"dns":          1,
			"connectivity": 1,
			"cdn":          1,
			"site":         1,
		},
		DefaultClassWeight:    1,
		MaxFailureIndex:       6,
		EvidenceMaxAge:        ProviderEgressLocationMaxAge,
		QualityOkNumerator:    9,
		QualityOkDenominator:  10,
		MinScoredLoads:        50,
		CountryGate:           true,
		BackfillTierOffset:    ClientScoreCutoffTier + 1 + MaxClientTierDemerit,
		RequestSettingsMaxAge: time.Minute,
	}
}

// The `egress_index` block of provider.yml. Every field is optional; an absent
// one keeps its default.
type egressIndexSettingsDocument struct {
	EgressIndex *struct {
		ClassWeights         map[string]int `yaml:"class_weights"`
		DefaultClassWeight   *int           `yaml:"default_class_weight"`
		MaxFailureIndex      *int           `yaml:"max_failure_index"`
		EvidenceMaxAge       *string        `yaml:"evidence_max_age"`
		QualityOkNumerator   *int           `yaml:"quality_ok_numerator"`
		QualityOkDenominator *int           `yaml:"quality_ok_denominator"`
		MinScoredLoads       *int           `yaml:"min_scored_loads"`
		CountryGate          *bool          `yaml:"country_gate"`
		BackfillTierOffset   *int           `yaml:"backfill_tier_offset"`
	} `yaml:"egress_index"`
}

// The defaults with the overrides of provider.yml, read on every call like the
// rollout flag, so a config change takes effect on the next pass without a
// restart. A missing provider.yml is the defaults, as it is for the rollout
// flag; an unreadable one, or a value out of range, keeps the default and says
// so, since a typo in one weight must not take the index away from the whole
// fleet.
func egressIndexSettings() *EgressIndexSettings {
	settings := DefaultEgressIndexSettings()
	resource, err := server.Config.SimpleResource(providerConfigResourceName)
	if err != nil || resource == nil {
		return settings
	}
	var document egressIndexSettingsDocument
	if err := resource.UnmarshalYamlE(&document); err != nil {
		glog.Errorf("[egress]provider config is unreadable (%s); the egress index keeps its defaults\n", err)
		return settings
	}
	overrides := document.EgressIndex
	if overrides == nil {
		return settings
	}

	// every value is stored or multiplied into a smallint score, so each is
	// held to what that column can carry
	setInt := func(name string, value *int, minValue int, target *int) {
		if value == nil {
			return
		}
		if *value < minValue || math.MaxInt16 < *value {
			glog.Errorf("[egress]provider config egress_index.%s = %d is outside [%d, %d]; kept %d\n", name, *value, minValue, math.MaxInt16, *target)
			return
		}
		*target = *value
	}
	for class, weight := range overrides.ClassWeights {
		defaultWeight := settings.classWeight(class)
		setInt(fmt.Sprintf("class_weights.%s", class), &weight, 0, &defaultWeight)
		settings.ClassWeights[class] = defaultWeight
	}
	setInt("default_class_weight", overrides.DefaultClassWeight, 0, &settings.DefaultClassWeight)
	setInt("max_failure_index", overrides.MaxFailureIndex, 0, &settings.MaxFailureIndex)
	// a run of no loads measures nothing, so at least one is required
	setInt("min_scored_loads", overrides.MinScoredLoads, 1, &settings.MinScoredLoads)
	// below one past the highest native tier a borrowed provider could tie
	// with, or rank ahead of, a native one even before the client's demerits
	setInt("backfill_tier_offset", overrides.BackfillTierOffset, MaxNativeClientScoreTier+1, &settings.BackfillTierOffset)

	numerator := settings.QualityOkNumerator
	denominator := settings.QualityOkDenominator
	setInt("quality_ok_numerator", overrides.QualityOkNumerator, 0, &numerator)
	setInt("quality_ok_denominator", overrides.QualityOkDenominator, 1, &denominator)
	// a rule above one no run can pass, which would empty quality fleet-wide
	if denominator < numerator {
		glog.Errorf("[egress]provider config egress_index quality ratio %d/%d is above one; kept %d/%d\n", numerator, denominator, settings.QualityOkNumerator, settings.QualityOkDenominator)
	} else {
		settings.QualityOkNumerator = numerator
		settings.QualityOkDenominator = denominator
	}

	if overrides.EvidenceMaxAge != nil {
		if evidenceMaxAge, err := time.ParseDuration(*overrides.EvidenceMaxAge); err == nil && 0 < evidenceMaxAge {
			settings.EvidenceMaxAge = evidenceMaxAge
		} else {
			glog.Errorf("[egress]provider config egress_index.evidence_max_age = %q is not a positive duration; kept %s\n", *overrides.EvidenceMaxAge, settings.EvidenceMaxAge)
		}
	}
	if overrides.CountryGate != nil {
		settings.CountryGate = *overrides.CountryGate
	}
	return settings
}

// The settings as a request path last read them.
type egressIndexSettingsSnapshot struct {
	settings *EgressIndexSettings
	loadTime time.Time
}

// The settings a request path reads: at most their own RequestSettingsMaxAge
// old, re-read by the first request past that age. Two requests racing past
// it both read the file, which is harmless.
var requestEgressIndexSettingsSnapshot atomic.Pointer[egressIndexSettingsSnapshot]

// Registers the backfill metrics, and forgets the request settings on a test
// reset so a test's provider.yml is read.
func init() {
	prometheus.MustRegister(findProviders2BackfillProviders, findProviders2AnsweredProviders)
	server.OnReset(func() {
		requestEgressIndexSettingsSnapshot.Store(nil)
	})
}

// The cost of one failed load of `class`.
func (self *EgressIndexSettings) classWeight(class string) int {
	if weight, ok := self.ClassWeights[class]; ok {
		return weight
	}
	return self.DefaultClassWeight
}

// The largest index these settings can produce, which bounds the dashboard's
// index labels.
func (self *EgressIndexSettings) MaxIndex() int {
	return min(self.MaxFailureIndex, math.MaxInt16)
}

// The part of a provider's latest health run the index reads: its scored
// loads and their per-class tally. Nothing of the reputation tally is read;
// after GEOMAP §11 there is no reputation class, and before it the tally was
// never a health figure.
type EgressHealthRun struct {
	MeasuredAt   time.Time
	OkCount      int
	Total        int
	ClassResults map[string]ProviderEgressHealthClassResult
}

// One provider's index and the verdict it was decided with.
type EgressIndex struct {
	Index int
	// whether a usable run decided the index
	Evidence bool
	// the 90 % verdict over the run's scored loads, meaningful only with
	// Evidence
	Quality bool
}

// Quality as the egress_quality column holds it: nil when there is no
// evidence, since "not probed" must stay distinguishable from "probed and
// failing".
func (self EgressIndex) QualityVerdict() *bool {
	if !self.Evidence {
		return nil
	}
	quality := self.Quality
	return &quality
}

// The index of GEOMAP §10.3 for a provider whose latest health run is `run`,
// nil for none. A run is evidence with at least MinScoredLoads scored loads,
// measured within EvidenceMaxAge. Without evidence the index is 0 and there is
// no verdict: such a provider is not ordered by an index at all, it is in the
// online bucket, and a written 0 keeps the column's NULL for the one case that
// must stay distinguishable, a row the new rollup has not reached.
//
// The index is the weighted, capped count of the run's failed loads. A load
// counts once whatever made it fail: a site a user could not reach through the
// exit is the fact, and the sites vendors used to call "reputation" pay here
// exactly as a site that timed out does. The verdict is the 90 % rule over all
// the run's scored loads.
func ComputeEgressIndex(run *EgressHealthRun, now time.Time, settings *EgressIndexSettings) EgressIndex {
	if run == nil || run.Total < settings.MinScoredLoads || run.MeasuredAt.Before(now.Add(-settings.EvidenceMaxAge)) {
		return EgressIndex{}
	}

	weightedFailures := 0
	attributedFailures := 0
	for class, tally := range run.ClassResults {
		failures := max(0, tally.Total-tally.OK)
		attributedFailures += failures
		weightedFailures += settings.classWeight(class) * failures
	}
	// the ingest requires the class tally to sum to the run, but a row written
	// before that check, or with no class breakdown at all, still pays for
	// every failed load it does not attribute
	unattributedFailures := max(0, (run.Total-run.OkCount)-attributedFailures)
	weightedFailures += settings.DefaultClassWeight * unattributedFailures

	return EgressIndex{
		Index:    min(weightedFailures, settings.MaxFailureIndex, math.MaxInt16),
		Evidence: true,
		Quality:  settings.QualityOkNumerator*run.Total <= settings.QualityOkDenominator*run.OkCount,
	}
}

// The reasons a provider is out of a bucket (GEOMAP §10.4), in the order the
// rules apply, so the first that holds is the one reported: a provider that is
// both dark and mislocated is reported dark. Blackhole and tls remove a
// provider from everything, country from every bucket and the counts, health
// from quality alone, and unprobed from quality and speed, leaving it to the
// online bucket.
const (
	ProviderExcludedBlackhole = "blackhole"
	ProviderExcludedTls       = "tls"
	ProviderExcludedCountry   = "country"
	ProviderExcludedHealth    = "health"
	ProviderExcludedUnprobed  = "unprobed"
)

// The reasons in the order the rules apply.
var ProviderExcludedReasons = []string{
	ProviderExcludedBlackhole,
	ProviderExcludedTls,
	ProviderExcludedCountry,
	ProviderExcludedHealth,
	ProviderExcludedUnprobed,
}

// What the rules read about one provider.
type providerEgressFacts struct {
	blackholed              bool
	tlsAuthenticationFailed bool
	// a fresh probe observed the exit in a country other than the published one
	countryMismatch bool
	// the rollup's columns. A nil index is a row the new rollup has not
	// written, which the rules before the index decide.
	egressIndex   *int
	egressQuality *bool
	// the rules before the index, read only while egressIndex is nil: the 24
	// hour health gate, whether there was a run within it, and the count rule
	// (healthy, and observed by a fresh probe where it is published)
	legacyHealthPasses   bool
	legacyHealthMeasured bool
	legacyCounted        bool
}

// What the rules do with one provider.
type providerEgressDecision struct {
	// the first rule that took the provider out of a bucket, "" for none
	reason string
	// absent from every result and count, force_minimum and an explicit client
	// id included
	hardExcluded bool
	// whether the provider passes each bucket's membership and counts toward
	// the location counts. Each is a minimum: force_minimum re-admits a
	// provider that fails one unless it is hard excluded.
	quality bool
	speed   bool
	// the online bucket: no probe verdict at all. It is never requested; a
	// short bucket borrows from it last, and the client-score job admits only
	// those of it that pass the reliability floors and the speed-mode score
	// maximum.
	online  bool
	counted bool
}

// Gathers what the rules read about one provider from this pass's bulk loads
// and the provider's rollup row. The published country is the country of the
// rollup's location, the observed one a fresh probe's.
func (self providerCountFilter) egressFacts(
	clientId server.Id,
	rollupCountryCode *string,
	egressIndex *int,
	egressQuality *bool,
	settings *EgressIndexSettings,
) *providerEgressFacts {
	publishedCountryCode := normalizeCountryCode(rollupCountryCode)
	observedCountryCode := self.countryCodes[clientId]
	_, measured := self.healthCounts[clientId]
	return &providerEgressFacts{
		blackholed:              self.isBlackholed(clientId),
		tlsAuthenticationFailed: self.tlsAuthenticationFailed[clientId],
		// a provider nobody has located, or whose own country is unknown,
		// cannot contradict itself
		countryMismatch: settings.CountryGate &&
			observedCountryCode != "" &&
			publishedCountryCode != "" &&
			observedCountryCode != publishedCountryCode,
		egressIndex:          egressIndex,
		egressQuality:        egressQuality,
		legacyHealthPasses:   self.passesHealth(clientId),
		legacyHealthMeasured: measured,
		// an unknown published country cannot be verified against anything,
		// so the old count rule fails it closed
		legacyCounted: publishedCountryCode != "" && self.countsTowardCountry(clientId, publishedCountryCode),
	}
}

// Applies the rules of GEOMAP §10.3 to one provider. egressTestEnabled is the
// rollout flag as the caller's pass applies it, which decides only a row the
// new rollup has not written: the online bucket took over its decision about
// the unprobed.
func decideProviderEgress(facts *providerEgressFacts, egressTestEnabled bool) providerEgressDecision {
	switch {
	case facts.blackholed:
		return providerEgressDecision{
			reason:       ProviderExcludedBlackhole,
			hardExcluded: true,
		}
	case facts.tlsAuthenticationFailed:
		return providerEgressDecision{
			reason:       ProviderExcludedTls,
			hardExcluded: true,
		}
	case facts.countryMismatch:
		// reachable and safe, just not where it is listed
		return providerEgressDecision{
			reason: ProviderExcludedCountry,
		}
	}

	if facts.egressIndex == nil {
		// a row the new rollup has not written keeps the rules it had: the
		// flag gates both buckets on the 24 hour health run, and the counts
		// also on a probe having observed the provider where it is published
		passes := !egressTestEnabled || facts.legacyHealthPasses
		decision := providerEgressDecision{
			quality: passes,
			speed:   passes,
			counted: !egressTestEnabled || facts.legacyCounted,
		}
		if !passes {
			if facts.legacyHealthMeasured {
				decision.reason = ProviderExcludedHealth
			} else {
				decision.reason = ProviderExcludedUnprobed
			}
		}
		return decision
	}

	switch {
	case facts.egressQuality == nil:
		// no probe verdict at all, no negative mark but the exclusions and
		// no positive one: the online bucket, and counted, since it never
		// fails closed
		return providerEgressDecision{
			reason:  ProviderExcludedUnprobed,
			online:  true,
			counted: true,
		}
	case !*facts.egressQuality:
		// probed, and more than one in ten of its loads failed: out of
		// quality, still in speed, and counted, since the counts are the
		// supply a user can use
		return providerEgressDecision{
			reason:  ProviderExcludedHealth,
			speed:   true,
			counted: true,
		}
	default:
		return providerEgressDecision{
			quality: true,
			speed:   true,
			counted: true,
		}
	}
}

// The bucket a request in rankMode borrows from first when its own comes up
// short, false for a mode that is neither.
func backfillRankMode(rankMode RankMode) (RankMode, bool) {
	switch rankMode {
	case RankModeQuality:
		return RankModeSpeed, true
	case RankModeSpeed:
		return RankModeQuality, true
	default:
		return "", false
	}
}

// How many providers each FindProviders2 answer borrowed from outside its own
// bucket (GEOMAP §10.3 "Backfill"), per
// requested rank mode, so a mass probe failure that empties a bucket shows as
// a wave of backfill rather than as a silent change in what users are handed.
// Every non-forced answer to a location request is observed, a zero included,
// so the answer rate and the borrowed per answer read from the one series.
var findProviders2BackfillProviders = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "urnetwork_provider_backfill",
	Help:    "Providers a FindProviders2 answer borrowed from outside its own bucket, per requested rank mode",
	Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100},
}, []string{"rank_mode"})

// Every provider those answers held, native and borrowed, so the borrowed share of what users were handed reads
// as one ratio -- what SIGNALS.md §2.19b alerts on when it stays above half.
var findProviders2AnsweredProviders = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_answered_total",
	Help: "Providers FindProviders2 location answers held, native and borrowed, per requested rank mode",
}, []string{"rank_mode"})

// The set of providers a current blackhole verdict or TLS-authentication
// failure excludes (GEOMAP §10.3), written whole by every location count pass
// (UpdateClientLocations) and read by FindProviders2 where it assembles a
// result. The score cache already leaves these providers out, but it is only
// as current as its last export, and a provider named by client id never
// passes through the cache: the set is what holds the exclusion on every path,
// within one count pass of the verdict. One key, so the read is one command.
const providerHardExclusionsKey = "{provider_hard_exclusions}"

// The same atomic set carries evidence that even an empty publication exists.
// This reserved member cannot be a provider id. Older writers omit it.
const providerHardExclusionsReadyMember = "ready:v1"

// Which candidates the complete cached set excludes. A missing, expired or
// legacy snapshot has unknown coverage, so read only these candidates from
// the primary database. Backend errors never become an empty exclusion set.
func getProviderHardExclusions(ctx context.Context, clientIds []server.Id) (excludedClientIds map[server.Id]bool, returnErr error) {
	excludedClientIds = map[server.Id]bool{}
	if len(clientIds) == 0 {
		return
	}
	members := make([]any, 0, len(clientIds)+1)
	members = append(members, providerHardExclusionsReadyMember)
	for _, clientId := range clientIds {
		members = append(members, clientId.String())
	}
	complete := false
	server.Redis(ctx, func(r server.RedisClient) {
		memberships, err := r.SMIsMember(ctx, providerHardExclusionsKey, members...).Result()
		if err != nil {
			returnErr = err
			return
		}
		if len(memberships) != len(members) {
			returnErr = fmt.Errorf("incomplete provider hard-exclusion membership response")
			return
		}
		complete = memberships[0]
		if !complete {
			return
		}
		for i, member := range memberships[1:] {
			if member {
				excludedClientIds[clientIds[i]] = true
			}
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	if !complete {
		return readProviderHardExclusions(ctx, clientIds)
	}
	return
}

// Read-through uses the exporter's exact dark and TLS rules without loading
// the fleet. Each query has at most 256 distinct candidates and result rows;
// it observes the caller's context and never publishes a partial global set.
func readProviderHardExclusions(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	const chunkSize = 256
	uniqueClientIds := make([]server.Id, 0, len(clientIds))
	seenClientIds := make(map[server.Id]bool, len(clientIds))
	for _, clientId := range clientIds {
		if !seenClientIds[clientId] {
			seenClientIds[clientId] = true
			uniqueClientIds = append(uniqueClientIds, clientId)
		}
	}
	excludedClientIds := map[server.Id]bool{}
	if len(uniqueClientIds) == 0 {
		return excludedClientIds, nil
	}
	minCheckedAt := server.NowUtc().Add(-ProviderBlackholeCheckMaxAge)
	rules := GetProviderEgressRules()
	query := `
		SELECT client_id
		FROM provider_blackhole_check
		WHERE client_id = ANY($1)
		  AND ` + ProviderBlackholeDarkSql("provider_blackhole_check", "$2", rules) + `
		UNION
		SELECT client_id
		FROM provider_egress_health
		WHERE client_id = ANY($1) AND tls_authentication_failure = true
	`
	var returnErr error
	// This is a safety decision, not an analytics read: do not use a replica
	// whose lag could re-admit a provider with a newly accepted hard verdict.
	server.Db(ctx, func(conn server.PgConn) {
		for start := 0; start < len(uniqueClientIds); start += chunkSize {
			chunkClientIds := uniqueClientIds[start:min(start+chunkSize, len(uniqueClientIds))]
			chunkClientIdSet := make(map[server.Id]bool, len(chunkClientIds))
			for _, clientId := range chunkClientIds {
				chunkClientIdSet[clientId] = true
			}
			if err := ctx.Err(); err != nil {
				returnErr = err
				return
			}
			rows, err := conn.Query(ctx, query, chunkClientIds, minCheckedAt.UTC())
			if err != nil {
				returnErr = err
				return
			}
			func() {
				defer rows.Close()
				for rows.Next() {
					var clientId server.Id
					if err := rows.Scan(&clientId); err != nil {
						returnErr = err
						return
					}
					if !chunkClientIdSet[clientId] || excludedClientIds[clientId] {
						returnErr = fmt.Errorf("invalid provider hard-exclusion result population")
						return
					}
					excludedClientIds[clientId] = true
				}
				returnErr = rows.Err()
			}()
			if returnErr != nil {
				return
			}
		}
	}, server.OptReadOnly(), server.OptNoRetry())
	if returnErr != nil {
		return nil, returnErr
	}
	return excludedClientIds, nil
}

// The newer of a provider's two runs, the health run and the location probe,
// whatever their age, so a reader can tell stale evidence from none; nil when
// there is neither.
func egressEvidenceTime(run *EgressHealthRun, observedAt *time.Time) *time.Time {
	var evidenceTime *time.Time
	if run != nil {
		measuredAt := run.MeasuredAt.UTC()
		evidenceTime = &measuredAt
	}
	if observedAt != nil && (evidenceTime == nil || evidenceTime.Before(*observedAt)) {
		observedTime := observedAt.UTC()
		evidenceTime = &observedTime
	}
	return evidenceTime
}

// The dashboard's index label for a provider whose row the new rollup has not
// written.
const ProviderEgressIndexNone = "none"

// The online bucket's name where the buckets are labelled beside the two rank
// modes. No request can name it.
const ProviderEgressBucketOnline = "online"

// The three buckets, in the order the backfill borrows toward.
var ProviderEgressBuckets = []string{
	RankModeQuality,
	RankModeSpeed,
	ProviderEgressBucketOnline,
}

// The connected public pool as the rules see it, for the providers dashboard:
// how many providers of each bucket carry each index, and how many each rule
// leaves out.
type ProviderEgressCounts struct {
	// bucket (a rank mode, or online), then index label, to providers.
	// Membership is the rules' alone, before the reliability and performance
	// minimums of the score cache.
	BucketIndexCounts map[string]map[string]int64
	// reason to providers, each under the first rule that took it out of a
	// bucket, every reason present
	ReasonCounts map[string]int64
	// the largest index the settings can produce
	MaxIndex int
}

// Decides every connected, valid, active, top-level provider with a Public
// provide key -- the population the online provider gauge counts -- as the
// client-score job would, with the rollout flag as it is now.
func CountProviderEgress(ctx context.Context) *ProviderEgressCounts {
	settings := egressIndexSettings()
	egressTestEnabled := providerEgressTestEnabled()
	countFilter := newProviderCountFilter(ctx, egressTestEnabled)

	counts := &ProviderEgressCounts{
		BucketIndexCounts: map[string]map[string]int64{},
		ReasonCounts:      map[string]int64{},
		MaxIndex:          settings.MaxIndex(),
	}
	for _, bucket := range ProviderEgressBuckets {
		counts.BucketIndexCounts[bucket] = map[string]int64{}
	}
	for _, reason := range ProviderExcludedReasons {
		counts.ReasonCounts[reason] = 0
	}

	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id,
				network_client_location_reliability.egress_index,
				network_client_location_reliability.egress_quality,
				country_location.country_code
			FROM network_client_location_reliability
			INNER JOIN network_client ON
				network_client.client_id = network_client_location_reliability.client_id
			LEFT JOIN location AS country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			WHERE
				network_client.active = true AND
				network_client.source_client_id IS NULL AND
				network_client_location_reliability.connected = true AND
				network_client_location_reliability.valid = true AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE
						provide_key.client_id = network_client_location_reliability.client_id AND
						provide_key.provide_mode = $1
				)
			`,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var egressIndex *int
				var egressQuality *bool
				var publishedCountryCode *string
				server.Raise(result.Scan(
					&clientId,
					&egressIndex,
					&egressQuality,
					&publishedCountryCode,
				))
				decision := decideProviderEgress(
					countFilter.egressFacts(clientId, publishedCountryCode, egressIndex, egressQuality, settings),
					egressTestEnabled,
				)
				// the index as the dashboard labels it
				indexLabel := ProviderEgressIndexNone
				if egressIndex != nil {
					indexLabel = strconv.Itoa(*egressIndex)
				}
				if decision.quality {
					counts.BucketIndexCounts[RankModeQuality][indexLabel] += 1
				}
				if decision.speed {
					counts.BucketIndexCounts[RankModeSpeed][indexLabel] += 1
				}
				if decision.online {
					counts.BucketIndexCounts[ProviderEgressBucketOnline][indexLabel] += 1
				}
				if decision.reason != "" {
					counts.ReasonCounts[decision.reason] += 1
				}
			}
		})
	})
	return counts
}

// One provider as the rules of GEOMAP §10.3 see it, for `bringyourctl provider
// inspect`.
type ProviderEgressInspection struct {
	ClientId  server.Id
	Connected bool
	Valid     bool
	// the country the provider is published under, "" when unknown
	PublishedCountryCode string
	MaxNetTypeScore      int
	MaxNetTypeScoreSpeed int
	// the rollup's columns
	EgressIndex        *int
	EgressQuality      *bool
	EgressEvidenceTime *time.Time
	// the latest health run, and the index it gives now, which the next
	// rollup will store
	HealthRun    *ProviderEgressHealth
	CurrentIndex EgressIndex
	// the country a fresh probe observed the exit in, "" for none
	ObservedCountryCode     string
	Blackholed              bool
	TlsAuthenticationFailed bool
	EgressTestEnabled       bool
	Settings                *EgressIndexSettings
	// the decision, as the client-score job makes it
	Reason       string
	HardExcluded bool
	Quality      bool
	Speed        bool
	Online       bool
	Counted      bool
}

// Reads one provider's rollup row and evidence and decides it, or returns nil
// when the provider has no rollup row. It loads the whole blackhole, TLS and
// observed-country sets, as a pass does, so it is for an operator's one-off
// question and not for a request path.
func InspectProviderEgress(ctx context.Context, clientId server.Id) *ProviderEgressInspection {
	var inspection *ProviderEgressInspection
	var publishedCountryCode *string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.connected,
				network_client_location_reliability.valid,
				network_client_location_reliability.max_net_type_score,
				network_client_location_reliability.max_net_type_score_speed,
				network_client_location_reliability.egress_index,
				network_client_location_reliability.egress_quality,
				network_client_location_reliability.egress_evidence_time,
				country_location.country_code
			FROM network_client_location_reliability
			LEFT JOIN location AS country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			WHERE network_client_location_reliability.client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				inspection = &ProviderEgressInspection{
					ClientId: clientId,
				}
				server.Raise(result.Scan(
					&inspection.Connected,
					&inspection.Valid,
					&inspection.MaxNetTypeScore,
					&inspection.MaxNetTypeScoreSpeed,
					&inspection.EgressIndex,
					&inspection.EgressQuality,
					&inspection.EgressEvidenceTime,
					&publishedCountryCode,
				))
			}
		})
	})
	if inspection == nil {
		return nil
	}

	inspection.Settings = egressIndexSettings()
	inspection.EgressTestEnabled = providerEgressTestEnabled()
	countFilter := newProviderCountFilter(ctx, inspection.EgressTestEnabled)

	facts := countFilter.egressFacts(
		clientId,
		publishedCountryCode,
		inspection.EgressIndex,
		inspection.EgressQuality,
		inspection.Settings,
	)
	decision := decideProviderEgress(facts, inspection.EgressTestEnabled)
	inspection.PublishedCountryCode = normalizeCountryCode(publishedCountryCode)
	inspection.ObservedCountryCode = countFilter.countryCodes[clientId]
	inspection.Blackholed = facts.blackholed
	inspection.TlsAuthenticationFailed = facts.tlsAuthenticationFailed
	inspection.Reason = decision.reason
	inspection.HardExcluded = decision.hardExcluded
	inspection.Quality = decision.quality
	inspection.Speed = decision.speed
	inspection.Online = decision.online
	inspection.Counted = decision.counted

	inspection.HealthRun = GetProviderEgressHealth(ctx, clientId)
	var run *EgressHealthRun
	if inspection.HealthRun != nil {
		run = &EgressHealthRun{
			MeasuredAt:   inspection.HealthRun.MeasuredAt,
			OkCount:      inspection.HealthRun.OKCount,
			Total:        inspection.HealthRun.Total,
			ClassResults: inspection.HealthRun.ClassResults,
		}
	}
	inspection.CurrentIndex = ComputeEgressIndex(run, server.NowUtc(), inspection.Settings)
	return inspection
}

// A stored country code as the rules compare it: lower case, without the
// padding a char(2) column can carry, "" for none.
func normalizeCountryCode(countryCode *string) string {
	if countryCode == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*countryCode))
}
