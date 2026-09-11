package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
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
// It returns the connection id, so a caller can disconnect the provider again.
func testing_connectProbeableProvider(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	locationId server.Id,
	clientAddress string,
	provideMode ProvideMode,
) server.Id {
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

	return connectionId
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

		testing_connectProbeableProvider(t, ctx, fresh, city.LocationId, "192.0.2.1:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, stale, city.LocationId, "192.0.2.2:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, never, city.LocationId, "192.0.2.3:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, nonPublic, city.LocationId, "192.0.2.4:0", ProvideModeNetwork)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: stale, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-72 * time.Hour),
		})
		for _, clientId := range []server.Id{fresh, stale} {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId: clientId, MeasuredAt: now,
				OKCount: 1, Total: 1,
			})
		}
		// `never` and `nonPublic` deliberately get no row at all

		// no attempt rows exist in this test, so the attempt cutoff never
		// excludes anything; freshness is the only variable
		due := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), now, 100)

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

		// A stale evidenced provider has a hard expiry; it is admitted before
		// unlocated work even when the unlocated backlog can fill the batch.
		neverIndex := slices.Index(due, never)
		staleIndex := slices.Index(due, stale)
		if neverIndex < staleIndex {
			t.Fatalf("stale provider at %d must sort before the unlocated one at %d", staleIndex, neverIndex)
		}

		// limit is honoured
		limited := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), now, 1)
		if len(limited) != 1 {
			t.Fatalf("len(due) = %d for limit 1, want 1", len(limited))
		}
		if limited[0] != stale {
			t.Fatalf("due[0] = %s for limit 1, want the urgent stale provider %s", limited[0], stale)
		}
	})
}

// A provider that connects, holds a Public provide key and fails every probe
// never gets a provider_egress_location row, so its observed_at stays NULL, so
// it sorts ahead of every stale-but-refreshable provider -- forever, on every
// poll. Enough of them to fill a batch and no healthy provider's location is
// ever refreshed again, silently: the endpoint keeps returning a full,
// plausible-looking batch of the same dead providers.
//
// A recent attempt must therefore defer a provider exactly as a fresh success
// does. Deleting the attempt predicate from GetProviderEgressLocationDue must
// fail this test.
func TestGetProviderEgressLocationDueDefersRecentlyAttempted(t *testing.T) {
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

		// never probed successfully, and the prober just tried it and failed
		dead := server.NewId()
		// probed successfully three days ago, never attempted since: the
		// provider that actually needs the next probe slot
		healthyStale := server.NewId()

		testing_connectProbeableProvider(t, ctx, dead, city.LocationId, "192.0.2.1:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, healthyStale, city.LocationId, "192.0.2.2:0", ProvideModePublic)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: healthyStale, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-72 * time.Hour),
		})
		// `dead` deliberately gets no location row -- it has never succeeded --
		// only a failed attempt seconds ago
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId:     dead,
			AttemptAt:    now.Add(-5 * time.Second),
			ProbeFailure: "tunnel_failed",
		})

		minObservedAt := now.Add(-24 * time.Hour)
		minAttemptAt := now.Add(-ProviderEgressProbeAttemptBackoff)

		due := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 100)

		if slices.Contains(due, dead) {
			t.Fatalf("due = %v, must not contain the provider attempted seconds ago (%s)", due, dead)
		}
		if !slices.Contains(due, healthyStale) {
			t.Fatalf("due = %v, must contain the stale-but-refreshable provider (%s)", due, healthyStale)
		}

		// the starvation itself: with a batch big enough for exactly one
		// provider, the slot must go to the one that can actually be refreshed,
		// not to the never-probed one that just failed. Without the attempt
		// predicate `dead` wins this on observed_at IS NULL every single poll.
		limited := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 1)
		if len(limited) != 1 {
			t.Fatalf("len(due) = %d for limit 1, want 1", len(limited))
		}
		if limited[0] != healthyStale {
			t.Fatalf("due[0] = %s for limit 1, want the refreshable provider %s, not the just-failed one %s", limited[0], healthyStale, dead)
		}

		// ... and the deferral is a backoff, not a ban: once the backoff has
		// elapsed the same provider is offered again. The caller computes the
		// cutoff as (wall clock - backoff), so a poll exactly one backoff period
		// after the attempt computes `now`.
		afterBackoff := GetProviderEgressLocationDue(ctx, minObservedAt, now, 100)
		if !slices.Contains(afterBackoff, dead) {
			t.Fatalf("due = %v, must contain the failed provider (%s) again once the attempt backoff has elapsed", afterBackoff, dead)
		}
	})
}

