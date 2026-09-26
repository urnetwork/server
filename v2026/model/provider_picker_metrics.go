// Fixed picker outcomes describe rows visible to the shared SDK, not candidate
// provider counts. No request, country, target, key or error is a metric label.
package model

import "github.com/prometheus/client_golang/prometheus"

type providerPickerMetricSet struct {
	outcomes                 *prometheus.CounterVec
	reads                    *prometheus.CounterVec
	children                 map[string]prometheus.Counter
	readInitial, readFilters prometheus.Counter
}

// Preinitialized children allow a reader to distinguish zero from old producers.
func newProviderPickerMetricSet() *providerPickerMetricSet {
	metrics := &providerPickerMetricSet{
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_provider_picker_outcomes_total", Help: "Picker model outcomes by fixed request surface and rendered-row emptiness; not HTTP delivery"}, []string{"surface", "outcome"}),
		reads:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "urnetwork_provider_picker_read_errors_total", Help: "Non-missing Redis picker read or decode failures by fixed phase; not missing keys or all endpoint errors"}, []string{"phase"}),
		children: map[string]prometheus.Counter{},
	}
	for _, surface := range []string{"initial", "search", "direct"} {
		for _, outcome := range []string{"nonempty", "empty", "error"} {
			metrics.children[surface+"/"+outcome] = metrics.outcomes.WithLabelValues(surface, outcome)
		}
	}
	metrics.readInitial = metrics.reads.WithLabelValues("initial")
	metrics.readFilters = metrics.reads.WithLabelValues("filters")
	return metrics
}

var providerPickerMetrics = newProviderPickerMetricSet()

func init() { prometheus.MustRegister(providerPickerMetrics.outcomes, providerPickerMetrics.reads) }

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
