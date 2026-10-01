// Fixed result boundaries distinguish completed checks, ordinary negatives
// still awaiting a cohort decision, and returned publication rows. None is a
// distinct-provider or durable replacement count; retries may count a row again.
package work

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

type providerEgressBlackholeResultClass uint8

const (
	blackholeResultOk providerEgressBlackholeResultClass = iota
	blackholeResultTls
	blackholeResultNegative
	blackholeResultNotMeasured
)

var blackholeResultLabels = [...]string{"ok", "tls_authentication_failed", "ordinary_negative", "not_measured"}

type providerEgressBlackholePublicationOutcome uint8

const (
	blackholePublicationAttempted providerEgressBlackholePublicationOutcome = iota
	blackholePublicationAcknowledged
	blackholePublicationCanceled
	blackholePublicationError
)

var blackholePublicationLabels = [...]string{"attempted", "acknowledged", "canceled", "error_or_unknown"}

type providerEgressBlackholeNegativeDisposition uint8

const (
	blackholeNegativeEligible providerEgressBlackholeNegativeDisposition = iota
	blackholeNegativeDarkGuard
	blackholeNegativeIncomplete
	blackholeNegativeReadiness
	blackholeNegativeCanceled
	blackholeNegativeOwnerExit
)

var blackholeNegativeDispositionLabels = [...]string{
	"guard_eligible", "dark_guard", "incomplete_sample", "readiness_lost", "task_canceled", "run_error_or_owner_exit",
}

// Classify the submission fields without interpreting error strings or making
// a new verdict. The ingest normalizes a passing row ahead of other flags.
func providerEgressBlackholeResultOf(check ingest.BlackholeCheck) providerEgressBlackholeResultClass {
	switch {
	case check.Ok:
		return blackholeResultOk
	case check.NotMeasured:
		return blackholeResultNotMeasured
	case check.Failure == egresshealth.FailureTlsAuthentication:
		return blackholeResultTls
	default:
		return blackholeResultNegative
	}
}

// A concurrent context cancellation must not relabel an actual nil return.
func providerEgressBlackholePublicationOf(err error) providerEgressBlackholePublicationOutcome {
	switch {
	case err == nil:
		return blackholePublicationAcknowledged
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return blackholePublicationCanceled
	default:
		return blackholePublicationError
	}
}

// No identities, payloads or owner registry are retained by the collector.
type providerEgressBlackholeResultSnapshot struct {
	completed    [4]uint64
	publications [4][4]uint64
	dispositions [6]uint64
	pending      int64
}

// One short lock protects concurrent cohort contributions and coherent scrapes.
// It never encloses a worker callback, network request or control-plane wait.
type providerEgressBlackholeResultMetrics struct {
	stateLock       sync.Mutex
	values          providerEgressBlackholeResultSnapshot
	completedDesc   *prometheus.Desc
	publicationDesc *prometheus.Desc
	dispositionDesc *prometheus.Desc
	pendingDesc     *prometheus.Desc
	enabledDesc     *prometheus.Desc
}

// Every cell is present at zero; missing capability is not a quiet cohort.
func newProviderEgressBlackholeResultMetrics() *providerEgressBlackholeResultMetrics {
	return &providerEgressBlackholeResultMetrics{
		completedDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_completed_results_total",
			"Completed check callbacks by original result before cohort guard or publication, not distinct providers or acknowledged checks", []string{"result"}, nil),
		publicationDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_publication_rows_total",
			"Payload rows at submit entry and by returned outcome; acknowledged can be replay or no-op, errors can follow a write, and no result proves current eligibility or row replacement", []string{"result", "outcome"}, nil),
		dispositionDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_negative_dispositions_total",
			"Completed ordinary-negative rows by cohort disposition; guard_eligible means eligible for submission, not acknowledged or a durable dark verdict", []string{"disposition"}, nil),
		pendingDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_negative_pending",
			"Completed ordinary negatives awaiting their owning cohort decision; excludes all safe early results including already acknowledged passes", nil, nil),
		enabledDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_result_observation_enabled",
			"Executable capability for fixed pre-guard results, cohort negative disposition and post-return publication row outcomes", nil, nil),
	}
}

