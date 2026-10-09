// Parent-bound state reads reject incomplete credentials before acquisition.
package jwt

import (
	"testing"

	"github.com/urnetwork/server"
)

// The ownership-aware API cannot accidentally fall back to account-only state.
func TestValidateByJwtStateForParentRefusesIncompleteIdentity(t *testing.T) {
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	account := NewByJwt(server.NewId(), server.NewId(), "synthetic", false, false)
	noDevice := account.Client(server.NewId(), server.NewId())
	noDevice.DeviceId = nil
	noClient := account.Client(server.NewId(), server.NewId())
	noClient.ClientId = nil
	for _, claims := range []*ByJwt{nil, account, noDevice, noClient} {
		if err := ValidateByJwtStateForParent(t.Context(), claims, server.NewId()); err == nil {
			t.Fatal("incomplete parent-bound credential was accepted")
		}
	}
	if attempts() != 0 {
		t.Fatal("incomplete parent-bound credential acquired PostgreSQL")
	}
}
