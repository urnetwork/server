// contract_close.yml reaches the expiry sweep's owner batches.
package work

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A payer with one balance and count reserved contracts on it, created inside
// the sweep's fresh window and past their stored deadline.
func newCloseExpiredOwnerBatchRows(t testing.TB, ctx context.Context, count int) (balanceId server.Id, contractIds []server.Id) {
	t.Helper()
	payerNetworkId, providerNetworkId := server.NewId(), server.NewId()
	sourceId, destinationId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, payerNetworkId, "synthetic-owner-batch-payer-"+payerNetworkId.String(), server.NewId())
	model.Testing_CreateNetwork(ctx, providerNetworkId, "synthetic-owner-batch-provider-"+providerNetworkId.String(), server.NewId())
	server.Tx(ctx, func(tx server.PgTx) {
		for clientId, networkId := range map[server.Id]server.Id{sourceId: payerNetworkId, destinationId: providerNetworkId} {
			server.RaisePgResult(tx.Exec(ctx, `
                INSERT INTO network_client (client_id, network_id, active, create_time, auth_time)
                VALUES ($1,$2,true,$3,$3)
            `, clientId, networkId, server.NowUtc()))
		}
	})
	model.AddBasicTransferBalance(ctx, payerNetworkId, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
	balances := model.GetActiveTransferBalances(ctx, payerNetworkId)
	if len(balances) != 1 {
		t.Fatal("synthetic payer did not have exactly one balance")
	}
	for range count {
		escrow, err := model.CreateTransferEscrow(ctx, payerNetworkId, sourceId, providerNetworkId, destinationId, 5)
		if err != nil {
			t.Fatal("synthetic reserved contract creation failed", err)
		}
		contractIds = append(contractIds, escrow.ContractId)
	}
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		for index, contractId := range contractIds {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=$1`,
				contractId, now.Add(-30*time.Minute).Add(time.Duration(index)*time.Second), now.Add(-time.Minute)))
		}
	})
	return balances[0].BalanceId, contractIds
}

// Records the contracts each admitted owned turn on the balance carried.
func observeCloseExpiredOwnerTurns(ctx context.Context, balanceId server.Id, contractIds []server.Id) (context.Context, func() [][]server.Id) {
	grant := server.NewPgOwnershipKey("transfer_balance", balanceId)
	keyContractIds := map[server.PgOwnershipKey]server.Id{}
	for _, contractId := range contractIds {
		keyContractIds[task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))] = contractId
	}
	var stateLock sync.Mutex
	admitted := [][]server.Id{}
	observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
		if event.Kind != server.PgOwnershipAdmitted || !slices.Contains(event.Keys, grant) {
			return
		}
		covered := []server.Id{}
		for _, key := range event.Keys {
			if contractId, ok := keyContractIds[key]; ok {
				covered = append(covered, contractId)
			}
		}
		if len(covered) == 0 {
			return
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		admitted = append(admitted, covered)
	})
	return observed, func() [][]server.Id {
		stateLock.Lock()
		defer stateLock.Unlock()
		return slices.Clone(admitted)
	}
}

// Without the key, one balance's due rows close in one owner batch through the
// real sweep task; expiry_owner_batch_parallel: 0 closes each row alone.
func TestCloseExpiredContractsOwnerBatchesFollowContractCloseConfig(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, c := range []struct {
			config    string
			wantTurns int
		}{
			{config: "deadline_close_parallel: 4\n", wantTurns: 1},
			{config: "expiry_owner_batch_parallel: 0\n", wantTurns: 3},
		} {
			func() {
				defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte(c.config))()
				ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 120*time.Second)
				defer cancel()
				balanceId, contractIds := newCloseExpiredOwnerBatchRows(t, ctx, 3)
				observed, admitted := observeCloseExpiredOwnerTurns(ctx, balanceId, contractIds)
				clientSession := session.Testing_CreateClientSession(observed, nil)
				defer clientSession.Cancel()
				if _, err := CloseExpiredContracts(&CloseExpiredContractsArgs{BlockSize: DefaultCloseExpiredContractsBlockSize}, clientSession); err != nil {
					t.Fatal("the sweep run failed", c.config, err)
				}
				turns := admitted()
				if len(turns) != c.wantTurns {
					t.Fatalf("config %q: the sweep took %d owned turns for %d rows of one balance, want %d", c.config, len(turns), len(contractIds), c.wantTurns)
				}
				for _, members := range turns {
					if len(members) != len(contractIds)/c.wantTurns {
						t.Fatalf("config %q: an owned turn covered %d rows, want %d", c.config, len(members), len(contractIds)/c.wantTurns)
					}
				}
				for _, contractId := range contractIds {
					if closed, terminal := model.GetContractClose(ctx, contractId); !terminal || closed.Outcome != model.ContractOutcomeSettled {
						t.Fatal("the sweep left a due row open", c.config)
					}
				}
			}()
		}
	})
}
