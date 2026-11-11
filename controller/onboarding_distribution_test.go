// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// P6 (mmm/ONBOARDING-RETENTION.md): the App Store and direct-download Mac
// builds send the same device type, spec and version, so the campaign gave
// every Mac app the App Store's rules. A sign-up and a device authenticating
// now record the distribution the app declares with its platform
// (X-UR-ClientInfo) in the network's onboarding row, where the rules read it.
func TestOnboardingRecordsTheAppsDistribution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		appSession := func(networkId server.Id, clientInfo string) *session.ClientSession {
			clientSession := session.NewLocalClientSession(ctx, "", &session.ByJwt{NetworkId: networkId, UserId: server.NewId()})
			clientSession.ClientInfo = session.ParseClientInfo(clientInfo, "")
			return clientSession
		}
		const (
			macDirect = `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","distribution":"mac_direct"}`
			macSpec   = "15.7.9 MacBook Air (M1, 2020)"
		)

		// sign-up on the account path: an email login from the direct DMG
		signUpNetworkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
				signUpNetworkId,
				"synthetic-distribution-signup-"+signUpNetworkId.String(),
				server.NewId(),
			))
		})
		signUp := appSession(signUpNetworkId, macDirect)
		defer signUp.Cancel()
		EnrollNetworkOnboarding(signUp, signUpNetworkId, "synthetic-"+signUpNetworkId.String()+"@example.com", false)
		row := model.GetNetworkOnboarding(ctx, signUpNetworkId)
		if row == nil {
			t.Fatal("the sign-up did not enter the campaign")
		}
		if row.Platform != "macos" || row.Distribution != session.ClientDistributionMacDirect {
			t.Fatalf("sign-up recorded %q/%q, want macos/mac_direct", row.Platform, row.Distribution)
		}

		// a device authenticating: a row without a platform takes the app's
		// platform and distribution
		deviceNetworkId := server.NewId()
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: deviceNetworkId, CreatedAt: now, Email: true}) {
			t.Fatal("could not create the device fixture")
		}
		device := appSession(deviceNetworkId, macDirect)
		defer device.Cancel()
		RecordOnboardingClientContext(device, deviceNetworkId, "Etc/UTC", "en-US", macSpec)
		row = model.GetNetworkOnboarding(ctx, deviceNetworkId)
		if row == nil || row.Platform != "macos" || row.Distribution != session.ClientDistributionMacDirect {
			t.Fatalf("auth-client recorded %+v, want macos/mac_direct", row)
		}

		// a Mac app from before the field recorded macos of unknown
		// distribution; the updated app's report fills it in
		olderNetworkId := server.NewId()
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: olderNetworkId, CreatedAt: now, Email: true}) {
			t.Fatal("could not create the older-app fixture")
		}
		older := appSession(olderNetworkId, `{"v":1,"device_type":"macos","app_version":"2026.11.10-1"}`)
		defer older.Cancel()
		RecordOnboardingClientContext(older, olderNetworkId, "", "", macSpec)
		if row = model.GetNetworkOnboarding(ctx, olderNetworkId); row == nil || row.Platform != "macos" || row.Distribution != "" {
			t.Fatalf("the older app recorded %+v, want macos of unknown distribution", row)
		}
		updated := appSession(olderNetworkId, `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","distribution":"app_store"}`)
		defer updated.Cancel()
		RecordOnboardingClientContext(updated, olderNetworkId, "", "", macSpec)
		if row = model.GetNetworkOnboarding(ctx, olderNetworkId); row == nil || row.Platform != "macos" || row.Distribution != session.ClientDistributionAppStore {
			t.Fatalf("the updated app recorded %+v, want macos/app_store", row)
		}
	})
}
