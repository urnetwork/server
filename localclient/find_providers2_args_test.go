package localclient

import (
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
)

// Hosted discovery hands the generator's request to the model through JSON
// (convert), so the capability filters a hosted device's profile sets reach
// FindProviders2 unchanged (FP2-MODERN.md), and an unset one stays unset.
func TestFindProviders2ArgsConvertCarriesTheCapabilityFilters(t *testing.T) {
	for _, c := range []struct {
		requireModernProvider bool
		postQuantumEncryption bool
	}{
		{},
		{requireModernProvider: true},
		{postQuantumEncryption: true},
		{requireModernProvider: true, postQuantumEncryption: true},
	} {
		request, err := convert[model.FindProviders2Args](&connect.FindProviders2Args{
			RankMode:              "speed",
			RequireModernProvider: c.requireModernProvider,
			PostQuantumEncryption: c.postQuantumEncryption,
		})
		if err != nil {
			t.Fatal(err)
		}
		if request.RequireModernProvider != c.requireModernProvider || request.PostQuantumEncryption != c.postQuantumEncryption || request.RankMode != "speed" {
			t.Errorf("converted %+v, want require_modern_provider %t and post_quantum_encryption %t", request, c.requireModernProvider, c.postQuantumEncryption)
		}
	}
}

// Hosted discovery hands the request and the answer through JSON (convert),
// so a strict request reaches FindProviders2 strict, and a provider the
// location fallback found above the requested city or region keeps its match
// while a provider of the requested target stays without one.
func TestFindProviders2ConvertCarriesTheLocationFallback(t *testing.T) {
	for _, strict := range []bool{false, true} {
		request, err := convert[model.FindProviders2Args](&connect.FindProviders2Args{Strict: strict})
		if err != nil {
			t.Fatal(err)
		}
		if request.Strict != strict {
			t.Errorf("strict %t converted to %t", strict, request.Strict)
		}
	}
	climbedClientId, exactClientId := server.NewId(), server.NewId()
	result, err := convert[connect.FindProviders2Result](&model.FindProviders2Result{
		Providers: []*model.FindProvidersProvider{
			{ClientId: climbedClientId, Match: model.LocationTypeRegion},
			{ClientId: exactClientId},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 2 || result.Providers[0].Match != "region" || result.Providers[1].Match != "" {
		t.Fatalf("converted %d providers without their matches", len(result.Providers))
	}
}
