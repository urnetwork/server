package model

import (
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

// A sender's zero final while the receiver has not reported settles the
// unused contract in that report and frees its reservation (owner,
// 2026-10-28). A sender that used the contract keeps waiting for the
// receiver, which is asked for its final.
func TestSenderZeroFinalSettlesUnusedContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)

		newContract := func() server.Id {
			escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 40)
			server.RunPosts(ctx, posts...)
			return escrow.ContractId
		}
		state := func(contractId server.Id) (settled bool, destinationFinal bool, destinationUsed ByteCount) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,
					COALESCE((SELECT NOT checkpoint FROM contract_close WHERE contract_id=$1 AND party=$2), false),
					COALESCE((SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party=$2), -1)
					FROM transfer_contract WHERE contract_id=$1`, contractId, ContractPartyDestination).Scan(&settled, &destinationFinal, &destinationUsed))
			})
			return
		}

		// identified report: the unused contract settles, its reservation is
		// free, and the receiver is not asked
		unused := newContract()
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 40 {
			t.Fatal("the fixture did not reserve the contract")
		}
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: unused, ClientId: f.sourceId}); err != nil {
			t.Fatal(err)
		}
		if settled, final, used := state(unused); !settled || !final || used != 0 {
			t.Fatalf("unused contract: settled=%t receiver_final=%t receiver_bytes=%d, want a settled zero final", settled, final, used)
		}
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 0 {
			t.Fatalf("the unused contract still reserves %d bytes", reserved)
		}
		if requests := TakeClientCloseRequests(ctx, f.destinationId, ClientCloseTakeLimit); !requests.Empty() {
			t.Fatal("a settled unused contract asked the receiver to close")
		}
		// a receiver report after the settlement is the benign duplicate close
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: unused, ClientId: f.destinationId, AckedByteCount: 5}); !errors.Is(err, errContractAlreadySettled) {
			t.Fatalf("a receiver report after the settlement: %v, want already settled", err)
		}

		// a client's legacy id-less report settles the same (owner, 2026-10-28)
		legacy := newContract()
		if _, err := CloseClientContractUsage(ctx, legacy, f.sourceId, 0, false); err != nil {
			t.Fatal(err)
		}
		if settled, final, _ := state(legacy); !settled || !final {
			t.Fatal("a client's legacy zero final did not settle the unused contract")
		}
		if requests := TakeClientCloseRequests(ctx, f.destinationId, ClientCloseTakeLimit); !requests.Empty() {
			t.Fatal("a client's legacy zero final asked the receiver to close")
		}

		// an internal legacy close keeps the two-final rule
		internal := newContract()
		if err := CloseContract(ctx, internal, f.sourceId, 0, false); err != nil {
			t.Fatal(err)
		}
		if settled, final, _ := state(internal); settled || final {
			t.Fatal("an internal legacy zero final settled the contract")
		}

		// a sender that used the contract waits for the receiver, which is asked
		usedContract := newContract()
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: usedContract, ClientId: f.sourceId, AckedByteCount: 1}); err != nil {
			t.Fatal(err)
		}
		if settled, _, receiverBytes := state(usedContract); settled || receiverBytes != -1 {
			t.Fatal("a used contract settled without its receiver")
		}
		if requests := TakeClientCloseRequests(ctx, f.destinationId, ClientCloseTakeLimit); len(requests.ContractIds) != 1 {
			t.Fatalf("the receiver of a used contract was asked to close %d contracts, want 1", len(requests.ContractIds))
		}

		// a sender checkpoint of zero is not a final
		checkpointed := newContract()
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: checkpointed, ClientId: f.sourceId, Checkpoint: true}); err != nil {
			t.Fatal(err)
		}
		if settled, _, _ := state(checkpointed); settled {
			t.Fatal("a sender checkpoint of zero settled the contract")
		}

		// a contract whose expiry has started is left to the expiry
		expiring := newContract()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true WHERE contract_id=$1`, expiring))
		})
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: expiring, ClientId: f.sourceId}); err != nil {
			t.Fatal(err)
		}
		if settled, final, _ := state(expiring); settled || final {
			t.Fatal("a zero final settled a contract whose expiry had started")
		}
	})
}
