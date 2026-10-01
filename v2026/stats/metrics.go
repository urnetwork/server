package stats

// FindProviders2 sample metrics and diff.
//
// For the competition we compare two sample dumps: the official one from main
// and the evaluation one. What matters is whether the evaluated FindProviders2
// selects BETTER providers and keeps matchmaking healthy. So the metrics fall
// into two groups:
//
//   - matchmaking health: how many candidates each call sees (pool_count), how
//     long the candidate load takes (load_millis), how many providers are
//     returned (chosen_count).
//   - selection quality: the attributes of the CHOSEN providers (joined back to
//     the candidate pool by id) — their scaled weight, tier, reliability, and
//     estimated speed/latency — plus the selection LIFT (chosen weight vs the
//     pool average), which says how much better than uniform the selection is.
//
// Plus the call-shape distributions (rank mode, caller country, force flags).
// Diff reports each metric for A, B, and the change, so a reviewer can see at a
// glance whether the evaluated build picks faster / more reliable providers.

import (
	"encoding/json"
	"fmt"
	mathrand "math/rand"
	"sort"
	"strings"

	"github.com/urnetwork/server/v2026/stats/sample"
)

const reservoirCap = 100000

// stat accumulates a mean and (via a bounded reservoir) approximate
// percentiles. Not safe for concurrent use.
type stat struct {
	n    int64
	sum  float64
	min  float64
	max  float64
	res  []float64
	rng  *mathrand.Rand
	seen int64
}

func newStat() *stat {
	return &stat{rng: mathrand.New(mathrand.NewSource(1)), min: 0, max: 0}
}

func (self *stat) add(v float64) {
	if self.n == 0 || v < self.min {
		self.min = v
	}
	if self.n == 0 || self.max < v {
		self.max = v
	}
	self.n += 1
	self.sum += v
	// reservoir sampling for percentiles
	self.seen += 1
	if len(self.res) < reservoirCap {
		self.res = append(self.res, v)
	} else if j := self.rng.Int63n(self.seen); j < int64(reservoirCap) {
		self.res[j] = v
	}
}

func (self *stat) mean() float64 {
	if self.n == 0 {
		return 0
	}
	return self.sum / float64(self.n)
}

