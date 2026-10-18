// The URL-success rule of the egress buckets (FP2FIX.md, 2026-10-13): a
// provider the probe has measured repeatedly in the evidence window without a
// single URL success is out of every bucket, Online included, and out of the
// counts. A provider without that much history keeps the benefit of the doubt,
// as does every provider of a country where the probe itself fails for most
// providers (the country guard). The rule is bucket membership only. Probe
// admission, the rolling quota and its census never read it, so an excluded
// provider keeps being measured and its first success restores it on the next
// pass. Explicit client ids, and force_minimum with rank_mode quality or
// speed, keep only the common hard exclusions, as they do for the URL ratio.
// force_minimum with rank_mode online changes nothing, so that answer is the
// Online pool, without the providers the rule excludes; no current caller
// sends it.
//
// The Online URL ratio (FP2FIX.md, owner considering it 2026-10-15) extends
// the rule and is off by default: with `online_url_success_ratio` above zero,
// a provider measured as often, with a success but a share of successes
// below the ratio, leaves every bucket and the counts the same way, under
// the low_success reason, and returns once its share reaches the ratio.
// Unprobed providers and those with a single run keep Online. The country
// guard stays the share of measured providers with a success: a guard on the
// share at the ratio would exempt nearly every country where the probe works
// (78 of 93 countries on Main on 2026-10-15, the US among them). An exempt
// country keeps its providers below the ratio as it keeps those without a
// success, unless `online_url_ratio_country_exemption` is false. That
// applies the ratio in every country, and so also takes an exempt country's
// providers without a success out of Online, since they are below every
// ratio. The kill switch turns the ratio off with the rule.
//
// Each fleet pass (the score export, the location counts, the dashboard
// counts) computes the country guard over its own population from the
// evidence it already loaded, under its own settings, and holds the floor's
// margin against the verdicts the score export last published, the one
// previous state every process shares. The score export publishes its guard,
// with the kill switch, floor and margin it applied, for the readers that
// decide a few providers at a time and cannot see a country's population:
// provider status, `bringyourctl provider inspect` and the exempt-country
// gauge. Those readers decide by the published guard alone, so they say what
// the export did.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The fewest measured URL runs in the evidence window that can show a provider
// carries no traffic. One failed run is a fault, not a verdict: a working
// provider fails a single run to a site that is briefly down or to one expired
// DNS budget, and a new provider's first result would otherwise decide it
// before anything confirmed it. A second failure with no success in between
// follows the provider rather than the run: other runs of providers whose run
// failed DNS fail about 70% of the time. The probe retries a failed provider
// after about a minute (UrlFailureIntervalSeconds), so a dead provider reaches
// two within minutes. Two runs that close can share one longer fault; that
// costs a working provider Online only until its next success, which the
// next pass reads. A provider with one run or none is warming up or unprobed
// and stays in Online with its URL weight, neutral while unprobed.
const ProviderOnlineMinMeasuredUrlRuns = 2

// Measured at least ProviderOnlineMinMeasuredUrlRuns times in the evidence
// window, by current evidence, without one success.
func providerCarriesNoUrl(counts ProviderEgressHealthCounts, urlIndex EgressIndex) bool {
	return urlIndex.Evidence && ProviderOnlineMinMeasuredUrlRuns <= counts.Total && counts.OKCount <= 0
}

// Measured at least ProviderOnlineMinMeasuredUrlRuns times in the evidence
// window, by current evidence, with a success, at a share of successes below
// the Online URL ratio, compared exactly like the country floor. A provider
// without a success is providerCarriesNoUrl's, and a ratio of zero holds none.
func providerBelowOnlineUrlRatio(counts ProviderEgressHealthCounts, urlIndex EgressIndex, ratio float64) bool {
	return urlIndex.Evidence &&
		ProviderOnlineMinMeasuredUrlRuns <= counts.Total &&
		0 < counts.OKCount &&
		providerOnlineSuccessShareBelow(counts.OKCount, counts.Total, ratio)
}

// Whether the Online URL ratio is on: a provider without a success is below
// it. A positive ratio finer than a millionth rounds to zero in the exact
// comparison, so it is off like zero.
func providerOnlineUrlRatioOn(ratio float64) bool {
	return providerOnlineSuccessShareBelow(0, 1, ratio)
}

