package model

import (
	"context"
	"sync"

	"github.com/urnetwork/server/v2026"
)

type payerAdmissionTurn struct {
	available chan struct{}
	refs      int
}

// Positive reservations can converge on the same payer's locked grants. Keep
// redundant waiters in this process outside the connection pool. This is
// only a scheduling optimization: all durable locks, fresh snapshots, revision
// fences and financial checks remain authoritative, including with old writers.
// Entries exist only while an operation holds or waits for that payer's turn.
type payerAdmissionQueue struct {
	mu    sync.Mutex
	turns map[server.Id]*payerAdmissionTurn
}

var transferEscrowAdmissionQueue payerAdmissionQueue

func (q *payerAdmissionQueue) acquire(ctx context.Context, payer server.Id) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	if q.turns == nil {
		q.turns = map[server.Id]*payerAdmissionTurn{}
	}
	turn := q.turns[payer]
	if turn == nil {
		turn = &payerAdmissionTurn{available: make(chan struct{}, 1)}
		turn.available <- struct{}{}
		q.turns[payer] = turn
	}
	turn.refs++
	q.mu.Unlock()

	dropRef := func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		turn.refs--
		if turn.refs == 0 {
			delete(q.turns, payer)
		}
	}
	select {
	case <-ctx.Done():
		dropRef()
		return nil, ctx.Err()
	case <-turn.available:
		var once sync.Once
		release := func() {
			once.Do(func() {
				turn.available <- struct{}{}
				dropRef()
			})
		}
		// A canceled caller may win the ready channel and cancellation race.
		// Give the turn back before admitting any database work.
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}

// Takes the payer's turn for a positive request on the PostgreSQL ledger path.
// Acquire holding no connection, retain the turn through transaction
// completion, and release on success, error or panic before post-commit work.
// Reads that decide what the transaction funds follow the acquisition, so a
// wait for the turn never separates them from the funding. Zero-byte anchors
// need no financial serialization, and Redis admission locks no grant, so
// neither takes a turn and its release does nothing. A positive companion that
// is subsequently clamped to zero may harmlessly take a turn before that read.
func acquireTransferEscrowTurn(ctx context.Context, payer server.Id, requested ByteCount) (func(), error) {
	if requested <= 0 || redisAdmissionFromContext(ctx) != nil {
		return func() {}, nil
	}
	defer server.EnterContractCreationStage(ctx, server.ContractStagePayerGate)()
	return transferEscrowAdmissionQueue.acquire(ctx, payer)
}

// The creation transaction of an owner that holds its payer's turn, or needs
// none. It never reruns: a transient failure reaches the owner.
func transferEscrowTxInTurn(ctx context.Context, callback func(server.PgTx)) {
	defer server.EnterContractCreationStage(ctx, server.ContractStageTransaction)()
	server.Tx(ctx, callback, server.TxReadCommitted, server.OptNoRetry())
}

// The stream state the insert binds is read first, holding no turn or
// connection (contract_stream_binding.go). Then the transaction runs on the
// payer's turn.
func transferEscrowTx(ctx context.Context, payer server.Id, requested ByteCount, callback func(server.PgTx)) error {
	resolveContractStreamBinding(ctx)
	release, err := acquireTransferEscrowTurn(ctx, payer, requested)
	if err != nil {
		return err
	}
	defer release()
	transferEscrowTxInTurn(ctx, callback)
	return nil
}
