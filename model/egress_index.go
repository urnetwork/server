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

// Provider serving eligibility is shared by score export, location counts,
// request hard exclusions and diagnostics. Reliability, ARIN risk and persistent
// URL security exceptions gate every bucket. Online includes every common-gate
// pass; quality and speed additionally require a passing ratio of accepted URL
// outcomes within eight hours. Only quality excludes ARIN non-quality entries.
// Performance and the success ratio order admitted providers, never add gates.

// The weights of the index and the bounds of its evidence, with the other
// tunables of the egress rules. The `egress_index` block of provider.yml,
// beside the rollout flag, overrides any of the index's (egressIndexSettings).
type EgressIndexSettings struct {
	// Explicit reader activation after complete native publication is attested.
	// False keeps union-cache reads while receipt writers and publishers roll out.
	NativeReaderEnabled bool
	// Legacy configuration compatibility; measured URLs now have equal weight.
	ClassWeights map[string]int
	// Legacy configuration compatibility, unused by the URL success ratio.
	DefaultClassWeight int
	// Maximum ranking penalty for a measured failure ratio of one.
	MaxFailureIndex int
	// Evidence lifetime, capped at eight hours; the exact boundary is stale.
	EvidenceMaxAge time.Duration
	// A window passes when
	// QualityOkNumerator·total ≤ QualityOkDenominator·ok over all its scored
	// loads, compared in integers so the boundary is exact for every total.
	QualityOkNumerator   int
	QualityOkDenominator int
	// Legacy configuration compatibility; any measured denominator above zero
	// is evidence, regardless of the former minimum sample size.
	MinScoredLoads int
	// Legacy diagnostic setting. Only ARIN risk gates country admission.
	CountryGate bool
	// added to the tier of a provider FindProviders2
	// borrows from the other bucket when a request's own bucket comes up short
	// (GEOMAP §10.3 "Backfill"); a provider borrowed from the online bucket
	// carries twice the offset. The client ranks on the tier plus demerits of
	// its own, up to MaxClientTierDemerit, which the server never sees, so the
	// default holds both edges of the borrowed band with every demerit added
	// on the better side and none on the worse:
	//
	//   - a native carries at most ClientScoreCutoffTier (3), so at most 10
	//     demerited, and a borrowed provider at least the offset: a demerited
	//     native ranks ahead of every borrowed provider from an offset of
	//     3 + 1 + 7 = 11;
	//   - a provider borrowed from the other bucket carries at most
	//     ClientScoreCutoffTier (3) plus the offset, so at most the offset
	//     plus 10 demerited, and an online one twice the offset: a demerited
	//     borrowed provider ranks ahead of the online bucket from an offset of
	//     3 + 1 + 7 = 11.
	//
	// The default is the larger, 11, with online at 22, and the borrowed keep
	// their order among themselves. The client bounds no tier -- a larger one
	// only ranks later -- so it can be raised. It is never set below
	// ClientScoreCutoffTier + 1 (4), the least that keeps every native
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
		EvidenceMaxAge:        ProviderEgressHealthMaxAge,
		QualityOkNumerator:    4,
		QualityOkDenominator:  5,
		MinScoredLoads:        1,
		CountryGate:           true,
		BackfillTierOffset:    ClientScoreCutoffTier + 1 + MaxClientTierDemerit,
		RequestSettingsMaxAge: time.Minute,
	}
}

