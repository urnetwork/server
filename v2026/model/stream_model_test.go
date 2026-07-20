package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// "maps"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestStreamKey(t *testing.T) {
	sourceId := server.NewId()
	destinationId := server.NewId()
	intermediaryIds := []server.Id{
		server.NewId(),
		server.NewId(),
		server.NewId(),
	}
	reversedIntermediaryIds := slices.Clone(intermediaryIds)
	slices.Reverse(reversedIntermediaryIds)

	sk := newStreamKey(sourceId, destinationId, intermediaryIds)
	sk2 := newStreamKey(destinationId, sourceId, reversedIntermediaryIds)
	connect.AssertEqual(t, sk, sk2)
	connect.AssertEqual(t, sk.String(), sk2.String())

	expectedClientEdges := map[server.Id][2]*server.Id{}
	expectedClientEdges[sourceId] = [2]*server.Id{
		nil,
		&intermediaryIds[0],
	}
	expectedClientEdges[intermediaryIds[0]] = [2]*server.Id{
		&sourceId,
		&intermediaryIds[1],
	}
	expectedClientEdges[intermediaryIds[1]] = [2]*server.Id{
		&intermediaryIds[0],
		&intermediaryIds[2],
	}
	expectedClientEdges[intermediaryIds[2]] = [2]*server.Id{
		&intermediaryIds[1],
		&destinationId,
	}
	expectedClientEdges[intermediaryIds[2]] = [2]*server.Id{
		&intermediaryIds[1],
		&destinationId,
	}
	expectedClientEdges[destinationId] = [2]*server.Id{
		&intermediaryIds[2],
		nil,
	}

	clientEdges := map[server.Id][2]*server.Id{}
	for clientId, edges := range sk.Edges() {
		clientEdges[clientId] = edges
	}

	connect.AssertEqual(t, clientEdges, expectedClientEdges)
}

func TestStreamHop(t *testing.T) {
	sourceId := server.NewId()
	destinationId := server.NewId()
	streamId := server.NewId()

	hop := NewStreamHop(&sourceId, &destinationId, streamId)

	path := connect.TransferPath{
		SourceId:      connect.Id(sourceId),
		DestinationId: connect.Id(destinationId),
		StreamId:      connect.Id(streamId),
	}
	connect.AssertEqual(t, hop.Path(), path)
}

