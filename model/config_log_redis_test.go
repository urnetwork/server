package model

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// §7.9's read cache {acl_}e and §8.9's {acl_}v (TEAM-M2-14): the one script that writes them after each
// append, on the private stack's Redis.

// The hash as the script left it.
func testConfigLogCacheHash(t testing.TB, ctx context.Context, networkId server.Id) map[string]string {
	t.Helper()
	var fieldValues map[string]string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		fieldValues, err = r.HGetAll(ctx, sidecar.AclEntryCacheKey(sidecar.Id(networkId))).Result()
		server.Raise(err)
	})
	return fieldValues
}

func testConfigLogVersion(t testing.TB, ctx context.Context, networkId server.Id) string {
	t.Helper()
	var version string
	server.Redis(ctx, func(r server.RedisClient) {
		version, _ = r.Get(ctx, sidecar.AclVersionKey(sidecar.Id(networkId))).Result()
	})
	return version
}

// A cache whose appends name the entries by their position.
type testConfigLogCache struct {
	ctx       context.Context
	networkId server.Id
	write     func(ctx context.Context, networkId server.Id, n uint64, base uint64, entryBytes []byte) error
}

func newTestConfigLogCache(t testing.TB, maxCount int) *testConfigLogCache {
	settings := DefaultConfigLogCacheSettings()
	settings.EntryMaxCount = maxCount
	return &testConfigLogCache{ctx: t.Context(), networkId: server.NewId(), write: NewConfigLogRedisEntryCache(settings)}
}

func (self *testConfigLogCache) append(t testing.TB, n uint64, base uint64) {
	t.Helper()
	if err := self.write(self.ctx, self.networkId, n, base, []byte(fmt.Sprintf("entry %d", n))); err != nil {
		t.Fatal(err)
	}
}

// The positions the hash holds entries for, and its b, m and whether its v is {acl_}v's.
func (self *testConfigLogCache) held(t testing.TB) (positions map[uint64]bool, base uint64, tip uint64, current bool) {
	t.Helper()
	fieldValues := testConfigLogCacheHash(t, self.ctx, self.networkId)
	positions = map[uint64]bool{}
	for field, value := range fieldValues {
		switch field {
		case sidecar.AclEntryCacheFieldBase:
			base, _ = strconv.ParseUint(value, 10, 64)
		case sidecar.AclEntryCacheFieldTip:
			tip, _ = strconv.ParseUint(value, 10, 64)
		case sidecar.AclEntryCacheFieldVersion:
			current = value == testConfigLogVersion(t, self.ctx, self.networkId)
		default:
			n, err := strconv.ParseUint(field, 10, 64)
			if err != nil || value != fmt.Sprintf("entry %d", n) {
				t.Fatalf("field %q holds %q", field, value)
			}
			positions[n] = true
		}
	}
	return
}

// Each append raises {acl_}v by one and records it; a new base drops the entries below it, and the hash
// holds the newest base's run with b and m.
func TestConfigLogCacheHoldsTheNewestBasesRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cache := newTestConfigLogCache(t, 1024)
		cache.append(t, 1, 1)
		cache.append(t, 2, 1)
		cache.append(t, 3, 1)
		positions, base, tip, current := cache.held(t)
		connect.AssertEqual(t, positions, map[uint64]bool{1: true, 2: true, 3: true})
		connect.AssertEqual(t, base, uint64(1))
		connect.AssertEqual(t, tip, uint64(3))
		connect.AssertEqual(t, current, true)
		connect.AssertEqual(t, testConfigLogVersion(t, cache.ctx, cache.networkId), "3")

		cache.append(t, 4, 4)
		cache.append(t, 5, 4)
		positions, base, tip, current = cache.held(t)
		connect.AssertEqual(t, positions, map[uint64]bool{4: true, 5: true})
		connect.AssertEqual(t, base, uint64(4))
		connect.AssertEqual(t, tip, uint64(5))
		connect.AssertEqual(t, current, true)
	})
}

