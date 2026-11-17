// A silent HTTP/3 leg is judged by place before it counts against its
// provider.
//
// A URL that declares h3 is loaded over HTTPS and then over HTTP/3 through
// the same provider (qualityprobe/egresshealth url_h3.go). Only a silent leg
// -- not one datagram back to the QUIC Initial -- is evidence against the
// provider's UDP path, and even that evidence is about the place before it is
// about the provider: national firewalls (Russia, China and Iran are
// reported to) and many enterprise and campus networks drop or throttle QUIC,
// and an honest exit behind them fails every HTTP/3 leg. So, as TLS findings
// are judged before anything is excluded (provider_egress_tls_verdict.go, the
// owner's hardened "place" decisions of 2026-11-07 and 11-08), a silent leg
// is the provider's own only where the place carries QUIC to that URL:
//
//   - The place evidence is per URL, country and UTC day: the established
//     networks (at least providerEgressH3PlaceMinNetworkAge old, as the TLS
//     place rule's networks) whose exits in that country reached the URL over
//     QUIC, their HTTP/3 leg passed or answered by the site. A day keeps at
//     most providerEgressH3PlaceNetworkCap of them, so the evidence costs a
//     handful of writes a day per URL and country, whatever the traffic.
//   - A silent leg on that day or the day before is the provider's (verdict
//     provider: its URL outcome fails) when at least
//     ProviderEgressH3PlaceMinNetworks networks other than the provider's own
//     reached the same URL over QUIC from the same country. Otherwise the
//     place does not carry it (verdict place): the outcome counts as its HTTPS
//     leg, which passed, exactly as for a URL without h3.
//   - Counted by network, a farm of providers on one network cannot make the
//     place for its neighbours; a network younger than two weeks makes none;
//     and the provider's own network never counts toward its own verdict.
//   - A provider whose country is unknown has no place to compare with: its
//     silent legs are judged the place's.
//
// The ingest judges each outcome as it arrives (controller
// SubmitProviderEgressHealth), before it is stored, so the stored ok_count,
// class tally, rolling quota and pacing all carry the verdict, and the
// verdict is kept in the evidence (url_probe_evidence.h3.verdict).
package model

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

const (
	// The other established networks in the same country that must have
	// reached a URL over QUIC, that day or the day before, for a silent leg
	// there to be the provider's: the three networks the TLS place rule asks
	// for.
	ProviderEgressH3PlaceMinNetworks = 3
	// How many networks a URL's day in one country keeps. With one of them
	// possibly the judged provider's own, eight leave more than the three
	// others the judgement needs.
	providerEgressH3PlaceNetworkCap = 8
	// Only a network this old is place evidence, as for the TLS place rule.
	providerEgressH3PlaceMinNetworkAge = providerEgressTlsPlaceMinNetworkAge
)

// What the judgement reads of a provider: its country, network, and whether
// that network is established.
type providerEgressH3Provider struct {
	countryCode string
	networkId   *server.Id
	established bool
}

func readProviderEgressH3Provider(ctx context.Context, clientId server.Id, establishedBy time.Time) providerEgressH3Provider {
	provider := providerEgressH3Provider{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT
				COALESCE(country_location.country_code, ''),
				client.network_id,
				COALESCE(network.create_time <= $2, false)
			FROM network_client AS client
			LEFT JOIN network ON network.network_id = client.network_id
			LEFT JOIN network_client_location_reliability AS reliability ON reliability.client_id = client.client_id
			LEFT JOIN location AS country_location ON country_location.location_id = reliability.country_location_id
			WHERE client.client_id = $1`,
			clientId, establishedBy.UTC())
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&provider.countryCode, &networkId, &provider.established))
				provider.networkId = &networkId
				provider.countryCode = strings.ToLower(strings.TrimSpace(provider.countryCode))
			}
		})
	})
	return provider
}

// Records that networkId's exit in countryCode reached the URL over QUIC on
// day. A network already recorded, or a full day, writes nothing and takes no
// row lock: on a busy URL nearly every leg is one of those, and an ON
// CONFLICT that updates nothing still locks the row, which writes its xmax
// and a WAL record. So those cost one read, and only a network new to the
// day reaches the upsert, at most providerEgressH3PlaceNetworkCap times a day.
func addProviderEgressH3Place(ctx context.Context, name string, countryCode string, day time.Time, networkId server.Id, now time.Time) {
	day = day.UTC().Truncate(24 * time.Hour)
	known := false
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM provider_egress_h3_place
				WHERE name = $1 AND country_code = $2 AND tally_day = $3
					AND (network_ids @> ARRAY[$4::uuid] OR $5 <= cardinality(network_ids))
			)`,
			name, countryCode, day, networkId, providerEgressH3PlaceNetworkCap).Scan(&known))
	})
	if known {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO provider_egress_h3_place AS place (name, country_code, tally_day, network_ids, update_time)
			VALUES ($1, $2, $3, ARRAY[$4::uuid], $5)
			ON CONFLICT (name, country_code, tally_day) DO UPDATE SET
				network_ids = place.network_ids || EXCLUDED.network_ids,
				update_time = EXCLUDED.update_time
			WHERE NOT (place.network_ids @> EXCLUDED.network_ids)
				AND cardinality(place.network_ids) < $6`,
			name, countryCode, day, networkId, now.UTC(), providerEgressH3PlaceNetworkCap))
	}, server.TxReadCommitted)
}

// The established networks other than excludeNetworkId that reached the URL
// over QUIC from countryCode on minDay or later.
func countProviderEgressH3PlaceNetworks(ctx context.Context, name string, countryCode string, minDay time.Time, excludeNetworkId *server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT count(DISTINCT network_id)::integer
			FROM provider_egress_h3_place AS place
			CROSS JOIN LATERAL unnest(place.network_ids) AS network_id
			WHERE place.name = $1 AND place.country_code = $2 AND place.tally_day >= $3
				AND network_id IS DISTINCT FROM $4`,
			name, countryCode, minDay.UTC().Truncate(24*time.Hour), excludeNetworkId).Scan(&count))
	})
	return count
}