// Only live, routable providers are probeable. A provider that has gone offline
// (connected = false) or that looks messed up from a routing perspective
// (valid = false, a generated column: more than one address hash or location on
// its live connections) must not be handed to the prober. Deleting either
// predicate from GetProviderEgressLocationDue must fail this test.
func TestGetProviderEgressLocationDueRequiresConnectedAndValid(t *testing.T) {
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

		good := server.NewId()
		disconnected := server.NewId()
		invalid := server.NewId()

		testing_connectProbeableProvider(t, ctx, good, city.LocationId, "192.0.2.1:0", ProvideModePublic)
		disconnectedConnectionId := testing_connectProbeableProvider(t, ctx, disconnected, city.LocationId, "192.0.2.2:0", ProvideModePublic)

		// `invalid` holds two simultaneous connections from two different
		// addresses, which makes client_address_hash_count = 2 and so the
		// generated `valid` column false. The two addresses must be in
		// different /29s: server.ClientIpHash buckets ipv4 to the /29 network,
		// so e.g. 192.0.2.3 and 192.0.2.4 would hash the same and count as one.
		testing_connectProbeableProvider(t, ctx, invalid, city.LocationId, "192.0.2.3:0", ProvideModePublic)
		secondHandlerId := CreateNetworkClientHandler(ctx)
		secondConnectionId, _, _, _, err := ConnectNetworkClient(ctx, invalid, "192.0.2.11:0", secondHandlerId)
		if err != nil {
			t.Fatalf("connect second address: %s", err)
		}
		if err := SetConnectionLocation(ctx, secondConnectionId, city.LocationId, &ConnectionLocationScores{}); err != nil {
			t.Fatalf("set second connection location: %s", err)
		}

		// first roll-up: everything above is connected
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		// `disconnected` drops off, and a second roll-up flips its reliability
		// row's connected to false (the row itself survives)
		if err := DisconnectNetworkClient(ctx, disconnectedConnectionId); err != nil {
			t.Fatalf("disconnect client: %s", err)
		}
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), server.NowUtc())

		due := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), now, 100)

		if !slices.Contains(due, good) {
			t.Fatalf("due = %v, must contain the connected, valid provider (%s)", due, good)
		}
		if slices.Contains(due, disconnected) {
			t.Fatalf("due = %v, must not contain the disconnected provider (%s)", due, disconnected)
		}
		if slices.Contains(due, invalid) {
			t.Fatalf("due = %v, must not contain the provider whose reliability row is not valid (%s)", due, invalid)
		}
	})
}

