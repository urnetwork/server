package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// "maps"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Listener shutdown joins a callback already dispatched by the poll worker so
// an owner may use CloseAndWait as a deterministic resource boundary.
func TestStreamHopListenerCloseAndWaitJoinsAdmittedCallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer testCancel()
		callbackEntered := make(chan struct{})
		releaseCallback := make(chan struct{})
		callbackReturned := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(releaseCallback) })
		listener := newStreamHopListener(
			testCtx,
			server.NewId(),
			func(*StreamHopEvent) {
				close(callbackEntered)
				<-releaseCallback
				close(callbackReturned)
			},
			time.Hour,
			0,
			GetStreamHops,
		)
		listener.afterCloseWaitForTest = func() {
			select {
			case <-callbackReturned:
			default:
				t.Error("listener close wait completed before admitted callback returned")
			}
		}
		listener.Resync()
		select {
		case <-callbackEntered:
		case <-testCtx.Done():
			t.Fatalf("stream hop callback did not enter: %v", testCtx.Err())
		}

		closeDone := make(chan struct{})
		go func() {
			listener.CloseAndWait()
			close(closeDone)
		}()
		select {
		case <-listener.ctx.Done():
		case <-testCtx.Done():
			t.Fatalf("stream hop listener did not close: %v", testCtx.Err())
		}
		releaseOnce.Do(func() { close(releaseCallback) })
		select {
		case <-callbackReturned:
		case <-testCtx.Done():
			t.Fatalf("stream hop callback did not return: %v", testCtx.Err())
		}
		select {
		case <-closeDone:
		case <-testCtx.Done():
			t.Fatalf("stream hop listener did not join callback: %v", testCtx.Err())
		}
	})
}

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

