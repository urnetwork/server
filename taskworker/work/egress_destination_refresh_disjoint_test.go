// The refresh never promotes a candidate that collides with a country list:
// the served pool would have to drop the country site, and an api that
// predates the fail-small pool would refuse the whole pool. This test uses
// only what the refresh had before the check, so it also runs, and fails,
// against it; egress_destination_refresh_hold_test.go pins the rest.
package work

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A news site failing the healthy exits opens a news promotion. The first
// news candidate in promotion order, news-a-colliding, shares its registrable
// domain with the de country site de-spiegel; news-b-clean collides with
// nothing. Returns the refresh's result and the candidates it host-checked.
func testRefreshWithACollidingCandidate(t testing.TB, ctx context.Context) (*RefreshEgressDestinationsResult, []string) {
	t.Helper()
	t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshCatalog+testRefreshCatalogSize+`
candidates:
  - {name: news-a-colliding, class: site, category: news, url: "https://www.spiegel.example/robots.txt", revision: 1}
  - {name: news-b-clean, class: site, category: news, url: "https://clean-news.example/robots.txt", revision: 1}
countries:
  de:
    - {name: de-spiegel, class: site, category: news, url: "https://spiegel.example/"}
    - {name: de-one, class: site, category: news, url: "https://one.example/"}
  fr:
    - {name: fr-one, class: site, category: news, url: "https://un.example/"}
    - {name: fr-two, class: site, category: news, url: "https://deux.example/"}
`)))

	now := server.NowUtc()
	day := now.UTC().Truncate(24 * time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		for _, row := range []struct {
			name            string
			loads, failures int
		}{
			{name: "synthetic-retiring-news", loads: 250, failures: 150},
			{name: "synthetic-steady-news", loads: 5000, failures: 0},
		} {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO provider_egress_site_tally (
					tally_day, name, country_code, region,
					load_count, failure_count, healthy_load_count, healthy_failure_count, update_time
				)
				VALUES ($1, $2, 'de', '', $3, $4, $3, $4, $5)
				`,
				day, row.name, row.loads, row.failures, now,
			))
		}
	})

	var stateLock sync.Mutex
	checked := []string{}
	check := func(_ context.Context, candidate egresshealth.Destination, _ egresshealth.RequestProfile, _ int, _ time.Duration) error {
		stateLock.Lock()
		defer stateLock.Unlock()
		checked = append(checked, candidate.Name)
		return nil
	}
	result, err := refreshEgressDestinations(ctx, now, check)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return result, checked
}

// The colliding candidate is held back before any host check and the next
// news candidate fills the opening, and the served pool keeps every country
// list.
func TestRefreshEgressDestinationsHoldsBackACandidateCollidingWithACountrySite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		result, checked := testRefreshWithACollidingCandidate(t, ctx)
		if result.Retired != 1 || result.Promoted != 1 || result.Unfilled != 0 {
			t.Errorf("refresh result = %+v, want the failing site retired and its opening filled", result)
		}
		if slices.Contains(checked, "news-a-colliding") {
			t.Errorf("host checks = %v, want the colliding candidate held back before any check", checked)
		}
		byName := testRefreshDestinationsByName(ctx)
		if colliding := byName["news-a-colliding"]; colliding == nil || colliding.Active {
			t.Errorf("news-a-colliding after the refresh = %+v, want it held back as a candidate", colliding)
		}
		if clean := byName["news-b-clean"]; clean == nil || !clean.Active || !clean.Probation {
			t.Errorf("news-b-clean after the refresh = %+v, want it promoted on probation", clean)
		}

		pool, err := controller.GetProviderEgressDestinationPool(ctx)
		if err != nil {
			t.Fatalf("the served pool is refused after the refresh: %v", err)
		}
		served := map[string]bool{}
		for _, destination := range pool.Destinations {
			served[destination.Name] = true
		}
		if served["news-a-colliding"] || !served["news-b-clean"] {
			t.Fatalf("served pool holds news-a-colliding=%t news-b-clean=%t", served["news-a-colliding"], served["news-b-clean"])
		}
		if len(pool.Countries["fr"]) != 2 || len(pool.Countries["de"]) == 0 {
			t.Fatalf("served countries = de:%d fr:%d, want both lists served", len(pool.Countries["de"]), len(pool.Countries["fr"]))
		}
		if err := egresshealth.ValidatePool(pool); err != nil {
			t.Fatalf("the prober refuses the served pool: %v", err)
		}
	})
}
