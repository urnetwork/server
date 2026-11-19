package model

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Country sites graduate into scoring, and leave it, on evidence alone (owner,
// 2026-11-08: "the graduation needs to be automatic not manual, since we will
// not maintain manual lists").
//
// A country site starts on probation: its outcomes are recorded and counted in
// the rolling quota, never scored (provider_egress_url_source.go). Each of its
// outcomes is paired, at ingest, with the same provider's own general outcomes
// of the eight hours before it (the pair tally), so the comparison is with the
// same exit at the same time. Once a day the pool refresh judges every listed
// country site on that tally and stores its state here; the ingest reads the
// state for each outcome. Scored country outcomes count where scored outcomes
// count (Quality, Speed, the index, ranking), never in the Online rule or its
// country guard (ProviderEgressHealthCounts.General).
//
// The unit of the comparison is the network, not the provider or the load.
// Main's fleet is concentrated: on 2026-11-08 one network held 47% of the
// probed providers in the US and 72-90% in vn, fr, de, gb, es, it and th, and
// only 15 countries had 20 or more networks with a URL result in a day (36 had
// 10 or more). A load-weighted comparison would be one farm's verdict, so each
// network contributes one paired difference, and a site is judged by the mean
// over networks with a t bound, the networks being the independent samples.
// The load-weighted difference is kept beside it as a guard, so a site the
// biggest operators cannot use does not graduate either.
//
// Only networks that have existed for two weeks are evidence, for a site or
// against it (ProviderEgressCountrySiteMinNetworkAge): their paired outcomes
// and their TLS findings. A farm can make the ten networks a judgement needs
// on demand, but not ones two weeks old.

// The judged window: a week, so weekday and weekend exits both count, and a
// new site (or one demoted) is on probation at least that long.
const ProviderEgressCountrySiteWindow = 7 * 24 * time.Hour

// The youngest a network may be, at the judgement, for its paired outcomes and
// its TLS findings to count for or against a country site: the floor place
// evidence has (providerEgressTlsPlaceMinNetworkAge, the owner's two weeks),
// for the same reason. A farm can create networks whenever it likes, but not
// ones that have existed for two weeks, so it cannot stage the networks a
// graduation needs, tip the load-weighted guard or a collapse with its loads,
// or demote a site with TLS failures common to its own networks. A network
// with no network row has no age, so it does not count either.
const ProviderEgressCountrySiteMinNetworkAge = providerEgressTlsPlaceMinNetworkAge

// The recent window a collapse is judged on: the day in progress and the day
// before, the tally's day granularity.
const ProviderEgressCountrySiteRecentDays = 2

// The first tally day of the recent window at now.
func ProviderEgressCountrySiteRecentStart(now time.Time) time.Time {
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1-ProviderEgressCountrySiteRecentDays)
}

// The pair tally keeps the window and the day in progress.
const ProviderEgressCountrySiteTallyRetention = ProviderEgressCountrySiteWindow + 24*time.Hour

// A country outcome is paired when its provider had at least this many scored
// general outcomes in the eight hours before it: the baseline is that
// provider's own general success share then. A steady provider has about ten.
const ProviderEgressCountrySitePairMinGeneral = 3

const (
	// A site graduates when, per network, it is no worse than the general
	// catalog by more than this share of loads, at one-sided 95%
	// confidence, and the load-weighted difference is no worse either. Each
	// of a country list's four sites carries one eighth of a provider's
	// normal turns, so a site at the margin moves a provider's scored ratio
	// by under half a point, and all four at it by a point and a half.
	ProviderEgressCountrySiteGraduateMargin = 0.03
	// A scored site is demoted when it is clearly worse: per network, worse
	// than this at one-sided 95% confidence. Twice the graduation margin, so
	// a site near either line does not flap between them.
	ProviderEgressCountrySiteDemoteMargin = 0.06
)

const (
	// A network counts toward a judgement once it has this many paired loads
	// on the site in the window: a steady provider makes about 45 a week on
	// each site of a four-site list (52 turns a day, half of them country
	// turns), so a network counts with two steady providers, or one online
	// all week. Fewer would put more noise than signal in its difference.
	ProviderEgressCountrySiteMinNetworkLoads = 50
	// At least this many such networks: below ten the t bound is too wide to
	// pass a three-point margin, and on Main 36 countries reach ten networks.
	ProviderEgressCountrySiteMinNetworks = 10
	// At least this many distinct providers in them.
	ProviderEgressCountrySiteMinProviders = 20
	// At least this many paired loads in them, the networks' minimum.
	ProviderEgressCountrySiteMinLoads = ProviderEgressCountrySiteMinNetworks * ProviderEgressCountrySiteMinNetworkLoads
)