// Reproduces Redis stream state that outlived removed one-shot clients. The
// reconnecting snapshot omits both inactive hop orientations while retaining
// an active client without a live connection row.
func TestStreamHopListenerPrunesInactiveAdjacentClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("stream-active-%s", networkId), server.NewId())

		orderedClientIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		slices.SortFunc(orderedClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		lowerInactiveId := orderedClientIds[0]
		clientId := orderedClientIds[1]
		upperInactiveId := orderedClientIds[2]
		activeDisconnectedId := server.NewId()
		for _, id := range []server.Id{lowerInactiveId, clientId, upperInactiveId, activeDisconnectedId} {
			Testing_CreateDevice(ctx, networkId, server.NewId(), id, "", "")
		}

		lowerContractId := server.NewId()
		upperContractId := server.NewId()
		activeContractId := server.NewId()
		lowerStreamId := AddToStream(ctx, lowerContractId, lowerInactiveId, clientId, nil)
		upperStreamId := AddToStream(ctx, upperContractId, clientId, upperInactiveId, nil)
		activeStreamId := AddToStream(ctx, activeContractId, clientId, activeDisconnectedId, nil)
		defer func() {
			RemoveFromStream(ctx, lowerContractId)
			RemoveFromStream(ctx, upperContractId)
			RemoveFromStream(ctx, activeContractId)
		}()

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET active = false WHERE client_id = ANY($1::uuid[])`,
				[]string{lowerInactiveId.String(), upperInactiveId.String()},
			))
		})

		var connectionCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT count(*) FROM network_client_connection WHERE client_id = $1`,
				activeDisconnectedId,
			)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("missing connection count")
				}
				server.Raise(result.Scan(&connectionCount))
			})
		})
		if connectionCount != 0 {
			t.Fatalf("active client has %d connection rows, want none", connectionCount)
		}

		eventIdBefore, hopsBefore := GetStreamHops(ctx, clientId)
		if len(hopsBefore) != 3 {
			t.Fatalf("cached stream hops before listener = %d, want 3", len(hopsBefore))
		}

		events := make(chan *StreamHopEvent, 1)
		listener := NewStreamHopListener(
			ctx,
			clientId,
			func(event *StreamHopEvent) { events <- event },
			time.Hour,
			0,
		)
		listener.Resync()
		var initialEvent *StreamHopEvent
		select {
		case initialEvent = <-events:
		case <-ctx.Done():
			listener.CloseAndWait()
			t.Fatalf("stream listener did not publish its initial snapshot: %v", ctx.Err())
		}
		listener.CloseAndWait()

		if len(initialEvent.StreamHops) != 1 {
			t.Fatalf("initial active stream hops = %d, want 1", len(initialEvent.StreamHops))
		}
		if got := initialEvent.StreamHops[0].StreamId(); got != activeStreamId {
			t.Fatalf("initial stream id = %s, want active disconnected stream %s", got, activeStreamId)
		}
		for _, staleStreamId := range []server.Id{lowerStreamId, upperStreamId} {
			if initialEvent.StreamHops[0].StreamId() == staleStreamId {
				t.Fatalf("initial snapshot retained inactive stream %s", staleStreamId)
			}
		}

		eventIdAfter, hopsAfter := GetStreamHops(ctx, clientId)
		if eventIdAfter <= eventIdBefore {
			t.Fatalf("stream event id after prune = %d, want greater than %d", eventIdAfter, eventIdBefore)
		}
		if len(hopsAfter) != 1 || !hopsAfter[initialEvent.StreamHops[0]] {
			t.Fatalf("cached stream hops after prune = %v, want initial active hop", hopsAfter)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET active = false WHERE client_id = $1`,
				clientId,
			))
		})
		finalEventId, finalHops := GetActiveStreamHops(ctx, clientId)
		if len(finalHops) != 0 {
			t.Fatalf("inactive owner stream hops = %v, want empty", finalHops)
		}
		if finalEventId != eventIdAfter {
			t.Fatalf("inactive owner snapshot event id = %d, want pre-prune %d", finalEventId, eventIdAfter)
		}
		if currentEventId := GetStreamEventId(ctx, clientId); currentEventId <= finalEventId {
			t.Fatalf("inactive owner current event id = %d, want greater than returned %d", currentEventId, finalEventId)
		}
	})
}

// Forces a stale writer to re-add a removed hop at the prune boundary. The
// filtered call remains bounded and returns the older version so the next
// listener tick cannot miss the concurrent write.
func TestActiveStreamHopsBoundsConcurrentStaleReAdd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("stream-readd-%s", networkId), server.NewId())
		clientId := server.NewId()
		inactiveId := server.NewId()
		for _, id := range []server.Id{clientId, inactiveId} {
			Testing_CreateDevice(ctx, networkId, server.NewId(), id, "", "")
		}
		contractId := server.NewId()
		AddToStream(ctx, contractId, clientId, inactiveId, nil)
		defer RemoveFromStream(ctx, contractId)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET active = false WHERE client_id = $1`,
				inactiveId,
			))
		})
		_, rawHops := GetStreamHops(ctx, clientId)
		if len(rawHops) != 1 {
			t.Fatalf("raw stream hops = %d, want 1", len(rawHops))
		}
		var staleStreamHop StreamHop
		for streamHop := range rawHops {
			staleStreamHop = streamHop
		}

		hookCalls := 0
		filteredEventId, filteredHops := getActiveStreamHops(ctx, clientId, func() {
			hookCalls++
			server.Redis(ctx, func(r server.RedisClient) {
				pipe := r.TxPipeline()
				pipe.SAdd(ctx, clientStreamHopsKey(clientId), staleStreamHop.Bytes())
				pipe.Incr(ctx, clientEventIdKey(clientId))
				pipe.Expire(ctx, clientStreamHopsKey(clientId), 8*time.Hour)
				pipe.Expire(ctx, clientEventIdKey(clientId), clientEventIdTtl)
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			})
		})
		if hookCalls != 1 {
			t.Fatalf("post-prune hook calls = %d, want 1", hookCalls)
		}
		if len(filteredHops) != 0 {
			t.Fatalf("filtered stream hops = %v, want none", filteredHops)
		}
		if currentEventId := GetStreamEventId(ctx, clientId); currentEventId <= filteredEventId {
			t.Fatalf("current event id = %d, want greater than returned %d", currentEventId, filteredEventId)
		}
		_, rawHops = GetStreamHops(ctx, clientId)
		if !rawHops[staleStreamHop] {
			t.Fatal("concurrent stale re-add was not installed")
		}

		_, filteredHops = GetActiveStreamHops(ctx, clientId)
		if len(filteredHops) != 0 {
			t.Fatalf("second filtered stream hops = %v, want none", filteredHops)
		}
		_, rawHops = GetStreamHops(ctx, clientId)
		if len(rawHops) != 0 {
			t.Fatalf("raw stream hops after convergence = %v, want none", rawHops)
		}
	})
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
		l := newStreamHopListener(ctx, clientId, c.Event, 200*time.Millisecond, 5, GetStreamHops)
		defer l.CloseAndWait()

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
		l2 := newStreamHopListener(ctx, clientId, c2.Event, 200*time.Millisecond, 5, GetStreamHops)
		defer l2.CloseAndWait()

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
		l := newStreamHopListener(ctx, intermediaryId, c.Event, 200*time.Millisecond, 5, GetStreamHops)
		defer l.CloseAndWait()

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
		listener := newStreamHopListener(
			ctx,
			hopClientId,
			func(event *StreamHopEvent) { events <- event },
			100*time.Millisecond,
			1,
			GetStreamHops,
		)
		defer listener.CloseAndWait()
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

