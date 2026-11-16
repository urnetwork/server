package model

import (
	"math"

	"github.com/urnetwork/server"
)

// What one network may hold of an extender sample the operator draws for
// clients: the geo dns sets (connect/EXTENDER.md C5, R2), the seed bundle's
// cohort (EXTENDER-UPDATE2.md §1.2) and an activation's bootstrap (C2). On
// 2026-11-16 two networks of one fork build held 3,219 of the 3,722 active
// extenders, so a sample drawn without regard to networks was about 86%
// theirs, and one operator could see, or drop, most of what a client first
// reached (owner, 2026-11-16: cap any one network at about a quarter of each
// sample a client receives).
//
// The cap is a preference, never a reason to answer less. A sample is taken in
// its own order, passing over an entry whose network already holds its quota,
// and when the order runs out with the sample short, the entries passed over
// fill it, in the same order. So the cap holds whenever enough other networks
// have candidates, and where too few do -- one operator's region, or a pool of
// two networks -- the sample is as full as it was without the cap, and the
// networks there hold more than their quota.
//
// The feed sample an extender serves and a client's own directory are not
// drawn here: a record names no network, so neither can tell one operator's
// extenders from another's.

// ExtenderSampleSettings bound what one network holds of an extender sample.
type ExtenderSampleSettings struct {
	// The largest share of one sample one network holds while other networks
	// can fill it, as a fraction of the sample's size: a network's quota is
	// the size times this, rounded up, and at least one. A quarter, so a fleet
	// that holds most of the directory holds about a quarter of what a client
	// is answered with. Non-positive takes no account of networks.
	MaxNetworkShare float64
}

func DefaultExtenderSampleSettings() *ExtenderSampleSettings {
	return &ExtenderSampleSettings{
		MaxNetworkShare: 0.25,
	}
}

// The most entries of one network a sample of sampleCount holds while other
// networks can fill it (ExtenderSampleNetworkQuota).
func (self *ExtenderSampleSettings) NetworkQuota(sampleCount int) int {
	return ExtenderSampleNetworkQuota(sampleCount, self.MaxNetworkShare)
}

// ExtenderSampleNetworkQuota is the most entries of one network a sample of
// sampleCount holds while other networks can fill it: sampleCount times
// maxNetworkShare, rounded up, at least one and at most sampleCount. A
// non-positive share is no cap, the whole sample. Pure.
func ExtenderSampleNetworkQuota(sampleCount int, maxNetworkShare float64) int {
	if sampleCount <= 0 {
		return 0
	}
	if maxNetworkShare <= 0 {
		return sampleCount
	}
	quota := int(math.Ceil(float64(sampleCount) * maxNetworkShare))
	return min(max(quota, 1), sampleCount)
}

// ExtenderNetworkSampler is one sample being taken under a network quota
// (ExtenderSampleSettings). Its owner walks the sample's candidates in their
// order, takes a candidate the sampler admits and keeps the rest aside, and
// once the order runs out fills what is left from those it kept aside, in
// their order, with TakeOver. Not safe for concurrent use.
type ExtenderNetworkSampler struct {
	sampleCount   int
	networkQuota  int
	takenCount    int
	networkCounts map[server.Id]int
}

// A sampler of a sample of sampleCount under the settings' quota.
func (self *ExtenderSampleSettings) NewNetworkSampler(sampleCount int) *ExtenderNetworkSampler {
	return NewExtenderNetworkSampler(sampleCount, self.NetworkQuota(sampleCount))
}

// NewExtenderNetworkSampler is a sampler of a sample of sampleCount in which a
// network holds at most networkQuota while others can fill it.
func NewExtenderNetworkSampler(sampleCount int, networkQuota int) *ExtenderNetworkSampler {
	return &ExtenderNetworkSampler{
		sampleCount:   max(sampleCount, 0),
		networkQuota:  networkQuota,
		networkCounts: map[server.Id]int{},
	}
}

// Whether the sample holds sampleCount entries.
func (self *ExtenderNetworkSampler) Full() bool {
	return self.sampleCount <= self.takenCount
}

// Whether the sample has room for one more entry of networkId under the
// quota: it is not full, and the network holds fewer than its quota.
func (self *ExtenderNetworkSampler) Admits(networkId server.Id) bool {
	return !self.Full() && self.networkCounts[networkId] < self.networkQuota
}

// Takes one entry of networkId, which the sampler admitted, or which is
// pinned whatever its network holds.
func (self *ExtenderNetworkSampler) Take(networkId server.Id) {
	self.takenCount += 1
	self.networkCounts[networkId] += 1
}

// Takes one entry kept aside for its network's quota, when the order ran out
// with the sample short; false, and nothing taken, once the sample is full.
func (self *ExtenderNetworkSampler) TakeOver(networkId server.Id) bool {
	if self.Full() {
		return false
	}
	self.Take(networkId)
	return true
}

// How many entries of networkId the sample holds.
func (self *ExtenderNetworkSampler) NetworkCount(networkId server.Id) int {
	return self.networkCounts[networkId]
}
