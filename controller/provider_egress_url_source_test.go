package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A catalog with one general destination, one candidate the test promotes
// on probation, and one country list (REVIEW-INDEPENDENT.md D3).
var providerEgressUrlSourceTestCatalog = []byte(`schema_version: 1
destinations:
  - name: general-site
    class: site
    category: reference
    url: https://general.example/
candidates:
  - name: promoted-site
    class: site
    category: reference
    url: https://promoted.example/
countries:
  de:
    - name: de-site
      class: site
      category: news
      url: https://de-news.example/
`)

// Serves providerEgressUrlSourceTestCatalog with its candidate promoted on
// probation, as the refresh leaves a promotion until the site graduates.
func pushProviderEgressUrlSourceTestCatalog(t testing.TB, ctx context.Context) {
	t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, providerEgressUrlSourceTestCatalog))
	sites, err := LoadProviderEgressSites()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if _, err := EnsureProviderEgressDestinationsSeeded(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	model.SyncProviderEgressDestinationCandidates(ctx, sites.Candidates)
	promotedTime := server.NowUtc().Add(-time.Hour)
	for _, row := range model.GetProviderEgressDestinations(ctx) {
		if row.Name == "promoted-site" {
			row.Active = true
			row.Probation = true
			row.PromotedTime = &promotedTime
			model.SetProviderEgressDestination(ctx, row)
		}
	}
	pool, err := GetProviderEgressDestinationPool(ctx)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	served := map[string]bool{}
	for _, destination := range pool.Destinations {
		served[destination.Name] = true
	}
	if !served["general-site"] || !served["promoted-site"] || len(pool.Countries["de"]) != 1 || pool.Countries["de"][0].Name != "de-site" {
		t.Fatalf("the served pool is not the test catalog: %+v %+v", pool.Destinations, pool.Countries)
	}
}

// One admitted URL turn of clientId on the named destination, submitted
// through the operator ingest exactly as the prober submits it.
func submitProviderEgressUrlSourceTestOutcome(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	cycleStartedAt time.Time,
	at time.Time,
	name string,
	url string,
	ok bool,
) {
	evidence := statsEgressTestUrlEvidence(at, ok)
	destination := egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: url}
	evidence.Destination = destination
	evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}}
	okCount, failedNames := 0, name
	if ok {
		okCount, failedNames = 1, ""
	}
	err := SubmitProviderEgressHealth(ctx, &SubmitProviderEgressHealthArgs{
		ClientId:         clientId,
		RunId:            server.NewId(),
		CycleStartedAt:   cycleStartedAt,
		UrlProbeEvidence: evidence,
		OKCount:          okCount,
		TotalCount:       1,
		ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: okCount, Total: 1}},
		FailedNames:      failedNames,
	})
	if err != nil {
		t.Fatalf("submit %s: %v", name, err)
	}
}

