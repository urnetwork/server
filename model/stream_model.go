package model

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"slices"
	"strings"
	"sync/atomic"
	// "encoding/json"
	"fmt"
	"iter"
	mathrand "math/rand"
	"sync"
	"time"

	"maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// note `streamKey` and `streamHop` are []byte so they can be directly stored into redis

type streamKey []byte

// stream keys are stored so that source id < destination id
// in this way the sender and receiver paths use the same stream
func newStreamKey(sourceId server.Id, destinationId server.Id, intermediaryIds []server.Id) streamKey {
	c := sourceId.Cmp(destinationId)
	for i := 0; c == 0 && i < len(intermediaryIds)-1-i; i += 1 {
		c = intermediaryIds[i].Cmp(intermediaryIds[len(intermediaryIds)-1-i])
	}

	sk := make([]byte, 16*(2+len(intermediaryIds)))
	if c <= 0 {
		// order source, intermed..., dest
		copy(sk, sourceId[:])
		for i := 0; i < len(intermediaryIds); i += 1 {
			intermediaryId := intermediaryIds[i]
			copy(sk[(i+1)*16:], intermediaryId[:])
		}
		copy(sk[(len(intermediaryIds)+1)*16:], destinationId[:])
	} else {
		// order dest, reverse(intermed...), source
		copy(sk, destinationId[:])
		for i := 0; i < len(intermediaryIds); i += 1 {
			intermediaryId := intermediaryIds[len(intermediaryIds)-1-i]
			copy(sk[(i+1)*16:], intermediaryId[:])
		}
		copy(sk[(len(intermediaryIds)+1)*16:], sourceId[:])
	}
	return sk
}

func (self streamKey) Bytes() []byte {
	return []byte(self)
}

func (self streamKey) SourceId() server.Id {
	return server.Id(self[:16])
}

func (self streamKey) DestinationId() server.Id {
	return server.Id(self[len(self)-16 : len(self)])
}

func (self streamKey) IntermediaryIds() (intermediaryIds []server.Id) {
	n := (len(self) / 16) - 2
	for i := range n {
		intermediaryId := server.Id(self[(i+1)*16 : (i+2)*16])
		intermediaryIds = append(intermediaryIds, intermediaryId)
	}
	return
}

// a fixed length string representation of the stream key
func (self streamKey) String() string {
	h := sha256.New()
	d := h.Sum(self)
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	return encoder.EncodeToString(d)
}

// enumerate ids and optional source and destination for each id
func (self streamKey) Edges() iter.Seq2[server.Id, [2]*server.Id] {
	return func(yield func(server.Id, [2]*server.Id) bool) {
		n := len(self) / 16
		for i := range n {
			clientId := server.Id(self[i*16 : (i+1)*16])
			var edges [2]*server.Id
			if 0 <= i-1 {
				sourceId := server.Id(self[(i-1)*16 : i*16])
				edges[0] = &sourceId
			}
			if i+1 < n {
				destinationId := server.Id(self[(i+1)*16 : (i+2)*16])
				edges[1] = &destinationId
			}
			if !yield(clientId, edges) {
				return
			}
		}
	}
}

// represents a hop through a client from source to destination
type StreamHop [48]byte

func NewStreamHop(sourceId *server.Id, destinationId *server.Id, streamId server.Id) StreamHop {
	var sh [48]byte
	if sourceId != nil {
		copy(sh[:], sourceId[:])
	}
	if destinationId != nil {
		copy(sh[16:], destinationId[:])
	}
	copy(sh[32:], streamId[:])
	return sh
}

func (self StreamHop) Bytes() []byte {
	return []byte(self[:])
}

func (self StreamHop) SourceId() *server.Id {
	sourceId := server.Id(self[0:16])
	if (sourceId == server.Id{}) {
		return nil
	}
	return &sourceId
}

func (self StreamHop) DestinationId() *server.Id {
	destinationId := server.Id(self[16:32])
	if (destinationId == server.Id{}) {
		return nil
	}
	return &destinationId
}

func (self StreamHop) StreamId() server.Id {
	return server.Id(self[32:48])
}

func (self StreamHop) Path() connect.TransferPath {
	sourceId := connect.Id(self[0:16])
	destinationId := connect.Id(self[16:32])
	streamId := connect.Id(self[32:48])
	return connect.TransferPath{
		SourceId:      sourceId,
		DestinationId: destinationId,
		StreamId:      streamId,
	}
}