// One published country's measured Online providers: those with current URL
// evidence, those of them with at least one success, those the rule would
// exclude and those below the Online URL ratio, with the guard's verdict for
// the country.
type providerOnlineSuccessCount struct {
	Measured  int
	Succeeded int
	NoSuccess int
	// with a success but below the Online URL ratio
	// (providerBelowOnlineUrlRatio); zero while the ratio is off
	LowSuccess int
	// The country keeps its providers: the probe has not shown it works
	// there, because fewer than the floor of the measured providers have a
	// success, or, for a country the export's last trusted guard exempted,
	// fewer than the floor plus its margin.
	Exempt bool
}

// The country guard of the URL-success rule over one population, with the
// rule as the pass that counted it applies it: its kill switch, its floor,
// its Online URL ratio and the verdict for each measured country. A fleet
// pass builds its own from its settings and the evidence it loaded. A scoped
// reader decodes the one the score export published, so it decides as that
// export did, whatever its own provider.yml says. A nil baseline, one a
// reader could not load, applies the rule and the ratio nowhere: without it
// nothing shows the probe works anywhere.
type providerOnlineSuccessBaseline struct {
	computeTime time.Time
	// when the score export published it, zero for a fleet pass's own
	publishTime time.Time
	// the kill switch, the floor and its margin the verdicts were decided
	// under, and when the guard they held the margin against was computed,
	// zero for the plain floor
	enabled             bool
	floor               float64
	margin              float64
	previousComputeTime time.Time
	// the Online URL ratio the verdicts were applied with, zero for off, and
	// whether it keeps the country exemption
	urlSuccessRatio          float64
	urlRatioCountryExemption bool
	// normalized published country code, "" for none, to its measured
	// providers and verdict; a country without a measured provider is absent
	countryCodeCounts map[string]providerOnlineSuccessCount
}

// Whether the country keeps its providers: no provider is measured there, or
// the guard found too few of the measured ones with a success.
func (self *providerOnlineSuccessBaseline) exempt(countryCode string) bool {
	if self == nil {
		return true
	}
	count, ok := self.countryCodeCounts[countryCode]
	return !ok || count.Measured <= 0 || count.Exempt
}

// Whether the rule takes the country's providers without a success out of
// every bucket: it is enabled and the country is not exempt.
func (self *providerOnlineSuccessBaseline) appliesIn(countryCode string) bool {
	return self != nil && self.enabled && !self.exempt(countryCode)
}

// Whether the Online URL ratio takes the country's providers below it out of
// every bucket: the rule is enabled, the ratio is on, and the country is not
// exempt or the ratio does not keep the exemption.
func (self *providerOnlineSuccessBaseline) urlRatioAppliesIn(countryCode string) bool {
	return self != nil && self.enabled && providerOnlineUrlRatioOn(self.urlSuccessRatio) &&
		(!self.urlRatioCountryExemption || !self.exempt(countryCode))
}

// Whether a provider without a success leaves every bucket in the country:
// the rule applies there, or the Online URL ratio does, since such a provider
// is below every ratio that is on.
func (self *providerOnlineSuccessBaseline) noSuccessAppliesIn(countryCode string) bool {
	return self.appliesIn(countryCode) || self.urlRatioAppliesIn(countryCode)
}

// Whether the provider has a success but leaves every bucket for a share of
// successes below the Online URL ratio, which applies in the country.
func (self *providerOnlineSuccessBaseline) belowUrlRatioIn(counts ProviderEgressHealthCounts, urlIndex EgressIndex, countryCode string) bool {
	return self.urlRatioAppliesIn(countryCode) && providerBelowOnlineUrlRatio(counts, urlIndex, self.urlSuccessRatio)
}

// Decides each measured country's verdict in place. A country becomes exempt
// when its share is below floor, and one in previouslyExemptCountryCodes
// stays exempt until its share reaches floor plus margin (at most one), so
// that a share hovering at the floor does not flip the country, and the
// providers the exemption keeps, between passes. The band is half-open like
// the floor: a share of exactly floor plus margin ends the exemption, and a
// margin of zero is the plain floor.
func decideProviderOnlineSuccessCountries(
	countryCodeCounts map[string]providerOnlineSuccessCount,
	floor float64,
	margin float64,
	previouslyExemptCountryCodes map[string]bool,
) {
	for countryCode, count := range countryCodeCounts {
		threshold := floor
		if previouslyExemptCountryCodes[countryCode] {
			threshold = min(1, floor+margin)
		}
		count.Exempt = count.Measured <= 0 || providerOnlineSuccessShareBelow(count.Succeeded, count.Measured, threshold)
		countryCodeCounts[countryCode] = count
	}
}