// The due queue has independent SQL heads for never located, stale location,
// stale health, and located-without-health providers. Every head reads the live
// provider population directly and must reject derivative and inactive clients
// even while their connected rows and Public keys remain.
func TestGetProviderEgressLocationDueExcludesDerivedAndInactiveClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)
		sourceClientId := testingCreateProviderClient(ctx, networkId, nil, true)

		activeClientIds := []server.Id{}
		excludedClientIds := []server.Id{}
		newGroup := func() (activeClientId server.Id, derivedClientId server.Id, inactiveClientId server.Id) {
			activeClientId = testingCreateProviderClient(ctx, networkId, nil, true)
			derivedClientId = testingCreateProviderClient(ctx, networkId, &sourceClientId, true)
			inactiveClientId = testingCreateProviderClient(ctx, networkId, nil, false)
			activeClientIds = append(activeClientIds, activeClientId)
			excludedClientIds = append(excludedClientIds, derivedClientId, inactiveClientId)
			for _, clientId := range []server.Id{activeClientId, derivedClientId, inactiveClientId} {
				testingInsertProviderLocationReliability(ctx, clientId, networkId, city)
			}
			return
		}

		// pass 1: no egress-location row
		newGroup()

		// pass 2: egress location older than the requested freshness horizon
		activeStaleId, derivedStaleId, inactiveStaleId := newGroup()
		for _, clientId := range []server.Id{activeStaleId, derivedStaleId, inactiveStaleId} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId:    clientId,
				LocationId:  city.LocationId,
				CountryCode: "us",
				ObservedAt:  now.Add(-48 * time.Hour),
			})
		}

		// pass 3: location is fresh, but an existing health row has aged out
		activeHealthId, derivedHealthId, inactiveHealthId := newGroup()
		for _, clientId := range []server.Id{activeHealthId, derivedHealthId, inactiveHealthId} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId:    clientId,
				LocationId:  city.LocationId,
				CountryCode: "us",
				ObservedAt:  now.Add(-time.Hour),
			})
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId:   clientId,
				MeasuredAt: now.Add(-2 * ProviderEgressHealthMaxAge),
				OKCount:    1,
				Total:      1,
			})
		}

		due := GetProviderEgressLocationDue(
			ctx,
			now.Add(-24*time.Hour),
			now.Add(-ProviderEgressProbeAttemptBackoff),
			100,
		)
		for _, clientId := range activeClientIds {
			if !slices.Contains(due, clientId) {
				t.Errorf("due = %v, missing active top-level provider %s", due, clientId)
			}
		}
		for _, clientId := range excludedClientIds {
			if slices.Contains(due, clientId) {
				t.Errorf("due = %v, contains derived or inactive provider %s", due, clientId)
			}
		}
	})
}

// The attempt upsert is monotonic in attempt_at, for the same reason the
// location upsert is: a replayed or out-of-order report must not move the last
// attempt backwards and hand the provider back to the prober early.
func TestProviderEgressProbeAttemptUpsertIgnoresOlderReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		newer := server.NowUtc()
		older := newer.Add(-time.Hour)

		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: clientId, AttemptAt: newer, ProbeFailure: "no_consensus",
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: clientId, AttemptAt: older, ProbeFailure: "tunnel_failed",
		})

		got := GetProviderEgressProbeAttempt(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored probe attempt")
		}
		connect.AssertEqual(t, got.ProbeFailure, "no_consensus")
		// postgres `timestamp` keeps microseconds, Go keeps nanoseconds, so
		// compare with a tolerance rather than for equality
		if delta := got.AttemptAt.Sub(newer); delta < -time.Millisecond || time.Millisecond < delta {
			t.Fatalf("attempt_at = %s, want the newer attempt %s", got.AttemptAt, newer)
		}

		// a strictly newer report does win
		newest := newer.Add(time.Minute)
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: clientId, AttemptAt: newest, ProbeFailure: "",
		})
		got = GetProviderEgressProbeAttempt(ctx, clientId)
		connect.AssertEqual(t, got.ProbeFailure, "")

		// absent
		if GetProviderEgressProbeAttempt(ctx, server.NewId()) != nil {
			t.Fatal("absent attempt must return nil")
		}
	})
}