// all keys have ttl of 1 day that is reset for each new contract
// meaning if there is no new activity, they will be cleared
// stream key -> stream id
// stream key -> set of contract id
// client id -> set of active stream hops
// client id -> event id counter
// event(client id) active streams changed event
// contract id -> stream key
// contract id -> stream id

func streamIdKey(streamKey []byte) string {
	return fmt.Sprintf("{%s}s2_sk_sid", streamKey)
}

func streamContractsKey(streamKey []byte) string {
	return fmt.Sprintf("{%s}s2_sk_cs", streamKey)
}

func clientStreamHopsKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}s2_c_hops", clientId)
}

// StreamHopsKeyEventPattern is the psubscribe pattern for the keyspace
// channels of all client hop-set keys on a db (PEERSSTREAMS2.md). Hop sets
// are tiny, so key events act as a dirty signal and the read is the existing
// full read — no per-member restructure.
func StreamHopsKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{*}s2_c_hops", db)
}

// ParseStreamHopsKeyEvent extracts the client id from a keyspace channel
// name matching `StreamHopsKeyEventPattern`.
func ParseStreamHopsKeyEvent(channel string) (clientId server.Id, ok bool) {
	i := strings.Index(channel, ":{")
	if i < 0 {
		return
	}
	rest := channel[i+len(":{"):]
	j := strings.Index(rest, "}s2_c_hops")
	if j < 0 || j != len(rest)-len("}s2_c_hops") {
		return
	}
	clientId, err := server.ParseId(rest[:j])
	if err != nil {
		return
	}
	ok = true
	return
}

// the event id counter must outlive any live listener's memory of the last
// event id (hops carry an 8h ttl; a counter reset while listeners hold a
// higher id would make new events look stale). Refreshed on every touch, it
// only expires after 24h of total silence for the client — at which point the
// hops key is long gone and listeners resync from an empty snapshot.
// Without a ttl these counters accumulate forever (one per client that ever
// appeared on a stream hop) and volatile-ttl eviction cannot touch them.
const clientEventIdTtl = 24 * time.Hour

func clientEventIdKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}s2_c_eid", clientId)
}

func contractStreamKey(contractId server.Id) string {
	return fmt.Sprintf("{%s}s2_ct_sk", contractId)
}

func contractStreamId(contractId server.Id) string {
	return fmt.Sprintf("{%s}s2_ct_sid", contractId)
}

// the pair marker is the set of a pair's active stream keys, maintained
// best-effort by add/join/remove. It lets the companion path resolve the
// origin flow's streams with one redis read instead of scanning the pair's
// open contracts — pairs that never stream (the common case) stop at an
// empty read. Members can be stale (redis loss, expiry races, streams
// created before this marker deployed converge as their contracts renew);
// readers must re-check stream liveness and prune
func pairStreamsKey(clientIdA server.Id, clientIdB server.Id) string {
	// normalize like `newStreamKey` so both directions map to one key
	if 0 < clientIdA.Cmp(clientIdB) {
		clientIdA, clientIdB = clientIdB, clientIdA
	}
	return fmt.Sprintf("{%s.%s}s2_pk", clientIdA, clientIdB)
}

