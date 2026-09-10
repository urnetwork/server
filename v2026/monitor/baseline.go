// Local metric history: learned "normal for now" bands that refine the static
// SIGNALS.md thresholds ("compare against trailing-hour median, not a
// constant", 1.1).
//
// The store is deliberately independent of everything the monitor watches: one
// append-only file per metric on local disk, loaded at start, pruned by age.
// Missing or short history degrades gracefully — probes fall back to their
// static bands until enough samples accrue. The store is safe for concurrent
// use.
package monitor

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// keep two weeks of history; enough for 7-day same-hour comparisons
	baselineRetention = 14 * 24 * time.Hour
	// rewrite a metric file when the on-disk line count is 2x the live set
	baselineCompactFactor = 2
)

type baselineSample struct {
	at time.Time
	v  float64
}

// baselineStore holds per-metric history rings, persisted under dir.
type baselineStore struct {
	dir string

	stateLock sync.Mutex
	// metric name -> samples in time order
	metricSamples map[string][]baselineSample
	// metric name -> on-disk lines since last compact
	metricAppendCounts map[string]int
}

// newBaselineStore opens (creating if needed) a store rooted at dir and loads
// history.
func newBaselineStore(dir string) (*baselineStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	self := &baselineStore{
		dir:                dir,
		metricSamples:      map[string][]baselineSample{},
		metricAppendCounts: map[string]int{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".metric") {
			continue
		}
		encodedName := strings.TrimSuffix(e.Name(), ".metric")
		name := ""
		if strings.HasPrefix(encodedName, "v2-") {
			nameBytes, decodeErr := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encodedName, "v2-"))
			if decodeErr == nil {
				name = string(nameBytes)
			}
		}
		if name == "" {
			// Compatibility with the original flat-name format. Its slash to
			// underscore mapping was lossy, but retaining the samples is more
			// useful than silently discarding every pre-refactor baseline.
			name = strings.ReplaceAll(encodedName, "_", "/")
		}
		self.metricSamples[name] = loadBaselineFile(filepath.Join(dir, e.Name()))
		self.metricAppendCounts[name] = len(self.metricSamples[name])
	}
	return self, nil
}

func loadBaselineFile(path string) []baselineSample {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	cutoff := time.Now().Add(-baselineRetention)
	samples := []baselineSample{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var unix int64
		var v float64
		if _, err := fmt.Sscanf(scanner.Text(), "%d %g", &unix, &v); err != nil {
			continue
		}
		at := time.Unix(unix, 0)
		if at.Before(cutoff) {
			continue
		}
		samples = append(samples, baselineSample{at: at, v: v})
	}
	return samples
}

func (self *baselineStore) path(metric string) string {
	// Metric IDs contain slashes and arbitrary key-family shapes. A reversible
	// encoding keeps filenames flat while allowing a fresh Signal.Run to load
	// the exact same metric name.
	name := "v2-" + base64.RawURLEncoding.EncodeToString([]byte(metric))
	return filepath.Join(self.dir, name+".metric")
}

// record appends one sample for metric and persists it.
func (self *baselineStore) record(metric string, at time.Time, v float64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	cutoff := time.Now().Add(-baselineRetention)
	samples := self.metricSamples[metric]
	for len(samples) > 0 && samples[0].at.Before(cutoff) {
		samples = samples[1:]
	}
	samples = append(samples, baselineSample{at: at, v: v})
	self.metricSamples[metric] = samples

	f, err := os.OpenFile(self.path(metric), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%d %g\n", at.Unix(), v)
	f.Close()
	self.metricAppendCounts[metric] += 1

	// compact when the file has accumulated well past the live window
	if self.metricAppendCounts[metric] > baselineCompactFactor*len(samples) && self.metricAppendCounts[metric] > 1000 {
		self.compactWithLock(metric, samples)
	}
}

func (self *baselineStore) compactWithLock(metric string, samples []baselineSample) {
	tmp := self.path(metric) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	for _, s := range samples {
		fmt.Fprintf(f, "%d %g\n", s.at.Unix(), s.v)
	}
	f.Close()
	if err := os.Rename(tmp, self.path(metric)); err != nil {
		return
	}
	self.metricAppendCounts[metric] = len(samples)
}

// trailingWindow returns the sample values for metric within [now-d, now].
func (self *baselineStore) trailingWindow(metric string, d time.Duration) []float64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	cutoff := time.Now().Add(-d)
	vs := []float64{}
	for _, s := range self.metricSamples[metric] {
		if !s.at.Before(cutoff) {
			vs = append(vs, s.v)
		}
	}
	return vs
}

// trailingMedian returns the median of the samples in the trailing window and
// how many samples informed it. ok is false when fewer than minSamples exist —
// callers fall back to static bands.
func (self *baselineStore) trailingMedian(metric string, window time.Duration, minSamples int) (median float64, n int, ok bool) {
	vs := self.trailingWindow(metric, window)
	n = len(vs)
	if n < minSamples {
		return 0, n, false
	}
	sort.Float64s(vs)
	if n%2 == 1 {
		return vs[n/2], n, true
	}
	return (vs[n/2-1] + vs[n/2]) / 2, n, true
}

// trailingMedianSpanning adds a minimum wall-clock span to the sample-count
// guard. A duplicated watcher or temporarily faster cadence can otherwise
// satisfy minSamples with only a short anomalous interval and call that a
// long-window baseline. Samples after now are ignored rather than allowing a
// bad local clock entry to manufacture the required span.
func (self *baselineStore) trailingMedianSpanning(
	metric string,
	window time.Duration,
	minSamples int,
	minSpan time.Duration,
) (median float64, n int, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	cutoff := now.Add(-window)
	vs := []float64{}
	var earliest time.Time
	var latest time.Time
	for _, sample := range self.metricSamples[metric] {
		if sample.at.Before(cutoff) || sample.at.After(now) {
			continue
		}
		vs = append(vs, sample.v)
		if earliest.IsZero() || sample.at.Before(earliest) {
			earliest = sample.at
		}
		if latest.IsZero() || latest.Before(sample.at) {
			latest = sample.at
		}
	}
	n = len(vs)
	if n < minSamples || earliest.IsZero() || latest.Sub(earliest) < minSpan {
		return 0, n, false
	}
	sort.Float64s(vs)
	if n%2 == 1 {
		return vs[n/2], n, true
	}
	return (vs[n/2-1] + vs[n/2]) / 2, n, true
}
