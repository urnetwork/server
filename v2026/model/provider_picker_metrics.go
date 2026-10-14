// Fixed picker outcomes describe rows visible to the shared SDK, not candidate
// provider counts. No request, country, target, key or error is a metric label.
package model

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type providerPickerMetricSet struct {
	outcomes                 *prometheus.CounterVec
	reads                    *prometheus.CounterVec
	children                 map[string]prometheus.Counter
	readInitial, readFilters prometheus.Counter
	phaseSeconds             *prometheus.SummaryVec
	phaseInflight            *prometheus.GaugeVec
	phases                   map[string]*providerPickerPhaseMetrics
	now                      func() time.Time
}

// Fixed, exclusive model stages. Redis stages include their wrapper PING,
// command/pipeline wait and decoding; they do not claim Redis server CPU.
var providerPickerPhases = []string{"caller_location", "initial_cache", "search_index", "location_cache", "filters", "format_result"}

type providerPickerPhaseMetrics struct {
	seconds  prometheus.Observer
	inflight prometheus.Gauge
}

// Preinitialized children allow a reader to distinguish zero from old producers.
func newProviderPickerMetricSet() *providerPickerMetricSet {
	metrics := &providerPickerMetricSet{
		outcomes:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_provider_picker_outcomes_total", Help: "Picker model outcomes by fixed request surface and rendered-row emptiness; not HTTP delivery"}, []string{"surface", "outcome"}),
		reads:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_provider_picker_read_errors_total", Help: "Non-missing Redis picker read or decode failures by fixed phase; not missing keys or all endpoint errors"}, []string{"phase"}),
		children:      map[string]prometheus.Counter{},
		phaseSeconds:  prometheus.NewSummaryVec(prometheus.SummaryOpts{Name: "urnetwork_provider_picker_phase_seconds", Help: "Exclusive picker model phase residence including errors and cancellation; not HTTP delivery or server CPU"}, []string{"surface", "phase"}),
		phaseInflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_provider_picker_phase_inflight", Help: "Picker model calls currently in each fixed exclusive phase"}, []string{"surface", "phase"}),
		phases:        map[string]*providerPickerPhaseMetrics{},
		now:           time.Now,
	}
	for _, surface := range []string{"initial", "search", "direct"} {
		for _, outcome := range []string{"nonempty", "empty", "error"} {
			metrics.children[surface+"/"+outcome] = metrics.outcomes.WithLabelValues(surface, outcome)
		}
		for _, phase := range providerPickerPhases {
			metrics.phases[surface+"/"+phase] = &providerPickerPhaseMetrics{
				seconds:  metrics.phaseSeconds.WithLabelValues(surface, phase),
				inflight: metrics.phaseInflight.WithLabelValues(surface, phase),
			}
		}
	}
	metrics.readInitial = metrics.reads.WithLabelValues("initial")
	metrics.readFilters = metrics.reads.WithLabelValues("filters")
	return metrics
}

var providerPickerMetrics = newProviderPickerMetricSet()

func init() {
	prometheus.MustRegister(providerPickerMetrics.outcomes, providerPickerMetrics.reads, providerPickerMetrics.phaseSeconds, providerPickerMetrics.phaseInflight)
}

// One request owns this state. Finishing is idempotent so deferred cleanup
// releases the final phase on a normal return, canceled read or panic.
type providerPickerObservation struct {
	metrics *providerPickerMetricSet
	surface string
	phase   *providerPickerPhaseMetrics
	started time.Time
}

func (self *providerPickerObservation) enter(phase string) {
	next := self.metrics.phases[self.surface+"/"+phase]
	if next == nil {
		return
	}
	self.finish()
	self.phase = next
	self.started = self.metrics.now()
	next.inflight.Inc()
}

func (self *providerPickerObservation) finish() {
	if self.phase == nil {
		return
	}
	self.phase.seconds.Observe(self.metrics.now().Sub(self.started).Seconds())
	self.phase.inflight.Dec()
	self.phase = nil
}

// Mirrors sdk.GetFilteredLocationsFromResult's visible collections. The API
// cannot certify HTTP delivery, a particular app version or final UI rendering.
func pickerHasRenderedRows(result *FindLocationsResult, search bool) bool {
	if result == nil {
		return false
	}
	for _, device := range result.Devices {
		if device != nil {
			return true
		}
	}
	for _, group := range result.Groups {
		if group != nil && (group.Promoted || (search && group.MatchDistance == 0)) {
			return true
		}
	}
	for _, location := range result.Locations {
		if location == nil {
			continue
		}
		if location.LocationType == LocationTypeCountry || (search && (location.MatchDistance == 0 || location.LocationType == LocationTypeCity || location.LocationType == LocationTypeRegion)) {
			return true
		}
	}
	return false
}

// A nil result (including a panic unwind) is never counted as successful empty.
func (self *providerPickerMetricSet) observe(surface string, result *FindLocationsResult, err error) {
	outcome := "empty"
	if err != nil || result == nil {
		outcome = "error"
	} else if pickerHasRenderedRows(result, surface != "initial") {
		outcome = "nonempty"
	}
	if child := self.children[surface+"/"+outcome]; child != nil {
		child.Inc()
	}
}
