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

	"github.com/go-playground/assert/v2"

	// "golang.org/x/exp/maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
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
	assert.Equal(t, sk, sk2)
	assert.Equal(t, sk.String(), sk2.String())

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

	assert.Equal(t, clientEdges, expectedClientEdges)
}

func TestStreamHop(t *testing.T) {
	sourceId := server.NewId()
	destinationId := server.NewId()
	streamId := server.NewId()

	hop := NewStreamHop(sourceId, destinationId, streamId)

	path := connect.TransferPath{
		SourceId:      connect.Id(sourceId),
		DestinationId: connect.Id(destinationId),
		StreamId:      connect.Id(streamId),
	}
	assert.Equal(t, hop.Path(), path)
}

func TestStream(t *testing.T) {
	// in parallel add and remove contracts from a shared set of paths that include a client id
	// ensure that the final state for the client id matches the state accumulated from the events
	server.DefaultTestEnv().Run(func() {

		keyCount := 32 * 1024
		contractCount := 4 * keyCount
		delayMax := 10 * time.Second

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
		l := NewStreamHopListener(ctx, clientId, c.Event, 5*time.Second)
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
				assert.Equal(t, ok, false)
			} else {
				assert.Equal(t, ok, true)
				streamKey := contractStreamKeys[contractId]
				assert.Equal(t, mStreamId, streamId)
				assert.Equal(t, mStreamKey, streamKey)
			}
		}

		select {
		case <-time.After(1 * time.Second):
		}

		assert.Equal(t, len(c.StreamIds()), len(finalStreamIds))
		assert.Equal(t, c.StreamIds(), finalStreamIds)

		_, streamHops := GetStreamHops(ctx, clientId)
		assert.Equal(t, c.StreamHops(), streamHops)

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
		l2 := NewStreamHopListener(ctx, clientId, c2.Event, 5*time.Second)
		defer l2.Close()

		select {
		case <-time.After(1 * time.Second):
		}

		assert.Equal(t, c2.StreamIds(), finalStreamIds)

		// remove the remaining contracts
		for contractId, remove := range removeContracts {
			if !remove {
				RemoveFromStream(ctx, contractId)
			}
		}

		select {
		case <-time.After(1 * time.Second):
		}

		assert.Equal(t, int(addCount.Load()), len(keepKeys))
		assert.Equal(t, int(removeCount.Load()), len(keepKeys))
		assert.Equal(t, c2.StreamIds(), map[server.Id]bool{})

		_, streamHops2 := GetStreamHops(ctx, clientId)
		assert.Equal(t, c2.StreamHops(), streamHops2)

	})

}
