// Database regressions exercise actual paid/free settlement and durable replay.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Seeds only the immutable proof surface for window and corruption tests.
func addStContractUsageSnapshotTestRow(t testing.TB, ctx context.Context, closeTime time.Time, snapshot *contractUsageSnapshot) server.Id {
	t.Helper()
	contractId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id,source_id,source_network_id,destination_id,destination_network_id,
			transfer_byte_count,outcome,close_time,provider_usage,usage_origin_is_source)
			VALUES($1,$2,$3,$4,$5,1000000,'settled',$6,$7,true)
		`, contractId, server.NewId(), server.NewId(), server.NewId(), server.NewId(), closeTime, snapshot))
	})
	return contractId
}

// Billing paid/free flags and absence of escrow cannot change equal work's
// share. A split-balance contract and a same-network contract count once each.
func TestStContractUsagePaidFreeAndNoEscrowEqual(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		payerNetwork, providerNetwork, freeNetwork := server.NewId(), server.NewId(), server.NewId()
		payer, paidProvider, freeProvider, freeOrigin := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3), contractPayoutTestId(4)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{payer: payerNetwork, paidProvider: providerNetwork, freeProvider: freeNetwork, freeOrigin: freeNetwork})
		addContractPayoutTestBalance(ctx, payerNetwork, 61)
		balance := addContractPayoutTestBalance(ctx, payerNetwork, 181)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=0,subsidy_net_revenue_nano_cents=0 WHERE balance_id=$1`, balance.BalanceId))
		})
		paid, err := CreateTransferEscrow(ctx, payerNetwork, payer, providerNetwork, paidProvider, 121)
		if err != nil {
			t.Fatal(err)
		}
		free, err := CreateContractNoEscrow(ctx, freeNetwork, freeOrigin, freeNetwork, freeProvider, 121)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct{ id, source, destination server.Id }{{id: paid.ContractId, source: payer, destination: paidProvider}, {id: free, source: freeOrigin, destination: freeProvider}} {
			if err := CloseContract(ctx, c.id, c.source, 121, false); err != nil {
				t.Fatal(err)
			}
			if err := CloseContract(ctx, c.id, c.destination, 120, false); err != nil {
				t.Fatal(err)
			}
			if err := CloseContract(ctx, c.id, c.destination, 120, false); !errors.Is(err, errContractAlreadySettled) {
				t.Fatalf("duplicate outcome: %v", err)
			}
		}
		if err := SettleEscrow(ctx, paid.ContractId, ContractOutcomeSettled); err != nil {
			t.Fatal(err)
		}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		got := map[server.Id]int64{}
		for _, u := range usages {
			got[u.ClientId] = u.PayoutByteCount
		}
		if !maps.Equal(got, map[server.Id]int64{paidProvider: 120, freeProvider: 120}) {
			t.Fatalf("funding changed usage: %v", got)
		}
		networks, err := GetStEpochNetworkUsage(ctx, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, n := range networks {
			total += n.PayoutByteCount
		}
		if total != 240 {
			t.Fatalf("network demand total=%d", total)
		}
		// Rewriting monetary attribution cannot alter the subnet snapshot.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow_sweep WHERE contract_id=$1`, paid.ContractId))
		})
		usages, err = GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 2 {
			t.Fatalf("billing dependency remains: %+v,%v", usages, err)
		}
	})
}

// A free return preserves its original provider even after the transport
// normalizes it to a no-escrow contract with no companion id.
func TestStContractUsageNormalizedReturnProvider(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		networkId := server.NewId()
		provider, consumer := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{provider: networkId, consumer: networkId})
		id, err := CreateContractNoEscrowWithUsageOrigin(ctx, networkId, provider, networkId, consumer, 100, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, provider, 100, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, consumer, 100, false); err != nil {
			t.Fatal(err)
		}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 1 || usages[0].ClientId != provider || usages[0].PayoutByteCount != 100 {
			t.Fatalf("consumer received return credit: %+v,%v", usages, err)
		}
	})
}

// Free stream shares retain their original provider networks after mutable
// memberships disappear; same-network intermediaries remain eligible.
func TestStContractUsageFreeStreamHistoricalStability(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		networkId := server.NewId()
		origin, first, second := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, first: networkId, second: networkId})
		id, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, second, 121)
		if err != nil {
			t.Fatal(err)
		}
		streamId := AddToStream(ctx, id, origin, second, []server.Id{first})
		if err := SetContractStream(ctx, id, streamId, []server.Id{first}); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, origin, 121, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, second, 121, false); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_participant WHERE stream_id=$1`, streamId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$1 WHERE client_id=$2`, server.NewId(), first))
		})
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		got := map[server.Id]int64{}
		for _, u := range usages {
			if u.NetworkId != networkId {
				t.Fatal("historical network changed")
			}
			got[u.ClientId] = u.PayoutByteCount
		}
		if !maps.Equal(got, map[server.Id]int64{first: 61, second: 60}) {
			t.Fatalf("immutable split changed: %v", got)
		}
	})
}

