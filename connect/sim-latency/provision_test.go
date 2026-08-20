package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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

func TestGeneratedClientPoolEntriesReplayFixtureSeed(t *testing.T) {
	config := &Config{
		Seed: 48,
		Clients: ClientsConfig{
			PoolSize: 64,
		},
	}
	first := generatedClientPoolEntries(config)
	second := generatedClientPoolEntries(config)
	if len(first) != config.Clients.PoolSize {
		t.Fatalf("entry count = %d, want %d", len(first), config.Clients.PoolSize)
	}

	seen := map[string]string{}
	for index, entry := range first {
		if entry != second[index] {
			t.Fatalf("entry %d differs for identical fixture seed:\n first=%+v\nsecond=%+v", index, entry, second[index])
		}
		if entry.Index != index {
			t.Fatalf("entry %d index = %d", index, entry.Index)
		}
		for kind, id := range map[string]string{
			"network": entry.NetworkId,
			"user":    entry.UserId,
			"device":  entry.DeviceId,
			"client":  entry.ClientId,
		} {
			if _, err := server.ParseId(id); err != nil {
				t.Fatalf("entry %d %s id %q is invalid: %v", index, kind, id, err)
			}
			if previous, ok := seen[id]; ok {
				t.Fatalf("entry %d %s id duplicates %s", index, kind, previous)
			}
			seen[id] = fmt.Sprintf("entry %d %s id", index, kind)
		}
	}

	other := generatedClientPoolEntries(&Config{
		Seed: config.Seed + 1,
		Clients: ClientsConfig{
			PoolSize: config.Clients.PoolSize,
		},
	})
	if first[0] == other[0] {
		t.Fatal("different fixture seeds produced the same first client identity")
	}
}

func TestMatureProviderReliabilitiesUseSeededDutyCycle(t *testing.T) {
	firstClientId := server.NewId()
	secondClientId := server.NewId()
	reliabilities, err := matureProviderReliabilities([]ProviderEntry{
		{
			Index:           4,
			ClientId:        firstClientId.String(),
			UptimeSeconds:   90,
			DowntimeSeconds: 10,
		},
		{
			Index:           9,
			ClientId:        secondClientId.String(),
			UptimeSeconds:   30,
			DowntimeSeconds: 0,
		},
	})
	if err != nil {
		t.Fatalf("matureProviderReliabilities: %v", err)
	}
	if len(reliabilities) != 2 {
		t.Fatalf("reliability count = %d, want 2", len(reliabilities))
	}
	if reliabilities[0].clientId != firstClientId || math.Abs(reliabilities[0].reliabilityWeight-0.9) > 1e-12 {
		t.Fatalf("first reliability = %+v, want %s at 0.9", reliabilities[0], firstClientId)
	}
	if reliabilities[1].clientId != secondClientId || reliabilities[1].reliabilityWeight != 1 {
		t.Fatalf("second reliability = %+v, want %s at 1", reliabilities[1], secondClientId)
	}
}

func TestMatureProviderReliabilitiesRejectInvalidGroundTruth(t *testing.T) {
	tests := []struct {
		name  string
		entry ProviderEntry
		want  string
	}{
		{
			name: "client id",
			entry: ProviderEntry{
				Index:           1,
				ClientId:        "invalid",
				UptimeSeconds:   1,
				DowntimeSeconds: 1,
			},
			want: "client id",
		},
		{
			name: "zero uptime",
			entry: ProviderEntry{
				Index:           2,
				ClientId:        server.NewId().String(),
				UptimeSeconds:   0,
				DowntimeSeconds: 1,
			},
			want: "uptime",
		},
		{
			name: "nan uptime",
			entry: ProviderEntry{
				Index:           3,
				ClientId:        server.NewId().String(),
				UptimeSeconds:   math.NaN(),
				DowntimeSeconds: 1,
			},
			want: "uptime",
		},
		{
			name: "negative downtime",
			entry: ProviderEntry{
				Index:           4,
				ClientId:        server.NewId().String(),
				UptimeSeconds:   1,
				DowntimeSeconds: -1,
			},
			want: "downtime",
		},
		{
			name: "infinite cycle",
			entry: ProviderEntry{
				Index:           5,
				ClientId:        server.NewId().String(),
				UptimeSeconds:   math.MaxFloat64,
				DowntimeSeconds: math.MaxFloat64,
			},
			want: "cycle",
		},
	}
	for _, test := range tests {
		_, err := matureProviderReliabilities([]ProviderEntry{test.entry})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s error = %v, want containing %q", test.name, err, test.want)
		}
	}
}

