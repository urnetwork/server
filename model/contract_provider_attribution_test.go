// Provider attribution regressions exercise actual escrow settlement before
// subnet usage and head exclusion consume the durable payout rows.
package model

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two service clients sharing one payout network remain independent subnet
// providers, even though their ordinary account payment is aggregated.
func TestContractPayoutPreservesSharedNetworkProviderUsage(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		originId, firstId, secondId := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{
			originId: originNetworkId, firstId: providerNetworkId, secondId: providerNetworkId,
		})
		balance := addContractPayoutTestBalance(ctx, originNetworkId, 121)
		escrow, err := CreateTransferEscrow(ctx, originNetworkId, originId, providerNetworkId, secondId, 121)
		if err != nil {
			t.Fatal(err)
		}
		streamId := AddToStream(ctx, escrow.ContractId, originId, secondId, []server.Id{firstId})
		if err := SetContractStream(ctx, escrow.ContractId, streamId, []server.Id{firstId}); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, escrow.ContractId, originId, 121, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, escrow.ContractId, secondId, 121, false); err != nil {
			t.Fatal(err)
		}

		got := map[server.Id]int64{}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, usage := range usages {
			if usage.NetworkId != providerNetworkId {
				t.Fatalf("provider %s changed network attribution", usage.ClientId)
			}
			got[usage.ClientId] = usage.PayoutByteCount
		}
		want := map[server.Id]int64{firstId: 61, secondId: 60}
		if !maps.Equal(got, want) {
			t.Fatalf("per-client settled usage = %v, want %v", got, want)
		}
		wantAccounts := map[server.Id]contractPayoutTestAmount{providerNetworkId: {byteCount: 121, payout: 121}}
		if gotAccounts := contractPayoutTestAmounts(t, ctx, escrow.ContractId); !maps.Equal(gotAccounts, wantAccounts) {
			t.Fatalf("network settlement = %v, want %v", gotAccounts, wantAccounts)
		}
		assertContractPayoutTestAccounts(t, ctx, []server.Id{originNetworkId, providerNetworkId}, wantAccounts)
		assertContractPayoutTestBalanceConsumed(t, ctx, balance.BalanceId, escrow.ContractId, 121)
		// A retry cannot duplicate the provider shares or account payment.
		if err := CloseContract(ctx, escrow.ContractId, secondId, 121, false); err == nil || !strings.Contains(err.Error(), "already closed with outcome settled") {
			t.Fatalf("duplicate close did not report its terminal settlement: %v", err)
		}
		if err := SettleEscrow(ctx, escrow.ContractId, ContractOutcomeSettled); err != nil {
			t.Fatalf("idempotent settlement retry: %v", err)
		}
		assertContractPayoutTestProviderAllocations(t, ctx, escrow.ContractId, map[server.Id]int64{firstId: 61, secondId: 60})
		assertContractPayoutTestAccounts(t, ctx, []server.Id{originNetworkId, providerNetworkId}, wantAccounts)
		// Membership is mutable after settlement; the saved shares and original
		// account network remain the immutable evidence for this epoch.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $1 WHERE client_id = $2`, server.NewId(), firstId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_participant WHERE stream_id = $1`, streamId))
		})
		usages, err = GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 2 {
			t.Fatalf("historical snapshot changed with membership: %+v, %v", usages, err)
		}
		for _, usage := range usages {
			if usage.NetworkId != providerNetworkId || usage.PayoutByteCount != want[usage.ClientId] {
				t.Fatalf("historical provider share changed: %+v", usage)
			}
		}
	})
}

// Every network sweep retains canonical, independent client amounts that
// reconcile exactly to both its byte and monetary totals.
func assertContractPayoutTestProviderAllocations(t testing.TB, ctx context.Context, contractId server.Id, want map[server.Id]int64) {
	t.Helper()
	got := map[server.Id]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT payout_byte_count, payout_net_revenue_nano_cents, provider_payouts FROM transfer_escrow_sweep WHERE contract_id = $1`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var byteCount, revenue int64
				var value []byte
				server.Raise(rows.Scan(&byteCount, &revenue, &value))
				var providers []contractProviderPayout
				if err := json.Unmarshal(value, &providers); err != nil || len(providers) == 0 {
					t.Fatalf("missing provider allocations: %s, %v", value, err)
				}
				var allocatedBytes, allocatedRevenue int64
				for index, provider := range providers {
					if index > 0 && !providers[index-1].ClientId.Less(provider.ClientId) {
						t.Fatal("provider allocation identities are not unique and canonical")
					}
					allocatedBytes += provider.PayoutByteCount
					allocatedRevenue += provider.PayoutNanoCents
					got[provider.ClientId] += provider.PayoutByteCount
				}
				if allocatedBytes != byteCount || allocatedRevenue != revenue {
					t.Fatalf("provider allocations do not conserve network totals: %d/%d versus %d/%d", allocatedBytes, allocatedRevenue, byteCount, revenue)
				}
			}
		})
	})
	if !maps.Equal(got, want) {
		t.Fatalf("durable provider allocations = %v, want %v", got, want)
	}
}

// Inserts one bounded historical settlement with exact endpoint identities.
// A nil allocation models the schema before per-client snapshots existed.
func addStProviderUsageTestSweep(t testing.TB, ctx context.Context, originNetworkId, providerNetworkId, providerId server.Id, sweepTime time.Time, allocations any) server.Id {
	t.Helper()
	originId := server.NewId()
	contractId, err := CreateContractNoEscrow(ctx, originNetworkId, originId, providerNetworkId, providerId, 121)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
            INSERT INTO transfer_escrow_sweep (contract_id, balance_id, network_id, destination_id,
                payout_byte_count, payout_net_revenue_nano_cents, sweep_time, provider_payouts)
            VALUES ($1, $2, $3, $4, 121, 121, $5, $6)
        `, contractId, server.NewId(), providerNetworkId, providerId, sweepTime, allocations))
	})
	return contractId
}

