package socketshost

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/sidecar"
)

// Read the real model through the socket block's internal client with the API's projection cold, empty and reserved.
func TestSocketBlockRequiresKnownScopeLabels(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "scope-"+networkId.String(), server.NewId())
		var labels []string
		known := false
		t.Cleanup(model.SetDnsScopeLabelSource(func(_ context.Context, id server.Id) ([]string, bool) {
			return labels, known && id == networkId
		}))
		client, bearer, _ := listenFactsClient(t, nil)
		deps := &HostDeps{}
		if err := deps.SetGateApiSources("https://api.example", bearer, client, DefaultHostSettings().ListenFactsMaxByteCount); err != nil {
			t.Fatal(err)
		}
		facts, err := deps.TlsListenFacts.ListenFacts(ctx, connect.Id(networkId))
		if facts != nil || err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("cold scope projection returned %+v, %v", facts, err)
		}
		known = true
		facts, err = deps.TlsListenFacts.ListenFacts(ctx, connect.Id(networkId))
		if err != nil || facts == nil || len(facts.ScopeLabels) != 0 {
			t.Fatalf("known empty scope set returned %+v, %v", facts, err)
		}
		labels = []string{"sales"}
		facts, err = deps.TlsListenFacts.ListenFacts(ctx, connect.Id(networkId))
		if err != nil || facts == nil || !slices.Equal(facts.ScopeLabels, labels) ||
			sidecar.ScreenHost("sales", &sidecar.ScreenFacts{ScopeLabels: facts.ScopeLabels}) != sidecar.ScreenVerdictDeny {
			t.Fatalf("reserved scope label returned %+v, %v", facts, err)
		}
	})
}