// m only rises: another instance's earlier entry landing late fills its hole and leaves the tip; an entry
// below the base is not stored; a stale caller's older base never lowers b.
func TestConfigLogCacheNeverLowersTheTipOrTheBase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cache := newTestConfigLogCache(t, 1024)
		cache.append(t, 10, 10)
		cache.append(t, 12, 10)
		positions, _, tip, _ := cache.held(t)
		connect.AssertEqual(t, positions, map[uint64]bool{10: true, 12: true})
		connect.AssertEqual(t, tip, uint64(12))

		cache.append(t, 11, 10)
		cache.append(t, 9, 1)
		positions, base, tip, current := cache.held(t)
		connect.AssertEqual(t, positions, map[uint64]bool{10: true, 11: true, 12: true})
		connect.AssertEqual(t, base, uint64(10))
		connect.AssertEqual(t, tip, uint64(12))
		connect.AssertEqual(t, current, true)
	})
}

// Past its count the oldest entries go, so the hash holds at most the newest EntryMaxCount.
func TestConfigLogCacheHoldsAtMostItsCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cache := newTestConfigLogCache(t, 3)
		for n := uint64(1); n <= 6; n += 1 {
			cache.append(t, n, 1)
		}
		positions, base, tip, _ := cache.held(t)
		connect.AssertEqual(t, positions, map[uint64]bool{4: true, 5: true, 6: true})
		connect.AssertEqual(t, base, uint64(1))
		connect.AssertEqual(t, tip, uint64(6))
	})
}

// FS-62's writer side: {acl_}e lost, the next append starts it again at the newest base, its earlier
// positions holes a reader skips; {acl_}e stale, an {acl_}v raised past it, it records the old value, so a
// reader takes proj/; the next append makes it current again.
func TestConfigLogCacheLostAndStale(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cache := newTestConfigLogCache(t, 1024)
		cache.append(t, 1, 1)
		cache.append(t, 2, 1)
		server.Redis(cache.ctx, func(r server.RedisClient) {
			server.Raise(r.Del(cache.ctx, sidecar.AclEntryCacheKey(sidecar.Id(cache.networkId))).Err())
		})
		cache.append(t, 3, 1)
		positions, base, tip, current := cache.held(t)
		connect.AssertEqual(t, positions, map[uint64]bool{3: true})
		connect.AssertEqual(t, base, uint64(1))
		connect.AssertEqual(t, tip, uint64(3))
		connect.AssertEqual(t, current, true)

		server.Redis(cache.ctx, func(r server.RedisClient) {
			server.Raise(r.Incr(cache.ctx, sidecar.AclVersionKey(sidecar.Id(cache.networkId))).Err())
		})
		_, _, _, current = cache.held(t)
		connect.AssertEqual(t, current, false)
		cache.append(t, 4, 1)
		_, _, _, current = cache.held(t)
		connect.AssertEqual(t, current, true)
	})
}

// The hints an entry earns: MembershipChanged for a statement of who belongs and for a tick, else
// AuthorityHeadChanged.
func TestConfigLogHintKinds(t *testing.T) {
	entry := func(kind uint32, body []byte) []byte {
		entryBytes, err := proto.Marshal(&bridge.AuthEntry{N: 2, Kind: kind, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return entryBytes
	}
	signed := func(signedType bridge.SignedType) []byte {
		signedBytes, err := proto.Marshal(&bridge.Signed{Type: uint32(signedType)})
		if err != nil {
			t.Fatal(err)
		}
		return signedBytes
	}
	connect.AssertEqual(t, configLogHintKind(entry(bridge.AuthEntryKindStatement, signed(bridge.SignedType_SIGNED_SIDECAR_REVOKE))), uint32(ConfigLogHintMembershipChanged))
	connect.AssertEqual(t, configLogHintKind(entry(bridge.AuthEntryKindStatement, signed(bridge.SignedType_SIGNED_MEMBER_ADMIT))), uint32(ConfigLogHintMembershipChanged))
	connect.AssertEqual(t, configLogHintKind(entry(bridge.AuthEntryKindTick, nil)), uint32(ConfigLogHintMembershipChanged))
	connect.AssertEqual(t, configLogHintKind(entry(bridge.AuthEntryKindStatement, signed(bridge.SignedType_SIGNED_GRANT_REVOKE))), uint32(ConfigLogHintAuthorityHeadChanged))
	connect.AssertEqual(t, configLogHintKind(entry(bridge.AuthEntryKindBase, nil)), uint32(ConfigLogHintAuthorityHeadChanged))
}
