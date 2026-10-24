package model

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A contract creation transaction performs only SQL. Its Redis reservations
// precede BEGIN and its acknowledgements follow commit, so a slow Redis cannot
// hold a pooler server idle in transaction, nor can a second pooled connection.
// The observer attributes I/O to the goroutine that owns the open transaction.
func TestRedisAdmissionCreationTransactionPerformsNoRedisIo(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		prober := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, prober)
		redisSelectionTestGrants(t, ctx, prober, proberGrantFirstCount, 4096, true)

		observation, uninstall := server.Testing_ObserveTxIo()
		defer uninstall()
		origin := createRedisAdmissionTest(ctx, f, 100)
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 100, time.Hour)
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatal("companion creation failed", err)
		}
		// 800 bytes remain, below the 900 byte request and its shrink floor
		if refused, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 900); err == nil || refused != nil || !strings.Contains(err.Error(), "Insufficient balance") {
			t.Fatal("over-balance request was not refused", refused, err)
		}
		whole, err := prober.create(ctx, 1024, false)
		if err != nil || whole == nil || len(whole.Balances) != 1 {
			t.Fatal("prober whole-grant creation failed", whole, err)
		}
		uninstall()

		creations := 0
		for _, statement := range observation.FirstStatements() {
			if strings.Contains(statement.TxSite, "transferEscrowTx") {
				creations++
			}
		}
		// The over-balance request is refused by its reservation, before any
		// transaction begins.
		if creations != 3 {
			t.Fatalf("observed %d creation transactions; want the plain, companion and prober creations", creations)
		}
		var inTransaction []error
		for _, event := range observation.Events() {
			if strings.Contains(event.TxSite, "transferEscrowTx") {
				inTransaction = append(inTransaction, errors.New(string(event.Kind)+" in "+event.TxSite+" from "+event.Site))
			} else {
				t.Logf("non-creation transaction %s: %s from %s", event.Kind, event.TxSite, event.Site)
			}
		}
		if len(inTransaction) != 0 {
			t.Fatalf("creation transactions held open across I/O:\n%v", errors.Join(inTransaction...))
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 200 {
			t.Fatal("reservations differ from the two created contracts", got)
		}
	})
}
