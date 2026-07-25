package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
		connect.AssertEqual(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.CountryCode, "us")
		connect.AssertEqual(t, stored.ASN, 401486)
		connect.AssertEqual(t, stored.Hosting, true)
		connect.AssertEqual(t, stored.CityConfident, false)

		// the resolved location must be the country-granular row, with no
		// city/region association
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		connect.AssertEqual(t, loc.LocationType, model.LocationTypeCountry)
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
		connect.AssertEqual(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.CityConfident, true)

		// the resolved location must be the city-granular row
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		connect.AssertEqual(t, loc.LocationType, model.LocationTypeCity)
	})
}

// An empty Country must be rejected, not silently stored: model.CreateLocation
// dedupes country rows on (location_type, country_code), so an empty name
// would create a canonical, permanently-blank row that every later lookup for
// that country reuses forever.
func TestSubmitProviderEgressLocationRejectsEmptyCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "   ", // blank after trimming
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("an empty country must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// A city-confident submission with an empty City must be rejected rather than
// silently falling back to country granularity: the same empty-canonical-row
// corruption applies to city/region rows.
func TestSubmitProviderEgressLocationRejectsEmptyCityWhenCityConfident(t *testing.T) {
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
			City:             "",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a city-confident submission with an empty city must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// A city-confident submission with an empty Region must likewise be rejected:
// the region row is created with the same dedupe-on-empty-name hazard.
func TestSubmitProviderEgressLocationRejectsEmptyRegionWhenCityConfident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Region:           "",
			City:             "Denver",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a city-confident submission with an empty region must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// An over-long Country must be rejected with a clear error instead of
// panicking inside model.CreateLocation on a Postgres "value too long for
// type character varying(128)" error.
func TestSubmitProviderEgressLocationRejectsOverLongCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          strings.Repeat("a", 129),
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("an over-long country must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// An over-long Org must likewise be rejected rather than panicking inside
// storage.
func TestSubmitProviderEgressLocationRejectsOverLongOrg(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Org:              strings.Repeat("a", 257),
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("an over-long org must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
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

// A far-future observed_at must be rejected, not just an old one: unchecked,
// it would defeat the monotonic upsert (it always "wins"), read as fresh
// forever, and outlive the taskworker sweep -- permanently pinning a
// provider's location with no API-side recovery. See
// MaxProviderEgressLocationSubmissionSkew.
func TestSubmitProviderEgressLocationRejectsFutureObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "jp",
			Country:          "Japan",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(10 * 365 * 24 * time.Hour),
		})
		if err == nil {
			t.Fatal("a far-future observed_at must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// A submission within the allowed clock-skew window must still be accepted:
// the future-timestamp rejection must not be so strict that ordinary clock
// drift between the prober and server breaks legitimate submissions.
func TestSubmitProviderEgressLocationAcceptsWithinSkewObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(1 * time.Minute),
		})
		connect.AssertEqual(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
	})
}

// A large 32-bit ASN (e.g. from the private-use range, common on
// hosting/VPN infrastructure) must round-trip cleanly, not panic. The asn
// column used to be `int` (Postgres int4, max ~2.147e9); ASNs are 32-bit
// unsigned (max ~4.295e9), so a value above int4's range panicked deep in
// pgx's arg encoding.
func TestSubmitProviderEgressLocationAcceptsLargeAsn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		const largeAsn = 4200000000

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			ASN:              largeAsn,
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.ASN, largeAsn)
	})
}