const (
	// A scored site collapses when, over the recent days, it succeeds for
	// fewer than half of the loads the same providers' general outcomes
	// predict: load-weighted (most providers) or in at least half of the
	// networks measured (most operators). Blocked, broken and changed sites
	// fall far below this; sampling noise does not come near it.
	ProviderEgressCountrySiteCollapseRatio = 0.5
	// The collapse needs at least this many recent paired loads.
	ProviderEgressCountrySiteCollapseMinLoads = 50
	// A network is measured recently with this many paired loads, and at
	// least this many networks must fail for a collapse by networks.
	ProviderEgressCountrySiteCollapseNetworkLoads = 10
	ProviderEgressCountrySiteCollapseMinNetworks  = 3
)

// One listed country site.
type ProviderEgressCountrySiteKey struct {
	Name        string
	CountryCode string
}

func (self ProviderEgressCountrySiteKey) String() string {
	return self.CountryCode + "/" + self.Name
}

// A country site's state (provider_egress_country_site), which the refresh
// writes and the ingest reads.
type ProviderEgressCountrySite struct {
	ProviderEgressCountrySiteKey
	// the listed URL the state was judged for; a changed URL starts over
	Url string
	// graduated: its outcomes are scored
	Scored bool
	// when it last went on probation: first listed, its URL changed, or
	// demoted. Only evidence from then on can graduate it.
	ProbationSince time.Time
	GraduatedTime  *time.Time
	DemotedTime    *time.Time
	DemoteCount    int
	DemoteReason   string
	// the last judgement
	JudgedTime       *time.Time
	Judgement        string
	NetworkCount     int
	ProviderCount    int
	PairedLoadCount  int
	PairedDifference *float64
	LowerBound       *float64
	UpperBound       *float64
	UpdateTime       time.Time
}

// The state's label for the gauges: scored, demoted (on probation again after
// a demotion) or probation.
func (self *ProviderEgressCountrySite) State() string {
	switch {
	case self.Scored:
		return "scored"
	case 0 < self.DemoteCount:
		return "demoted"
	default:
		return "probation"
	}
}

// Every state label.
var ProviderEgressCountrySiteStates = []string{"probation", "scored", "demoted"}

const providerEgressCountrySiteColumns = `name, country_code, url, scored, probation_since, graduated_time,
	demoted_time, demote_count, demote_reason, judged_time, judgement, network_count, provider_count,
	paired_load_count, paired_difference, lower_bound, upper_bound, update_time`