func TestRemoveExpiredProviderEgressProbeAttempts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Synthetic City",
			Region:       "Synthetic Region",
			Country:      "Synthetic Country",
			CountryCode:  "zz",
		}
		CreateLocation(ctx, city)

		eligibleNoLocation := testingCreateProviderClient(ctx, networkId, nil, true)
		located := testingCreateProviderClient(ctx, networkId, nil, true)
		inactive := testingCreateProviderClient(ctx, networkId, nil, false)
		ineligible := testingCreateProviderClient(ctx, networkId, nil, true)
		orphan := server.NewId()
		recentLocated := testingCreateProviderClient(ctx, networkId, nil, true)
		for _, clientId := range []server.Id{
			eligibleNoLocation, located, inactive, ineligible, recentLocated,
		} {
			testingInsertProviderLocationReliability(ctx, clientId, networkId, city)
		}
		SetProvide(ctx, ineligible, map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("synthetic-network-secret"),
		})
		for _, clientId := range []server.Id{located, recentLocated} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, LocationId: city.LocationId,
				CountryCode: "zz", ObservedAt: now,
			})
		}

		old := now.Add(-30 * 24 * time.Hour)
		for _, clientId := range []server.Id{
			eligibleNoLocation, located, inactive, ineligible, orphan,
		} {
			SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
				ClientId: clientId, AttemptAt: old,
			})
		}
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: recentLocated, AttemptAt: now,
		})

		RemoveExpiredProviderEgressProbeAttempts(ctx, now.Add(-24*time.Hour))

		if GetProviderEgressProbeAttempt(ctx, eligibleNoLocation) == nil {
			t.Fatal("old attempt for an active eligible no-location provider must be retained")
		}
		if GetProviderEgressProbeAttempt(ctx, recentLocated) == nil {
			t.Fatal("recent located attempt must survive the age cutoff")
		}
		for label, clientId := range map[string]server.Id{
			"located": located, "inactive": inactive,
			"ineligible": ineligible, "orphan": orphan,
		} {
			if GetProviderEgressProbeAttempt(ctx, clientId) != nil {
				t.Errorf("old %s attempt must be swept", label)
			}
		}
	})
}

