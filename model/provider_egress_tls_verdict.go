// An exact-URL TLS finding excludes its provider only once it is judged
// specific to that provider.
//
// A URL turn whose certificate fails verification on any hop stores a finding
// for that hop's URL (provider_egress_url_security, tls_failure). A site whose
// certificate expired, a prober whose CA bundle or clock is wrong, and a
// country whose networks answer blocked sites with their own certificate all
// produce exactly the finding a provider attack does. So a finding starts
// pending: it excludes nothing until the judge (the taskworker's
// JudgeProviderEgressTlsFindings) stores a verdict, and only the provider
// verdict excludes.
//
// The judge decides each finding on evidence close in time to it:
//
//   - retired: its destination has been out of the served pool on every
//     judge run for a whole site refresh interval (SiteRefreshIntervalSeconds,
//     one day). No drawn turn can fail or clear it while the pool does not
//     serve it, so rather than exclude and be rechecked indefinitely on a
//     URL nothing else measures, it neither excludes nor drives rechecks,
//     and nothing is deleted. A name out of the pool for less than that is
//     judged as if it were served, so a pool read that misses a name, a
//     configuration release in flight or a refresh that holds a site back
//     for a while retires nothing. Retirement follows the pool, not the
//     evidence: once the pool serves the name again the finding is judged
//     again like a new one, and it is pending, so rechecked, until a fresh
//     failure or the place rule decides it.
//   - site: a site or prober fault. Within the prober-fault window
//     (SiteProberFaultWindowSeconds, one hour) either the URL's host failed a
//     direct, verified TLS handshake from the server, which shares the
//     prober's CA bundle and clock, or providers on at least
//     CommonModeNetworkCount (16) networks failed TLS on the URL. Sixteen is
//     the URL common-mode guard's window: the fewest networks the prober
//     judges a shared failure on.
//   - place: a national block answering with its own certificate, which
//     fails the URL's TLS verification for every network in the provider's
//     country and says nothing about the provider. Over the site window
//     (SiteWindowSeconds, three days), and never counting the provider's own
//     network, whose providers one farm can fill: at least PlaceMinNetworks
//     (three, the server's distinct-network quorum) other networks in the
//     country hold a TLS finding on the exact URL; at least
//     SiteRegionFailShare (0.9) of the other networks' connected providers
//     there whose TLS on the URL was verified or failed saw it fail; and the
//     country's exits work, failing less than SiteRetireShare (0.5) of their
//     scored loads on every other site, over at least SiteRegionMinSamples
//     (30) such loads, so a country whose exits fail most sites is not read
//     as a block of each, and the blocked site's own failures do not hide
//     that the rest works. Only TLS verification outcomes count toward the
//     URL: a reset, a timeout or a block page is no evidence of a certificate
//     block, so resetting a site's handshakes for days before intercepting it
//     builds no place.
//   - provider: none of the above, and the URL's host authenticated a direct
//     TLS handshake from the server within the prober-fault window. The
//     certificate is good and the prober can verify it, so a provider that
//     presented another is intercepting. It excludes the provider.
//   - pending: none of the above and nothing corroborates it yet (the direct
//     check was inconclusive).
//
// Each failure is judged once, on evidence close in time to it: the verdict
// records the failure it decided (tls_verdict_measured_at), and a finding is
// judged again only while pending or once a newer failure refreshes it. A
// finding judged a site fault during an outage therefore stays one after the
// site is fixed, rather than reading as the provider's own once the direct
// check passes again; a provider verdict holds while its provider is offline;
// and every failed recheck is judged again on its own evidence. A pending
// finding older than the direct window keeps waiting for a fresh failure,
// since evidence taken now says nothing about one hours ago. Retirement is
// the one judgement that depends on time between runs: the judge carries
// from run to run, in its task's arguments, when each finding's destination
// name was first read out of the pool (NewProviderEgressTlsPool). A lost
// clock, from a new chain or a rollback, only starts the count again, which
// delays a retirement and never hastens one.
//
// Rechecks follow from the verdict too. Pending and provider findings are
// rechecked: the claim offers their stored destinations, and their provider
// is turned no faster than a fully measured one
// (providerUrlProbeSecurityPacedAttemptSql). A recheck's outcome is evidence
// about that URL alone, so it stays out of the eight-hour ratio and the
// rolling quota (storeProviderEgressHealth).
package model

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// One finding's stored judgement (provider_egress_url_security.tls_verdict).
type ProviderEgressTlsVerdict int16

