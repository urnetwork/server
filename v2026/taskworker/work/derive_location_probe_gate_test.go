package work

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The probe-country gate of connect/GEOMAP.md §5.4 as amended by §10.3: a
// derived place in a country other than the one a fresh egress probe watched
// the provider's exit leave from is not published, and the node keeps its
// genesis.

// On the synthetic graph the "country" provider's genesis is Middle, in ga,
// and its pings place it at Northgate, in cm. A fresh probe observing ga
// refuses the crossing; one observing cm lets it through, since the derived
// place then agrees with where the traffic exits. Pure.
func TestPlanDerivationRefusesACrossingAgainstAFreshProbe(t *testing.T) {
	settings := deriveTestSettings()
	settings.LambdaRegion = 0
	settings.LambdaCountry = 0

	for _, test := range []struct {
		probeCountryCode string
		published        bool
	}{
		{probeCountryCode: "", published: true},
		{probeCountryCode: "ga", published: false},
		{probeCountryCode: "cm", published: true},
	} {
		graph := newDeriveTestGraph(t, settings)
		nodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, graph.providers["country"])
		graph.geneses[nodeId].probeCountryCode = test.probeCountryCode
		inputs := graph.inputs(8)
		plan := planDerivation(inputs, graph.geneses, map[string]*model.DerivedLocation{}, graph.places, settings)

		publication := derivePublicationFor(plan, nodeId)
		connect.AssertEqual(t, publication != nil, test.published)
		summary := plan.summary(inputs)
		if test.published {
			connect.AssertEqual(t, summary.RefusedProbeCountry, 0)
			connect.AssertEqual(t, publication.mapping.place.City, "Northgate")
			continue
		}
		connect.AssertEqual(t, summary.RefusedProbeCountry, 1)
		connect.AssertEqual(t, len(plan.probeCountryRefusals), 1)
		refusal := plan.probeCountryRefusals[0]
		connect.AssertEqual(t, refusal.nodeId, nodeId)
		connect.AssertEqual(t, refusal.place.CountryCode, "cm")
		connect.AssertEqual(t, refusal.probeCountryCode, "ga")

		// the provider that crossed only a region is not touched
		regionNodeId := deriveNodeId(model.DerivedLocationNodeKindProvider, graph.providers["region"])
		if derivePublicationFor(plan, regionNodeId) == nil {
			t.Fatal("the gate refused a provider no probe contradicts")
		}
	}
}

// The job on the seeded graph with a fresh probe of the "country" provider
// that observed its exit in ga: the crossing into cm is not published, the
// provider keeps its genesis, and the refusal is counted.
func TestDeriveLocationsRefusesACrossingAgainstAFreshProbe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_PushPlaces(deriveTestPlaces)()
		places := model.CurrentPlaces()
		if places == nil {
			t.Fatal("the pushed place list is not current")
		}

		settings := deriveTestSettings()
		settings.LambdaRegion = 0
		settings.LambdaCountry = 0
		graph := newDeriveTestGraph(t, settings)
		graph.places = places
		now := server.NowUtc()
		seedDeriveTestGraph(t, ctx, graph, 8, now)

		// the probe's location row is not one the job can read, so the
		// genesis stays the connection's lookup, and only the probe's country
		// speaks
		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    graph.providers["country"],
			LocationId:  server.NewId(),
			CountryCode: "ga",
			ObservedAt:  now,
		})

		result, err := deriveLocations(ctx, places, settings, DefaultDeriveLocationsSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%+v", result)
		connect.AssertEqual(t, result.RefusedProbeCountry, 1)
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, graph.providers["country"]) != nil {
			t.Fatal("a crossing against a fresh probe's country was published")
		}
		if model.GetDerivedLocation(ctx, model.DerivedLocationNodeKindProvider, graph.providers["region"]) == nil {
			t.Fatal("the gate withheld a provider no probe contradicts")
		}
		connect.AssertEqual(t, result.CrossedCountry, 0)
	})
}
