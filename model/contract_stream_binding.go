// A streamed contract's stream id is written by its insert.
//
// Contract creation used to insert the row without its stream and join the
// stream after commit with a second UPDATE of the same row. stream_id is
// indexed (the contract orphan sweep's contract_participant parent probe reads
// transfer_contract_stream_id), so that UPDATE was never HOT: it wrote a full
// row version and re-inserted the fresh contract into every transfer_contract
// index, at the right-edge pages that every concurrent creation also writes.
//
// The creation request now carries a binding in its context. Immediately
// before the creation transaction begins, its owner reads the Redis stream
// state the binding needs, a read that changes no membership
// (resolveContractStreamBinding). The insert inside the transaction writes the
// stream id read there and does no Redis I/O for it, so a slow or failing
// Redis cannot hold the transaction open. The Redis membership is still joined
// after commit, exactly as before, and stays the authority. When the joined
// stream is the bound one, the common case, the contract row is not written
// again. The stream id genuinely becomes known later only when the stream
// state changed between the read and the join (the stream ended, or another
// request started it): SetContractStream then replaces the bound id once, and
// ClearContractStreamBinding clears a bound id whose stream the contract could
// not join. Contracts created without a binding, or by an owner that does not
// resolve it, keep the late write.
package model

import (
	"context"
	"sync"

	"github.com/urnetwork/server"
)

type contractStreamBindingContextKey struct{}

// One creation request. Each transaction owner resolves the binding again
// before it begins, and an insert binds the latest resolution; each contract
// id keeps the stream id of its latest insert. Safe for concurrent use.
type contractStreamBinding struct {
	// reads the stream id an insert writes, or nil for no stream. It reads
	// Redis, so it runs only before a transaction begins.
	read func(ctx context.Context) *server.Id
	// the id AddToStream gives a stream it starts for this request, so a
	// stream that did not exist at the read starts with the bound id
	proposedStreamId *server.Id

	stateLock sync.Mutex
	// the latest read's stream id; nil before the first read, and when the
	// contract will join no stream
	streamId          *server.Id
	contractStreamIds map[server.Id]server.Id
}

func contractStreamBindingFromContext(ctx context.Context) *contractStreamBinding {
	binding, _ := ctx.Value(contractStreamBindingContextKey{}).(*contractStreamBinding)
	return binding
}

// The contract will be added to its stream with AddToStream after commit. The
// insert writes the id of the stream while it has members, else the id
// AddToStream gives the stream it starts.
func WithAddStreamBinding(ctx context.Context, sourceId server.Id, destinationId server.Id, intermediaryIds []server.Id) context.Context {
	proposedStreamId := server.NewId()
	key := newStreamKey(sourceId, destinationId, intermediaryIds)
	return context.WithValue(ctx, contractStreamBindingContextKey{}, &contractStreamBinding{
		read: func(ctx context.Context) *server.Id {
			if streamId, ok := memberStreamId(ctx, key); ok {
				return &streamId
			}
			return &proposedStreamId
		},
		proposedStreamId: &proposedStreamId,
	})
}

// A contract without intermediaries joins the pair's active stream after
// commit (AddContractToPairStream). Forced and without one, it is added to
// the direct stream with AddToStream; otherwise it carries no stream.
func WithPairStreamBinding(ctx context.Context, sourceId server.Id, destinationId server.Id, forceStream bool) context.Context {
	var proposedStreamId *server.Id
	if forceStream {
		streamId := server.NewId()
		proposedStreamId = &streamId
	}
	return context.WithValue(ctx, contractStreamBindingContextKey{}, &contractStreamBinding{
		read: func(ctx context.Context) *server.Id {
			if streamId, ok := joinablePairStreamId(ctx, sourceId, destinationId); ok {
				return &streamId
			}
			if !forceStream {
				return nil
			}
			if streamId, ok := memberStreamId(ctx, newStreamKey(sourceId, destinationId, nil)); ok {
				return &streamId
			}
			return proposedStreamId
		},
		proposedStreamId: proposedStreamId,
	})
}

// A companion joins the stream its origin contract belongs to after commit,
// or the pair's active stream when the origin's is gone
// (AddCompanionContractToStream). The creation transaction selects the origin
// only after the read, so the insert writes the pair's joinable stream. Every
// stream of the origin's flow is a stream of the pair, so this is the
// origin's stream whenever the pair carries one stream. When the companion
// joins another of several pair streams, the association replaces the bound
// id once.
func WithCompanionStreamBinding(ctx context.Context, sourceId server.Id, destinationId server.Id) context.Context {
	return context.WithValue(ctx, contractStreamBindingContextKey{}, &contractStreamBinding{
		read: func(ctx context.Context) *server.Id {
			if streamId, ok := joinablePairStreamId(ctx, sourceId, destinationId); ok {
				return &streamId
			}
			return nil
		},
	})
}