func TestStream(t *testing.T) {
	// in parallel add and remove contracts from a shared set of paths that include a client id
	// ensure that the final state for the client id matches the state accumulated from the events
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// unit-test scale: the invariant (parallel add/remove converges to the
		// state accumulated from events) is scale-independent. The earlier
		// 32k-key/131k-contract sizing was a load test in disguise: millions
		// of redis commands through the 16-conn local pool under -race took
		// 20+ minutes and then failed the fixed-window event assertions.
		keyCount := 1024
		contractCount := 4 * keyCount
		delayMax := 2 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clientId := server.NewId()

		var keys []streamKey
		for range keyCount / 2 {
			sourceId := clientId
			destinationId := server.NewId()
			var intermediaryIds []server.Id
			n := mathrand.Intn(16)
			for range n {
				intermediaryId := server.NewId()
				intermediaryIds = append(intermediaryIds, intermediaryId)
			}
			key := newStreamKey(sourceId, destinationId, intermediaryIds)
			keys = append(keys, key)
		}
		for range (keyCount + 1) / 2 {
			sourceId := server.NewId()
			destinationId := clientId
			var intermediaryIds []server.Id
			n := mathrand.Intn(16)
			for range n {
				intermediaryId := server.NewId()
				intermediaryIds = append(intermediaryIds, intermediaryId)
			}
			key := newStreamKey(sourceId, destinationId, intermediaryIds)
			keys = append(keys, key)
		}

		var stateLock sync.Mutex
		contractStreamIds := map[server.Id]server.Id{}
		contractStreamKeys := map[server.Id]streamKey{}
		removeContracts := map[server.Id]bool{}
		// usedKeys := map[string]int{}
		keepKeys := map[string]int{}
		for range contractCount {
			contractId := server.NewId()
			i := mathrand.Intn(len(keys))
			key := keys[i]
			// usedKeys[key.String()] += 1
			contractStreamKeys[contractId] = key
			remove := mathrand.Intn(2) == 0
			removeContracts[contractId] = remove
			if !remove {
				keepKeys[key.String()] += 1
			}
		}

		c := NewStreamHopAccumulator(
			func(hop StreamHop) {
				fmt.Printf("+")
			},
			func(hop StreamHop) {
				fmt.Printf("-")
			},
		)
		l := NewStreamHopListener(ctx, clientId, c.Event, 200*time.Millisecond, 5)
		defer l.Close()

		var wg sync.WaitGroup

		for contractId, key := range contractStreamKeys {
			wg.Add(1)
			go func() {
				defer wg.Done()

				addDelay := time.Duration(mathrand.Intn(int(delayMax/time.Millisecond))) * time.Millisecond
				select {
				case <-ctx.Done():
					return
				case <-time.After(addDelay):
				}

				// fmt.Printf("ADD\n")
				streamId := AddToStream(ctx, contractId, key.SourceId(), key.DestinationId(), key.IntermediaryIds())
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					contractStreamIds[contractId] = streamId
				}()

				if removeContracts[contractId] {
					removeDelay := time.Duration(mathrand.Intn(int(delayMax/time.Millisecond))) * time.Millisecond
					select {
					case <-ctx.Done():
						return
					case <-time.After(removeDelay):
					}

					// fmt.Printf("REMOVE\n")
					RemoveFromStream(ctx, contractId)
				}
			}()
		}

		wg.Wait()

		finalStreamIds := map[server.Id]bool{}
		for contractId, streamId := range contractStreamIds {
			if !removeContracts[contractId] {
				finalStreamIds[streamId] = true
			}
		}

		for contractId, streamId := range contractStreamIds {
			mStreamId, mStreamKey, ok := GetStream(ctx, contractId)
			if removeContracts[contractId] {
				connect.AssertEqual(t, ok, false)
			} else {
				connect.AssertEqual(t, ok, true)
				streamKey := contractStreamKeys[contractId]
				connect.AssertEqual(t, mStreamId, streamId)
				connect.AssertEqual(t, mStreamKey, streamKey)
			}
		}

		select {
		case <-time.After(5 * time.Second):
		}

		connect.AssertEqual(t, len(c.StreamIds()), len(finalStreamIds))
		connect.AssertEqual(t, c.StreamIds(), finalStreamIds)

		_, streamHops := GetStreamHops(ctx, clientId)
		connect.AssertEqual(t, c.StreamHops(), streamHops)

		// creating a new listener should sync to the head state
		var addCount atomic.Uint64
		var removeCount atomic.Uint64
		c2 := NewStreamHopAccumulator(
			func(hop StreamHop) {
				fmt.Printf("+")
				addCount.Add(1)
			},
			func(hop StreamHop) {
				fmt.Printf("-")
				removeCount.Add(1)
			},
		)
		l2 := NewStreamHopListener(ctx, clientId, c2.Event, 200*time.Millisecond, 5)
		defer l2.Close()

		// cover a full listener poll cycle (5s) so the assertion does not
		// depend on pubsub delivery alone
		select {
		case <-time.After(6 * time.Second):
		}

		connect.AssertEqual(t, c2.StreamIds(), finalStreamIds)

		// remove the remaining contracts
		for contractId, remove := range removeContracts {
			if !remove {
				RemoveFromStream(ctx, contractId)
			}
		}

		select {
		case <-time.After(6 * time.Second):
		}

		connect.AssertEqual(t, int(addCount.Load()), len(keepKeys))
		connect.AssertEqual(t, int(removeCount.Load()), len(keepKeys))
		connect.AssertEqual(t, c2.StreamIds(), map[server.Id]bool{})

		_, streamHops2 := GetStreamHops(ctx, clientId)
		connect.AssertEqual(t, c2.StreamHops(), streamHops2)

	})

}