// returns a stream id to use for the contract
// if stream count changes from 0, associate the stream id with each hop on the stream
func AddToStream(
	ctx context.Context,
	contractId server.Id,
	sourceId server.Id,
	destinationId server.Id,
	intermediaryIds []server.Id,
) (
	streamId server.Id,
) {
	ttl := 8 * time.Hour
	ttlSeconds := int64(ttl / time.Second)
	streamKey := newStreamKey(sourceId, destinationId, intermediaryIds)

	// will be overwritten if already set
	server.Redis(ctx, func(r server.RedisClient) {
		// note the keys in eval have to be in the same hash slot
		result := r.Eval(
			ctx,
			`
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local stream_id = ARGV[1]
			local contract_id = ARGV[2]
			local ttl = ARGV[3]

			local initial_size = redis.call('SCARD', stream_contracts_key)
			local changed = redis.call('SADD', stream_contracts_key, contract_id)

			if initial_size == 0 then
				if 0 < changed then
			    	redis.call('SET', stream_id_key, stream_id)
			    end
			else
				stream_id = redis.call('GET', stream_id_key)
			end

			if 0 < changed then
				redis.call('EXPIRE', stream_id_key, ttl)
				redis.call('EXPIRE', stream_contracts_key, ttl)
			end

			return {stream_id, initial_size, changed}
			`,
			[]string{
				streamIdKey(streamKey),
				streamContractsKey(streamKey),
			},
			server.NewId().Bytes(),
			contractId.Bytes(),
			ttlSeconds,
		)
		values, err := result.Slice()
		if err != nil {
			panic(err)
		}

		streamId = server.Id([]byte(values[0].(string)))
		initialSize := values[1].(int64)
		changed := 0 < values[2].(int64)

		if changed {
			pipe := r.TxPipeline()
			pipe.Set(ctx, contractStreamKey(contractId), streamKey.Bytes(), ttl)
			pipe.Set(ctx, contractStreamId(contractId), streamId.Bytes(), ttl)
			_, err := pipe.Exec(ctx)
			if err != nil {
				panic(err)
			}

			// the pair marker key is in its own slot: plain pipeline
			r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				pairKey := pairStreamsKey(streamKey.SourceId(), streamKey.DestinationId())
				pipe.SAdd(ctx, pairKey, streamKey.Bytes())
				pipe.Expire(ctx, pairKey, ttl)
				return nil
			})

			if initialSize == 0 {
				for clientId, edges := range streamKey.Edges() {
					// bump the per-client hops version (PEERS2.md
					// dirty-counter + poll; no pubsub delivery)
					streamHopsKey := clientStreamHopsKey(clientId)
					streamHop := NewStreamHop(edges[0], edges[1], streamId)

					pipe := r.TxPipeline()
					pipe.Incr(ctx, clientEventIdKey(clientId))
					pipe.SAdd(ctx, streamHopsKey, streamHop.Bytes())
					pipe.Expire(ctx, streamHopsKey, ttl)
					pipe.Expire(ctx, clientEventIdKey(clientId), clientEventIdTtl)
					_, err := pipe.Exec(ctx)
					if err != nil {
						panic(err)
					}
				}
			} else {
				for clientId, _ := range streamKey.Edges() {
					streamHopsKey := clientStreamHopsKey(clientId)
					r.Expire(ctx, streamHopsKey, ttl)
					r.Expire(ctx, clientEventIdKey(clientId), clientEventIdTtl)
				}
			}
		}
	})
	return
}

// AddCompanionContractToStream marks a companion contract with the active
// stream of its origin flow and joins it to the stream's membership. Without
// the membership the receive sequence on the other side cannot see that the
// stream is active when it inspects the contract, and the stream would be
// torn down when the last origin contract closes even though the companion
// reply is still open (`RemoveFromStream` on close applies to the companion
// like any stream contract once joined).
// The escrow-linked origin alone cannot carry the stream marking: the
// earliest-origin linkage may predate the stream or may have already closed
// out of it while newer origin contracts keep the stream active. On a miss,
// resolve candidates through the pair marker and join the first active
// stream found.
func AddCompanionContractToStream(
	ctx context.Context,
	contractId server.Id,
	originContractId server.Id,
	sourceId server.Id,
	destinationId server.Id,
) (streamId server.Id, ok bool) {
	if _, key, found := GetStream(ctx, originContractId); found {
		streamId, ok = joinStream(ctx, contractId, key)
		if ok {
			return
		}
	}
	return AddContractToPairStream(ctx, contractId, sourceId, destinationId)
}

// AddContractToPairStream marks a contract with the pair's active stream and
// joins it to the stream's membership, resolving candidates through the pair
// marker. This is the resolution for reply contracts that carry no origin
// linkage — a same-network companion reply normalized to a network no-escrow
// contract still has to carry the origin flow's stream id, or the receive
// sequence on the other side cannot see that the stream is active when it
// inspects the contract.
func AddContractToPairStream(
	ctx context.Context,
	contractId server.Id,
	sourceId server.Id,
	destinationId server.Id,
) (streamId server.Id, ok bool) {
	pairKey := pairStreamsKey(sourceId, destinationId)
	var candidateKeys []streamKey
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.SMembers(ctx, pairKey).Result()
		if err != nil {
			panic(err)
		}
		for _, member := range members {
			candidateKeys = append(candidateKeys, streamKey([]byte(member)))
		}
	})
	for _, key := range candidateKeys {
		streamId, ok = joinStream(ctx, contractId, key)
		if ok {
			return
		}
		// the stream is gone; prune the stale marker member
		server.Redis(ctx, func(r server.RedisClient) {
			r.SRem(ctx, pairKey, key.Bytes())
		})
	}
	return
}

