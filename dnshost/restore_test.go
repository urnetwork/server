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

// sidecartest.RunDnsRestoreStoreSuite on Postgres (D-TEST-2): labels through dns_label, open cases
// through abuse_case, admin events through network_admin_event, and no NS state kept until dns_ns
// exists.
func TestRestoreStoreRunsItsSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		sidecartest.RunDnsRestoreStoreSuite(t, &sidecartest.DnsRestoreStoreSubject{
			Store: &RestoreStore{},
			NewRoot: func(ctx context.Context) (string, error) {
				id := server.NewId().String()
				root := "r" + strings.ReplaceAll(id[len(id)-12:], "-", "") + ".example."
				return root, model.EnsureDnsRoot(ctx, root)
			},
			NewNetwork: func(ctx context.Context, root string) (string, sidecar.Id, error) {
				networkId := server.NewId()
				model.Testing_CreateNetwork(ctx, networkId, "restore-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
				label, err := model.EnsureNetworkLabel(ctx, networkId, root)
				return label, sidecar.Id(networkId), err
			},
			OpenCase: func(ctx context.Context, networkId sidecar.Id) error {
				id := server.Id(networkId)
				var err error
				server.Tx(ctx, func(tx server.PgTx) {
					_, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
						NetworkId:     &id,
						Category:      model.AbuseCategoryPhishing,
						Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: "www.restore.example"},
						ReporterClass: model.AbuseReporterClassWebRisk,
						JoinKind:      model.AbuseJoinKindHost,
						JoinKey:       "www.restore.example",
					}, server.NowUtc(), model.DefaultAbuseSettings())
				})
				return err
			},
			AdminEventCount: func(ctx context.Context, networkId sidecar.Id, incidentId sidecar.Id) (int, error) {
				count := 0
				for _, event := range model.GetNetworkAdminEvents(ctx, server.Id(networkId), 0, 0) {
					if event.Action == model.AdminEventActionDnsRestore && event.TargetId != nil && *event.TargetId == server.Id(incidentId) {
						count += 1
					}
				}
				return count, nil
			},
		})
	})
}
