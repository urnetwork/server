package acceptance

import (
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"time"
)

type wireGuardDNSReadinessKey struct{}

const wireGuardDNSHistoryCap = 128
const wireGuardDNSReadinessTextCap = 4096

type wireGuardDNSHistory struct {
	flow    wireGuardDNSFlow
	request uint64
}

type wireGuardDNSCounts struct {
	exchanges, packets, matched, closed, priorActive, priorClosed, ambiguous, uncompared uint64
	io                                                                                   [7]uint64
	lookup                                                                               [6]uint64
	mismatch                                                                             [16]uint64
	limited                                                                              bool
}

type wireGuardDNSReadinessSample struct {
	request             uint64
	startMS, durationMS int64
	counts              wireGuardDNSCounts
}

// Tuples never leave this readiness-scoped object. They are retained only so
// a later request can distinguish a known closed socket from an unobserved
// tuple. Freeze clears them, and caps fail closed with explicit limit flags.
// Frozen samples contain only finite classes/counters, never addresses, ports,
// DNS payloads/IDs, credentials or arbitrary error strings.
type wireGuardDNSReadiness struct {
	mu                  sync.Mutex
	started             time.Time
	requests, records   uint64
	flows               [wireGuardDNSHistoryCap]wireGuardDNSHistory
	flowN               int
	flowLimited, frozen bool
	counts              wireGuardDNSCounts
	samples             [8]wireGuardDNSReadinessSample
	sampleN             int
	text                string
}

func (w *wireGuardDNSReadiness) begin() (uint64, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.frozen {
		return 0, time.Time{}
	}
	now := time.Now()
	if w.started.IsZero() {
		w.started = now
	}
	w.requests++
	return w.requests, now
}

func (w *wireGuardDNSReadiness) register(request uint64, flow wireGuardDNSFlow) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.frozen || request == 0 {
		return -1
	}
	if w.flowN == len(w.flows) {
		w.flowLimited = true
		return -1
	}
	index := w.flowN
	w.flows[index] = wireGuardDNSHistory{flow: wireGuardDNSFlow{local: flow.local, remote: flow.remote, active: true}, request: request}
	w.flowN++
	return index
}

func (w *wireGuardDNSReadiness) closeFlow(index int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.frozen && 0 <= index && index < w.flowN {
		w.flows[index].flow.active = false
	}
}

func (w *wireGuardDNSReadiness) priorTuple(request uint64, flow wireGuardDNSFlow) uint8 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var result uint8
	for _, prior := range w.flows[:w.flowN] {
		if prior.request == request || dnsTupleDifference(prior.flow, flow) != 0 {
			continue
		}
		if prior.flow.active {
			result |= 1
		} else {
			result |= 2
		}
	}
	return result
}

func dnsRequestCounts(r *wireGuardDNSRequest) wireGuardDNSCounts {
	c := wireGuardDNSCounts{exchanges: uint64(r.flowN), packets: uint64(r.packetN), limited: r.limited}
	for i, value := range r.io {
		c.io[i] = uint64(value)
	}
	for i, value := range r.lookup {
		c.lookup[i] = uint64(value)
	}
	for _, p := range r.packets[:r.packetN] {
		if p.match != 0 {
			c.matched++
		}
		if p.closedCurrent {
			c.closed++
		}
		if p.prior&1 != 0 {
			c.priorActive++
		}
		if p.prior&2 != 0 {
			c.priorClosed++
		}
		switch bits.OnesCount16(p.closestActive) {
		case 0:
			c.uncompared++
		case 1:
			c.mismatch[bits.TrailingZeros16(p.closestActive)]++
		default:
			c.ambiguous++
		}
	}
	return c
}

