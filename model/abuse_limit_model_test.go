package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.7's limits (abuse_limit_model.go; ABUSE-M5-19): the tightening's refusals beside their allowed twins, the row
// it writes and the lift's clear, and the closure-growth case, on the private stack in a build with sidecar_pending.

// pro.yml's numbers the cases read: an eraser's allowance and the floor under it, and the day's statements.
func testAbuseLimitSetup(t testing.TB) {
	t.Helper()
	t.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
		"storage":                   8 * 1024 * 1024,
		"evs_closure_bytes_per_day": 64 * 1024 * 1024,
		"evs_closure_bytes_floor":   4 * 1024 * 1024,
		"evs_closure_uploads_per_s": 4,
		"config_statements_per_day": 1000,
	}))
}

// A tightening's target reads back as written, and a target that is not <limit_key>=<limit_value> is refused.
func TestAbuseLimitTightenTargetRoundTrips(t *testing.T) {
	target := AbuseLimitTightenTarget("evs_closure_bytes_per_day", 4*1024*1024)
	limitKey, limitValue, err := ParseAbuseLimitTightenTarget(target)
	if err != nil || limitKey != "evs_closure_bytes_per_day" || limitValue != 4*1024*1024 {
		t.Fatalf("%s: %s %d %v", target, limitKey, limitValue, err)
	}
	for _, bad := range []string{"", "evs_closure_bytes_per_day", "=4", "evs_closure_bytes_per_day=four", "shop.root.example"} {
		if _, _, err := ParseAbuseLimitTightenTarget(bad); !errors.Is(err, ErrAbuseLimitRefused) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// Each refused row beside its allowed twin: below the floor and at it, the floor's own key and the pacing key,
// which no row sets, a forwarding key, which no table has, and a lower day's statements.
func TestAbuseLimitTightenRefusesWhatQuotaRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testAbuseLimitSetup(t)
		caseId := server.NewId()
		floor := int64(4 * 1024 * 1024)
		for _, check := range []struct {
			name       string
			limitKey   string
			limitValue int64
			allowed    bool
		}{
			{"below the floor", "evs_closure_bytes_per_day", floor - 1, false},
			{"at the floor", "evs_closure_bytes_per_day", floor, true},
			{"setting the floor", "evs_closure_bytes_floor", floor, false},
			{"setting the pacing", "evs_closure_uploads_per_s", 1, false},
			{"a forwarding key", "forwarding_bytes_per_day", 0, false},
			{"a socket key", "sockets", 0, false},
			{"a key §21 does not tighten", "storage", 1024, false},
			{"fewer statements", "config_statements_per_day", 2, true},
		} {
			err := ValidateAbuseLimitTighten(check.limitKey, check.limitValue, caseId)
			if (err == nil) != check.allowed {
				t.Errorf("%s: %v", check.name, err)
			}
			if err != nil && !errors.Is(err, ErrAbuseLimitRefused) {
				t.Errorf("%s: %v is not a refusal", check.name, err)
			}
		}
	})
}

// A row written for a case cites it and stands in the network's limits; a lift clears only a row still citing
// that case.
func TestAbuseLimitRowCitesItsCaseAndALiftClearsIt(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testAbuseLimitSetup(t)
		defer Testing_SetNetworkSidecarLimitSettings(&NetworkSidecarLimitSettings{SnapshotTtl: time.Hour, ReloadTimeout: 30 * time.Second})()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "limit-"+networkId.String()[24:], server.NewId())
		caseId := server.NewId()
		otherCaseId := server.NewId()
		target := AbuseLimitTightenTarget("evs_closure_bytes_per_day", 4*1024*1024)

		if err := SetAbuseLimitRow(ctx, networkId, caseId, target); err != nil {
			t.Fatal(err)
		}
		rows, err := GetNetworkSidecarLimitRows(ctx, networkId)
		if err != nil || len(rows) != 1 || rows[0].Reason != AbuseLimitReason(caseId) || rows[0].LimitValue != 4*1024*1024 {
			t.Fatalf("rows %+v, %v", rows, err)
		}
		if got := NetworkSidecarLimits(ctx, networkId).Limit("evs_closure_bytes_per_day"); got != 4*1024*1024 {
			t.Fatalf("the network's allowance reads %d", got)
		}
		// a lift of another case leaves this case's row
		if cleared, err := ClearAbuseLimitRow(ctx, networkId, otherCaseId, target); err != nil || cleared {
			t.Fatalf("another case's lift: %t, %v", cleared, err)
		}
		if cleared, err := ClearAbuseLimitRow(ctx, networkId, caseId, target); err != nil || !cleared {
			t.Fatalf("the case's lift: %t, %v", cleared, err)
		}
		if rows, _ := GetNetworkSidecarLimitRows(ctx, networkId); len(rows) != 0 {
			t.Fatalf("rows after the lift %+v", rows)
		}
		if err := SetAbuseLimitRow(ctx, networkId, caseId, AbuseLimitTightenTarget("evs_closure_bytes_floor", 1)); !errors.Is(err, ErrAbuseLimitRefused) {
			t.Fatalf("a floor row: %v", err)
		}
	})
}

