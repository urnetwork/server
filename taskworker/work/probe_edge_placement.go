package work

import (
	"context"

	"github.com/urnetwork/server/edgeplacement"
)

// The edge placement of one probe pass (connect/MOVE.md §13): the pass's
// tunnels dial the edge the platform's load model chooses, not the one DNS
// caches hand the taskworker host. Each pass owns one, read when the pass
// starts and refreshed while it runs, and closes it when the pass ends.
func newProbeEdgePlacement(ctx context.Context) *edgeplacement.EdgePlacement {
	return edgeplacement.NewEdgePlacement(ctx, edgeplacement.DefaultEdgePlacementSettings())
}
