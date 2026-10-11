package model

// A return companion whose forward contract passed its deadline has no origin,
// and old clients keep asking for it. Its absence used to be decided inside a
// read committed write transaction: BEGIN, the plain and the chained origin
// reads, COMMIT. These tests drive the real companion creation against the
// test database and record the statements it issues.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func isTransactionStatement(statement string) bool {
	statement = strings.ToLower(statement)
	return strings.HasPrefix(statement, "begin") || statement == "commit" || statement == "rollback"
}

// Without an origin the companion is refused after one read, and no
// transaction is opened for it.
func TestCompanionWithoutOriginReadsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		// the provider's return companion; its origin, payer to provider, is gone
		escrow, err := clients.create(observed, 1024, true)
		if escrow != nil || !errors.Is(err, ErrMissingCompanionOrigin) {
			t.Fatalf("a companion without an origin = %+v, %v", escrow, err)
		}
		reads := statements.take()
		if len(reads) != 1 {
			t.Fatalf("a companion without an origin used %d statements, want 1: %q", len(reads), reads)
		}
		if isTransactionStatement(reads[0]) {
			t.Fatalf("a companion without an origin opened a transaction: %q", reads)
		}
	})
}

// With an origin the existence read comes first, and the write transaction
// that follows selects the origin again before creating the companion on it.
func TestCompanionWithOriginSelectsItAgainInTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
		addContractPayoutTestBalance(ctx, clients.providerNetworkId, 8192)
		origin, err := clients.create(ctx, 1024, false)
		if err != nil || origin == nil {
			t.Fatalf("origin creation failed: %v", err)
		}

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		companion, err := clients.create(observed, 1024, true)
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatalf("a companion with an origin = %+v, %v", companion, err)
		}
		reads := statements.take()
		begin, reselect := -1, -1
		for i, statement := range reads {
			if begin < 0 && strings.HasPrefix(strings.ToLower(statement), "begin") {
				begin = i
			}
			if reselect < 0 && strings.Contains(statement, "AS earliest_origin") {
				reselect = i
			}
		}
		if len(reads) == 0 || !strings.HasPrefix(reads[0], "SELECT EXISTS") || begin < 1 || reselect < begin {
			t.Fatalf("want the existence read, then BEGIN, then the origin selection inside the transaction: %q", reads)
		}
	})
}

// A closed forward contract stays an origin for the linger timeout after its
// close. The existence read must find it through its closed branch, and must
// not find it once the close is older than the timeout. The origin settled
// 20 minutes ago within its 60 minute deadline; the margins on both sides
// are minutes, so the database and process clocks need not agree closely.
func TestCompanionLingeringOriginPassesExistenceRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
		originId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id,
					destination_network_id, destination_id,
					transfer_byte_count, payer_network_id,
					create_time, close_time, outcome, provider_usage
				) VALUES (
					$1, $2, $3, $4, $5, 1024, $2,
					clock_timestamp() AT TIME ZONE 'UTC' - interval '30 minutes',
					clock_timestamp() AT TIME ZONE 'UTC' - interval '20 minutes',
					$6, '{"version":1,"byte_count":0,"providers":[]}'::jsonb
				)
			`, originId, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, ContractOutcomeSettled))
		})

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		// a 10 minute linger has passed since the close
		escrow, err := CreateCompanionTransferEscrow(observed, clients.providerNetworkId, clients.providerId,
			clients.payerNetworkId, clients.payerId, 1024, 10*time.Minute)
		if escrow != nil || !errors.Is(err, ErrMissingCompanionOrigin) {
			t.Fatalf("a companion on an origin closed past its linger = %+v, %v", escrow, err)
		}
		if reads := statements.take(); len(reads) != 1 {
			t.Fatalf("a companion on an origin closed past its linger used %d statements, want 1: %q", len(reads), reads)
		}

		// a one hour linger still holds the origin
		companion, err := CreateCompanionTransferEscrow(observed, clients.providerNetworkId, clients.providerId,
			clients.payerNetworkId, clients.payerId, 1024, time.Hour)
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != originId {
			t.Fatalf("a companion on a lingering origin = %+v, %v", companion, err)
		}
		if reads := statements.take(); len(reads) == 0 || !strings.HasPrefix(reads[0], "SELECT EXISTS") {
			t.Fatalf("a companion on a lingering origin did not start with the existence read: %q", reads)
		}
	})
}