// A network's quota state row.
func testAbuseQuotaState(t testing.TB, ctx context.Context, networkId server.Id, storedByteCount int64, storageOver bool) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO network_quota_state (network_id, stored_byte_count, storage_over)
			VALUES ($1, $2, $3)
			ON CONFLICT (network_id) DO UPDATE SET stored_byte_count = $2, storage_over = $3
		`, networkId, storedByteCount, storageOver))
	})
}

// The closure-growth cases of network.
func testAbuseClosureGrowthCases(t testing.TB, ctx context.Context, networkId server.Id) (count int, actions int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_case WHERE network_id = $1 AND reporter_class = $2`, networkId, string(AbuseReporterClassClosureGrowth)).Scan(&count))
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_action WHERE network_id = $1`, networkId).Scan(&actions))
	})
	return
}

// FS-47's §21 case at the row level: a network whose closures today take it past its storage limit gets one case,
// no action, and no second while it is open; one under its limit, one not over, and one that took no closure bytes
// today get none.
func TestAbuseClosureGrowthOpensOneCaseAndNoAction(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testAbuseLimitSetup(t)
		limit := int64(8 * 1024 * 1024)
		network := func(name string) server.Id {
			networkId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, name+networkId.String()[24:], server.NewId())
			return networkId
		}
		growing := network("growing-")
		underLimit := network("under-")
		notOver := network("notover-")
		noClosures := network("noclosure-")
		testAbuseQuotaState(t, ctx, growing, limit+1024, true)
		testAbuseQuotaState(t, ctx, underLimit, limit, true)
		testAbuseQuotaState(t, ctx, notOver, limit+1024, false)
		testAbuseQuotaState(t, ctx, noClosures, limit+1024, true)
		defer Testing_SetAbuseClosureBytesToday(func(ctx context.Context, networkId server.Id, now time.Time) (int64, error) {
			if networkId == noClosures {
				return 0, nil
			}
			return 1024 * 1024, nil
		})()

		now := server.NowUtc()
		settings := DefaultAbuseSettings()
		result := RunAbuseClosureGrowth(ctx, now, settings)
		if result.OpenedCount != 1 || result.FailedCount != 0 {
			t.Fatalf("%+v", result)
		}
		if count, actions := testAbuseClosureGrowthCases(t, ctx, growing); count != 1 || actions != 0 {
			t.Fatalf("the growing network: %d cases, %d actions", count, actions)
		}
		for _, networkId := range []server.Id{underLimit, notOver, noClosures} {
			if count, _ := testAbuseClosureGrowthCases(t, ctx, networkId); count != 0 {
				t.Fatalf("a case for a network that is not growing past its limit")
			}
		}
		// a day later, the case still open: no second
		if again := RunAbuseClosureGrowth(ctx, now.Add(24*time.Hour), settings); again.OpenedCount != 0 {
			t.Fatalf("a second pass opened %d", again.OpenedCount)
		}
		if count, _ := testAbuseClosureGrowthCases(t, ctx, growing); count != 1 {
			t.Fatalf("%d cases after the second pass", count)
		}
	})
}

// The pass runs late in each UTC day: the next ClosureGrowthPassTime after now.
func TestNextAbuseClosureGrowthTimeIsLateInTheDay(t *testing.T) {
	settings := DefaultAbuseSettings()
	morning := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if next := NextAbuseClosureGrowthTime(morning, settings); !next.Equal(time.Date(2026, 3, 2, 23, 30, 0, 0, time.UTC)) {
		t.Fatalf("from the morning: %s", next)
	}
	late := time.Date(2026, 3, 2, 23, 45, 0, 0, time.UTC)
	if next := NextAbuseClosureGrowthTime(late, settings); !next.Equal(time.Date(2026, 3, 3, 23, 30, 0, 0, time.UTC)) {
		t.Fatalf("from late: %s", next)
	}
}