// Expiry may close a one-sided contract for billing, but its synthetic peer
// acceptance earns zero subnet usage and carries that explicit reason.
func TestStContractUsageForcedCloseDoesNotInventBilateralReport(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		networkId := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, provider: networkId})
		id, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, provider, 100)
		if err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, id, provider, 100, true); err != nil {
			t.Fatal(err)
		}
		if _, err := ForceCloseOpenContractIds(ctx, start.Add(time.Hour), 10, 1, 0, 0); err != nil {
			t.Fatal(err)
		}
		var data []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&data))
		})
		snapshot, err := decodeContractUsageSnapshot(data)
		if err != nil || snapshot.ByteCount != 0 || snapshot.ExcludedReason != "expired_unconfirmed" {
			t.Fatalf("synthetic close gained credit: %s,%v", data, err)
		}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 0 {
			t.Fatalf("forced close poisoned epoch: %+v,%v", usages, err)
		}
	})
}

// Invalid snapshots fail the whole epoch, not only the corrupted provider.
// Unknown non-credit outcomes are excluded without inventing usage proofs.
func TestStContractUsageEpochRejectsPartialAndTamperedHistory(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Unix(1_700_000_000, 0).UTC()
		snapshot := &contractUsageSnapshot{Version: 1, ByteCount: 10, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 10}}}
		addStContractUsageSnapshotTestRow(t, ctx, start, snapshot)
		id := addStContractUsageSnapshotTestRow(t, ctx, start, nil)
		if usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err == nil || usages != nil {
			t.Fatalf("partial epoch accepted: %+v,%v", usages, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled' WHERE contract_id=$1`, id))
		})
		if usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err != nil || len(usages) != 1 {
			t.Fatalf("cancellation poisoned usage: %+v,%v", usages, err)
		}
		invalid := *snapshot
		invalid.ByteCount++
		data, _ := json.Marshal(invalid)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='settled',provider_usage=$2 WHERE contract_id=$1`, id, data))
		})
		if usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err == nil || usages != nil {
			t.Fatalf("tampered epoch accepted: %+v,%v", usages, err)
		}
	})
}

// The outcome and its usage proof share one commit; a rollback preserves
// neither, and a later normal retry produces exactly one durable snapshot.
func TestStContractUsageOutcomeRollbackAndReplay(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, provider: networkId})
		id, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, provider, 100)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',100,false),($1,'destination',100,false)`, id))
		})
		abort := errors.New("fixture aborted settlement transaction")
		recovered := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				closed, err := claimContractOutcomeInTx(ctx, tx, id, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					panic("fixture failed to own outcome")
				}
				panic(abort)
			})
		})
		if recovered != abort {
			t.Fatalf("unexpected rollback cause: %v", recovered)
		}
		var outcome *string
		var data []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT outcome,provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&outcome, &data))
		})
		if outcome != nil || len(data) != 0 {
			t.Fatalf("partial transaction published: %v,%s", outcome, data)
		}
		closed, err := settleContract(ctx, id)
		if err != nil || !closed {
			t.Fatalf("normal recovery: %t,%v", closed, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&data))
		})
		snapshot, err := decodeContractUsageSnapshot(data)
		if err != nil || snapshot.ByteCount != 100 {
			t.Fatalf("recovery lost proof: %s,%v", data, err)
		}
		closed, err = settleContract(ctx, id)
		if err != nil || closed {
			t.Fatalf("duplicate outcome gained ownership: %t,%v", closed, err)
		}
	})
}

// Explicit dispute adjudication uses the selected authenticated report rather
// than a billing average, and does not require the losing party to agree.
func TestStContractUsageAdjudicatedOutcome(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		payerNetwork, providerNetwork := server.NewId(), server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: payerNetwork, provider: providerNetwork})
		addContractPayoutTestBalance(ctx, payerNetwork, 100)
		escrow, err := CreateTransferEscrow(ctx, payerNetwork, origin, providerNetwork, provider, 100)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',11,false),($1,'destination',77,false)`, escrow.ContractId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, escrow.ContractId))
		})
		if err := SettleEscrow(ctx, escrow.ContractId, ContractOutcomeDisputeResolvedToDestination); err != nil {
			t.Fatal(err)
		}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 1 || usages[0].PayoutByteCount != 77 {
			t.Fatalf("adjudication not preserved: %+v,%v", usages, err)
		}
	})
}