const (
	// Not judged yet. Excludes nothing; rechecked at the capped pace.
	ProviderEgressTlsVerdictPending ProviderEgressTlsVerdict = 0
	// Specific to the provider. Excludes it from every bucket; rechecked at
	// the capped pace.
	ProviderEgressTlsVerdictProvider ProviderEgressTlsVerdict = 1
	// A site or prober fault. Excludes nothing; not rechecked.
	ProviderEgressTlsVerdictSite ProviderEgressTlsVerdict = 2
	// The site does not work from the provider's place. Excludes nothing;
	// not rechecked.
	ProviderEgressTlsVerdictPlace ProviderEgressTlsVerdict = 3
	// The pool has not served the URL's destination for a refresh interval.
	// Excludes nothing; not rechecked. Pending again once it is served.
	ProviderEgressTlsVerdictRetired ProviderEgressTlsVerdict = 4
)

// Every verdict, in label order, for metrics that report each one.
var ProviderEgressTlsVerdicts = []ProviderEgressTlsVerdict{
	ProviderEgressTlsVerdictPending,
	ProviderEgressTlsVerdictProvider,
	ProviderEgressTlsVerdictSite,
	ProviderEgressTlsVerdictPlace,
	ProviderEgressTlsVerdictRetired,
}

// The verdict's metric and log label.
func (self ProviderEgressTlsVerdict) String() string {
	switch self {
	case ProviderEgressTlsVerdictPending:
		return "pending"
	case ProviderEgressTlsVerdictProvider:
		return "provider"
	case ProviderEgressTlsVerdictSite:
		return "site"
	case ProviderEgressTlsVerdictPlace:
		return "place"
	case ProviderEgressTlsVerdictRetired:
		return "retired"
	default:
		return fmt.Sprintf("unknown_%d", int16(self))
	}
}

// Whether a finding with this verdict keeps its provider out of every bucket.
func (self ProviderEgressTlsVerdict) Excludes() bool {
	return self == ProviderEgressTlsVerdictProvider
}

// Whether a finding with this verdict is still offered for recheck.
func (self ProviderEgressTlsVerdict) Rechecked() bool {
	return self == ProviderEgressTlsVerdictPending || self == ProviderEgressTlsVerdictProvider
}

// An unresolved finding that excludes its provider. The alias names a
// provider_egress_url_security row; the partial index on tls_failure serves
// the lookup and the verdict filters its few rows.
func providerUrlSecurityExcludesSql(alias string) string {
	return fmt.Sprintf("(%s.tls_failure AND %s.tls_verdict = %d)", alias, alias, ProviderEgressTlsVerdictProvider)
}

// An unresolved finding the scheduler still rechecks.
func providerUrlSecurityRechecksSql(alias string) string {
	return fmt.Sprintf("(%s.tls_failure AND %s.tls_verdict IN (%d, %d))",
		alias, alias, ProviderEgressTlsVerdictPending, ProviderEgressTlsVerdictProvider)
}

// The outcome of one direct, verified TLS handshake from the server to a
// finding's host, with no provider in the path.
type ProviderEgressTlsDirectOutcome int

const (
	// No handshake completed or failed verification: a dial error, a
	// timeout, a reset, or no check made. Corroborates nothing.
	ProviderEgressTlsDirectInconclusive ProviderEgressTlsDirectOutcome = iota
	// The handshake completed and the chain and hostname verified.
	ProviderEgressTlsDirectVerified
	// The certificate failed verification, as the probe classifies it
	// (egresshealth.IsTlsAuthenticationFailure).
	ProviderEgressTlsDirectFailed
)

// The outcome's metric and log label.
func (self ProviderEgressTlsDirectOutcome) String() string {
	switch self {
	case ProviderEgressTlsDirectVerified:
		return "verified"
	case ProviderEgressTlsDirectFailed:
		return "failed"
	default:
		return "inconclusive"
	}
}

// The judge's windows and thresholds. Each is an existing convention of the
// prober or the pool refresh (see the package comment above), not a new knob;
// ProviderEgressTlsJudgeSettingsFor reads them from the site settings.
type ProviderEgressTlsJudgeSettings struct {
	// How close in time a direct check and other networks' failures must be
	// to a finding to judge it: the prober-fault window.
	DirectWindow time.Duration
	// The fewest distinct networks failing TLS on one URL within DirectWindow
	// that make the failure common-mode: the URL common-mode guard's window.
	CommonModeNetworkCount int
	// The evidence the place rule reads: the refresh's site window.
	PlaceWindow time.Duration
	// The fewest other networks in the country holding a TLS finding on the
	// URL: the server's distinct-network quorum (ProviderClientVerdictQuorum).
	PlaceMinNetworks int
	// The share of the other networks' TLS verification outcomes on the URL
	// at the place that must have failed: the refresh's share for marking a
	// site broken at a place.
	PlaceFailShare float64
	// The fewest scored loads of the place's other sites its working share
	// stands on: the refresh's fewest loads at a place a mark stands on.
	PlaceMinSamples int
	// The place's failure share over its scored loads on every other site
	// below which its exits work well enough to judge one site there.
	PlaceWorkingShare float64
	// How long a destination name must be out of the served pool, on every
	// run, before its findings are retired: the refresh interval, so an
	// absence has outlasted a whole refresh cycle rather than one pool read.
	RetireAfter time.Duration
}

