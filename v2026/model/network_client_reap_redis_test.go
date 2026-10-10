package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

func TestReapedClientRedisCleanupUsesBoundedChunks(t *testing.T) {
	clientIDs := make([]server.Id, 2*removeRedisCleanupChunkCount+17)
	for i := range clientIDs {
		clientIDs[i] = server.NewId()
	}

	var chunkSizes []int
	var visited []server.Id
	forEachReapedClientRedisChunk(clientIDs, func(chunk []server.Id) {
		chunkSizes = append(chunkSizes, len(chunk))
		visited = append(visited, chunk...)
	})

	wantChunkSizes := []int{removeRedisCleanupChunkCount, removeRedisCleanupChunkCount, 17}
	if len(chunkSizes) != len(wantChunkSizes) {
		t.Fatalf("cleanup chunks = %v, want %v", chunkSizes, wantChunkSizes)
	}
	for i := range wantChunkSizes {
		if chunkSizes[i] != wantChunkSizes[i] {
			t.Fatalf("cleanup chunks = %v, want %v", chunkSizes, wantChunkSizes)
		}
	}
	if len(visited) != len(clientIDs) {
		t.Fatalf("visited %d clients, want %d", len(visited), len(clientIDs))
	}
	for i := range clientIDs {
		if visited[i] != clientIDs[i] {
			t.Fatalf("visited client %d = %s, want %s", i, visited[i], clientIDs[i])
		}
	}
}

func TestReapedClientRedisCleanupPreservesReassignedEgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		targetA, targetB, survivor := server.NewId(), server.NewId(), server.NewId()
		targets := []server.Id{targetA, targetB}
		hashA, hashB := strings.Repeat("a", 64), strings.Repeat("b", 64)

		server.Redis(ctx, func(r server.RedisClient) {
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, clientPublicKeyRedisKey(targetA), []byte("target-a"), 0)
				pipe.Set(ctx, clientPublicKeyRedisKey(targetB), []byte("target-b"), 0)
				pipe.Set(ctx, clientPublicKeyRedisKey(survivor), []byte("survivor"), 0)
				pipe.HSet(ctx, verifyClientEgressKey(targetA), hashA, "1")
				pipe.HSet(ctx, verifyClientEgressKey(targetB), hashB, "1")
				pipe.Set(ctx, verifyEgressKeyFromHex(hashA), targetA.String(), 0)
				// targetB's stale reverse entry now points at an address that was
				// reassigned; compare-delete must preserve the new owner.
				pipe.Set(ctx, verifyEgressKeyFromHex(hashB), survivor.String(), 0)
				pipe.SAdd(ctx, verifyEligibleKey, targetA.String(), targetB.String(), survivor.String())
				return nil
			})
			server.Raise(err)
		})

		removeReapedClientRedisState(ctx, targets)

		server.Redis(ctx, func(r server.RedisClient) {
			for _, target := range targets {
				if count, err := r.Exists(ctx, clientPublicKeyRedisKey(target), verifyClientEgressKey(target)).Result(); err != nil || count != 0 {
					t.Fatalf("target %s retained %d public/reverse keys: %v", target, count, err)
				}
				if member, err := r.SIsMember(ctx, verifyEligibleKey, target.String()).Result(); err != nil || member {
					t.Fatalf("target %s eligible = %v, err=%v; want false", target, member, err)
				}
			}

			if _, err := r.Get(ctx, verifyEgressKeyFromHex(hashA)).Result(); !errors.Is(err, redis.Nil) {
				t.Fatalf("target-owned forward entry survived: %v", err)
			}
			owner, err := r.Get(ctx, verifyEgressKeyFromHex(hashB)).Result()
			if err != nil || owner != survivor.String() {
				t.Fatalf("reassigned forward owner = %q, err=%v; want %s", owner, err, survivor)
			}
			publicKey, err := r.Get(ctx, clientPublicKeyRedisKey(survivor)).Result()
			if err != nil || publicKey != "survivor" {
				t.Fatalf("survivor public key = %q, err=%v", publicKey, err)
			}
			member, err := r.SIsMember(ctx, verifyEligibleKey, survivor.String()).Result()
			if err != nil || !member {
				t.Fatalf("survivor eligible = %v, err=%v; want true", member, err)
			}

			_, err = r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Del(ctx, clientPublicKeyRedisKey(survivor), verifyEgressKeyFromHex(hashB))
				pipe.SRem(ctx, verifyEligibleKey, survivor.String())
				return nil
			})
			server.Raise(err)
		})
	})
}
