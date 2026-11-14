//go:build sidecar_pending

package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
)

// The Postgres store keeps what the engine's store suite asks of every
// TlsEdgeStore, the fake's included, each hook writing through the model
// production uses. No model writes an alias yet (§12.5, M6), so the alias
// case is skipped, and the run says so.
func TestTlsEdgeStorePassesItsSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		store := NewTlsEdgeStore(nil)
		sidecartest.RunTlsEdgeStoreSuite(t, store, &sidecartest.TlsEdgeStoreSuiteHooks{
			Root: root,
			AddLabel: func(ctx context.Context, networkId sidecar.Id) (string, error) {
				id := server.Id(networkId)
				Testing_CreateNetwork(ctx, id, "tls-"+strings.ReplaceAll(id.String()[24:], "-", ""), server.NewId())
				// a label takes its place in the map's serial order when its zone is made, as publishing makes it
				if _, err := EnsureNetworkZone(ctx, id, root); err != nil {
					return "", err
				}
				return EnsureNetworkLabel(ctx, id, root)
			},
			WithdrawNetwork: func(ctx context.Context, networkId sidecar.Id) (returnErr error) {
				server.Tx(ctx, func(tx server.PgTx) {
					returnErr = DnsWithdrawNetworkInTx(ctx, tx, server.Id(networkId))
				})
				return
			},
			SuspendZone: func(ctx context.Context, networkId sidecar.Id, until time.Time) error {
				if until.IsZero() {
					return SuspendZone(ctx, server.Id(networkId), "suite", nil)
				}
				return SuspendZone(ctx, server.Id(networkId), "suite", &until)
			},
			SuspendHost: func(ctx context.Context, networkId sidecar.Id, host string) error {
				return SuspendHost(ctx, server.Id(networkId), host)
			},
			Restore: func(ctx context.Context, networkId sidecar.Id, host string) error {
				return Restore(ctx, server.Id(networkId), host)
			},
			SetSuspension: func(ctx context.Context, row *sidecar.TlsSuspendArgs) error {
				return SetTlsSuspension(ctx, row)
			},
			LiftSuspension: func(ctx context.Context, networkId sidecar.Id, host string) error {
				return LiftTlsSuspension(ctx, server.Id(networkId), host)
			},
			ReadRecords: func(ctx context.Context, networkId sidecar.Id, since time.Time) ([]*tlsedgeservice.TlsConnectionRecord, error) {
				return Testing_ReadTlsConnectionRecords(ctx, networkId, since), nil
			},
		})
	})
}
