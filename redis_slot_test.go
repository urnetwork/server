package server

import (
	"strconv"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The cluster specification's CRC16 check value and slots Redis answers to CLUSTER KEYSLOT, with its
// hash tag rules: the first { to the next }, an empty {} hashing the whole key.
func TestRedisHashSlotVectors(t *testing.T) {
	if crc := redisCrc16("123456789"); crc != 0x31c3 {
		t.Fatalf("crc16 = %#x, want 0x31c3", crc)
	}
	for _, c := range []struct {
		key  string
		slot int
	}{
		{key: "123456789", slot: 0x31c3},
		{key: "foo", slot: 12182},
		{key: "bar", slot: 5061},
		{key: "hello", slot: 866},
		{key: "{foo}bar", slot: 12182},
		{key: "x{foo}y{bar}", slot: 12182},
		{key: "", slot: 0},
	} {
		if slot := RedisHashSlot(c.key); slot != c.slot {
			t.Errorf("%q: slot %d, want %d", c.key, slot, c.slot)
		}
	}
	for _, c := range []struct {
		key string
		tag string
	}{
		{key: "{user1000}.following", tag: "user1000"},
		{key: "foo{}{bar}", tag: "foo{}{bar}"},
		{key: "foo{{bar}}zap", tag: "{bar"},
		{key: "foo{bar}{zap}", tag: "bar"},
		{key: "{scp_0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a51}tokrev", tag: "scp_0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a51"},
		{key: "no tag", tag: "no tag"},
		{key: "open{only", tag: "open{only"},
	} {
		if tag := sidecar.RedisHashTag(c.key); tag != c.tag || RedisHashSlot(c.key) != int(redisCrc16(c.tag)%RedisSlotCount) {
			t.Errorf("%q: tag %q, want %q", c.key, tag, c.tag)
		}
	}
}

// The same slot as sidecartest's own CRC, which T2's fake cluster places keys by, for a client's keys
// and the sentinels' tags.
func TestRedisHashSlotMatchesTheFakeCluster(t *testing.T) {
	for range 1000 {
		key := "{scp_" + connect.NewId().String() + "}tok"
		if RedisHashSlot(key) != sidecartest.RedisClusterSlot(key) {
			t.Fatalf("%s: %d and %d", key, RedisHashSlot(key), sidecartest.RedisClusterSlot(key))
		}
	}
	for n := range 100000 {
		key := "{tokgen_" + strconv.Itoa(n) + "}"
		if RedisHashSlot(key) != sidecartest.RedisClusterSlot(key) {
			t.Fatalf("%s: %d and %d", key, RedisHashSlot(key), sidecartest.RedisClusterSlot(key))
		}
	}
}
