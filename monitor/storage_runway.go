// The max-of-windows storage runway estimator of SIGNALS.md §2.4f, shared by
// storage-runway (§2.4f) and minio-runway (§11.6b). Each signal reads node
// filesystem history through one bounded loopback Mimir range query and judges
// the estimate with its own thresholds. This file owns the range-answer
// validation, the estimator, and the memory that lets an unknown runway keep
// the severity of the decline it cannot rule out (SIGNALS.md §7, 2026-10-22
// emission rule 1). Package functions are safe for concurrent use; the risk
// memory documents its own locking.
package monitor

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The estimator's tunables. Each runway signal carries its own copy.
type StorageRunwayEstimatorSettings struct {
	// Decline windows, each ending at the newest sample. The fastest decline
	// over any window is the rate, so an acceleration shows at once while a
	// cleanup can only shorten the windows it falls in.
	Windows []time.Duration
	// Resolution of the bounded range query.
	Step time.Duration
	// The newest evaluated grid point is this far behind now: the newest Mimir
	// samples are partial while scrapes catch up (RUN-MAIN.md, tier-0 vitals).
	QueryLag time.Duration
	// The newest sample may be at most this old at the newest grid point.
	// Zero requires the newest grid point itself, which Mimir fills only from
	// a raw sample inside its own lookback.
	Freshness time.Duration
	// The sample nearest a window endpoint stands in for it within this
	// distance. Declines divide by the actual elapsed time.
	EndpointTolerance time.Duration
	// The pace floor's decline window and the history it is taken over.
	PaceWindow  time.Duration
	PaceHistory time.Duration
	// An unknown runway on a volume whose decline over UnknownRiskWindow
	// exceeded UnknownPageDeclineBytesPerHour pages; otherwise it warns.
	UnknownRiskWindow              time.Duration
	UnknownPageDeclineBytesPerHour float64
	// Bounds on the Mimir answer: bytes and series.
	MaxResponseBytes int
	MaxSeries        int
	// Bounds the curl request; the signal budget bounds the whole run.
	QueryTimeout time.Duration
}

// The §2.4f defaults: 1, 6 and 24 hour windows over seven days of 15-minute
// history, and emission rule 1's 5 GB/h over the last six hours.
func DefaultStorageRunwayEstimatorSettings() StorageRunwayEstimatorSettings {
	return StorageRunwayEstimatorSettings{
		Windows:                        []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour},
		Step:                           15 * time.Minute,
		QueryLag:                       2 * time.Minute,
		Freshness:                      0,
		EndpointTolerance:              15 * time.Minute,
		PaceWindow:                     24 * time.Hour,
		PaceHistory:                    7 * 24 * time.Hour,
		UnknownRiskWindow:              6 * time.Hour,
		UnknownPageDeclineBytesPerHour: 5 * 1000 * 1000 * 1000,
		MaxResponseBytes:               4 * 1024 * 1024,
		MaxSeries:                      256,
		QueryTimeout:                   20 * time.Second,
	}
}

// The grid of one range query: start and end are whole seconds on the step.
type storageRunwayGrid struct {
	start time.Time
	end   time.Time
	step  time.Duration
}

// The query must reach back over the pace history plus its window, and over
// every decline window. Starting on a whole number of steps before the end
// keeps the newest grid point at the end itself.
func (self StorageRunwayEstimatorSettings) grid(now time.Time) storageRunwayGrid {
	end := now.UTC().Add(-self.QueryLag).Truncate(time.Second)
	span := self.PaceHistory + self.PaceWindow
	for _, window := range self.Windows {
		span = max(span, window+self.EndpointTolerance)
	}
	span = max(span, self.UnknownRiskWindow+self.EndpointTolerance)
	step := max(self.Step.Truncate(time.Second), time.Second)
	steps := int64((span + step - 1) / step)
	return storageRunwayGrid{start: end.Add(-time.Duration(steps) * step), end: end, step: step}
}

