package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// E2's widget email is judged by the network's newest active client that has
// a device: its spec and the app version it last connected with. A removed
// client, or one without a device, does not count.
func TestGetNetworkNewestClientBuild(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-build-%s", networkId), server.NewId())
		if build := GetNetworkNewestClientBuild(ctx, networkId); build != nil {
			t.Fatalf("a network without clients has build %+v", build)
		}
		base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
		created := func(clientId server.Id, after time.Duration, active bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET create_time = $2, active = $3 WHERE client_id = $1`,
					clientId,
					base.Add(after),
					active,
				))
			})
		}
		want := func(deviceSpec string, clientVersion string) {
			build := GetNetworkNewestClientBuild(ctx, networkId)
			if build == nil || build.DeviceSpec != deviceSpec || build.ClientVersion != clientVersion {
				t.Fatalf("newest build = %+v, want %q %q", build, deviceSpec, clientVersion)
			}
		}

		phoneClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), phoneClientId, "synthetic phone", "17.1 Pixel 8 Pro")
		created(phoneClientId, 0, true)
		want("17.1 Pixel 8 Pro", "")
		SetDeviceClientVersion(ctx, phoneClientId, "2026.10.15-1072765730")
		want("17.1 Pixel 8 Pro", "2026.10.15-1072765730")

		// a newer client that was removed does not count
		removedClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), removedClientId, "synthetic laptop", "Windows 11")
		created(removedClientId, time.Hour, false)
		want("17.1 Pixel 8 Pro", "2026.10.15-1072765730")

		// nor does a newer one without a device
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client (client_id, network_id, description, create_time, auth_time) VALUES ($1, $2, $3, $4, $4)`,
				server.NewId(),
				networkId,
				"synthetic automation",
				base.Add(2*time.Hour),
			))
		})
		want("17.1 Pixel 8 Pro", "2026.10.15-1072765730")

		// the newest active client with a device wins, version or not
		macClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), macClientId, "synthetic mac", "15.7.9 MacBook Air (M1, 2020)")
		created(macClientId, 3*time.Hour, true)
		want("15.7.9 MacBook Air (M1, 2020)", "")
	})
}
