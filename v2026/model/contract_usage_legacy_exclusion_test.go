// Explicit historical debt cannot create credit or hide a new missing snapshot.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Wire fixtures remain independent of the production decoder and use only
// synthetic identities and a synthetic reviewed cohort digest.
func contractUsageLegacyTestWire(contractId server.Id, closedAt time.Time, debt ByteCount) map[string]any {
	return map[string]any{"version": 1, "byte_count": 0, "providers": []any{}, "excluded_reason": "legacy_usage_unavailable",
		"legacy_exclusion": map[string]any{"repair_manifest_sha256": "sha256:" + strings.Repeat("ab", 32),
			"contract_id": contractId.String(), "epoch": 17, "closed_at": closedAt.Format(time.RFC3339Nano),
			"retained_report_minimum": debt, "final_acceptance": false}}
}

// The original missing-proof failure becomes an explicit zero-credit debt;
// authenticated neighboring work is conserved and future omissions still fail.
func TestStContractUsageLegacyExclusionRetainsUncreditedDebt(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Unix(1_700_000_000, 0).UTC()
		provider, network := server.NewId(), server.NewId()
		addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 121,
			Providers: []contractProviderUsage{{ClientId: provider, NetworkId: network, ByteCount: 121}}})
		id := addStContractUsageSnapshotTestRow(t, ctx, start, nil)
		addStContractUsageSnapshotTestRow(t, ctx, start.Add(2*time.Hour), nil)
		server.ApplyDbMigrations(ctx)
		if usages, err := GetStEpochProviderUsageAtEpoch(ctx, 17, start, start.Add(time.Hour)); err == nil || usages != nil {
			t.Fatalf("missing historical usage accepted: %+v, %v", usages, err)
		}
		data, err := json.Marshal(contractUsageLegacyTestWire(id, start, 987))
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1 AND provider_usage IS NULL`, id, data))
		})
		usages, err := GetStEpochProviderUsageAtEpoch(ctx, 17, start, start.Add(time.Hour))
		if err != nil || len(usages) != 1 || usages[0].ClientId != provider || usages[0].NetworkId != network || usages[0].PayoutByteCount != 121 {
			t.Fatalf("explicit debt must preserve only verified neighbor credit: %+v, %v", usages, err)
		}
		if usages, err := GetStEpochProviderUsageAtEpoch(ctx, 18, start, start.Add(time.Hour)); err == nil || usages != nil {
			t.Fatalf("debt borrowed another payout epoch: %+v, %v", usages, err)
		}
		if usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err == nil || usages != nil {
			t.Fatalf("unscoped reader admitted historical debt: %+v, %v", usages, err)
		}
		snapshot, err := decodeContractUsageSnapshot(data)
		if err != nil || snapshot.ByteCount != 0 || len(snapshot.Providers) != 0 {
			t.Fatalf("debt credited: %+v, %v", snapshot, err)
		}
		roundTrip, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var retained map[string]any
		if err := json.Unmarshal(roundTrip, &retained); err != nil {
			t.Fatal(err)
		}
		legacy, ok := retained["legacy_exclusion"].(map[string]any)
		if !ok || legacy["retained_report_minimum"] != float64(987) || legacy["final_acceptance"] != false {
			t.Fatalf("lost explicit uncredited debt: %s", roundTrip)
		}
		if usages, err := GetStEpochProviderUsageAtEpoch(ctx, 17, start.Add(time.Hour), start.Add(3*time.Hour)); err == nil || usages != nil {
			t.Fatalf("future missing snapshot inherited historical repair: %+v, %v", usages, err)
		}
	})
}

// A typed exclusion is bound to its exact terminal contract and cannot be
// transplanted onto another contract or a changed settlement timestamp.
func TestStContractUsageLegacyExclusionRejectsForeignTerminalOwner(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	// These malformed historical rows must remain rejected by the reader;
	// the prospective database guard now prevents creating new copies.
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Unix(1_700_000_000, 0).UTC()
		for i := 0; i < 2; i++ {
			closedAt := start.Add(time.Duration(i) * time.Hour)
			id := addStContractUsageSnapshotTestRow(t, ctx, closedAt, nil)
			wire := contractUsageLegacyTestWire(server.NewId(), closedAt, 1)
			if i == 1 {
				wire = contractUsageLegacyTestWire(id, closedAt.Add(time.Second), 1)
			}
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1`, id, data))
			})
		}
		server.ApplyDbMigrations(ctx)
		for i := 0; i < 2; i++ {
			closedAt := start.Add(time.Duration(i) * time.Hour)
			if usages, err := GetStEpochProviderUsageAtEpoch(ctx, 17, closedAt, closedAt.Add(time.Hour)); err == nil || usages != nil {
				t.Fatalf("foreign terminal repair accepted: %+v, %v", usages, err)
			}
		}
	})
}

// Presence, zero credit, canonical custody hash and explicit nonacceptance
// are independent requirements; ordinary and expiry snapshots cannot borrow it.
func TestStContractUsageLegacyExclusionRejectsMalformedProof(t *testing.T) {
	for _, mutate := range []func(map[string]any, map[string]any){
		func(w, l map[string]any) { delete(w, "legacy_exclusion") },
		func(w, l map[string]any) { delete(l, "repair_manifest_sha256") },
		func(w, l map[string]any) { l["repair_manifest_sha256"] = "sha256:" + strings.Repeat("AB", 32) },
		func(w, l map[string]any) { l["contract_id"] = (server.Id{}).String() },
		func(w, l map[string]any) { l["epoch"] = 0 },
		func(w, l map[string]any) { delete(l, "closed_at") },
		func(w, l map[string]any) { delete(l, "retained_report_minimum") },
		func(w, l map[string]any) { l["retained_report_minimum"] = -1 },
		func(w, l map[string]any) { delete(l, "final_acceptance") },
		func(w, l map[string]any) { l["final_acceptance"] = true },
		func(w, l map[string]any) { w["excluded_reason"] = "" },
		func(w, l map[string]any) { w["excluded_reason"] = "expired_unconfirmed" },
		func(w, l map[string]any) { w["expiry"] = map[string]any{} },
		func(w, l map[string]any) { w["byte_count"] = 1 },
		func(w, l map[string]any) {
			w["providers"] = []any{map[string]any{"client_id": contractPayoutTestId(1), "network_id": contractPayoutTestId(2), "byte_count": 0}}
		},
		func(w, l map[string]any) { l["unreviewed"] = true },
	} {
		wire := contractUsageLegacyTestWire(contractPayoutTestId(9), time.Unix(1_700_000_000, 0).UTC(), 15)
		mutate(wire, wire["legacy_exclusion"].(map[string]any))
		data, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeContractUsageSnapshot(data); err == nil {
			t.Fatalf("malformed repair accepted: %s", data)
		}
	}
}