// Every stored state.
func GetProviderEgressCountrySites(ctx context.Context) map[ProviderEgressCountrySiteKey]*ProviderEgressCountrySite {
	states := map[ProviderEgressCountrySiteKey]*ProviderEgressCountrySite{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT `+providerEgressCountrySiteColumns+` FROM provider_egress_country_site`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				state := &ProviderEgressCountrySite{}
				var difference, lower, upper *float32
				server.Raise(result.Scan(
					&state.Name, &state.CountryCode, &state.Url, &state.Scored, &state.ProbationSince,
					&state.GraduatedTime, &state.DemotedTime, &state.DemoteCount, &state.DemoteReason,
					&state.JudgedTime, &state.Judgement, &state.NetworkCount, &state.ProviderCount,
					&state.PairedLoadCount, &difference, &lower, &upper, &state.UpdateTime,
				))
				state.PairedDifference, state.LowerBound, state.UpperBound = providerEgressFloat64(difference), providerEgressFloat64(lower), providerEgressFloat64(upper)
				states[state.ProviderEgressCountrySiteKey] = state
			}
		})
	})
	return states
}

func providerEgressFloat64(value *float32) *float64 {
	if value == nil {
		return nil
	}
	f := float64(*value)
	return &f
}

func providerEgressFloat32(value *float64) *float32 {
	if value == nil {
		return nil
	}
	f := float32(*value)
	return &f
}

// Writes the given states, each whole, and removes the states of the sites
// that are no longer listed, in one transaction: the refresh is the only
// writer, and runs once at a time.
func SetProviderEgressCountrySites(ctx context.Context, states []*ProviderEgressCountrySite, listed []ProviderEgressCountrySiteKey) {
	server.Tx(ctx, func(tx server.PgTx) {
		names := make([]string, 0, len(listed))
		countryCodes := make([]string, 0, len(listed))
		for _, key := range listed {
			names = append(names, key.Name)
			countryCodes = append(countryCodes, key.CountryCode)
		}
		server.RaisePgResult(tx.Exec(ctx, `
			DELETE FROM provider_egress_country_site AS site
			WHERE NOT EXISTS (
				SELECT 1 FROM unnest($1::varchar[], $2::varchar[]) AS listed(name, country_code)
				WHERE listed.name = site.name AND listed.country_code = site.country_code
			)`, names, countryCodes))
		now := server.NowUtc()
		utc := func(t *time.Time) *time.Time {
			if t == nil {
				return nil
			}
			u := t.UTC()
			return &u
		}
		for _, state := range states {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO provider_egress_country_site (`+providerEgressCountrySiteColumns+`)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
				ON CONFLICT (name, country_code) DO UPDATE SET
					url = EXCLUDED.url,
					scored = EXCLUDED.scored,
					probation_since = EXCLUDED.probation_since,
					graduated_time = EXCLUDED.graduated_time,
					demoted_time = EXCLUDED.demoted_time,
					demote_count = EXCLUDED.demote_count,
					demote_reason = EXCLUDED.demote_reason,
					judged_time = EXCLUDED.judged_time,
					judgement = EXCLUDED.judgement,
					network_count = EXCLUDED.network_count,
					provider_count = EXCLUDED.provider_count,
					paired_load_count = EXCLUDED.paired_load_count,
					paired_difference = EXCLUDED.paired_difference,
					lower_bound = EXCLUDED.lower_bound,
					upper_bound = EXCLUDED.upper_bound,
					update_time = EXCLUDED.update_time`,
				state.Name, state.CountryCode, state.Url, state.Scored, state.ProbationSince.UTC(),
				utc(state.GraduatedTime), utc(state.DemotedTime), state.DemoteCount, providerEgressTruncate(state.DemoteReason, 256),
				utc(state.JudgedTime), providerEgressTruncate(state.Judgement, 512), state.NetworkCount, state.ProviderCount,
				state.PairedLoadCount, providerEgressFloat32(state.PairedDifference), providerEgressFloat32(state.LowerBound),
				providerEgressFloat32(state.UpperBound), now,
			))
		}
	})
}

func providerEgressTruncate(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}

// Whether the listed country site scores: it graduated. A site without a
// state is on probation.
func providerEgressCountrySiteScoredInTx(ctx context.Context, tx server.PgTx, name string, countryCode string) bool {
	var scored bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM provider_egress_country_site WHERE name = $1 AND country_code = $2 AND scored
	)`, name, countryCode).Scan(&scored))
	return scored
}

// Adds one measured country outcome to the pair tally, beside the same
// provider's scored general outcomes of the eight hours before it, read
// through the provider's own index on the history. An outcome whose provider
// had too few of them is tallied unpaired. The day's row carries the
// provider's network as it was when the row was first written, so the refresh
// sums the tally by network without looking each provider up again; a
// provider with no network is no evidence, and is not tallied.
//
// The pair judges the site, so ok is the outcome's IPv4 HTTPS leg and the
// general side counts IPv4 HTTPS legs too: a stored HTTP/3 or IPv6 leg follows
// only a passed IPv4 HTTPS leg (egresshealth.UrlProbeEvidence.HttpsOk), and a
// URL that failed only a later leg says the provider does not carry UDP or
// IPv6, not how the site does.
func addProviderEgressCountrySiteTallyInTx(ctx context.Context, tx server.PgTx, health *ProviderEgressHealth, ok int, policyVersion int) {
	measuredAt := health.MeasuredAt.UTC()
	var generalTotal, generalOk int
	server.Raise(tx.QueryRow(ctx, `
		SELECT COUNT(*)::integer, COALESCE(SUM(CASE WHEN 0 < ok_count OR (url_probe_evidence -> 'h3') IS NOT NULL OR (url_probe_evidence -> 'ipv6') IS NOT NULL THEN 1 ELSE 0 END), 0)::integer
		FROM provider_egress_health_history
		WHERE client_id = $1 AND measured_at > $2::timestamp - interval '8 hours' AND measured_at < $2
		AND total_count = 1 AND url_probe_policy_version = $3 AND NOT url_probation AND url_source = $4`,
		health.ClientId, measuredAt, policyVersion, int16(ProviderEgressUrlSourceGeneral),
	).Scan(&generalTotal, &generalOk))
	pairedLoads, pairedOk, expectedOk := 0, 0, 0.0
	if ProviderEgressCountrySitePairMinGeneral <= generalTotal {
		pairedLoads, pairedOk, expectedOk = 1, ok, float64(generalOk)/float64(generalTotal)
	}
	tlsFailures := 0
	if health.TLSAuthenticationFailure {
		tlsFailures = 1
	}
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO provider_egress_country_site_tally AS tally (
			tally_day, name, country_code, client_id, network_id, load_count, ok_count,
			paired_load_count, paired_ok_count, expected_ok_sum, tls_failure_count, update_time
		)
		SELECT $1::date, $2::varchar, $3::varchar, client.client_id, client.network_id, 1, $5::integer,
			$6::integer, $7::integer, $8::double precision, $9::integer, $10::timestamp
		FROM network_client AS client
		WHERE client.client_id = $4
		ON CONFLICT (tally_day, name, country_code, client_id) DO UPDATE SET
			load_count = tally.load_count + 1,
			ok_count = tally.ok_count + EXCLUDED.ok_count,
			paired_load_count = tally.paired_load_count + EXCLUDED.paired_load_count,
			paired_ok_count = tally.paired_ok_count + EXCLUDED.paired_ok_count,
			expected_ok_sum = tally.expected_ok_sum + EXCLUDED.expected_ok_sum,
			tls_failure_count = tally.tls_failure_count + EXCLUDED.tls_failure_count,
			update_time = EXCLUDED.update_time`,
		measuredAt.Truncate(24*time.Hour), health.UrlProbeEvidence.Destination.Name, health.UrlCountryCode, health.ClientId,
		ok, pairedLoads, pairedOk, expectedOk, tlsFailures, server.NowUtc(),
	))
}

