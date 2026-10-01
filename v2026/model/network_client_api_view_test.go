package model

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// FindProvidersProvider.IntermediaryIds is reserved for future multi-hop
// routes and never populated; the wire omits it rather than sending null.
func TestFindProvidersProviderOmitsReservedIntermediaryIds(t *testing.T) {
	provider := findProvidersProviderFromClientScore(
		&ClientScore{
			ClientId: server.NewId(),
			Tiers:    map[string]int{RankModeQuality: 0},
		},
		RankModeQuality,
		nil,
	)
	b, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if raw, ok := wire["intermediary_ids"]; ok {
		t.Errorf("intermediary_ids should be omitted, got %s", raw)
	}
	for _, required := range []string{"client_id", "estimated_bytes_per_second", "has_estimated_bytes_per_second", "tier"} {
		if _, ok := wire[required]; !ok {
			t.Errorf("%s missing from %s", required, b)
		}
	}
}

// The resident (exchange host, service, block and internal ports) is
// platform-internal routing state and is not part of the client list view.
func TestNetworkClientInfoDoesNotExposeResident(t *testing.T) {
	b, err := json.Marshal(newNetworkClientsResult(map[server.Id]*NetworkClientInfo{
		server.NewId(): {NetworkClient: NetworkClient{ClientId: server.NewId()}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Clients []map[string]json.RawMessage `json:"clients"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Clients) != 1 {
		t.Fatalf("clients = %s", b)
	}
	for _, internal := range []string{"resident", "resident_internal_ports"} {
		if raw, ok := wire.Clients[0][internal]; ok {
			t.Errorf("%s should not be in the api view, got %s", internal, raw)
		}
	}
}
