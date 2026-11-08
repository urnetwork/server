package work

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A country list judged on the same evidence from networks of different ages:
// four sites on probation and five scored.
var egressCountrySiteNetworkAgeTestCatalog = []byte(`schema_version: 1
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
  fr:
    - name: fr-young
      class: site
      category: news
      url: https://fr-young.example/
    - name: fr-unknown
      class: site
      category: news
      url: https://fr-unknown.example/
    - name: fr-aged
      class: site
      category: news
      url: https://fr-aged.example/
    - name: fr-gate
      class: site
      category: news
      url: https://fr-gate.example/
    - name: fr-collapse-young
      class: site
      category: news
      url: https://fr-collapse-young.example/
    - name: fr-collapse-unknown
      class: site
      category: news
      url: https://fr-collapse-unknown.example/
    - name: fr-collapse-aged
      class: site
      category: news
      url: https://fr-collapse-aged.example/
    - name: fr-tls-young
      class: site
      category: news
      url: https://fr-tls-young.example/
    - name: fr-tls-aged
      class: site
      category: news
      url: https://fr-tls-aged.example/
`)

// Only networks two weeks old are evidence for or against a country site, so
// a farm of young networks can neither graduate a site nor demote one, and
// neither can networks with no network row; networks two weeks old, with the
// same evidence, can. Before the floor, sixteen networks made the day before
// yesterday graduated a site on paired evidence they staged, demoted a scored
// one by failing it, demoted another with TLS failures common to them, and
// held a fourth back with one pending TLS finding.
func TestEgressCountrySitesYoungNetworksCanNeitherGraduateNorDemote(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, egressCountrySiteNetworkAgeTestCatalog))
		sites, err := controller.LoadProviderEgressSites()
		if err != nil {
			t.Fatal(err)
		}
		now := server.NowUtc().Truncate(time.Microsecond)
		day := now.UTC().Truncate(24 * time.Hour)
		site := func(name string) model.ProviderEgressCountrySiteKey {
			return model.ProviderEgressCountrySiteKey{Name: name, CountryCode: "fr"}
		}

		// four sites on probation for longer than the window, five scored
		listed, urls := model.ProviderEgressCountrySitesListed(sites.Countries)
		scored := map[string]bool{"fr-collapse-young": true, "fr-collapse-unknown": true, "fr-collapse-aged": true, "fr-tls-young": true, "fr-tls-aged": true}
		states := []*model.ProviderEgressCountrySite{}
		for _, key := range listed {
			state := &model.ProviderEgressCountrySite{ProviderEgressCountrySiteKey: key, Url: urls[key], ProbationSince: now.Add(-8 * 24 * time.Hour)}
			if scored[key.Name] {
				graduated := now.Add(-20 * 24 * time.Hour)
				state.Scored, state.ProbationSince, state.GraduatedTime = true, now.Add(-30*24*time.Hour), &graduated
			}
			states = append(states, state)
		}
		model.SetProviderEgressCountrySites(ctx, states, listed)

		// farms of two providers per network: sixteen networks made thirteen
		// days ago, twelve with no network row, and twelve two weeks and a
		// day old
		farm := func(networks int) [][]server.Id {
			farm := [][]server.Id{}
			for range networks {
				networkId := server.NewId()
				providers := []server.Id{}
				for range 2 {
					clientId := server.NewId()
					model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
					providers = append(providers, clientId)
				}
				farm = append(farm, providers)
			}
			return farm
		}
		clientsOf := func(farm [][]server.Id) []server.Id {
			clientIds := []server.Id{}
			for _, providers := range farm {
				clientIds = append(clientIds, providers...)
			}
			return clientIds
		}
		young, unknown, aged := farm(16), farm(12), farm(12)
		testingEgressCountrySiteNetworks(ctx, now.Add(-13*24*time.Hour), clientsOf(young)...)
		testingEgressCountrySiteNetworks(ctx, now.Add(-15*24*time.Hour), clientsOf(aged)...)

		// today's pair tally: as good as the general catalog for the same
		// providers (alternate networks a success short), or failing every
		// load where the general outcomes predict 48 of 60 per network
		tally := func(name string, farm [][]server.Id, collapsed bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				for i, providers := range farm {
					for j, clientId := range providers {
						ok := 24
						if collapsed {
							ok = 0
						} else if i%2 == 1 && j == 0 {
							ok = 23
						}
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_country_site_tally
							(tally_day, name, country_code, client_id, load_count, ok_count, paired_load_count, paired_ok_count, expected_ok_sum, update_time)
							VALUES ($1, $2, 'fr', $3, 30, $4, 30, $4, 24, $5)`, day, name, clientId, ok, now))
					}
				}
			})
		}
		tally("fr-young", young, false)
		tally("fr-unknown", unknown, false)
		tally("fr-aged", aged, false)
		tally("fr-gate", aged, false)
		tally("fr-collapse-young", young, true)
		tally("fr-collapse-unknown", unknown, true)
		tally("fr-collapse-aged", aged, true)

		// TLS findings an hour ago: one provider in each young network failed
		// fr-tls-young, which the TLS judge calls a site fault (sixteen
		// networks in an hour); one provider of the aged farm failed
		// fr-tls-aged, judged a site fault; and one young provider's failure
		// on fr-gate is pending
		finding := func(clientId server.Id, name string, verdict model.ProviderEgressTlsVerdict) {
			destination := egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: "https://" + name + ".example/"}
			destinationJson, err := json.Marshal(destination)
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security
					(client_id, url_key, destination, measured_at, tls_failure, tls_verdict) VALUES ($1, $2, $3::jsonb, $4, true, $5)`,
					clientId, egresshealth.UrlProbeDestinationKey(destination), string(destinationJson), now.Add(-time.Hour), int16(verdict)))
			})
		}
		for _, providers := range young {
			finding(providers[0], "fr-tls-young", model.ProviderEgressTlsVerdictSite)
		}
		finding(aged[0][0], "fr-tls-aged", model.ProviderEgressTlsVerdictSite)
		finding(young[0][1], "fr-gate", model.ProviderEgressTlsVerdictPending)

		check := func(phase string, want map[string]string) {
			stored := model.GetProviderEgressCountrySites(ctx)
			for name, state := range want {
				if got := stored[site(name)]; got == nil || got.State() != state {
					t.Errorf("%s: %s, want %s", phase, describeEgressCountrySiteTestState(got), state)
				}
			}
		}

		result := refreshEgressCountrySites(ctx, now, sites, false)
		check("judged on young and aged farms", map[string]string{
			"fr-young":            "probation",
			"fr-unknown":          "probation",
			"fr-aged":             "scored",
			"fr-gate":             "scored",
			"fr-collapse-young":   "scored",
			"fr-collapse-unknown": "scored",
			"fr-collapse-aged":    "demoted",
			"fr-tls-young":        "scored",
			"fr-tls-aged":         "demoted",
		})
		if result.graduated != 2 || result.demoted != 2 {
			t.Errorf("judged on young and aged farms: %+v, want 2 graduated (fr-aged, fr-gate) and 2 demoted (fr-collapse-aged, fr-tls-aged)", result)
		}
		stored := model.GetProviderEgressCountrySites(ctx)
		if state := stored[site("fr-young")]; state == nil || state.NetworkCount != 0 || !strings.Contains(state.Judgement, "too few samples") {
			t.Errorf("the young farm's site: %s, want no networks counted", describeEgressCountrySiteTestState(state))
		}
		if state := stored[site("fr-aged")]; state == nil || state.NetworkCount != 12 || state.ProviderCount != 24 {
			t.Errorf("the aged farm's site: %s, want its 12 networks and 24 providers", describeEgressCountrySiteTestState(state))
		}

		// the same young farm, judged once its networks are two weeks old,
		// graduates and demotes as the aged farm did; the networks with no
		// network row still count for nothing
		testingEgressCountrySiteNetworks(ctx, now.Add(-15*24*time.Hour), clientsOf(young)...)
		result = refreshEgressCountrySites(ctx, now, sites, false)
		check("judged once the young farm is two weeks old", map[string]string{
			"fr-young":            "scored",
			"fr-unknown":          "probation",
			"fr-collapse-young":   "demoted",
			"fr-collapse-unknown": "scored",
			"fr-tls-young":        "demoted",
		})
		if result.graduated != 1 || result.demoted != 2 {
			t.Errorf("judged once the young farm is two weeks old: %+v, want 1 graduated (fr-young) and 2 demoted (fr-collapse-young, fr-tls-young)", result)
		}
	})
}
