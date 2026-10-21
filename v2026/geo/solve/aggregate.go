// The aggregation of co-signed ping samples into terms (§5.1): one term per
// ordered pair, its round trip the median (or minimum) of the pair's samples,
// with the samples each pair keeps bounded so the memory follows the pairs and
// not the pings.

package solve

import (
	"cmp"
	"hash/fnv"
	"math"
	"slices"
)

// Gathers co-signed ping samples and aggregates each ordered source→target pair
// into one Term. The two directions of a pair stay two terms, so each is
// weighed by the reputation of the source that measured it.
//
// A pair keeps its first PairSampleReservoir samples, and after that a uniform
// sample of all it has seen (reservoir sampling): the median of that sample
// stands for the median of them all, and the pair's memory stays bounded
// however many pings a day brings. The choice of which sample a new one
// replaces is drawn from the pair's own identity -- its source and target ids,
// never the order this aggregator happened to meet them in -- and how many it
// has seen, so a pair's samples depend only on its own sequence of pings: not
// on how the pairs' pings interleave, nor on which of several aggregators (the
// derive job's cursors, split by pinger) the pair went to. Samples are kept
// as float64: the median of a float32 would be off by up to a fifth of a metre
// at continental range, enough to move a genesis that is exactly the truth.
// Not safe for concurrent use.
type Aggregator struct {
	edgeAggregate  EdgeAggregate
	reservoirSize  int
	nodeIdIndexes  map[string]uint32
	nodeIds        []string
	pairKeyIndexes map[uint64]uint32
	pairs          []aggregatorPair
}

// One ordered pair: its key (source index, target index), the seed of its
// reservoir draws, the samples it has seen, and those it keeps.
type aggregatorPair struct {
	key       uint64
	seed      uint64
	seenCount uint64
	rtts      []float64
}

// A new aggregator with the settings' aggregate and reservoir size.
func NewAggregator(settings *Settings) *Aggregator {
	return &Aggregator{
		edgeAggregate:  settings.EdgeAggregate,
		reservoirSize:  max(1, settings.PairSampleReservoir),
		nodeIdIndexes:  map[string]uint32{},
		pairKeyIndexes: map[uint64]uint32{},
	}
}

// Records one co-signed sample. A sample from a node to itself, or with a round
// trip that is negative or not finite, measures nothing and is dropped
// (false).
func (self *Aggregator) Add(source string, target string, rttMs float64) bool {
	if source == target || !(0 <= rttMs) || math.IsInf(rttMs, 1) {
		return false
	}
	nodeIndex := func(nodeId string) uint32 {
		if index, ok := self.nodeIdIndexes[nodeId]; ok {
			return index
		}
		index := uint32(len(self.nodeIds))
		self.nodeIdIndexes[nodeId] = index
		self.nodeIds = append(self.nodeIds, nodeId)
		return index
	}
	key := uint64(nodeIndex(source))<<32 | uint64(nodeIndex(target))
	pairIndex, ok := self.pairKeyIndexes[key]
	if !ok {
		// FNV-1a over the source id, a zero byte and the target id: a hash
		// of the pair's identity alone, the same in every process
		identity := fnv.New64a()
		identity.Write([]byte(source))
		identity.Write([]byte{0})
		identity.Write([]byte(target))
		pairIndex = uint32(len(self.pairs))
		self.pairKeyIndexes[key] = pairIndex
		self.pairs = append(self.pairs, aggregatorPair{key: key, seed: identity.Sum64()})
	}
	pair := &self.pairs[pairIndex]
	if len(pair.rtts) < self.reservoirSize {
		pair.rtts = append(pair.rtts, rttMs)
	} else {
		// A uniform draw from the samples seen so far, this one included,
		// made from the pair's seed and the count by Knuth's multiplicative
		// hashing: the new sample takes a kept one's place with probability
		// reservoirSize/(seenCount + 1), which keeps the reservoir a uniform
		// sample of everything seen.
		const golden = 0x9e3779b97f4a7c15
		draw := (pair.seed ^ (pair.seenCount * golden)) * golden
		draw = (draw ^ (draw >> 29)) * golden
		draw ^= draw >> 32
		if slot := draw % (pair.seenCount + 1); slot < uint64(self.reservoirSize) {
			pair.rtts[slot] = rttMs
		}
	}
	pair.seenCount += 1
	return true
}

// One term per ordered pair, ordered by source and then target, each counting
// every sample its pair saw.
func (self *Aggregator) Terms() []Term {
	terms := make([]Term, 0, len(self.pairs))
	for i := range self.pairs {
		pair := &self.pairs[i]
		terms = append(terms, Term{
			Source:  self.nodeIds[pair.key>>32],
			Target:  self.nodeIds[uint32(pair.key)],
			RttMs:   aggregateRtt(slices.Clone(pair.rtts), self.edgeAggregate),
			Samples: int(min(pair.seenCount, math.MaxInt32)),
		})
	}
	slices.SortFunc(terms, func(a Term, b Term) int {
		if c := cmp.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return cmp.Compare(a.Target, b.Target)
	})
	return terms
}

// A pair's samples reduced to one round trip. The median of an even count is
// the mean of the two middle samples, which keeps it unbiased under symmetric
// noise. The samples are reordered.
func aggregateRtt(rtts []float64, edgeAggregate EdgeAggregate) float64 {
	if len(rtts) == 0 {
		return math.NaN()
	}
	switch edgeAggregate {
	case EdgeAggregateMin:
		return slices.Min(rtts)
	default:
		slices.Sort(rtts)
		middle := len(rtts) / 2
		if len(rtts)%2 == 1 {
			return rtts[middle]
		}
		return (rtts[middle-1] + rtts[middle]) / 2
	}
}
