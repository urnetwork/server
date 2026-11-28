// The board reads each tempo's complete trails from sn_trail_hop
// (GetSnTopTrailEvidence). A hop that recorded the zero hash counts as a hop
// and names its network, but shows no prefix, as every validator path scores
// it. Hops confirmed under the proxied-egress mark (2026-11-19 to 2026-11-21,
// reverted) recorded the zero hash. Read as a prefix, it would be one unit
// shared by every network that had such a hop in the tempo. Pure: the rows
// are built here.
package model

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestSnTopTrailEvidenceZeroHashIsNoPrefix(t *testing.T) {
	evidence := &SnTopTrailEvidence{
		Prefixes: map[server.Id][][32]byte{},
		Networks: map[server.Id]server.Id{},
	}
	seen := map[server.Id]map[[32]byte]bool{}
	slot, other := snTopTestId(1), snTopTestId(2)
	slotNetwork, otherNetwork := snTopTestId(101), snTopTestId(102)
	var zero [32]byte
	prefix := snTopTestPrefix(7)

	// the slot's hop recorded under the mark, then two after the release
	evidence.addHop(seen, slot, &slotNetwork, zero[:])
	evidence.addHop(seen, slot, &slotNetwork, prefix[:])
	evidence.addHop(seen, slot, &slotNetwork, prefix[:])
	// another network's hop recorded under the mark
	evidence.addHop(seen, other, &otherNetwork, zero[:])
	// a malformed hash is no hop
	evidence.addHop(seen, other, &otherNetwork, []byte{1, 2, 3})

	if evidence.Hops != 4 {
		t.Fatalf("%d hops, want 4: the zero-hash hops count, the malformed one does not", evidence.Hops)
	}
	if got := evidence.Prefixes[slot]; len(got) != 1 || got[0] != prefix {
		t.Fatalf("the slot shows prefixes %x, want its own prefix alone", got)
	}
	if got, ok := evidence.Prefixes[other]; ok {
		t.Fatalf("a network whose only hop recorded the zero hash shows prefixes %x", got)
	}
	if evidence.Networks[slot] != slotNetwork || evidence.Networks[other] != otherNetwork {
		t.Fatalf("networks %v, want each hop's network", evidence.Networks)
	}
}
