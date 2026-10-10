package model

// A retained old refresh can shorten a newer key only when its snapshot starts
// after that creation. The final deployment retires this source owner entirely.

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The begin/publish scripts are byte-identical in prior commit 57e3de521 and
// final ec8a66b08; the prior owner supplies a 60-second publication TTL.
func TestContractHolePriorRefreshOrderingCanShortenNewLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		packetCtx := server.WithoutPostgres(ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		keys := contractHoleKeys(f.sourceId, f.destinationId)
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
		beginPrior := func(token string) int64 {
			var expiration int64
			server.Redis(packetCtx, func(client server.RedisClient) {
				server.Raise(client.Eval(packetCtx, contractHoleBeginRefreshScript, keys, token, contractHoleSourceTimeout.Milliseconds()).Err())
				var err error
				expiration, err = client.Eval(packetCtx, `return redis.call('PEXPIRETIME',KEYS[1])`, keys[3:4]).Int64()
				server.Raise(err)
			})
			return expiration
		}
		publishPrior := func(token string, members []contractHoleMember, wantPublished, wantCount int64) {
			args := []any{token, time.Minute.Milliseconds()}
			for _, member := range members {
				args = append(args, member.ContractId.String(), contractHoleExpirationScore(*member.ExpirationTime))
			}
			server.Redis(packetCtx, func(client server.RedisClient) {
				got, err := client.Eval(packetCtx, contractHolePublishScript, keys, args...).Slice()
				if err != nil || len(got) != 2 || got[0] != wantPublished || got[1] != wantCount {
					t.Fatal("prior snapshot publication result", got, err, "want", wantPublished, wantCount)
				}
			})
		}

		first, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		firstExpiration, err := GetContractExpirationTime(ctx, first)
		if err != nil || firstExpiration == nil {
			t.Fatal("first persisted deadline missing", firstExpiration, err)
		}
		earlierToken := server.NewId().String()
		earlierTokenExpiration := beginPrior(earlierToken)
		second, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		server.Redis(packetCtx, func(client server.RedisClient) {
			// Observe absence and the still-live token window atomically so
			// token expiry cannot stand in for the lifecycle fence.
			observed, err := client.Eval(packetCtx, `
local now = redis.call('TIME')
if tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000) >= tonumber(ARGV[1]) then return -1 end
return redis.call('EXISTS', KEYS[1])`, keys[3:4], earlierTokenExpiration).Int64()
			server.Raise(err)
			if observed == -1 {
				t.Fatal("fixture exceeded the prior token window before the fence assertion")
			}
			if observed != 0 {
				t.Fatal("committed creation did not fence the earlier snapshot", observed)
			}
		})
		secondExpiration, err := GetContractExpirationTime(ctx, second)
		if err != nil || secondExpiration == nil {
			t.Fatal("second persisted deadline missing", secondExpiration, err)
		}
		longExpiration := pairExpiry()
		firstMember := contractHoleMember{ContractId: first, ExpirationTime: firstExpiration}
		secondMember := contractHoleMember{ContractId: second, ExpirationTime: secondExpiration}
		publishPrior(earlierToken, []contractHoleMember{firstMember}, 0, 0)
		if got := pairExpiry(); got != longExpiration {
			t.Fatal("fenced earlier snapshot changed the new lease", longExpiration, got)
		}
		requireContractHoleCount(t, packetCtx, f.sourceId, f.destinationId, 2)

		laterToken := server.NewId().String()
		beginPrior(laterToken)
		publishPrior(laterToken, []contractHoleMember{firstMember, secondMember}, 1, 2)
		shortExpiration := pairExpiry()
		if shortExpiration <= 0 || shortExpiration >= longExpiration {
			t.Fatal("later prior snapshot did not shorten the new lease", longExpiration, shortExpiration)
		}
		server.Redis(packetCtx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				ttl, err := client.PTTL(packetCtx, key).Result()
				if err != nil || ttl <= 0 || ttl > time.Minute {
					t.Fatal("later prior snapshot did not publish its sixty-second TTL", ttl, err)
				}
			}
		})
		requireContractHoleCount(t, packetCtx, f.destinationId, f.sourceId, 2)
		server.Redis(packetCtx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				server.Raise(client.PExpireAt(packetCtx, key, time.Unix(1, 0)).Err())
			}
		})
		eligible, err := HasResumableContractForPair(ctx, f.sourceId, f.destinationId)
		if err != nil || !eligible {
			t.Fatal("source stopped being eligible at projection expiry", eligible, err)
		}
		status, until, err := ReadContractHoleLease(packetCtx, f.sourceId, f.destinationId)
		if err != nil || status != ContractHoleUnknown || !until.IsZero() {
			t.Fatal("expired prior snapshot obtained packet authority", status, until, err)
		}
		if server.PacketPostgresAttempts(packetCtx) != 0 {
			t.Fatal("prior snapshot packet checks queried PostgreSQL")
		}
	})
}
