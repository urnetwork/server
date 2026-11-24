// A hosted device of a network on the free plan reserves fewer first-window
// registrations than a Pro one (owner, 2026-11-24): three, its whole first
// window of two quality exits and one speed exit, where a Pro device reserves
// six. The plan is the parent credential's pro claim, read when the authority
// is built. Every identity is synthetic and nothing reaches a database: a
// registration that gets past admission fails its local preflight with a 401.
package localclient

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// An authority built as NewWithSettings builds one once it has validated a
// parent credential with the given pro claim, on the owner's bound.
func freePlanTestAuthority(t *testing.T, admission *MintAdmission, pro bool) (*Authority, *connect.AuthNetworkClientArgs) {
	t.Helper()
	claims := session.NewByJwt(server.NewId(), server.NewId(), "free-plan-test", false, pro).Client(server.NewId(), server.NewId())
	authority := newAuthority(t.Context(), claims, "", "https://control.example", &AuthoritySettings{MintAdmission: admission})
	t.Cleanup(authority.Close)
	parent := connect.Id(*claims.ClientId)
	return authority, &connect.AuthNetworkClientArgs{SourceClientId: &parent}
}

// With the shared slots full, a free device that cannot route takes three
// reserved registrations at once and then waits for a shared slot like any
// other; a Pro device takes six, as before.
func TestFreeDeviceReservesItsWholeFirstWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		admission := NewMintAdmission(2)
		first := holdSharedMintSlot(t, admission)
		second := holdSharedMintSlot(t, admission)
		for _, c := range []struct {
			pro      bool
			reserved int
		}{
			{pro: false, reserved: 3},
			{pro: true, reserved: 6},
		} {
			cold, args := freePlanTestAuthority(t, admission, c.pro)
			mintAdmissionNoRoute(cold)
			start := time.Now()
			for i := range c.reserved {
				if _, err := cold.AuthNetworkClient(t.Context(), args); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
					t.Fatalf("pro=%t: reserved registration %d answered %v, want its preflight 401", c.pro, i+1, err)
				}
			}
			if waited := time.Since(start); waited != 0 {
				t.Fatalf("pro=%t: the reserved registrations waited %s", c.pro, waited)
			}
			_, err := cold.AuthNetworkClient(t.Context(), args)
			if mintAdmissionStatus(t, err) != http.StatusTooManyRequests || time.Since(start) != time.Second {
				t.Fatalf("pro=%t: registration %d answered %v after %s, want 429 after its wait", c.pro, c.reserved+1, err, time.Since(start))
			}
		}
		admission.release(first)
		admission.release(second)
		assertMintAdmissionIdle(t, admission)
	})
}

// The authority reports its credential's pro claim, which a hosted device
// reads its plan from.
func TestAuthorityReadsThePlanFromItsCredential(t *testing.T) {
	for _, pro := range []bool{false, true} {
		authority, _ := freePlanTestAuthority(t, nil, pro)
		if authority.Pro() != pro {
			t.Errorf("an authority on a credential with pro=%t reports %t", pro, authority.Pro())
		}
	}
}
