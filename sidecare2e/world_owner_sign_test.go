//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

package sidecare2e

// The e2e world's T3 world signs each network's owner credential with session's fixture signer, which only a _test.go
// file may call (session's TestProductionSigningCannotUseFixtureBypass), so sidecart3's world_networks.go names none.

import (
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecart3"
)

func init() {
	sidecart3.Testing_SetT3WorldOwnerSigner(func(credential *session.ByJwt) string {
		return credential.Testing_Sign()
	})
}