func (self *stat) percentile(p float64) float64 {
	if len(self.res) == 0 {
		return 0
	}
	sorted := append([]float64(nil), self.res...)
	sort.Float64s(sorted)
	i := int(p * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// Metrics accumulates over a stream of FindProviders2 samples.
type Metrics struct {
	Samples int64

	poolCount      *stat
	loadMillis     *stat
	effectiveCount *stat
	chosenCount    *stat

	poolWeight       *stat
	chosenWeight     *stat
	chosenTier       *stat
	chosenScore      *stat
	chosenRel        *stat
	chosenRelLatency *stat
	chosenSpeedMbps  *stat

	chosenTotal     int64
	chosenHasSpeed  int64
	chosenHasLat    int64
	chosenNotInPool int64

	rankMode      map[string]int64
	callerCountry map[string]int64
	forceMinimum  int64
	forceCount    int64
	bestAvailable int64
}

func NewMetrics() *Metrics {
	return &Metrics{
		poolCount:        newStat(),
		loadMillis:       newStat(),
		effectiveCount:   newStat(),
		chosenCount:      newStat(),
		poolWeight:       newStat(),
		chosenWeight:     newStat(),
		chosenTier:       newStat(),
		chosenScore:      newStat(),
		chosenRel:        newStat(),
		chosenRelLatency: newStat(),
		chosenSpeedMbps:  newStat(),
		rankMode:         map[string]int64{},
		callerCountry:    map[string]int64{},
	}
}

const bytesPerSecondToMbps = 8.0 / 1e6

// Add folds one sample into the metrics.
func (self *Metrics) Add(s *sample.FindProviders2Sample) {
	self.Samples += 1
	self.poolCount.add(float64(s.PoolCount))
	self.loadMillis.add(float64(s.LoadMillis))
	self.effectiveCount.add(float64(s.EffectiveCount))
	self.chosenCount.add(float64(len(s.ChosenClientIds)))

	if s.RankMode != "" {
		self.rankMode[s.RankMode] += 1
	} else {
		self.rankMode["quality"] += 1
	}
	if s.CallerCountryCode != "" {
		self.callerCountry[s.CallerCountryCode] += 1
	} else {
		self.callerCountry["(none)"] += 1
	}
	if s.ForceMinimum {
		self.forceMinimum += 1
	}
	if s.ForceCount {
		self.forceCount += 1
	}
	if s.SpecBestAvailable {
		self.bestAvailable += 1
	}

	// pool weight average over all candidates
	byId := make(map[string]*sample.Candidate, len(s.Candidates))
	for _, candidate := range s.Candidates {
		self.poolWeight.add(float64(candidate.ScaledWeight))
		byId[string(candidate.ClientId)] = candidate
	}

	// selection quality: the chosen providers' attributes
	for _, chosenId := range s.ChosenClientIds {
		candidate, ok := byId[string(chosenId)]
		if !ok {
			// a direct-client-spec provider, or a chosen id below the capped
			// candidate tail — not attributable to a candidate row
			self.chosenNotInPool += 1
			continue
		}
		self.chosenTotal += 1
		self.chosenWeight.add(float64(candidate.ScaledWeight))
		self.chosenTier.add(float64(candidate.Tier))
		self.chosenScore.add(float64(candidate.Score))
		self.chosenRel.add(candidate.ReliabilityWeight)
		self.chosenRelLatency.add(float64(candidate.MinRelativeLatencyMillis))
		self.chosenSpeedMbps.add(float64(candidate.MaxBytesPerSecond) * bytesPerSecondToMbps)
		if candidate.HasSpeedTest {
			self.chosenHasSpeed += 1
		}
		if candidate.HasLatencyTest {
			self.chosenHasLat += 1
		}
	}
}

// Report is the finalized, serializable summary of a dump.
type Report struct {
	Samples int64 `json:"samples"`

	// matchmaking health
	PoolCountMean  float64 `json:"pool_count_mean"`
	PoolCountP50   float64 `json:"pool_count_p50"`
	PoolCountP95   float64 `json:"pool_count_p95"`
	LoadMillisMean float64 `json:"load_millis_mean"`
	LoadMillisP50  float64 `json:"load_millis_p50"`
	LoadMillisP95  float64 `json:"load_millis_p95"`
	EffectiveCount float64 `json:"effective_count_mean"`
	ChosenCount    float64 `json:"chosen_count_mean"`

	// selection quality (over chosen providers)
	PoolWeightMean       float64 `json:"pool_weight_mean"`
	ChosenWeightMean     float64 `json:"chosen_weight_mean"`
	SelectionLift        float64 `json:"selection_lift"`
	ChosenTierMean       float64 `json:"chosen_tier_mean"`
	ChosenScoreMean      float64 `json:"chosen_score_mean"`
	ChosenReliabilityM   float64 `json:"chosen_reliability_mean"`
	ChosenRelLatencyMean float64 `json:"chosen_rel_latency_ms_mean"`
	ChosenSpeedMbpsMean  float64 `json:"chosen_speed_mbps_mean"`
	ChosenHasSpeedFrac   float64 `json:"chosen_has_speed_frac"`
	ChosenHasLatencyFrac float64 `json:"chosen_has_latency_frac"`

	// call shape
	RankMode       map[string]float64 `json:"rank_mode_frac"`
	TopCallerCC    map[string]float64 `json:"top_caller_country_frac"`
	ForceMinimumFr float64            `json:"force_minimum_frac"`
	BestAvailFrac  float64            `json:"best_available_frac"`
}

func (self *Metrics) Report() Report {
	frac := func(n int64) float64 {
		if self.Samples == 0 {
			return 0
		}
		return float64(n) / float64(self.Samples)
	}
	chosenFrac := func(n int64) float64 {
		if self.chosenTotal == 0 {
			return 0
		}
		return float64(n) / float64(self.chosenTotal)
	}
	lift := 0.0
	if pw := self.poolWeight.mean(); pw > 0 {
		lift = self.chosenWeight.mean() / pw
	}

	rankFrac := map[string]float64{}
	for k, v := range self.rankMode {
		rankFrac[k] = frac(v)
	}

	return Report{
		Samples:              self.Samples,
		PoolCountMean:        self.poolCount.mean(),
		PoolCountP50:         self.poolCount.percentile(0.5),
		PoolCountP95:         self.poolCount.percentile(0.95),
		LoadMillisMean:       self.loadMillis.mean(),
		LoadMillisP50:        self.loadMillis.percentile(0.5),
		LoadMillisP95:        self.loadMillis.percentile(0.95),
		EffectiveCount:       self.effectiveCount.mean(),
		ChosenCount:          self.chosenCount.mean(),
		PoolWeightMean:       self.poolWeight.mean(),
		ChosenWeightMean:     self.chosenWeight.mean(),
		SelectionLift:        lift,
		ChosenTierMean:       self.chosenTier.mean(),
		ChosenScoreMean:      self.chosenScore.mean(),
		ChosenReliabilityM:   self.chosenRel.mean(),
		ChosenRelLatencyMean: self.chosenRelLatency.mean(),
		ChosenSpeedMbpsMean:  self.chosenSpeedMbps.mean(),
		ChosenHasSpeedFrac:   chosenFrac(self.chosenHasSpeed),
		ChosenHasLatencyFrac: chosenFrac(self.chosenHasLat),
		RankMode:             rankFrac,
		TopCallerCC:          topFracs(self.callerCountry, self.Samples, 8),
		ForceMinimumFr:       frac(self.forceMinimum),
		BestAvailFrac:        frac(self.bestAvailable),
	}
}

func topFracs(counts map[string]int64, total int64, n int) map[string]float64 {
	type kv struct {
		k string
		v int64
	}
	items := make([]kv, 0, len(counts))
	for k, v := range counts {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].v > items[j].v })
	out := map[string]float64{}
	for i := 0; i < n && i < len(items); i += 1 {
		if total > 0 {
			out[items[i].k] = float64(items[i].v) / float64(total)
		}
	}
	return out
}

