//go:build !sidecar_pending

package model

// FS-54 for network_sidecar_limit: a release leg until MG1's Gn-out links no table, so
// a network's limits are its tier's alone, read with no query, and every write is
// refused rather than sent to a missing table.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestNetworkSidecarLimitsWithoutMg1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if server.SidecarMg1Linked() {
			t.Fatal("a release build links MG1")
		}
		defer Testing_SetNetworkSidecarLimitSettings(&NetworkSidecarLimitSettings{
			SnapshotTtl:   0,
			ReloadTimeout: time.Second,
		})()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"dns_entries": 5})()
		networkId := server.NewId()
		NetworkTierOf(ctx, networkId)

		attempts, stop := server.DenyPostgresForTest(t)
		limit := NetworkSidecarLimits(ctx, networkId).Limit("dns_entries")
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
		connect.AssertEqual(t, limit, int64(5))

		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "dns_entries", 1, "abuse case 0001"), ErrNetworkSidecarLimitUnavailable)
		_, err := ClearNetworkSidecarLimit(ctx, networkId, "dns_entries")
		connect.AssertEqual(t, err, ErrNetworkSidecarLimitUnavailable)
		_, err = GetNetworkSidecarLimitRows(ctx, networkId)
		connect.AssertEqual(t, err, ErrNetworkSidecarLimitUnavailable)
	})
}