// One network's paired evidence on one country site: over the judged window,
// and over the recent days.
type ProviderEgressCountrySiteNetworkEvidence struct {
	NetworkId server.Id
	// paired loads, their successes, and the successes the same providers'
	// general outcomes predict
	PairedLoads int
	PairedOk    int
	ExpectedOk  float64
	// distinct providers with a paired load
	Providers         int
	RecentPairedLoads int
	RecentPairedOk    int
	RecentExpectedOk  float64
}

// Each listed site's paired evidence per network: from its window start (a
// day) and over the recent days from recentStart, leaving out the providers
// in excluded (those a TLS finding judged their own excludes, which fail every
// site alike). A provider's loads count for the network the tally stored with
// them. Only networks created by establishedBy are read: a younger network,
// or one with no network row, is no evidence
// (ProviderEgressCountrySiteMinNetworkAge).
func GetProviderEgressCountrySiteEvidence(
	ctx context.Context,
	windowStarts map[ProviderEgressCountrySiteKey]time.Time,
	recentStart time.Time,
	establishedBy time.Time,
	excluded []server.Id,
) map[ProviderEgressCountrySiteKey][]ProviderEgressCountrySiteNetworkEvidence {
	evidence := map[ProviderEgressCountrySiteKey][]ProviderEgressCountrySiteNetworkEvidence{}
	if len(windowStarts) == 0 {
		return evidence
	}
	names, countryCodes, starts := []string{}, []string{}, []time.Time{}
	for key, start := range windowStarts {
		names = append(names, key.Name)
		countryCodes = append(countryCodes, key.CountryCode)
		starts = append(starts, start.UTC().Truncate(24*time.Hour))
	}
	if excluded == nil {
		excluded = []server.Id{}
	}
	server.Db(ctx, func(conn server.PgConn) {
		// the tally is summed by the network each row carries, and the
		// network's age is read once per network after that, so the read looks
		// up no provider and only as many networks as measured the sites
		result, err := conn.Query(ctx, `
			WITH site AS (
				SELECT * FROM unnest($1::varchar[], $2::varchar[], $3::date[]) AS site(name, country_code, window_start)
			),
			evidence AS (
				SELECT tally.name, tally.country_code, tally.network_id,
					COALESCE(SUM(tally.paired_load_count) FILTER (WHERE site.window_start <= tally.tally_day), 0)::bigint AS paired_loads,
					COALESCE(SUM(tally.paired_ok_count) FILTER (WHERE site.window_start <= tally.tally_day), 0)::bigint AS paired_ok,
					COALESCE(SUM(tally.expected_ok_sum) FILTER (WHERE site.window_start <= tally.tally_day), 0) AS expected_ok,
					COUNT(DISTINCT tally.client_id) FILTER (WHERE site.window_start <= tally.tally_day AND 0 < tally.paired_load_count) AS providers,
					COALESCE(SUM(tally.paired_load_count) FILTER (WHERE $4 <= tally.tally_day), 0)::bigint AS recent_paired_loads,
					COALESCE(SUM(tally.paired_ok_count) FILTER (WHERE $4 <= tally.tally_day), 0)::bigint AS recent_paired_ok,
					COALESCE(SUM(tally.expected_ok_sum) FILTER (WHERE $4 <= tally.tally_day), 0) AS recent_expected_ok
				FROM site
				JOIN provider_egress_country_site_tally AS tally
					ON tally.name = site.name AND tally.country_code = site.country_code
					AND LEAST(site.window_start, $4::date) <= tally.tally_day
				WHERE NOT (tally.client_id = ANY($5::uuid[]))
				GROUP BY tally.name, tally.country_code, tally.network_id
			)
			SELECT evidence.name, evidence.country_code, evidence.network_id,
				evidence.paired_loads, evidence.paired_ok, evidence.expected_ok, evidence.providers,
				evidence.recent_paired_loads, evidence.recent_paired_ok, evidence.recent_expected_ok
			FROM evidence
			JOIN network ON network.network_id = evidence.network_id
			WHERE network.create_time <= $6`,
			names, countryCodes, starts, recentStart.UTC().Truncate(24*time.Hour), excluded, establishedBy.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var key ProviderEgressCountrySiteKey
				var network ProviderEgressCountrySiteNetworkEvidence
				var pairedLoads, pairedOk, providers, recentLoads, recentOk int64
				server.Raise(result.Scan(
					&key.Name, &key.CountryCode, &network.NetworkId,
					&pairedLoads, &pairedOk, &network.ExpectedOk, &providers,
					&recentLoads, &recentOk, &network.RecentExpectedOk,
				))
				network.PairedLoads, network.PairedOk, network.Providers = int(pairedLoads), int(pairedOk), int(providers)
				network.RecentPairedLoads, network.RecentPairedOk = int(recentLoads), int(recentOk)
				evidence[key] = append(evidence[key], network)
			}
		})
	})
	return evidence
}