// The measured countries the guard exempts, none for a nil guard.
func (self *providerOnlineSuccessBaseline) exemptCountryCodes() map[string]bool {
	exemptCountryCodes := map[string]bool{}
	if self == nil {
		return exemptCountryCodes
	}
	for countryCode, count := range self.countryCodeCounts {
		if 0 < count.Measured && count.Exempt {
			exemptCountryCodes[countryCode] = true
		}
	}
	return exemptCountryCodes
}

// The score export's last trusted published guard, the previous state that
// every computing path (the score export, the location counts, the dashboard
// counts) holds the floor's margin against. Main runs each pass in whichever
// process claims it, so a state a process kept for itself would alternate
// between processes; the published guard is the one state they all read. It
// is read from Redis, not through the status path's cache, so a pass sees the
// export's latest guard. Nil, the plain floor, without a trusted guard: none
// published, an unreadable or version 1 one, or one past its max age.
func readProviderOnlineSuccessPreviousBaseline(ctx context.Context, pass string) *providerOnlineSuccessBaseline {
	previous, err := readProviderOnlineSuccessBaseline(ctx, server.NowUtc())
	if err != nil {
		if glog.V(1) {
			glog.Infof("[egress]%s decides the online url-success margin by the plain floor (guard: %s)\n", pass, err)
		}
		return nil
	}
	return previous
}

// A share of the guard is compared in millionths, in integers, so a share
// equal to the floor as provider.yml writes it is never below it. A float
// product can be: 0.28·25 is 7.0000000000000009, which would put 7 of 25
// below a floor of 0.28. A floor finer than a millionth rounds to the
// nearest one.
const providerOnlineSuccessShareScale = 1000 * 1000

// Whether succeeded of measured is less than share, compared exactly.
func providerOnlineSuccessShareBelow(succeeded int, measured int, share float64) bool {
	scaledShare := int64(math.Round(share * providerOnlineSuccessShareScale))
	return int64(succeeded)*providerOnlineSuccessShareScale < scaledShare*int64(measured)
}

// Counts a pass's population by published country, and decides each country
// under the settings' floor and margin, holding the margin against the
// previous guard: every provider the common gates admit, with current URL
// evidence. Unmeasured providers say nothing about the probe and are left
// out, and so are providers the common gates exclude, which are in no bucket
// whatever their URLs. A nil previous guard decides by the plain floor. The
// guard carries the settings' Online URL ratio, and counts the providers
// below it, without deciding any country by it.
func newProviderOnlineSuccessBaseline(
	filter providerCountFilter,
	clientIdCountryCodes map[server.Id]string,
	settings *EgressIndexSettings,
	previous *providerOnlineSuccessBaseline,
) *providerOnlineSuccessBaseline {
	baseline := &providerOnlineSuccessBaseline{
		computeTime:              filter.now,
		enabled:                  settings.OnlineRequiresUrlSuccess,
		floor:                    settings.OnlineSuccessCountryFloor,
		margin:                   settings.OnlineSuccessCountryFloorMargin,
		urlSuccessRatio:          settings.OnlineUrlSuccessRatio,
		urlRatioCountryExemption: settings.OnlineUrlRatioCountryExemption,
		countryCodeCounts:        map[string]providerOnlineSuccessCount{},
	}
	for clientId, countryCode := range clientIdCountryCodes {
		if filter.hasHardEgressFailure(clientId) {
			continue
		}
		counts, urlIndex := filter.urlIndex(clientId, settings)
		if !urlIndex.Evidence {
			continue
		}
		count := baseline.countryCodeCounts[countryCode]
		count.Measured += 1
		if 0 < counts.OKCount {
			count.Succeeded += 1
		}
		if providerCarriesNoUrl(counts, urlIndex) {
			count.NoSuccess += 1
		}
		if providerBelowOnlineUrlRatio(counts, urlIndex, baseline.urlSuccessRatio) {
			count.LowSuccess += 1
		}
		baseline.countryCodeCounts[countryCode] = count
	}
	if previous != nil {
		baseline.previousComputeTime = previous.computeTime
	}
	decideProviderOnlineSuccessCountries(baseline.countryCodeCounts, baseline.floor, baseline.margin, previous.exemptCountryCodes())
	return baseline
}