// Legacy single-provider and current multi-provider history coexist in one
// epoch. Half-open boundaries, unchanged network totals, and whole-row cleanup
// remain exact without inventing an allocation for unrecorded legacy clients.
func TestStEpochProviderUsageMixedLegacyAndCurrentHistory(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Unix(1_700_000_000, 0).UTC()
		end := start.Add(time.Hour)
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		legacyId, firstId, secondId := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, legacyId, start, nil)
		modernContractId := addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, firstId, start.Add(time.Minute), []contractProviderPayout{
			{ClientId: firstId, PayoutByteCount: 61, PayoutNanoCents: 61},
			{ClientId: secondId, PayoutByteCount: 60, PayoutNanoCents: 60},
		})
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, server.NewId(), start.Add(-time.Microsecond), nil)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, server.NewId(), end, nil)
		usages, err := GetStEpochProviderUsage(ctx, start, end)
		if err != nil {
			t.Fatal(err)
		}
		got := map[server.Id]int64{}
		for _, usage := range usages {
			got[usage.ClientId] = usage.PayoutByteCount
		}
		want := map[server.Id]int64{legacyId: 121, firstId: 61, secondId: 60}
		if !maps.Equal(got, want) {
			t.Fatalf("mixed history = %v, want %v", got, want)
		}
		networkUsage := GetStEpochNetworkUsage(ctx, start, end)
		if len(networkUsage) != 1 || networkUsage[0].PayoutByteCount != 242 {
			t.Fatalf("provider expansion changed network demand accounting: %+v", networkUsage)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, modernContractId))
		})
		if removed, _, done := SweepOrphanContractData(ctx, SweepOrphanCursor{}, 0, 1); !done || removed != 1 {
			t.Fatalf("orphan sweep removed=%d done=%t, want one completed row", removed, done)
		}
		usages, err = GetStEpochProviderUsage(ctx, start, end)
		if err != nil || len(usages) != 1 || usages[0].ClientId != legacyId || usages[0].PayoutByteCount != 121 {
			t.Fatalf("orphan cleanup left provider attribution behind: %+v, %v", usages, err)
		}
	})
}

// A pre-snapshot stream sweep may combine multiple same-network providers.
// Reject that ambiguity even alongside valid history; the minimum stored
// destination id is not evidence that its client carried every credited byte.
func TestStEpochProviderUsageRejectsAmbiguousLegacyAggregate(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Unix(1_700_000_000, 0).UTC()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		firstId, secondId := contractPayoutTestId(2), contractPayoutTestId(3)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, server.NewId(), start, nil)
		contractId := addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, firstId, start, nil)
		streamId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET stream_id = $1 WHERE contract_id = $2`, streamId, contractId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_participant (stream_id, client_id, network_id) VALUES ($1, $2, $3)`, streamId, secondId, providerNetworkId))
		})
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "ambiguous") || usages != nil {
			t.Fatalf("ambiguous legacy history produced partial credit: %+v, %v", usages, err)
		}
	})
}

// SetContractStream can republish an intermediary after its account network
// changes. That current membership is not proof of which clients contributed
// to an older aggregate; the legacy row must remain uncreditable.
func TestStEpochProviderUsageRejectsLegacyStreamAfterMembershipRewrite(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Unix(1_700_000_000, 0).UTC()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		firstId, secondId := contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{firstId: providerNetworkId, secondId: providerNetworkId})
		contractId := addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, firstId, start, nil)
		streamId := server.NewId()
		if err := SetContractStream(ctx, contractId, streamId, []server.Id{secondId}); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $1 WHERE client_id = $2`, server.NewId(), secondId))
		})
		if err := SetContractStream(ctx, contractId, streamId, []server.Id{secondId}); err != nil {
			t.Fatal(err)
		}
		if usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err == nil || usages != nil {
			t.Fatalf("mutable membership laundered ambiguous legacy credit: %+v, %v", usages, err)
		}
	})
}

// Modern malformed allocations cannot masquerade as legacy data. Missing or
// duplicate clients, negative/missing fields, and either conservation error
// reject the entire epoch, including any otherwise valid rows.
func TestStEpochProviderUsageRejectsInvalidAllocationsWithoutLegacyFallback(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Unix(1_700_000_000, 0).UTC()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		providerId := contractPayoutTestId(2)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, server.NewId(), start, nil)
		contractId := addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, start, nil)
		for _, value := range []string{
			`[{"client_id":"` + providerId.String() + `","payout_byte_count":120,"payout_nano_cents":121}]`,
			`[{"client_id":"` + providerId.String() + `","payout_byte_count":121,"payout_nano_cents":120}]`,
			`[{"client_id":"` + providerId.String() + `","payout_byte_count":61,"payout_nano_cents":61},{"client_id":"` + providerId.String() + `","payout_byte_count":60,"payout_nano_cents":60}]`,
			`[{"client_id":"00000000-0000-0000-0000-000000000000","payout_byte_count":121,"payout_nano_cents":121}]`,
			`[{"client_id":"` + providerId.String() + `","payout_byte_count":-1,"payout_nano_cents":121}]`,
			`[{"client_id":"` + providerId.String() + `","payout_byte_count":121}]`,
			`[{"client_id":"not-a-client","payout_byte_count":121,"payout_nano_cents":121}]`,
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET provider_payouts = $1::jsonb WHERE contract_id = $2`, value, contractId))
			})
			if usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err == nil || usages != nil {
				t.Fatalf("invalid allocation produced partial or legacy credit: %s: %+v, %v", value, usages, err)
			}
		}
	})
}