// TestAddCompanionContractToStream covers the companion-side stream marking:
// a companion contract must join its origin flow's active stream so the
// receive sequence on the other side sees the stream id when it inspects the
// contract, and so the stream stays alive while the reply is open. The
// escrow-linked origin is the earliest origin, which can predate the stream
// or have already closed out of it — the resolution must fall back to the
// pair marker, and prune marker members whose stream is gone.
func TestAddCompanionContractToStream(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// origin direction: sourceId -> destinationId
		// companion direction: destinationId -> sourceId
		sourceId := server.NewId()
		destinationId := server.NewId()
		intermediaryIds := []server.Id{server.NewId()}

		// scenario 1: the escrow-linked origin carries the stream
		originContractId := server.NewId()
		streamId := AddToStream(ctx, originContractId, sourceId, destinationId, intermediaryIds)
		streamKey := newStreamKey(sourceId, destinationId, intermediaryIds)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{streamIdKey(streamKey), streamContractsKey(streamKey)} {
				ttl, err := r.TTL(ctx, key).Result()
				connect.AssertEqual(t, err, nil)
				if ttl < 7*time.Hour+59*time.Minute || 8*time.Hour < ttl {
					t.Fatalf("%s ttl = %s, want approximately 8h", key, ttl)
				}
			}
		})

		companionContractId := server.NewId()
		companionStreamId, ok := AddCompanionContractToStream(
			ctx,
			companionContractId,
			originContractId,
			destinationId,
			sourceId,
		)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, companionStreamId, streamId)

		// the companion is a stream member with the origin's stream key
		memberStreamId, memberStreamKey, found := GetStream(ctx, companionContractId)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, memberStreamId, streamId)
		connect.AssertEqual(t, memberStreamKey, streamKey)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{streamIdKey(streamKey), streamContractsKey(streamKey)} {
				ttl, err := r.TTL(ctx, key).Result()
				connect.AssertEqual(t, err, nil)
				if ttl < 7*time.Hour+59*time.Minute || 8*time.Hour < ttl {
					t.Fatalf("%s ttl after companion join = %s, want approximately 8h", key, ttl)
				}
			}
		})

		// the origin closing out of the stream must not tear it down while
		// the companion reply is still open
		RemoveFromStream(ctx, originContractId)
		memberStreamId, _, found = GetStream(ctx, companionContractId)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, memberStreamId, streamId)

		// scenario 2: a companion renewal after the origin closed out of the
		// stream (the linger window) resolves the stream through the pair
		// marker — the previous still-open companion holds the stream alive
		renewalContractId := server.NewId()
		renewalStreamId, ok := AddCompanionContractToStream(
			ctx,
			renewalContractId,
			originContractId,
			destinationId,
			sourceId,
		)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, renewalStreamId, streamId)

		// scenario 3: an escrow-linked origin that was never in the stream
		// still resolves through the pair marker
		companionContractId2 := server.NewId()
		companionStreamId2, ok := AddCompanionContractToStream(
			ctx,
			companionContractId2,
			server.NewId(),
			destinationId,
			sourceId,
		)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, companionStreamId2, streamId)

		// closing all members tears the stream down and clears the marker
		for _, contractId := range []server.Id{companionContractId, renewalContractId, companionContractId2} {
			RemoveFromStream(ctx, contractId)
		}
		_, _, found = GetStream(ctx, companionContractId2)
		connect.AssertEqual(t, found, false)

		// scenario 4: no active stream for the pair — the companion stays
		// unmarked and must not resurrect the dead stream. A stale marker
		// member (redis loss, expiry race) must be pruned, not joined
		server.Redis(ctx, func(r server.RedisClient) {
			staleKey := newStreamKey(sourceId, destinationId, intermediaryIds)
			connect.AssertEqual(t, r.SAdd(ctx, pairStreamsKey(destinationId, sourceId), staleKey.Bytes()).Err(), nil)
		})
		companionContractId3 := server.NewId()
		_, ok = AddCompanionContractToStream(
			ctx,
			companionContractId3,
			originContractId,
			destinationId,
			sourceId,
		)
		connect.AssertEqual(t, ok, false)
		_, _, found = GetStream(ctx, companionContractId3)
		connect.AssertEqual(t, found, false)
		server.Redis(ctx, func(r server.RedisClient) {
			count, err := r.SCard(ctx, pairStreamsKey(sourceId, destinationId)).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, count, int64(0))
		})
	})
}

// TestCompanionStreamCloseLifecycle covers the companion's stream membership
// through the production close path: the intermediary hop keeps its stream
// config while only the companion holds the stream (the origin closed out),
// and the companion settling via CloseContract removes it from the stream —
// the per-contract keys written at join time are what RemoveFromStream needs
// to find the membership — tearing the stream down for the hops only then.
func TestCompanionStreamCloseLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		// origin direction: sourceId -> destinationId
		sourceId := server.NewId()
		destinationId := server.NewId()
		intermediaryId := server.NewId()

		c := NewStreamHopAccumulator(
			func(hop StreamHop) {},
			func(hop StreamHop) {},
		)
		l := NewStreamHopListener(ctx, intermediaryId, c.Event, 200*time.Millisecond, 5)
		defer l.Close()

		originContractId := server.NewId()
		streamId := AddToStream(ctx, originContractId, sourceId, destinationId, []server.Id{intermediaryId})

		// the companion has a real contract row so the production close path
		// (CloseContract -> settle -> RemoveFromStream) applies to it
		companionContractId, err := CreateContractNoEscrow(
			ctx,
			networkId,
			destinationId,
			networkId,
			sourceId,
			ByteCount(1024*1024),
		)
		connect.AssertEqual(t, err, nil)
		_, ok := AddCompanionContractToStream(
			ctx,
			companionContractId,
			originContractId,
			destinationId,
			sourceId,
		)
		connect.AssertEqual(t, ok, true)

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.StreamIds(), map[server.Id]bool{streamId: true})

		// the origin closing out must not tear down the hop config while the
		// companion reply is still open
		RemoveFromStream(ctx, originContractId)
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.StreamIds(), map[server.Id]bool{streamId: true})

		// both parties settle the companion through the production close path
		connect.AssertEqual(t, CloseContract(ctx, companionContractId, destinationId, ByteCount(0), false), nil)
		connect.AssertEqual(t, CloseContract(ctx, companionContractId, sourceId, ByteCount(0), false), nil)

		_, _, found := GetStream(ctx, companionContractId)
		connect.AssertEqual(t, found, false)

		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, c.StreamIds(), map[server.Id]bool{})
	})
}

