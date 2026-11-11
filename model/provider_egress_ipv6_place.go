// A failed IPv6 leg is judged by place before it counts against its provider,
// as a silent HTTP/3 leg is (provider_egress_h3_place.go).
//
// A dual-stack provider's load of a URL that serves IPv6 is made over IPv4
// and then over IPv6 through the same provider (qualityprobe/egresshealth
// url_family.go). IPv6 is not neutral by place: whole countries, ISPs and
// many enterprise networks carry it badly or not at all, a CDN can answer AAAA
// from POPs whose IPv6 does not serve the site, and a provider's IPv6 can be a
// tunnel that egresses elsewhere. So an IPv6 failure is the provider's own only
// where the place carries IPv6 to that URL:
//
//   - The place evidence is per URL, country, UTC day and transport: the
//     established networks (at least providerEgressH3PlaceMinNetworkAge old)
//     whose exits in that country passed the URL's IPv6 HTTPS leg (tcp), or
//     whose IPv6 HTTP/3 leg passed or was answered by the site (udp). A day
//     keeps at most providerEgressH3PlaceNetworkCap of them per transport.
//   - A failed IPv6 HTTPS leg on that day or the day before is the provider's
//     (verdict provider: its URL outcome fails) when at least
//     ProviderEgressH3PlaceMinNetworks networks other than the provider's own
//     passed the same URL over IPv6 from the same country; a silent IPv6
//     HTTP/3 leg likewise against the networks that reached the URL over QUIC
//     over IPv6. Otherwise the place does not carry it (verdict place).
//   - The outcome passes when its IPv4 HTTPS leg passed and every leg that
//     failed after it, a silent IPv4 HTTP/3 leg included, was judged the
//     place's; it fails when any was judged the provider's.
//   - Counted by network, a farm on one network cannot make the place for its
//     neighbours; a young network makes none; the provider's own network never
//     counts toward its own verdict; a provider whose country is unknown has
//     its IPv6 failures judged the place's.
//
// A failed IPv4 leg is judged as it always was: by nothing but the outcome.
// IPv4 is what every provider is admitted on, and today's HTTPS judging (with
// TLS findings judged on their own) stays as it is.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The transports of the IPv6 place evidence.
const (
	ProviderEgressIpv6PlaceTcp = "tcp"
	ProviderEgressIpv6PlaceUdp = "udp"
)

// Records that networkId's exit in countryCode reached the URL over IPv6 on
// day, over the transport. A network already recorded, or a full day, writes
// nothing and takes no row lock, as for the HTTP/3 evidence.
func addProviderEgressIpv6Place(ctx context.Context, name string, countryCode string, day time.Time, transport string, networkId server.Id, now time.Time) {
	day = day.UTC().Truncate(24 * time.Hour)
	known := false
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM provider_egress_ipv6_place
				WHERE name = $1 AND country_code = $2 AND tally_day = $3 AND transport = $4
					AND (network_ids @> ARRAY[$5::uuid] OR $6 <= cardinality(network_ids))
			)`,
			name, countryCode, day, transport, networkId, providerEgressH3PlaceNetworkCap).Scan(&known))
	})
	if known {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO provider_egress_ipv6_place AS place (name, country_code, tally_day, transport, network_ids, update_time)
			VALUES ($1, $2, $3, $4, ARRAY[$5::uuid], $6)
			ON CONFLICT (name, country_code, tally_day, transport) DO UPDATE SET
				network_ids = place.network_ids || EXCLUDED.network_ids,
				update_time = EXCLUDED.update_time
			WHERE NOT (place.network_ids @> EXCLUDED.network_ids)
				AND cardinality(place.network_ids) < $7`,
			name, countryCode, day, transport, networkId, now.UTC(), providerEgressH3PlaceNetworkCap))
	}, server.TxReadCommitted)
}

