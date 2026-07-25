package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestSubmitProviderEgressLocationCountryOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "US",
			Country:          "United States",
			ASN:              401486,
			Org:              "RAVNIX LLC",
			Hosting:          true,
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		assert.Equal(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		assert.Equal(t, stored.CountryCode, "us")
		assert.Equal(t, stored.ASN, 401486)
		assert.Equal(t, stored.Hosting, true)
		assert.Equal(t, stored.CityConfident, false)

		// the resolved location must be the country-granular row, with no
		// city/region association
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		assert.Equal(t, loc.LocationType, model.LocationTypeCountry)
		if loc.CityLocationId != (server.Id{}) {
			t.Fatal("a country-granularity row must not have a city association")
		}
		if loc.RegionLocationId != (server.Id{}) {
			t.Fatal("a country-granularity row must not have a region association")
		}
	})
}

func TestSubmitProviderEgressLocationRejectsNotCountryConfident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			CountryConfident: false,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a submission that is not country-confident must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

func TestSubmitProviderEgressLocationRejectsUnknownClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         server.NewId(), // never created
			CountryCode:      "us",
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("unknown client_id must be rejected")
		}
	})
}

func TestSubmitProviderEgressLocationCityConfidentStoresCity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Region:           "Colorado",
			City:             "Denver",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		assert.Equal(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		assert.Equal(t, stored.CityConfident, true)

		// the resolved location must be the city-granular row
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		assert.Equal(t, loc.LocationType, model.LocationTypeCity)
	})
}

func TestSubmitProviderEgressLocationRejectsStaleObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(-30 * 24 * time.Hour),
		})
		if err == nil {
			t.Fatal("a submission observed long ago must be rejected")
		}
	})
}
