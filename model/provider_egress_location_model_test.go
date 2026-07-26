package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
		connect.AssertEqual(t, got.LocationId, country.LocationId)
		connect.AssertEqual(t, got.CountryCode, "us")
		connect.AssertEqual(t, got.ASN, 401486)
		connect.AssertEqual(t, got.Hosting, true)
		connect.AssertEqual(t, got.Proxy, false)

		// upsert replaces, given a strictly newer observed_at: the upsert is
		// monotonic (see TestProviderEgressLocationUpsertIgnoresOlderReplay below),
		// so a second submission at the same observed_at would not win.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "us",
			ASN:         999,
			Hosting:     false,
			Proxy:       true,
			ObservedAt:  now.Add(time.Minute),
		})
		got = GetProviderEgressLocation(ctx, clientId)
		connect.AssertEqual(t, got.ASN, 999)
		connect.AssertEqual(t, got.Hosting, false)
		connect.AssertEqual(t, got.Proxy, true)
	})
}

// The upsert is monotonic in observed_at: a replayed submission older than
// what is already stored must not clobber the newer row.
func TestProviderEgressLocationUpsertIgnoresOlderReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		usCountry := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, usCountry)

		jpCountry := &Location{
			LocationType: LocationTypeCountry,
			Country:      "Japan",
			CountryCode:  "jp",
		}
		CreateLocation(ctx, jpCountry)

		clientId := server.NewId()
		newer := server.NowUtc()
		older := newer.Add(-1 * time.Hour)

		// the newer probe lands first
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  jpCountry.LocationId,
			CountryCode: "jp",
			ASN:         111,
			ObservedAt:  newer,
		})

		// a stale/replayed older probe arrives afterward and must not win
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  usCountry.LocationId,
			CountryCode: "us",
			ASN:         222,
			ObservedAt:  older,
		})

		got := GetProviderEgressLocation(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored egress location")
		}
		connect.AssertEqual(t, got.CountryCode, "jp")
		connect.AssertEqual(t, got.ASN, 111)
		connect.AssertEqual(t, got.LocationId, jpCountry.LocationId)
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
		connect.AssertEqual(t, got.CountryCode, "us")
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

// testing_connectProbeableProvider stands up the minimum a client needs to
// look like a live provider to the due-selection query: a device, a live
// connection with a resolved location, and a provide key of the given mode.
// The caller must run UpdateClientLocationReliabilities afterward -- that is
// what rolls the live connection tables up into the
// network_client_location_reliability row (connected + valid) the query reads.
func testing_connectProbeableProvider(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	locationId server.Id,
	clientAddress string,
	provideMode ProvideMode,
) {
	Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")

	handlerId := CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	if err != nil {
		t.Fatalf("connect client: %s", err)
	}

	if err := SetConnectionLocation(ctx, connectionId, locationId, &ConnectionLocationScores{}); err != nil {
		t.Fatalf("set connection location: %s", err)
	}

	SetProvide(ctx, clientId, map[ProvideMode][]byte{
		provideMode: []byte("provide-secret"),
	})
}

// The prober asks the server what to probe next. The answer must be sourced
// from the live provider population and not from provider_egress_location,
// because the dominant case -- a provider that has never been probed at all --
// has no row there.
func TestGetProviderEgressLocationDue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		fresh := server.NewId()
		stale := server.NewId()
		never := server.NewId()
		// a provider that cannot serve a stranger is unprobeable: the tunnel
		// contract would be refused, so it must never be offered to the prober
		nonPublic := server.NewId()

		testing_connectProbeableProvider(t, ctx, fresh, city.LocationId, "0.0.0.1:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, stale, city.LocationId, "0.0.0.2:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, never, city.LocationId, "0.0.0.3:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, nonPublic, city.LocationId, "0.0.0.4:0", ProvideModeNetwork)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: stale, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-72 * time.Hour),
		})
		// `never` and `nonPublic` deliberately get no row at all

		due := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), 100)

		// a provider probed an hour ago must not be re-probed; one probed three
		// days ago must be; one never probed must be
		if slices.Contains(due, fresh) {
			t.Fatalf("due = %v, must not contain the provider probed an hour ago (%s)", due, fresh)
		}
		if !slices.Contains(due, stale) {
			t.Fatalf("due = %v, must contain the provider probed three days ago (%s)", due, stale)
		}
		if !slices.Contains(due, never) {
			t.Fatalf("due = %v, must contain the never-probed provider (%s)", due, never)
		}
		// unprobeable regardless of freshness
		if slices.Contains(due, nonPublic) {
			t.Fatalf("due = %v, must not contain the provider without a Public provide key (%s)", due, nonPublic)
		}

		// oldest first, so the longest-unprobed are probed first: the
		// never-probed provider sorts ahead of the three-days-stale one
		neverIndex := slices.Index(due, never)
		staleIndex := slices.Index(due, stale)
		if staleIndex < neverIndex {
			t.Fatalf("never-probed provider at %d must sort before the stale one at %d", neverIndex, staleIndex)
		}

		// limit is honoured
		limited := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), 1)
		if len(limited) != 1 {
			t.Fatalf("len(due) = %d for limit 1, want 1", len(limited))
		}
		if limited[0] != never {
			t.Fatalf("due[0] = %s for limit 1, want the never-probed provider %s", limited[0], never)
		}
	})
}
