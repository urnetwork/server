// Tests of the aggregation of samples into terms: the median and the minimum,
// the terms per ordered pair, and the bounded reservoir.

package solve

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"testing"
)

// The median and the minimum of a pair's samples, of odd and even counts.
func TestAggregateRtt(t *testing.T) {
	for _, test := range []struct {
		name          string
		rtts          []float64
		edgeAggregate EdgeAggregate
		want          float64
	}{
		{name: "median of one", rtts: []float64{7}, edgeAggregate: EdgeAggregateMedian, want: 7},
		{name: "median of an odd count, unordered", rtts: []float64{9, 1, 5, 30, 4}, edgeAggregate: EdgeAggregateMedian, want: 5},
		// the mean of the two middle samples
		{name: "median of an even count", rtts: []float64{10, 2, 4, 100}, edgeAggregate: EdgeAggregateMedian, want: 7},
		// one wildly queued sample moves the median by one rank, not by its size
		{name: "median shrugs off an outlier", rtts: []float64{10, 11, 12, 13, 1000}, edgeAggregate: EdgeAggregateMedian, want: 12},
		{name: "min", rtts: []float64{9, 1.5, 5, 30, 4}, edgeAggregate: EdgeAggregateMin, want: 1.5},
		{name: "min of one", rtts: []float64{7}, edgeAggregate: EdgeAggregateMin, want: 7},
	} {
		if got := aggregateRtt(test.rtts, test.edgeAggregate); got != test.want {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
	if got := aggregateRtt(nil, EdgeAggregateMedian); !math.IsNaN(got) {
		t.Errorf("no samples: %v, want NaN", got)
	}
}

// One term per ordered pair, the two directions apart, ordered, with every
// sample counted and the samples that measure nothing dropped.
func TestAggregatorTerms(t *testing.T) {
	for _, edgeAggregate := range []EdgeAggregate{EdgeAggregateMedian, EdgeAggregateMin} {
		settings := DefaultSettings()
		settings.EdgeAggregate = edgeAggregate
		aggregator := NewAggregator(settings)
		for _, rtt := range []float64{12, 10, 11} {
			aggregator.Add("b", "a", rtt)
		}
		// the other direction of the same pair is its own term
		for _, rtt := range []float64{20, 26} {
			aggregator.Add("a", "b", rtt)
		}
		aggregator.Add("a", "c", 3)
		// none of these measures anything
		for _, rejected := range []struct {
			source, target string
			rttMs          float64
		}{
			{source: "a", target: "a", rttMs: 5},
			{source: "a", target: "c", rttMs: -1},
			{source: "a", target: "c", rttMs: math.NaN()},
			{source: "a", target: "c", rttMs: math.Inf(1)},
		} {
			if aggregator.Add(rejected.source, rejected.target, rejected.rttMs) {
				t.Fatalf("Add(%q, %q, %v) accepted", rejected.source, rejected.target, rejected.rttMs)
			}
		}

		want := []Term{
			{Source: "a", Target: "b", RttMs: 23, Samples: 2},
			{Source: "a", Target: "c", RttMs: 3, Samples: 1},
			{Source: "b", Target: "a", RttMs: 11, Samples: 3},
		}
		if edgeAggregate == EdgeAggregateMin {
			want[0].RttMs = 20
			want[2].RttMs = 10
		}
		terms := aggregator.Terms()
		if len(terms) != len(want) {
			t.Fatalf("%v: terms %+v, want %+v", edgeAggregate, terms, want)
		}
		for i := range want {
			if terms[i] != want[i] {
				t.Fatalf("%v: term %d = %+v, want %+v", edgeAggregate, i, terms[i], want[i])
			}
		}
	}
}

// A pair keeps at most PairSampleReservoir samples however many it sees, counts
// every one it saw, and keeps a uniform sample of them: over many pairs of
// 1 000 samples each, a sample's chance of being kept does not depend on when
// it came, and each pair's median stays near the median of all it saw.
func TestAggregatorReservoir(t *testing.T) {
	settings := DefaultSettings()
	aggregator := NewAggregator(settings)
	const pairCount = 400
	const seenCount = 1000
	for p := 0; p < pairCount; p += 1 {
		for j := 0; j < seenCount; j += 1 {
			// the round trip is the sample's arrival index, so what the
			// reservoir kept says when it came
			aggregator.Add(fmt.Sprintf("source-%03d", p), "target", float64(j))
		}
	}
	// by tenths of the arrival order, how many samples were kept
	tenths := make([]int, 10)
	for _, pair := range aggregator.pairs {
		if len(pair.rtts) != settings.PairSampleReservoir || pair.seenCount != seenCount {
			t.Fatalf("pair kept %d of %d", len(pair.rtts), pair.seenCount)
		}
		for _, rtt := range pair.rtts {
			tenths[int(rtt)*10/seenCount] += 1
		}
	}
	want := float64(pairCount*settings.PairSampleReservoir) / 10
	for tenth, kept := range tenths {
		// four standard deviations of a binomial count
		if 4*math.Sqrt(want) < math.Abs(float64(kept)-want) {
			t.Fatalf("tenth %d of the arrivals: %d kept, want %.0f ± %.0f: %v", tenth, kept, want, 4*math.Sqrt(want), tenths)
		}
	}
	// Each pair's median is the median of 16 uniform draws from 0..999: over
	// the pairs they average 500, to within four standard errors, and spread
	// about 1000/(2·√(16 + 2)) ≈ 118.
	var medianSum float64
	var medianSumSquares float64
	for _, term := range aggregator.Terms() {
		if term.Samples != seenCount {
			t.Fatalf("term counts %d samples, saw %d", term.Samples, seenCount)
		}
		medianSum += term.RttMs
		medianSumSquares += term.RttMs * term.RttMs
	}
	medianMean := medianSum / pairCount
	medianSpread := math.Sqrt(medianSumSquares/pairCount - medianMean*medianMean)
	if 4*118/math.Sqrt(pairCount) < math.Abs(medianMean-seenCount/2) || !(80 < medianSpread && medianSpread < 160) {
		t.Fatalf("reservoir medians average %.1f and spread %.1f, want 500 and about 118", medianMean, medianSpread)
	}
	t.Logf("kept by tenth of arrival: %v (%.0f each expected); medians average %.1f, spread %.1f", tenths, want, medianMean, medianSpread)
}

// A pair's reservoir depends only on the pair and its own samples, not on the
// order its nodes were first seen in: two aggregators that meet the same pair
// at different node indexes -- one of them saw an unrelated pair first -- keep
// the same samples of the same sequence and give the same term.
func TestAggregatorReservoirIgnoresFirstSeenOrder(t *testing.T) {
	settings := DefaultSettings()
	first := NewAggregator(settings)
	second := NewAggregator(settings)
	second.Add("unrelated-source", "unrelated-target", 7)
	for j := 0; j < 20*settings.PairSampleReservoir; j += 1 {
		// a sample sequence the reservoir has to choose from
		rttMs := float64((j * 7919) % 1000)
		first.Add("pinger", "target", rttMs)
		second.Add("pinger", "target", rttMs)
	}
	firstPair := first.pairs[first.pairKeyIndexes[uint64(first.nodeIdIndexes["pinger"])<<32|uint64(first.nodeIdIndexes["target"])]]
	secondPair := second.pairs[second.pairKeyIndexes[uint64(second.nodeIdIndexes["pinger"])<<32|uint64(second.nodeIdIndexes["target"])]]
	if !slices.Equal(firstPair.rtts, secondPair.rtts) {
		t.Fatalf("the reservoirs differ: %v and %v", firstPair.rtts, secondPair.rtts)
	}
	secondTerms := second.Terms()
	if firstTerms := first.Terms(); len(firstTerms) != 1 || len(secondTerms) != 2 || firstTerms[0] != secondTerms[0] {
		t.Fatalf("terms %+v and %+v", firstTerms, secondTerms)
	}
}

// Pairs split whole across several aggregators -- as the derive job's cursors
// split them by pinger -- give the terms one aggregator gives, whatever the
// split.
func TestAggregatorSplitByPingerMatchesOne(t *testing.T) {
	settings := DefaultSettings()
	// one round trip a pinger measured to a target
	type sample struct {
		source string
		target string
		rttMs  float64
	}
	samples := []sample{}
	for s := 0; s < 60; s += 1 {
		for g := 0; g < 5; g += 1 {
			for j := 0; j < 3*settings.PairSampleReservoir; j += 1 {
				samples = append(samples, sample{
					source: fmt.Sprintf("pinger-%02d", s),
					target: fmt.Sprintf("target-%d", g),
					rttMs:  float64((s*131 + g*17 + j*7919) % 1000),
				})
			}
		}
	}
	one := NewAggregator(settings)
	for _, sample := range samples {
		one.Add(sample.source, sample.target, sample.rttMs)
	}
	want := one.Terms()
	for _, parts := range []int{2, 4, 16} {
		aggregators := make([]*Aggregator, parts)
		for p := range aggregators {
			aggregators[p] = NewAggregator(settings)
		}
		for _, sample := range samples {
			// a pinger's pairs all go to one part, by a hash of the pinger
			part := 0
			for _, b := range []byte(sample.source) {
				part = (part*31 + int(b)) % parts
			}
			aggregators[part].Add(sample.source, sample.target, sample.rttMs)
		}
		merged := []Term{}
		for _, aggregator := range aggregators {
			merged = append(merged, aggregator.Terms()...)
		}
		slices.SortFunc(merged, func(a Term, b Term) int {
			if c := cmp.Compare(a.Source, b.Source); c != 0 {
				return c
			}
			return cmp.Compare(a.Target, b.Target)
		})
		if !slices.Equal(merged, want) {
			differing := 0
			for i := range min(len(merged), len(want)) {
				if merged[i] != want[i] {
					differing += 1
				}
			}
			t.Fatalf("%d parts: %d of %d terms differ from one aggregator's", parts, differing, len(want))
		}
	}
}