// Judges a URL outcome's HTTP/3 leg by place, and records the place evidence
// a leg that reached its site gives (see the file comment). Any other outcome
// is returned as it is. The caller's record is not modified.
func JudgeProviderEgressH3(ctx context.Context, health *ProviderEgressHealth) *ProviderEgressHealth {
	if health == nil || health.UrlProbeEvidence == nil || health.UrlProbeEvidence.H3 == nil || health.Total != 1 ||
		health.UrlProbeEvidence.H3.Verdict != "" {
		return health
	}
	evidence := health.UrlProbeEvidence
	now := server.NowUtc()
	day := evidence.MeasuredAt.UTC().Truncate(24 * time.Hour)
	switch evidence.H3.Udp {
	case egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeH3UdpAnswered:
		provider := readProviderEgressH3Provider(ctx, health.ClientId, now.Add(-providerEgressH3PlaceMinNetworkAge))
		if provider.countryCode != "" && provider.networkId != nil && provider.established {
			addProviderEgressH3Place(ctx, evidence.Destination.Name, provider.countryCode, day, *provider.networkId, now)
		}
		return health
	case egresshealth.UrlProbeH3UdpSilent:
		if health.OKCount != 0 {
			return health
		}
		provider := readProviderEgressH3Provider(ctx, health.ClientId, now.Add(-providerEgressH3PlaceMinNetworkAge))
		networks := 0
		if provider.countryCode != "" {
			networks = countProviderEgressH3PlaceNetworks(ctx, evidence.Destination.Name, provider.countryCode, day.Add(-24*time.Hour), provider.networkId)
		}
		return judgedProviderEgressH3(health, networks)
	}
	return health
}

// The verdict of a silent leg once the other established networks in its
// place that reached its URL over QUIC are counted: provider at
// ProviderEgressH3PlaceMinNetworks or more, its outcome failing; place
// otherwise, its outcome counted as the HTTPS leg that passed, the one load a
// success in its class and no failed name. The caller's record is not
// modified.
func judgedProviderEgressH3(health *ProviderEgressHealth, otherNetworks int) *ProviderEgressHealth {
	judged := *health
	evidence := *health.UrlProbeEvidence
	leg := *evidence.H3
	evidence.H3 = &leg
	judged.UrlProbeEvidence = &evidence
	if ProviderEgressH3PlaceMinNetworks <= otherNetworks {
		leg.Verdict = egresshealth.UrlProbeH3VerdictProvider
		return &judged
	}
	leg.Verdict = egresshealth.UrlProbeH3VerdictPlace
	passedByPlace(&judged, health)
	return &judged
}

// Counts a judged outcome whose every failed later leg was the place's as the
// IPv4 HTTPS leg that passed: the one load a success in its class and no failed
// name. health is the outcome as it came, which is not modified.
func passedByPlace(judged *ProviderEgressHealth, health *ProviderEgressHealth) {
	judged.OKCount = 1
	class := string(judged.UrlProbeEvidence.Destination.Class)
	judged.ClassResults = map[string]ProviderEgressHealthClassResult{}
	for name, tally := range health.ClassResults {
		judged.ClassResults[name] = tally
	}
	tally := judged.ClassResults[class]
	tally.OK, tally.Total = 1, 1
	judged.ClassResults[class] = tally
	failed := []string{}
	for _, name := range splitProviderEgressNames(health.FailedNames) {
		if name != judged.UrlProbeEvidence.Destination.Name {
			failed = append(failed, name)
		}
	}
	judged.FailedNames = strings.Join(failed, ",")
}

// Removes the HTTP/3 place evidence older than the judgement reads: the day
// before minDay and earlier.
func RemoveExpiredProviderEgressH3Places(ctx context.Context, minDay time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_h3_place WHERE tally_day < $1`,
			minDay.UTC().Truncate(24*time.Hour)))
	})
}
