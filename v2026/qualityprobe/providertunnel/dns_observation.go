// Fixed-size diagnostics join a DNS wave to its own tunnel's setup state.
// They neither own work nor supply evidence to the provider verdict.
package providertunnel

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
)

type dnsResult int

const (
	dnsAnswer dnsResult = iota
	dnsAuthoritativeEmpty
	dnsTimeout
	dnsUnanswered
	dnsCanceled
)

type dnsPathState int

const (
	dnsPathUnknown dnsPathState = iota
	dnsPathActive
	dnsPathForming
	dnsPathPlatformUnreachable
	dnsPathProviderUnresponsive
	dnsPathRateLimited
	dnsPathAuthFailing
	dnsPathLost
	dnsPathClosed
)

var dnsResultLabels = [...]string{"answer", "authoritative_empty", "timeout", "unanswered", "canceled"}
var dnsPathLabels = [...]string{"unknown", "active", "forming", "platform_unreachable", "provider_unresponsive", "rate_limited", "auth_failing", "lost", "closed"}

// An optional aggregate observer supplied by the embedding metrics owner.
// Recording uses only atomics: no callback, queue, goroutine, or I/O can delay
// a probe. It retains no tunnel identities, DNS state, or admission budgets.
// Safe for concurrent use; do not copy after first use.
type DnsObservations struct {
	counts      [len(dnsResultLabels)][len(dnsPathLabels)]atomic.Uint64
	routeTiming [len(dnsResultLabels)][len(dnsRouteLabels)]dnsRouteTimingCell
	contract    [len(dnsResultLabels)][len(dnsContractLabels)]dnsContractCell
}

// One fixed-cardinality row. Labels are defined here, never copied from an
// endpoint, monitor reason, identity, or error supplied by a peer.
type DnsObservation struct {
	Result string
	Path   string
	Count  uint64
}

// Reads independent monotonic counters, not an atomic cross-cell time slice.
func (self *DnsObservations) Snapshot() [len(dnsResultLabels) * len(dnsPathLabels)]DnsObservation {
	var values [len(dnsResultLabels) * len(dnsPathLabels)]DnsObservation
	index := 0
	for result, resultLabel := range dnsResultLabels {
		for path, pathLabel := range dnsPathLabels {
			value := DnsObservation{Result: resultLabel, Path: pathLabel}
			if self != nil {
				value.Count = self.counts[result][path].Load()
			}
			values[index] = value
			index++
		}
	}
	return values
}

// Invalid internal enum values cannot create another label or panic a probe.
func (self *DnsObservations) record(result dnsResult, path dnsPathState) {
	if self == nil || result < 0 || len(dnsResultLabels) <= int(result) || path < 0 || len(dnsPathLabels) <= int(path) {
		return
	}
	self.counts[result][path].Add(1)
}

// A current routing-eligible path outranks an earlier evaluation failure.
// A terminal tunnel loss outranks its monitor's last asynchronous snapshot.
func dnsPathFromMonitor(lost context.Context, window *connect.WindowExpandEvent, providers map[connect.Id]*connect.ProviderEvent) dnsPathState {
	if lost != nil && lost.Err() != nil {
		if errors.Is(context.Cause(lost), ErrTunnelClosed) {
			return dnsPathClosed
		}
		return dnsPathLost
	}
	for _, provider := range providers {
		if provider != nil && provider.State.IsActive() {
			return dnsPathActive
		}
	}
	if window == nil {
		return dnsPathUnknown
	}
	switch window.Reason {
	case connect.WindowStallPlatformUnreachable:
		return dnsPathPlatformUnreachable
	case connect.WindowStallProvidersUnresponsive:
		return dnsPathProviderUnresponsive
	case connect.WindowStallRateLimited:
		return dnsPathRateLimited
	case connect.WindowStallAuthFailing:
		return dnsPathAuthFailing
	default:
		return dnsPathForming
	}
}
