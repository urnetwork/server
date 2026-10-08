package model

import (
	"context"
	"sync"

	"github.com/urnetwork/server"
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

// Acquire before opening any connection, retain the turn across transaction
// retries, and release on success, error or panic before any post-commit work.
// Zero-byte anchors need no financial serialization. A positive companion that
// is subsequently clamped to zero may harmlessly take a turn before that read.
func transferEscrowTx(ctx context.Context, payer server.Id, requested ByteCount, callback func(server.PgTx)) error {
	if requested > 0 && redisAdmissionFromContext(ctx) == nil {
		leaveGate := server.EnterContractCreationStage(ctx, server.ContractStagePayerGate)
		release, err := transferEscrowAdmissionQueue.acquire(ctx, payer)
		leaveGate()
		if err != nil {
			return err
		}
		defer release()
	}
	defer server.EnterContractCreationStage(ctx, server.ContractStageTransaction)()
	server.Tx(ctx, callback, server.TxReadCommitted, server.OptNoRetry())
	return nil
}
