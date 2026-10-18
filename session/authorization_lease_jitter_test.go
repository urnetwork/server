package session

import (
	"testing"

	"github.com/urnetwork/server"
)

// The jitter is a stable fraction of the credential and stays inside [0, 1),
// so a retry delay never exceeds one and a half times its interval.
func TestAuthorizationRetryJitterIsFixedPerCredential(t *testing.T) {
	credential := scriptedLeaseCredential()
	if first, second := authorizationRetryJitter(credential), authorizationRetryJitter(credential); first != second || first != 0.5 {
		t.Fatalf("jitter=%v then %v, want a fixed 0.5", first, second)
	}
	for _, tail := range []byte{0x00, 0x01, 0x7f, 0xff} {
		clientId := server.Id{15: tail}
		jitter := authorizationRetryJitter(&ByJwt{ClientId: &clientId})
		if jitter < 0 || 1 <= jitter {
			t.Fatalf("jitter=%v for tail %#x is outside [0, 1)", jitter, tail)
		}
	}
	networkOnly := &ByJwt{NetworkId: server.Id{15: 0x40}}
	if jitter := authorizationRetryJitter(networkOnly); jitter != 0.25 {
		t.Fatalf("network credential jitter=%v, want 0.25", jitter)
	}
}