// The published country of each counted row, as the rules compare it.
func providerCountRowCountryCodes(rows []providerCountRow) map[server.Id]string {
	clientIdCountryCodes := make(map[server.Id]string, len(rows))
	for _, row := range rows {
		clientIdCountryCodes[row.clientId] = normalizeCountryCode(row.claimedCountryCode)
	}
	return clientIdCountryCodes
}

// One country the guard exempts, with its measured Online providers, those of
// them with a success, and the providers the exemption keeps in Online.
type ProviderOnlineSuccessExemptCountry struct {
	CountryCode string
	Measured    int
	Succeeded   int
	Kept        int
}

// Every measured country the guard exempts, in country code order. A country
// without a measured provider is exempt too but has nothing to keep, so it is
// not listed. The exemption keeps the providers without a success, and those
// below the Online URL ratio while the ratio keeps the exemption. A ratio
// applied in every country takes both out of an exempt country too, so there
// the exemption keeps none.
func (self *providerOnlineSuccessBaseline) exemptCountries() []*ProviderOnlineSuccessExemptCountry {
	exemptCountries := []*ProviderOnlineSuccessExemptCountry{}
	if self == nil {
		return exemptCountries
	}
	keepsNone := providerOnlineUrlRatioOn(self.urlSuccessRatio) && !self.urlRatioCountryExemption
	for countryCode, count := range self.countryCodeCounts {
		if 0 < count.Measured && count.Exempt {
			kept := count.NoSuccess + count.LowSuccess
			if keepsNone {
				kept = 0
			}
			exemptCountries = append(exemptCountries, &ProviderOnlineSuccessExemptCountry{
				CountryCode: countryCode,
				Measured:    count.Measured,
				Succeeded:   count.Succeeded,
				Kept:        kept,
			})
		}
	}
	slices.SortFunc(exemptCountries, func(a *ProviderOnlineSuccessExemptCountry, b *ProviderOnlineSuccessExemptCountry) int {
		return strings.Compare(a.CountryCode, b.CountryCode)
	})
	return exemptCountries
}

// One line for a pass's log: the rule's state, the Online URL ratio while it
// is on, and each exempt country with its successes of measured providers and
// the providers it keeps, marked when only the margin keeps it exempt.
func (self *providerOnlineSuccessBaseline) logSummary() string {
	if !self.enabled {
		return "disabled (online_requires_url_success is not true)"
	}
	exemptCountries := self.exemptCountries()
	kept := 0
	exemptTexts := make([]string, 0, len(exemptCountries))
	for _, exemptCountry := range exemptCountries {
		kept += exemptCountry.Kept
		countryCode := exemptCountry.CountryCode
		if countryCode == "" {
			countryCode = "unknown"
		}
		exemptText := fmt.Sprintf("%s %d/%d kept %d", countryCode, exemptCountry.Succeeded, exemptCountry.Measured, exemptCountry.Kept)
		if !providerOnlineSuccessShareBelow(exemptCountry.Succeeded, exemptCountry.Measured, self.floor) {
			exemptText += " by margin"
		}
		exemptTexts = append(exemptTexts, exemptText)
	}
	marginText := "by the plain floor, without a trusted previous guard"
	if !self.previousComputeTime.IsZero() {
		marginText = fmt.Sprintf("against the guard computed %s", self.previousComputeTime.UTC().Format(time.RFC3339))
	}
	// nothing while the ratio is off, the line as it was before the ratio
	urlRatioText := ""
	if providerOnlineUrlRatioOn(self.urlSuccessRatio) {
		if self.urlRatioCountryExemption {
			urlRatioText = fmt.Sprintf(", online url ratio %v outside the exempt countries", self.urlSuccessRatio)
		} else {
			urlRatioText = fmt.Sprintf(", online url ratio %v in every country", self.urlSuccessRatio)
		}
	}
	return fmt.Sprintf(
		"country floor %v margin %v %s%s, %d countries exempt keeping %d providers [%s]",
		self.floor,
		self.margin,
		marginText,
		urlRatioText,
		len(exemptCountries),
		kept,
		strings.Join(exemptTexts, ", "),
	)
}

