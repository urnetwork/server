package connect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	connectcore "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func requireServicePoolBudget(t *testing.T, total connectcore.ByteCount) {
	t.Helper()
	stats := connectcore.GetMessagePoolAggregateStats()
	if stats.CapacityByteCount > total || total-stats.CapacityByteCount >= 16*1024 {
		t.Errorf("global free-buffer capacity=%d, total budget=%d", stats.CapacityByteCount, total)
	}
	if stats.RetainedByteCount > total {
		t.Errorf("native returned high-water=%d exceeds total budget=%d", stats.RetainedByteCount, total)
	}
	packet, large := total/3, total-total/3
	for _, class := range connectcore.GetMessagePoolClassStats() {
		var expected connectcore.ByteCount
		switch class.Size {
		case 256:
			expected = packet / 4
		case 2048:
			expected = packet - packet/4
		case 4096, 8192:
			expected = large / 2
		default:
			t.Fatalf("unreviewed pool class %d", class.Size)
		}
		capacity := connectcore.ByteCount(class.Size * class.Capacity)
		if capacity > expected || expected-capacity >= connectcore.ByteCount(class.Size) {
			t.Errorf("class=%d capacity=%d budget=%d", class.Size, capacity, expected)
		}
		if class.Retained > class.Capacity {
			t.Errorf("class=%d retained=%d exceeds capacity=%d", class.Size, class.Retained, class.Capacity)
		}
	}
}

// Reaches the actual startup pool call before a refused readiness result.
// The listener and readiness dependencies are local; no socket, DB, or Redis
// fixture is needed to test the process-global startup allowance.
func TestServiceMessagePoolStartupUsesOneTotalBudget(t *testing.T) {
	for key, value := range map[string]string{
		"WARP_ENV": "local", "WARP_VERSION": "0.0.0",
		"WARP_HOST": "private-pool", "WARP_SERVICE": "connect", "WARP_BLOCK": "private-pool",
		"WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080",
	} {
		t.Setenv(key, value)
	}
	stop := errors.New("private pool startup boundary")
	serves := 0
	err := runWithDependencies(context.Background(), RunOptions{Port: 8080},
		func(context.Context) error { return stop },
		func(context.Context) func() { t.Error("refused startup published metrics"); return func() {} },
		func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
			serves++
			return nil
		},
	)
	if err != nil || serves != 1 {
		t.Fatalf("startup result=%v local status calls=%d", err, serves)
	}
	requireServicePoolBudget(t, 16<<30)
	t.Logf("production_startup_capacity=%d expected_total=%d", connectcore.GetMessagePoolAggregateStats().CapacityByteCount, connectcore.ByteCount(16<<30))
}

// A real four-class return burst fills the historical 3x free-list ceiling.
// Applying the service policy must drop only excess free roots, keep borrowed
// read-only shares intact, and bound subsequent native Get/Return high-waters.
func TestServiceMessagePoolLoadedReturnHonorsTotalAndBorrowedOwners(t *testing.T) {
	const budget connectcore.ByteCount = 32 << 20 // scaled local control
	connectcore.ClearMessagePools()
	connectcore.ResizeMessagePools(budget) // exact historical API/legacy ceiling
	t.Cleanup(func() { connectcore.ClearMessagePools(); ConfigureMessagePools() })
	before := connectcore.GetMessagePoolAggregateStats()
	if before.CapacityByteCount != 3*budget {
		t.Fatalf("legacy baseline capacity=%d want=%d", before.CapacityByteCount, 3*budget)
	}
	type heldRoot struct{ owner, shared, expected []byte }
	var held []heldRoot
	t.Cleanup(func() {
		for _, root := range held {
			if root.owner != nil {
				connectcore.MessagePoolReturn(root.owner)
			}
			if root.shared != nil {
				connectcore.MessagePoolReturn(root.shared)
			}
		}
	})
	sizes := map[int]int{256: 80, 2048: 1400, 4096: 3072, 8192: 8000}
	for _, class := range connectcore.GetMessagePoolClassStats() {
		buffers := make([][]byte, class.Capacity+8)
		for i := range buffers {
			buffers[i] = connectcore.MessagePoolGet(sizes[class.Size])
			buffers[i][0], buffers[i][len(buffers[i])-1] = byte(i), byte(i>>8)
		}
		owner := buffers[0]
		expected := bytes.Repeat([]byte{byte(class.Size / 256)}, len(owner))
		copy(owner, expected)
		held = append(held, heldRoot{owner, connectcore.MessagePoolShareReadOnly(owner), expected})
		for _, message := range buffers[1:] {
			if !connectcore.MessagePoolReturn(message) {
				t.Fatal("native burst owner was not finally returned")
			}
		}
		clear(buffers)
	}
	legacy := connectcore.GetMessagePoolAggregateStats()
	if legacy.RetainedByteCount != 3*budget || legacy.Taken-legacy.Returned != before.Taken-before.Returned+4 {
		t.Fatalf("native high-water or borrowed-root ownership: %+v", legacy)
	}
	resizeServiceMessagePools(budget)
	bounded := connectcore.GetMessagePoolAggregateStats()
	requireServicePoolBudget(t, budget)
	if bounded.Taken != legacy.Taken || bounded.Returned != legacy.Returned {
		t.Fatal("resizing changed live ownership counters")
	}
	for i := range held {
		root := &held[i]
		if !bytes.Equal(root.owner, root.expected) || !bytes.Equal(root.shared, root.expected) {
			t.Fatal("free-list resize changed a borrowed payload")
		}
		if connectcore.MessagePoolReturn(root.owner) {
			t.Fatal("borrowed root returned before its read-only share")
		}
		root.owner = nil
		if !bytes.Equal(root.shared, root.expected) || !connectcore.MessagePoolReturn(root.shared) {
			t.Fatal("last shared owner did not preserve and release the exact payload")
		}
		root.shared = nil
	}
	for round := 0; round < 3; round++ {
		for _, class := range connectcore.GetMessagePoolClassStats() {
			buffers := make([][]byte, class.Capacity+8)
			for i := range buffers {
				buffers[i] = connectcore.MessagePoolGet(sizes[class.Size])
			}
			for _, message := range buffers {
				if !connectcore.MessagePoolReturn(message) {
					t.Fatal("refill did not release its final owner")
				}
			}
		}
		requireServicePoolBudget(t, budget)
	}
	after := connectcore.GetMessagePoolAggregateStats()
	if after.Taken-after.Returned != before.Taken-before.Returned {
		t.Fatalf("final owner imbalance: before=%+v after=%+v", before, after)
	}
	t.Logf("legacy_retained=%d bounded_retained=%d total_budget=%d held_roots=4 shares_preserved=true refill_rounds=3 taken_delta=%d returned_delta=%d", legacy.RetainedByteCount, after.RetainedByteCount, budget, after.Taken-before.Taken, after.Returned-before.Returned)
}
