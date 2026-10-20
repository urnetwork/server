package model

import (
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"sort"
	"testing"
	"time"
)

func TestSessionStreamGrantFanoutProfile(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network, user := server.NewId(), server.NewId()
		clients := []server.Id{server.NewId(), server.NewId()}
		for _, id := range clients {
			claims := session.NewByJwt(network, user, "fanout", false, false).Client(server.NewId(), id)
			sid := server.NewId()
			claims.SessionId = &sid
			if err := session.PublishConnectionAuthority(ctx, claims, server.NewId(), 1, now.Add(90*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		streams := make([]server.Id, 1000)
		for i := range streams {
			streams[i] = server.NewId()
			if err := storeStreamParticipants(ctx, streams[i], newStreamKey(clients[0], clients[1], nil)); err != nil {
				t.Fatal(err)
			}
			if grant, err := AuthorizeStream(ctx, streams[i], now); err != nil || !grant.Allowed {
				t.Fatal("stream not admitted", err)
			}
		}
		durations := make([]time.Duration, len(streams))
		start := time.Now()
		for i, id := range streams {
			before := time.Now()
			grant, err := AuthorizeStream(ctx, id, now.Add(time.Second))
			durations[i] = time.Since(before)
			if err != nil || !grant.Allowed {
				t.Fatal("renewal fanout failed", err)
			}
		}
		elapsed := time.Since(start)
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		t.Logf("stream lease fanout: grants=1000 sequential renewal pass=%s p50=%s p99=%s", elapsed, durations[500], durations[990])
	})
}