// A provider with an admission token, so its URL turns are admitted runs of
// the rolling quota.
func createProviderEgressUrlSourceTestCycle(t testing.TB, ctx context.Context, clientId server.Id, cycleStartedAt time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
			VALUES($1,$2,$2,true)`, clientId, cycleStartedAt.UTC()))
	})
}

// D3: a turn on a country site, or on a general site still on probation, is
// measured, recorded with its source and counted in the rolling quota, but it
// stays out of the eight-hour window every bucket and the Online rule read,
// and out of the scored loads the fleet gauges and the pool refresh read.
// Before the fix only the metric label knew the source, and both turns
// counted at once.
func TestProviderEgressUrlOutcomesOfCountryAndProbationSitesAreRecordedNotScored(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pushProviderEgressUrlSourceTestCatalog(t, ctx)
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := server.NewId()
		cycleStartedAt := now.Add(-time.Hour)
		createProviderEgressUrlSourceTestCycle(t, ctx, clientId, cycleStartedAt)

		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-3*time.Minute), "general-site", "https://general.example/", true)
		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-2*time.Minute), "promoted-site", "https://promoted.example/", false)
		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-time.Minute), "de-site", "https://de-news.example/", false)

		// the eight-hour window: the general site's success alone
		counts := model.GetAllProviderEgressHealthCounts(ctx)[clientId]
		if counts.OKCount != 1 || counts.Total != 1 {
			t.Fatalf("eight-hour window counts %d/%d, want the general site's 1/1 alone", counts.OKCount, counts.Total)
		}

		// the rolling quota: three measured runs, so recording a site unscored
		// costs no extra turns
		var runs int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provider_egress_health_history
				WHERE client_id=$1 AND url_probe AND total_count=1`, clientId).Scan(&runs))
		})
		if runs != 3 {
			t.Fatalf("%d measured runs in the rolling quota, want all 3", runs)
		}

		// the scored loads the pool refresh holds against the prober-fault line
		contacted := model.GetProviderEgressHealthContactedTotal(ctx, now.Add(-time.Hour))
		if contacted.Ok != 1 || contacted.Total != 1 || contacted.NoContactFailures != 0 {
			t.Fatalf("contacted scored loads %+v, want the general site's 1/1 alone", contacted)
		}

		// the latest-run row the fleet's class gauges sum: the country turn
		// is newest, and it adds no scored load
		classTotals := model.GetProviderEgressHealthClassTotals(ctx, now.Add(-time.Hour))
		if site := classTotals["site"]; site.Total != 0 {
			t.Fatalf("latest-run class tally %+v, want no scored load from the country turn", site)
		}
		health := model.GetProviderEgressHealth(ctx, clientId)
		if health == nil || health.FailedNames != "" || health.UnscoredFailedNames != "de-site" {
			t.Fatalf("latest run %+v, want the country site's failure among the unscored names", health)
		}

		// every outcome is recorded with its source and probation state
		type recorded struct {
			name      string
			source    int
			probation bool
		}
		got := []recorded{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT url_probe_evidence->'destination'->>'name', url_source, url_probation
				FROM provider_egress_health_history WHERE client_id=$1 ORDER BY measured_at`, clientId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var r recorded
					server.Raise(result.Scan(&r.name, &r.source, &r.probation))
					got = append(got, r)
				}
			})
		})
		want := []recorded{{"general-site", 0, false}, {"promoted-site", 0, true}, {"de-site", 1, true}}
		if len(got) != len(want) {
			t.Fatalf("recorded %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("recorded %+v, want %+v", got, want)
			}
		}
	})
}

// A promoted site counts from the moment the refresh graduates it, and only
// from then: the store reads the pool as it stands. Each stored outcome is
// counted once under its source and scoring, a replay not at all.
func TestProviderEgressUrlOutcomeScoresOnceItsSiteGraduates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pushProviderEgressUrlSourceTestCatalog(t, ctx)
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := server.NewId()
		cycleStartedAt := now.Add(-time.Hour)
		createProviderEgressUrlSourceTestCycle(t, ctx, clientId, cycleStartedAt)
		counter := func(source string, scoring string, outcome string) float64 {
			return testutil.ToFloat64(providerEgressUrlOutcomesTotal.WithLabelValues(source, scoring, outcome))
		}
		probationErrors := counter("general", "probation", "error")
		countryErrors := counter("country", "probation", "error")
		scoredSuccesses := counter("general", "scored", "success")

		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-4*time.Minute), "promoted-site", "https://promoted.example/", false)
		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-3*time.Minute), "de-site", "https://de-news.example/", false)
		if counts, ok := model.GetAllProviderEgressHealthCounts(ctx)[clientId]; ok {
			t.Fatalf("outcomes on probation counted %d/%d", counts.OKCount, counts.Total)
		}

		for _, row := range model.GetProviderEgressDestinations(ctx) {
			if row.Name == "promoted-site" {
				row.Probation = false
				model.SetProviderEgressDestination(ctx, row)
			}
		}
		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-2*time.Minute), "promoted-site", "https://promoted.example/", true)
		counts := model.GetAllProviderEgressHealthCounts(ctx)[clientId]
		if counts.OKCount != 1 || counts.Total != 1 {
			t.Fatalf("eight-hour window counts %d/%d, want the graduated site's 1/1 alone", counts.OKCount, counts.Total)
		}

		if got := counter("general", "probation", "error") - probationErrors; got != 1 {
			t.Fatalf("%v general probation errors counted, want 1", got)
		}
		if got := counter("country", "probation", "error") - countryErrors; got != 1 {
			t.Fatalf("%v country probation errors counted, want 1", got)
		}
		if got := counter("general", "scored", "success") - scoredSuccesses; got != 1 {
			t.Fatalf("%v scored general successes counted, want 1", got)
		}

		// a replay of a stored run stores and counts nothing
		evidence := statsEgressTestUrlEvidence(now.Add(-time.Minute), true)
		destination := egresshealth.Destination{Name: "general-site", Class: egresshealth.ClassSite, Url: "https://general.example/"}
		evidence.Destination = destination
		evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: evidence.MeasuredAt, TlsAuthenticated: true}}
		args := &SubmitProviderEgressHealthArgs{
			ClientId: clientId, RunId: server.NewId(), CycleStartedAt: cycleStartedAt, UrlProbeEvidence: evidence,
			OKCount: 1, TotalCount: 1, ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}},
		}
		for range 2 {
			if err := SubmitProviderEgressHealth(ctx, args); err != nil {
				t.Fatalf("submit: %v", err)
			}
		}
		if got := counter("general", "scored", "success") - scoredSuccesses; got != 2 {
			t.Fatalf("%v scored general successes counted after a replay, want 2", got)
		}
	})
}

// A security recheck of a country site with an open finding stays a
// security-only receipt (D1): out of the rolling quota as well as the window,
// and counted as a recheck, not as an outcome on probation.
func TestProviderEgressUrlRecheckOfACountrySiteStaysSecurityOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pushProviderEgressUrlSourceTestCatalog(t, ctx)
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := server.NewId()
		cycleStartedAt := now.Add(-time.Hour)
		createProviderEgressUrlSourceTestCycle(t, ctx, clientId, cycleStartedAt)
		destination := egresshealth.Destination{Name: "de-site", Class: egresshealth.ClassSite, Url: "https://de-news.example/"}
		destinationJson, err := json.Marshal(destination)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure,tls_verdict)
				VALUES($1,$2,$3::jsonb,$4,true,$5)`, clientId, egresshealth.UrlProbeDestinationKey(destination), string(destinationJson),
				now.Add(-10*time.Minute), int16(model.ProviderEgressTlsVerdictPending)))
		})
		rechecks := testutil.ToFloat64(providerEgressUrlOutcomesTotal.WithLabelValues("country", "security_recheck", "success"))
		submitProviderEgressUrlSourceTestOutcome(t, ctx, clientId, cycleStartedAt, now.Add(-time.Minute), "de-site", "https://de-news.example/", true)

		var runs, outcomes int
		var probation bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE url_probe AND total_count=1), count(*), bool_and(url_probation)
				FROM provider_egress_health_history WHERE client_id=$1`, clientId).Scan(&runs, &outcomes, &probation))
		})
		if runs != 0 || outcomes != 1 || !probation {
			t.Fatalf("recheck stored %d quota runs of %d rows (probation %t), want a security-only receipt", runs, outcomes, probation)
		}
		if got := testutil.ToFloat64(providerEgressUrlOutcomesTotal.WithLabelValues("country", "security_recheck", "success")) - rechecks; got != 1 {
			t.Fatalf("%v country rechecks counted, want 1", got)
		}
	})
}
