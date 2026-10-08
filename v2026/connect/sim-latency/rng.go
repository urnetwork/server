package main

// rng is a small seeded random source. A simulation seeds everything from the
// config so runs are reproducible; per-provider dynamics seed their own rng
// from the entry seed. Not safe for concurrent use — each user holds its own.

import (
	"math"
	mathrand "math/rand"
	"net/netip"

	"github.com/urnetwork/server/v2026"
)

type rng struct {
	r *mathrand.Rand
}

func newRng(seed int64) *rng {
	return &rng{r: mathrand.New(mathrand.NewSource(seed))}
}

func (self *rng) float64() float64 {
	return self.r.Float64()
}

func (self *rng) int63() int64 {
	return self.r.Int63()
}

// id returns a deterministic id from the rng, so `init` reproduces the whole
// providers.yml (ids included) from the seed. These are opaque identity keys;
// unlike minted ULIDs their high bits are not a creation timestamp, which the
// sim does not rely on.
func (self *rng) id() server.Id {
	var b [16]byte
	for i := range b {
		b[i] = byte(self.r.Intn(256))
	}
	id, _ := server.IdFromBytes(b[:])
	return id
}

func (self *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return self.r.Intn(n)
}

// poisson draws a Poisson count with the given mean (Knuth's algorithm; mean
// is small here — arrivals per second).
func (self *rng) poisson(mean float64) int {
	if mean <= 0 {
		return 0
	}
	l := math.Exp(-mean)
	k := 0
	p := 1.0
	for {
		k += 1
		p *= self.r.Float64()
		if p <= l {
			return k - 1
		}
	}
}

// geometricDepth draws a depth with the given mean (>= 0), so a crawl tree
// terminates in expectation at meanDepth.
func (self *rng) geometricDepth(mean float64) int {
	if mean <= 0 {
		return 0
	}
	// P(continue) = mean/(mean+1) gives E[depth] = mean
	pContinue := mean / (mean + 1)
	depth := 0
	for self.r.Float64() < pContinue {
		depth += 1
	}
	return depth
}

// ipIterator walks the host addresses of a prefix in order.
type ipIterator struct {
	addr   netip.Addr
	prefix netip.Prefix
}

func newIpIterator(prefix netip.Prefix) *ipIterator {
	// start at the first host address after the network address
	return &ipIterator{addr: prefix.Masked().Addr().Next(), prefix: prefix}
}

// next returns the next host address, or false when the prefix is exhausted.
func (self *ipIterator) next() (netip.Addr, bool) {
	if !self.prefix.Contains(self.addr) {
		return netip.Addr{}, false
	}
	addr := self.addr
	self.addr = self.addr.Next()
	return addr, true
}
