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
// production uses, a custom host through §12.8's add and remove. The alias
// case is skipped here, and the run says so.
func TestTlsEdgeStorePassesItsSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		store := NewTlsEdgeStore(nil)
		// the suite's networks are Teams for their custom hosts (R-DNS-3)
		team := map[server.Id]bool{}
		testDnsSetTeam(t, team)
		sidecartest.RunTlsEdgeStoreSuite(t, store, &sidecartest.TlsEdgeStoreSuiteHooks{
			Root: root,
			AddLabel: func(ctx context.Context, networkId sidecar.Id) (string, error) {
				id := server.Id(networkId)
				Testing_CreateNetwork(ctx, id, "tls-"+strings.ReplaceAll(id.String()[24:], "-", ""), server.NewId())
				// drawn alone, as a sidecar's first GET /sidecar/label draws it: the draw takes its place in the
				// map's serial order
				return EnsureNetworkLabel(ctx, id, root)
			},
			AddCustomHost: func(ctx context.Context, networkId sidecar.Id, host string, ownerHost string, active bool) error {
				id := server.Id(networkId)
				team[id] = true
				// the host's domain is the case's own, verified for the network (§7.6)
				_, domain, _ := strings.Cut(host, ".")
				verified := ""
				server.Db(ctx, func(conn server.PgConn) {
					verified = getNetworkDnsCustomHostDomainInTx(ctx, conn, id, host)
				})
				if verified == "" {
					testDnsNetworkDomain(t, ctx, id, domain, true)
				}
				label, err := EnsureNetworkLabel(ctx, id, root)
				if err != nil {
					return err
				}
				add := &sidecar.DnsCustomHostAddRequest{Host: host, Owner: ownerHost + "." + label + "." + root}
				if active {
					// weak mode is active at once (§12.8 step 2); strong mode stays pending for certs' walk
					add.Mode = sidecar.DnsCustomHostModeWeak
					add.WeakDisclosureAccepted = true
				}
				_, err = AddDnsCustomHost(ctx, &DnsCustomHostRequest{Root: root, NetworkId: id, Add: add, ActorKind: AdminEventActorSystem})
				return err
			},
			RemoveCustomHost: func(ctx context.Context, networkId sidecar.Id, host string) error {
				_, err := RemoveDnsCustomHost(ctx, &DnsCustomHostRequest{
					Root:      root,
					NetworkId: server.Id(networkId),
					Remove:    &sidecar.DnsCustomHostRemoveRequest{Host: host},
					ActorKind: AdminEventActorSystem,
				})
				return err
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
