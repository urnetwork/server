package model

// Readers must accept the maximum lifespan before any writer emits that TTL.

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Exercise both the prior reader's bound and the new Redis/Go lease boundary.
// The larger key TTL cannot extend an individual member's signed deadline.
func TestContractHoleRedisAcceptsMaximumLifespanTtlBeforeWriterRollout(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := server.WithoutPostgres(t.Context())
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		deadline := server.NowUtc().Add(DefaultContractExpiration - time.Minute).Truncate(time.Millisecond)
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create", deadline))
		keys := contractHoleKeys(source, destination)[:2]
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys {
				server.Raise(client.PExpire(ctx, key, DefaultContractExpiration).Err())
			}
			if err := client.Eval(ctx, contractHoleReadScript, keys, contractHoleMemberLimit, time.Minute.Milliseconds()).Err(); err == nil {
				t.Fatal("prior sixty-second reader unexpectedly accepted the new writer TTL")
			}
		})
		status, validUntil, err := ReadContractHoleLease(ctx, source, destination)
		if err != nil || status != ContractHolePositive || !validUntil.Equal(deadline) {
			t.Fatalf("maximum-lifespan read status=%d lease=%s deadline=%s error=%v", status, validUntil, deadline, err)
		}
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("TTL compatibility used PostgreSQL")
		}
	})
}