// The score export's guard, for the readers that decide a few providers at a
// time: the rule as the export applied it (its kill switch, floor and
// margin), its verdict for each measured country, which the margin makes
// depend on the export's previous verdicts too, and the counts behind each
// verdict. A reader decides by these alone, never by its own settings, so a
// provider's status, its inspection and the gauge say what the export did,
// even while provider.yml or the code differs between hosts, as it does
// during a rollout or after a rollback.
const providerOnlineSuccessBaselineKey = "{provider_online_success_baseline}"

// The version this code writes while the Online URL ratio is on, the newest
// it reads. Version 3 adds the ratio, whether it keeps the country exemption
// and each country's providers below it. A reader from before the ratio
// refuses it, and decides by no guard rather than without the ratio the
// export applied. Version 1 carried the counts alone and each reader applied
// its own kill switch and floor to them, which could disagree with the
// export; a reader treats it as no guard, every country exempt, until the
// export publishes a later version.
const providerOnlineSuccessBaselineVersion = 3

// The version of a guard without the Online URL ratio: what the code before
// the ratio writes, and what this code writes while the ratio is off, so a
// reader of either code reads it, and a rollout or a rollback with the ratio
// off changes no reader's guard. This code reads it as the rule with the
// ratio off.
const providerOnlineSuccessBaselineVersionWithoutUrlRatio = 2

// The oldest published guard a reader trusts. The export publishes a new one
// at the end of every pass, so one older than this means the export has
// stopped publishing it, as after a rollback to code without the rule, and
// its verdicts no longer describe the pool. A reader then exempts every
// country. A pass longer than this leaves readers exempting every country
// until it publishes.
const providerOnlineSuccessBaselineMaxAge = 60 * time.Minute

// How long a guard read or write may wait. Neither is worth holding up a
// status answer or the export for: a reader without a guard exempts every
// country.
const providerOnlineSuccessBaselineRedisTimeout = 2 * time.Second

// The published guard. Every field of its version is required: version 3
// adds the Online URL ratio's, which version 2 leaves out. The pointers tell
// a missing one from false or zero.
type providerOnlineSuccessBaselineDocument struct {
	Version     int                                           `json:"version"`
	ComputeTime time.Time                                     `json:"compute_time"`
	PublishTime time.Time                                     `json:"publish_time"`
	Enabled     *bool                                         `json:"enabled"`
	Floor       *float64                                      `json:"floor"`
	Margin      *float64                                      `json:"margin"`
	Countries   map[string]providerOnlineSuccessCountDocument `json:"countries"`
	// version 3
	UrlSuccessRatio          *float64 `json:"url_success_ratio,omitempty"`
	UrlRatioCountryExemption *bool    `json:"url_ratio_country_exemption,omitempty"`
}

// One measured country of the published guard.
type providerOnlineSuccessCountDocument struct {
	Measured  int   `json:"measured"`
	Succeeded int   `json:"succeeded"`
	NoSuccess int   `json:"no_success"`
	Exempt    *bool `json:"exempt"`
	// version 3
	LowSuccess *int `json:"low_success,omitempty"`
}

// The guard as the export publishes it at publishTime: version 3 while the
// Online URL ratio is on, otherwise version 2, as the code before the ratio
// writes it.
func encodeProviderOnlineSuccessBaseline(baseline *providerOnlineSuccessBaseline, publishTime time.Time) ([]byte, error) {
	urlRatioOn := providerOnlineUrlRatioOn(baseline.urlSuccessRatio)
	countries := make(map[string]providerOnlineSuccessCountDocument, len(baseline.countryCodeCounts))
	for countryCode, count := range baseline.countryCodeCounts {
		exempt := count.Exempt
		countryDocument := providerOnlineSuccessCountDocument{
			Measured:  count.Measured,
			Succeeded: count.Succeeded,
			NoSuccess: count.NoSuccess,
			Exempt:    &exempt,
		}
		if urlRatioOn {
			lowSuccess := count.LowSuccess
			countryDocument.LowSuccess = &lowSuccess
		}
		countries[countryCode] = countryDocument
	}
	enabled := baseline.enabled
	floor := baseline.floor
	margin := baseline.margin
	document := &providerOnlineSuccessBaselineDocument{
		Version:     providerOnlineSuccessBaselineVersionWithoutUrlRatio,
		ComputeTime: baseline.computeTime,
		PublishTime: publishTime,
		Enabled:     &enabled,
		Floor:       &floor,
		Margin:      &margin,
		Countries:   countries,
	}
	if urlRatioOn {
		urlSuccessRatio := baseline.urlSuccessRatio
		urlRatioCountryExemption := baseline.urlRatioCountryExemption
		document.Version = providerOnlineSuccessBaselineVersion
		document.UrlSuccessRatio = &urlSuccessRatio
		document.UrlRatioCountryExemption = &urlRatioCountryExemption
	}
	return json.Marshal(document)
}

