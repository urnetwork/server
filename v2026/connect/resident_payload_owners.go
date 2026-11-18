package connect

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Optional accounting for three exact resident payload ownership intervals.
// References may share a backing allocation with another stage or SDK owner;
// backing charges are not additive physical heap bytes. No owners are retained
// by this ledger, and collection never traverses a queue, resident, or heap.
const residentPayloadShardCount = 64

type residentPayloadLedger struct {
	shards [residentPayloadShardCount]residentPayloadShard
}

// A shard occupies whole cache lines. Owners select a fixed shard from their
// existing ID; that key never becomes a metric label or a retained reference.
type residentPayloadShard struct {
	writers  atomic.Int64
	revision atomic.Uint64
	groups   [3]residentPayloadCounters
	_        [120]byte
}

type residentPayloadStage uint8

const (
	residentPayloadControl residentPayloadStage = iota
	residentPayloadForwardIngress
	residentPayloadForwardOutput
)

type residentPayloadCharge struct{ messages, logical, backing int64 }
type residentPayloadCounters struct {
	messages, logical, backing atomic.Int64
	admitted, released         atomic.Uint64
}
type residentPayloadGroup struct {
	Messages, LogicalBytes, BackingCharge int64
	Admitted, Released                    uint64
}
type residentPayloadSnapshot struct {
	Enabled, Complete bool
	Groups            [3]residentPayloadGroup
}

// Constructed compatibility fixtures can own queues without an Exchange.
// Such owners keep the original return path with accounting disabled.
func (e *Exchange) residentPayloadLedger() *residentPayloadLedger {
	if e == nil {
		return nil
	}
	return e.payloadOwnerLedger
}

func payloadCharge(message []byte) residentPayloadCharge {
	return residentPayloadCharge{1, int64(len(message)), int64(clientconnect.MessagePoolRootByteCount(message))}
}

func controlPayloadCharge(frames []*protocol.Frame) residentPayloadCharge {
	var total residentPayloadCharge
	for _, frame := range frames {
		charge := payloadCharge(frame.MessageBytes)
		total.messages += charge.messages
		total.logical += charge.logical
		total.backing += charge.backing
	}
	return total
}

func (l *residentPayloadLedger) update(stage residentPayloadStage, owner byte, charge residentPayloadCharge, acquire bool) {
	if l == nil {
		return
	}
	shard := &l.shards[int(owner)%residentPayloadShardCount]
	shard.writers.Add(1)
	g := &shard.groups[stage]
	direction := int64(-1)
	if acquire {
		direction = 1
		g.admitted.Add(uint64(charge.messages))
	} else {
		g.released.Add(uint64(charge.messages))
	}
	g.messages.Add(direction * charge.messages)
	g.logical.Add(direction * charge.logical)
	g.backing.Add(direction * charge.backing)
	shard.revision.Add(1)
	shard.writers.Add(-1)
}

func (l *residentPayloadLedger) snapshot() residentPayloadSnapshot {
	return l.snapshotAt(nil)
}

// Collection visits each fixed shard once without retrying. Every shard must
// be coherent at its own observation interval; the sum is not one global
// atomic instant. The boundary is a deterministic local test seam only.
func (l *residentPayloadLedger) snapshotAt(boundary func(int)) residentPayloadSnapshot {
	if l == nil {
		return residentPayloadSnapshot{}
	}
	out := residentPayloadSnapshot{Enabled: true, Complete: true}
	for i := range l.shards {
		shard := &l.shards[i]
		beforeWriters, beforeRevision := shard.writers.Load(), shard.revision.Load()
		var groups [3]residentPayloadGroup
		for j := range groups {
			g := &shard.groups[j]
			groups[j] = residentPayloadGroup{g.messages.Load(), g.logical.Load(), g.backing.Load(), g.admitted.Load(), g.released.Load()}
		}
		if boundary != nil {
			boundary(i)
		}
		afterWriters, afterRevision := shard.writers.Load(), shard.revision.Load()
		if beforeWriters != 0 || afterWriters != 0 || beforeRevision != afterRevision {
			out.Complete = false
			return out
		}
		for j, g := range groups {
			if !validResidentPayloadGroup(g) {
				out.Complete = false
				return out
			}
			total := &out.Groups[j]
			total.Messages += g.Messages
			total.LogicalBytes += g.LogicalBytes
			total.BackingCharge += g.BackingCharge
			total.Admitted += g.Admitted
			total.Released += g.Released
		}
	}
	for _, g := range out.Groups {
		if !validResidentPayloadGroup(g) {
			out.Complete = false
			break
		}
	}
	return out
}

func (r *Resident) releaseControlPayload(frames []*protocol.Frame) {
	if l := r.exchange.residentPayloadLedger(); l != nil {
		l.update(residentPayloadControl, r.clientId[15], controlPayloadCharge(frames), false)
	}
	returnResidentControlFrames(frames)
}

