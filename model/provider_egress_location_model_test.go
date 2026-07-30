package model

import (
	"context"
	"fmt"
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

		// oldest first, so the longest-unprobed are probed first: the
		// never-probed provider sorts ahead of the three-days-stale one
		neverIndex := slices.Index(due, never)
		staleIndex := slices.Index(due, stale)
		if staleIndex < neverIndex {
			t.Fatalf("never-probed provider at %d must sort before the stale one at %d", neverIndex, staleIndex)
		}

		// limit is honoured
		limited := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), now, 1)
		if len(limited) != 1 {
			t.Fatalf("len(due) = %d for limit 1, want 1", len(limited))
		}
		if limited[0] != never {
			t.Fatalf("due[0] = %s for limit 1, want the never-probed provider %s", limited[0], never)
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

		testing_connectProbeableProvider(t, ctx, dead, city.LocationId, "0.0.0.1:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, healthyStale, city.LocationId, "0.0.0.2:0", ProvideModePublic)

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

		testing_connectProbeableProvider(t, ctx, good, city.LocationId, "0.0.0.1:0", ProvideModePublic)
		disconnectedConnectionId := testing_connectProbeableProvider(t, ctx, disconnected, city.LocationId, "0.0.0.2:0", ProvideModePublic)

		// `invalid` holds two simultaneous connections from two different
		// addresses, which makes client_address_hash_count = 2 and so the
		// generated `valid` column false. The two addresses must be in
		// different /29s: server.ClientIpHash buckets ipv4 to the /29 network,
		// so e.g. 0.0.0.3 and 0.0.0.4 would hash the same and count as one.
		testing_connectProbeableProvider(t, ctx, invalid, city.LocationId, "0.0.0.3:0", ProvideModePublic)
		secondHandlerId := CreateNetworkClientHandler(ctx)
		secondConnectionId, _, _, _, err := ConnectNetworkClient(ctx, invalid, "0.0.8.3:0", secondHandlerId)
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

		keep := server.NewId()
		drop := server.NewId()
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: keep, AttemptAt: server.NowUtc(),
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: drop, AttemptAt: server.NowUtc().Add(-30 * 24 * time.Hour),
		})

		RemoveExpiredProviderEgressProbeAttempts(ctx, server.NowUtc().Add(-24*time.Hour))

		if GetProviderEgressProbeAttempt(ctx, keep) == nil {
			t.Fatal("recent attempt must survive the sweep")
		}
		if GetProviderEgressProbeAttempt(ctx, drop) != nil {
			t.Fatal("old attempt must be swept")
		}
	})
}

// GetProviderEgressLocationDue is served by two statements -- never-probed
// first, then stale-but-probed only when the first came up short -- because the
// single-statement form sorts on observed_at from an outer-joined table, which
// cannot use an index and becomes a full scan plus an unindexable sort at 100k
// providers.
//
// The split is only safe if the concatenation is row-for-row what one statement
// returned, at every limit. That is what this asserts: it builds one population
// covering every eligibility case and then walks the limit from 0 past the end,
// requiring each result to be exactly the prefix of the full ordering. Limit 3
// is the seam (pass one exactly fills the batch) and limit 4 is the first that
// crosses into pass two.
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

		// three never-probed providers: the dominant group, and the reason the
		// ordering is NULLS FIRST
		never := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		// two stale ones, at different ages -- the older must be handed out first
		staleOlder := server.NewId()
		staleNewer := server.NewId()
		// probed an hour ago: not due
		fresh := server.NewId()
		// never probed, but attempted seconds ago: deferred by the backoff, and
		// the case that would otherwise starve the queue
		attempted := server.NewId()
		// probed long ago AND attempted seconds ago. This one is only screened
		// by the backoff predicate on the stale-but-probed pass -- the
		// never-probed pass never sees it, because it has an egress row. Drop
		// that predicate and it reappears in the batch.
		staleAttempted := server.NewId()
		// no Public provide key: unprobeable at any freshness
		nonPublic := server.NewId()

		address := 0
		connectProvider := func(clientId server.Id, provideMode ProvideMode) {
			address += 1
			testing_connectProbeableProvider(
				t, ctx, clientId, city.LocationId,
				fmt.Sprintf("0.0.%d.1:0", address), provideMode,
			)
		}
		for _, clientId := range never {
			connectProvider(clientId, ProvideModePublic)
		}
		connectProvider(staleOlder, ProvideModePublic)
		connectProvider(staleNewer, ProvideModePublic)
		connectProvider(fresh, ProvideModePublic)
		connectProvider(attempted, ProvideModePublic)
		connectProvider(staleAttempted, ProvideModePublic)
		connectProvider(nonPublic, ProvideModeNetwork)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: staleOlder, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-100 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: staleNewer, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-50 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: staleAttempted, LocationId: city.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-200 * time.Hour),
		})
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

		minObservedAt := now.Add(-24 * time.Hour)
		minAttemptAt := now.Add(-ProviderEgressProbeAttemptBackoff)

		// the never-probed group ties on a missing observed_at, so client_id
		// alone orders it -- and postgres orders uuid by bytes, which is what
		// server.Id.Cmp does
		expected := slices.Clone(never)
		slices.SortFunc(expected, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		// ... then the probed group, oldest probe first
		expected = append(expected, staleOlder, staleNewer)

		due := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 100)
		if !slices.Equal(due, expected) {
			t.Fatalf("due = %v, want %v (never-probed by client_id, then stale oldest-first; fresh/attempted/stale-attempted/non-public excluded)", due, expected)
		}

		// every limit must return exactly the prefix of that ordering. limit 3
		// is the pass-one/pass-two seam; 4 is the first to cross it.
		for limit := 0; limit <= len(expected)+2; limit += 1 {
			want := expected[:min(limit, len(expected))]
			got := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, limit)
			if !slices.Equal(got, want) {
				t.Errorf("limit %d: due = %v, want %v", limit, got, want)
			}
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
