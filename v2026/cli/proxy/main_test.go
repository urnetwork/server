package main

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestResizeProxyMessagePoolsCapsAllClassesAtEightGiB(t *testing.T) {
	resizeProxyMessagePools()

	stats := connect.GetMessagePoolAggregateStats()
	if stats.CapacityByteCount > proxyMessagePoolByteCount {
		t.Fatalf("aggregate capacity = %d, exceeds total budget %d", stats.CapacityByteCount, proxyMessagePoolByteCount)
	}
	// Integer division by four class sizes leaves only a small rounding gap.
	// The former one-argument call is roughly 24 GiB and fails this bound by a
	// wide margin, making this a deterministic regression for the defect.
	if deficit := proxyMessagePoolByteCount - stats.CapacityByteCount; deficit >= connect.ByteCount(16<<10) {
		t.Fatalf("aggregate capacity = %d, unexpected budget deficit %d", stats.CapacityByteCount, deficit)
	}

	packetBudget := proxyMessagePoolByteCount / 3
	largeBudget := proxyMessagePoolByteCount - packetBudget
	var packetCapacity, largeCapacity connect.ByteCount
	for _, class := range connect.GetMessagePoolClassStats() {
		classBytes := connect.ByteCount(class.Size * class.Capacity)
		if class.Size <= 2048 {
			packetCapacity += classBytes
		} else {
			largeCapacity += classBytes
		}
	}
	if packetCapacity > packetBudget || packetBudget-packetCapacity >= connect.ByteCount(4<<10) {
		t.Fatalf("packet capacity = %d, want budget %d within rounding", packetCapacity, packetBudget)
	}
	if largeCapacity > largeBudget || largeBudget-largeCapacity >= connect.ByteCount(12<<10) {
		t.Fatalf("large-object capacity = %d, want budget %d within rounding", largeCapacity, largeBudget)
	}
}
