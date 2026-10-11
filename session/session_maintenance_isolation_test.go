// One failing index member must not hide later due networks in its shard.
package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// A malformed member scores first. The later due network, which has no live
// session, must still be reviewed and retired while the failure stays due.
func TestSessionIndexSweepContinuesPastFailedMember(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		network[15] = 5
		shard := int(network[15]) % SessionIndexShards
		if err := publishSessionIndex(ctx, network, now.Add(-time.Minute), server.NewId().String()); err != nil {
			t.Fatal(err)
		}
		keys := sessionIndexKeys(network)
		const malformed = "synthetic-malformed-member"
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.ZAdd(ctx, keys[0], redis.Z{Score: 0, Member: malformed}).Err()
		}))
		reviewed, err := SweepSessionIndexShard(ctx, shard, 32, now)
		if reviewed != 1 || err == nil || !strings.Contains(err.Error(), malformed) {
			t.Fatal("one failed member stopped its shard or hid its own failure", reviewed, err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			if _, err := r.ZScore(ctx, keys[0], network.String()).Result(); err != server.RedisNil {
				t.Fatal("the later due network was not reviewed", err)
			}
			if _, err := r.ZScore(ctx, keys[0], malformed).Result(); err != nil {
				t.Fatal("the failed member left the due set without review", err)
			}
			return nil
		}))
	})
}