// Reads the stream id the request's next insert writes. Each creation
// transaction owner calls this immediately before its transaction begins,
// never inside one. A no-op without a binding.
func resolveContractStreamBinding(ctx context.Context) {
	binding := contractStreamBindingFromContext(ctx)
	if binding == nil {
		return
	}
	defer server.EnterContractCreationStage(ctx, server.ContractStageStream)()
	streamId := binding.read(ctx)
	binding.stateLock.Lock()
	defer binding.stateLock.Unlock()
	binding.streamId = streamId
}

// Records and returns the stream id a new contract's insert writes: the
// latest resolution. Nil without a binding, before a resolution, and when the
// contract will join no stream. It runs inside the creation transaction and
// does no I/O.
func bindContractStreamAtInsert(ctx context.Context, contractId server.Id) *server.Id {
	binding := contractStreamBindingFromContext(ctx)
	if binding == nil {
		return nil
	}
	binding.stateLock.Lock()
	defer binding.stateLock.Unlock()
	if binding.streamId == nil {
		delete(binding.contractStreamIds, contractId)
		return nil
	}
	if binding.contractStreamIds == nil {
		binding.contractStreamIds = map[server.Id]server.Id{}
	}
	binding.contractStreamIds[contractId] = *binding.streamId
	streamId := *binding.streamId
	return &streamId
}

// The stream id that the contract's insert wrote under this request's
// binding, if any.
func boundContractStreamId(ctx context.Context, contractId server.Id) *server.Id {
	binding := contractStreamBindingFromContext(ctx)
	if binding == nil {
		return nil
	}
	binding.stateLock.Lock()
	defer binding.stateLock.Unlock()
	if streamId, ok := binding.contractStreamIds[contractId]; ok {
		return &streamId
	}
	return nil
}

// The id AddToStream gives a stream it starts: the request's proposal when
// its binding has one, else a new id. A proposal is never an ended stream's
// id, whose retirement marker would refuse the new stream.
func proposedStreamIdForAdd(ctx context.Context) server.Id {
	if binding := contractStreamBindingFromContext(ctx); binding != nil && binding.proposedStreamId != nil {
		return *binding.proposedStreamId
	}
	return server.NewId()
}

// The contract joined no stream after commit, so a stream id bound at its
// insert must not stay on the row. This writes only when the stream read
// before the creation transaction ended before the join.
func ClearContractStreamBinding(ctx context.Context, contractId server.Id) {
	boundStreamId := boundContractStreamId(ctx, contractId)
	if boundStreamId == nil {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE transfer_contract SET stream_id = NULL WHERE contract_id = $1 AND stream_id = $2`,
			contractId,
			*boundStreamId,
		))
	})
	binding := contractStreamBindingFromContext(ctx)
	binding.stateLock.Lock()
	defer binding.stateLock.Unlock()
	delete(binding.contractStreamIds, contractId)
}

// The id of the stream at `key` while it has members: the stream AddToStream
// joins rather than starts.
func memberStreamId(ctx context.Context, key streamKey) (streamId server.Id, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		// the two keys share one hash slot
		result, err := r.Eval(
			ctx,
			`
			if redis.call('SCARD', KEYS[2]) == 0 then
				return false
			end
			return redis.call('GET', KEYS[1])
			`,
			[]string{
				streamIdKey(key),
				streamContractsKey(key),
			},
		).Text()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId = server.Id([]byte(result))
		ok = true
	})
	return
}

// The id of the stream at `key` that joinStream would join.
func joinableStreamId(ctx context.Context, key streamKey) (streamId server.Id, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		result, err := r.Get(ctx, streamIdKey(key)).Bytes()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId = server.Id(result)
		ok = true
	})
	return
}

// The first joinable stream of the pair marker, in the order
// AddContractToPairStream tries to join them. Stale members are left for the
// join to prune.
func joinablePairStreamId(ctx context.Context, sourceId server.Id, destinationId server.Id) (streamId server.Id, ok bool) {
	var candidateKeys []streamKey
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.SMembers(ctx, pairStreamsKey(sourceId, destinationId)).Result()
		if err != nil {
			panic(err)
		}
		for _, member := range members {
			candidateKeys = append(candidateKeys, streamKey([]byte(member)))
		}
	})
	for _, key := range candidateKeys {
		if streamId, ok = joinableStreamId(ctx, key); ok {
			return
		}
	}
	return
}