func TestWriteMatureReliabilityScoresPersistsFixtureWeights(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		locationId, err := provisionRegion(ctx, RegionConfig{
			Country:     "Simulation Reliability Country",
			CountryCode: "zr",
			Region:      "Simulation Reliability Region",
			City:        "Simulation Reliability City",
		})
		if err != nil {
			t.Fatalf("provisionRegion: %v", err)
		}

		entries := []ProviderEntry{
			{
				Index:           0,
				NetworkId:       server.NewId().String(),
				UserId:          server.NewId().String(),
				DeviceId:        server.NewId().String(),
				ClientId:        server.NewId().String(),
				UptimeSeconds:   90,
				DowntimeSeconds: 10,
			},
			{
				Index:           1,
				NetworkId:       server.NewId().String(),
				UserId:          server.NewId().String(),
				DeviceId:        server.NewId().String(),
				ClientId:        server.NewId().String(),
				UptimeSeconds:   30,
				DowntimeSeconds: 10,
			},
			{
				Index:           2,
				NetworkId:       server.NewId().String(),
				UserId:          server.NewId().String(),
				DeviceId:        server.NewId().String(),
				ClientId:        server.NewId().String(),
				UptimeSeconds:   99,
				DowntimeSeconds: 1,
			},
		}
		if err := provisionProviders(ctx, entries, locationId, "ZR"); err != nil {
			t.Fatalf("provisionProviders: %v", err)
		}

		server.Db(ctx, func(conn server.PgConn) {
			for index, entry := range entries {
				server.RaisePgResult(conn.Exec(
					ctx,
					`
					INSERT INTO network_client_location_reliability (
						client_id, network_id, update_block_number,
						city_location_id, region_location_id, country_location_id,
						client_address_hash_count, location_count, connected
					)
					VALUES ($1, $2, 0, $3, $3, $3, 1, 1, $4)
					`,
					server.RequireParseId(entry.ClientId),
					server.RequireParseId(entry.NetworkId),
					locationId,
					index < 2,
				))
			}
		})

		reliabilities, err := matureProviderReliabilities(entries)
		if err != nil {
			t.Fatalf("matureProviderReliabilities: %v", err)
		}
		perfect := make([]matureProviderReliability, 0, len(reliabilities))
		for _, reliability := range reliabilities {
			perfect = append(perfect, matureProviderReliability{
				clientId:          reliability.clientId,
				reliabilityWeight: 1,
			})
		}
		now := time.Date(2026, time.August, 20, 8, 0, 0, 0, time.UTC)
		writeMatureReliabilityScores(ctx, now, 13*time.Hour, perfect)
		writeMatureReliabilityScores(ctx, now, 13*time.Hour, reliabilities)

		weightClientIds := map[server.Id][]float64{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT client_id, independent_reliability_weight, reliability_weight
				FROM client_connection_reliability_score
				WHERE client_id = ANY($1::uuid[])
				ORDER BY client_id, lookback_index
				`,
				[]server.Id{
					server.RequireParseId(entries[0].ClientId),
					server.RequireParseId(entries[1].ClientId),
					server.RequireParseId(entries[2].ClientId),
				},
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var independentWeight float64
					var reliabilityWeight float64
					server.Raise(result.Scan(&clientId, &independentWeight, &reliabilityWeight))
					if independentWeight != reliabilityWeight {
						t.Fatalf("client %s weights differ: %f != %f", clientId, independentWeight, reliabilityWeight)
					}
					weightClientIds[clientId] = append(weightClientIds[clientId], reliabilityWeight)
				}
			})
		})

		for index, want := range []float64{0.9, 0.75} {
			clientId := server.RequireParseId(entries[index].ClientId)
			weights := weightClientIds[clientId]
			if len(weights) != 3 {
				t.Fatalf("client %s score count = %d, want 3", clientId, len(weights))
			}
			for _, weight := range weights {
				if math.Abs(weight-want) > 1e-12 {
					t.Fatalf("client %s weight = %f, want %f", clientId, weight, want)
				}
			}
		}
		disconnectedClientId := server.RequireParseId(entries[2].ClientId)
		if len(weightClientIds[disconnectedClientId]) != 0 {
			t.Fatalf("disconnected client %s received reliability scores", disconnectedClientId)
		}
	})
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