// The established networks other than excludeNetworkId that reached the URL
// over IPv6 and the transport from countryCode on minDay or later.
func countProviderEgressIpv6PlaceNetworks(ctx context.Context, name string, countryCode string, transport string, minDay time.Time, excludeNetworkId *server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT count(DISTINCT network_id)::integer
			FROM provider_egress_ipv6_place AS place
			CROSS JOIN LATERAL unnest(place.network_ids) AS network_id
			WHERE place.name = $1 AND place.country_code = $2 AND place.transport = $3 AND place.tally_day >= $4
				AND network_id IS DISTINCT FROM $5`,
			name, countryCode, transport, minDay.UTC().Truncate(24*time.Hour), excludeNetworkId).Scan(&count))
	})
	return count
}

// Removes the IPv6 place evidence older than the judgement reads: the day
// before minDay and earlier.
func RemoveExpiredProviderEgressIpv6Places(ctx context.Context, minDay time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_ipv6_place WHERE tally_day < $1`,
			minDay.UTC().Truncate(24*time.Hour)))
	})
}

// The place evidence a judgement reads and writes, behind seams a pure test
// can replace (the database functions above and in provider_egress_h3_place.go
// are the production ones).
type providerEgressLegPlaces struct {
	provider  func() providerEgressH3Provider
	addH3     func(name string, countryCode string, day time.Time, networkId server.Id)
	countH3   func(name string, countryCode string, minDay time.Time, excludeNetworkId *server.Id) int
	addIpv6   func(name string, countryCode string, day time.Time, transport string, networkId server.Id)
	countIpv6 func(name string, countryCode string, transport string, minDay time.Time, excludeNetworkId *server.Id) int
}

// The database's place evidence for one provider's outcome. The provider is
// read at most once, and only when a leg needs it.
func providerEgressLegPlacesOf(ctx context.Context, clientId server.Id, now time.Time) providerEgressLegPlaces {
	var provider *providerEgressH3Provider
	return providerEgressLegPlaces{
		provider: func() providerEgressH3Provider {
			if provider == nil {
				read := readProviderEgressH3Provider(ctx, clientId, now.Add(-providerEgressH3PlaceMinNetworkAge))
				provider = &read
			}
			return *provider
		},
		addH3: func(name string, countryCode string, day time.Time, networkId server.Id) {
			addProviderEgressH3Place(ctx, name, countryCode, day, networkId, now)
		},
		countH3: func(name string, countryCode string, minDay time.Time, excludeNetworkId *server.Id) int {
			return countProviderEgressH3PlaceNetworks(ctx, name, countryCode, minDay, excludeNetworkId)
		},
		addIpv6: func(name string, countryCode string, day time.Time, transport string, networkId server.Id) {
			addProviderEgressIpv6Place(ctx, name, countryCode, day, transport, networkId, now)
		},
		countIpv6: func(name string, countryCode string, transport string, minDay time.Time, excludeNetworkId *server.Id) int {
			return countProviderEgressIpv6PlaceNetworks(ctx, name, countryCode, transport, minDay, excludeNetworkId)
		},
	}
}

// Judges a URL outcome's later legs by place -- its IPv4 HTTP/3 leg, and the
// IPv6 legs of a dual-stack provider -- and records the place evidence each
// leg that reached its site gives (see the file comment and
// provider_egress_h3_place.go). An outcome with no IPv6 leg is judged exactly
// as JudgeProviderEgressH3 judges it. Any other outcome is returned as it is.
// The caller's record is not modified.
func JudgeProviderEgressLegs(ctx context.Context, health *ProviderEgressHealth) *ProviderEgressHealth {
	if health == nil || health.UrlProbeEvidence == nil || health.UrlProbeEvidence.Ipv6 == nil {
		return JudgeProviderEgressH3(ctx, health)
	}
	if health.Total != 1 || health.UrlProbeEvidence.HasLegVerdict() {
		return health
	}
	return judgeProviderEgressLegs(health, providerEgressLegPlacesOf(ctx, health.ClientId, server.NowUtc()))
}

