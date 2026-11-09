//go:build !sidecar_pending

package model

// A release leg links no MG2, so nothing reads or writes network_quota_state: every call refuses with
// ErrNetworkQuotaStateUnavailable and touches no store.

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

func TestNetworkQuotaStateRefusesWithoutMg2(t *testing.T) {
	if server.SidecarMg2Linked() {
		t.Fatal("a release leg links MG2")
	}
	ctx := context.Background()
	networkId := server.NewId()
	now := server.NowUtc()
	_, err := GetNetworkQuotaState(ctx, networkId)
	if !errors.Is(err, ErrNetworkQuotaStateUnavailable) {
		t.Fatalf("GetNetworkQuotaState: %v", err)
	}
	if err := SetNetworkStoredByteCount(ctx, networkId, 1); !errors.Is(err, ErrNetworkQuotaStateUnavailable) {
		t.Fatalf("SetNetworkStoredByteCount: %v", err)
	}
	if err := MarkNetworkSpent(ctx, networkId, now); !errors.Is(err, ErrNetworkQuotaStateUnavailable) {
		t.Fatalf("MarkNetworkSpent: %v", err)
	}
	if err := ApplyStoredByteDeltaInTx(ctx, nil, networkId, 1); !errors.Is(err, ErrNetworkQuotaStateUnavailable) {
		t.Fatalf("ApplyStoredByteDeltaInTx: %v", err)
	}
	if _, err := ClearSpentNetworks(ctx, now, nil); !errors.Is(err, ErrNetworkQuotaStateUnavailable) {
		t.Fatalf("ClearSpentNetworks: %v", err)
	}
	if _, err := GetQuotaOverLimit(ctx); !errors.Is(err, ErrNetworkQuotaStateUnavailable) {
		t.Fatalf("GetQuotaOverLimit: %v", err)
	}
	// a good batch is no place to apply, and the producer drops it after its retries: under-billing only
	batch := &sidecar.UsageBatch{
		BatchId:  connect.NewId(),
		Producer: sidecar.QuotaProducerEvs,
		Events:   []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 1}},
	}
	if applied, err := ApplyUsageBatch(ctx, batch, nil, now); applied || !errors.Is(err, ErrUsageBatchUnavailable) {
		t.Fatalf("ApplyUsageBatch: %v, %v", applied, err)
	}
	// a bad one is still refused as bad
	batch.Producer = "api"
	if _, err := ApplyUsageBatch(ctx, batch, nil, now); !errors.Is(err, ErrUsageBatchRefused) {
		t.Fatalf("ApplyUsageBatch of a bad batch: %v", err)
	}
	result, err := FlushServiceDebits(ctx, 0, nil, nil)
	if err != nil || result != (ServiceDebitFlushResult{}) {
		t.Fatalf("FlushServiceDebits: %+v, %v", result, err)
	}
	if ServiceDebitShardPending(ctx, 0) {
		t.Fatal("a shard reads pending with no journal")
	}
}
