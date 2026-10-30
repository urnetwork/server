package localclient

import (
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
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