// TestExpireLeakedStreamKeys covers the one-shot cleanup for the
// duration-as-nanoseconds ttl leak: keys carrying the effectively-infinite
// pre-fix ttl are clamped to 8h, healthy keys are left alone.
func TestExpireLeakedStreamKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a healthy stream written by the fixed path
		healthyContractId := server.NewId()
		AddToStream(ctx, healthyContractId, server.NewId(), server.NewId(), nil)
		_, healthyKey, found := GetStream(ctx, healthyContractId)
		connect.AssertEqual(t, found, true)

		// Reproduce the pre-fix state: EXPIRE received the 8h ttl in
		// nanoseconds. Use fixed IDs containing NULs so this test proves SCAN
		// -> PTTL -> EXPIRE preserves binary Redis keys. A shell variable does
		// not, and can turn a valid key into a different hash slot and a false
		// MOVED.
		leakedStreamKey := newStreamKey(
			server.Id{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			server.Id{16, 0, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31},
			nil,
		)
		currentLeakedKey := streamIdKey(leakedStreamKey)
		// Cast to []byte to reproduce the historical formatter. Formatting
		// streamKey directly would call its diagnostic String method instead.
		legacyLeakedKey := fmt.Sprintf("{%s}s_sk_cs", []byte(leakedStreamKey))
		durationOverflowLeakedKey := fmt.Sprintf("{%s}s_sk_sid", []byte(leakedStreamKey))
		unrelatedKey := fmt.Sprintf("{%s}nonsense_sk_cache", []byte(leakedStreamKey))
		connect.AssertEqual(t, strings.IndexByte(currentLeakedKey, 0) >= 0, true)
		connect.AssertEqual(t, strings.IndexByte(legacyLeakedKey, 0) >= 0, true)
		connect.AssertEqual(t, isStreamTTLKey(currentLeakedKey), true)
		connect.AssertEqual(t, isStreamTTLKey(legacyLeakedKey), true)
		connect.AssertEqual(t, isStreamTTLKey(durationOverflowLeakedKey), true)
		connect.AssertEqual(t, isStreamTTLKey(unrelatedKey), false)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{currentLeakedKey, legacyLeakedKey, unrelatedKey} {
				connect.AssertEqual(t, r.Set(ctx, key, server.NewId().Bytes(), 0).Err(), nil)
				connect.AssertEqual(t, r.Do(ctx, "EXPIRE", key, int64(8*time.Hour/time.Nanosecond)).Err(), nil)
				leakedTTL, err := r.TTL(ctx, key).Result()
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, 8*time.Hour < leakedTTL, true)
			}

			// 18,446,744,073,710 milliseconds is roughly 584 years and
			// wraps to less than one millisecond when go-redis's typed PTTL
			// helper multiplies it into a time.Duration. The cleanup must use
			// the raw Redis integer or it silently leaves this leaked key.
			const overflowTTLMillis int64 = 18_446_744_073_710
			connect.AssertEqual(t, r.Set(ctx, durationOverflowLeakedKey, server.NewId().Bytes(), 0).Err(), nil)
			connect.AssertEqual(t, r.Do(ctx, "PEXPIRE", durationOverflowLeakedKey, overflowTTLMillis).Err(), nil)
			rawTTLMillis, err := r.Do(ctx, "PTTL", durationOverflowLeakedKey).Int64()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, (8*time.Hour).Milliseconds() < rawTTLMillis, true)
			overflowedTTL, err := r.PTTL(ctx, durationOverflowLeakedKey).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, overflowedTTL <= 8*time.Hour, true)
		})

		scannedCount, fixedCount, err := ExpireLeakedStreamKeys(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 4 <= scannedCount, true)
		connect.AssertEqual(t, 3 <= fixedCount, true)

		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{currentLeakedKey, legacyLeakedKey, durationOverflowLeakedKey} {
				leakedTTLMillis, err := r.Do(ctx, "PTTL", key).Int64()
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, 0 < leakedTTLMillis && leakedTTLMillis <= (8*time.Hour).Milliseconds(), true)
			}
			unrelatedTTLMillis, err := r.Do(ctx, "PTTL", unrelatedKey).Int64()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, (8*time.Hour).Milliseconds() < unrelatedTTLMillis, true)

			healthyTtl, err := r.TTL(ctx, streamIdKey(healthyKey)).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, 0 < healthyTtl && healthyTtl <= 8*time.Hour, true)
		})
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
		l := newStreamHopListener(ctx, clientId, c.Event, 200*time.Millisecond, 5, GetStreamHops)
		defer l.CloseAndWait()

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