// One loopback query_range request. Raw selectors keep their own labels; the
// answer is bounded in bytes before it is parsed.
func storageRunwayRangeCommand(marker, query string, grid storageRunwayGrid, settings StorageRunwayEstimatorSettings) string {
	timeoutSeconds := max(int(settings.QueryTimeout/time.Second), 1)
	return "# " + marker + "\n" +
		"curl -fsS --max-time " + strconv.Itoa(timeoutSeconds) + " --max-filesize " + strconv.Itoa(settings.MaxResponseBytes) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("start="+strconv.FormatInt(grid.start.Unix(), 10)) +
		" --data-urlencode " + shellSingleQuote("end="+strconv.FormatInt(grid.end.Unix(), 10)) +
		" --data-urlencode " + shellSingleQuote("step="+strconv.FormatInt(int64(grid.step/time.Second), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query_range'"
}

// An anchored alternation of exact values for a PromQL regex matcher.
func storageRunwayAlternation(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, regexp.QuoteMeta(value))
	}
	sort.Strings(quoted)
	return "(?:" + strings.Join(quoted, "|") + ")"
}

// A value at one grid point.
type storageRunwaySample struct {
	at    time.Time
	value float64
}

// One series of a validated range answer. invalid carries a fixed reason when
// the series' own values broke the grid or value contract; its samples are
// then empty, so the owning volume stays unknown rather than partly observed.
type storageRunwayRangeSeries struct {
	labels  map[string]string
	samples []storageRunwaySample
	invalid string
}

