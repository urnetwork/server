package work

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A graduated country site that collapses is demoted by the next refresh, and
// its outcomes are unscored again from then on; a refresh skipped for a prober
// fault demotes nothing. A site whose listed URL changed starts over on
// probation.
func TestEgressCountrySiteThatCollapsesIsDemotedByTheRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, egressCountrySiteRefreshTestCatalog))
		sites, err := controller.LoadProviderEgressSites()
		if err != nil {
			t.Fatal(err)
		}
		now := server.NowUtc().Truncate(time.Microsecond)
		day := now.UTC().Truncate(24 * time.Hour)
		equal := model.ProviderEgressCountrySiteKey{Name: "de-equal", CountryCode: "de"}
		worse := model.ProviderEgressCountrySiteKey{Name: "de-worse", CountryCode: "de"}
		listed, urls := model.ProviderEgressCountrySitesListed(sites.Countries)
		states := []*model.ProviderEgressCountrySite{}
		for _, key := range listed {
			graduated := now.Add(-20 * 24 * time.Hour)
			state := &model.ProviderEgressCountrySite{ProviderEgressCountrySiteKey: key, Url: urls[key], Scored: true, ProbationSince: now.Add(-30 * 24 * time.Hour), GraduatedTime: &graduated}
			if key == worse {
				state.Url = "https://de-worse.example/before"
			}
			states = append(states, state)
		}
		model.SetProviderEgressCountrySites(ctx, states, listed)

		// today, de-equal succeeds for none of the loads its providers'
		// general outcomes predict, in twelve networks a month old
		clientIds := []server.Id{}
		for range 12 {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			clientIds = append(clientIds, clientId)
		}
		testingEgressCountrySiteNetworks(ctx, now.Add(-30*24*time.Hour), clientIds...)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, clientId := range clientIds {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_country_site_tally
					(tally_day, name, country_code, client_id, network_id, load_count, ok_count, paired_load_count, paired_ok_count, expected_ok_sum, update_time)
					SELECT $1::date, 'de-equal', 'de', client.client_id, client.network_id, 60, 0, 60, 0, 48, $3::timestamp
					FROM network_client AS client WHERE client.client_id = $2`, day, clientId, now))
			}
		})

		demotions := testutil.ToFloat64(egressCountrySiteChangesTotal.WithLabelValues("demoted", model.ProviderEgressCountrySiteReasonCollapsed))
		if result := refreshEgressCountrySites(ctx, now, sites, true); result.demoted != 0 {
			t.Fatalf("a skipped refresh demoted %d sites", result.demoted)
		}
		if state := model.GetProviderEgressCountrySites(ctx)[equal]; state == nil || !state.Scored {
			t.Fatalf("a skipped refresh changed the state: %s", describeEgressCountrySiteTestState(state))
		}

		result := refreshEgressCountrySites(ctx, now, sites, false)
		stored := model.GetProviderEgressCountrySites(ctx)
		if state := stored[equal]; result.demoted != 1 || state == nil || state.Scored || state.DemoteCount != 1 || state.State() != "demoted" {
			t.Fatalf("collapse not demoted: %+v %s", result, describeEgressCountrySiteTestState(state))
		}
		if got := testutil.ToFloat64(egressCountrySiteChangesTotal.WithLabelValues("demoted", model.ProviderEgressCountrySiteReasonCollapsed)) - demotions; got != 1 {
			t.Fatalf("%v collapse demotions counted, want 1", got)
		}
		if state := stored[worse]; state == nil || state.Scored || state.Url != urls[worse] || state.DemoteCount != 0 {
			t.Fatalf("a site whose URL changed did not start over on probation: %s", describeEgressCountrySiteTestState(state))
		}
		if got := testutil.ToFloat64(egressCountrySitesGauge.WithLabelValues("demoted")); got != 1 {
			t.Fatalf("%v demoted sites gauged, want 1", got)
		}

		// its outcomes are unscored again
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
		submitEgressCountrySiteRefreshTestOutcome(t, ctx, clientId, server.NowUtc(), "de-equal", true)
		if counts, ok := model.GetAllProviderEgressHealthCounts(ctx)[clientId]; ok {
			t.Fatalf("a demoted site's outcome counted %d/%d", counts.OKCount, counts.Total)
		}
	})
}
