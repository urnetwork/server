//go:build sidecar_pending

package model

// network_sidecar_limit and NetworkSidecarLimits (DESIGN.md 20.5; FS-47's row clause;
// R-QUOTA-5). MG1 is pending, so these run under -tags sidecar_pending, against the test
// database and redis, through .stack/server-test.sh. network_sidecar_limit_release_test.go
// holds the release leg.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A fresh snapshot that never reloads on its own during a test, and the pro.yml numbers
// the tests read, whatever the environment's pro.yml says.
func testingNetworkSidecarLimitSetup(t testing.TB) {
	t.Helper()
	if !server.SidecarMg1Linked() {
		t.Fatal("a pending build does not link MG1")
	}
	t.Cleanup(Testing_SetNetworkSidecarLimitSettings(&NetworkSidecarLimitSettings{
		SnapshotTtl:   time.Hour,
		ReloadTimeout: 30 * time.Second,
	}))
	t.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
		"sidecar_clients":            20,
		"dns_entries":                5,
		"storage":                    1024 * 1024 * 1024,
		"acl_groups":                 0,
		"directory_requests_per_day": 50,
		"evs_closure_bytes_per_day":  64 * 1024 * 1024,
		"evs_closure_bytes_floor":    4 * 1024 * 1024,
		"evs_closure_uploads_per_s":  4,
	}))
	t.Cleanup(Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{
		"sidecar_clients": 200,
		"dns_entries":     200,
	}))
}

func testingSidecarLimitNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("sidecarlimit%s", networkId), server.NewId())
	return networkId
}

// A row raises a limit (ops) or tightens one (§21), and either wins over the tier, for
// tier keys and sidecar_abuse: keys alike; the keys with no row read the tier's.
func TestNetworkSidecarLimitRowsRaiseAndTighten(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingNetworkSidecarLimitSetup(t)
		ctx := context.Background()
		networkId := testingSidecarLimitNetwork(t, ctx)
		other := testingSidecarLimitNetwork(t, ctx)

		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "sidecar_clients", 500, "ops: embed pilot, change 1234"), nil)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "dns_entries", 1, "abuse case 0001"), nil)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "directory_requests_per_day", 10, "abuse case 0002"), nil)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "acl_groups", 1, "ops: change 1235"), nil)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "storage", -1, "ops: change 1236"), nil)

		limits := NetworkSidecarLimits(ctx, networkId)
		connect.AssertEqual(t, limits.Limit("sidecar_clients"), int64(500))
		connect.AssertEqual(t, limits.Limit("dns_entries"), int64(1))
		connect.AssertEqual(t, limits.Limit("directory_requests_per_day"), int64(10))
		connect.AssertEqual(t, limits.Limit("acl_groups"), int64(1))
		connect.AssertEqual(t, limits.Limit("storage"), proSidecarLimitNone)
		// keys with no row read the tier's
		connect.AssertEqual(t, limits.Limit("evs_closure_bytes_per_day"), int64(64*1024*1024))
		connect.AssertEqual(t, limits.Limit("evs_closure_uploads_per_s"), int64(4))

		// another network reads its tier alone
		otherLimits := NetworkSidecarLimits(ctx, other)
		connect.AssertEqual(t, otherLimits.Limit("sidecar_clients"), int64(20))
		connect.AssertEqual(t, otherLimits.Limit("dns_entries"), int64(5))
		connect.AssertEqual(t, otherLimits.Limit("directory_requests_per_day"), int64(50))

		// rows win over a higher tier too
		now := server.NowUtc()
		testingAddTierBalance(t, ctx, networkId, true, now.Add(-time.Hour), now.Add(time.Hour))
		UpdateProNetwork(ctx, networkId)
		proLimits := NetworkSidecarLimits(ctx, networkId)
		connect.AssertEqual(t, proLimits.Limit("dns_entries"), int64(1))
		connect.AssertEqual(t, proLimits.Limit("sidecar_clients"), int64(500))

		rows, err := GetNetworkSidecarLimitRows(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		keys := []string{}
		for _, row := range rows {
			keys = append(keys, row.LimitKey)
		}
		connect.AssertEqual(t, keys, []string{"acl_groups", "directory_requests_per_day", "dns_entries", "sidecar_clients", "storage"})
		connect.AssertEqual(t, rows[2].Reason, "abuse case 0001")

		// lifting a row returns the key to the tier, at once in this process
		cleared, err := ClearNetworkSidecarLimit(ctx, networkId, "dns_entries")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cleared, true)
		connect.AssertEqual(t, NetworkSidecarLimits(ctx, networkId).Limit("dns_entries"), int64(200))
		cleared, err = ClearNetworkSidecarLimit(ctx, networkId, "dns_entries")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cleared, false)
	})
}

