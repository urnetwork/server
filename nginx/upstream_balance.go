package nginx

// Upstream block selection with nginx's balancers (owner: s-forward),
// reproduced exactly, so each block gets the share nginx would give it and a
// mixed rollout of nginx and the front never remaps a client (§4.2):
//   - weighted least_conn (ngx_http_upstream_least_conn_module.c): the block
//     with the fewest active requests per unit of weight, compared as
//     conns × other weight, with a tie broken by smooth weighted round robin
//     over the tied blocks from the first of them;
//   - for a stateful service, nginx's consistent hash on the client address
//     (`hash $remote_addr consistent`, ngx_http_upstream_hash_module.c): a
//     ring of 160 points per unit of weight per block, each the crc32 of the
//     block's server string, `<upstream host>:<port>` split at its port as
//     crc32(host \0 port), chained with the previous point; a request takes
//     the first point at or after the crc32 of nginx's text of the client
//     address, and a retry walks forward from it past blocks already tried,
//     falling back to round robin after 20 steps;
//   - smooth weighted round robin (ngx_http_upstream_round_robin.c), that
//     fallback.
//
// Blocks are visited in nginx's server order: the generator lists a
// service's blocks sorted by name (warp/warpctl/config.go:1797-1798), which
// decides ties, so the group sorts them the same way rather than keeping the
// route file's order. nginx's ring sort is glibc's qsort, a stable merge
// sort on the lb image's Ubuntu 24.04, so points with equal hashes keep
// generation order and the first survives; the ring here is sorted stably
// the same way.
//
// A block of weight 0 is never balanced to (nginx refuses weight=0), though a
// request may still name it. nginx keeps the balancing state per worker; the
// front keeps it per process. All of it is guarded by the group's stateLock.

import (
	"encoding/binary"
	"hash/crc32"
	"slices"
)

// nginx's consistent-hash constants (ngx_http_upstream_hash_module.c).
// Compatibility values: changing either remaps clients.
const (
	// ring points per unit of block weight
	upstreamRingPointsPerWeight = 160
	// ring steps a request may walk before falling back to round robin
	upstreamRingMaxSteps = 20
)

// One point of the consistent-hash ring.
type upstreamRingPoint struct {
	hash  uint32
	block *upstreamBlock
}

// One request's place on the ring: nginx's per-request hp->hash and
// hp->tries, kept across its attempts so a retry walks forward from the first
// pick.
type upstreamRingWalk struct {
	// whether the walk has started: the first selection looks up the key
	started bool
	// the crc32 of nginx's text of the client address
	keyHash uint32
	// false for an empty key, which nginx balances by round robin
	hasKey bool
	// the ring index the walk is at
	index uint32
	// steps taken past ineligible points
	steps int
}

// The host and port nginx hashes for a server string
// (ngx_http_upstream_update_chash): the digits after the last ':' are the
// port, unless a non-digit comes first, when the whole string is the host.
func upstreamHashServerParts(server string) (host string, port string) {
	for j := 0; j < len(server); j += 1 {
		c := server[len(server)-j-1]
		if c == ':' {
			return server[:len(server)-j-1], server[len(server)-j:]
		}
		if c < '0' || '9' < c {
			break
		}
	}
	return server, ""
}

// Builds nginx's ring over the balanced blocks, in their order.
func newUpstreamRing(blocks []*upstreamBlock) []upstreamRingPoint {
	ring := []upstreamRingPoint{}
	for _, block := range blocks {
		if block.weight <= 0 {
			continue
		}
		host, port := upstreamHashServerParts(block.hashServer)
		// crc32(host \0 port), left unfinalized as nginx's base_hash is. Go's
		// crc32.Update finalizes its result and unfinalizes its input, so the
		// finalized checksum chains exactly as nginx's raw state does.
		baseHash := crc32.ChecksumIEEE([]byte(host + "\x00" + port))
		var previousHash [4]byte
		for range block.weight * upstreamRingPointsPerWeight {
			hash := crc32.Update(baseHash, crc32.IEEETable, previousHash[:])
			ring = append(ring, upstreamRingPoint{
				hash:  hash,
				block: block,
			})
			// nginx feeds back the point in host byte order, little endian
			// on the lb's hosts
			binary.LittleEndian.PutUint32(previousHash[:], hash)
		}
	}
	slices.SortStableFunc(ring, func(a upstreamRingPoint, b upstreamRingPoint) int {
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		default:
			return 0
		}
	})
	return slices.CompactFunc(ring, func(a upstreamRingPoint, b upstreamRingPoint) bool {
		return a.hash == b.hash
	})
}