// ExpireLeakedStreamKeys is a one-shot cleanup for stream keys written with
// an effectively-infinite ttl: before 2026-07-20 the AddToStream/joinStream
// evals passed the 8h ttl as a raw `time.Duration` eval arg, which go-redis
// writes as its int64 NANOSECONDS — `EXPIRE <key> 28800000000000` (~913,000
// years). Orphaned streams therefore never aged out (~1.1M stream id keys
// plus their contract sets observed on main 2026-07-20). Any
// `s2_sk_sid` / `s2_sk_cs` key with a ttl beyond the intended 8h is clamped
// to 8h: an active stream re-refreshes on its next contract add, an orphan
// finally expires.
func ExpireLeakedStreamKeys(ctx context.Context) (scannedCount int64, fixedCount int64, returnErr error) {
	ttl := 8 * time.Hour

	server.Redis(ctx, func(r server.RedisClient) {
		fixNode := func(nodeCtx context.Context, node redis.UniversalClient) error {
			var cursor uint64
			for {
				keys, nextCursor, err := node.Scan(nodeCtx, cursor, "*s2_sk_*", 5000).Result()
				if err != nil {
					return err
				}
				if 0 < len(keys) {
					atomic.AddInt64(&scannedCount, int64(len(keys)))

					pttlCmds := make([]*redis.DurationCmd, len(keys))
					_, err := node.Pipelined(nodeCtx, func(pipe redis.Pipeliner) error {
						for i, key := range keys {
							pttlCmds[i] = pipe.PTTL(nodeCtx, key)
						}
						return nil
					})
					if err != nil {
						return err
					}
					leakedKeys := []string{}
					for i, pttlCmd := range pttlCmds {
						// negative ttls are missing (-2) or persistent (-1)
						// keys; the stream writers always set a ttl, so
						// leave those to inspection
						if ttl < pttlCmd.Val() {
							leakedKeys = append(leakedKeys, keys[i])
						}
					}
					if 0 < len(leakedKeys) {
						_, err := node.Pipelined(nodeCtx, func(pipe redis.Pipeliner) error {
							for _, key := range leakedKeys {
								pipe.Expire(nodeCtx, key, ttl)
							}
							return nil
						})
						if err != nil {
							return err
						}
						atomic.AddInt64(&fixedCount, int64(len(leakedKeys)))
					}
				}
				cursor = nextCursor
				if cursor == 0 {
					return nil
				}
			}
		}

		if clusterClient, ok := r.(*redis.ClusterClient); ok {
			// ForEachMaster runs its callback CONCURRENTLY, one goroutine
			// per master — the counts are atomics
			returnErr = clusterClient.ForEachMaster(ctx, func(nodeCtx context.Context, node *redis.Client) error {
				return fixNode(nodeCtx, node)
			})
		} else {
			returnErr = fixNode(ctx, r)
		}
	})
	return
}

// joinStream adds `contractId` to the stream at `key`, if that stream is
// still active
func joinStream(
	ctx context.Context,
	contractId server.Id,
	key streamKey,
) (streamId server.Id, ok bool) {

	ttl := 8 * time.Hour
	ttlSeconds := int64(ttl / time.Second)

	server.Redis(ctx, func(r server.RedisClient) {
		// note the keys in eval have to be in the same hash slot.
		// re-check liveness inside the eval: the stream can be removed
		// between the member lookup and the join, and a join must not
		// resurrect a dead stream (its hops are already torn down)
		result := r.Eval(
			ctx,
			`
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local contract_id = ARGV[1]
			local ttl = ARGV[2]

			local stream_id = redis.call('GET', stream_id_key)
			if stream_id == false then
				return false
			end

			redis.call('SADD', stream_contracts_key, contract_id)
			redis.call('EXPIRE', stream_id_key, ttl)
			redis.call('EXPIRE', stream_contracts_key, ttl)

			return stream_id
			`,
			[]string{
				streamIdKey(key),
				streamContractsKey(key),
			},
			contractId.Bytes(),
			ttlSeconds,
		)
		streamIdBytes, err := result.Text()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId = server.Id([]byte(streamIdBytes))

		pipe := r.TxPipeline()
		pipe.Set(ctx, contractStreamKey(contractId), key.Bytes(), ttl)
		pipe.Set(ctx, contractStreamId(contractId), streamId.Bytes(), ttl)
		_, err = pipe.Exec(ctx)
		if err != nil {
			panic(err)
		}

		// the pair marker key is in its own slot: plain pipeline
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			pairKey := pairStreamsKey(key.SourceId(), key.DestinationId())
			pipe.SAdd(ctx, pairKey, key.Bytes())
			pipe.Expire(ctx, pairKey, ttl)
			return nil
		})

		// joining an existing stream never changes hop membership;
		// refresh the hop key ttls like a same-stream add
		for clientId, _ := range key.Edges() {
			streamHopsKey := clientStreamHopsKey(clientId)
			r.Expire(ctx, streamHopsKey, ttl)
			r.Expire(ctx, clientEventIdKey(clientId), clientEventIdTtl)
		}

		ok = true
	})
	return
}