// The due queue takes bounded location, existing-health, and missing-health
// heads, merges their absolute hard deadlines in Go, and only then fills unused
// slots from the unlocated lane. This reproduces the saturated-backlog failure:
// every limit below the unlocated population used to return only unlocated
// rows, making every urgent lane unreachable.
func TestGetProviderEgressLocationDueOrderingIsStableAcrossLimits(t *testing.T) {
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

		// More unlocated providers than the smallest tested batch reproduce the
		// production saturation boundary.
		unlocated := []server.Id{
			server.NewId(), server.NewId(), server.NewId(), server.NewId(),
		}
		healthWithoutLocation := server.NewId()
		crossedDeadlines := server.NewId()
		overlap := server.NewId()
		healthFirst := server.NewId()
		missingHealth := server.NewId()
		locationSecond := server.NewId()
		locationTie := server.NewId()
		healthTie := server.NewId()
		fresh := server.NewId()
		attempted := server.NewId()
		staleAttempted := server.NewId()
		nonPublic := server.NewId()

		address := 0
		connectProvider := func(clientId server.Id, provideMode ProvideMode) {
			address += 1
			testing_connectProbeableProvider(
				t, ctx, clientId, city.LocationId,
				fmt.Sprintf("192.0.2.%d:0", address), provideMode,
			)
		}
		for _, clientId := range unlocated {
			connectProvider(clientId, ProvideModePublic)
		}
		connectProvider(healthWithoutLocation, ProvideModePublic)
		for _, clientId := range []server.Id{
			crossedDeadlines, overlap, healthFirst, missingHealth, locationSecond, locationTie, healthTie,
		} {
			connectProvider(clientId, ProvideModePublic)
		}
		connectProvider(fresh, ProvideModePublic)
		connectProvider(attempted, ProvideModePublic)
		connectProvider(staleAttempted, ProvideModePublic)
		connectProvider(nonPublic, ProvideModeNetwork)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		// Location cleanup deliberately does not delete the independently useful
		// health history. Such a provider belongs to the no-location lane, not
		// stale health, even when its retained health row is the oldest evidence
		// in the database.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: healthWithoutLocation, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-30 * 24 * time.Hour),
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: healthWithoutLocation, MeasuredAt: now.Add(-30 * 24 * time.Hour),
			OKCount: 1, Total: 1,
		})
		RemoveExpiredProviderEgressLocations(ctx, now.Add(-28*24*time.Hour))
		if GetProviderEgressLocation(ctx, healthWithoutLocation) != nil {
			t.Fatal("expired location must be removed for the health-without-location control")
		}
		if GetProviderEgressHealth(ctx, healthWithoutLocation) == nil {
			t.Fatal("location cleanup must retain independent health evidence")
		}
		unlocated = append(unlocated, healthWithoutLocation)

		// Hard deadlines, in order:
		// crossed health (-1h), overlap health (+5m), healthFirst (+10m), missing health (+15m),
		// locationSecond (+20m), then locationTie/healthTie (+30m,
		// client_id tie-break).
		locationTimes := map[server.Id]time.Time{
			crossedDeadlines: now.Add(-90 * time.Hour),
			overlap:          now.Add(-ProviderEgressLocationMaxAge + 40*time.Minute),
			locationSecond:   now.Add(-ProviderEgressLocationMaxAge + 20*time.Minute),
			locationTie:      now.Add(-ProviderEgressLocationMaxAge + 30*time.Minute),
		}
		for clientId, observedAt := range locationTimes {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, LocationId: city.LocationId,
				CountryCode: "us", ObservedAt: observedAt,
			})
		}
		healthTimes := map[server.Id]time.Time{
			// Both crossedDeadlines lanes are due, but its 25-hour-old
			// health expires before its 90-hour-old location. The merge and
			// monitor category must follow that earlier absolute deadline.
			crossedDeadlines: now.Add(-25 * time.Hour),
			overlap:          now.Add(-ProviderEgressHealthMaxAge + 5*time.Minute),
			healthFirst:      now.Add(-ProviderEgressHealthMaxAge + 10*time.Minute),
			healthTie:        now.Add(-ProviderEgressHealthMaxAge + 30*time.Minute),
		}
		for clientId, measuredAt := range healthTimes {
			if clientId != overlap && clientId != crossedDeadlines {
				SetProviderEgressLocation(ctx, &ProviderEgressLocation{
					ClientId: clientId, LocationId: city.LocationId,
					CountryCode: "us", ObservedAt: now.Add(-time.Hour),
				})
			}
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId: clientId, MeasuredAt: measuredAt,
				OKCount: 1, Total: 1,
			})
		}
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: missingHealth, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-ProviderEgressHealthMaxAge + 15*time.Minute),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: staleAttempted, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-200 * time.Hour),
		})
		for _, clientId := range []server.Id{locationSecond, locationTie, fresh, staleAttempted} {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId: clientId, MeasuredAt: now,
				OKCount: 1, Total: 1,
			})
		}
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: attempted, AttemptAt: now.Add(-5 * time.Second),
			ProbeFailure: "tunnel_failed",
		})
		// oldest observed_at of all, so it would sort to the head of the
		// stale group if the backoff did not exclude it
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: staleAttempted, AttemptAt: now.Add(-5 * time.Second),
			ProbeFailure: "tunnel_failed",
		})

		minObservedAt := now.Add(-ProviderEgressLocationMaxAge / 2)
		minAttemptAt := now.Add(-ProviderEgressProbeAttemptBackoff)

		tied := []server.Id{locationTie, healthTie}
		slices.SortFunc(tied, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		expected := []server.Id{crossedDeadlines, overlap, healthFirst, missingHealth, locationSecond}
		expected = append(expected, tied...)
		slices.SortFunc(unlocated, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		expected = append(expected, unlocated...)

		due := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 100)
		if !slices.Equal(due, expected) {
			t.Fatalf("due = %v, want EDF urgent rows then unlocated rows %v", due, expected)
		}
		overlapCount := 0
		for _, clientId := range due {
			if clientId == overlap {
				overlapCount += 1
			}
		}
		if overlapCount != 1 {
			t.Fatalf("overlapping location/health candidate appears %d times, want once", overlapCount)
		}

		// Every limit is the exact prefix of the same stable ordering. Limits 1
		// through 4 are all smaller than the saturated unlocated backlog and
		// therefore fail under the former fixed-pass precedence.
		for limit := 0; limit <= len(expected)+2; limit += 1 {
			want := expected[:min(limit, len(expected))]
			got := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, limit)
			if !slices.Equal(got, want) {
				t.Errorf("limit %d: due = %v, want %v", limit, got, want)
			}
		}
	})
}

