package work

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The seed cohort (extender_seed_cohort.go) is pure: these tests need no
// database. What they pin is what the bundle may disclose.

var testExtenderSeedSecret = []byte("test extender seed directory secret")

// A fixed identity key per index, so the partitions a test draws are the same
// on every run.
func testExtenderSeedPublicKey(i int) ed25519.PublicKey {
	seed := sha256.Sum256([]byte(fmt.Sprintf("extender seed test key %d", i)))
	return ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
}

type testExtenderSeedPool struct {
	addresses []*model.NetworkExtenderDnsAddress
	keyHexes  map[server.Id]string
}

func newTestExtenderSeedPool() *testExtenderSeedPool {
	return &testExtenderSeedPool{keyHexes: map[server.Id]string{}}
}

// Adds one extender with an address in each given family, in the given tier
// and canary channel, and returns its id.
func (self *testExtenderSeedPool) add(t *testing.T, tier int, canaryChannel string, ipVersions ...int) server.Id {
	t.Helper()
	publicKey := testExtenderSeedPublicKey(len(self.keyHexes))
	extenderId := server.NewId()
	n := len(self.keyHexes) + 1
	for _, ipVersion := range ipVersions {
		ip := netip.MustParseAddr(fmt.Sprintf("198.51.%d.%d", n/250, n%250+1))
		if ipVersion == 6 {
			ip = netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", n))
		}
		self.addresses = append(self.addresses, &model.NetworkExtenderDnsAddress{
			ExtenderId:    extenderId,
			PublicKey:     publicKey,
			IpVersion:     ipVersion,
			Ip:            ip,
			CountryCode:   "us",
			DirectoryTier: tier,
			CanaryChannel: canaryChannel,
		})
	}
	self.keyHexes[extenderId] = hex.EncodeToString(publicKey)
	return extenderId
}

func (self *testExtenderSeedPool) keyHexesOf(extenderIds []server.Id) []string {
	keyHexes := []string{}
	for _, extenderId := range extenderIds {
		keyHexes = append(keyHexes, self.keyHexes[extenderId])
	}
	return keyHexes
}

// The open pool of one family, keyed as the dns sets key it.
func (self *testExtenderSeedPool) openKeyHexes(ipVersion int) []string {
	keyHexes := []string{}
	for _, address := range self.addresses {
		if address.IpVersion == ipVersion && address.DirectoryTier == connect.ExtenderDirectoryTierOpen && address.CanaryChannel == "" {
			keyHexes = append(keyHexes, hex.EncodeToString(address.PublicKey))
		}
	}
	return keyHexes
}

// A gated, a channel-restricted or a keyless extender is never in the cohort,
// even when it is most of the pool and even when a previous bundle is claimed
// to have held it.
func TestExtenderSeedCohortExcludesGatedAndChannelRestrictedRecords(t *testing.T) {
	pool := newTestExtenderSeedPool()
	excluded := map[server.Id]bool{}
	for range 20 {
		excluded[pool.add(t, connect.ExtenderDirectoryTierGated, "", 4, 6)] = true
		excluded[pool.add(t, connect.ExtenderDirectoryTierGated, connect.ExtenderChannelGated, 4)] = true
		// a dns canary keeps the open tier in its row but is channel-restricted
		excluded[pool.add(t, connect.ExtenderDirectoryTierOpen, connect.ExtenderChannelDns, 4, 6)] = true
	}
	keyless := server.NewId()
	pool.addresses = append(pool.addresses, &model.NetworkExtenderDnsAddress{
		ExtenderId:    keyless,
		IpVersion:     4,
		Ip:            netip.MustParseAddr("192.0.2.1"),
		DirectoryTier: connect.ExtenderDirectoryTierOpen,
	})
	excluded[keyless] = true
	open := []server.Id{}
	for range 3 {
		open = append(open, pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4))
	}

	excludedKeyHexes := []string{}
	for extenderId := range excluded {
		if keyHex, ok := pool.keyHexes[extenderId]; ok {
			excludedKeyHexes = append(excludedKeyHexes, keyHex)
		}
	}
	for _, previous := range [][]string{nil, excludedKeyHexes} {
		cohort := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, 1, previous, 8)
		if len(cohort) != len(open) {
			t.Fatalf("cohort %d, want the %d open extenders", len(cohort), len(open))
		}
		for _, extenderId := range cohort {
			if excluded[extenderId] {
				t.Fatalf("the cohort holds a gated, channel-restricted or keyless extender")
			}
			if !slices.Contains(open, extenderId) {
				t.Fatalf("the cohort holds an extender that is not in the pool")
			}
		}
	}
}