func RemoveFromStream(ctx context.Context, contractId server.Id) (streamId server.Id, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamKeyCmd := pipe.Get(ctx, contractStreamKey(contractId))
		streamIdCmd := pipe.Get(ctx, contractStreamId(contractId))
		_, err := pipe.Exec(ctx)
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}

		streamKeyBytes, err := streamKeyCmd.Bytes()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamKey := streamKey(streamKeyBytes)

		streamIdBytes, err := streamIdCmd.Bytes()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId = server.Id(streamIdBytes)

		// note the keys in eval have to be in the same hash slot
		result := r.Eval(
			ctx,
			`
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local contract_id = ARGV[1]

			local changed = redis.call('SREM', stream_contracts_key, contract_id)
			local final_size = redis.call('SCARD', stream_contracts_key)

			if final_size == 0 then
				if 0 < changed then
			    	redis.call('DEL', stream_id_key)
			    end
			end

			return {final_size, changed}
			`,
			[]string{
				streamIdKey(streamKey),
				streamContractsKey(streamKey),
			},
			contractId.Bytes(),
		)
		values, err := result.Slice()
		if err != nil {
			panic(err)
		}

		finalSize := values[0].(int64)
		changed := 0 < values[1].(int64)

		if changed {
			pipe := r.TxPipeline()
			pipe.Del(ctx, contractStreamKey(contractId))
			pipe.Del(ctx, contractStreamId(contractId))
			_, err := pipe.Exec(ctx)
			if err != nil {
				panic(err)
			}

			if finalSize == 0 {
				r.SRem(ctx, pairStreamsKey(streamKey.SourceId(), streamKey.DestinationId()), streamKey.Bytes())

				for clientId, edges := range streamKey.Edges() {
					// bump the per-client hops version (PEERS2.md
					// dirty-counter + poll; no pubsub delivery)
					streamHopsKey := clientStreamHopsKey(clientId)
					streamHop := NewStreamHop(edges[0], edges[1], streamId)

					pipe := r.TxPipeline()
					pipe.Incr(ctx, clientEventIdKey(clientId))
					pipe.SRem(ctx, streamHopsKey, streamHop.Bytes())
					pipe.Expire(ctx, clientEventIdKey(clientId), clientEventIdTtl)
					_, err := pipe.Exec(ctx)
					if err != nil {
						panic(err)
					}
				}
			}
		}

		ok = true
	})
	return
}

func GetStream(ctx context.Context, contractId server.Id) (streamId server.Id, key streamKey, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamIdCmd := pipe.Get(ctx, contractStreamId(contractId))
		streamKeyCmd := pipe.Get(ctx, contractStreamKey(contractId))
		_, err := pipe.Exec(ctx)
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}

		streamIdBytes, err := streamIdCmd.Bytes()

		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId = server.Id(streamIdBytes)

		streamKeyBytes, err := streamKeyCmd.Bytes()

		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		key = streamKey(streamKeyBytes)

		ok = true
	})
	return
}

func GetStreamEventId(ctx context.Context, clientId server.Id) (eventId int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		eventId_, err := r.Get(ctx, clientEventIdKey(clientId)).Int64()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId = eventId_
	})
	return
}

