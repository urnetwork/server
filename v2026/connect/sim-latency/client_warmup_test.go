package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestBuildWarmClientPoolRetriesMissingAndPreservesOrder(t *testing.T) {
	pool := make([]ClientIdentity, 5)
	for index := range pool {
		pool[index].ClientId = server.NewId()
	}

	attemptCounts := make([]int, len(pool))
	var attemptLock sync.Mutex
	clients := buildWarmClientPool(
		context.Background(),
		pool,
		3,
		0,
		func(identity ClientIdentity, poolIndex int) *pooledClient {
			attemptLock.Lock()
			attemptCounts[poolIndex]++
			attempt := attemptCounts[poolIndex]
			attemptLock.Unlock()

			// Make completion order differ from fixture order. Slot 1 also
			// simulates the transient first-attempt miss seen in the campaign.
			time.Sleep(time.Duration(len(pool)-poolIndex) * time.Millisecond)
			if poolIndex == 1 && attempt == 1 {
				return nil
			}
			return &pooledClient{label: identity.ClientId.String()}
		},
	)

	if len(clients) != len(pool) {
		t.Fatalf("warm clients = %d, want %d", len(clients), len(pool))
	}
	for index, client := range clients {
		if want := pool[index].ClientId.String(); client.label != want {
			t.Fatalf("client %d label = %q, want %q", index, client.label, want)
		}
		wantAttempts := 1
		if index == 1 {
			wantAttempts = 2
		}
		if attemptCounts[index] != wantAttempts {
			t.Fatalf(
				"client %d attempts = %d, want %d",
				index,
				attemptCounts[index],
				wantAttempts,
			)
		}
	}
}

func TestBuildWarmClientPoolHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	clients := buildWarmClientPool(
		ctx,
		[]ClientIdentity{{ClientId: server.NewId()}},
		3,
		0,
		func(ClientIdentity, int) *pooledClient {
			called = true
			return &pooledClient{}
		},
	)
	if called || len(clients) != 0 {
		t.Fatalf("canceled warmup called builder=%t, clients=%d", called, len(clients))
	}
}
