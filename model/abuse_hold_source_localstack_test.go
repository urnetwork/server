//go:build sidecar_localstack

package model_test

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The taskworker's reader resolves its repository's placement and group owners on
// private MinIO, then reads the matching lease and acknowledgement hashes from Redis.
func TestLegalHoldSourceReadsScopedSoftStateOnT3(t *testing.T) {
	env := &server.TestEnv{ApplyDbMigrations: false}
	env.Run(t, func(test testing.TB) {
		net := sidecart3.NewNet(test, nil)
		defer net.Close()
		checkHoldPlacementSoftState(test, net.Host(), net.Clock().Now())
	})
}