func TestProviderEgressStaleHealthDueQueryRequiresLocation(t *testing.T) {
	normalized := strings.Join(strings.Fields(providerEgressStaleHealthDueQuery), " ")
	for _, want := range []string{
		"FROM provider_egress_health INNER JOIN provider_egress_location ON provider_egress_location.client_id = provider_egress_health.client_id",
		"ORDER BY provider_egress_health.measured_at ASC, provider_egress_health.client_id ASC LIMIT $4",
	} {
		if !strings.Contains(normalized, want) {
			t.Fatalf("stale-health query lacks %q: %s", want, normalized)
		}
	}
}

func TestGetProviderEgressLocationDueAdvancesAfterAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Synthetic City",
			Region:       "Synthetic Region",
			Country:      "Synthetic Country",
			CountryCode:  "zz",
		}
		CreateLocation(ctx, city)

		first := server.NewId()
		second := server.NewId()
		for i, clientId := range []server.Id{first, second} {
			testing_connectProbeableProvider(
				t, ctx, clientId, city.LocationId,
				fmt.Sprintf("192.0.2.%d:0", i+1), ProvideModePublic,
			)
		}
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: first, LocationId: city.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-ProviderEgressLocationMaxAge + time.Minute),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: second, LocationId: city.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-ProviderEgressLocationMaxAge + 2*time.Minute),
		})
		for _, clientId := range []server.Id{first, second} {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId: clientId, MeasuredAt: now,
				OKCount: 1, Total: 1,
			})
		}

		minObservedAt := now.Add(-ProviderEgressLocationMaxAge / 2)
		minAttemptAt := now.Add(-ProviderEgressProbeAttemptBackoff)
		if got := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 1); !slices.Equal(got, []server.Id{first}) {
			t.Fatalf("first due = %v, want earliest deadline %s", got, first)
		}

		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: first, AttemptAt: now, ProbeFailure: "synthetic_failure",
		})
		if got := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 1); !slices.Equal(got, []server.Id{second}) {
			t.Fatalf("second due = %v, want next deadline %s after recording the first attempt", got, second)
		}
	})
}

func TestProviderEgressHealthDeadlineIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var definition string
		var valid, ready, nonPartial bool
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT
					regexp_replace(pg_get_indexdef(index_relation.oid), '[[:space:]]+', ' ', 'g'),
					index_record.indisvalid,
					index_record.indisready,
					index_record.indpred IS NULL
				FROM pg_index AS index_record
				JOIN pg_class AS index_relation ON index_relation.oid = index_record.indexrelid
				WHERE index_relation.oid = to_regclass('provider_egress_health_measured_at_client_id')
			`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&definition, &valid, &ready, &nonPartial))
				}
			})
		})
		const expected = "CREATE INDEX provider_egress_health_measured_at_client_id ON public.provider_egress_health USING btree (measured_at, client_id)"
		if definition != expected || !valid || !ready || !nonPartial {
			t.Fatalf("stale-health deadline index = (%q, valid=%t, ready=%t, non_partial=%t), want exact valid/ready non-partial definition %q", definition, valid, ready, nonPartial, expected)
		}
	})
}

// TestProviderEgressLocationHasVerdictColumns asserts the three verdict columns
// exist. They are additive with safe defaults, so no existing reader or row is
// affected -- but nothing can record a verdict until they are there.
func TestProviderEgressLocationHasVerdictColumns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, col := range []string{"verdict", "verdict_reason", "assurance"} {
			var exists bool
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT EXISTS (
						SELECT 1 FROM information_schema.columns
						WHERE table_name = 'provider_egress_location' AND column_name = $1
					)
					`,
					col,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&exists))
					}
				})
			})
			if !exists {
				t.Errorf("provider_egress_location missing column %q", col)
			}
		}
	})
}