// Validate a bounded Mimir range answer. A failed or partial answer is one
// fixed reason. Per-series grid or value faults are isolated to that series.
// Values are finite and nonnegative; callers check their own domains.
func parseStorageRunwayRange(output string, grid storageRunwayGrid, settings StorageRunwayEstimatorSettings) ([]storageRunwayRangeSeries, string) {
	if len(output) > settings.MaxResponseBytes {
		return nil, "response-byte-bound"
	}
	var response struct {
		Status   string            `json:"status"`
		Error    json.RawMessage   `json:"error"`
		Warnings []json.RawMessage `json:"warnings"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(output), &response) != nil || response.Status != "success" ||
		response.Data.ResultType != "matrix" || len(response.Warnings) != 0 || len(response.Error) != 0 {
		return nil, "incomplete-or-invalid-response"
	}
	if len(response.Data.Result) > settings.MaxSeries {
		return nil, "series-bound"
	}
	stepSeconds := int64(grid.step / time.Second)
	startSeconds, endSeconds := grid.start.Unix(), grid.end.Unix()
	points := int((endSeconds-startSeconds)/stepSeconds) + 1
	seriesList := make([]storageRunwayRangeSeries, 0, len(response.Data.Result))
	for _, result := range response.Data.Result {
		series := storageRunwayRangeSeries{labels: result.Metric}
		if series.labels == nil {
			series.labels = map[string]string{}
		}
		if len(result.Values) == 0 || len(result.Values) > points {
			series.invalid = "invalid-series-length"
			seriesList = append(seriesList, series)
			continue
		}
		previous := int64(math.MinInt64)
		samples := make([]storageRunwaySample, 0, len(result.Values))
		for _, pair := range result.Values {
			var stamp float64
			var text string
			if len(pair) != 2 || json.Unmarshal(pair[0], &stamp) != nil || json.Unmarshal(pair[1], &text) != nil ||
				math.IsNaN(stamp) || math.IsInf(stamp, 0) || stamp != math.Trunc(stamp) {
				series.invalid = "invalid-sample-shape"
				break
			}
			seconds := int64(stamp)
			if seconds < startSeconds || seconds > endSeconds || (seconds-startSeconds)%stepSeconds != 0 || seconds <= previous {
				series.invalid = "off-grid-or-unordered-sample"
				break
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				series.invalid = "invalid-sample-value"
				break
			}
			previous = seconds
			samples = append(samples, storageRunwaySample{at: time.Unix(seconds, 0).UTC(), value: value})
		}
		if series.invalid == "" {
			series.samples = samples
		}
		seriesList = append(seriesList, series)
	}
	return seriesList, ""
}

// Filesystem byte gauges are whole bytes below 2^53.
func storageRunwayWholeBytes(samples []storageRunwaySample) bool {
	for _, sample := range samples {
		if sample.value != math.Trunc(sample.value) || sample.value > 9007199254740991 {
			return false
		}
	}
	return true
}

// The measured decline over one window ending at the newest sample.
type storageRunwayDecline struct {
	window   time.Duration
	measured bool
	// bytes per second, zero when the volume grew
	rate    float64
	elapsed time.Duration
}

// One volume's estimate at the grid end.
type storageRunwayEstimate struct {
	present bool
	// the newest sample is at the grid end within the freshness bound
	fresh  bool
	latest storageRunwaySample
	// bytes above the floor; negative below it
	free     float64
	declines []storageRunwayDecline
	// every window measured from a fresh newest sample
	complete bool
	// bytes per second, the fastest measured decline, and its window
	rate       float64
	rateWindow time.Duration
	// the largest decline over one pace window inside the pace history, in
	// bytes per pace window, and the span of history it was taken over
	paceDecline float64
	paceCovered time.Duration
	// the decline over the unknown-risk window ending at the newest sample,
	// in bytes per second, when that sample is inside the window
	recentRate  float64
	recentKnown bool
}

// The sample nearest at, within tolerance, strictly before the newest sample.
func storageRunwayNearest(samples []storageRunwaySample, at time.Time, tolerance time.Duration, before time.Time) (storageRunwaySample, bool) {
	index := sort.Search(len(samples), func(i int) bool { return !samples[i].at.Before(at) })
	best, found := storageRunwaySample{}, false
	for _, candidate := range []int{index - 1, index} {
		if candidate < 0 || candidate >= len(samples) || !samples[candidate].at.Before(before) {
			continue
		}
		distance := samples[candidate].at.Sub(at)
		if distance < 0 {
			distance = -distance
		}
		if distance > tolerance {
			continue
		}
		bestDistance := best.at.Sub(at)
		if bestDistance < 0 {
			bestDistance = -bestDistance
		}
		if !found || distance < bestDistance {
			best, found = samples[candidate], true
		}
	}
	return best, found
}

// Estimate one volume's runway. samples are ascending grid samples of the
// available bytes; floor is the volume's held-back floor in bytes.
func estimateStorageRunway(settings StorageRunwayEstimatorSettings, samples []storageRunwaySample, end time.Time, floor float64) storageRunwayEstimate {
	estimate := storageRunwayEstimate{}
	if len(samples) == 0 {
		return estimate
	}
	estimate.present = true
	estimate.latest = samples[len(samples)-1]
	estimate.fresh = !estimate.latest.at.After(end) && end.Sub(estimate.latest.at) <= settings.Freshness
	estimate.free = estimate.latest.value - floor
	estimate.complete = estimate.fresh
	decline := func(from storageRunwaySample) (float64, time.Duration) {
		elapsed := estimate.latest.at.Sub(from.at)
		return max(0, from.value-estimate.latest.value) / elapsed.Seconds(), elapsed
	}
	for _, window := range settings.Windows {
		measured := storageRunwayDecline{window: window}
		if from, found := storageRunwayNearest(samples, estimate.latest.at.Add(-window), settings.EndpointTolerance, estimate.latest.at); found {
			measured.measured = true
			measured.rate, measured.elapsed = decline(from)
			if measured.rate > estimate.rate {
				estimate.rate, estimate.rateWindow = measured.rate, window
			}
		} else {
			estimate.complete = false
		}
		estimate.declines = append(estimate.declines, measured)
	}
	earliestPaced := time.Time{}
	for _, sample := range samples {
		if sample.at.Before(estimate.latest.at.Add(-settings.PaceHistory)) {
			continue
		}
		from, found := storageRunwayNearest(samples, sample.at.Add(-settings.PaceWindow), settings.EndpointTolerance, sample.at)
		if !found {
			continue
		}
		elapsed := sample.at.Sub(from.at)
		paced := max(0, from.value-sample.value) * settings.PaceWindow.Seconds() / elapsed.Seconds()
		estimate.paceDecline = max(estimate.paceDecline, paced)
		if earliestPaced.IsZero() {
			earliestPaced = sample.at
		}
	}
	if !earliestPaced.IsZero() {
		estimate.paceCovered = estimate.latest.at.Sub(earliestPaced)
	}
	riskStart := end.Add(-settings.UnknownRiskWindow)
	if !estimate.latest.at.Before(riskStart) {
		if from, found := storageRunwayNearest(samples, riskStart, settings.EndpointTolerance, estimate.latest.at); found {
			estimate.recentRate, _ = decline(from)
			estimate.recentKnown = true
		}
	}
	return estimate
}

// The runway to the floor at the measured rate. A volume with no measured
// decline has no runway finding; one already at or below its floor while
// declining has none left.
func (self storageRunwayEstimate) runway() (time.Duration, bool) {
	if self.rate <= 0 {
		return 0, false
	}
	if self.free <= 0 {
		return 0, true
	}
	seconds := self.free / self.rate
	if seconds >= float64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// Fixed public fields: bytes, decimal GB and hours. No device, address or
// label value enters them.
func (self storageRunwayEstimate) observed(floor float64) string {
	parts := []string{
		"available_bytes=" + strconv.FormatFloat(self.latest.value, 'f', 0, 64),
		"floor_bytes=" + strconv.FormatFloat(floor, 'f', 0, 64),
		"free_above_floor_gb=" + strconv.FormatFloat(gb(self.free), 'f', 1, 64),
	}
	for _, decline := range self.declines {
		name := "decline_" + storageRunwayWindowName(decline.window) + "_gb_per_hour="
		if !decline.measured {
			parts = append(parts, name+"unknown")
			continue
		}
		parts = append(parts, name+strconv.FormatFloat(gb(decline.rate*3600), 'f', 2, 64))
	}
	if runway, declining := self.runway(); declining {
		parts = append(parts, "rate_window="+storageRunwayWindowName(self.rateWindow),
			"runway_hours="+strconv.FormatFloat(runway.Hours(), 'f', 1, 64))
	} else {
		parts = append(parts, "runway_hours=none-no-decline")
	}
	parts = append(parts,
		"largest_pace_decline_gb="+strconv.FormatFloat(gb(self.paceDecline), 'f', 1, 64),
		"pace_history_hours="+strconv.FormatFloat(self.paceCovered.Hours(), 'f', 1, 64),
	)
	return strings.Join(parts, " ")
}

// 1h, 6h, 24h, 7d, 14d; minutes for anything shorter than an hour.
func storageRunwayWindowName(window time.Duration) string {
	switch {
	case window > 48*time.Hour && window%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(window/(24*time.Hour)), 10) + "d"
	case window >= time.Hour && window%time.Hour == 0:
		return strconv.FormatInt(int64(window/time.Hour), 10) + "h"
	default:
		return strconv.FormatInt(int64(window/time.Minute), 10) + "m"
	}
}

// Remembers each volume's latest measured recent decline, so a later unknown
// runway keeps the severity of the decline it cannot rule out. Entries are
// keyed by stable volume target and forgotten once older than the risk
// window. Methods are safe for concurrent use.
type storageRunwayRiskMemory struct {
	stateLock   sync.Mutex
	volumeRisks map[string]storageRunwaySample
}

// Record a measured recent decline rate in bytes per second.
func (self *storageRunwayRiskMemory) remember(volume string, rate float64, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.volumeRisks == nil {
		self.volumeRisks = map[string]storageRunwaySample{}
	}
	self.volumeRisks[volume] = storageRunwaySample{at: at, value: rate}
}

// The remembered rate when it was measured inside the risk window.
func (self *storageRunwayRiskMemory) recall(volume string, now time.Time, window time.Duration) (float64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	risk, found := self.volumeRisks[volume]
	if !found {
		return 0, false
	}
	if risk.at.Before(now.Add(-window)) {
		delete(self.volumeRisks, volume)
		return 0, false
	}
	return risk.value, true
}

// Emission rule 1: the tier of an unknown runway, and the basis named in the
// alert. A recent decline above the page line, measured now or remembered
// from a run inside the risk window, pages.
func (self *storageRunwayRiskMemory) unknownTier(volume string, estimate storageRunwayEstimate, now time.Time, settings StorageRunwayEstimatorSettings) (string, string) {
	rate, known := estimate.recentRate, estimate.recentKnown
	basis := "measured"
	if !known {
		rate, known = self.recall(volume, now, settings.UnknownRiskWindow)
		basis = "remembered"
	}
	if !known {
		return tierWarn, "recent_decline=unknown"
	}
	text := "recent_decline_basis=" + basis + " recent_decline_gb_per_hour=" + strconv.FormatFloat(gb(rate*3600), 'f', 2, 64)
	if rate*3600 > settings.UnknownPageDeclineBytesPerHour {
		return tierPage, text
	}
	return tierWarn, text
}
