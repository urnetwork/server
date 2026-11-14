package connect

// The cost of the liveness lease writer at a production g-process's size,
// 18,000 connections of which 8,200 provide (CPUOPT4 Part A §5.2), against the
// full walk it replaced (transport_announce_liveness_lease_equivalence_test.go):
// a session's start and end with the woken write it causes, and a full pass.
// The shard writes answer at once, so these measure the writer alone.

import (
	"context"
	mathrand "math/rand/v2"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const livenessLeaseBenchmarkConnectionCount = 18000
const livenessLeaseBenchmarkProvidingCount = 8200

func livenessLeaseBenchmarkShardWrite(ctx context.Context, shard int, scored []server.Id, extended []server.Id, removed []server.Id) ([]bool, error) {
	return make([]bool, len(extended)), nil
}

// A never-started writer holding the benchmark's connections: providing, then
// not providing, then a few pending, all written once.
func newLivenessLeaseBenchmarkWriter(b *testing.B) (*providerLivenessLeaseWriter, *mathrand.Rand) {
	// a fixed seed, so a run replays
	rng := mathrand.New(mathrand.NewPCG(1, 0))
	writer := newProviderLivenessLeaseWriter(model.ProviderLivenessLeaseRefreshInterval)
	writer.startOnce.Do(func() {})
	writer.observe = func(string, int) {}
	writer.writeShard = livenessLeaseBenchmarkShardWrite
	for i, clientId := range livenessLeaseClientIdsForTest(rng, livenessLeaseBenchmarkConnectionCount) {
		connection := writer.begin(clientId, nil)
		switch {
		case i < livenessLeaseBenchmarkProvidingCount:
			connection.afterRead(map[model.ProvideMode]bool{model.ProvideModePublic: true})
		case i < livenessLeaseBenchmarkConnectionCount-500:
			connection.afterRead(map[model.ProvideMode]bool{})
		}
	}
	writer.write(context.Background(), true)
	return writer, rng
}

func newLivenessLeaseBenchmarkFullWalk(b *testing.B) (*livenessLeaseFullWalkWriterForTest, *mathrand.Rand) {
	// a fixed seed, so a run replays
	rng := mathrand.New(mathrand.NewPCG(1, 0))
	fullWalk := newLivenessLeaseFullWalkWriterForTest(time.Now)
	fullWalk.writeShard = livenessLeaseBenchmarkShardWrite
	for i, clientId := range livenessLeaseClientIdsForTest(rng, livenessLeaseBenchmarkConnectionCount) {
		connection, _ := fullWalk.begin(clientId)
		switch {
		case i < livenessLeaseBenchmarkProvidingCount:
			connection.afterRead(map[model.ProvideMode]bool{model.ProvideModePublic: true})
		case i < livenessLeaseBenchmarkConnectionCount-500:
			connection.afterRead(map[model.ProvideMode]bool{})
		}
	}
	fullWalk.write(context.Background(), true)
	return fullWalk, rng
}

// A session: its connection registers, the woken write writes it, it reads
// providing, the woken write scores it, and it ends.
func BenchmarkLivenessLeaseSessionIndexed(b *testing.B) {
	writer, rng := newLivenessLeaseBenchmarkWriter(b)
	ctx := context.Background()
	clientIds := livenessLeaseClientIdsForTest(rng, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		connection := writer.begin(clientIds[i%len(clientIds)], nil)
		writer.write(ctx, false)
		connection.afterRead(map[model.ProvideMode]bool{model.ProvideModePublic: true})
		writer.write(ctx, false)
		connection.end()
	}
}

func BenchmarkLivenessLeaseSessionFullWalk(b *testing.B) {
	fullWalk, rng := newLivenessLeaseBenchmarkFullWalk(b)
	ctx := context.Background()
	clientIds := livenessLeaseClientIdsForTest(rng, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		connection, _ := fullWalk.begin(clientIds[i%len(clientIds)])
		fullWalk.write(ctx, false)
		connection.afterRead(map[model.ProvideMode]bool{model.ProvideModePublic: true})
		fullWalk.write(ctx, false)
		connection.end()
	}
}

// The full pass each refresh interval.
func BenchmarkLivenessLeaseFullPassIndexed(b *testing.B) {
	writer, _ := newLivenessLeaseBenchmarkWriter(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		writer.write(ctx, true)
	}
}

func BenchmarkLivenessLeaseFullPassFullWalk(b *testing.B) {
	fullWalk, _ := newLivenessLeaseBenchmarkFullWalk(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		fullWalk.write(ctx, true)
	}
}