// TestProviderEgressLocationVerdictDefaults pins the write path's normalization.
// SetProviderEgressLocation names every column explicitly, which bypasses the
// column defaults, so a caller that computes no judgement -- every caller until
// the ingest path does -- must still store unverified/direct, not the empty
// string.
func TestProviderEgressLocationVerdictDefaults(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		observedAt := server.NowUtc()

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  server.NewId(),
			CountryCode: "es",
			ObservedAt:  observedAt,
		})

		stored := GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected a stored egress location")
		}
		connect.AssertEqual(t, stored.Verdict, ProviderEgressVerdictUnverified)
		connect.AssertEqual(t, stored.VerdictReason, "")
		connect.AssertEqual(t, stored.Assurance, ProviderEgressAssuranceDirect)

		// an explicit judgement is stored verbatim
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:      clientId,
			LocationId:    server.NewId(),
			CountryCode:   "de",
			ObservedAt:    observedAt.Add(time.Hour),
			Verdict:       "suspect",
			VerdictReason: "unstable",
			Assurance:     ProviderEgressAssuranceDirect,
		})

		stored = GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected a stored egress location")
		}
		connect.AssertEqual(t, stored.Verdict, "suspect")
		connect.AssertEqual(t, stored.VerdictReason, "unstable")
		connect.AssertEqual(t, stored.Assurance, ProviderEgressAssuranceDirect)
	})
}

func TestGetAllProviderEgressCountryCodes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		observed := server.NewId()
		unobserved := server.NewId()
		stale := server.NewId()

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    observed,
			CountryCode: "GB",
			Verdict:     "verified",
			ObservedAt:  server.NowUtc(),
		})
		// probed once, well outside the trust window. The sweeper deliberately
		// retains rows far longer than ProviderEgressLocationMaxAge because
		// "reads already ignore stale rows" (see
		// taskworker/work/provider_egress_location_work.go), so a row like this
		// really does sit in the table -- and this bulk read is one of the
		// reads that has to ignore it. Unbounded, it would keep counting this
		// provider as supply for a country it may have left weeks ago.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    stale,
			CountryCode: "GB",
			Verdict:     "verified",
			ObservedAt:  server.NowUtc().Add(-ProviderEgressLocationMaxAge - time.Hour),
		})

		codes := GetAllProviderEgressCountryCodes(ctx)

		// observed providers come back lowercased, so callers can compare
		// against location.country_code without normalising at each site
		connect.AssertEqual(t, codes[observed], "gb")

		// a provider with no observed location is ABSENT, not "".
		// Callers rely on the two-value lookup to fail closed.
		_, ok := codes[unobserved]
		connect.AssertEqual(t, ok, false)

		// a provider whose only observation has aged out is ABSENT too --
		// indistinguishable from never probed, which fails closed
		_, ok = codes[stale]
		connect.AssertEqual(t, ok, false)
	})
}

