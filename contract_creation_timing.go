package server

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// These stages partition one synchronous CreateContract call. Nested model
// stages temporarily replace their caller's stage; asynchronous post workers
// are measured only by their caller's joined post_commit span. No identifier,
// arbitrary error, SQL, credential or client-supplied label is retained.
type ContractCreationStage uint8

const (
	ContractStageOther ContractCreationStage = iota
	ContractStageRelationship
	ContractStageProvideModes
	ContractStageProvideSecret
	ContractStageMetadata
	ContractStageEndpointLookup
	ContractStageCompanionOrigin
	ContractStagePayerGate
	ContractStageTransaction
	ContractStageShardFence
	ContractStageGrantSelection
	ContractStageReservationSnapshot
	ContractStageClientFence
	ContractStagePostCommit
	ContractStageClientStamp
	ContractStageStream
	ContractStageResponse
	ContractStageCompanionPlainOriginRead
	ContractStageCompanionFallbackOriginRead
	contractStageCount
)

var contractCreationStages = [contractStageCount]string{
	"other", "relationship", "provide_modes", "provide_secret", "metadata",
	"endpoint_lookup", "companion_origin", "payer_gate", "transaction",
	"shard_fence", "grant_selection", "reservation_snapshot", "client_fence",
	"post_commit", "client_stamp", "stream", "response",
	"companion_plain_origin_read", "companion_fallback_origin_read",
}

type ContractCompanionOriginPhase uint8

const (
	ContractCompanionPlainOrigin ContractCompanionOriginPhase = iota
	ContractCompanionFallbackOrigin
	contractCompanionOriginPhaseCount
)

var contractCompanionOriginPhases = [contractCompanionOriginPhaseCount]string{"plain", "fallback"}

type ContractCompanionOriginOutcome uint8

const (
	ContractCompanionOriginFound ContractCompanionOriginOutcome = iota
	ContractCompanionOriginMissing
	ContractCompanionOriginPayerHandoff
	ContractCompanionOriginError
	contractCompanionOriginOutcomeCount
)

var contractCompanionOriginOutcomes = [contractCompanionOriginOutcomeCount]string{
	"origin_ready", "missing_origin", "payer_handoff", "error_or_panic",
}

// A generated contract reply is not proof of delivery, first provider write,
// provider response or a completed probe. Cancellation takes precedence.
type ContractCreationResult uint8

const (
	ContractCreationError ContractCreationResult = iota
	ContractCreationReply
	ContractCreationRejected
	ContractCreationCanceled
	ContractCreationPanic
	contractCreationResultCount
)

var contractCreationResults = [contractCreationResultCount]string{
	"error", "contract_reply", "protocol_reject", "canceled", "panic",
}

type contractCreationValues struct {
	seconds           [2][contractStageCount]float64
	counts            [2][contractCreationResultCount]uint64
	inflight          [2][contractStageCount]int64
	companionReads    [2][contractCompanionOriginPhaseCount]uint64
	companionOutcomes [2][contractCompanionOriginOutcomeCount]uint64
}

type contractCreationMetrics struct {
	mu                                                                    sync.Mutex
	values                                                                contractCreationValues
	now                                                                   func() time.Time
	seconds, counts, inflight, enabled, companionReads, companionOutcomes *prometheus.Desc
}

func newContractCreationMetrics() *contractCreationMetrics {
	return &contractCreationMetrics{
		now:               time.Now,
		seconds:           prometheus.NewDesc("urnetwork_contract_creation_completed_stage_seconds_total", "Exclusive wall residence of returned CreateContract calls, including cancellation and panic; no unfinished residence", []string{"ingress", "stage"}, nil),
		counts:            prometheus.NewDesc("urnetwork_contract_creation_completed_total", "Returned CreateContract calls by response disposition; contract_reply does not prove delivery or provider contact", []string{"ingress", "outcome"}, nil),
		inflight:          prometheus.NewDesc("urnetwork_contract_creation_stage_inflight", "CreateContract calls currently in each exclusive synchronous stage", []string{"ingress", "stage"}, nil),
		enabled:           prometheus.NewDesc("urnetwork_contract_creation_stage_timing_enabled", "Capability for bounded exclusive CreateContract stage timing", nil, nil),
		companionReads:    prometheus.NewDesc("urnetwork_contract_creation_companion_origin_reads_total", "Companion origin Query-through-row-drain attempts by finite phase; application wall spans do not identify PostgreSQL wait or CPU", []string{"ingress", "phase"}, nil),
		companionOutcomes: prometheus.NewDesc("urnetwork_contract_creation_companion_origin_outcomes_total", "Companion origin callback attempts by finite selection result", []string{"ingress", "outcome"}, nil),
	}
}

var defaultContractCreationMetrics = newContractCreationMetrics()

func init() { prometheus.MustRegister(defaultContractCreationMetrics) }

func (m *contractCreationMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.seconds
	ch <- m.counts
	ch <- m.inflight
	ch <- m.enabled
	ch <- m.companionReads
	ch <- m.companionOutcomes
}

func (m *contractCreationMetrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.Lock()
	values := m.values
	m.mu.Unlock()
	for ingress, label := range [2]string{"internal", "http"} {
		for stage, name := range contractCreationStages {
			ch <- prometheus.MustNewConstMetric(m.seconds, prometheus.CounterValue, values.seconds[ingress][stage], label, name)
			ch <- prometheus.MustNewConstMetric(m.inflight, prometheus.GaugeValue, float64(values.inflight[ingress][stage]), label, name)
		}
		for outcome, name := range contractCreationResults {
			ch <- prometheus.MustNewConstMetric(m.counts, prometheus.CounterValue, float64(values.counts[ingress][outcome]), label, name)
		}
		for phase, name := range contractCompanionOriginPhases {
			ch <- prometheus.MustNewConstMetric(m.companionReads, prometheus.CounterValue, float64(values.companionReads[ingress][phase]), label, name)
		}
		for outcome, name := range contractCompanionOriginOutcomes {
			ch <- prometheus.MustNewConstMetric(m.companionOutcomes, prometheus.CounterValue, float64(values.companionOutcomes[ingress][outcome]), label, name)
		}
	}
	ch <- prometheus.MustNewConstMetric(m.enabled, prometheus.GaugeValue, 1)
}

