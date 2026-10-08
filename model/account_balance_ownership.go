// Paid and provided account projections share the actual primary-key owner.
// Admission is independent of task names and uses the same physical session as
// the complete monetary transaction. No allocation-size fallback bypasses it.
package model

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A private test context can hold the real write acknowledgement before its
// applied marker. Production has no hook; SQL limits and the financial body
// remain unchanged. The hook must honor the invocation's finite context.
type accountBalanceWriteTestKey struct{}

func observeAccountBalanceWriteForTest(ctx context.Context, tx server.PgTx, networkId server.Id) {
	if hook, ok := ctx.Value(accountBalanceWriteTestKey{}).(func(context.Context, server.PgTx, server.Id)); ok {
		hook(ctx, tx, networkId)
	}
}

func accountBalanceOwnershipKeys(networkIds []server.Id) []server.PgOwnershipKey {
	keys := make([]server.PgOwnershipKey, len(networkIds))
	for index, networkId := range networkIds {
		keys[index] = server.NewPgOwnershipKey("account_balance", networkId)
	}
	return keys
}

// This read takes no row locks. The claimed pending payload is immutable except
// for its monotonic applied marker; the transaction re-reads and validates all
// allocation identities after acquiring every account owner.
func readLegacyProviderOwnership(ctx context.Context, taskId server.Id) (networkIds []server.Id, returnErr error) {
	server.HandleError(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				cause, ok := recovered.(error)
				if !ok {
					cause = errors.New("non-error provider ownership read panic")
				}
				panic(withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, cause))
			}
		}()
		server.Db(ctx, func(conn server.PgConn) {
			var data string
			err := conn.QueryRow(ctx, `SELECT args_json FROM pending_task WHERE task_id=$1 AND function_name=$2`,
				taskId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&data)
			if errors.Is(err, pgx.ErrNoRows) {
				err = errors.New("legacy provider total task ownership missing")
			}
			server.Raise(withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err))
			payload, err := decodeLegacyProviderTotals(data)
			server.Raise(withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, err))
			for _, total := range payload.Totals {
				networkIds = append(networkIds, total.NetworkId)
			}
		}, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	return
}

// Keep the fixed diagnostic phases and acknowledged-commit behavior of the
// existing projection wrapper. Its body and commit never automatically replay.
func runLegacyProviderTotalsOwnedTx(ctx context.Context, networkIds []server.Id, apply func(server.PgTx) error) error {
	return runLegacyProviderTotalsTxWithOwner(ctx, apply, func(ctx context.Context, callback func(server.PgTx), options ...any) {
		server.OwnedTx(ctx, accountBalanceOwnershipKeys(networkIds), callback, options...)
	})
}

// All production singleton executions supply the admitted identities. Direct
// transaction helpers remain useful for rollback/error tests of the SQL itself.
func validateLegacyProviderOwnership(payload legacyProviderTotalsPayload, networkIds []server.Id) error {
	current := make([]server.Id, len(payload.Totals))
	for index, total := range payload.Totals {
		current[index] = total.NetworkId
	}
	if !slices.Equal(current, networkIds) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("legacy provider total allocation changed before ownership"))
	}
	return nil
}

// Payment network identity is immutable after insertion. The completion UPDATE
// predicates on it again before mutation. Historical review-only rows may have
// no network; they complete under their payment owner without an account credit.
func readPaymentAccountNetwork(ctx context.Context, paymentId server.Id) (networkId *server.Id, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(ctx, `SELECT network_id FROM account_payment WHERE payment_id=$1`, paymentId).Scan(&networkId)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		exists = true
	}, server.OptNoRetry())
	return
}
