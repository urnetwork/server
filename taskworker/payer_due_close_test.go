// A payer's own activity retires its contracts that outlived their deadline,
// through the production worker, without waiting for the contract-ordered
// closers. These tests run no deadline pass or expiry sweep: on Main on
// 2026-10-26 the deadline pass's retry span had not moved all day and the
// sweep was reading contracts minted on 2026-10-18.
package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A provider endpoint and one payer network with one client.
type payerDueCloseNetworks struct {
	providerNetworkId server.Id
	providerClientId  server.Id
	payerNetworkId    server.Id
	payerClientId     server.Id
}

func newPayerDueCloseNetworks(ctx context.Context, name string) *payerDueCloseNetworks {
	n := &payerDueCloseNetworks{
		providerNetworkId: server.NewId(), providerClientId: server.NewId(),
		payerNetworkId: server.NewId(), payerClientId: server.NewId(),
	}
	model.Testing_CreateNetwork(ctx, n.providerNetworkId, name+"-provider", server.NewId())
	model.Testing_CreateDevice(ctx, n.providerNetworkId, server.NewId(), n.providerClientId, "synthetic-provider", "synthetic")
	model.Testing_CreateNetwork(ctx, n.payerNetworkId, name+"-payer", server.NewId())
	model.Testing_CreateDevice(ctx, n.payerNetworkId, server.NewId(), n.payerClientId, "synthetic-payer", "synthetic")
	return n
}

// A daily free grant of byteCount over the window.
func (self *payerDueCloseNetworks) addFreeGrant(ctx context.Context, byteCount model.ByteCount, startTime time.Time, endTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(model.AddGrantTransferBalanceInTx(tx, ctx, self.payerNetworkId, model.GrantKindFree, byteCount, startTime, endTime))
	})
}

// Contracts admitted through Redis, each closed by its source with nothing
// used and never by its provider: a killed client's contracts.
func (self *payerDueCloseNetworks) abandonContracts(t testing.TB, ctx context.Context, count int, byteCount model.ByteCount) []server.Id {
	t.Helper()
	contractIds := []server.Id{}
	for range count {
		escrow, err := model.CreateTransferEscrow(ctx, self.payerNetworkId, self.payerClientId, self.providerNetworkId, self.providerClientId, byteCount)
		if err != nil {
			t.Fatal("contract admission failed", err)
		}
		server.Raise(model.CloseContract(ctx, escrow.ContractId, self.payerClientId, 0, false))
		contractIds = append(contractIds, escrow.ContractId)
	}
	return contractIds
}