// FS-47's row clause: a row below the floor, or one changing the pacing or the floor,
// is refused before anything writes it.
func TestNetworkSidecarLimitRefusesFloorAndPacingRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingNetworkSidecarLimitSetup(t)
		ctx := context.Background()
		networkId := testingSidecarLimitNetwork(t, ctx)

		for _, c := range []struct {
			limitKey   string
			limitValue int64
		}{
			{limitKey: "evs_closure_bytes_per_day", limitValue: 4*1024*1024 - 1},
			{limitKey: "evs_closure_bytes_per_day", limitValue: 0},
			{limitKey: "evs_closure_uploads_per_s", limitValue: 8},
			{limitKey: "evs_closure_uploads_per_s", limitValue: 1},
			{limitKey: "evs_closure_bytes_floor", limitValue: 8 * 1024 * 1024},
			{limitKey: "evs_closure_bytes_floor", limitValue: 1024},
			{limitKey: "forwarding", limitValue: 1},
		} {
			err := SetNetworkSidecarLimit(ctx, networkId, c.limitKey, c.limitValue, "abuse case 0003")
			if !errors.Is(err, ErrNetworkSidecarLimitRefused) {
				t.Errorf("%s=%d: %v, want refused", c.limitKey, c.limitValue, err)
			}
		}
		rows, err := GetNetworkSidecarLimitRows(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(rows), 0)

		// the floor itself, and none, may stand
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "evs_closure_bytes_per_day", 4*1024*1024, "abuse case 0004"), nil)
		connect.AssertEqual(t, NetworkSidecarLimits(ctx, networkId).Limit("evs_closure_bytes_per_day"), int64(4*1024*1024))
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "evs_closure_bytes_per_day", -1, "ops: change 1237"), nil)
		connect.AssertEqual(t, NetworkSidecarLimits(ctx, networkId).Limit("evs_closure_bytes_per_day"), proSidecarLimitNone)

		// no row for a network that does not exist
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, server.NewId(), "dns_entries", 1, "ops: change 1238"), ErrNetworkNotFound)
	})
}

// A row written to the table directly, past the validation: under the floor it reads
// the floor, and one setting the pacing, the floor, an unknown key or a value its kind
// cannot take is ignored, in the snapshot and in a read before the snapshot alike.
func TestNetworkSidecarLimitDirectRowsClampedOrIgnored(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingNetworkSidecarLimitSetup(t)
		ctx := context.Background()
		networkId := testingSidecarLimitNetwork(t, ctx)

		server.Tx(ctx, func(tx server.PgTx) {
			for limitKey, limitValue := range map[string]int64{
				"evs_closure_bytes_per_day": 1024,
				"evs_closure_uploads_per_s": 1,
				"evs_closure_bytes_floor":   0,
				"no_such_limit":             9,
				"acl_groups":                7,
				"dns_entries":               -5,
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_sidecar_limit (network_id, limit_key, limit_value, reason)
						VALUES ($1, $2, $3, 'written directly')
					`,
					networkId,
					limitKey,
					limitValue,
				))
			}
		})

		check := func(limits *ProSidecarLimits) {
			connect.AssertEqual(t, limits.Limit("evs_closure_bytes_per_day"), int64(4*1024*1024))
			connect.AssertEqual(t, limits.Limit("evs_closure_uploads_per_s"), int64(4))
			connect.AssertEqual(t, limits.Limit("evs_closure_bytes_floor"), int64(4*1024*1024))
			connect.AssertEqual(t, limits.Limit("acl_groups"), int64(0))
			connect.AssertEqual(t, limits.Limit("dns_entries"), int64(5))
		}
		// no snapshot yet: the read takes the network's own rows
		installed, _ := Testing_NetworkSidecarLimitSnapshotKeys(networkId)
		connect.AssertEqual(t, installed, false)
		check(NetworkSidecarLimits(ctx, networkId))

		ReloadNetworkSidecarLimits(ctx)
		installed, limitKeys := Testing_NetworkSidecarLimitSnapshotKeys(networkId)
		connect.AssertEqual(t, installed, true)
		connect.AssertEqual(t, limitKeys, []string{"evs_closure_bytes_per_day"})
		check(NetworkSidecarLimits(ctx, networkId))
	})
}

// R-QUOTA-5: once the snapshot and the tier are warm, a read issues no query, for a
// network with no row and for one with rows alike.
func TestNetworkSidecarLimitNoRowIssuesNoQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingNetworkSidecarLimitSetup(t)
		ctx := context.Background()
		withRow := testingSidecarLimitNetwork(t, ctx)
		withoutRow := testingSidecarLimitNetwork(t, ctx)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, withRow, "dns_entries", 2, "abuse case 0005"), nil)

		// warm both tiers' caches
		NetworkTierOf(ctx, withRow)
		NetworkTierOf(ctx, withoutRow)
		ReloadNetworkSidecarLimits(ctx)

		attempts, stop := server.DenyPostgresForTest(t)
		var withRowLimit, withoutRowLimit int64
		for i := 0; i < 100; i += 1 {
			withRowLimit = NetworkSidecarLimits(ctx, withRow).Limit("dns_entries")
			withoutRowLimit = NetworkSidecarLimits(ctx, withoutRow).Limit("dns_entries")
		}
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
		connect.AssertEqual(t, withRowLimit, int64(2))
		connect.AssertEqual(t, withoutRowLimit, int64(5))
	})
}

// A network's rows leave with it, in its deletion transaction.
func TestNetworkSidecarLimitsLeaveWithTheirNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingNetworkSidecarLimitSetup(t)
		ctx := context.Background()
		networkId := testingSidecarLimitNetwork(t, ctx)
		other := testingSidecarLimitNetwork(t, ctx)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, networkId, "dns_entries", 2, "abuse case 0006"), nil)
		connect.AssertEqual(t, SetNetworkSidecarLimit(ctx, other, "dns_entries", 3, "abuse case 0007"), nil)

		server.Tx(ctx, func(tx server.PgTx) {
			removeNetworkSidecarLimitsInTx(ctx, tx, networkId)
		})
		rows, err := GetNetworkSidecarLimitRows(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(rows), 0)
		rows, err = GetNetworkSidecarLimitRows(ctx, other)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(rows), 1)
	})
}
