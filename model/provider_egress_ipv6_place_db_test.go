package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The judgement of a dual-stack outcome, and its outcome as stored: the
// history row's ok_count and the verdicts kept in its evidence.
func testingIpv6JudgeAndStore(t testing.TB, health *ProviderEgressHealth) (*ProviderEgressHealth, int, string, string) {
	t.Helper()
	ctx := t.Context()
	judged := JudgeProviderEgressLegs(ctx, health)
	if err := judged.UrlProbeEvidence.ValidateOutcome(judged.OKCount, judged.Total, judged.TLSAuthenticationFailure); err != nil {
		t.Fatalf("the judged outcome fails validation: %v", err)
	}
	if !StoreProviderEgressHealth(ctx, judged).Stored {
		t.Fatal("the judged outcome was not stored")
	}
	var ok int
	var tcpVerdict, udpVerdict string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT ok_count, COALESCE(url_probe_evidence -> 'ipv6' ->> 'verdict', ''),
			COALESCE(url_probe_evidence -> 'ipv6' -> 'h3' ->> 'verdict', '')
			FROM provider_egress_health_history WHERE run_id = $1`, judged.RunId).Scan(&ok, &tcpVerdict, &udpVerdict))
	})
	return judged, ok, tcpVerdict, udpVerdict
}

// Where three other established networks in the provider's country passed the
// same URL over IPv6, a failed IPv6 HTTPS leg is the provider's: its outcome
// fails, although its IPv4 legs passed.
func TestIpv6FailureWhereThePlaceReachesTheUrlIsTheProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "IPv6 City", "IPv6 Region", "IPv6 Country", "zt")
		providers := testingH3Providers(t, city, 4)
		testingTlsEstablishNetworks(ctx, providers...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("dual-site", "https://dual.example/")
		for i, clientId := range providers[:3] {
			testingIpv6JudgeAndStore(t, testingIpv6UrlResult(clientId, site, now.Add(time.Duration(i)*time.Second), "", egresshealth.UrlProbeIpv6TcpPassed, ""))
		}
		judged, ok, verdict, _ := testingIpv6JudgeAndStore(t, testingIpv6UrlResult(providers[3], site, now.Add(time.Minute), "", egresshealth.UrlProbeIpv6TcpFailed, ""))
		if judged.OKCount != 0 || ok != 0 || verdict != egresshealth.UrlProbeH3VerdictProvider || judged.FailedNames != site.Name {
			t.Fatalf("ok=%d stored=%d verdict=%q failed=%q; the IPv6 failure is the provider's where its place reaches the URL over IPv6", judged.OKCount, ok, verdict, judged.FailedNames)
		}
	})
}

// Where fewer than three other networks passed the URL over IPv6, a failed
// IPv6 leg is the place's, and the outcome counts as its IPv4 legs, which
// passed. Place evidence is per transport: networks that passed over IPv6 TCP
// say nothing of QUIC over IPv6, so a silent IPv6 HTTP/3 leg there is the
// place's too.
func TestIpv6FailureWhereThePlaceDoesNotReachTheUrlCountsItsIpv4Legs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Thin IPv6 City", "Thin IPv6 Region", "Thin IPv6 Country", "zu")
		providers := testingH3Providers(t, city, 6)
		testingTlsEstablishNetworks(ctx, providers...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("dual-site", "https://dual.example/")
		for i, clientId := range providers[:2] {
			testingIpv6JudgeAndStore(t, testingIpv6UrlResult(clientId, site, now.Add(time.Duration(i)*time.Second), "", egresshealth.UrlProbeIpv6TcpPassed, ""))
		}
		judged, ok, verdict, _ := testingIpv6JudgeAndStore(t, testingIpv6UrlResult(providers[2], site, now.Add(time.Minute), "", egresshealth.UrlProbeIpv6TcpFailed, ""))
		if judged.OKCount != 1 || ok != 1 || verdict != egresshealth.UrlProbeH3VerdictPlace || judged.FailedNames != "" {
			t.Fatalf("ok=%d stored=%d verdict=%q failed=%q; where the place does not reach the URL over IPv6 the outcome is its IPv4 legs'", judged.OKCount, ok, verdict, judged.FailedNames)
		}
		// three networks have now passed over IPv6 TCP, none over QUIC over IPv6
		testingIpv6JudgeAndStore(t, testingIpv6UrlResult(providers[3], site, now.Add(2*time.Minute), "", egresshealth.UrlProbeIpv6TcpPassed, ""))
		site.H3 = true
		judged, ok, _, udpVerdict := testingIpv6JudgeAndStore(t, testingIpv6UrlResult(providers[4], site, now.Add(3*time.Minute), egresshealth.UrlProbeH3UdpPassed,
			egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeH3UdpSilent))
		if judged.OKCount != 1 || ok != 1 || udpVerdict != egresshealth.UrlProbeH3VerdictPlace {
			t.Fatalf("ok=%d stored=%d udp verdict=%q; IPv6 TCP evidence judged a silent IPv6 HTTP/3 leg", judged.OKCount, ok, udpVerdict)
		}
		var networks int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT cardinality(network_ids) FROM provider_egress_ipv6_place
				WHERE name = $1 AND country_code = 'zu' AND transport = 'tcp'`, site.Name).Scan(&networks))
		})
		if networks != 4 {
			t.Fatalf("IPv6 TCP place evidence holds %d networks, want the 4 that passed", networks)
		}
	})
}

