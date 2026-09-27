package controller

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	stconn "github.com/urnetwork/server/v2026/st"
)

// Testnet has no external USD market; only a fully identified mainnet subnet
// may generate a GeckoTerminal lookup.
func TestStatsAlphaPriceURLIsMainnetOnly(t *testing.T) {
	if url := statsAlphaPriceURL(&StConfig{Profile: stconn.ProfileTestnet, Netuid: 521}); url != "" {
		t.Fatalf("testnet alpha price URL = %q", url)
	}
	if url := statsAlphaPriceURL(&StConfig{Profile: stconn.ProfileMainnet}); url != "" {
		t.Fatalf("zero-netuid alpha price URL = %q", url)
	}
	want := "https://api.geckoterminal.com/api/v2/networks/bittensor/pools/0-521"
	if url := statsAlphaPriceURL(&StConfig{Profile: stconn.ProfileMainnet, Netuid: 521}); url != want {
		t.Fatalf("mainnet alpha price URL = %q, want %q", url, want)
	}
}

// the per-country provider gauge is pushed by the taskworker until the
// process exits, so a country that lost its last provider must have its
// series deleted on the next replace, not left pushing its final count
// forever. registration is lazy like the scalar stats gauges, so a vec that
// was never replaced is never exported
func TestStatsGaugeVecReplaceDeletesStaleSeries(t *testing.T) {
	vec := newStatsGaugeVec(
		"test_replace_deletes_stale_series",
		"test",
		"country_code",
		"country",
	)
	name := "urnetwork_stats_test_replace_deletes_stale_series"

	if vec.registered {
		t.Fatal("a stats gauge vec must not register before its first replace")
	}

	vec.replace([]statsLabeledValue{
		{labelValues: []string{"AU", "Australia"}, value: 3},
		{labelValues: []string{"JP", "Japan"}, value: 1},
	})
	if !vec.registered {
		t.Fatal("the first replace must register the vec")
	}
	if count := testutil.CollectAndCount(vec.gauge, name); count != 2 {
		t.Fatalf("series after first replace = %d, want 2", count)
	}
	if value := testutil.ToFloat64(vec.gauge.WithLabelValues("AU", "Australia")); value != 3 {
		t.Fatalf("AU = %f, want 3", value)
	}

	// JP loses its last provider and DE gains one: JP's series must go
	// away, AU must update in place, DE must appear
	vec.replace([]statsLabeledValue{
		{labelValues: []string{"AU", "Australia"}, value: 4},
		{labelValues: []string{"DE", "Germany"}, value: 2},
	})
	if count := testutil.CollectAndCount(vec.gauge, name); count != 2 {
		t.Fatalf("series after second replace = %d, want 2 (JP deleted, DE added)", count)
	}
	if value := testutil.ToFloat64(vec.gauge.WithLabelValues("AU", "Australia")); value != 4 {
		t.Fatalf("AU = %f, want 4", value)
	}
	if value := testutil.ToFloat64(vec.gauge.WithLabelValues("DE", "Germany")); value != 2 {
		t.Fatalf("DE = %f, want 2", value)
	}
	// reading JP through WithLabelValues would recreate it; check the
	// tracked set instead
	if _, ok := vec.current[statsLabelKey([]string{"JP", "Japan"})]; ok {
		t.Fatal("JP must be dropped from the tracked series")
	}

	// an empty replace deletes everything and keeps the vec registered
	vec.replace(nil)
	if count := testutil.CollectAndCount(vec.gauge, name); count != 0 {
		t.Fatalf("series after empty replace = %d, want 0", count)
	}
	if !vec.registered {
		t.Fatal("an empty replace must not unregister the vec")
	}

	// the vec is registered with the default registry, like every stats
	// gauge, so the pusher exports it
	prometheus.DefaultRegisterer.Unregister(vec.gauge)
}

// The extender and contract gauges (connect/EXTENDER.md M4).
//
// The collector's db refresh is driven against seeded rows and the exported
// series are read back, so the gauge names, the label sets and the values are
// all pinned at once. Addresses are RFC 5737 and RFC 3849 documentation
// addresses and every id is generated.

// One extender of the given families, located in country, stored as an
// activation would have stored it.
func testStatsExtender(
	ctx context.Context,
	name string,
	location *model.Location,
	countryCode string,
	ips ...string,
) *model.NetworkExtender {
	addresses := []*model.NetworkExtenderAddress{}
	for _, ip := range ips {
		addr := netip.MustParseAddr(ip)
		addresses = append(addresses, &model.NetworkExtenderAddress{
			IpVersion:    server.IpVersionForAddr(addr),
			Ip:           addr,
			Carriers:     []string{connect.ExtenderCarrierTcp},
			ActivateTime: server.NowUtc(),
			Active:       true,
		})
	}
	extender := &model.NetworkExtender{
		ExtenderId:  server.NewId(),
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   []byte("stats-collector-" + name),
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: countryCode,
		Active:      true,
	}
	if location != nil {
		extender.LocationId = &location.LocationId
		extender.CountryLocationId = &location.CountryLocationId
		if (location.RegionLocationId != server.Id{}) {
			extender.RegionLocationId = &location.RegionLocationId
		}
		if (location.CityLocationId != server.Id{}) {
			extender.CityLocationId = &location.CityLocationId
		}
	}
	model.Testing_CreateNetworkExtender(ctx, extender, addresses)
	return extender
}

