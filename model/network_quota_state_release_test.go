//go:build !sidecar_pending

package model

// A release leg links no MG2, so nothing reads or writes network_quota_state: every call refuses with
// ErrNetworkQuotaStateUnavailable and touches no store.

import (
	"context"
	"errors"
	"testing"

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
}
