package server

// The Redis Cluster hash slot of a key, as every master computes it (the cluster specification's
// "Keys distribution model"): CRC16/XMODEM of the key's hash tag, or of the whole key when it has none,
// modulo 16384. A tag is what lies between the key's first { and the next }, when that is not empty, so
// {scp_<C>}tok and {scp_<C>}tokrev share a slot. §18.7 stores the slot of each client's {scp_<C>} beside
// its sidecar_push_token row, so taskworker's reconcile reads a slot's rows by index, and taskworker's
// {tokgen_<n>} sentinels are the least n whose tag hashes to each slot.

import (
	"strings"
)

// How many hash slots a Redis Cluster has: a protocol value.
const RedisSlotCount = 16384

// The part of a key Redis Cluster hashes: its tag, or the whole key when it has none. An empty {} is no
// tag.
func RedisHashTagOf(key string) string {
	if start := strings.IndexByte(key, '{'); 0 <= start {
		if length := strings.IndexByte(key[start+1:], '}'); 0 < length {
			return key[start+1 : start+1+length]
		}
	}
	return key
}

// CRC16/XMODEM: polynomial 0x1021, initial value 0, no reflection, as the cluster specification gives it.
func redisCrc16(data string) uint16 {
	crc := uint16(0)
	for i := 0; i < len(data); i += 1 {
		crc ^= uint16(data[i]) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// The hash slot of key, 0 to RedisSlotCount - 1: what CLUSTER KEYSLOT answers.
func RedisHashSlot(key string) int {
	return int(redisCrc16(RedisHashTagOf(key)) % RedisSlotCount)
}