// Replaces the published guard, expiring with the export it describes, and
// hands it to this process's readers at once.
func publishProviderOnlineSuccessBaseline(ctx context.Context, baseline *providerOnlineSuccessBaseline, ttl time.Duration) error {
	publishTime := server.NowUtc()
	value, err := encodeProviderOnlineSuccessBaseline(baseline, publishTime)
	if err != nil {
		return err
	}
	redisCtx, cancel := context.WithTimeout(ctx, providerOnlineSuccessBaselineRedisTimeout)
	defer cancel()
	err = server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		return r.Set(redisCtx, providerOnlineSuccessBaselineKey, value, ttl).Err()
	})
	if err != nil {
		return err
	}
	published := *baseline
	published.publishTime = publishTime
	providerOnlineSuccessBaselineReader.store(&published)
	return nil
}

// The guard the score export last published, nil when there is none or it is
// not to be trusted, which applies the rule nowhere. This process reads it
// from Redis at most once in providerOnlineSuccessBaselineReadMaxAge.
func getProviderOnlineSuccessBaseline(ctx context.Context) *providerOnlineSuccessBaseline {
	return providerOnlineSuccessBaselineReader.get(ctx)
}

// The states of the published guard a reader meets in the ordinary course of
// a rollout: no guard yet, or a version 1 guard from the code before this
// one. A reader logs them as information, not as errors. Every reason a read
// returns reads as the clause after "online url-success guard: ". The
// rollout of the Online URL ratio meets a version 2 guard, from the code
// before the ratio, and every export with the ratio off publishes one: that
// state is no reason at all, since a reader trusts it as the rule with the
// ratio off.
var (
	errProviderOnlineSuccessBaselineNone     = errors.New("none is published")
	errProviderOnlineSuccessBaselineVersion1 = errors.New("the published one is version 1, which does not say how the export applied the rule")
)

// Whether a reason a read returns is one of the expected states.
func providerOnlineSuccessBaselineExpectedErr(err error) bool {
	return errors.Is(err, errProviderOnlineSuccessBaselineNone) || errors.Is(err, errProviderOnlineSuccessBaselineVersion1)
}

// Reads the published guard from Redis at now: an error when there is none,
// Redis is unavailable, or the document is not to be trusted.
func readProviderOnlineSuccessBaseline(ctx context.Context, now time.Time) (*providerOnlineSuccessBaseline, error) {
	var value []byte
	redisCtx, cancel := context.WithTimeout(ctx, providerOnlineSuccessBaselineRedisTimeout)
	defer cancel()
	err := server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		var err error
		value, err = r.Get(redisCtx, providerOnlineSuccessBaselineKey).Bytes()
		return err
	})
	if errors.Is(err, redis.Nil) {
		return nil, errProviderOnlineSuccessBaselineNone
	}
	if err != nil {
		return nil, fmt.Errorf("Redis is unavailable (%w)", err)
	}
	baseline, err := decodeProviderOnlineSuccessBaseline(value, now)
	if providerOnlineSuccessBaselineExpectedErr(err) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("the published one is not to be trusted (%w)", err)
	}
	return baseline, nil
}

// How long a process reuses the guard it read. Every provider status call
// and inspection reads the guard, a Redis read and the decode of about 15 KB,
// so a process reads it at most once in this time. The export publishes a
// new guard once a pass, minutes apart, and the publishing process hands its
// own to its readers at once, so this delays what a reader in another process
// sees by at most this much.
const providerOnlineSuccessBaselineReadMaxAge = 30 * time.Second