// The first ring index whose point is at or after hash; len(ring) when every
// point is before it, which wraps to 0 (ngx_http_upstream_find_chash_point).
func upstreamRingFind(ring []upstreamRingPoint, hash uint32) uint32 {
	i, j := 0, len(ring)
	for i < j {
		k := (i + j) / 2
		switch {
		case hash > ring[k].hash:
			i = k + 1
		case hash < ring[k].hash:
			j = k
		default:
			return uint32(k)
		}
	}
	return uint32(i)
}

// Whether a balanced request may go to the block now: it has weight, this
// request hasn't tried it, and it is under ForwardSettings.MaxBlockActiveRequests
// (nginx's max_conns).
func (self *upstreamGroup) blockEligibleWithLock(block *upstreamBlock, triedBlocks []bool) bool {
	if block.weight <= 0 || triedBlocks[block.index] {
		return false
	}
	maxBlockActiveRequests := self.settings.Forward.MaxBlockActiveRequests
	return maxBlockActiveRequests <= 0 || block.activeRequests < maxBlockActiveRequests
}

// nginx's weighted least_conn (ngx_http_upstream_get_least_conn_peer). Nil
// when no block is eligible, nginx's "no live upstreams".
func (self *upstreamGroup) selectLeastConnWithLock(triedBlocks []bool) *upstreamBlock {
	if self.balancedBlockCount == 1 {
		// peers->single
		return self.selectRoundRobinWithLock(triedBlocks)
	}
	var best *upstreamBlock
	many := false
	for _, block := range self.blocks {
		if !self.blockEligibleWithLock(block, triedBlocks) {
			continue
		}
		if best == nil || block.activeRequests*best.weight < best.activeRequests*block.weight {
			best = block
			many = false
		} else if block.activeRequests*best.weight == best.activeRequests*block.weight {
			many = true
		}
	}
	if best == nil {
		return nil
	}
	if many {
		// smooth weighted round robin over the tied blocks, from the first;
		// effective weight is the weight, since max_fails=0 never lowers it
		total := 0
		first := best
		for _, block := range self.blocks[first.index:] {
			if !self.blockEligibleWithLock(block, triedBlocks) {
				continue
			}
			if block.activeRequests*first.weight != first.activeRequests*block.weight {
				continue
			}
			block.currentWeight += block.weight
			total += block.weight
			if best.currentWeight < block.currentWeight {
				best = block
			}
		}
		best.currentWeight -= total
	}
	return best
}

// nginx's smooth weighted round robin (ngx_http_upstream_get_round_robin_peer
// and ngx_http_upstream_get_peer), the fallback of both balancers. A single
// balanced block is chosen whenever it is under its cap, tried or not, as
// nginx's single peer is.
func (self *upstreamGroup) selectRoundRobinWithLock(triedBlocks []bool) *upstreamBlock {
	if self.balancedBlockCount == 1 {
		for _, block := range self.blocks {
			if block.weight <= 0 {
				continue
			}
			maxBlockActiveRequests := self.settings.Forward.MaxBlockActiveRequests
			if 0 < maxBlockActiveRequests && maxBlockActiveRequests <= block.activeRequests {
				return nil
			}
			return block
		}
		return nil
	}
	var best *upstreamBlock
	total := 0
	for _, block := range self.blocks {
		if !self.blockEligibleWithLock(block, triedBlocks) {
			continue
		}
		block.currentWeight += block.weight
		total += block.weight
		if best == nil || best.currentWeight < block.currentWeight {
			best = block
		}
	}
	if best == nil {
		return nil
	}
	best.currentWeight -= total
	return best
}

// nginx's consistent hash (ngx_http_upstream_init_chash_peer and
// ngx_http_upstream_get_chash_peer): the first eligible block on the ring at
// or after the key, walking forward, and round robin after 20 steps, for an
// empty key or for a single balanced block.
func (self *upstreamGroup) selectRingWithLock(walk *upstreamRingWalk, triedBlocks []bool) *upstreamBlock {
	if !walk.started {
		walk.started = true
		if 0 < len(self.ring) {
			walk.index = upstreamRingFind(self.ring, walk.keyHash)
		}
	}
	if upstreamRingMaxSteps < walk.steps || self.balancedBlockCount == 1 || !walk.hasKey {
		return self.selectRoundRobinWithLock(triedBlocks)
	}
	if len(self.ring) == 0 {
		return nil
	}
	for {
		block := self.ring[walk.index%uint32(len(self.ring))].block
		if self.blockEligibleWithLock(block, triedBlocks) {
			return block
		}
		walk.index += 1
		walk.steps += 1
		if upstreamRingMaxSteps < walk.steps {
			return self.selectRoundRobinWithLock(triedBlocks)
		}
	}
}

// A new walk for a client address: the crc32 of nginx's $remote_addr text.
func newUpstreamRingWalk(clientAddrText string) *upstreamRingWalk {
	return &upstreamRingWalk{
		keyHash: crc32.ChecksumIEEE([]byte(clientAddrText)),
		hasKey:  clientAddrText != "",
	}
}
