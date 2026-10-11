package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A policy-table barrier catches any reintroduced admission query before the
// contract transaction. The held table is independent of per-contract writes.
func TestRedisAdmissionDoesNotWaitForRetiredPolicyTable(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE redis_contract_admission_policy IN ACCESS EXCLUSIVE MODE`))
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		escrow, err := CreateTransferEscrow(bounded, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 17)
		if err != nil || escrow == nil {
			t.Fatal("contract creation waited for retired policy table", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("default admission did not reserve in Redis", got)
		}
	})
}

// The bridge must work before and after the separately deployed column drop.
// Normal contracts and companion replies keep the same reservation authority.
func TestRedisAdmissionCreatesAfterEnabledColumnRemoval(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE redis_contract_admission_policy DROP COLUMN enabled`))
		})
		origin := createRedisAdmissionTest(ctx, f, 100)
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 100, time.Hour)
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatal("companion depended on retired policy column", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 200 {
			t.Fatal("post-drop reservations differ", got)
		}
		server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, companion.ContractId)...)
		server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, origin.ContractId)...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatal("post-drop settlement retained debt", got)
		}
	})
}