// The guard as this process last read it, shared by every scoped reader. A
// failed read is kept for the window too, so an unavailable Redis costs one
// read a window rather than one a status call, and each failure window is
// logged once when it starts and once when it ends, however many readers ask
// meanwhile. While one caller reads past the window, the others keep the last
// result rather than wait or read too. A read does not end with the caller
// that started it, since its result is everyone's. Safe for concurrent use.
type providerOnlineSuccessBaselineReads struct {
	maxAge time.Duration
	now    func() time.Time
	read   func(ctx context.Context, now time.Time) (*providerOnlineSuccessBaseline, error)
	// log the start of a failure window and its end
	errorf func(format string, args ...any)
	infof  func(format string, args ...any)

	stateLock sync.Mutex
	// the last result, nil for a failed read, and when it was read or stored;
	// hasResult is false before the first
	baseline   *providerOnlineSuccessBaseline
	resultTime time.Time
	hasResult  bool
	// A cold or expired read owns its generation. Publication or reset
	// supersedes it, so its completion cannot replace the installed state.
	reading    bool
	generation uint64
	// the last read failed, and its failure window has been logged
	failing bool
}

// This process's guard reads.
var providerOnlineSuccessBaselineReader = &providerOnlineSuccessBaselineReads{
	maxAge: providerOnlineSuccessBaselineReadMaxAge,
	now:    server.NowUtc,
	read:   readProviderOnlineSuccessBaseline,
	errorf: glog.Errorf,
	infof:  glog.Infof,
}

// Forgets the last read on a test reset, as a fresh process would.
func init() {
	server.OnReset(func() {
		providerOnlineSuccessBaselineReader.reset()
	})
}

// The guard as last read within the window, or read now.
func (self *providerOnlineSuccessBaselineReads) get(ctx context.Context) *providerOnlineSuccessBaseline {
	now := self.now()
	var generation uint64
	baseline, current := func() (*providerOnlineSuccessBaseline, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.reading || (self.hasResult && now.Sub(self.resultTime) < self.maxAge) {
			return self.baseline, true
		}
		self.generation += 1
		generation = self.generation
		self.reading = true
		return nil, false
	}()
	if current {
		return baseline
	}

	baseline, err := func() (baseline *providerOnlineSuccessBaseline, err error) {
		// a read that panics must not leave every later caller on the last
		// result; the guard is advisory, so it fails like any other read
		defer func() {
			if recovered := recover(); recovered != nil {
				baseline, err = nil, fmt.Errorf("read failed: %v", recovered)
			}
		}()
		// The result serves every caller in the window, so the caller's own
		// cancellation must not fail it: a status request that ends early
		// would empty the guard for everyone. The read keeps its own timeout.
		return self.read(context.WithoutCancel(ctx), now)
	}()
	failureStarted, failureEnded := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if generation != self.generation {
			baseline = self.baseline
			return false, false
		}
		self.reading = false
		self.baseline = baseline
		self.resultTime = now
		self.hasResult = true
		failureStarted := err != nil && !self.failing
		failureEnded := err == nil && self.failing
		self.failing = err != nil
		return failureStarted, failureEnded
	}()
	if failureStarted {
		// the expected states of a rollout are information; the rest are errors
		logf := self.errorf
		if providerOnlineSuccessBaselineExpectedErr(err) {
			logf = self.infof
		}
		logf("[egress]online url-success guard: %s; every country is exempt until a trusted guard is read\n", err)
	}
	if failureEnded {
		self.infof("[egress]online url-success guard: a trusted guard is read again\n")
	}
	return baseline
}

// Replaces the last result with a guard this process just published, which
// is current for a full window and ends any failure window.
func (self *providerOnlineSuccessBaselineReads) store(baseline *providerOnlineSuccessBaseline) {
	failureEnded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.generation += 1
		self.reading = false
		self.baseline = baseline
		self.resultTime = self.now()
		self.hasResult = true
		failureEnded := self.failing
		self.failing = false
		return failureEnded
	}()
	if failureEnded {
		self.infof("[egress]online url-success guard: a trusted guard is read again\n")
	}
}

// Forgets the last result, so the next caller reads.
func (self *providerOnlineSuccessBaselineReads) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.generation += 1
	self.baseline = nil
	self.resultTime = time.Time{}
	self.hasResult = false
	self.reading = false
	self.failing = false
}