// The payer's balance as the apps read it.
func (self *payerDueCloseNetworks) subscriptionBalance(t testing.TB, ctx context.Context) *controller.SubscriptionBalanceResult {
	t.Helper()
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: self.payerNetworkId,
		ClientId:  &self.payerClientId,
		UserId:    server.NewId(),
	})
	result, err := controller.SubscriptionBalance(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Runs the production worker until no task is due, within a bound.
func evalPayerDueCloseTasks(t testing.TB, ctx context.Context) {
	t.Helper()
	worker, err := InitTaskWorkerForProfile(ctx, nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	for range 64 {
		finished, retried, posts, err := worker.EvalTasks(8)
		if err != nil {
			t.Fatal("production worker failed", err)
		}
		if len(finished)+len(retried)+len(posts) == 0 {
			return
		}
	}
}

// Every listed contract settled at its stored deadline with nothing paid, as
// a contract whose only report used nothing settles, and released its escrow.
func requirePayerDueContractsRetired(t testing.TB, ctx context.Context, contractIds []server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var settled, released, atDeadline int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=0),
			(SELECT count(*) FROM transfer_escrow e JOIN transfer_contract c USING (contract_id)
				WHERE e.contract_id=ANY($1) AND e.settle_time=c.expiration_time)`,
			contractIds).Scan(&settled, &released, &atDeadline))
		if settled != len(contractIds) || released != len(contractIds) || atDeadline != len(contractIds) {
			t.Fatalf("contracts past their deadline were not retired: settled=%d released=%d at_deadline=%d of %d",
				settled, released, atDeadline, len(contractIds))
		}
	})
}

// Production shape (kayceleow, sparkmindsgoldlion and primary, October 2026):
// contracts that killed clients left open stayed open for days on daily
// grants that had ended, and the apps showed their bytes as Pending. A
// balance read that shows Pending no current grant reserves now asks for the
// payer's drain, and the production worker retires the contracts at their
// stored deadlines and releases their escrow. On the unchanged server the
// read asked for nothing and the contracts stayed open.
func TestBalanceReadRetiresPendingStrandedOnAnEndedGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		n := newPayerDueCloseNetworks(ctx, "synthetic-stranded-pending")
		now := server.NowUtc()
		// yesterday's grant, while it ran
		n.addFreeGrant(ctx, 30*model.Mib, now.Add(-time.Hour), now.Add(time.Hour))
		contractIds := n.abandonContracts(t, ctx, 3, 1*model.Mib)
		// a day later the grant has ended and the deadlines passed a day ago
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=start_time-interval '25 hours',
				end_time=end_time-interval '25 hours' WHERE network_id=$1`, n.payerNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=create_time-interval '25 hours',
				expiration_time=expiration_time-interval '25 hours' WHERE contract_id=ANY($1)`, contractIds))
		})
		n.addFreeGrant(ctx, 30*model.Mib, now.Add(-time.Hour), now.Add(24*time.Hour))

		before := n.subscriptionBalance(t, ctx)
		if before.OpenTransferByteCount != 3*model.Mib {
			t.Fatal("the abandoned contracts were not Pending", before.OpenTransferByteCount)
		}
		evalPayerDueCloseTasks(t, ctx)

		requirePayerDueContractsRetired(t, ctx, contractIds)
		if pending := model.GetOpenTransferByteCount(ctx, n.payerNetworkId); pending != 0 {
			t.Fatal("Pending stayed after the drain", pending)
		}
		if after := n.subscriptionBalance(t, ctx); after.OpenTransferByteCount != 0 || after.BalanceByteCount != 30*model.Mib {
			t.Fatal("the balance read still showed the retired contracts", after.OpenTransferByteCount, after.BalanceByteCount)
		}
	})
}

// Production shape (7830, 8050): a free payer's contracts filled its daily
// grant and outlived their deadline while their reservations still held the
// grant's credit, so every new contract was refused for insufficient balance
// until a closer reached them. The refusal now asks for the payer's drain;
// the production worker retires the contracts, the debit flush releases their
// reservations, and the next admission succeeds. On the unchanged server the
// refusal asked for nothing, the contracts stayed open and the payer stayed
// refused.
func TestRefusedAdmissionRetiresThePayersDueContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		n := newPayerDueCloseNetworks(ctx, "synthetic-refused-admission")
		now := server.NowUtc()
		n.addFreeGrant(ctx, 3*model.Mib, now.Add(-2*time.Hour), now.Add(22*time.Hour))
		contractIds := n.abandonContracts(t, ctx, 3, 1*model.Mib)
		// the deadlines passed an hour ago; the reservations' lease has not
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=create_time-interval '2 hours',
				expiration_time=expiration_time-interval '2 hours' WHERE contract_id=ANY($1)`, contractIds))
		})
		if _, err := model.CreateTransferEscrow(ctx, n.payerNetworkId, n.payerClientId, n.providerNetworkId, n.providerClientId, 1*model.Mib); err == nil {
			t.Fatal("the full grant admitted another contract")
		}

		evalPayerDueCloseTasks(t, ctx)
		requirePayerDueContractsRetired(t, ctx, contractIds)
		// The always-running debit flush chains apply the retirements' journal
		// rows and release their reservations.
		for shard := range model.TransferDebitShardCount {
			if _, err := model.FlushTransferDebits(ctx, shard, nil, 64); err != nil {
				t.Fatal("debit flush failed", shard, err)
			}
		}
		if _, err := model.CreateTransferEscrow(ctx, n.payerNetworkId, n.payerClientId, n.providerNetworkId, n.providerClientId, 1*model.Mib); err != nil {
			t.Fatal("the payer was still refused after its due contracts were retired", err)
		}
	})
}

// Both profiles run the drain a request publishes, and the production owner
// lets each worker run one at a time.
func TestClosePayerDueContractsRegisteredInBothProfilesAndLimitedPerWorker(t *testing.T) {
	name := model.NewClosePayerDueContractsTaskTarget().TargetFunctionName()
	if name != task.NewTaskTarget(model.ClosePayerDueContractsTask).TargetFunctionName() {
		t.Fatal("the drain target has another function", name)
	}
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if !present {
			t.Fatalf("profile %q does not register %s", profile, name)
		}
		if limit := taskWorkerSettingsForProfile(nil, profile).TargetClaimLimits[name]; limit != 1 {
			t.Fatalf("profile %q runs %d drains per worker at once, want 1", profile, limit)
		}
	}
}
