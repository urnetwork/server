// Malformed client projections retain the prior transaction's independent effects.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Runtime errors remain visible while every submitted client operation runs.
// In particular a bad dirty version cannot suppress a valid hop removal, and
// a bad hop key cannot strand a newly created dirty version without its TTL.
func TestStreamRemovalBatchMalformedHopRetainsTransactionEffects(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		for _, failure := range []string{"version_type", "hop_type"} {
			bad, good := server.NewId(), server.NewId()
			badSource, badDestination, goodSource := server.NewId(), server.NewId(), server.NewId()
			AddToStream(ctx, bad, badSource, badDestination, nil)
			AddToStream(ctx, good, goodSource, server.NewId(), nil)
			before := GetStreamEventId(ctx, goodSource)
			server.Redis(ctx, func(r server.RedisClient) {
				switch failure {
				case "version_type":
					server.Raise(r.Set(ctx, clientEventIdKey(badSource), "synthetic", 0).Err())
				case "hop_type":
					server.Raise(r.Del(ctx, clientEventIdKey(badSource), clientStreamHopsKey(badSource)).Err())
					server.Raise(r.Set(ctx, clientStreamHopsKey(badSource), "synthetic", 0).Err())
				}
			})
			var caught error
			server.HandleError(func() { removeFromStreams(ctx, []server.Id{bad, good}) }, func(err error) { caught = err })
			if caught == nil {
				t.Error("malformed client projection became successful cleanup", failure)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				ttl, err := r.PTTL(ctx, clientEventIdKey(badSource)).Result()
				server.Raise(err)
				if ttl <= 0 || ttl > clientEventIdTtl {
					t.Error("client command failure skipped the dirty version's finite expiry", failure, ttl)
				}
				if failure == "version_type" {
					count, err := r.SCard(ctx, clientStreamHopsKey(badSource)).Result()
					server.Raise(err)
					if count != 0 {
						t.Error("malformed dirty version suppressed a valid hop removal", count)
					}
				} else {
					version, err := r.Get(ctx, clientEventIdKey(badSource)).Result()
					server.Raise(err)
					if version != "1" {
						t.Error("malformed hop changed the submitted dirty increment", version)
					}
				}
			})
			for _, id := range []server.Id{bad, good} {
				if _, _, ok := GetStream(ctx, id); ok {
					t.Error("client projection error suppressed contract lookup cleanup", failure, id)
				}
			}
			_, oppositeHops := GetStreamHops(ctx, badDestination)
			_, healthyHops := GetStreamHops(ctx, goodSource)
			if len(oppositeHops) != 0 || len(healthyHops) != 0 || GetStreamEventId(ctx, goodSource) != before+1 {
				t.Error("malformed client suppressed healthy clients or neighboring stream", failure)
			}
			if _, ok := RemoveFromStream(ctx, good); ok || GetStreamEventId(ctx, goodSource) != before+1 {
				t.Error("healthy replay changed after a client projection error", failure)
			}
		}
	})
}
