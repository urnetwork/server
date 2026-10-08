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

// Account credits and their durable applied markers share one complete owner.
// Preserve null versus stored run-once identities through the locked re-read.
type legacyProviderOwnership struct {
	networkIds        []server.Id
	taskIdRunOnceKeys map[server.Id]*string
}

func (self *legacyProviderOwnership) keys() []server.PgOwnershipKey {
	keys := accountBalanceOwnershipKeys(self.networkIds)
	for taskId, runOnceKey := range self.taskIdRunOnceKeys {
		keys = append(keys, task.PendingTaskOwnershipKey(taskId, runOnceKey))
	}
	return keys
}

// Only production admission supplies a queue map. Low-level rollback controls
// can still exercise the SQL body directly, without claiming entry ownership.
func (self *legacyProviderOwnership) validateQueue(tx server.PgTx, taskId server.Id, runOnceKey *string) error {
	if self == nil || self.taskIdRunOnceKeys == nil {
		return nil
	}
	expected, exists := self.taskIdRunOnceKeys[taskId]
	if !exists || (expected == nil) != (runOnceKey == nil) ||
		(expected != nil && *expected != *runOnceKey) ||
		!server.TxOwnsKeys(tx, []server.PgOwnershipKey{task.PendingTaskOwnershipKey(taskId, runOnceKey)}) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, errors.New("legacy provider total queue identity changed before ownership"))
	}
	return nil
}

// This read takes no row locks. The transaction validates both allocation and
// exact stored queue identities after acquiring every account and queue owner.
func readLegacyProviderOwnership(ctx context.Context, taskId server.Id) (ownership *legacyProviderOwnership, returnErr error) {
	ownership = &legacyProviderOwnership{taskIdRunOnceKeys: map[server.Id]*string{}}
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
			var runOnceKey *string
			err := conn.QueryRow(ctx, `SELECT args_json,run_once_key FROM pending_task WHERE task_id=$1 AND function_name=$2`,
				taskId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&data, &runOnceKey)
			if errors.Is(err, pgx.ErrNoRows) {
				err = errors.New("legacy provider total task ownership missing")
			}
			server.Raise(withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err))
			payload, err := decodeLegacyProviderTotals(data)
			server.Raise(withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, err))
			for _, total := range payload.Totals {
				ownership.networkIds = append(ownership.networkIds, total.NetworkId)
			}
			ownership.taskIdRunOnceKeys[taskId] = runOnceKey
		}, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	return
}

// Preparation supplies one prospective provider identity; the locked body still
// validates every durable allocation. This bounded read only discovers queue keys.
func readLegacyProviderBatchOwnership(ctx context.Context, taskIds []server.Id, networkId server.Id) (ownership *legacyProviderOwnership, returnErr error) {
	if len(taskIds) < 2 || len(taskIds) > legacyProviderTotalsBatchLimit || networkId == (server.Id{}) {
		return nil, withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("invalid legacy provider total batch"))
	}
	ownership = &legacyProviderOwnership{
		networkIds:        []server.Id{networkId},
		taskIdRunOnceKeys: map[server.Id]*string{},
	}
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
			rows, err := conn.Query(ctx, `SELECT task_id,run_once_key FROM pending_task
                WHERE task_id=ANY($1) AND function_name=$2 ORDER BY task_id`,
				taskIds, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName())
			server.Raise(err)
			defer rows.Close()
			for rows.Next() {
				var taskId server.Id
				var runOnceKey *string
				server.Raise(rows.Scan(&taskId, &runOnceKey))
				ownership.taskIdRunOnceKeys[taskId] = runOnceKey
			}
			server.Raise(rows.Err())
			if len(ownership.taskIdRunOnceKeys) != len(taskIds) {
				panic(errors.New("legacy provider total batch ownership missing"))
			}
		}, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	return
}

// Keep the fixed diagnostic phases and acknowledged-commit behavior of the
// existing projection wrapper. Its body and commit never automatically replay.
func runLegacyProviderTotalsOwnedTx(ctx context.Context, ownership *legacyProviderOwnership, apply func(server.PgTx) error) error {
	return runLegacyProviderTotalsTxWithOwner(ctx, apply, func(ctx context.Context, callback func(server.PgTx), options ...any) {
		server.OwnedTx(ctx, ownership.keys(), callback, options...)
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