// The cohort never exceeds the record limit or the configured count, and is
// drawn from the dns default sets' partition of each family, which the geo
// dns already discloses, while that partition holds a cohort.
func TestExtenderSeedCohortIsBoundedToTheDnsDefaultPartitions(t *testing.T) {
	pool := newTestExtenderSeedPool()
	for range 300 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4)
	}
	for range 120 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 6)
	}
	for range 60 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	partition := map[string]bool{}
	for _, ipVersion := range []int{4, 6} {
		members, _, count := connect.ExtenderPartitionMembers(testExtenderSeedSecret, connect.ExtenderChannelDns, []byte(extenderDnsDefaultVantage), pool.openKeyHexes(ipVersion))
		if count < 2 {
			t.Fatalf("the family %d pool is one partition; the test needs several", ipVersion)
		}
		for _, keyHex := range members {
			partition[keyHex] = true
		}
	}

	if len(partition) < ExtenderSeedMaxRecordCount {
		t.Fatalf("the default partitions hold %d members; the test needs a full cohort", len(partition))
	}
	for _, count := range []int{0, 3, 8, 20} {
		want := count
		if count <= 0 || ExtenderSeedMaxRecordCount < count {
			want = ExtenderSeedMaxRecordCount
		}
		for epoch := uint64(1); epoch <= 30; epoch += 1 {
			cohort := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, epoch, nil, count)
			if len(cohort) != want {
				t.Fatalf("count %d epoch %d: cohort %d, want %d", count, epoch, len(cohort), want)
			}
			for _, keyHex := range pool.keyHexesOf(cohort) {
				if !partition[keyHex] {
					t.Fatalf("count %d epoch %d: a cohort member is outside the dns default partitions", count, epoch)
				}
			}
		}
	}
}

// Within an epoch the cohort is the same on every replica and every publish;
// a member that leaves is replaced alone, and the others keep their places, so
// polling every publish of an epoch reveals the cohort plus its replacements,
// not a new slice each time.
func TestExtenderSeedCohortIsStableWithinAnEpoch(t *testing.T) {
	pool := newTestExtenderSeedPool()
	for range 200 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	first := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, 7, nil, 8)
	again := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, 7, nil, 8)
	if !slices.Equal(first, again) {
		t.Fatalf("the same inputs drew two cohorts")
	}

	// one member goes away
	gone := first[3]
	remaining := []*model.NetworkExtenderDnsAddress{}
	for _, address := range pool.addresses {
		if address.ExtenderId != gone {
			remaining = append(remaining, address)
		}
	}
	next := selectExtenderSeedCohort(remaining, testExtenderSeedSecret, 7, pool.keyHexesOf(first), 8)
	if len(next) != 8 {
		t.Fatalf("cohort %d after one member left, want 8", len(next))
	}
	stayed := slices.DeleteFunc(slices.Clone(first), func(extenderId server.Id) bool { return extenderId == gone })
	if !slices.Equal(next[:7], stayed) {
		t.Fatalf("the members that stayed in the pool did not keep their order")
	}
	if slices.Contains(next, gone) || slices.Contains(first, next[7]) {
		t.Fatalf("the vacancy was not filled by a new member")
	}

	// many publishes in one epoch, each with the previous cohort: nothing
	// changes while nothing leaves
	revealed := map[server.Id]bool{}
	previous := pool.keyHexesOf(next)
	for range 50 {
		cohort := selectExtenderSeedCohort(remaining, testExtenderSeedSecret, 7, previous, 8)
		for _, extenderId := range cohort {
			revealed[extenderId] = true
		}
		previous = pool.keyHexesOf(cohort)
	}
	if len(revealed) != 8 {
		t.Fatalf("fifty publishes in one epoch revealed %d records, want the 8 of the cohort", len(revealed))
	}
}