func (c *wireGuardDNSCounts) add(other wireGuardDNSCounts) {
	c.exchanges += other.exchanges
	c.packets += other.packets
	c.matched += other.matched
	c.closed += other.closed
	c.priorActive += other.priorActive
	c.priorClosed += other.priorClosed
	c.ambiguous += other.ambiguous
	c.uncompared += other.uncompared
	c.limited = c.limited || other.limited
	for i, value := range other.io {
		c.io[i] += value
	}
	for i, value := range other.lookup {
		c.lookup[i] += value
	}
	for i, value := range other.mismatch {
		c.mismatch[i] += value
	}
}

func (c wireGuardDNSCounts) summary() string {
	var mismatch []string
	for mask, count := range c.mismatch {
		if count != 0 {
			mismatch = append(mismatch, fmt.Sprintf("%s:%d", dnsTupleFields(uint8(mask)), count))
		}
	}
	return fmt.Sprintf("exchanges=%d sampled_packets=%d matched=%d closed_current=%d prior_active=%d prior_closed=%d closest_active_fields=[%s] ambiguous=%d uncompared=%d io(read/data/bytes/readerr/write/bytes/writeerr)=%d/%d/%d/%d/%d/%d/%d lookup(start/ok/notfound/deadline/cancel/error)=%d/%d/%d/%d/%d/%d limited=%t",
		c.exchanges, c.packets, c.matched, c.closed, c.priorActive, c.priorClosed, strings.Join(mismatch, ","), c.ambiguous, c.uncompared,
		c.io[0], c.io[1], c.io[2], c.io[5], c.io[3], c.io[4], c.io[6], c.lookup[0], c.lookup[1], c.lookup[2], c.lookup[3], c.lookup[4], c.lookup[5], c.limited)
}

// Called only by the request's freezeOnce while its mutex is held. The window
// never takes a request/stack lock, so stack -> request -> window stays acyclic.
func (w *wireGuardDNSReadiness) record(r *wireGuardDNSRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.frozen || r.windowRequest == 0 {
		return
	}
	counts := dnsRequestCounts(r)
	w.records++
	w.counts.add(counts)
	sample := wireGuardDNSReadinessSample{request: r.windowRequest, startMS: max(0, r.started.Sub(w.started).Milliseconds()), durationMS: max(0, time.Since(r.started).Milliseconds()), counts: counts}
	if w.sampleN < len(w.samples) {
		w.samples[w.sampleN] = sample
		w.sampleN++
	} else {
		// Preserve the first four plus the most recent four completions.
		copy(w.samples[4:7], w.samples[5:8])
		w.samples[7] = sample
	}
}

func (w *wireGuardDNSReadiness) freeze() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.frozen {
		return w.text
	}
	w.frozen = true
	clear(w.flows[:])
	w.flowN = 0
	if w.records == 0 {
		return ""
	}
	var samples []string
	for _, s := range w.samples[:w.sampleN] {
		samples = append(samples, fmt.Sprintf("a%d{start_ms=%d duration_ms=%d %s}", s.request, s.startMS, s.durationMS, s.counts.summary()))
	}
	for {
		w.text = fmt.Sprintf("WireGuard DNS readiness window frozen=true packet_scope=request_window_not_ownership checksums=unchecked generation=unproven provider=unavailable started=%d frozen_attempts=%d omitted_samples=%d flow_limit=%t text_limit=%t aggregate={%s} samples=[%s]",
			w.requests, w.records, w.records-uint64(len(samples)), w.flowLimited, len(samples) != w.sampleN, w.counts.summary(), strings.Join(samples, ","))
		if len(w.text) <= wireGuardDNSReadinessTextCap || len(samples) == 0 {
			return w.text
		}
		// Drop a whole middle sample; never truncate a classification or hide
		// the omission. Aggregate counters still include every frozen attempt.
		i := len(samples) / 2
		samples = append(samples[:i], samples[i+1:]...)
	}
}

func (w *wireGuardDNSReadiness) failure(err error) error {
	if summary := w.freeze(); summary != "" {
		return fmt.Errorf("%w; %s", err, summary)
	}
	return err
}
