package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// One URL outcome of clientId on site at `at` whose HTTPS leg passed and
// whose HTTP/3 leg was udp: passed, answered by the site, or silent.
func testingH3UrlResult(clientId server.Id, site egresshealth.Destination, at time.Time, udp string) *ProviderEgressHealth {
	evidence := fp2TestUrlProbeEvidence(at, true)
	site.H3 = true
	evidence.Destination = site
	evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: site, MeasuredAt: at, TlsAuthenticated: true}}
	ok := 1
	switch udp {
	case egresshealth.UrlProbeH3UdpPassed:
		evidence.H3 = &egresshealth.UrlProbeH3Evidence{Udp: udp, ContentClassification: "content", PerformanceClassification: "insufficient_sample",
			TlsAuthenticated: true, StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
			RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000}
	case egresshealth.UrlProbeH3UdpAnswered:
		evidence.H3 = &egresshealth.UrlProbeH3Evidence{Udp: udp, FailureStage: "tls"}
	case egresshealth.UrlProbeH3UdpSilent:
		ok = 0
		evidence.FailureStage = egresshealth.FailureStageH3
		evidence.H3 = &egresshealth.UrlProbeH3Evidence{Udp: udp, FailureStage: egresshealth.FailureStageDialQuic}
	}
	failed := ""
	if ok == 0 {
		failed = site.Name
	}
	return &ProviderEgressHealth{
		RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, OKCount: ok, Total: 1, UrlProbeEvidence: evidence,
		ClassResults: map[string]ProviderEgressHealthClassResult{string(site.Class): {OK: ok, Total: 1}}, FailedNames: failed,
	}
}

// Providers connected in a new country, each on a network of its own unless
// they share networkId, with their reliability rows (and so their countries)
// written.
func testingH3Providers(t testing.TB, location *Location, count int) []server.Id {
	ctx := t.Context()
	providers := []server.Id{}
	for range count {
		providers = append(providers, egressTestConnect(ctx, t, location, egressTestUnsampled, nil, nil).clientId)
	}
	return providers
}

func testingH3Locate(t testing.TB) {
	now := server.NowUtc()
	UpdateClientLocationReliabilities(t.Context(), now.Add(-time.Hour), now)
}

