// The URL-success rule of the egress buckets (FP2FIX.md, 2026-10-13): a
// provider the probe has measured repeatedly in the evidence window without a
// single URL success is out of every bucket, Online included, and out of the
// counts. A provider without that much history keeps the benefit of the doubt,
// as does every provider of a country where the probe itself fails for most
// providers (the country guard). The rule is bucket membership only. Probe
// admission, the rolling quota and its census never read it, so an excluded
// provider keeps being measured and its first success restores it on the next
// pass. Explicit client ids and force_minimum keep only the common hard
// exclusions, as they do for the URL ratio.
//
// Each fleet pass (the score export, the location counts, the dashboard
// counts) computes the country guard over its own population from the
// evidence it already loaded. The score export publishes its guard for the
// readers that decide a few providers at a time and cannot see a country's
// population: provider status, `bringyourctl provider inspect` and the
// exempt-country gauge.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// One published country's measured Online providers: those with current URL
// evidence, those of them with at least one success, and those the rule would
// exclude.
type providerOnlineSuccessCount struct {
	Measured  int `json:"measured"`
	Succeeded int `json:"succeeded"`
	NoSuccess int `json:"no_success"`
}

// The country guard of the URL-success rule over one population. A nil
// baseline, one a reader could not load, exempts every country: without it
// nothing shows the probe works anywhere.
type providerOnlineSuccessBaseline struct {
	computeTime time.Time
	// normalized published country code, "" for none, to its providers
	countryCodeCounts map[string]providerOnlineSuccessCount
}

// Whether the country keeps its providers: the probe has not shown it works
// there, because no provider is measured or fewer than floor of the measured
// providers have a success.
func (self *providerOnlineSuccessBaseline) exempt(countryCode string, floor float64) bool {
	if self == nil {
		return true
	}
	count := self.countryCodeCounts[countryCode]
	return count.Measured <= 0 || float64(count.Succeeded) < floor*float64(count.Measured)
}

// Counts a pass's population by published country: every provider the common
// gates admit, with current URL evidence. Unmeasured providers say nothing
// about the probe and are left out, and so are providers the common gates
// exclude, which are in no bucket whatever their URLs.
func newProviderOnlineSuccessBaseline(
	filter providerCountFilter,
	clientIdCountryCodes map[server.Id]string,
	settings *EgressIndexSettings,
) *providerOnlineSuccessBaseline {
	baseline := &providerOnlineSuccessBaseline{
		computeTime:       filter.now,
		countryCodeCounts: map[string]providerOnlineSuccessCount{},
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
		baseline.countryCodeCounts[countryCode] = count
	}
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

// Every measured country the guard exempts under floor, in country code order.
// A country without a measured provider is exempt too but has nothing to keep,
// so it is not listed.
func (self *providerOnlineSuccessBaseline) exemptCountries(floor float64) []*ProviderOnlineSuccessExemptCountry {
	exemptCountries := []*ProviderOnlineSuccessExemptCountry{}
	if self == nil {
		return exemptCountries
	}
	for countryCode, count := range self.countryCodeCounts {
		if 0 < count.Measured && self.exempt(countryCode, floor) {
			exemptCountries = append(exemptCountries, &ProviderOnlineSuccessExemptCountry{
				CountryCode: countryCode,
				Measured:    count.Measured,
				Succeeded:   count.Succeeded,
				Kept:        count.NoSuccess,
			})
		}
	}
	slices.SortFunc(exemptCountries, func(a *ProviderOnlineSuccessExemptCountry, b *ProviderOnlineSuccessExemptCountry) int {
		return strings.Compare(a.CountryCode, b.CountryCode)
	})
	return exemptCountries
}

// One line for a pass's log: the rule's state, and each exempt country with
// its successes of measured providers and the providers it keeps.
func (self *providerOnlineSuccessBaseline) logSummary(settings *EgressIndexSettings) string {
	if !settings.OnlineRequiresUrlSuccess {
		return "disabled (online_requires_url_success: false)"
	}
	exemptCountries := self.exemptCountries(settings.OnlineSuccessCountryFloor)
	kept := 0
	exemptTexts := make([]string, 0, len(exemptCountries))
	for _, exemptCountry := range exemptCountries {
		kept += exemptCountry.Kept
		countryCode := exemptCountry.CountryCode
		if countryCode == "" {
			countryCode = "unknown"
		}
		exemptTexts = append(exemptTexts, fmt.Sprintf("%s %d/%d kept %d", countryCode, exemptCountry.Succeeded, exemptCountry.Measured, exemptCountry.Kept))
	}
	return fmt.Sprintf(
		"country floor %v, %d countries exempt keeping %d providers [%s]",
		settings.OnlineSuccessCountryFloor,
		len(exemptCountries),
		kept,
		strings.Join(exemptTexts, ", "),
	)
}

// The score export's guard, for readers that decide a few providers at a time.
// It holds counts, not verdicts, so each reader applies the floor and the kill
// switch of its own settings.
const providerOnlineSuccessBaselineKey = "{provider_online_success_baseline}"

// A reader accepts only this version of the document.
const providerOnlineSuccessBaselineVersion = 1

// How long a guard read or write may wait. Neither is worth holding up a
// status answer or the export for: a reader without a guard exempts every
// country.
const providerOnlineSuccessBaselineRedisTimeout = 2 * time.Second

type providerOnlineSuccessBaselineDocument struct {
	Version     int                                   `json:"version"`
	ComputeTime time.Time                             `json:"compute_time"`
	Countries   map[string]providerOnlineSuccessCount `json:"countries"`
}

// Replaces the published guard, expiring with the export it describes.
func publishProviderOnlineSuccessBaseline(ctx context.Context, baseline *providerOnlineSuccessBaseline, ttl time.Duration) error {
	value, err := json.Marshal(&providerOnlineSuccessBaselineDocument{
		Version:     providerOnlineSuccessBaselineVersion,
		ComputeTime: baseline.computeTime,
		Countries:   baseline.countryCodeCounts,
	})
	if err != nil {
		return err
	}
	redisCtx, cancel := context.WithTimeout(ctx, providerOnlineSuccessBaselineRedisTimeout)
	defer cancel()
	return server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		return r.Set(redisCtx, providerOnlineSuccessBaselineKey, value, ttl).Err()
	})
}

