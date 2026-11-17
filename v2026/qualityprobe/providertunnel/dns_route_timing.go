// DNS timing joins private-tunnel resolver waves to source-owned route admission.
// Aggregates contain no provider identities or verdicts.
package providertunnel

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type dnsRouteClass int

const (
	dnsRouteUnknown dnsRouteClass = iota
	dnsRouteStable
	dnsRouteAdmitted
	dnsRouteUnreadyEndpoints
	dnsRouteChanged
	dnsRouteLost
	dnsRouteClosed
)

var dnsRouteLabels = [...]string{"unknown", "stable_route", "current_route_admitted", "unready_endpoints", "changed_or_ambiguous", "lost", "closed"}

// An ephemeral pair retains one local identity solely to compare continuity.
// It is never exported or retained by the process aggregate. The snapshot is
// not event history: zero active endpoints cannot rule out an intervening route.
type dnsRouteSnapshot struct {
	known      bool
	path       dnsPathState
	active     int
	clientId   connect.Id
	admittedAt time.Time
}

func dnsRouteFromMonitor(lost context.Context, window *connect.WindowExpandEvent, providers map[connect.Id]*connect.ProviderEvent) dnsRouteSnapshot {
	value := dnsRouteSnapshot{
		known: window != nil || providers != nil || lost != nil && lost.Err() != nil,
		path:  dnsPathFromMonitor(lost, window, providers),
	}
	for id, event := range providers {
		if event != nil && event.State.IsActive() {
			value.active++
			value.clientId, value.admittedAt = id, event.EventTime
		}
	}
	return value
}

// Fixed microsecond sums avoid time.Duration overflow in a long-lived process
// aggregate. Values are independent atomic cells, not an atomic histogram.
type dnsRouteTimingCell struct {
	count  atomic.Uint64
	micros [3]atomic.Uint64
}

type DnsRouteTimingObservation struct {
	Result                        string
	Route                         string
	Count                         uint64
	BeforeCurrentAdmissionSeconds float64
	AfterCurrentAdmissionSeconds  float64
	UnattributedSeconds           float64
}

func (self *DnsObservations) RouteTimingSnapshot() [len(dnsResultLabels) * len(dnsRouteLabels)]DnsRouteTimingObservation {
	var rows [len(dnsResultLabels) * len(dnsRouteLabels)]DnsRouteTimingObservation
	i := 0
	for result, resultLabel := range dnsResultLabels {
		for route, routeLabel := range dnsRouteLabels {
			row := DnsRouteTimingObservation{Result: resultLabel, Route: routeLabel}
			if self != nil {
				cell := &self.routeTiming[result][route]
				row.Count = cell.count.Load()
				row.BeforeCurrentAdmissionSeconds = float64(cell.micros[0].Load()) / 1e6
				row.AfterCurrentAdmissionSeconds = float64(cell.micros[1].Load()) / 1e6
				row.UnattributedSeconds = float64(cell.micros[2].Load()) / 1e6
			}
			rows[i] = row
			i++
		}
	}
	return rows
}

func classifyDnsRouteTiming(start, end time.Time, before, after dnsRouteSnapshot) (dnsRouteClass, [3]time.Duration) {
	var parts [3]time.Duration
	if start.IsZero() || end.Before(start) {
		return dnsRouteUnknown, parts
	}
	elapsed := end.Sub(start)
	parts[2] = elapsed
	// Closure/loss prevents stale active snapshots from claiming continuity.
	if before.path == dnsPathLost || after.path == dnsPathLost {
		return dnsRouteLost, parts
	}
	if before.path == dnsPathClosed || after.path == dnsPathClosed {
		return dnsRouteClosed, parts
	}
	if !before.known || !after.known {
		return dnsRouteUnknown, parts
	}
	if before.active == 0 && after.active == 0 {
		return dnsRouteUnreadyEndpoints, parts
	}
	if before.active == 1 && after.active == 1 && before.clientId == after.clientId &&
		!before.admittedAt.IsZero() && before.admittedAt.Equal(after.admittedAt) && !after.admittedAt.After(start) {
		return dnsRouteStable, [3]time.Duration{0, elapsed, 0}
	}
	if before.active == 0 && after.active == 1 && !after.admittedAt.IsZero() &&
		!after.admittedAt.Before(start) && !after.admittedAt.After(end) {
		// This split describes the currently observed route's timestamp. It
		// does not say no earlier route existed and disappeared in this wave.
		return dnsRouteAdmitted, [3]time.Duration{after.admittedAt.Sub(start), end.Sub(after.admittedAt), 0}
	}
	return dnsRouteChanged, parts
}

func (self *DnsObservations) recordRouteTiming(result dnsResult, start, end time.Time, before, after dnsRouteSnapshot) {
	if self == nil || result < 0 || len(dnsResultLabels) <= int(result) {
		return
	}
	route, parts := classifyDnsRouteTiming(start, end, before, after)
	cell := &self.routeTiming[result][route]
	for i, value := range parts {
		cell.micros[i].Add(uint64(value.Microseconds()))
	}
	cell.count.Add(1)
}
