package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestProviderEgressLocationUpsertAndGet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		clientId := server.NewId()
		now := server.NowUtc()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "us",
			ASN:         401486,
			Org:         "RAVNIX LLC",
			Hosting:     true,
			ObservedAt:  now,
		})

		got := GetProviderEgressLocation(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, got.LocationId, country.LocationId)
		assert.Equal(t, got.CountryCode, "us")
		assert.Equal(t, got.ASN, 401486)
		assert.Equal(t, got.Hosting, true)
		assert.Equal(t, got.Proxy, false)

		// upsert replaces
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "us",
			ASN:         999,
			Hosting:     false,
			Proxy:       true,
			ObservedAt:  now,
		})
		got = GetProviderEgressLocation(ctx, clientId)
		assert.Equal(t, got.ASN, 999)
		assert.Equal(t, got.Hosting, false)
		assert.Equal(t, got.Proxy, true)
	})
}

func TestProviderEgressLocationCountryCodeLowercased(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		clientId := server.NewId()
		// geolocation APIs return uppercase codes (e.g. "US"); the model must
		// normalize to lowercase before storing, matching CreateLocation's
		// established invariant that country codes are stored/compared lowercased.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "US",
			ASN:         12345,
			Org:         "TEST ORG",
			ObservedAt:  server.NowUtc(),
		})

		got := GetProviderEgressLocation(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, got.CountryCode, "us")
	})
}

func TestProviderEgressLocationFreshness(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		fresh := server.NewId()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc(),
		})
		stale := server.NewId()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: stale, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc().Add(-8 * 24 * time.Hour),
		})

		if GetFreshProviderEgressLocation(ctx, fresh, ProviderEgressLocationMaxAge) == nil {
			t.Fatal("fresh entry must be returned")
		}
		if GetFreshProviderEgressLocation(ctx, stale, ProviderEgressLocationMaxAge) != nil {
			t.Fatal("stale entry must not be returned")
		}
		// absent
		if GetFreshProviderEgressLocation(ctx, server.NewId(), ProviderEgressLocationMaxAge) != nil {
			t.Fatal("absent entry must return nil")
		}
	})
}

func TestRemoveExpiredProviderEgressLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		keep := server.NewId()
		drop := server.NewId()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: keep, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc(),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: drop, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc().Add(-30 * 24 * time.Hour),
		})

		RemoveExpiredProviderEgressLocations(ctx, server.NowUtc().Add(-14*24*time.Hour))

		if GetProviderEgressLocation(ctx, keep) == nil {
			t.Fatal("recent entry must survive the sweep")
		}
		if GetProviderEgressLocation(ctx, drop) != nil {
			t.Fatal("old entry must be swept")
		}
	})
}