// The guard the score export last published, nil when there is none or it is
// unreadable, which exempts every country.
func getProviderOnlineSuccessBaseline(ctx context.Context) *providerOnlineSuccessBaseline {
	var value []byte
	redisCtx, cancel := context.WithTimeout(ctx, providerOnlineSuccessBaselineRedisTimeout)
	defer cancel()
	err := server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		var err error
		value, err = r.Get(redisCtx, providerOnlineSuccessBaselineKey).Bytes()
		return err
	})
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		glog.Errorf("[egress]online url-success guard is unavailable (%s); every country is exempt\n", err)
		return nil
	}
	baseline, err := decodeProviderOnlineSuccessBaseline(value)
	if err != nil {
		glog.Errorf("[egress]online url-success guard is unreadable (%s); every country is exempt\n", err)
		return nil
	}
	return baseline
}

// A published guard, or an error for any document a reader must not trust.
func decodeProviderOnlineSuccessBaseline(value []byte) (*providerOnlineSuccessBaseline, error) {
	var document providerOnlineSuccessBaselineDocument
	if err := json.Unmarshal(value, &document); err != nil {
		return nil, err
	}
	if document.Version != providerOnlineSuccessBaselineVersion {
		return nil, fmt.Errorf("version %d is not %d", document.Version, providerOnlineSuccessBaselineVersion)
	}
	if document.Countries == nil {
		return nil, fmt.Errorf("no countries")
	}
	for countryCode, count := range document.Countries {
		if count.Succeeded < 0 || count.NoSuccess < 0 || count.Measured < count.Succeeded+count.NoSuccess {
			return nil, fmt.Errorf("country %q has inconsistent counts", countryCode)
		}
	}
	return &providerOnlineSuccessBaseline{
		computeTime:       document.ComputeTime,
		countryCodeCounts: document.Countries,
	}, nil
}

// The countries the URL-success rule exempts now: the guard the score export
// last published, under this process's floor. False when the rule is disabled
// or no guard is readable, when there is nothing to publish.
func GetProviderOnlineSuccessExemptCountries(ctx context.Context) ([]*ProviderOnlineSuccessExemptCountry, bool) {
	settings := egressIndexSettings()
	if !settings.OnlineRequiresUrlSuccess {
		return nil, false
	}
	baseline := getProviderOnlineSuccessBaseline(ctx)
	if baseline == nil {
		return nil, false
	}
	return baseline.exemptCountries(settings.OnlineSuccessCountryFloor), true
}
