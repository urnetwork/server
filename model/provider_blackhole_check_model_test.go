package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A current failing check must override a passing health measurement.
//
// This is the whole reason the check exists. Egress health sweeps the fleet
// over hours to days, so a provider that goes dark keeps its passing tally --
// and its place in the public list -- until the next sweep reaches it. The
// hourly check closes that window.
func TestBlackholedProviderFailsTheHealthGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		healthy := server.NewId()
		blackholed := server.NewId()

		for _, clientId := range []server.Id{healthy, blackholed} {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId:   clientId,
				MeasuredAt: now.Add(-time.Hour),
				OKCount:    100, Total: 100,
			})
		}

		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: blackholed, CheckedAt: now.Add(-time.Minute),
			OK: false, Failure: "all_destinations_failed",
		})
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: healthy, CheckedAt: now.Add(-time.Minute), OK: true,
		})

		f := newProviderCountFilter(ctx, true)

		if !f.passesHealth(healthy) {
			t.Errorf("a provider measured healthy and checked ok must pass the gate")
		}
		if f.passesHealth(blackholed) {
			t.Errorf("a provider whose current check says nothing got through must NOT pass the gate, " +
				"even with a passing health measurement -- that combination is exactly a provider that went dark since it was last swept")
		}
	})
}

// A check that has aged out must NOT be read as "blackholed".
//
// The signal can only ever remove providers, so when its evidence lapses the
// provider falls back to being judged on egress health alone. Treating "not
// checked recently" as "dark" would empty the list the moment the sweep
// stalled.
func TestStaleBlackholeCheckDoesNotExclude(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		clientId := server.NewId()
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   clientId,
			MeasuredAt: now.Add(-time.Hour),
			OKCount:    100, Total: 100,
		})
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId:  clientId,
			CheckedAt: now.Add(-ProviderBlackholeCheckMaxAge - time.Minute),
			OK:        false, Failure: "all_destinations_failed",
		})

		if !newProviderCountFilter(ctx, true).passesHealth(clientId) {
			t.Errorf("a failing check older than %s must not keep excluding the provider: "+
				"a stalled sweep would otherwise drain the list", ProviderBlackholeCheckMaxAge)
		}
	})
}

// The upsert is monotonic: an out-of-order or replayed report must not move a
// provider's last-checked time backwards, which would hand it straight back to
// the sweep and, worse, could resurrect a stale verdict over a current one.
func TestSetProviderBlackholeCheckIsMonotonic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		clientId := server.NewId()

		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: clientId, CheckedAt: now.Add(-time.Minute), OK: true,
		})
		// an older report arriving late
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: clientId, CheckedAt: now.Add(-time.Hour),
			OK: false, Failure: "tunnel_failed",
		})

		c := GetProviderBlackholeCheck(ctx, clientId)
		connect.AssertNotEqual(t, c, nil)
		if !c.OK {
			t.Errorf("a report older than the stored one overwrote it: ok=%v failure=%q checked_at=%s",
				c.OK, c.Failure, c.CheckedAt)
		}
	})
}

// The due query offers never-checked providers first, then the least recently
// checked, and never offers one checked inside the window.
func TestGetProviderBlackholeCheckDue(t *testing.T) {
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

		never := server.NewId()
		stale := server.NewId()
		fresh := server.NewId()

		testing_connectProbeableProvider(t, ctx, never, city.LocationId, "0.0.0.1:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, stale, city.LocationId, "0.0.0.2:0", ProvideModePublic)
		testing_connectProbeableProvider(t, ctx, fresh, city.LocationId, "0.0.0.3:0", ProvideModePublic)
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: stale, CheckedAt: now.Add(-24 * time.Hour), OK: true,
		})
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: fresh, CheckedAt: now.Add(-time.Minute), OK: true,
		})

		due := GetProviderBlackholeCheckDue(ctx, now.Add(-ProviderBlackholeCheckDueAge), 100, 0, 1)

		has := func(id server.Id) bool {
			for _, d := range due {
				if d == id {
					return true
				}
			}
			return false
		}
		if !has(never) {
			t.Errorf("due = %v, must contain the never-checked provider %s", due, never)
		}
		if !has(stale) {
			t.Errorf("due = %v, must contain the provider checked a day ago %s", due, stale)
		}
		if has(fresh) {
			t.Errorf("due = %v, must not contain the provider checked a minute ago %s: "+
				"re-checking inside the window spends the sweep on providers that do not need it", due, fresh)
		}
		// a failing check carries NO backoff -- that is how a recovered provider
		// gets back into the list -- so ordering is by age alone
		if 0 < len(due) && due[0] != never {
			t.Errorf("due[0] = %s, want the never-checked provider %s first", due[0], never)
		}
	})
}

// A materialized connected row can outlive the client state that made it
// eligible. The blackhole sweep must spend its bounded slots only on active
// top-level providers, never derivative return-traffic identities or inactive
// clients that still retain a Public key.
func TestGetProviderBlackholeCheckDueExcludesDerivedAndInactiveClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
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
		activeClientId := testingCreateProviderClient(ctx, networkId, nil, true)
		derivedClientId := testingCreateProviderClient(ctx, networkId, &sourceClientId, true)
		inactiveClientId := testingCreateProviderClient(ctx, networkId, nil, false)
		for _, clientId := range []server.Id{activeClientId, derivedClientId, inactiveClientId} {
			testingInsertProviderLocationReliability(ctx, clientId, networkId, city)
		}

		due := GetProviderBlackholeCheckDue(ctx, server.NowUtc(), 100, 0, 1)
		if !slices.Contains(due, activeClientId) {
			t.Errorf("due = %v, missing active top-level provider %s", due, activeClientId)
		}
		for _, clientId := range []server.Id{derivedClientId, inactiveClientId} {
			if slices.Contains(due, clientId) {
				t.Errorf("due = %v, contains derived or inactive provider %s", due, clientId)
			}
		}
	})
}
