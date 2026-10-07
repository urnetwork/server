package model

// Mixed deployment keeps the prior writer's exact script as a test fixture.
// Expiry is an explicit Redis transition, never a wall-clock sleep.

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Exact contractHoleEventScript from server commit
// 57e3de521fe1b43dc479f82cd124b780ba821198. Its writer supplies a 60-second TTL.
const contractHolePriorWriterScript = `
local now = redis.call('TIME')
local nowms = tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000)
redis.call('DEL', KEYS[4])
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', nowms)
if ARGV[1] == 'invalidate' then
    redis.call('DEL', KEYS[1], KEYS[2])
    return 0
end
if ARGV[1] == 'oversized' then
    redis.call('SET', KEYS[5], '1', 'PX', ARGV[3])
    redis.call('DEL', KEYS[1], KEYS[2])
    return 0
end
local ttl = redis.call('PTTL', KEYS[2])
if ttl <= 0 then
    redis.call('DEL', KEYS[1], KEYS[2])
    ttl = tonumber(ARGV[3])
else
    local countttl = redis.call('PTTL', KEYS[1])
    if countttl > 0 then ttl = math.min(ttl, countttl) end
end
local expires = nowms + ttl
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local before = redis.call('ZCARD', KEYS[2])
if before > 0 then
    redis.call('SET', KEYS[1], before, 'KEEPTTL')
else
    redis.call('DEL', KEYS[1])
end
if ARGV[1] == 'create' then
    local deadline = ARGV[5]
    if (deadline == '+inf' or tonumber(deadline) > nowms) and not redis.call('GET', KEYS[5]) and not redis.call('ZSCORE', KEYS[3], ARGV[2]) then
        if before >= tonumber(ARGV[4]) and not redis.call('ZSCORE', KEYS[2], ARGV[2]) then
            redis.call('SET', KEYS[5], '1', 'PX', ARGV[3])
            redis.call('DEL', KEYS[1], KEYS[2])
            return 0
        end
        if redis.call('ZADD', KEYS[2], 'NX', deadline, ARGV[2]) == 1 then
            redis.call('INCR', KEYS[1])
        end
    end
else
    redis.call('ZADD', KEYS[3], nowms + tonumber(ARGV[3]), ARGV[2])
    redis.call('PEXPIRE', KEYS[3], ARGV[3])
    if redis.call('ZREM', KEYS[2], ARGV[2]) == 1 then
        redis.call('DECR', KEYS[1])
    end
end
local count = redis.call('ZCARD', KEYS[2])
if count == 0 then
    redis.call('DEL', KEYS[1], KEYS[2])
else
    redis.call('PEXPIREAT', KEYS[1], expires)
    redis.call('PEXPIREAT', KEYS[2], expires)
end
return count
`