// The judgement of JudgeProviderEgressLegs over the given place evidence, for
// an outcome with an IPv6 leg.
func judgeProviderEgressLegs(health *ProviderEgressHealth, places providerEgressLegPlaces) *ProviderEgressHealth {
	evidence := *health.UrlProbeEvidence
	day := evidence.MeasuredAt.UTC().Truncate(24 * time.Hour)
	name := evidence.Destination.Name
	if evidence.H3 != nil {
		leg := *evidence.H3
		evidence.H3 = &leg
	}
	ipv6 := *evidence.Ipv6
	evidence.Ipv6 = &ipv6
	if ipv6.H3 != nil {
		leg := *ipv6.H3
		ipv6.H3 = &leg
	}
	// what a leg that reached its site records, and what a failed leg reads
	record := func(add func(countryCode string, networkId server.Id)) {
		provider := places.provider()
		if provider.countryCode != "" && provider.networkId != nil && provider.established {
			add(provider.countryCode, *provider.networkId)
		}
	}
	verdict := func(count func(countryCode string, excludeNetworkId *server.Id) int) string {
		provider := places.provider()
		networks := 0
		if provider.countryCode != "" {
			networks = count(provider.countryCode, provider.networkId)
		}
		if ProviderEgressH3PlaceMinNetworks <= networks {
			return egresshealth.UrlProbeH3VerdictProvider
		}
		return egresshealth.UrlProbeH3VerdictPlace
	}
	minDay := day.Add(-24 * time.Hour)
	failed, provided := 0, 0
	judge := func(set *string, count func(countryCode string, excludeNetworkId *server.Id) int) {
		failed++
		*set = verdict(count)
		if *set == egresshealth.UrlProbeH3VerdictProvider {
			provided++
		}
	}

	if leg := evidence.H3; leg != nil {
		switch leg.Udp {
		case egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeH3UdpAnswered:
			record(func(countryCode string, networkId server.Id) { places.addH3(name, countryCode, day, networkId) })
		case egresshealth.UrlProbeH3UdpSilent:
			judge(&leg.Verdict, func(countryCode string, excludeNetworkId *server.Id) int {
				return places.countH3(name, countryCode, minDay, excludeNetworkId)
			})
		}
	}
	switch ipv6.Tcp {
	case egresshealth.UrlProbeIpv6TcpPassed:
		record(func(countryCode string, networkId server.Id) {
			places.addIpv6(name, countryCode, day, ProviderEgressIpv6PlaceTcp, networkId)
		})
	case egresshealth.UrlProbeIpv6TcpFailed:
		judge(&ipv6.Verdict, func(countryCode string, excludeNetworkId *server.Id) int {
			return places.countIpv6(name, countryCode, ProviderEgressIpv6PlaceTcp, minDay, excludeNetworkId)
		})
	}
	if leg := ipv6.H3; leg != nil {
		switch leg.Udp {
		case egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeH3UdpAnswered:
			record(func(countryCode string, networkId server.Id) {
				places.addIpv6(name, countryCode, day, ProviderEgressIpv6PlaceUdp, networkId)
			})
		case egresshealth.UrlProbeH3UdpSilent:
			judge(&leg.Verdict, func(countryCode string, excludeNetworkId *server.Id) int {
				return places.countIpv6(name, countryCode, ProviderEgressIpv6PlaceUdp, minDay, excludeNetworkId)
			})
		}
	}
	if failed == 0 {
		// every later leg reached its site or was not measured: nothing to judge
		return health
	}
	if health.OKCount != 0 {
		// a passing outcome with a failed leg is no prober's (ValidateOutcome);
		// it stays as it came
		return health
	}
	judged := *health
	judged.UrlProbeEvidence = &evidence
	if provided == 0 {
		// every failure was the place's: the outcome counts as its IPv4 HTTPS
		// leg, which passed
		passedByPlace(&judged, health)
	}
	return &judged
}