// A provider whose egress HEALTH has aged out must be re-offered even though
// its location is still fresh.
//
// The two ages are independent and only one of them gated re-probing. Health
// decides whether a provider is published (providerCountFilter.passesHealth),
// but both of the original passes keyed off provider_egress_location, which is
// trusted seven times longer. A provider probed once and then quietly going
// dark kept its passing tally, was never re-offered, and stayed in the public
// list for days -- observed on beta as 12 of 12 sampled providers answering
// ok=0/131 while still advertised.
func TestGetProviderEgressLocationDueOffersStaleHealth(t *testing.T) {
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

		staleHealth := server.NewId()
		freshHealth := server.NewId()

		testing_connectProbeableProvider(t, ctx, staleHealth, city.LocationId, "192.0.2.1:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, freshHealth, city.LocationId, "192.0.2.2:0", ProvideModePublic)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		// BOTH locations are fresh, so neither is due under the location-driven
		// passes. Health age is the only variable.
		for _, clientId := range []server.Id{staleHealth, freshHealth} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, LocationId: city.LocationId,
				CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
			})
		}

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   staleHealth,
			MeasuredAt: now.Add(-ProviderEgressHealthMaxAge),
			OKCount:    100, Total: 100,
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   freshHealth,
			MeasuredAt: now.Add(-1 * time.Minute),
			OKCount:    100, Total: 100,
		})

		due := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), now, 100)

		if !slices.Contains(due, staleHealth) {
			t.Errorf("due = %v, must contain the provider whose health aged out (%s): "+
				"its tally still gates the public list, so leaving it unmeasured advertises a provider nothing has checked in over %s",
				due, staleHealth, ProviderEgressHealthMaxAge)
		}
		if slices.Contains(due, freshHealth) {
			t.Errorf("due = %v, must not contain the provider measured a minute ago (%s): "+
				"re-probing on fresh evidence spends the queue on providers that do not need it",
				due, freshHealth)
		}
	})
}

// A location report can succeed while the independent health check is skipped
// or its non-fatal report fails. Publication fails closed on that missing row,
// so the scheduler must retry it through an urgent, attempt-backed-off lane
// rather than waiting for the seven-day location lifecycle.
func TestGetProviderEgressLocationDueOffersMissingHealthAfterAttemptBackoff(t *testing.T) {
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

		missingUnattempted := server.NewId()
		missingRecentlyAttempted := server.NewId()
		healthy := server.NewId()
		for i, clientId := range []server.Id{missingUnattempted, missingRecentlyAttempted, healthy} {
			testing_connectProbeableProvider(
				t, ctx, clientId, city.LocationId,
				fmt.Sprintf("192.0.2.%d:0", i+1), ProvideModePublic,
			)
		}
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		for _, clientId := range []server.Id{missingUnattempted, missingRecentlyAttempted, healthy} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, LocationId: city.LocationId,
				CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
			})
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: healthy, MeasuredAt: now,
			OKCount: 1, Total: 1,
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: missingRecentlyAttempted, AttemptAt: now,
			ProbeFailure: "synthetic_health_report_failure",
		})

		due := GetProviderEgressLocationDue(
			ctx, now.Add(-ProviderEgressLocationMaxAge/2),
			now.Add(-ProviderEgressProbeAttemptBackoff), 100,
		)
		if !slices.Contains(due, missingUnattempted) {
			t.Errorf("due = %v, must contain the fresh-location provider with no health row (%s)", due, missingUnattempted)
		}
		if slices.Contains(due, missingRecentlyAttempted) {
			t.Errorf("due = %v, must defer the missing-health provider attempted moments ago (%s)", due, missingRecentlyAttempted)
		}
		if slices.Contains(due, healthy) {
			t.Errorf("due = %v, must not contain the provider with fresh location and health (%s)", due, healthy)
		}

		afterBackoff := GetProviderEgressLocationDue(
			ctx, now.Add(-ProviderEgressLocationMaxAge/2), now.Add(time.Second), 100,
		)
		if !slices.Contains(afterBackoff, missingRecentlyAttempted) {
			t.Errorf("due = %v, must contain the missing-health provider after its attempt backoff (%s)", afterBackoff, missingRecentlyAttempted)
		}
	})
}