// One contract with an exact create_time, optionally disputed and optionally
// with an extender party.
func testStatsContract(
	ctx context.Context,
	createTime time.Time,
	dispute bool,
	withExtender bool,
) {
	contractId := server.NewId()
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id,
				source_network_id,
				source_id,
				destination_network_id,
				destination_id,
				transfer_byte_count,
				create_time,
				dispute
			)
			VALUES ($1, $2, $3, $4, $5, 1024, $6, $7)
			`,
			contractId,
			server.NewId(),
			server.NewId(),
			server.NewId(),
			server.NewId(),
			createTime.UTC(),
			dispute,
		))
		if withExtender {
			server.RaisePgResult(conn.Exec(
				ctx,
				`
				INSERT INTO contract_extender (
					contract_id,
					extender_id,
					party,
					client_id,
					network_id,
					create_time
				)
				VALUES ($1, $2, $3, $4, $5, $6)
				`,
				contractId,
				server.NewId(),
				model.ContractPartySource,
				server.NewId(),
				server.NewId(),
				createTime.UTC(),
			))
		}
	})
}

func testStatsGaugeValue(t testing.TB, gauge *statsGauge) float64 {
	t.Helper()
	if !gauge.registered {
		t.Fatal("the gauge was never set, so it is never exported")
	}
	return testutil.ToFloat64(gauge.gauge)
}

func testStatsFamilyValue(t testing.TB, vec *statsGaugeVec, ipFamily string) float64 {
	t.Helper()
	return testutil.ToFloat64(vec.gauge.WithLabelValues(ipFamily))
}

// M4: the db refresh exports the extender population, both family splits and
// the six contract gauges, with a series per family whether or not the family
// has any members.
func TestStatsRefreshDbExportsExtenderAndContractGauges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sydney := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Sydney",
			Region:       "New South Wales",
			Country:      "Australia",
			CountryCode:  "au",
		}
		model.CreateLocation(ctx, sydney)
		tokyo := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Tokyo",
			Region:       "Tokyo",
			Country:      "Japan",
			CountryCode:  "jp",
		}
		model.CreateLocation(ctx, tokyo)

		// australia: one dualstack and one v4-only; japan: one v4-only
		testStatsExtender(ctx, "au-dual", sydney, "au", "192.0.2.50", "2001:db8::50")
		testStatsExtender(ctx, "au-v4", sydney, "au", "192.0.2.51")
		testStatsExtender(ctx, "jp-v4", tokyo, "jp", "192.0.2.52")

		// a provider population with a proven family each, so the provider
		// family gauge has something to publish beside the extenders
		addProvider := func(location *model.Location, ipv4Proven bool, ipv6Proven bool) {
			clientId := server.NewId()
			networkId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(
					ctx,
					`
					INSERT INTO network_client_location_reliability (
						client_id,
						network_id,
						update_block_number,
						region_location_id,
						country_location_id,
						client_address_hash_count,
						location_count,
						connected,
						ipv4_proven,
						ipv6_proven
					)
					VALUES ($1, $2, 1, $3, $4, 1, 1, true, $5, $6)
					`,
					clientId,
					networkId,
					location.RegionLocationId,
					location.CountryLocationId,
					ipv4Proven,
					ipv6Proven,
				))
			})
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModePublic: []byte("public-secret"),
			})
		}
		addProvider(sydney, true, true)
		addProvider(sydney, true, false)
		addProvider(tokyo, false, false)

		// contracts in the current partial hour bucket, which is always
		// counted live: two open, one of them with an extender party, and one
		// open dispute
		now := server.NowUtc()
		testStatsContract(ctx, now, false, false)
		testStatsContract(ctx, now, false, true)
		testStatsContract(ctx, now, true, false)

		statsRefreshDb(ctx)

		if value := testStatsGaugeValue(t, statsOnlineExtendersGauge); value != 3 {
			t.Fatalf("online_extenders = %f, want 3", value)
		}

		// the per-country gauge replaces its whole label set, like providers
		if count := testutil.CollectAndCount(
			statsOnlineExtendersByCountryGauge.gauge,
			"urnetwork_stats_online_extenders_by_country",
		); count != 2 {
			t.Fatalf("online_extenders_by_country series = %d, want 2", count)
		}
		if value := testutil.ToFloat64(
			statsOnlineExtendersByCountryGauge.gauge.WithLabelValues("AU", "Australia"),
		); value != 2 {
			t.Fatalf("online_extenders_by_country{AU} = %f, want 2", value)
		}
		if value := testutil.ToFloat64(
			statsOnlineExtendersByCountryGauge.gauge.WithLabelValues("JP", "Japan"),
		); value != 1 {
			t.Fatalf("online_extenders_by_country{JP} = %f, want 1", value)
		}

		// both family gauges publish all three series every refresh, so a
		// family with no members is a zero and never an absence
		for name, vec := range map[string]*statsGaugeVec{
			"urnetwork_stats_online_providers_by_ip_family": statsOnlineProvidersByIpFamilyGauge,
			"urnetwork_stats_online_extenders_by_ip_family": statsOnlineExtendersByIpFamilyGauge,
		} {
			if count := testutil.CollectAndCount(vec.gauge, name); count != 3 {
				t.Fatalf("%s series = %d, want 3", name, count)
			}
		}
		if value := testStatsFamilyValue(t, statsOnlineExtendersByIpFamilyGauge, "dualstack"); value != 1 {
			t.Fatalf("online_extenders_by_ip_family{dualstack} = %f, want 1", value)
		}
		if value := testStatsFamilyValue(t, statsOnlineExtendersByIpFamilyGauge, "ipv4"); value != 2 {
			t.Fatalf("online_extenders_by_ip_family{ipv4} = %f, want 2", value)
		}
		if value := testStatsFamilyValue(t, statsOnlineExtendersByIpFamilyGauge, "ipv6"); value != 0 {
			t.Fatalf("online_extenders_by_ip_family{ipv6} = %f, want an exported zero", value)
		}
		if value := testStatsFamilyValue(t, statsOnlineProvidersByIpFamilyGauge, "dualstack"); value != 1 {
			t.Fatalf("online_providers_by_ip_family{dualstack} = %f, want 1", value)
		}
		// a v4-only proven row and a row written before the proven columns
		// existed both read as ipv4
		if value := testStatsFamilyValue(t, statsOnlineProvidersByIpFamilyGauge, "ipv4"); value != 2 {
			t.Fatalf("online_providers_by_ip_family{ipv4} = %f, want 2", value)
		}
		if value := testStatsFamilyValue(t, statsOnlineProvidersByIpFamilyGauge, "ipv6"); value != 0 {
			t.Fatalf("online_providers_by_ip_family{ipv6} = %f, want an exported zero", value)
		}

		if value := testStatsGaugeValue(t, statsOpenContractsGauge); value != 2 {
			t.Fatalf("open_contracts = %f, want 2", value)
		}
		if value := testStatsGaugeValue(t, statsOpenContractsWithExtenderGauge); value != 1 {
			t.Fatalf("open_contracts_with_extender = %f, want 1", value)
		}
		if value := testStatsGaugeValue(t, statsOpenDisputesGauge); value != 1 {
			t.Fatalf("open_disputes = %f, want 1", value)
		}
		if value := testStatsGaugeValue(t, statsContracts24hGauge); value != 3 {
			t.Fatalf("contracts_24h = %f, want 3 (the dispute is a contract too)", value)
		}
		if value := testStatsGaugeValue(t, statsContractsWithExtender24hGauge); value != 1 {
			t.Fatalf("contracts_with_extender_24h = %f, want 1", value)
		}
		if value := testStatsGaugeValue(t, statsDisputes24hGauge); value != 1 {
			t.Fatalf("disputes_24h = %f, want 1", value)
		}
	})
}

// An empty population still publishes all three family series and a zero
// total, because a gauge that has been set once must never go silent on the
// refresh that finds nothing (M4).
func TestStatsIpFamilyValuesAlwaysPublishEveryFamily(t *testing.T) {
	values := statsIpFamilyValues(0, 0, 0)
	if len(values) != 3 {
		t.Fatalf("family series = %d, want 3", len(values))
	}
	for index, ipFamily := range []string{"ipv4", "ipv6", "dualstack"} {
		value := values[index]
		if len(value.labelValues) != 1 || value.labelValues[0] != ipFamily {
			t.Fatalf("series %d = %v, want %s", index, value.labelValues, ipFamily)
		}
		if value.value != 0 {
			t.Fatalf("%s = %f, want an exported zero", ipFamily, value.value)
		}
	}

	counted := statsIpFamilyValues(7, 3, 11)
	want := map[string]float64{"ipv4": 7, "ipv6": 3, "dualstack": 11}
	for _, value := range counted {
		if value.value != want[value.labelValues[0]] {
			t.Fatalf("%s = %f, want %f", value.labelValues[0], value.value, want[value.labelValues[0]])
		}
	}
}
