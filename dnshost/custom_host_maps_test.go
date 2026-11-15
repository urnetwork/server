//go:build sidecar_pending

package dnshost

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// sidecartest.RunDnsCustomHostMapStoreSuite on Postgres (D-TEST-2): versions raised by the model over active
// network_dns_custom_host rows, and the applied rows and readiness the store keeps.
func TestCustomHostMapsRunsItsSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		networkId := server.NewId()
		sidecartest.RunDnsCustomHostMapStoreSuite(t, &sidecartest.DnsCustomHostMapStoreSubject{
			Store: NewCustomHostMaps(),
			NewRoot: func(ctx context.Context) (string, error) {
				id := server.NewId().String()
				root := "m" + strings.ReplaceAll(id[len(id)-12:], "-", "") + ".example."
				return root, model.EnsureDnsRoot(ctx, root)
			},
			Raise: func(ctx context.Context, root string, hosts []string) (version uint64, returnErr error) {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_dns_custom_host WHERE root = $1 AND NOT (host = ANY($2))`, root, hosts))
					for _, host := range hosts {
						server.RaisePgResult(tx.Exec(
							ctx,
							`
								INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
								VALUES ($1, $2, $3, $4, $5, $6, true, $7)
								ON CONFLICT (host) DO NOTHING
							`,
							host,
							root,
							networkId,
							"shop.label00001."+root,
							sidecar.DnsCustomHostStateActive,
							sidecar.DnsCustomHostModeWeak,
							server.NowUtc(),
						))
					}
					version, returnErr = model.DnsRaiseCustomHostMapInTx(ctx, tx, root)
				}, server.TxReadCommitted)
				return
			},
			HostReady: func(ctx context.Context, root string, host string) (ready bool, returnErr error) {
				server.Db(ctx, func(conn server.PgConn) {
					returnErr = conn.QueryRow(ctx, `SELECT ready_time IS NOT NULL FROM network_dns_custom_host WHERE host = $1`, host).Scan(&ready)
				})
				return
			},
		})
	})
}