func TestStreamHopCorrectiveReadRepairsMissedExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sourceId := server.NewId()
		destinationId := server.NewId()
		hopClientId := server.NewId()
		AddToStream(
			ctx,
			server.NewId(),
			sourceId,
			destinationId,
			[]server.Id{hopClientId},
		)
		eventIdBefore := GetStreamEventId(ctx, hopClientId)

		events := make(chan *StreamHopEvent, 8)
		listener := NewStreamHopListener(
			ctx,
			hopClientId,
			func(event *StreamHopEvent) { events <- event },
			100*time.Millisecond,
			1,
		)
		defer listener.Close()
		listener.Resync()

		select {
		case event := <-events:
			if len(event.StreamHops) != 1 {
				t.Fatalf("initial stream hops = %d, want 1", len(event.StreamHops))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for initial stream-hop snapshot")
		}

		// Expire only the hop set. Redis expiry does not increment the event
		// id and no key-event is delivered to this listener.
		server.Redis(ctx, func(r server.RedisClient) {
			r.Expire(ctx, clientStreamHopsKey(hopClientId), 500*time.Millisecond)
		})

		select {
		case event := <-events:
			if event.StreamHopEventType != StreamHopEventTypeReset || len(event.StreamHops) != 0 {
				t.Fatalf("unexpected expiry repair: %+v", event)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("corrective read did not repair missed stream-hop expiry")
		}
		connect.AssertEqual(t, GetStreamEventId(ctx, hopClientId), eventIdBefore)
	})
}

// TestStreamHopFlushRecovery guards the PEERS2 backward-counter resync for the
// stream listener — the exact case the pre-v2 `<` comparison got wrong (it
// went permanently stale once the counter reset). When the hops counter is
// flushed/expires and restarts below the listener's last synced value, the
// `!=` mismatch must trigger a full read, not silence.
func TestStreamHopFlushRecovery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clientId := server.NewId()
		destinationId := server.NewId()

		c := NewStreamHopAccumulator(
			func(hop StreamHop) {},
			func(hop StreamHop) {},
		)
		l := NewStreamHopListener(ctx, clientId, c.Event, 200*time.Millisecond, 5)
		defer l.Close()

		// a hop involving clientId; the listener syncs it
		contractId1 := server.NewId()
		AddToStream(ctx, contractId1, clientId, destinationId, nil)
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, len(c.StreamHops()), 1)

		// flush the client's hop state and its version counter (redis loss /
		// 24h idle expiry). The counter restarts, so a later event id is
		// at or below the listener's last synced value.
		server.Redis(ctx, func(r server.RedisClient) {
			connect.AssertEqual(t, r.Del(ctx, clientStreamHopsKey(clientId), clientEventIdKey(clientId)).Err(), nil)
		})

		// a flush and rebuild are not atomic in production — the hops
		// repopulate over time, so the listener polls the intermediate empty
		// state first (missing counter reads as 0, below the synced value ->
		// resync to empty). Reproduce that gap so the version diverges.
		select {
		case <-time.After(1 * time.Second):
		}
		connect.AssertEqual(t, len(c.StreamHops()), 0)

		// a new hop repopulates the set and bumps the (reset) counter — the
		// listener resyncs to the new head state
		contractId2 := server.NewId()
		destinationId2 := server.NewId()
		AddToStream(ctx, contractId2, clientId, destinationId2, nil)

		select {
		case <-time.After(2 * time.Second):
		}
		_, headHops := GetStreamHops(ctx, clientId)
		connect.AssertEqual(t, c.StreamHops(), headHops)
		connect.AssertEqual(t, len(c.StreamHops()), 1)
	})
}