func GetStreamHops(ctx context.Context, clientId server.Id) (eventId int64, streamHops map[StreamHop]bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamHopsCmd := pipe.SMembers(ctx, clientStreamHopsKey(clientId))
		eventIdCmd := pipe.Get(ctx, clientEventIdKey(clientId))
		_, err := pipe.Exec(ctx)
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}

		streamHopStrs, err := streamHopsCmd.Result()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId_, err := eventIdCmd.Int64()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId = eventId_

		streamHops = map[StreamHop]bool{}
		for _, streamHopStr := range streamHopStrs {
			streamHop := StreamHop([]byte(streamHopStr))
			streamHops[streamHop] = true
		}
	})
	return
}

type StreamHopEventType int

const (
	StreamHopEventTypeAdded   StreamHopEventType = 1
	StreamHopEventTypeRemoved StreamHopEventType = 2
	StreamHopEventTypeReset   StreamHopEventType = 3
)

type StreamHopEvent struct {
	EventId int64
	// add, remove, reset
	StreamHopEventType StreamHopEventType
	StreamHops         []StreamHop
}

type StreamHopListener struct {
	ctx           context.Context
	cancel        context.CancelFunc
	clientId      server.Id
	callback      func(*StreamHopEvent)
	pollInterval  time.Duration
	fullReadEvery int

	// key-event inputs (PEERSSTREAMS2.md): `kick` wakes the loop for an
	// immediate counter check (every hop add/remove bumps the counter, so
	// the normal tick body delivers the change); `forceResync` additionally
	// discards the synced state (dropped events, subscriber (re)connect)
	kick        chan struct{}
	forceResync atomic.Bool
	reconcile   atomic.Bool
}

// NewStreamHopListener polls the client's hops version counter every
// `pollInterval` (jittered ±20%) and emits a Reset event with the full hop
// set whenever the counter moved (PEERS2.md). Every `fullReadEvery`-th tick
// full-reads unconditionally as insurance against a missed bump. No
// subscriptions, no standing connections.
func NewStreamHopListener(ctx context.Context, clientId server.Id, callback func(*StreamHopEvent), pollInterval time.Duration, fullReadEvery int) *StreamHopListener {
	cancelCtx, cancel := context.WithCancel(ctx)

	shl := &StreamHopListener{
		ctx:           cancelCtx,
		cancel:        cancel,
		clientId:      clientId,
		callback:      callback,
		pollInterval:  pollInterval,
		fullReadEvery: fullReadEvery,
		kick:          make(chan struct{}, 1),
	}
	go server.HandleError(shl.run)
	return shl
}

// Kick wakes the loop for an immediate counter check (a hop-set key event
// arrived). Non-blocking, coalescing. Safe from the subscriber's demux
// goroutine.
func (self *StreamHopListener) Kick() {
	select {
	case self.kick <- struct{}{}:
	default:
	}
}

// Resync forces the next wake-up to full-read regardless of the version
// counter (events may have been dropped). Non-blocking, coalescing.
func (self *StreamHopListener) Resync() {
	self.forceResync.Store(true)
	self.Kick()
}

// Reconcile forces an authoritative full read without forcing a callback when
// state is unchanged. It repairs expiry/missed events without periodic reset
// traffic to clients.
func (self *StreamHopListener) Reconcile() {
	self.reconcile.Store(true)
	self.Kick()
}

