//go:build sidecar_leaktest

// CC3's Postgres leak (§22.7's self-test; TEST-M2-09): a value put in a sidecar table's column, as a server bug storing
// what it must not would put it, which CaptureStores must catch. It links only under sidecar_leaktest, which check/
// keeps off every release leg, beside sidecartest's leaks for the other classes; a T3 Net with server's migrations
// applied runs it (capture_stores_test.go).

package sidecart3

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The value as an identity's public key in sidecar_identity (MG1), its bytea holding any byte.
var _ = sidecartest.RegisterLeak(&sidecartest.Leak{Class: sidecartest.PrivacyClassPostgres, Binary: true, Put: func(t testing.TB, net *sidecartest.Net, value []byte) {
	server.Tx(context.Background(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			context.Background(),
			`INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time) VALUES ($1, $2, $3, 'ed25519', 'active', now())`,
			server.NewId(), server.NewId(), value,
		))
	})
}})
