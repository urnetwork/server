package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The referral code check runs before an account exists, so its only budget
// owner is the caller's address. This pins that the budget refuses only after
// the address limit is spent and that a second address is unaffected.
func TestReferralCodeValidateRateLimitIsPerAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		checking := session.NewLocalClientSession(ctx, "203.0.113.40:41001", nil)
		defer checking.Cancel()
		for i := 0; i < ReferralCodeValidateAddressLimit; i += 1 {
			if err := CheckReferralCodeValidateRateLimit(checking); err != nil {
				t.Fatalf("check %d within the budget was refused: %v", i+1, err)
			}
		}

		err := CheckReferralCodeValidateRateLimit(checking)
		if err == nil {
			t.Fatalf("check %d over the budget was allowed", ReferralCodeValidateAddressLimit+1)
		}
		retryAfter, ok := err.(interface{ RetryAfterSeconds() int })
		if !ok || retryAfter.RetryAfterSeconds() <= 0 {
			t.Fatalf("over-budget refusal carries no retry hint: %v", err)
		}

		other := session.NewLocalClientSession(ctx, "198.51.100.77:41002", nil)
		defer other.Cancel()
		if err := CheckReferralCodeValidateRateLimit(other); err != nil {
			t.Fatalf("a different address shares the spent budget: %v", err)
		}
	})
}

// A session whose address cannot be resolved is allowed rather than refused:
// refusing would deny the check behind an unparseable proxy chain while
// teaching an attacker nothing.
func TestReferralCodeValidateRateLimitAllowsUnresolvableAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		unresolvable := session.NewLocalClientSession(ctx, "", nil)
		defer unresolvable.Cancel()
		if err := CheckReferralCodeValidateRateLimit(unresolvable); err != nil {
			t.Fatalf("unresolvable address was refused: %v", err)
		}
	})
}
