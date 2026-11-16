// One URL request is the complete unit of scheduled measurement.
package fleetprobe

import (
	"context"

	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// UrlProbeOptions shares tunnel/reporting plumbing with the legacy standalone
// prober. The scheduled entry point always forces one randomized URL attempt.
type UrlProbeOptions = FullOptions

func RunUrlProbes(ctx context.Context, providers []prober.Provider, options UrlProbeOptions) (prober.Summary, error) {
	options.UrlProbe = true
	options.AllDestinations = false
	options.LoadAttempts = 1
	return RunFull(ctx, providers, options)
}