// The `egress_index` block of provider.yml. Every field is optional; an absent
// one keeps its default.
type egressIndexSettingsDocument struct {
	EgressIndex *struct {
		NativeReaderEnabled  *bool          `yaml:"native_reader_enabled"`
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

	if overrides.NativeReaderEnabled != nil {
		settings.NativeReaderEnabled = *overrides.NativeReaderEnabled
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
	setInt("backfill_tier_offset", overrides.BackfillTierOffset, ClientScoreCutoffTier+1, &settings.BackfillTierOffset)

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

// One immutable settings pointer is captured by each discovery request, so a
// refresh cannot switch cache schemas between its primary and alternate loads.
func requestEgressIndexSettings() *EgressIndexSettings {
	snapshot := requestEgressIndexSettingsSnapshot.Load()
	if snapshot == nil || snapshot.settings.RequestSettingsMaxAge <= time.Since(snapshot.loadTime) {
		snapshot = &egressIndexSettingsSnapshot{settings: egressIndexSettings(), loadTime: time.Now()}
		requestEgressIndexSettingsSnapshot.Store(snapshot)
	}
	return snapshot.settings
}

// Quiet processes also expose their effective setting. Scraping uses the same
// bounded local-config cache as requests; it does not read provider storage.
var findProviders2NativeReaderEnabled = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_native_reader_enabled",
	Help: "Effective cached native-reader configuration for non-forced discovery requests; does not attest publication or request outcomes",
}, func() float64 {
	if requestEgressIndexSettings().NativeReaderEnabled {
		return 1
	}
	return 0
})

// Registers the backfill metrics, and forgets the request settings on a test
// reset so a test's provider.yml is read.
func init() {
	prometheus.MustRegister(findProviders2BackfillProviders, findProviders2AnsweredProviders, findProviders2NativeReaderEnabled)
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
	// the success-ratio verdict over measured URLs, meaningful only with
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

// Computes admission and ranking from accepted URL outcomes in the evidence
// window. Zero measured outcomes are unknown. The index is the ceiling of the
// failure fraction times MaxFailureIndex; every measured URL has equal weight.
func ComputeEgressIndex(run *EgressHealthRun, now time.Time, settings *EgressIndexSettings) EgressIndex {
	if run == nil || run.Total <= 0 || !run.MeasuredAt.After(now.Add(-min(settings.EvidenceMaxAge, ProviderEgressHealthMaxAge))) {
		return EgressIndex{}
	}

	// Normalize by measured URLs so collecting more samples cannot turn the
	// same success ratio into a worse tier merely by accumulating failures.
	failures := max(0, run.Total-run.OkCount)
	failureIndex := (int64(failures)*int64(settings.MaxFailureIndex) + int64(run.Total) - 1) / int64(run.Total)
	return EgressIndex{
		Index:    int(min(failureIndex, int64(settings.MaxFailureIndex), int64(math.MaxInt16))),
		Evidence: true,
		Quality:  settings.QualityOkNumerator*run.Total <= settings.QualityOkDenominator*run.OkCount,
	}
}

// A measured success ratio contributes a positive selection weight alongside
// reliability. Missing evidence is neutral and remains available online.
func providerUrlProbeSuccessWeight(counts ProviderEgressHealthCounts) float64 {
	if counts.Total <= 0 {
		return 1
	}
	ratio := max(0.0, min(1.0, float64(counts.OKCount)/float64(counts.Total)))
	return 0.1 + 0.9*ratio
}

// Common gate failures exclude every bucket. Health/unprobed and ARIN
// non-quality explain missing native membership while preserving online supply.
// Legacy blackhole/country reason labels remain for metric compatibility only.
const (
	ProviderExcludedBlackhole      = "blackhole"
	ProviderExcludedTls            = "tls"
	ProviderExcludedCountry        = "country"
	ProviderExcludedHealth         = "health"
	ProviderExcludedUnprobed       = "unprobed"
	ProviderExcludedArinRisk       = "arin_risk"
	ProviderExcludedArinNonQuality = "arin_non_quality"
	ProviderExcludedReliability    = "reliability"
)

// The reasons in the order the rules apply.
var ProviderExcludedReasons = []string{
	ProviderExcludedBlackhole,
	ProviderExcludedTls,
	ProviderExcludedCountry,
	ProviderExcludedHealth,
	ProviderExcludedUnprobed,
	ProviderExcludedArinRisk,
	ProviderExcludedArinNonQuality,
	ProviderExcludedReliability,
}

// What the rules read about one provider.
type providerEgressFacts struct {
	arinRisk                bool
	arinNonQuality          bool
	reliabilityFailed       bool
	blackholed              bool
	tlsAuthenticationFailed bool
	// a fresh probe observed the exit in a country other than the published one
	countryMismatch bool
	// The rollup index is a ranking diagnostic; the verdict uses current history.
	egressIndex   *int
	egressQuality *bool
}

// What the rules do with one provider.
type providerEgressDecision struct {
	// the first rule that took the provider out of a bucket, "" for none
	reason string
	// absent from every result and count, force_minimum and an explicit client
	// id included
	hardExcluded bool
	// Native membership in each request mode; performance adds no gate.
	quality bool
	speed   bool
	// Every common-gate pass belongs to online and advertised supply.
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
	// Read accepted health directly so an old rollup cannot extend freshness.
	var quality *bool
	if counts, ok := self.healthCounts[clientId]; ok {
		quality = ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount, Total: counts.Total}, self.now, settings).QualityVerdict()
	}
	return &providerEgressFacts{
		arinRisk:                self.arinRisk[clientId],
		arinNonQuality:          self.arinNonQuality[clientId],
		reliabilityFailed:       self.reliabilityFailed[clientId],
		blackholed:              self.isBlackholed(clientId),
		tlsAuthenticationFailed: self.tlsAuthenticationFailed[clientId],
		// a provider nobody has located, or whose own country is unknown,
		// cannot contradict itself
		countryMismatch: settings.CountryGate &&
			observedCountryCode != "" &&
			publishedCountryCode != "" &&
			observedCountryCode != publishedCountryCode,
		egressIndex:   egressIndex,
		egressQuality: quality,
	}
}