// The judge's settings from the pool refresh's site settings, with the URL
// common-mode guard's network count.
func ProviderEgressTlsJudgeSettingsFor(site *ProviderEgressSiteSettings, commonModeNetworkCount int) *ProviderEgressTlsJudgeSettings {
	return &ProviderEgressTlsJudgeSettings{
		DirectWindow:           site.SiteProberFaultWindow(),
		CommonModeNetworkCount: commonModeNetworkCount,
		PlaceWindow:            site.SiteWindow(),
		PlaceMinNetworks:       ProviderClientVerdictQuorum,
		PlaceFailShare:         site.SiteRegionFailShare,
		PlaceMinSamples:        site.SiteRegionMinSamples,
		PlaceWorkingShare:      site.SiteRetireShare,
		RetireAfter:            site.SiteRefreshInterval(),
	}
}

// One unresolved finding as the judge reads it.
type ProviderEgressTlsFinding struct {
	ClientId server.Id
	UrlKey   string
	// the stored destination's catalog name and the hop URL that failed
	Name       string
	Url        string
	MeasuredAt time.Time
	Verdict    ProviderEgressTlsVerdict
	// the failure the stored verdict was decided on, nil when none was
	VerdictMeasuredAt *time.Time
	// the provider's network, nil when its client row is gone
	NetworkId *server.Id
	// the provider's published country, "" when unknown, and its location
	CountryCode       string
	CountryLocationId *server.Id
}

// The network the finding's provider counts as: its own network, or itself
// when its client row is gone, as the URL common-mode guard counts one.
func (self *ProviderEgressTlsFinding) network() string {
	if self.NetworkId != nil {
		return "network:" + self.NetworkId.String()
	}
	return "client:" + self.ClientId.String()
}

// Whether the stored verdict already decided the finding's latest failure.
// Pending decides nothing, and neither does retirement, which follows the
// pool: a retired finding whose destination is served again is judged again.
func (self *ProviderEgressTlsFinding) Judged() bool {
	return self.Verdict != ProviderEgressTlsVerdictPending && self.Verdict != ProviderEgressTlsVerdictRetired &&
		self.VerdictMeasuredAt != nil && self.VerdictMeasuredAt.Equal(self.MeasuredAt)
}

