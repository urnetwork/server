package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

// A legacy guest network has no login method. RefreshToken signs every jwt
// with GuestMode=false, so after one refresh the claim no longer marks the
// guest, and a client gating purchases on the claim would sell a plan to a
// network nothing can sign back in to. The balance reports the guest from the
// live auth methods instead.
func TestSubscriptionBalanceGuestIgnoresTheJwtClaim(t *testing.T) {
	ctx := context.Background()
	userId := server.NewId()
	fakeAuthMethods := func(has bool) func(context.Context, server.Id) bool {
		return func(_ context.Context, id server.Id) bool {
			if id != userId {
				t.Fatalf("auth methods looked up for %s, want the session user %s", id, userId)
			}
			return has
		}
	}
	sessionWithClaim := func(guestMode bool) *session.ClientSession {
		return session.NewLocalClientSession(ctx, "127.0.0.1:1", &jwt.ByJwt{
			NetworkId: server.NewId(),
			UserId:    userId,
			GuestMode: guestMode,
		})
	}

	// a refreshed legacy guest: claim cleared, still no login method
	if !isGuestNetwork(sessionWithClaim(false), fakeAuthMethods(false)) {
		t.Fatalf("refreshed legacy guest (GuestMode=false, no login method) not reported as guest")
	}
	// a guest that just added a login method, jwt not refreshed yet
	if isGuestNetwork(sessionWithClaim(true), fakeAuthMethods(true)) {
		t.Fatalf("network with a login method reported as guest because of a stale GuestMode claim")
	}
	if isGuestNetwork(sessionWithClaim(false), fakeAuthMethods(true)) {
		t.Fatalf("network with a login method reported as guest")
	}
}

// The field is additive: always on the wire as "guest", so a client can read
// false as "has a login method" and older clients ignore it.
func TestSubscriptionBalanceResultGuestWireName(t *testing.T) {
	for _, guest := range []bool{true, false} {
		b, err := json.Marshal(&SubscriptionBalanceResult{Guest: guest})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if v, ok := m["guest"]; !ok || v != guest {
			t.Fatalf("guest=%v marshals as %v (present=%v): %s", guest, v, ok, b)
		}
	}
}