// A planned update -- the cohort epoch turning, with no previous cohort carried
// across it -- draws again from the same partitions.
func TestExtenderSeedCohortPlannedUpdateDrawsFromTheSamePartitions(t *testing.T) {
	pool := newTestExtenderSeedPool()
	for range 1000 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4)
	}
	partition := map[string]bool{}
	members, _, _ := connect.ExtenderPartitionMembers(testExtenderSeedSecret, connect.ExtenderChannelDns, []byte(extenderDnsDefaultVantage), pool.openKeyHexes(4))
	for _, keyHex := range members {
		partition[keyHex] = true
	}
	if len(members) <= 8 {
		t.Fatalf("the partition has %d members; the test needs more than a cohort", len(members))
	}
	distinct := map[server.Id]bool{}
	changed := false
	previous := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, 1, nil, 8)
	for epoch := uint64(2); epoch <= 12; epoch += 1 {
		cohort := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, epoch, nil, 8)
		changed = changed || !slices.Equal(cohort, previous)
		for _, extenderId := range cohort {
			distinct[extenderId] = true
			if !partition[pool.keyHexes[extenderId]] {
				t.Fatalf("epoch %d drew outside the partition", epoch)
			}
		}
		previous = cohort
	}
	if !changed {
		t.Fatalf("eleven planned updates never changed the cohort")
	}
	if len(members) < len(distinct) {
		t.Fatalf("planned updates revealed %d records of a %d-record partition", len(distinct), len(members))
	}
}

// With both families in the pool, the cohort holds both: an IPv4-only and an
// IPv6-only client each find candidates.
func TestExtenderSeedCohortCoversBothFamilies(t *testing.T) {
	pool := newTestExtenderSeedPool()
	for range 600 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4)
	}
	for range 600 {
		pool.add(t, connect.ExtenderDirectoryTierOpen, "", 6)
	}
	families := map[server.Id][]int{}
	for _, address := range pool.addresses {
		families[address.ExtenderId] = append(families[address.ExtenderId], address.IpVersion)
	}
	for epoch := uint64(1); epoch <= 20; epoch += 1 {
		cohort := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, epoch, nil, 8)
		ipv4, ipv6 := 0, 0
		for _, extenderId := range cohort {
			if slices.Contains(families[extenderId], 4) {
				ipv4 += 1
			}
			if slices.Contains(families[extenderId], 6) {
				ipv6 += 1
			}
		}
		if ipv4 < 4 || ipv6 < 4 {
			t.Fatalf("epoch %d: %d IPv4 and %d IPv6 members, want at least 4 of each", epoch, ipv4, ipv6)
		}
	}

	// a family that is absent leaves the whole cohort to the other
	v4only := newTestExtenderSeedPool()
	for range 50 {
		v4only.add(t, connect.ExtenderDirectoryTierOpen, "", 4)
	}
	if cohort := selectExtenderSeedCohort(v4only.addresses, testExtenderSeedSecret, 1, nil, 8); len(cohort) == 0 {
		t.Fatalf("an IPv4-only pool drew no cohort")
	}
}

// A default partition short of a cohort is taken whole, and the rest comes from
// the partitions after it around the ring, the same members on every draw.
func TestExtenderSeedCohortFillsAShortPartitionAroundTheRing(t *testing.T) {
	for _, size := range []int{5, 9, 20, 40, 70} {
		pool := newTestExtenderSeedPool()
		for range size {
			pool.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
		}
		members, _, partitionCount := connect.ExtenderPartitionMembers(testExtenderSeedSecret, connect.ExtenderChannelDns, []byte(extenderDnsDefaultVantage), pool.openKeyHexes(4))
		want := min(ExtenderSeedMaxRecordCount, size)
		for epoch := uint64(1); epoch <= 5; epoch += 1 {
			cohort := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, epoch, nil, 8)
			if len(cohort) != want {
				t.Fatalf("pool %d: cohort %d, want %d", size, len(cohort), want)
			}
			keyHexes := pool.keyHexesOf(cohort)
			if len(members) <= want {
				for _, keyHex := range members {
					if !slices.Contains(keyHexes, keyHex) {
						t.Fatalf("pool %d (%d partitions): a short default partition was not taken whole", size, partitionCount)
					}
				}
			} else {
				for _, keyHex := range keyHexes {
					if !slices.Contains(members, keyHex) {
						t.Fatalf("pool %d (%d partitions): a member came from beyond a full default partition", size, partitionCount)
					}
				}
			}
			if again := selectExtenderSeedCohort(pool.addresses, testExtenderSeedSecret, epoch, nil, 8); !slices.Equal(again, cohort) {
				t.Fatalf("pool %d: two draws of one epoch differ", size)
			}
		}
	}
}