// The host the finding's URL dials: the name or address a direct check
// verifies, "" for a URL that is not a probe target.
func (self *ProviderEgressTlsFinding) Host() string {
	target, err := url.Parse(self.Url)
	if err != nil || target.Scheme != "https" {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
}

// Whether the finding is recent enough for a direct check now to judge it.
func (self *ProviderEgressTlsJudgeSettings) DirectCheckJudges(now time.Time, finding *ProviderEgressTlsFinding) bool {
	return now.Sub(finding.MeasuredAt) <= self.DirectWindow
}

// Reads the unresolved findings, newest first, at most limit, each with its
// provider's network and published country. The partial index on tls_failure
// holds only unresolved rows; the joins are primary-key lookups per row.
func GetProviderEgressTlsFindings(ctx context.Context, limit int) []*ProviderEgressTlsFinding {
	findings := []*ProviderEgressTlsFinding{}
	if limit <= 0 {
		return findings
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT security.client_id, security.url_key,
				COALESCE(security.destination->>'name', ''), COALESCE(security.destination->>'url', ''),
				security.measured_at, security.tls_verdict, security.tls_verdict_measured_at,
				client.network_id, COALESCE(country.country_code, ''), country.location_id
			FROM provider_egress_url_security AS security
			LEFT JOIN network_client AS client ON client.client_id = security.client_id
			LEFT JOIN network_client_location_reliability AS provider_location ON provider_location.client_id = security.client_id
			LEFT JOIN location AS country ON country.location_id = provider_location.country_location_id
			WHERE security.tls_failure
			ORDER BY security.measured_at DESC, security.client_id, security.url_key
			LIMIT $1`, limit)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				finding := &ProviderEgressTlsFinding{}
				var verdict int16
				var verdictMeasuredAt *time.Time
				var networkId, countryLocationId *server.Id
				server.Raise(rows.Scan(&finding.ClientId, &finding.UrlKey, &finding.Name, &finding.Url,
					&finding.MeasuredAt, &verdict, &verdictMeasuredAt, &networkId, &finding.CountryCode, &countryLocationId))
				finding.Verdict = ProviderEgressTlsVerdict(verdict)
				finding.VerdictMeasuredAt = verdictMeasuredAt
				finding.NetworkId = networkId
				finding.CountryLocationId = countryLocationId
				finding.CountryCode = strings.ToLower(strings.TrimSpace(finding.CountryCode))
				findings = append(findings, finding)
			}
		})
	})
	return findings
}

// The most destination names one run's pool clock tracks. The names are the
// catalog's, about a hundred and fifty; past the bound, names in order start
// no clock, which delays their retirement and never hastens it.
const providerEgressTlsUnservedNameLimit = 1024

// The served pool as one judge run reads it.
type ProviderEgressTlsPool struct {
	// the destination names the pool serves now, general and every country's
	Served map[string]bool
	// for each name of a finding not yet retired that is out of the pool, the
	// first of the consecutive runs that read it out
	UnservedSince map[string]time.Time
}

// This run's pool from the names served now and the clock the previous run
// carried: a finding's name that is out of the pool keeps the time it was
// first read out, or starts its clock now. A name served again drops, and so
// does one whose findings are all retired, which stay retired while it is out
// anyway. A time ahead of now, another host's clock, starts again.
func NewProviderEgressTlsPool(
	now time.Time,
	findings []*ProviderEgressTlsFinding,
	served map[string]bool,
	previous map[string]time.Time,
) *ProviderEgressTlsPool {
	nameSet := map[string]bool{}
	for _, finding := range findings {
		if !served[finding.Name] && finding.Verdict != ProviderEgressTlsVerdictRetired {
			nameSet[finding.Name] = true
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	if providerEgressTlsUnservedNameLimit < len(names) {
		names = names[:providerEgressTlsUnservedNameLimit]
	}
	unservedSince := make(map[string]time.Time, len(names))
	for _, name := range names {
		if since, ok := previous[name]; ok && !since.After(now) {
			unservedSince[name] = since
		} else {
			unservedSince[name] = now
		}
	}
	return &ProviderEgressTlsPool{Served: served, UnservedSince: unservedSince}
}

// Where a finding's destination name stands against the pool on one run.
type providerEgressTlsServing int

const (
	// in the served pool
	providerEgressTlsServed providerEgressTlsServing = iota
	// out of the pool, on every run for less than RetireAfter: judged as if
	// served, so a transient absence changes nothing
	providerEgressTlsLeaving
	// out of the pool on every run for at least RetireAfter
	providerEgressTlsGone
)

// A name is gone only once an earlier run read it out too, whatever
// RetireAfter is: one run's read never retires anything.
func (self *ProviderEgressTlsPool) serving(now time.Time, name string, settings *ProviderEgressTlsJudgeSettings) providerEgressTlsServing {
	if self.Served[name] {
		return providerEgressTlsServed
	}
	if since, ok := self.UnservedSince[name]; ok && since.Before(now) && settings.RetireAfter <= now.Sub(since) {
		return providerEgressTlsGone
	}
	return providerEgressTlsLeaving
}

// Whether a run judges the finding on evidence: its latest failure is not
// decided yet, and it is neither to be retired nor retired and still out of
// the pool.
func providerEgressTlsJudgesOnEvidence(finding *ProviderEgressTlsFinding, serving providerEgressTlsServing) bool {
	switch {
	case serving == providerEgressTlsGone:
		return false
	case finding.Verdict == ProviderEgressTlsVerdictRetired && serving != providerEgressTlsServed:
		return false
	default:
		return !finding.Judged()
	}
}

// The hosts this run's direct checks are for: those of the findings it judges
// on evidence whose failure is recent enough for a check now to be close to
// it, each once.
func ProviderEgressTlsDirectHosts(
	now time.Time,
	findings []*ProviderEgressTlsFinding,
	pool *ProviderEgressTlsPool,
	settings *ProviderEgressTlsJudgeSettings,
) []string {
	hostSet := map[string]bool{}
	for _, finding := range findings {
		if !settings.DirectCheckJudges(now, finding) ||
			!providerEgressTlsJudgesOnEvidence(finding, pool.serving(now, finding.Name, settings)) {
			continue
		}
		if host := finding.Host(); host != "" {
			hostSet[host] = true
		}
	}
	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// One finding's place evidence over the place window. None of it counts the
// finding's own network.
type providerEgressTlsPlaceEvidence struct {
	// other networks in the country holding a TLS finding on the URL
	networks int
	// the other networks' connected, validly located providers in the
	// country whose latest TLS on the URL failed verification, and whose was
	// verified
	failed   int
	verified int
	// the country's scored loads, and failures, on every site but this one
	countryLoads    int
	countryFailures int
}

// The place rule: the URL's certificate fails from the place across other
// networks, nearly always, while the place's exits work.
func (self providerEgressTlsPlaceEvidence) broken(settings *ProviderEgressTlsJudgeSettings) bool {
	return settings.PlaceMinNetworks <= self.networks &&
		0 < self.failed &&
		settings.PlaceFailShare*float64(self.failed+self.verified) <= float64(self.failed) &&
		settings.PlaceMinSamples <= self.countryLoads &&
		float64(self.countryFailures) < settings.PlaceWorkingShare*float64(self.countryLoads)
}

// One URL at one country.
type providerEgressTlsPlaceKey struct {
	urlKey            string
	countryLocationId server.Id
}

// TLS verification outcomes on one URL by one network's providers.
type providerEgressTlsPlaceOutcome struct {
	failed   int
	verified int
}

// One country's scored loads over the place window, in all and by site.
type providerEgressTlsCountryTally struct {
	loads    int
	failures int
	sites    map[string]providerEgressTlsCountryTally
}

// The country's scored loads and failures on every site but name, so that a
// blocked site's own failures never read as the country's exits failing.
func (self providerEgressTlsCountryTally) without(name string) (loads int, failures int) {
	site := self.sites[name]
	return self.loads - site.loads, self.failures - site.failures
}

// The place evidence of each finding. Only a finding judged on evidence,
// with a known country and recent enough for the place window, whose URL and
// country hold findings on at least PlaceMinNetworks other networks, can
// meet the rule; it alone gets the outcomes and the tally, read once per URL
// and country and once per country for the whole run. The others get their
// network count and nothing else.
func getProviderEgressTlsPlaceEvidence(
	ctx context.Context,
	now time.Time,
	settings *ProviderEgressTlsJudgeSettings,
	findings []*ProviderEgressTlsFinding,
	judged []bool,
) []providerEgressTlsPlaceEvidence {
	evidence := make([]providerEgressTlsPlaceEvidence, len(findings))
	inWindow := func(finding *ProviderEgressTlsFinding) bool {
		return finding.CountryLocationId != nil && now.Sub(finding.MeasuredAt) <= settings.PlaceWindow
	}
	networks := map[providerEgressTlsPlaceKey]map[string]bool{}
	for _, finding := range findings {
		if !inWindow(finding) {
			continue
		}
		key := providerEgressTlsPlaceKey{urlKey: finding.UrlKey, countryLocationId: *finding.CountryLocationId}
		if networks[key] == nil {
			networks[key] = map[string]bool{}
		}
		networks[key][finding.network()] = true
	}
	needed := []int{}
	keySet := map[providerEgressTlsPlaceKey]bool{}
	countrySet := map[string]bool{}
	for i, finding := range findings {
		if !judged[i] || finding.CountryCode == "" || !inWindow(finding) {
			continue
		}
		key := providerEgressTlsPlaceKey{urlKey: finding.UrlKey, countryLocationId: *finding.CountryLocationId}
		others := len(networks[key])
		if networks[key][finding.network()] {
			others -= 1
		}
		evidence[i].networks = others
		if others < settings.PlaceMinNetworks {
			continue
		}
		needed = append(needed, i)
		keySet[key] = true
		countrySet[finding.CountryCode] = true
	}
	if len(needed) == 0 {
		return evidence
	}
	keys := make([]providerEgressTlsPlaceKey, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	countryCodes := make([]string, 0, len(countrySet))
	for countryCode := range countrySet {
		countryCodes = append(countryCodes, countryCode)
	}
	outcomes := getProviderEgressTlsPlaceOutcomes(ctx, now.Add(-settings.PlaceWindow), keys)
	tallies := getProviderEgressTlsCountryTallies(ctx, now, settings, countryCodes)
	for _, i := range needed {
		finding := findings[i]
		key := providerEgressTlsPlaceKey{urlKey: finding.UrlKey, countryLocationId: *finding.CountryLocationId}
		for networkId, outcome := range outcomes[key] {
			if finding.NetworkId != nil && networkId == *finding.NetworkId {
				continue
			}
			evidence[i].failed += outcome.failed
			evidence[i].verified += outcome.verified
		}
		evidence[i].countryLoads, evidence[i].countryFailures = tallies[finding.CountryCode].without(finding.Name)
	}
	return evidence
}

// The TLS verification outcomes on each URL since since, by network, of the
// connected providers with a valid location in its country: each provider's
// latest outcome on the URL, failed or verified. A reset, a timeout or a
// block page records no outcome. The country's providers come from the
// location index, and each one's outcome is one primary-key lookup, so a run
// reads only the URLs and countries that can meet the rule.
func getProviderEgressTlsPlaceOutcomes(
	ctx context.Context,
	since time.Time,
	keys []providerEgressTlsPlaceKey,
) map[providerEgressTlsPlaceKey]map[server.Id]providerEgressTlsPlaceOutcome {
	outcomes := map[providerEgressTlsPlaceKey]map[server.Id]providerEgressTlsPlaceOutcome{}
	if len(keys) == 0 {
		return outcomes
	}
	urlKeys := make([]string, 0, len(keys))
	countryLocationIds := make([]server.Id, 0, len(keys))
	for _, key := range keys {
		urlKeys = append(urlKeys, key.urlKey)
		countryLocationIds = append(countryLocationIds, key.countryLocationId)
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT place.url_key, place.country_location_id, client.network_id,
				COUNT(*) FILTER (WHERE outcome.tls_failure)::bigint,
				COUNT(*) FILTER (WHERE NOT outcome.tls_failure)::bigint
			FROM unnest($1::varchar[], $2::uuid[]) AS place(url_key, country_location_id)
			JOIN network_client_location_reliability AS provider_location
				ON provider_location.connected AND provider_location.valid
				AND provider_location.country_location_id = place.country_location_id
			CROSS JOIN LATERAL (
				SELECT security.tls_failure FROM provider_egress_url_security AS security
				WHERE security.client_id = provider_location.client_id AND security.url_key = place.url_key
				AND $3 <= security.measured_at
			) AS outcome
			JOIN network_client AS client ON client.client_id = provider_location.client_id
			GROUP BY place.url_key, place.country_location_id, client.network_id`,
			urlKeys, countryLocationIds, since.UTC())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var key providerEgressTlsPlaceKey
				var networkId server.Id
				var failed, verified int64
				server.Raise(rows.Scan(&key.urlKey, &key.countryLocationId, &networkId, &failed, &verified))
				if outcomes[key] == nil {
					outcomes[key] = map[server.Id]providerEgressTlsPlaceOutcome{}
				}
				outcomes[key][networkId] = providerEgressTlsPlaceOutcome{failed: int(failed), verified: int(verified)}
			}
		})
	})
	return outcomes
}

