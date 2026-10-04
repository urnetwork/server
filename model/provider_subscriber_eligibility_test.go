package model

import (
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// Scoped resource overlays preserve the fixture's other provider settings.
// There is no process-global policy flag to leak between tests.
func enableSubscriberQualityPolicy(t testing.TB) {
	t.Helper()
	config := map[string]any{}
	if resource, err := server.Config.SimpleResource(providerConfigResourceName); err == nil && resource != nil {
		config, err = resource.ParseE()
		if err != nil {
			t.Fatal(err)
		}
	}
	if config == nil {
		config = map[string]any{}
	}
	config["subscriber_quality_policy_version"] = 2
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, encoded))
}

func TestQualityRequiresSubscriberAcrossNativeForceAndNamed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		cacheClock, cacheNow := subscriberCacheTestClock()
		previousCache := providerSubscriberNegativeCache
		providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, cacheNow)
		t.Cleanup(func() { providerSubscriberNegativeCache = previousCache })
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "United States", "us")
		verified := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		unknown := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		excluded := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinQualityVerified: true, ArinNonQuality: true})
		for _, provider := range []*egressTestProvider{verified, unknown, excluded} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestPasses(ctx, t)
		// A false legacy exception remains in storage; the rollup must exclude
		// it because no positive subscriber fact was recorded.
		server.Db(ctx, func(conn server.PgConn) {
			var nonQuality bool
			server.Raise(conn.QueryRow(ctx, `SELECT arin_non_quality FROM network_client_location_reliability WHERE client_id=$1`, unknown.clientId).Scan(&nonQuality))
			if !nonQuality {
				t.Fatal("legacy false exception entered native Quality")
			}
		})
		for _, force := range []bool{false, true} {
			for index, specs := range [][]*ProviderSpec{
				egressTestLocationSpec(city),
				{{ClientId: &verified.clientId}, {ClientId: &unknown.clientId}, {ClientId: &excluded.clientId}},
			} {
				providers := egressTestFind(ctx, t, specs, RankModeQuality, 10, force, server.NewId())
				if !force && index == 0 {
					if len(providers) != 3 || providers[0].ClientId != verified.clientId || providers[0].Tier != 0 {
						t.Fatal("Quality discovery lost native priority or lower Speed fallback")
					}
					for _, provider := range providers[1:] {
						if provider.Tier < egressTestBackfillOffset() {
							t.Fatal("unverified access entered the native Quality tier")
						}
					}
				} else if ids := egressTestIds(providers); !slices.Equal(ids, []server.Id{verified.clientId}) {
					t.Fatalf("force=%t: Quality returned unverified or excluded access", force)
				}
			}
		}
		if speed := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, false, server.NewId()); len(speed) != 3 {
			t.Fatal("subscriber requirement changed independent Speed eligibility")
		}
		// Add a new unknown connection after publication. Even a stale native
		// Quality cache and complete common exclusion snapshot must reject it.
		handler := CreateNetworkClientHandler(ctx)
		connection, _, _, _, err := ConnectNetworkClient(ctx, verified.clientId, "192.0.2.240:0", handler)
		if err != nil {
			t.Fatal(err)
		}
		if providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, true, server.NewId()); len(providers) != 0 {
			t.Fatal("missing location on a new live connection bypassed stale Quality cache")
		}
		// Expired handler generations must not poison a current subscriber,
		// but a new heartbeat makes the unclassified connection live again.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time=$2 WHERE handler_id=$1`, handler, server.NowUtc().Add(-3*NetworkClientHandlerHeartbeatTimeout)))
		})
		// A prior refusal may remain for at most one second from its read start.
		// Advance the cache clock; no real sleep or positive-cache bypass.
		cacheClock.Add(subscriberNegativeTTL.Nanoseconds())
		if providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, true, server.NewId()); len(providers) != 1 {
			t.Fatal("expired unknown handler blocked a current subscriber connection")
		}
		if err := HeartbeatNetworkClientHandler(ctx, handler); err != nil {
			t.Fatal(err)
		}
		if providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, true, server.NewId()); len(providers) != 0 {
			t.Fatal("fresh unknown handler reused an old subscriber decision")
		}
		if err := DisconnectNetworkClient(ctx, connection); err != nil {
			t.Fatal(err)
		}
		// A prior refusal may remain for at most one second from its read start.
		// Advance the cache clock; no real sleep or positive-cache bypass.
		cacheClock.Add(subscriberNegativeTTL.Nanoseconds())
		if providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, true, server.NewId()); len(providers) != 1 {
			t.Fatal("disconnected unknown history blocked current subscriber access")
		}
		if err := DisconnectNetworkClient(ctx, verified.connectionId); err != nil {
			t.Fatal(err)
		}
		if providers := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &verified.clientId}}, RankModeQuality, 1, true, server.NewId()); len(providers) != 0 {
			t.Fatal("no live subscriber connection was admitted by explicit ID")
		}
	})
}