// One unresolved TLS finding on a country site.
type ProviderEgressCountrySiteTlsFinding struct {
	Verdict    ProviderEgressTlsVerdict
	MeasuredAt time.Time
}

// The unresolved TLS findings measured from since on the named sites, any hop
// of them (a redirect hop's finding keeps its site's name), read through the
// partial index on unresolved findings, a few hundred rows. Only findings of
// providers in networks created by establishedBy are read, as for the paired
// evidence (ProviderEgressCountrySiteMinNetworkAge): the TLS judge calls a
// failure common to sixteen networks within an hour a site fault whatever
// their age, so without the floor a farm of young networks could demote a
// scored site, or hold one back, by failing it together.
func GetProviderEgressCountrySiteTlsFindings(ctx context.Context, names []string, since time.Time, establishedBy time.Time) map[string][]ProviderEgressCountrySiteTlsFinding {
	findings := map[string][]ProviderEgressCountrySiteTlsFinding{}
	if len(names) == 0 {
		return findings
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT security.destination->>'name', security.tls_verdict, security.measured_at
			FROM provider_egress_url_security AS security
			JOIN network_client AS client ON client.client_id = security.client_id
			JOIN network ON network.network_id = client.network_id
			WHERE security.tls_failure AND $2 <= security.measured_at
			AND security.destination->>'name' = ANY($1::text[])
			AND network.create_time <= $3`,
			names, since.UTC(), establishedBy.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				var verdict int16
				var finding ProviderEgressCountrySiteTlsFinding
				server.Raise(result.Scan(&name, &verdict, &finding.MeasuredAt))
				finding.Verdict = ProviderEgressTlsVerdict(verdict)
				findings[name] = append(findings[name], finding)
			}
		})
	})
	return findings
}

// Drops the pair tally's days before before.
func RemoveExpiredProviderEgressCountrySiteTallies(ctx context.Context, before time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_country_site_tally WHERE tally_day < $1`,
			before.UTC().Truncate(24*time.Hour)))
	})
}