// The judgement of a silent leg, and its outcome as stored: the history row's
// ok_count and the verdict kept in its evidence.
func testingH3JudgeAndStore(t testing.TB, health *ProviderEgressHealth) (*ProviderEgressHealth, int, string) {
	t.Helper()
	ctx := t.Context()
	judged := JudgeProviderEgressH3(ctx, health)
	if err := judged.UrlProbeEvidence.ValidateOutcome(judged.OKCount, judged.Total, judged.TLSAuthenticationFailure); err != nil {
		t.Fatalf("the judged outcome fails validation: %v", err)
	}
	if !StoreProviderEgressHealth(ctx, judged).Stored {
		t.Fatal("the judged outcome was not stored")
	}
	var ok int
	var verdict string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT ok_count, COALESCE(url_probe_evidence -> 'h3' ->> 'verdict', '')
			FROM provider_egress_health_history WHERE run_id = $1`, judged.RunId).Scan(&ok, &verdict))
	})
	return judged, ok, verdict
}

// Where three other established networks in the provider's country reach the
// same URL over QUIC, a silent leg is the provider's: its outcome fails.
func TestH3SilentLegWhereThePlaceReachesTheUrlIsTheProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "H3 City", "H3 Region", "H3 Country", "zx")
		providers := testingH3Providers(t, city, 4)
		testingTlsEstablishNetworks(ctx, providers...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("h3-site", "https://h3.example/")
		for i, clientId := range providers[:3] {
			udp := egresshealth.UrlProbeH3UdpPassed
			if i == 2 {
				// a site answering and refusing proves the place carries QUIC too
				udp = egresshealth.UrlProbeH3UdpAnswered
			}
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, site, now.Add(time.Duration(i)*time.Second), udp))
		}
		judged, ok, verdict := testingH3JudgeAndStore(t, testingH3UrlResult(providers[3], site, now.Add(time.Minute), egresshealth.UrlProbeH3UdpSilent))
		if judged.OKCount != 0 || ok != 0 || verdict != egresshealth.UrlProbeH3VerdictProvider || judged.FailedNames != site.Name {
			t.Fatalf("ok=%d stored=%d verdict=%q failed=%q; the silence is the provider's where its place reaches the URL", judged.OKCount, ok, verdict, judged.FailedNames)
		}
	})
}

// Where the place does not reach the URL over QUIC -- fewer than three other
// networks did -- a silent leg is the place's, and the outcome counts as its
// HTTPS leg, which passed.
func TestH3SilentLegWhereThePlaceDoesNotReachTheUrlCountsItsHttpsLeg(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Blocked H3 City", "Blocked H3 Region", "Blocked H3 Country", "zw")
		providers := testingH3Providers(t, city, 3)
		testingTlsEstablishNetworks(ctx, providers...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("h3-site", "https://h3.example/")
		for i, clientId := range providers[:2] {
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, site, now.Add(time.Duration(i)*time.Second), egresshealth.UrlProbeH3UdpPassed))
		}
		judged, ok, verdict := testingH3JudgeAndStore(t, testingH3UrlResult(providers[2], site, now.Add(time.Minute), egresshealth.UrlProbeH3UdpSilent))
		if judged.OKCount != 1 || ok != 1 || verdict != egresshealth.UrlProbeH3VerdictPlace || judged.FailedNames != "" ||
			judged.ClassResults["site"] != (ProviderEgressHealthClassResult{OK: 1, Total: 1}) {
			t.Fatalf("ok=%d stored=%d verdict=%q failed=%q classes=%v; a place that does not reach the URL over QUIC must not fail the provider",
				judged.OKCount, ok, verdict, judged.FailedNames, judged.ClassResults)
		}
	})
}

// The place is made only by other, established networks: providers on the
// judged provider's own network, and networks younger than two weeks, never
// count toward it, however many of them reach the URL.
func TestH3PlaceIsNotMadeByTheProvidersOwnNetworkOrYoungNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Farm H3 City", "Farm H3 Region", "Farm H3 Country", "zv")
		farm := server.NewId()
		own := []server.Id{}
		for range 5 {
			own = append(own, egressTestConnectOn(ctx, t, farm, city, egressTestUnsampled, nil, nil).clientId)
		}
		young := testingH3Providers(t, city, 3)
		established := testingH3Providers(t, city, 2)
		testingTlsEstablishNetworks(ctx, append(append([]server.Id{}, own...), established...)...)
		testingTlsCreateNetworks(ctx, server.NowUtc().Add(-24*time.Hour), young...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("h3-site", "https://h3.example/")
		at := now
		for _, clientId := range append(append(append([]server.Id{}, own[1:]...), young...), established...) {
			at = at.Add(time.Second)
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, site, at, egresshealth.UrlProbeH3UdpPassed))
		}
		judged, ok, verdict := testingH3JudgeAndStore(t, testingH3UrlResult(own[0], site, now.Add(time.Minute), egresshealth.UrlProbeH3UdpSilent))
		if ok != 1 || verdict != egresshealth.UrlProbeH3VerdictPlace {
			t.Fatalf("ok=%d verdict=%q; only two other established networks reach the URL, which makes no place", judged.OKCount, verdict)
		}
	})
}

// The place is per URL and country: other URLs reached over QUIC, and the
// same URL reached from another country, are no evidence for this one.
func TestH3PlaceIsPerUrlAndCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		home := egressTestCity(ctx, "Home H3 City", "Home H3 Region", "Home H3 Country", "zu")
		abroad := egressTestCity(ctx, "Abroad H3 City", "Abroad H3 Region", "Abroad H3 Country", "zt")
		homeProviders := testingH3Providers(t, home, 4)
		abroadProviders := testingH3Providers(t, abroad, 3)
		testingTlsEstablishNetworks(ctx, append(append([]server.Id{}, homeProviders...), abroadProviders...)...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("h3-site", "https://h3.example/")
		other := testingTlsSite("other-h3-site", "https://other-h3.example/")
		at := now
		for _, clientId := range homeProviders[:3] {
			at = at.Add(time.Second)
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, other, at, egresshealth.UrlProbeH3UdpPassed))
		}
		for _, clientId := range abroadProviders {
			at = at.Add(time.Second)
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, site, at, egresshealth.UrlProbeH3UdpPassed))
		}
		judged, ok, verdict := testingH3JudgeAndStore(t, testingH3UrlResult(homeProviders[3], site, now.Add(time.Minute), egresshealth.UrlProbeH3UdpSilent))
		if ok != 1 || verdict != egresshealth.UrlProbeH3VerdictPlace {
			t.Fatalf("ok=%d verdict=%q; evidence for another URL or another country made this place", judged.OKCount, verdict)
		}
		// and from the provider's own country, the same URL is the evidence
		for _, clientId := range homeProviders[:3] {
			at = at.Add(time.Second)
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, site, at, egresshealth.UrlProbeH3UdpPassed))
		}
		_, ok, verdict = testingH3JudgeAndStore(t, testingH3UrlResult(homeProviders[3], site, at.Add(time.Minute), egresshealth.UrlProbeH3UdpSilent))
		if ok != 0 || verdict != egresshealth.UrlProbeH3VerdictProvider {
			t.Fatalf("ok=%d verdict=%q; three other home networks reaching the URL make the place", ok, verdict)
		}
	})
}

// A day keeps at most eight networks of a URL's place, so the evidence costs
// a bounded number of writes whatever the traffic, and the oldest days are
// removed with the tallies.
func TestH3PlaceEvidenceIsBoundedAndExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Busy H3 City", "Busy H3 Region", "Busy H3 Country", "zs")
		providers := testingH3Providers(t, city, 12)
		testingTlsEstablishNetworks(ctx, providers...)
		testingH3Locate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("h3-site", "https://h3.example/")
		for i, clientId := range providers {
			testingH3JudgeAndStore(t, testingH3UrlResult(clientId, site, now.Add(time.Duration(i)*time.Second), egresshealth.UrlProbeH3UdpPassed))
		}
		count := func() (rows int, networks int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*)::integer, COALESCE(max(cardinality(network_ids)), 0)::integer
					FROM provider_egress_h3_place WHERE country_code = 'zs'`).Scan(&rows, &networks))
			})
			return
		}
		if rows, networks := count(); rows != 1 || networks != providerEgressH3PlaceNetworkCap {
			t.Fatalf("rows=%d networks=%d; a day of one URL's place keeps %d networks", rows, networks, providerEgressH3PlaceNetworkCap)
		}
		RemoveExpiredProviderEgressH3Places(ctx, now.Add(48*time.Hour))
		if rows, _ := count(); rows != 0 {
			t.Fatalf("rows=%d after the place evidence expired", rows)
		}
	})
}

