package work

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// A catalog with two general destinations and one country list of four
// sites: one as good as the general catalog, one worse, one measured by too
// few networks, and one failing TLS.
var egressCountrySiteRefreshTestCatalog = []byte(`schema_version: 1
destinations:
  - name: general-one
    class: site
    category: reference
    url: https://general-one.example/
  - name: general-two
    class: site
    category: news
    url: https://general-two.example/
countries:
  de:
    - name: de-equal
      class: site
      category: news
      url: https://de-equal.example/
    - name: de-worse
      class: site
      category: news
      url: https://de-worse.example/
    - name: de-thin
      class: site
      category: news
      url: https://de-thin.example/
    - name: de-tls
      class: site
      category: news
      url: https://de-tls.example/
`)

// Gives the networks of clientIds a network row created at createdAt: the
// test providers' networks have none, and country site evidence counts only
// networks two weeks old (model.ProviderEgressCountrySiteMinNetworkAge).
func testingEgressCountrySiteNetworks(ctx context.Context, createdAt time.Time, clientIds ...server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO network (network_id, network_name, admin_user_id, create_time)
			SELECT DISTINCT client.network_id, 'country-site-test-' || client.network_id::text, client.network_id, $1::timestamp
			FROM network_client AS client WHERE client.client_id = ANY($2)
			ON CONFLICT (network_id) DO UPDATE SET create_time = EXCLUDED.create_time`,
			createdAt.UTC(), clientIds))
	})
}

// A stored state for a failure message: the state embeds its key, whose
// String would otherwise stand for all of it.
func describeEgressCountrySiteTestState(state *model.ProviderEgressCountrySite) string {
	if state == nil {
		return "no state"
	}
	return fmt.Sprintf("%s %s (%s, demoted %d times, %d networks): %s",
		state.ProviderEgressCountrySiteKey, state.State(), state.Url, state.DemoteCount, state.NetworkCount, state.Judgement)
}

// One URL outcome of clientId on the named destination, submitted through the
// operator ingest exactly as the prober submits it.
func submitEgressCountrySiteRefreshTestOutcome(t testing.TB, ctx context.Context, clientId server.Id, at time.Time, name string, ok bool) {
	destination := egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: "https://" + name + ".example/"}
	evidence := &egresshealth.UrlProbeEvidence{
		PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
		Destination: destination, MeasuredAt: at, ContentMatcherVersion: 1,
		Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}},
		StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
		RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
		ContentClassification: "content", PerformanceClassification: "insufficient_sample",
	}
	okCount, failedNames := 1, ""
	if !ok {
		evidence.ContentClassification = "captcha"
		evidence.PerformanceClassification = ""
		evidence.FailureStage = "response_content"
		okCount, failedNames = 0, name
	}
	err := controller.SubmitProviderEgressHealth(ctx, &controller.SubmitProviderEgressHealthArgs{
		ClientId:         clientId,
		RunId:            server.NewId(),
		UrlProbeEvidence: evidence,
		OKCount:          okCount,
		TotalCount:       1,
		ClassResults:     map[string]controller.ProviderEgressHealthClassResult{"site": {OK: okCount, Total: 1}},
		FailedNames:      failedNames,
	})
	if err != nil {
		t.Fatalf("submit %s: %v", name, err)
	}
}

// Country sites graduate on evidence, with no list to maintain: a week after
// the refresh first saw the country list, a site that did as well as the
// general catalog for the same providers, measured by enough networks two
// weeks old, scores; a worse site, a site too few networks measured and a
// site failing TLS stay on probation, recorded and unscored. Before automatic
// graduation no country site ever scored.
func TestEgressCountrySitesGraduateOnPairedEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, egressCountrySiteRefreshTestCatalog))
		now := server.NowUtc().Truncate(time.Microsecond)

		// the refresh first sees the country list a week ago
		if _, err := refreshEgressDestinations(ctx, now.Add(-7*24*time.Hour-time.Minute), testRefreshNoCandidateCheck(t)); err != nil {
			t.Fatalf("first refresh: %v", err)
		}

		// ten networks a month old, of two providers each: every general
		// outcome passes, so a site as good as the general catalog passes too
		const networks, providersPerNetwork, loadsPerSite = 10, 2, 26
		at := now.Add(-50 * time.Minute)
		next := func() time.Time {
			at = at.Add(50 * time.Millisecond)
			return at
		}
		clientIds := []server.Id{}
		for network := range networks {
			networkId := server.NewId()
			for range providersPerNetwork {
				clientId := server.NewId()
				model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
				clientIds = append(clientIds, clientId)
				for range 5 {
					submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, next(), "general-one", true)
				}
				for load := range loadsPerSite {
					submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, next(), "de-equal", true)
					submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, next(), "de-worse", load%2 == 0)
					submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, next(), "de-tls", true)
					if network < 4 {
						submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, next(), "de-thin", true)
					}
				}
			}
		}
		testingEgressCountrySiteNetworks(ctx, now.Add(-30*24*time.Hour), clientIds...)
		// one provider's latest turn on de-tls failed TLS, not yet judged
		tlsDestination := egresshealth.Destination{Name: "de-tls", Class: egresshealth.ClassSite, Url: "https://de-tls.example/"}
		tlsDestinationJson, err := json.Marshal(tlsDestination)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure)
				VALUES($1,$2,$3::jsonb,$4,true)
				ON CONFLICT (client_id,url_key) DO UPDATE SET destination=EXCLUDED.destination, measured_at=EXCLUDED.measured_at,
					tls_failure=true, tls_verdict=0, tls_verdict_measured_at=NULL`,
				clientIds[0], egresshealth.UrlProbeDestinationKey(tlsDestination), string(tlsDestinationJson), next()))
		})

		// a week after the first refresh
		if _, err := refreshEgressDestinations(ctx, now, testRefreshNoCandidateCheck(t)); err != nil {
			t.Fatalf("second refresh: %v", err)
		}

		// a new outcome on each site, each by a provider of its own: only the
		// site that graduated counts in the eight-hour window
		for _, c := range []struct {
			name   string
			scored bool
		}{
			{"de-equal", true},
			{"de-worse", false},
			{"de-thin", false},
			{"de-tls", false},
		} {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, server.NowUtc(), c.name, true)
			counts, scored := model.GetAllProviderEgressHealthCounts(ctx)[clientId]
			if scored != c.scored || (scored && counts.Total != 1) {
				t.Errorf("%s: a new outcome scored %t (%d/%d), want %t", c.name, scored, counts.OKCount, counts.Total, c.scored)
			}
		}
	})
}