func (self *StreamHopListener) run() {
	defer self.cancel()

	// the last synced version. `!=` (never `<`): the counter moves backward
	// when it ttl-expires (clientEventIdTtl) or the db is flushed — any
	// mismatch resyncs from a full read. (The v1 `<` comparison could go
	// permanently stale across a counter reset.)
	var eventId int64
	synced := false
	streamHopState := map[StreamHop]bool{}

	// full-read and deliver the snapshot when the version moved. `!=` (never
	// `<`): the counter moves backward when it ttl-expires or the db is
	// flushed. A flush-and-rebuild is not atomic in production (hops
	// repopulate over time), so a poll observes the intermediate state and
	// the version diverges — the comparison catches every realistic case
	// without re-delivering on a static hop set. (The v1 `<` comparison could
	// go permanently stale across a counter reset.)
	reset := func() {
		resetEventId, resetStreamHops := GetStreamHops(self.ctx, self.clientId)
		stateChanged := !maps.Equal(streamHopState, resetStreamHops)
		eventId = resetEventId
		streamHopState = resetStreamHops
		if !synced || stateChanged {
			synced = true

			resetEvent := &StreamHopEvent{
				StreamHopEventType: StreamHopEventTypeReset,
				EventId:            resetEventId,
				StreamHops:         slices.Collect(maps.Keys(resetStreamHops)),
			}
			self.callback(resetEvent)
		}
	}

	jitterInterval := func(consecutiveErrors int) time.Duration {
		// ±20% jitter spreads fleet polls; error backoff (up to 4x) keeps a
		// sick slot from being hammered
		interval := self.pollInterval + time.Duration((mathrand.Float64()*0.4-0.2)*float64(self.pollInterval))
		return interval << consecutiveErrors
	}

	consecutiveErrors := 0
	tick := 0
	// the poll deadline is absolute so a stream of kicks cannot starve the
	// corrective poll (PEERSSTREAMS2.md §5.4)
	nextPollTime := time.Now().Add(jitterInterval(0))
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.kick:
		case <-time.After(time.Until(nextPollTime)):
			nextPollTime = time.Now().Add(jitterInterval(consecutiveErrors))
		}

		tick += 1
		force := self.forceResync.Swap(false)
		reconcile := self.reconcile.Swap(false)
		// contain redis panics to the tick: a failed poll must never kill the
		// listener (2026-07-15: panicked listeners died permanently and their
		// clients silently stopped receiving updates). The periodic full read
		// (fullReadEvery) re-reads as a cheap hygiene check; it still only
		// delivers on a version change. A forced resync (dropped key events,
		// subscriber (re)connect) always full-reads.
		if r := server.HandleError(func() {
			if force {
				synced = false
			}
			if !synced ||
				reconcile ||
				(0 < self.fullReadEvery && tick%self.fullReadEvery == 0) ||
				GetStreamEventId(self.ctx, self.clientId) != eventId {
				reset()
			}
		}); r != nil {
			consecutiveErrors = min(consecutiveErrors+1, 2)
			if force {
				// the resync did not complete; keep it pending
				self.forceResync.Store(true)
			}
			if reconcile {
				self.reconcile.Store(true)
			}
		} else {
			consecutiveErrors = 0
		}
	}
}

func (self *StreamHopListener) Close() {
	self.cancel()
}

type StreamHopAccumulator struct {
	addedCallback   func(StreamHop)
	removedCallback func(StreamHop)

	stateLock sync.Mutex
	hops      map[StreamHop]bool
}

func NewStreamHopAccumulator(addedCallback func(StreamHop), removedCallback func(StreamHop)) *StreamHopAccumulator {
	return &StreamHopAccumulator{
		addedCallback:   addedCallback,
		removedCallback: removedCallback,
		hops:            map[StreamHop]bool{},
	}
}

func (self *StreamHopAccumulator) Event(event *StreamHopEvent) {
	var added []StreamHop
	var removed []StreamHop

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		switch event.StreamHopEventType {
		case StreamHopEventTypeAdded:
			for _, hop := range event.StreamHops {
				if !self.hops[hop] {
					self.hops[hop] = true
					added = append(added, hop)
				}
			}
		case StreamHopEventTypeRemoved:
			for _, hop := range event.StreamHops {
				if self.hops[hop] {
					delete(self.hops, hop)
					removed = append(removed, hop)
				}
			}
		case StreamHopEventTypeReset:
			hops := map[StreamHop]bool{}
			for _, hop := range event.StreamHops {
				hops[hop] = true
			}

			for hop, _ := range self.hops {
				if !hops[hop] {
					removed = append(removed, hop)
				}
			}
			for hop, _ := range hops {
				if !self.hops[hop] {
					added = append(added, hop)
				}
			}
			self.hops = hops
		}
	}()

	if self.addedCallback != nil {
		for _, hop := range added {
			self.addedCallback(hop)
		}
	}
	if self.removedCallback != nil {
		for _, hop := range removed {
			self.removedCallback(hop)
		}
	}
}

func (self *StreamHopAccumulator) StreamHops() map[StreamHop]bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	streamHops := maps.Clone(self.hops)
	return streamHops
}

func (self *StreamHopAccumulator) StreamIds() map[server.Id]bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	streamIds := map[server.Id]bool{}
	for hop, _ := range self.hops {
		streamIds[hop.StreamId()] = true
	}
	return streamIds
}