var egressProbeBlackholeResults = newProviderEgressBlackholeResultMetrics()

func init() {
	prometheus.MustRegister(egressProbeBlackholeResults)
}

// Copies values before exposing them to a potentially slow metrics consumer.
func (self *providerEgressBlackholeResultMetrics) snapshot() providerEgressBlackholeResultSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.values
}

func (self *providerEgressBlackholeResultMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.completedDesc
	ch <- self.publicationDesc
	ch <- self.dispositionDesc
	ch <- self.pendingDesc
	ch <- self.enabledDesc
}

func (self *providerEgressBlackholeResultMetrics) Collect(ch chan<- prometheus.Metric) {
	values := self.snapshot()
	for result, label := range blackholeResultLabels {
		ch <- prometheus.MustNewConstMetric(self.completedDesc, prometheus.CounterValue, float64(values.completed[result]), label)
		for outcome, outcomeLabel := range blackholePublicationLabels {
			ch <- prometheus.MustNewConstMetric(self.publicationDesc, prometheus.CounterValue, float64(values.publications[result][outcome]), label, outcomeLabel)
		}
	}
	for disposition, label := range blackholeNegativeDispositionLabels {
		ch <- prometheus.MustNewConstMetric(self.dispositionDesc, prometheus.CounterValue, float64(values.dispositions[disposition]), label)
	}
	ch <- prometheus.MustNewConstMetric(self.pendingDesc, prometheus.GaugeValue, float64(values.pending))
	ch <- prometheus.MustNewConstMetric(self.enabledDesc, prometheus.GaugeValue, 1)
}

// The caller freezes the row classes at entry; the inner reporter may normalize
// a copy, but cannot change which submitted payload this outcome describes.
func (self *providerEgressBlackholeResultMetrics) publication(counts [4]uint64, outcome providerEgressBlackholePublicationOutcome) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for result, count := range counts {
		self.values.publications[result][outcome] += count
	}
}

// Each cohort owns only its ordinary-negative contribution. Workers complete
// before finalization; close is idempotent, and late callbacks cannot revive it.
type providerEgressBlackholeResultOwner struct {
	metrics *providerEgressBlackholeResultMetrics
	pending uint64
	closed  bool
}

func (self *providerEgressBlackholeResultMetrics) begin() *providerEgressBlackholeResultOwner {
	return &providerEgressBlackholeResultOwner{metrics: self}
}

// Called only for the retained OnCompleted result, never canceled/discarded
// worker work. Classification cannot add a provider negative or skip a guard.
func (self *providerEgressBlackholeResultOwner) observe(check ingest.BlackholeCheck) {
	metrics := self.metrics
	result := providerEgressBlackholeResultOf(check)
	metrics.stateLock.Lock()
	defer metrics.stateLock.Unlock()
	if self.closed {
		return
	}
	metrics.values.completed[result]++
	if result == blackholeResultNegative {
		self.pending++
		metrics.values.pending++
	}
}

// No sibling owner can release this cohort's retained negative accounting.
func (self *providerEgressBlackholeResultOwner) resolve(disposition providerEgressBlackholeNegativeDisposition) {
	self.metrics.stateLock.Lock()
	defer self.metrics.stateLock.Unlock()
	self.resolveWithLock(disposition)
}

func (self *providerEgressBlackholeResultOwner) resolveWithLock(disposition providerEgressBlackholeNegativeDisposition) {
	if self.closed {
		return
	}
	self.metrics.values.pending -= int64(self.pending)
	self.metrics.values.dispositions[disposition] += self.pending
	self.pending = 0
}

// An early return/panic with unresolved negatives is not successful guard
// release. This also bounds retention when a runner exits before its summary.
func (self *providerEgressBlackholeResultOwner) close() {
	self.metrics.stateLock.Lock()
	defer self.metrics.stateLock.Unlock()
	self.resolveWithLock(blackholeNegativeOwnerExit)
	self.closed = true
}