// The evidence the judgement reads is kept a day and the day before: older
// days are removed with the HTTP/3 evidence.
func TestIpv6PlaceEvidenceExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		networkId := server.NewId()
		for _, age := range []time.Duration{0, 24 * time.Hour, 3 * 24 * time.Hour} {
			addProviderEgressIpv6Place(ctx, "expiring-site", "zs", now.Add(-age), ProviderEgressIpv6PlaceTcp, networkId, now)
		}
		RemoveExpiredProviderEgressTallies(ctx, now.Add(-2*24*time.Hour))
		if count := countProviderEgressIpv6PlaceNetworks(ctx, "expiring-site", "zs", ProviderEgressIpv6PlaceTcp, now.Add(-5*24*time.Hour), nil); count != 1 {
			t.Fatalf("%d networks after expiry, want the one network of the days kept", count)
		}
		var days int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provider_egress_ipv6_place WHERE name = 'expiring-site'`).Scan(&days))
		})
		if days != 2 {
			t.Fatalf("%d days kept, want today and yesterday", days)
		}
	})
}

// The claim carries the families each provider advertises, by the rule
// find-providers2 serves them by: both proven is dual-stack; IPv4 proven, or
// nothing proven (a legacy provider), is IPv4 alone. The claim never issues an
// IPv6-only provider.
func TestUrlProbeClaimCarriesTheAdvertisedFamily(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clients := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 4)
		proven := map[server.Id][2]bool{
			clients[0]: {true, true},
			clients[1]: {true, false},
			clients[2]: {false, false},
			clients[3]: {false, true},
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for clientId, families := range proven {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET ipv4_proven = $2, ipv6_proven = $3 WHERE client_id = $1`,
					clientId, families[0], families[1]))
			}
		})
		var observation ProviderUrlProbeDueObservation
		due := ClaimProviderUrlProbeDueWithObservation(ctx, now, 10, 0, 1, &observation)
		got := map[server.Id]connect.IpFamily{}
		for _, provider := range due.Providers {
			got[provider.ClientId] = provider.IpFamily
		}
		want := map[server.Id]connect.IpFamily{
			clients[0]: connect.IpFamilyDualstack,
			clients[1]: connect.IpFamilyV4Only,
			clients[2]: connect.IpFamilyV4Only,
		}
		if len(got) != len(want) {
			t.Fatalf("claimed %v, want %v", got, want)
		}
		for clientId, family := range want {
			if got[clientId] != family {
				t.Fatalf("claimed %v, want %v", got, want)
			}
		}
		if ProviderIpFamilyOfProven(false, true) != connect.IpFamilyV6Only || ProviderIpFamilyOfProven(false, false) != connect.IpFamilyV4Only {
			t.Fatal("the proven-family rule is not find-providers2's")
		}
	})
}