// A prior writer can lose its short-lived projection while the same durable
// contract remains eligible. The final reader refuses without source fallback;
// a final creation restores evidence and an old writer preserves its longer TTL.
func TestContractHoleMixedWriterExpiryLeavesEligibleSourceUnknown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		packetCtx := server.WithoutPostgres(ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		keys := contractHoleKeys(f.sourceId, f.destinationId)
		storedExpiration := func(id server.Id) time.Time {
			var created, expiration time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT create_time,expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&created, &expiration))
			})
			if !expiration.Equal(created.Truncate(time.Millisecond).Add(DefaultContractExpiration)) {
				t.Fatal("contract did not retain its persisted maximum lifespan", created, expiration)
			}
			return expiration
		}
		pairExpiry := func() int64 {
			var expiration int64
			server.Redis(packetCtx, func(client server.RedisClient) {
				for index, key := range keys[:2] {
					got, err := client.Eval(packetCtx, `return redis.call('PEXPIRETIME',KEYS[1])`, []string{key}).Int64()
					server.Raise(err)
					if index == 0 {
						expiration = got
					} else if got != expiration {
						t.Fatal("pair key expirations disagree", expiration, got)
					}
				}
			})
			return expiration
		}
		priorCreate := func(id server.Id, expiration time.Time, want int64) {
			server.Redis(packetCtx, func(client server.RedisClient) {
				got, err := client.Eval(packetCtx, contractHolePriorWriterScript, keys,
					"create", id.String(), time.Minute.Milliseconds(), contractHoleMemberLimit, contractHoleExpirationScore(expiration)).Int64()
				if err != nil || got != want {
					t.Fatal("prior writer result", got, err, "want", want)
				}
				score, err := client.ZScore(packetCtx, keys[1], id.String()).Result()
				if err != nil || score != float64(expiration.UnixMilli()) {
					t.Fatal("prior writer changed the persisted member deadline", score, expiration, err)
				}
			})
		}

		prior, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		priorExpiration := storedExpiration(prior)
		// Retain the real source row but replace its final-writer publication
		// with the exact prior publication on an empty pair.
		server.Redis(packetCtx, func(client server.RedisClient) {
			server.Raise(client.Del(packetCtx, keys...).Err())
		})
		priorCreate(prior, priorExpiration, 1)
		server.Redis(packetCtx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				ttl, err := client.PTTL(packetCtx, key).Result()
				if err != nil || ttl <= 0 || ttl > time.Minute {
					t.Fatal("prior writer did not publish its sixty-second TTL", ttl, err)
				}
			}
		})
		priorPairExpiration := pairExpiry()
		status, until, err := ReadContractHoleLease(packetCtx, f.destinationId, f.sourceId)
		if err != nil || status != ContractHolePositive || !until.Before(priorExpiration) || until.After(time.UnixMilli(priorPairExpiration)) {
			t.Fatal("prior publication did not bound the final reader", status, until, priorExpiration, err)
		}

		server.Redis(packetCtx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				server.Raise(client.PExpireAt(packetCtx, key, time.Unix(1, 0)).Err())
			}
		})
		eligible, err := HasResumableContractForPair(ctx, f.sourceId, f.destinationId)
		if err != nil || !eligible {
			t.Fatal("source contract stopped being eligible at projection expiry", eligible, err)
		}
		status, until, err = ReadContractHoleLease(packetCtx, f.sourceId, f.destinationId)
		if err != nil || status != ContractHoleUnknown || !until.IsZero() {
			t.Fatal("expired prior projection obtained authority", status, until, err)
		}

		current, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		currentExpiration := storedExpiration(current)
		requireContractHoleCount(t, packetCtx, f.sourceId, f.destinationId, 1)
		server.Redis(packetCtx, func(client server.RedisClient) {
			if _, err := client.ZScore(packetCtx, keys[1], prior.String()).Result(); !errors.Is(err, server.RedisNil) {
				t.Fatal("final creation silently rebuilt the prior member", err)
			}
			ttl, err := client.PTTL(packetCtx, keys[0]).Result()
			if err != nil || ttl <= time.Minute || ttl > DefaultContractExpiration {
				t.Fatal("final creation did not supply a longer bounded TTL", ttl, err)
			}
		})
		status, until, err = ReadContractHoleLease(packetCtx, f.sourceId, f.destinationId)
		if err != nil || status != ContractHolePositive || until.After(currentExpiration) {
			t.Fatal("final creation did not restore a strict positive lease", status, until, currentExpiration, err)
		}

		currentPairExpiration := pairExpiry()
		priorCreate(prior, priorExpiration, 2)
		if got := pairExpiry(); got != currentPairExpiration {
			t.Fatal("prior writer shortened the existing long TTL", currentPairExpiration, got)
		}
		requireContractHoleCount(t, packetCtx, f.sourceId, f.destinationId, 2)
		if server.PacketPostgresAttempts(packetCtx) != 0 {
			t.Fatal("mixed-writer packet checks queried PostgreSQL")
		}
	})
}