// The pool refresh's prober-fault line judges loads by their HTTPS leg, as the
// site tallies do: a URL outcome whose only failure is a silent HTTP/3 leg
// judged the provider's is a load that reached its site over TCP, so a fleet
// whose providers do not carry UDP cannot stop the refresh. Failures of the
// HTTPS leg still count.
func TestGetProviderEgressHealthContactedTotalJudgesHttp3LoadsByTheirHttpsLeg(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		at := now.Add(-time.Minute)
		site := testingTlsSite("h3-site", "https://h3.example/")
		silent := testingH3UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpSilent)
		silent = judgedProviderEgressH3(silent, ProviderEgressH3PlaceMinNetworks)
		if silent.OKCount != 0 || silent.UrlProbeEvidence.H3.Verdict != egresshealth.UrlProbeH3VerdictProvider {
			t.Fatalf("fixture: %+v", silent)
		}
		for _, health := range []*ProviderEgressHealth{
			silent,
			testingH3UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpPassed),
			testingH3UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpAnswered),
		} {
			if err := health.UrlProbeEvidence.ValidateOutcome(health.OKCount, health.Total, health.TLSAuthenticationFailure); err != nil {
				t.Fatalf("fixture fails validation: %v", err)
			}
			if !StoreProviderEgressHealth(ctx, health).Stored {
				t.Fatal("the outcome was not stored")
			}
		}
		// an HTTPS failure that reached its site
		failed := fp2TestUrlProbeEvidence(at, false)
		if !StoreProviderEgressHealth(ctx, &ProviderEgressHealth{
			RunId: server.NewId(), ClientId: server.NewId(), MeasuredAt: at, Total: 1, UrlProbeEvidence: failed,
			ClassResults: map[string]ProviderEgressHealthClassResult{string(failed.Destination.Class): {OK: 0, Total: 1}}, FailedNames: failed.Destination.Name,
		}).Stored {
			t.Fatal("the HTTPS failure was not stored")
		}
		if got, want := GetProviderEgressHealthContactedTotal(ctx, now.Add(-time.Hour)), (ProviderEgressContactedTotal{Ok: 3, Total: 4}); got != want {
			t.Fatalf("contacted total = %+v, want %+v", got, want)
		}
	})
}

// Every HTTP/3 leg that reached its site offers its network to the place; on
// a busy URL nearly every offer is a network already counted or a day already
// full. Those are a read, and take no row lock: a lock-only ON CONFLICT would
// write the tuple's xmax and a WAL record on each leg. The row's xmax (which
// an ON CONFLICT update leaves set to its own lock) moves only with a write.
func TestH3PlaceEvidenceOfAKnownNetworkOrAFullDayTakesNoRowLock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		day := now.UTC().Truncate(24 * time.Hour)
		read := func() (xmax string, networks int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT xmax::text, cardinality(network_ids)::integer
					FROM provider_egress_h3_place WHERE name = 'h3-site' AND country_code = 'zs' AND tally_day = $1`, day).Scan(&xmax, &networks))
			})
			return
		}
		first := server.NewId()
		addProviderEgressH3Place(ctx, "h3-site", "zs", day, first, now)
		before, networks := read()
		if networks != 1 {
			t.Fatalf("networks=%d after the first network", networks)
		}
		addProviderEgressH3Place(ctx, "h3-site", "zs", day, first, now)
		if xmax, networks := read(); xmax != before || networks != 1 {
			t.Fatalf("xmax %s -> %s networks=%d; a network already counted locked the row", before, xmax, networks)
		}
		for range providerEgressH3PlaceNetworkCap - 1 {
			addProviderEgressH3Place(ctx, "h3-site", "zs", day, server.NewId(), now)
		}
		full, networks := read()
		if networks != providerEgressH3PlaceNetworkCap {
			t.Fatalf("networks=%d after filling the day", networks)
		}
		addProviderEgressH3Place(ctx, "h3-site", "zs", day, server.NewId(), now)
		if xmax, networks := read(); xmax != full || networks != providerEgressH3PlaceNetworkCap {
			t.Fatalf("xmax %s -> %s networks=%d; a full day locked the row", full, xmax, networks)
		}
	})
}