// Each country's scored loads and failures over the place window, in all and
// by site, from the site tally. One read of the tally's day range serves the
// whole run.
func getProviderEgressTlsCountryTallies(
	ctx context.Context,
	now time.Time,
	settings *ProviderEgressTlsJudgeSettings,
	countryCodes []string,
) map[string]providerEgressTlsCountryTally {
	tallies := map[string]providerEgressTlsCountryTally{}
	if len(countryCodes) == 0 {
		return tallies
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT country_code, name, SUM(load_count)::bigint, SUM(failure_count)::bigint
			FROM provider_egress_site_tally
			WHERE $1 <= tally_day AND country_code = ANY($2)
			GROUP BY country_code, name`,
			ProviderEgressSiteTallyWindowStart(now, settings.PlaceWindow), countryCodes)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var countryCode, name string
				var loads, failures int64
				server.Raise(rows.Scan(&countryCode, &name, &loads, &failures))
				country, ok := tallies[countryCode]
				if !ok {
					country = providerEgressTlsCountryTally{sites: map[string]providerEgressTlsCountryTally{}}
				}
				country.loads += int(loads)
				country.failures += int(failures)
				site := country.sites[name]
				site.loads += int(loads)
				site.failures += int(failures)
				country.sites[name] = site
				tallies[countryCode] = country
			}
		})
	})
	return tallies
}

// One finding's judgement and the evidence behind it.
type ProviderEgressTlsJudgement struct {
	Finding *ProviderEgressTlsFinding
	Verdict ProviderEgressTlsVerdict
	// Decided says the verdict decided the finding's latest failure, on
	// evidence close to it, so the failure is not judged again. A pending
	// verdict, a retirement, or a finding kept as it was, decided nothing.
	Decided bool
	// distinct networks failing TLS on the URL within the direct window
	CommonModeNetworks int
	Direct             ProviderEgressTlsDirectOutcome
	// The place evidence, never counting the finding's own network: other
	// networks in its country with a TLS finding on the URL; their connected
	// providers' failed and verified TLS outcomes on it; and the country's
	// scored loads and failures on every other site.
	PlaceNetworks   int
	PlaceFailed     int
	PlaceVerified   int
	CountryLoads    int
	CountryFailures int
}

// Whether this run changes what is stored for the finding: its verdict, or
// which failure the verdict decided.
func (self *ProviderEgressTlsJudgement) Changed() bool {
	return self.Verdict != self.Finding.Verdict || self.Decided && !self.Finding.Judged()
}

// Decides one finding: its verdict, and whether that verdict decided its
// latest failure. serving says where its destination name stands against the
// served pool; commonModeNetworks counts the networks failing TLS on its URL
// within the direct window; direct is this run's direct check of its host.
func judgeProviderEgressTlsFinding(
	now time.Time,
	finding *ProviderEgressTlsFinding,
	serving providerEgressTlsServing,
	commonModeNetworks int,
	direct ProviderEgressTlsDirectOutcome,
	place providerEgressTlsPlaceEvidence,
	settings *ProviderEgressTlsJudgeSettings,
) (verdict ProviderEgressTlsVerdict, decided bool) {
	if !providerEgressTlsJudgesOnEvidence(finding, serving) {
		switch {
		case serving == providerEgressTlsGone:
			// out of the pool on every run for a refresh interval
			return ProviderEgressTlsVerdictRetired, false
		case finding.Verdict == ProviderEgressTlsVerdictRetired:
			// still out of the pool, on a clock that started again
			return ProviderEgressTlsVerdictRetired, false
		default:
			// this failure was decided on evidence close to it
			return finding.Verdict, false
		}
	}
	age := now.Sub(finding.MeasuredAt)
	close := age <= settings.DirectWindow
	if close && (settings.CommonModeNetworkCount <= commonModeNetworks || direct == ProviderEgressTlsDirectFailed) {
		return ProviderEgressTlsVerdictSite, true
	}
	if age <= settings.PlaceWindow && finding.CountryCode != "" && place.broken(settings) {
		return ProviderEgressTlsVerdictPlace, true
	}
	if close && direct == ProviderEgressTlsDirectVerified {
		return ProviderEgressTlsVerdictProvider, true
	}
	// Nothing conclusive: an inconclusive direct check, or evidence taken now
	// that is not close to a failure this old. What is stored stands until
	// the failure can be judged: a new finding stays pending, and a verdict
	// decided on an earlier failure holds, so the server's own network
	// trouble never lifts a quarantine. A retired finding whose destination
	// is served again is pending: rechecked again, so a provider that still
	// intercepts fails a recheck and is judged on that fresh failure.
	if finding.Verdict == ProviderEgressTlsVerdictRetired {
		return ProviderEgressTlsVerdictPending, false
	}
	return finding.Verdict, false
}

// Distinct networks per URL whose findings are failures within the direct
// window that the judge has not attributed to their provider: pending ones,
// which are new, and site ones. A provider verdict's own rechecks refresh its
// finding, so counting them would let a group of networks with judged
// findings rechecking one URL vote themselves common-mode.
func providerEgressTlsCommonModeNetworks(
	now time.Time,
	findings []*ProviderEgressTlsFinding,
	settings *ProviderEgressTlsJudgeSettings,
) map[string]int {
	networks := map[string]map[string]bool{}
	for _, finding := range findings {
		if finding.Verdict != ProviderEgressTlsVerdictPending && finding.Verdict != ProviderEgressTlsVerdictSite {
			continue
		}
		if !settings.DirectCheckJudges(now, finding) {
			continue
		}
		urlNetworks, ok := networks[finding.UrlKey]
		if !ok {
			urlNetworks = map[string]bool{}
			networks[finding.UrlKey] = urlNetworks
		}
		urlNetworks[finding.network()] = true
	}
	counts := map[string]int{}
	for urlKey, urlNetworks := range networks {
		counts[urlKey] = len(urlNetworks)
	}
	return counts
}

// Judges every finding of one run. pool is the run's served pool
// (NewProviderEgressTlsPool); direct holds this run's direct check of each
// host (absent is inconclusive). Reads the place evidence the run needs once
// (getProviderEgressTlsPlaceEvidence); writes nothing.
func JudgeProviderEgressTlsFindings(
	ctx context.Context,
	now time.Time,
	findings []*ProviderEgressTlsFinding,
	pool *ProviderEgressTlsPool,
	direct map[string]ProviderEgressTlsDirectOutcome,
	settings *ProviderEgressTlsJudgeSettings,
) []*ProviderEgressTlsJudgement {
	commonMode := providerEgressTlsCommonModeNetworks(now, findings, settings)
	serving := make([]providerEgressTlsServing, len(findings))
	judged := make([]bool, len(findings))
	for i, finding := range findings {
		serving[i] = pool.serving(now, finding.Name, settings)
		judged[i] = providerEgressTlsJudgesOnEvidence(finding, serving[i])
	}
	placeEvidence := getProviderEgressTlsPlaceEvidence(ctx, now, settings, findings, judged)

	judgements := make([]*ProviderEgressTlsJudgement, 0, len(findings))
	for i, finding := range findings {
		place := placeEvidence[i]
		outcome := direct[finding.Host()]
		judgement := &ProviderEgressTlsJudgement{
			Finding:            finding,
			CommonModeNetworks: commonMode[finding.UrlKey],
			Direct:             outcome,
			PlaceNetworks:      place.networks,
			PlaceFailed:        place.failed,
			PlaceVerified:      place.verified,
			CountryLoads:       place.countryLoads,
			CountryFailures:    place.countryFailures,
		}
		judgement.Verdict, judgement.Decided = judgeProviderEgressTlsFinding(
			now, finding, serving[i], commonMode[finding.UrlKey], outcome, place, settings)
		judgements = append(judgements, judgement)
	}
	return judgements
}

// Stores every changed verdict and recomputes the TLS projection of each
// provider whose findings changed. A finding is written only while it is the
// row the judge read: a newer receipt that refreshed, re-failed or cleared it
// in the meantime wins, and the next run judges that. Returns the judgements
// stored.
func SetProviderEgressTlsVerdicts(ctx context.Context, judgements []*ProviderEgressTlsJudgement) []*ProviderEgressTlsJudgement {
	changed := []*ProviderEgressTlsJudgement{}
	for _, judgement := range judgements {
		if judgement.Changed() {
			changed = append(changed, judgement)
		}
	}
	if len(changed) == 0 {
		return []*ProviderEgressTlsJudgement{}
	}
	type findingKey struct {
		clientId server.Id
		urlKey   string
	}
	byKey := map[findingKey]*ProviderEgressTlsJudgement{}
	clientIds := make([]server.Id, 0, len(changed))
	urlKeys := make([]string, 0, len(changed))
	measuredAts := make([]time.Time, 0, len(changed))
	previous := make([]int16, 0, len(changed))
	previousDecided := make([]*time.Time, 0, len(changed))
	verdicts := make([]int16, 0, len(changed))
	decided := make([]bool, 0, len(changed))
	for _, judgement := range changed {
		finding := judgement.Finding
		byKey[findingKey{clientId: finding.ClientId, urlKey: finding.UrlKey}] = judgement
		clientIds = append(clientIds, finding.ClientId)
		urlKeys = append(urlKeys, finding.UrlKey)
		measuredAts = append(measuredAts, finding.MeasuredAt.UTC())
		previous = append(previous, int16(finding.Verdict))
		var previousDecidedAt *time.Time
		if finding.VerdictMeasuredAt != nil {
			at := finding.VerdictMeasuredAt.UTC()
			previousDecidedAt = &at
		}
		previousDecided = append(previousDecided, previousDecidedAt)
		verdicts = append(verdicts, int16(judgement.Verdict))
		decided = append(decided, judgement.Decided)
	}
	stored := []*ProviderEgressTlsJudgement{}
	server.Tx(ctx, func(tx server.PgTx) {
		stored = []*ProviderEgressTlsJudgement{}
		storedClientIds := map[server.Id]bool{}
		// A verdict that decided nothing keeps the failure an earlier verdict
		// decided, if any, so a stale finding stays undecided rather than
		// borrowing that decision; a pending one, a retired finding served
		// again, records none, as a new finding does. Bounded statements keep
		// each join a short key lookup even when a fleet-wide fault leaves many
		// findings.
		const chunk = 1000
		for start := 0; start < len(clientIds); start += chunk {
			end := min(start+chunk, len(clientIds))
			rows, err := tx.Query(ctx, `
				UPDATE provider_egress_url_security AS security SET
					tls_verdict = judged.verdict,
					tls_verdict_measured_at = CASE
						WHEN judged.decided THEN security.measured_at
						WHEN judged.verdict = `+fmt.Sprint(int16(ProviderEgressTlsVerdictPending))+` THEN NULL
						ELSE security.tls_verdict_measured_at END
				FROM unnest($1::uuid[], $2::varchar[], $3::timestamp[], $4::smallint[], $5::timestamp[], $6::smallint[], $7::boolean[])
					AS judged(client_id, url_key, measured_at, previous, previous_decided_at, verdict, decided)
				WHERE security.client_id = judged.client_id AND security.url_key = judged.url_key
				AND security.tls_failure AND security.measured_at = judged.measured_at
				AND security.tls_verdict = judged.previous
				AND security.tls_verdict_measured_at IS NOT DISTINCT FROM judged.previous_decided_at
				RETURNING security.client_id, security.url_key`,
				clientIds[start:end], urlKeys[start:end], measuredAts[start:end], previous[start:end],
				previousDecided[start:end], verdicts[start:end], decided[start:end])
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var key findingKey
					server.Raise(rows.Scan(&key.clientId, &key.urlKey))
					if judgement, ok := byKey[key]; ok {
						stored = append(stored, judgement)
						storedClientIds[key.clientId] = true
					}
				}
			})
		}
		if len(storedClientIds) == 0 {
			return
		}
		ids := make([]server.Id, 0, len(storedClientIds))
		for clientId := range storedClientIds {
			ids = append(ids, clientId)
		}
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE provider_egress_health AS health SET
				tls_authentication_failure = `+providerLegacyTlsUnclearedSql("health")+` OR EXISTS (
					SELECT 1 FROM provider_egress_url_security AS security
					WHERE security.client_id = health.client_id AND `+providerUrlSecurityExcludesSql("security")+`
				)
			WHERE health.client_id = ANY($1)`, ids))
	}, server.TxReadCommitted)
	return stored
}
