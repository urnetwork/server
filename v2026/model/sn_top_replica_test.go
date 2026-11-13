package model

// The estimate's current membership and wall-time hops cannot advance a signed
// fleet replica, even when an old approximate EMA has been retained.
import (
	"math/big"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestSnTopReplicaDoesNotFoldUnauthenticatedEvidence(t *testing.T) {
	key := SnTopFleetKey{FleetId: [32]byte{1}, Hotkey: [32]byte{2}, Generation: 1, Uid: 3}
	clientId, networkId := server.NewId(), server.NewId()
	prior := SnTopUnitState{Ema: 4, EmaExact: big.NewRat(4, 1), LastTempoIndex: 100}
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex:  105,
		Providers:   []SnTopProvider{{ClientId: clientId, NetworkId: networkId, TrailPrefixes: [][32]byte{{1}, {2}}}},
		Fleets:      []SnTopFleetInput{{Key: key, ClientIds: []server.Id{clientId}}},
		PriorFleets: map[SnTopFleetKey]SnTopUnitState{key: prior},
	})
	state := projection.FleetStates[key]
	if state.LastTempoIndex != prior.LastTempoIndex || state.EmaExact.Cmp(prior.EmaExact) != 0 {
		t.Fatalf("unauthenticated evidence advanced replica: %+v", state)
	}
	if projection.Fleets[0].ReplicaAvailable || projection.ClaimedCutoff != nil {
		t.Fatal("unauthenticated evidence exposed an exact replica")
	}
}