// A published guard read at now, or an error for any document a reader must
// not trust: another version, a missing field, inconsistent counts, or one
// published more than providerOnlineSuccessBaselineMaxAge ago. A version 2
// guard is the rule with the Online URL ratio off.
func decodeProviderOnlineSuccessBaseline(value []byte, now time.Time) (*providerOnlineSuccessBaseline, error) {
	var document providerOnlineSuccessBaselineDocument
	if err := json.Unmarshal(value, &document); err != nil {
		return nil, fmt.Errorf("it is not json: %w", err)
	}
	if document.Version == 1 {
		return nil, errProviderOnlineSuccessBaselineVersion1
	}
	if document.Version != providerOnlineSuccessBaselineVersionWithoutUrlRatio && document.Version != providerOnlineSuccessBaselineVersion {
		return nil, fmt.Errorf("it is version %d, not %d or %d", document.Version, providerOnlineSuccessBaselineVersionWithoutUrlRatio, providerOnlineSuccessBaselineVersion)
	}
	if document.Enabled == nil ||
		document.Floor == nil || !(0 <= *document.Floor && *document.Floor <= 1) ||
		document.Margin == nil || !(0 <= *document.Margin && *document.Margin <= 1) {
		return nil, fmt.Errorf("it does not say how the export applied the rule")
	}
	withUrlRatio := document.Version == providerOnlineSuccessBaselineVersion
	urlSuccessRatio := 0.0
	urlRatioCountryExemption := true
	if withUrlRatio {
		if document.UrlSuccessRatio == nil || !(0 <= *document.UrlSuccessRatio && *document.UrlSuccessRatio <= 1) ||
			document.UrlRatioCountryExemption == nil {
			return nil, fmt.Errorf("it does not say how the export applied the online url ratio")
		}
		urlSuccessRatio = *document.UrlSuccessRatio
		urlRatioCountryExemption = *document.UrlRatioCountryExemption
	}
	if document.Countries == nil {
		return nil, fmt.Errorf("it has no countries")
	}
	if age := now.Sub(document.PublishTime); providerOnlineSuccessBaselineMaxAge < age {
		return nil, fmt.Errorf("it was published %s ago, more than %s", age.Round(time.Second), providerOnlineSuccessBaselineMaxAge)
	}
	countryCodeCounts := make(map[string]providerOnlineSuccessCount, len(document.Countries))
	for countryCode, count := range document.Countries {
		if count.Exempt == nil || count.Succeeded < 0 || count.NoSuccess < 0 || count.Measured < count.Succeeded+count.NoSuccess {
			return nil, fmt.Errorf("its country %q has inconsistent counts", countryCode)
		}
		lowSuccess := 0
		if withUrlRatio {
			// a provider below the ratio has a success
			if count.LowSuccess == nil || *count.LowSuccess < 0 || count.Succeeded < *count.LowSuccess {
				return nil, fmt.Errorf("its country %q has inconsistent counts", countryCode)
			}
			lowSuccess = *count.LowSuccess
		}
		countryCodeCounts[countryCode] = providerOnlineSuccessCount{
			Measured:   count.Measured,
			Succeeded:  count.Succeeded,
			NoSuccess:  count.NoSuccess,
			LowSuccess: lowSuccess,
			Exempt:     *count.Exempt,
		}
	}
	return &providerOnlineSuccessBaseline{
		computeTime:              document.ComputeTime,
		publishTime:              document.PublishTime,
		enabled:                  *document.Enabled,
		floor:                    *document.Floor,
		margin:                   *document.Margin,
		urlSuccessRatio:          urlSuccessRatio,
		urlRatioCountryExemption: urlRatioCountryExemption,
		countryCodeCounts:        countryCodeCounts,
	}, nil
}

// The countries the URL-success rule exempts now, as the score export last
// published them. False when no guard is readable, or the export applied the
// rule nowhere, when there is nothing to publish.
func GetProviderOnlineSuccessExemptCountries(ctx context.Context) ([]*ProviderOnlineSuccessExemptCountry, bool) {
	baseline := getProviderOnlineSuccessBaseline(ctx)
	if baseline == nil || !baseline.enabled {
		return nil, false
	}
	return baseline.exemptCountries(), true
}