type contractCreationTimingKey struct{}

// Used only by the synchronous owning controller/model call chain. It owns no
// work, timer, cancellation, connection or background goroutine.
type ContractCreationTiming struct {
	mu       sync.Mutex
	metrics  *contractCreationMetrics
	ctx      context.Context
	ingress  int
	stage    ContractCreationStage
	last     time.Time
	seconds  [contractStageCount]float64
	finished bool
}

func BeginContractCreationTiming(ctx context.Context, httpIngress bool) (context.Context, *ContractCreationTiming) {
	return beginContractCreationTiming(ctx, httpIngress, defaultContractCreationMetrics)
}

// Parallel post callbacks retain their caller's cancellation, deadlines and
// other context values, but cannot enter the synchronous stage owner. Their
// entire joined lifetime belongs to the caller's post_commit span.
func WithoutContractCreationTiming(ctx context.Context) context.Context {
	if owner, _ := ctx.Value(contractCreationTimingKey{}).(*ContractCreationTiming); owner == nil {
		return ctx
	}
	return context.WithValue(ctx, contractCreationTimingKey{}, (*ContractCreationTiming)(nil))
}

func beginContractCreationTiming(ctx context.Context, httpIngress bool, metrics *contractCreationMetrics) (context.Context, *ContractCreationTiming) {
	owner := &ContractCreationTiming{metrics: metrics, ctx: ctx, last: metrics.now()}
	if httpIngress {
		owner.ingress = 1
	}
	metrics.mu.Lock()
	metrics.values.inflight[owner.ingress][ContractStageOther]++
	metrics.mu.Unlock()
	return context.WithValue(ctx, contractCreationTimingKey{}, owner), owner
}

func (o *ContractCreationTiming) advance(now time.Time) {
	o.seconds[o.stage] += max(0, now.Sub(o.last).Seconds())
	o.last = now
}

// Enter/leave calls must nest on the synchronous owner. Nil/uninstrumented
// contexts have no allocation or global metric effect. Deferred leave is safe
// during panic unwinding and after the owner has already finished.
func EnterContractCreationStage(ctx context.Context, stage ContractCreationStage) func() {
	o, _ := ctx.Value(contractCreationTimingKey{}).(*ContractCreationTiming)
	if o == nil || stage >= contractStageCount {
		return func() {}
	}
	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return func() {}
	}
	previous := o.stage
	o.advance(o.metrics.now())
	o.metrics.mu.Lock()
	o.metrics.values.inflight[o.ingress][previous]--
	o.metrics.values.inflight[o.ingress][stage]++
	o.metrics.mu.Unlock()
	o.stage = stage
	o.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if o.finished {
				return
			}
			o.advance(o.metrics.now())
			o.metrics.mu.Lock()
			o.metrics.values.inflight[o.ingress][o.stage]--
			o.metrics.values.inflight[o.ingress][previous]++
			o.metrics.mu.Unlock()
			o.stage = previous
		})
	}
}

// BeginContractCompanionOriginRead measures only the synchronous application
// span from Query entry through WithPgResult row drain. Call the returned leave
// function both immediately after the drain and in a defer for panic unwinding;
// EnterContractCreationStage makes that double close idempotent.
func BeginContractCompanionOriginRead(ctx context.Context, phase ContractCompanionOriginPhase) func() {
	o, _ := ctx.Value(contractCreationTimingKey{}).(*ContractCreationTiming)
	if o == nil || phase >= contractCompanionOriginPhaseCount {
		return func() {}
	}
	stage := ContractStageCompanionPlainOriginRead
	if phase == ContractCompanionFallbackOrigin {
		stage = ContractStageCompanionFallbackOriginRead
	}
	leave := EnterContractCreationStage(ctx, stage)
	o.metrics.mu.Lock()
	o.metrics.values.companionReads[o.ingress][phase]++
	o.metrics.mu.Unlock()
	return leave
}

// The callback can run again after a payer handoff; each invocation records
// one outcome without an identifier, SQL text, or caller-controlled label.
func RecordContractCompanionOriginOutcome(ctx context.Context, outcome ContractCompanionOriginOutcome) {
	o, _ := ctx.Value(contractCreationTimingKey{}).(*ContractCreationTiming)
	if o == nil || outcome >= contractCompanionOriginOutcomeCount {
		return
	}
	o.metrics.mu.Lock()
	o.metrics.values.companionOutcomes[o.ingress][outcome]++
	o.metrics.mu.Unlock()
}

func (o *ContractCreationTiming) Finish(result ContractCreationResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	o.finished = true
	o.advance(o.metrics.now())
	if o.ctx.Err() != nil {
		result = ContractCreationCanceled
	}
	if result >= contractCreationResultCount {
		result = ContractCreationError
	}
	o.metrics.mu.Lock()
	defer o.metrics.mu.Unlock()
	o.metrics.values.inflight[o.ingress][o.stage]--
	for stage, value := range o.seconds {
		o.metrics.values.seconds[o.ingress][stage] += value
	}
	o.metrics.values.counts[o.ingress][result]++
}