// A site's paired evidence, summarized as the judgement reads it.
type ProviderEgressCountrySiteSummary struct {
	// the networks with at least ProviderEgressCountrySiteMinNetworkLoads
	// paired loads, their distinct providers and paired loads
	Networks  int
	Providers int
	Loads     int
	// the mean over those networks of each one's paired difference (its
	// successes less the successes its providers' general outcomes predict,
	// per load), and its one-sided 95% bounds; zero without two networks
	Difference float64
	Lower      float64
	Upper      float64
	// the same difference load-weighted, over every network
	LoadDifference float64
	AllLoads       int
}

// The one-sided 95% quantile of Student's t with df degrees of freedom: the
// Cornish-Fisher expansion about the normal quantile to the third order,
// within 0.001 of the tables from nine degrees of freedom, the fewest a
// judgement uses.
func ProviderEgressStudentT95(df int) float64 {
	const z = 1.6448536269514722
	d := float64(max(1, df))
	z3 := z * z * z
	z5 := z3 * z * z
	z7 := z5 * z * z
	return z + (z3+z)/(4*d) + (5*z5+16*z3+3*z)/(96*d*d) + (3*z7+19*z5+17*z3-15*z)/(384*d*d*d)
}

// Summarizes a site's evidence over its window.
func SummarizeProviderEgressCountrySite(evidence []ProviderEgressCountrySiteNetworkEvidence) ProviderEgressCountrySiteSummary {
	summary := ProviderEgressCountrySiteSummary{}
	differences := []float64{}
	allResidual := 0.0
	for _, network := range evidence {
		if 0 < network.PairedLoads {
			summary.AllLoads += network.PairedLoads
			allResidual += float64(network.PairedOk) - network.ExpectedOk
		}
		if network.PairedLoads < ProviderEgressCountrySiteMinNetworkLoads {
			continue
		}
		summary.Networks++
		summary.Providers += network.Providers
		summary.Loads += network.PairedLoads
		differences = append(differences, (float64(network.PairedOk)-network.ExpectedOk)/float64(network.PairedLoads))
	}
	if 0 < summary.AllLoads {
		summary.LoadDifference = allResidual / float64(summary.AllLoads)
	}
	if len(differences) < 2 {
		return summary
	}
	mean := 0.0
	for _, difference := range differences {
		mean += difference
	}
	mean /= float64(len(differences))
	variance := 0.0
	for _, difference := range differences {
		variance += (difference - mean) * (difference - mean)
	}
	variance /= float64(len(differences) - 1)
	bound := ProviderEgressStudentT95(len(differences)-1) * math.Sqrt(variance/float64(len(differences)))
	summary.Difference, summary.Lower, summary.Upper = mean, mean-bound, mean+bound
	return summary
}

// Whether the site collapsed over the recent days, and how: it succeeds for
// fewer than ProviderEgressCountrySiteCollapseRatio of the loads its
// providers' general outcomes predict, load-weighted, or in at least half of
// the networks measured recently.
func providerEgressCountrySiteCollapse(evidence []ProviderEgressCountrySiteNetworkEvidence) (bool, string) {
	loads, ok, expected := 0, 0, 0.0
	measured, failing := 0, 0
	networks := 0
	for _, network := range evidence {
		if network.RecentPairedLoads <= 0 {
			continue
		}
		networks++
		loads += network.RecentPairedLoads
		ok += network.RecentPairedOk
		expected += network.RecentExpectedOk
		if ProviderEgressCountrySiteCollapseNetworkLoads <= network.RecentPairedLoads && 0 < network.RecentExpectedOk {
			measured++
			if float64(network.RecentPairedOk) < ProviderEgressCountrySiteCollapseRatio*network.RecentExpectedOk {
				failing++
			}
		}
	}
	if ProviderEgressCountrySiteCollapseMinLoads <= loads && ProviderEgressCountrySiteCollapseMinNetworks <= networks &&
		0 < expected && float64(ok) < ProviderEgressCountrySiteCollapseRatio*expected {
		return true, fmt.Sprintf("collapsed: %d successes of %d recent loads where the same providers' general outcomes predict %.0f, from %d networks", ok, loads, expected, networks)
	}
	if ProviderEgressCountrySiteCollapseMinNetworks <= failing && measured <= 2*failing {
		return true, fmt.Sprintf("collapsed: failing in %d of the %d networks measured recently (under %.0f%% of the successes their general outcomes predict)", failing, measured, 100*ProviderEgressCountrySiteCollapseRatio)
	}
	return false, ""
}