// DiffJSON returns {"a":..., "b":..., "delta":...} for the two reports.
func DiffJSON(a Report, b Report) ([]byte, error) {
	return json.MarshalIndent(map[string]any{"a": a, "b": b}, "", "  ")
}

// FormatDiff renders a human-readable side-by-side comparison. labelA/labelB
// name the two dumps (e.g. "official" and "eval"). betterLower marks metrics
// where a lower value is better, so the arrow points the right way.
func FormatDiff(labelA string, a Report, labelB string, b Report) string {
	var out strings.Builder
	fmt.Fprintf(&out, "FindProviders2 sample diff: %s (A) vs %s (B)\n\n", labelA, labelB)

	row := func(name string, va, vb float64, betterLower bool, format string) {
		delta := vb - va
		pct := ""
		if va != 0 {
			pct = fmt.Sprintf("%+.1f%%", 100*delta/va)
		}
		arrow := ""
		if delta != 0 {
			improved := (delta < 0) == betterLower
			if improved {
				arrow = "  better"
			} else {
				arrow = "  worse"
			}
		}
		fmt.Fprintf(&out, "  %-26s "+format+"   "+format+"   %+.4g %8s%s\n", name, va, vb, delta, pct, arrow)
	}

	fmt.Fprintf(&out, "  %-26s %14s   %14s   %s\n", "metric", labelA, labelB, "delta")
	fmt.Fprintf(&out, "  %-26s %14d   %14d\n", "samples", a.Samples, b.Samples)
	out.WriteString("\n  -- matchmaking health --\n")
	row("pool_count.mean", a.PoolCountMean, b.PoolCountMean, false, "%14.2f")
	row("pool_count.p95", a.PoolCountP95, b.PoolCountP95, false, "%14.2f")
	row("load_millis.mean", a.LoadMillisMean, b.LoadMillisMean, true, "%14.2f")
	row("load_millis.p95", a.LoadMillisP95, b.LoadMillisP95, true, "%14.2f")
	row("chosen_count.mean", a.ChosenCount, b.ChosenCount, false, "%14.2f")
	out.WriteString("\n  -- selection quality (chosen providers) --\n")
	row("selection_lift", a.SelectionLift, b.SelectionLift, false, "%14.3f")
	row("chosen_weight.mean", a.ChosenWeightMean, b.ChosenWeightMean, false, "%14.4f")
	row("chosen_tier.mean", a.ChosenTierMean, b.ChosenTierMean, true, "%14.3f")
	row("chosen_score.mean", a.ChosenScoreMean, b.ChosenScoreMean, true, "%14.3f")
	row("chosen_reliability.mean", a.ChosenReliabilityM, b.ChosenReliabilityM, false, "%14.4f")
	row("chosen_rel_latency_ms.mean", a.ChosenRelLatencyMean, b.ChosenRelLatencyMean, true, "%14.2f")
	row("chosen_speed_mbps.mean", a.ChosenSpeedMbpsMean, b.ChosenSpeedMbpsMean, false, "%14.2f")
	row("chosen_has_speed.frac", a.ChosenHasSpeedFrac, b.ChosenHasSpeedFrac, false, "%14.3f")
	row("chosen_has_latency.frac", a.ChosenHasLatencyFrac, b.ChosenHasLatencyFrac, false, "%14.3f")

	out.WriteString("\n  -- call shape --\n")
	fmt.Fprintf(&out, "  rank_mode      A=%v  B=%v\n", fmtFracs(a.RankMode), fmtFracs(b.RankMode))
	fmt.Fprintf(&out, "  caller_country A=%v\n                 B=%v\n", fmtFracs(a.TopCallerCC), fmtFracs(b.TopCallerCC))
	row("force_minimum.frac", a.ForceMinimumFr, b.ForceMinimumFr, false, "%14.3f")
	row("best_available.frac", a.BestAvailFrac, b.BestAvailFrac, false, "%14.3f")

	return out.String()
}

func fmtFracs(m map[string]float64) string {
	type kv struct {
		k string
		v float64
	}
	items := make([]kv, 0, len(m))
	for k, v := range m {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].v > items[j].v })
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%.2f", item.k, item.v))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
