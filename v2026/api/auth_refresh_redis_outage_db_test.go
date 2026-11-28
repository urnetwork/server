package api

// The no-extend fallback end to end, against the database: a tracked network
// token renews at POST /auth/network-refresh while every Redis node's dials
// time out. The renewal carries the token's session, which the mint cannot
// register. Before, the route answered 503; with the fallback it answers 200
// with a token on the same session, expiring with the presented token.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"

	"github.com/urnetwork/server/v2026/session"
)

func TestNetworkRefreshKeepsItsSessionWhileRedisDialsTimeOut(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		// a tracked sign-in: the renewal registers the root token's session
		tracked := self.networkRefresh(t, self.rootToken)
		if tracked.SessionId == nil {
			t.Fatal("the renewed network token carries no session")
		}

		_, pop := server.Testing_StallAuthRedisDials(48)
		w := self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+tracked.Testing_Sign(), nil)
		pop()
		if w.Code != http.StatusOK {
			t.Fatalf("network refresh while Redis dials time out = %d: %s, want 200", w.Code, w.Body.String())
		}
		var result controller.NetworkRefreshTokenResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Error != nil || result.ByJwt == "" {
			t.Fatalf("network refresh = %s: %v", w.Body.String(), err)
		}
		renewed, err := session.ParseByJwt(ctx, result.ByJwt)
		if err != nil {
			t.Fatal(err)
		}
		if renewed.SessionId == nil || *renewed.SessionId != *tracked.SessionId {
			t.Fatalf("the renewal carries session %v, want %s", renewed.SessionId, tracked.SessionId)
		}
		if renewed.ExpiresAt == nil || !renewed.ExpiresAt.Time.Equal(tracked.ExpiresAt.Time) {
			t.Fatalf("the renewal expires %v, want the presented token's %v", renewed.ExpiresAt, tracked.ExpiresAt)
		}
		if renewed.CreateTime != tracked.CreateTime {
			t.Fatalf("the renewal's create time %v is not the presented token's %v", renewed.CreateTime, tracked.CreateTime)
		}
	})
}