// What one judgement decided.
type ProviderEgressCountrySiteChange string

const (
	ProviderEgressCountrySiteUnchanged ProviderEgressCountrySiteChange = ""
	ProviderEgressCountrySiteGraduated ProviderEgressCountrySiteChange = "graduated"
	ProviderEgressCountrySiteDemoted   ProviderEgressCountrySiteChange = "demoted"
)

// Why a site was demoted, as the change counter labels it.
const (
	ProviderEgressCountrySiteReasonPaired    = "paired"
	ProviderEgressCountrySiteReasonWorse     = "worse"
	ProviderEgressCountrySiteReasonCollapsed = "collapsed"
	ProviderEgressCountrySiteReasonTls       = "tls"
)

// One judgement: the state to store, what changed and why.
type ProviderEgressCountrySiteJudgement struct {
	State  *ProviderEgressCountrySite
	Change ProviderEgressCountrySiteChange
	Reason string
}

// Judges one listed site at now. state is its stored state, which the caller
// has started over for a site newly listed or whose URL changed; evidence is
// its paired evidence from its window start (the later of the window's and its
// probation's start while on probation); findings are its unresolved TLS
// findings of the window. Both come only from networks that have existed for
// ProviderEgressCountrySiteMinNetworkAge at now, as the reads return them
// (GetProviderEgressCountrySiteEvidence, GetProviderEgressCountrySiteTlsFindings),
// so every network counted below, and every load and finding, is one of those.
//
// On probation, it graduates when all of these hold:
//   - it has been on probation for the whole window;
//   - the window's evidence meets the minimums: ProviderEgressCountrySiteMinNetworks
//     networks each with ProviderEgressCountrySiteMinNetworkLoads paired loads,
//     ProviderEgressCountrySiteMinProviders providers and
//     ProviderEgressCountrySiteMinLoads paired loads;
//   - per network it is no worse than the general catalog for the same
//     providers by more than ProviderEgressCountrySiteGraduateMargin, at
//     one-sided 95% confidence, and the load-weighted difference is no worse
//     than that either;
//   - no TLS failure on it since its probation began is still pending, or
//     judged a site or place fault (TLS judge, provider_egress_tls_verdict.go).
//     A failure judged the provider's own is that provider's: the provider is
//     excluded from every bucket and left out of this evidence.
//
// Scored, it is demoted to probation when any of these holds:
//   - a TLS failure on it in the recent days is judged a site or place fault;
//   - it collapsed in the recent days (providerEgressCountrySiteCollapse);
//   - the window's evidence meets the minimums and, per network, it is worse
//     than ProviderEgressCountrySiteDemoteMargin at one-sided 95% confidence.
//
// A demoted site needs a whole window of fresh evidence to graduate again,
// and the gap between the margins keeps a site near either from flapping.
func JudgeProviderEgressCountrySite(
	state *ProviderEgressCountrySite,
	evidence []ProviderEgressCountrySiteNetworkEvidence,
	findings []ProviderEgressCountrySiteTlsFinding,
	now time.Time,
) ProviderEgressCountrySiteJudgement {
	judged := *state
	judgedTime := now
	judged.JudgedTime = &judgedTime
	summary := SummarizeProviderEgressCountrySite(evidence)
	judged.NetworkCount, judged.ProviderCount, judged.PairedLoadCount = summary.Networks, summary.Providers, summary.Loads
	judged.PairedDifference, judged.LowerBound, judged.UpperBound = nil, nil, nil
	if 2 <= summary.Networks {
		difference, lower, upper := summary.Difference, summary.Lower, summary.Upper
		judged.PairedDifference, judged.LowerBound, judged.UpperBound = &difference, &lower, &upper
	}
	minimums := ProviderEgressCountrySiteMinNetworks <= summary.Networks &&
		ProviderEgressCountrySiteMinProviders <= summary.Providers &&
		ProviderEgressCountrySiteMinLoads <= summary.Loads
	evidenceText := fmt.Sprintf(
		"%d networks, %d providers, %d paired loads; per network %+.3f [%+.3f, %+.3f], load-weighted %+.3f over %d loads",
		summary.Networks, summary.Providers, summary.Loads, summary.Difference, summary.Lower, summary.Upper, summary.LoadDifference, summary.AllLoads,
	)
	recentStart := ProviderEgressCountrySiteRecentStart(now)
	unchanged := func(judgement string) ProviderEgressCountrySiteJudgement {
		judged.Judgement = judgement
		return ProviderEgressCountrySiteJudgement{State: &judged}
	}

	if judged.Scored {
		demote := func(reason string, detail string) ProviderEgressCountrySiteJudgement {
			demotedTime := now
			judged.Scored = false
			judged.ProbationSince = now
			judged.DemotedTime = &demotedTime
			judged.DemoteCount++
			judged.DemoteReason = detail
			judged.Judgement = "demoted, " + detail + "; " + evidenceText
			return ProviderEgressCountrySiteJudgement{State: &judged, Change: ProviderEgressCountrySiteDemoted, Reason: reason}
		}
		for _, finding := range findings {
			if !finding.MeasuredAt.Before(recentStart) &&
				(finding.Verdict == ProviderEgressTlsVerdictSite || finding.Verdict == ProviderEgressTlsVerdictPlace) {
				return demote(ProviderEgressCountrySiteReasonTls, fmt.Sprintf("a TLS failure on it was judged a %s fault", finding.Verdict))
			}
		}
		if collapsed, detail := providerEgressCountrySiteCollapse(evidence); collapsed {
			return demote(ProviderEgressCountrySiteReasonCollapsed, detail)
		}
		if minimums && summary.Upper < -ProviderEgressCountrySiteDemoteMargin {
			return demote(ProviderEgressCountrySiteReasonWorse, fmt.Sprintf("clearly worse than the general catalog for the same providers (bound %+.3f under %+.3f)", summary.Upper, -ProviderEgressCountrySiteDemoteMargin))
		}
		return unchanged("scored; " + evidenceText)
	}

	if onProbation := now.Sub(judged.ProbationSince); onProbation < ProviderEgressCountrySiteWindow {
		return unchanged(fmt.Sprintf("on probation for %s of %s; %s", onProbation.Round(time.Hour), ProviderEgressCountrySiteWindow, evidenceText))
	}
	if !minimums {
		return unchanged(fmt.Sprintf("too few samples (needs %d networks at least %d days old with %d paired loads each, %d providers, %d loads); %s",
			ProviderEgressCountrySiteMinNetworks, int(ProviderEgressCountrySiteMinNetworkAge/(24*time.Hour)), ProviderEgressCountrySiteMinNetworkLoads,
			ProviderEgressCountrySiteMinProviders, ProviderEgressCountrySiteMinLoads, evidenceText))
	}
	for _, finding := range findings {
		if finding.MeasuredAt.Before(judged.ProbationSince) || finding.Verdict == ProviderEgressTlsVerdictProvider || finding.Verdict == ProviderEgressTlsVerdictRetired {
			continue
		}
		return unchanged(fmt.Sprintf("a TLS failure on it is %s; %s", finding.Verdict, evidenceText))
	}
	if summary.Lower < -ProviderEgressCountrySiteGraduateMargin || summary.LoadDifference < -ProviderEgressCountrySiteGraduateMargin {
		return unchanged("worse than the general catalog for the same providers; " + evidenceText)
	}
	graduatedTime := now
	judged.Scored = true
	judged.GraduatedTime = &graduatedTime
	judged.Judgement = "graduated; " + evidenceText
	return ProviderEgressCountrySiteJudgement{State: &judged, Change: ProviderEgressCountrySiteGraduated, Reason: ProviderEgressCountrySiteReasonPaired}
}

// The listed country sites of a catalog's country lists, in country and name
// order, with their URLs.
func ProviderEgressCountrySitesListed(countries map[string][]egresshealth.Destination) ([]ProviderEgressCountrySiteKey, map[ProviderEgressCountrySiteKey]string) {
	keys := []ProviderEgressCountrySiteKey{}
	urls := map[ProviderEgressCountrySiteKey]string{}
	for countryCode, destinations := range countries {
		for _, destination := range destinations {
			key := ProviderEgressCountrySiteKey{Name: destination.Name, CountryCode: strings.ToLower(countryCode)}
			if _, ok := urls[key]; !ok {
				keys = append(keys, key)
			}
			urls[key] = destination.Url
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].CountryCode != keys[j].CountryCode {
			return keys[i].CountryCode < keys[j].CountryCode
		}
		return keys[i].Name < keys[j].Name
	})
	return keys, urls
}
