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
		// BELOW the listener's last synced value.
		server.Redis(ctx, func(r server.RedisClient) {
			connect.AssertEqual(t, r.Del(ctx, clientStreamHopsKey(clientId), clientEventIdKey(clientId)).Err(), nil)
		})

		// a new hop repopulates the set and bumps the (reset) counter to 1,
		// below the listener's previous value — the `!=` resync must fire and
		// deliver the new head state
		contractId2 := server.NewId()
		destinationId2 := server.NewId()
		AddToStream(ctx, contractId2, clientId, destinationId2, nil)

		select {
		case <-time.After(3 * time.Second):
		}
		_, headHops := GetStreamHops(ctx, clientId)
		connect.AssertEqual(t, c.StreamHops(), headHops)
		connect.AssertEqual(t, len(c.StreamHops()), 1)
	})
}
