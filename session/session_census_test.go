package session

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// LiveNetworkSessions lists the live tracked sessions in id order without the
// inventory script's writes: an ended member is not pruned, no revision is
// initialized and no expiration moves (bringyourctl network sign-out-all reads
// it in its dry run).
func TestLiveNetworkSessionsReadsWithoutWriting(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		now := time.UnixMilli(server.NowUtc().UnixMilli()).UTC()
		first, second, ended := server.NewId(), server.NewId(), server.NewId()
		if second.Less(first) {
			first, second = second, first
		}
		key := SessionKey(networkId, "z")
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.ZAdd(
				ctx,
				key,
				redis.Z{Score: float64(now.Add(2 * time.Hour).UnixMilli()), Member: second.String()},
				redis.Z{Score: float64(now.Add(time.Hour).UnixMilli()), Member: first.String()},
				redis.Z{Score: float64(now.UnixMilli()), Member: ended.String()},
			).Err())
		})

		sessions, err := LiveNetworkSessions(ctx, networkId, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 2 ||
			sessions[0].SessionId != first || !sessions[0].AcceptUntil.Equal(now.Add(time.Hour)) ||
			sessions[1].SessionId != second || !sessions[1].AcceptUntil.Equal(now.Add(2*time.Hour)) {
			t.Fatalf("live sessions %+v", sessions)
		}

		server.Redis(ctx, func(r server.RedisClient) {
			if count := r.ZCard(ctx, key).Val(); count != 3 {
				t.Fatal("the read pruned the inventory", count)
			}
			if exists := r.Exists(ctx, SessionKey(networkId, "generation"), SessionKey(networkId, "eid")).Val(); exists != 0 {
				t.Fatal("the read initialized the revision", exists)
			}
			if ttl := r.PTTL(ctx, key).Val(); ttl != -1 {
				t.Fatal("the read set an expiration", ttl)
			}
		})

		empty, err := LiveNetworkSessions(ctx, server.NewId(), now)
		if err != nil || len(empty) != 0 {
			t.Fatal("a network without sessions", empty, err)
		}
	})
}
