package sidecart3

// sidecart3's test binaries sign a T3 world's owner credentials with session's fixture signer, which only a _test.go
// file may call (session's TestProductionSigningCannotUseFixtureBypass), so world_networks.go names no fixture signer.

import (
	"testing"

	"github.com/urnetwork/server/session"
)

func init() {
	Testing_SetT3WorldOwnerSigner(func(credential *session.ByJwt) string {
		return credential.Testing_Sign()
	})
}

// Without an installed signer a world makes no network: AddNetwork refuses before it writes a row or signs anything,
// so a binary whose _test.go files install none fails closed rather than handing an owner an unsigned credential.
// The pop puts this binary's signer back.
func TestT3WorldAddNetworkRefusesWithoutAnOwnerSigner(t *testing.T) {
	pop := Testing_SetT3WorldOwnerSigner(nil)
	defer pop()
	network, err := (&T3World{}).AddNetwork(t.Context(), "unsigned")
	if err == nil || network != nil {
		t.Fatalf("AddNetwork without an owner signer: %v, %v", network, err)
	}
	pop()
	if t3WorldOwnerSigner.Load() == nil {
		t.Fatal("the pop left no signer installed")
	}
}