// Decides bucket membership once. The historical rollout flag has no authority
// to admit unmeasured rows or bypass common gates.
func decideProviderEgress(facts *providerEgressFacts, egressTestEnabled bool) providerEgressDecision {
	switch {
	case facts.reliabilityFailed:
		return providerEgressDecision{
			reason:       ProviderExcludedReliability,
			hardExcluded: true,
		}
	case facts.arinRisk:
		return providerEgressDecision{
			reason:       ProviderExcludedArinRisk,
			hardExcluded: true,
		}
	case facts.tlsAuthenticationFailed:
		return providerEgressDecision{
			reason:       ProviderExcludedTls,
			hardExcluded: true,
		}
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
		// A measured failing ratio belongs to online, not quality or speed.
		return providerEgressDecision{
			reason:  ProviderExcludedHealth,
			online:  true,
			counted: true,
		}
	default:
		decision := providerEgressDecision{
			quality: !facts.arinNonQuality,
			speed:   true,
			online:  true,
			counted: true,
		}
		if facts.arinNonQuality {
			decision.reason = ProviderExcludedArinNonQuality
		}
		return decision
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

// A complete cached snapshot of common gate failures covers normal, explicit
// client and force_minimum paths. Missing snapshots use bounded candidate reads.
const providerHardExclusionsKey = "{provider_hard_exclusions}"

// The same atomic set carries evidence that even an empty publication exists.
// This reserved member cannot be a provider id. Older writers omit it.
const providerHardExclusionsReadyMember = "ready:v2"

// Bound every input before evaluating the shared fleet policy. Without these
// request-only relation names, hashed EXISTS alternatives can scan the fleet
// even though the outer candidate list and returned rows are bounded.
func providerHardExclusionsSql() string {
	return `
		WITH client_connection_reliability_score AS MATERIALIZED (
			SELECT client_id, lookback_index, independent_reliability_weight
			FROM client_connection_reliability_score WHERE client_id = ANY($1)
		), provider_egress_health AS MATERIALIZED (
			SELECT client_id, tls_authentication_failure, legacy_tls_authentication_failure
			FROM provider_egress_health WHERE client_id = ANY($1)
		), provider_egress_url_security AS MATERIALIZED (
			SELECT client_id, tls_failure
			FROM provider_egress_url_security WHERE client_id = ANY($1)
		)
		SELECT client_id
		FROM network_client_location_reliability AS provider_location
		WHERE client_id = ANY($1) AND NOT (` + providerEgressEligibilitySql("provider_location") + `)
		UNION
		SELECT client_id
		FROM provider_egress_health
		WHERE client_id = ANY($1) AND tls_authentication_failure = true
		UNION
		SELECT client_id FROM provider_egress_url_security
		WHERE client_id = ANY($1) AND tls_failure
	`
}

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

// Read-through uses the exporter's reliability, ARIN-risk and security rules without loading
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
	query := providerHardExclusionsSql()
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
			rows, err := conn.Query(ctx, query, chunkClientIds)
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
	// Membership uses the shared common gates and URL ratio.
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
	ArinRisk                bool
	ArinNonQuality          bool
	ReliabilityFailed       bool
	UrlSuccessCount         int
	UrlErrorCount           int
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

// Reads one provider's rollup and accepted URL evidence for operator inspection.
// This diagnostic reads the publication pass's evidence for this provider only.
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
	countFilter := newProviderCountFilterForClients(ctx, []server.Id{clientId})

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
	inspection.ArinRisk = facts.arinRisk
	inspection.ArinNonQuality = facts.arinNonQuality
	inspection.ReliabilityFailed = facts.reliabilityFailed
	inspection.Reason = decision.reason
	inspection.HardExcluded = decision.hardExcluded
	inspection.Quality = decision.quality
	inspection.Speed = decision.speed
	inspection.Online = decision.online
	inspection.Counted = decision.counted

	inspection.HealthRun = GetProviderEgressHealth(ctx, clientId)
	var run *EgressHealthRun
	if counts, ok := countFilter.healthCounts[clientId]; ok {
		inspection.UrlSuccessCount = counts.OKCount
		inspection.UrlErrorCount = counts.Total - counts.OKCount
		run = &EgressHealthRun{
			MeasuredAt: counts.MeasuredAt,
			OkCount:    counts.OKCount,
			Total:      counts.Total,
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