// Merely deploying the new reader/writer/guard must preserve published behavior.
// Coordinated activation changes both the rollup and warm-cache request boundary.
func TestSubscriberQualityActivationDefaultsOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("{}\n")))
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "United States", "us")
		unknown := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		excluded := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinNonQuality: true})
		for _, provider := range []*egressTestProvider{unknown, excluded} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestPasses(ctx, t)
		assertRollup := func(want bool) {
			server.Db(ctx, func(conn server.PgConn) {
				var got bool
				server.Raise(conn.QueryRow(ctx, `SELECT arin_non_quality FROM network_client_location_reliability WHERE client_id=$1`, unknown.clientId).Scan(&got))
				if got != want {
					t.Fatalf("activation rollup=%t want=%t", got, want)
				}
			})
		}
		assertRollup(false)
		if found := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, false, server.NewId()); len(found) != 2 {
			t.Fatal("default-off policy changed legacy Quality/backfill behavior")
		}
		enableSubscriberQualityPolicy(t)
		if found := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, true, server.NewId()); len(found) != 0 {
			t.Fatal("explicit activation trusted stale legacy cache")
		}
		egressTestPasses(ctx, t)
		assertRollup(true)
		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("subscriber_quality_policy_version: 0\n")))
		egressTestPasses(ctx, t)
		assertRollup(false)
		if found := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, false, server.NewId()); len(found) != 2 {
			t.Fatal("explicit policy rollback failed to restore legacy interpretation")
		}
	})
}

func TestSubscriberQualityActivationRejectsInvalidPolicy(t *testing.T) {
	if enabled, err := subscriberQualityPolicyEnabledFromResource(nil, server.ErrResourceNotFound); enabled || err != nil {
		t.Fatal("an absent optional policy did not retain legacy behavior")
	}
	if _, err := subscriberQualityPolicyEnabledFromResource(nil, server.ErrResourceUnavailable); err == nil {
		t.Fatal("an unavailable policy silently disabled subscriber enforcement")
	}
	for _, row := range []struct {
		name, config     string
		enabled, invalid bool
	}{
		{name: "absent", config: "{}"},
		{name: "unrelated", config: "enable_egress_test: true"},
		{name: "zero", config: "subscriber_quality_policy_version: 0"},
		{name: "two", config: "subscriber_quality_policy_version: 2", enabled: true},
		{name: "one", config: "subscriber_quality_policy_version: 1", invalid: true},
		{name: "future", config: "subscriber_quality_policy_version: 3", invalid: true},
		{name: "malformed", config: "subscriber_quality_policy_version: [2]", invalid: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte(row.config)))
			enabled, err := subscriberQualityPolicyEnabled()
			if enabled != row.enabled || (err != nil) != row.invalid {
				t.Fatalf("activation=%t error=%v", enabled, err)
			}
		})
	}
}
