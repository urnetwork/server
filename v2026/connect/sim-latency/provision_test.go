package main

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestFirstNetworkAdminUsersUsesFirstFixtureEntry(t *testing.T) {
	users := firstNetworkAdminUsers([]ProviderEntry{
		{NetworkId: "shared", UserId: "first"},
		{NetworkId: "singleton", UserId: "only"},
		{NetworkId: "shared", UserId: "second"},
	})
	if got := users["shared"]; got != "first" {
		t.Fatalf("shared-network admin = %q, want first fixture user", got)
	}
	if got := users["singleton"]; got != "only" {
		t.Fatalf("singleton admin = %q, want only fixture user", got)
	}
}

func TestProvisionProvidersCreatesCurrentSelectionPrerequisites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		firstUserId := server.NewId()
		secondUserId := server.NewId()
		firstClientId := server.NewId()
		secondClientId := server.NewId()
		entries := []ProviderEntry{
			{
				Index:     0,
				NetworkId: networkId.String(),
				UserId:    firstUserId.String(),
				DeviceId:  server.NewId().String(),
				ClientId:  firstClientId.String(),
				UserType:  "hosting",
				Component: "mobile-variable",
			},
			{
				Index:     1,
				NetworkId: networkId.String(),
				UserId:    secondUserId.String(),
				DeviceId:  server.NewId().String(),
				ClientId:  secondClientId.String(),
			},
		}

		locationId, err := provisionRegion(ctx, RegionConfig{
			Country:     "Simulation Test Country",
			CountryCode: "zz",
			Region:      "Simulation Test Region",
			City:        "Simulation Test City",
		})
		if err != nil {
			t.Fatalf("provisionRegion: %v", err)
		}
		if err := provisionProviders(ctx, entries, locationId, "ZZ"); err != nil {
			t.Fatalf("provisionProviders: %v", err)
		}

		// Re-provisioning an existing run must reactivate clients. This matters
		// after a prior connection lifecycle has marked a fixture inactive.
		server.Db(ctx, func(conn server.PgConn) {
			_, err := conn.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, firstClientId)
			server.Raise(err)
		})
		if err := provisionIdentityBatch(ctx, entries); err != nil {
			t.Fatalf("re-provision identities: %v", err)
		}

		var adminUserId server.Id
		var userCount int
		var active bool
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					n.admin_user_id,
					(SELECT COUNT(*) FROM network_user WHERE user_id = ANY($2)),
					nc.active
				FROM network n
				JOIN network_client nc ON nc.network_id = n.network_id
				WHERE n.network_id = $1 AND nc.client_id = $3
				`,
				networkId,
				[]server.Id{firstUserId, secondUserId},
				firstClientId,
			)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("provisioned identity rows not found")
				}
				server.Raise(result.Scan(&adminUserId, &userCount, &active))
			})
		})
		if adminUserId != firstUserId {
			t.Fatalf("network admin = %s, want first fixture user %s", adminUserId, firstUserId)
		}
		if userCount != 2 {
			t.Fatalf("network_user count = %d, want 2", userCount)
		}
		if !active {
			t.Fatal("re-provisioned client is inactive")
		}

		health := model.GetProviderEgressHealth(ctx, firstClientId)
		if health == nil {
			t.Fatal("simulated egress health not provisioned")
		}
		if health.OKCount != 1 || health.Total != 1 {
			t.Fatalf("simulated egress health = %d/%d, want 1/1", health.OKCount, health.Total)
		}
		if got := health.ClassResults["sim"]; got.OK != 1 || got.Total != 1 {
			t.Fatalf("simulated class health = %+v, want 1/1", got)
		}

		location := model.GetProviderEgressLocation(ctx, firstClientId)
		if location == nil {
			t.Fatal("simulated egress location not provisioned")
		}
		if location.LocationId != locationId || location.CountryCode != "zz" {
			t.Fatalf("simulated location = %s/%q, want %s/zz", location.LocationId, location.CountryCode, locationId)
		}
		if !location.Hosting || !location.Mobile || location.Proxy {
			t.Fatalf("simulated location flags = hosting:%t mobile:%t proxy:%t", location.Hosting, location.Mobile, location.Proxy)
		}
		if location.Verdict != "verified" || location.Assurance != model.ProviderEgressAssuranceDirect {
			t.Fatalf("simulated location judgement = %q/%q", location.Verdict, location.Assurance)
		}
	})
}
