// The Find script's baseline on the test Redis (SOCK-M5-17's BenchmarkFindScript; §22.8, SOCK.md reading 15): one Pick
// a call on server's store, which is the whole of a passing Find's Redis work, over a group of the sample's 64
// listeners, and a keyed Pick over a group of 1,000 listeners that opted in to affinity, which reads its whole group
// (SOCK-M6-01). The private stack's Redis is one node on a laptop, so the numbers are a baseline to compare a change
// against, not §22.8's evidence, which is a T5 host's (the load harness, sidecartest.SocketLoad). The benchmarks run
// inside a test, since the test Redis comes only through server's test environment, and log what testing.Benchmark
// measured; -short skips them.

package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A group of listenerCount listeners on store, each opted in to affinity when affinity is set, and its key.
func benchFindGroup(b testing.TB, ctx context.Context, store *model.SocketStore, listenerCount int, affinity bool) (connect.Id, []byte) {
	b.Helper()
	networkId := connect.NewId()
	nameToken := connect.NewId()
	for index := range listenerCount {
		listenId := connect.NewId()
		_, err := store.ApplyPage(ctx, &socketsservice.SocketStorePage{
			NetworkId:        networkId,
			ListenerClientId: connect.NewId(),
			SetVersion:       1,
			Registrations: []*sidecar.SocketRegistration{{
				NameToken:  nameToken[:],
				SealedName: make([]byte, 152),
				ListenId:   &listenId,
				Protocol:   "tcp",
				Weight:     100,
				MaxStreams: 256,
				Serving:    true,
				Affinity:   affinity,
				Inflight:   index % 50,
				Claim:      []byte("bench claim"),
			}},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	return networkId, nameToken[:]
}

// One Pick a call, by sixteen dialers in turn, so each dialer's share stays within its clamp.
func benchFindPicks(store *model.SocketStore, networkId connect.Id, nameKey []byte, affinityKeys [][]byte) func(b *testing.B) {
	return func(b *testing.B) {
		ctx := context.Background()
		dialerClientIds := make([]connect.Id, 16)
		for index := range dialerClientIds {
			dialerClientIds[index] = connect.NewId()
		}
		b.ReportAllocs()
		index := 0
		for b.Loop() {
			request := &socketsservice.SocketStorePickRequest{
				NetworkId:       networkId,
				NameKey:         nameKey,
				DialerClientId:  dialerClientIds[index%len(dialerClientIds)],
				DialerNetworkId: networkId,
				FirstDraw:       uint32(index * 2654435761),
				SecondDraw:      uint32(index * 40503),
			}
			if 0 < len(affinityKeys) {
				request.AffinityKey = affinityKeys[index%len(affinityKeys)]
			}
			pick, err := store.Pick(ctx, request)
			if err != nil || pick.Outcome != socketsservice.SocketPickFound {
				b.Fatalf("a pick: %+v, %v", pick, err)
			}
			index += 1
		}
	}
}

// BenchmarkFindScript's two runs on the test Redis, logged.
func TestT3FindScriptBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("a baseline is measured only in a full run")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		store := model.NewSocketStore(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil)
		networkId, nameKey := benchFindGroup(t, ctx, store, socketsservice.DefaultSocketStoreSettings().SampleCount, false)
		sampled := testing.Benchmark(benchFindPicks(store, networkId, nameKey, nil))
		t.Logf("BenchmarkFindScript, a group of %d: %s %s", socketsservice.DefaultSocketStoreSettings().SampleCount, sampled.String(), sampled.MemString())

		keyedNetworkId, keyedNameKey := benchFindGroup(t, ctx, store, 1000, true)
		affinityKeys := [][]byte{}
		for index := range 64 {
			affinityKeys = append(affinityKeys, []byte(fmt.Sprintf("bench visitor %d", index)))
		}
		keyed := testing.Benchmark(benchFindPicks(store, keyedNetworkId, keyedNameKey, affinityKeys))
		t.Logf("BenchmarkFindScript, a keyed pick over a group of 1,000 that opted in: %s %s", keyed.String(), keyed.MemString())
		if sampled.N == 0 || keyed.N == 0 {
			t.Fatal("a benchmark ran no picks")
		}
	})
}