func (r *Resident) drainControlPayloads() {
	for {
		select {
		case frames := <-r.controlIngress:
			r.releaseControlPayload(frames)
		default:
			return
		}
	}
}

func (r *Resident) releaseForwardIngress(message []byte) {
	if ledger := r.exchange.residentPayloadLedger(); ledger != nil {
		ledger.update(residentPayloadForwardIngress, r.clientId[15], payloadCharge(message), false)
	}
	clientconnect.MessagePoolReturn(message)
}

func (r *Resident) drainForwardIngress(queue <-chan residentForwardIngress) {
	for {
		select {
		case message := <-queue:
			r.releaseForwardIngress(message.transferFrameBytes)
		default:
			return
		}
	}
}

func (f *ResidentForward) releasePayload(message []byte) {
	if ledger := f.exchange.residentPayloadLedger(); ledger != nil {
		ledger.update(residentPayloadForwardOutput, f.clientId[15], payloadCharge(message), false)
	}
	clientconnect.MessagePoolReturn(message)
}

func (f *ResidentForward) drainPayloads() {
	for {
		select {
		case message := <-f.send:
			f.releasePayload(message)
		default:
			return
		}
	}
}

type residentPayloadCollector struct {
	snapshot                                                          func() residentPayloadSnapshot
	enabled, complete, messages, logical, backing, admitted, released *prometheus.Desc
}

func newResidentPayloadCollector(snapshot func() residentPayloadSnapshot) *residentPayloadCollector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("urnetwork_connect_resident_payload_"+name, help, labels, nil)
	}
	return &residentPayloadCollector{snapshot: snapshot,
		enabled:  desc("enabled", "Whether fixed resident payload ownership accounting is enabled."),
		complete: desc("sample_complete", "Whether every fixed shard observation is coherent; their sum is not a global atomic instant. False omits ownership values."),
		messages: desc("messages", "Current message references held by this stage, including an active consumer or admission offer.", "stage"),
		logical:  desc("logical_bytes", "Visible payload lengths held by this stage; excludes owner envelopes and other stages.", "stage"),
		backing:  desc("backing_charge_bytes", "Complete pooled slices charged at their class, other slices at visible length; aliases may overlap and this is not physical heap size.", "stage"),
		admitted: desc("admitted_total", "Payload references acquired by this stage.", "stage"),
		released: desc("released_total", "Payload references handed onward or returned by this stage.", "stage"),
	}
}

func (c *residentPayloadCollector) Describe(out chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.enabled, c.complete, c.messages, c.logical, c.backing, c.admitted, c.released} {
		out <- d
	}
}

func validResidentPayloadGroup(g residentPayloadGroup) bool {
	const maxExact = 1 << 53
	return 0 <= g.Messages && g.Messages <= maxExact && 0 <= g.LogicalBytes && g.LogicalBytes <= maxExact &&
		g.LogicalBytes <= g.BackingCharge && g.BackingCharge <= maxExact && g.Released <= g.Admitted &&
		g.Admitted <= maxExact && uint64(g.Messages) == g.Admitted-g.Released
}

func (c *residentPayloadCollector) Collect(out chan<- prometheus.Metric) {
	s := c.snapshot()
	valid := s.Enabled && s.Complete
	for _, g := range s.Groups {
		valid = valid && validResidentPayloadGroup(g)
	}
	boolValue := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	out <- prometheus.MustNewConstMetric(c.enabled, prometheus.GaugeValue, boolValue(s.Enabled))
	out <- prometheus.MustNewConstMetric(c.complete, prometheus.GaugeValue, boolValue(valid))
	if !valid {
		return
	}
	for i, stage := range [...]string{"control_ingress", "forward_ingress", "forward_output"} {
		g := s.Groups[i]
		out <- prometheus.MustNewConstMetric(c.messages, prometheus.GaugeValue, float64(g.Messages), stage)
		out <- prometheus.MustNewConstMetric(c.logical, prometheus.GaugeValue, float64(g.LogicalBytes), stage)
		out <- prometheus.MustNewConstMetric(c.backing, prometheus.GaugeValue, float64(g.BackingCharge), stage)
		out <- prometheus.MustNewConstMetric(c.admitted, prometheus.CounterValue, float64(g.Admitted), stage)
		out <- prometheus.MustNewConstMetric(c.released, prometheus.CounterValue, float64(g.Released), stage)
	}
}

func registerResidentPayloadMetrics(registry prometheus.Registerer, ledger *residentPayloadLedger) (func(), error) {
	if ledger == nil {
		return func() {}, nil
	}
	c := newResidentPayloadCollector(ledger.snapshot)
	if err := registry.Register(c); err != nil {
		return nil, err
	}
	return func() { registry.Unregister(c) }, nil
}
