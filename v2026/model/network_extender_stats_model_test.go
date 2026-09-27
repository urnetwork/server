package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The extender population counts the map and the gauges read
// (connect/EXTENDER.md M2, M4). The activation helpers they seed with live in
// network_extender_location_test.go.
//
// Every address is an RFC 5737 or RFC 3849 documentation address and every id
// is generated, so nothing here names anything real.

func testStatsExtenderCountry(
	counts []ExtenderCountryCount,
	countryCode string,
) ExtenderCountryCount {
	for _, count := range counts {
		if count.CountryCode == countryCode {
			return count
		}
	}
	return ExtenderCountryCount{}
}

// M2: the online extender population by country and by family. A dualstack
// extender counts once, as dualstack; an extender that lost every address is
// not online at all; an extender with no country location is labeled by its
// code.
func TestCountExtendersByCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		// australia: one v4-only, one v6-only, one dualstack
		testStatsActivate(ctx, "au-v4", 4, "192.0.2.20", "au", sydney)
		testStatsActivate(ctx, "au-v6", 6, "2001:db8::20", "au", sydney)
		testStatsActivate(ctx, "au-dual", 4, "192.0.2.21", "au", sydney)
		testStatsActivate(ctx, "au-dual", 6, "2001:db8::21", "au", sydney)

		// japan: one v4-only
		testStatsActivate(ctx, "jp-v4", 4, "192.0.2.22", "jp", tokyo)

		// an extender whose activation resolved no location is grouped under
		// the empty code, so the population total still holds
		testStatsActivate(ctx, "unknown", 4, "192.0.2.23", "", nil)

		// one that lost every address: active row, no active address, so not
		// online
		offline := testStatsActivate(ctx, "au-offline", 4, "192.0.2.24", "au", sydney)
		Testing_DeactivateNetworkExtenderAddress(ctx, offline.Extender.ExtenderId, 4)

		// one revoked outright, its address row still active: an inactive
		// extender is never online, whatever its addresses say
		Testing_CreateNetworkExtender(
			ctx,
			&NetworkExtender{
				ExtenderId:  server.NewId(),
				NetworkId:   server.NewId(),
				ClientId:    server.NewId(),
				PublicKey:   []byte("extender-stats-au-revoked"),
				CreateTime:  server.NowUtc(),
				TcpPort:     443,
				UdpPort:     443,
				DnsPort:     53,
				DnsTld:      connect.DefaultExtenderDnsTld,
				CountryCode: "au",
				Active:      false,
			},
			[]*NetworkExtenderAddress{testExtenderCacheAddress("192.0.2.25", true)},
		)

		counts := CountExtendersByCountry(ctx)

		au := testStatsExtenderCountry(counts, "AU")
		if au.Count != 3 || au.Ipv4Count != 1 || au.Ipv6Count != 1 || au.DualstackCount != 1 {
			t.Fatalf("AU = %+v, want 3 = 1 ipv4 + 1 ipv6 + 1 dualstack", au)
		}
		if au.Country != "Australia" {
			t.Fatalf("AU label = %q, want the country location's name", au.Country)
		}

		jp := testStatsExtenderCountry(counts, "JP")
		if jp.Count != 1 || jp.Ipv4Count != 1 || jp.Ipv6Count != 0 || jp.DualstackCount != 0 {
			t.Fatalf("JP = %+v, want 1 ipv4", jp)
		}
		if jp.Country != "Japan" {
			t.Fatalf("JP label = %q", jp.Country)
		}

		unknown := testStatsExtenderCountry(counts, "")
		if unknown.Count != 1 {
			t.Fatalf("the extender with no country = %+v, want 1", unknown)
		}

		// the families of every group sum to its total, and the totals sum to
		// the population
		var total int64
		for _, count := range counts {
			if count.Ipv4Count+count.Ipv6Count+count.DualstackCount != count.Count {
				t.Fatalf("%s families do not sum to its total: %+v", count.CountryCode, count)
			}
			total += count.Count
		}
		if total != 5 {
			t.Fatalf("online extenders = %d, want 5", total)
		}

		// the codes are upper case and ordered, like the provider counts
		previous := ""
		for _, count := range counts {
			if count.CountryCode != "" && count.CountryCode != testStatsUpper(count.CountryCode) {
				t.Fatalf("country code %q is not upper case", count.CountryCode)
			}
			if count.CountryCode < previous {
				t.Fatalf("country codes are not ordered: %q after %q", count.CountryCode, previous)
			}
			previous = count.CountryCode
		}
	})
}

func testStatsUpper(s string) string {
	out := []rune{}
	for _, r := range s {
		if 'a' <= r && r <= 'z' {
			r = r - 'a' + 'A'
		}
		out = append(out, r)
	}
	return string(out)
}

// An extender activated before the location columns existed has no country
// location, so its label falls back to the upper-case code (M4). The row is
// written directly, the way a pre-migration row reads.
func TestCountExtendersByCountryWithoutALocationRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		Testing_CreateNetworkExtender(
			ctx,
			&NetworkExtender{
				ExtenderId:  server.NewId(),
				NetworkId:   server.NewId(),
				ClientId:    server.NewId(),
				PublicKey:   []byte("extender-stats-legacy"),
				CreateTime:  server.NowUtc(),
				TcpPort:     443,
				UdpPort:     443,
				DnsPort:     53,
				DnsTld:      connect.DefaultExtenderDnsTld,
				CountryCode: "de",
				Active:      true,
			},
			[]*NetworkExtenderAddress{testExtenderCacheAddress("192.0.2.30", true)},
		)

		de := testStatsExtenderCountry(CountExtendersByCountry(ctx), "DE")
		if de.Count != 1 || de.Country != "DE" {
			t.Fatalf("DE = %+v, want 1 labeled by its upper-case code", de)
		}
	})
}

// M2: the provider family split, against a reliability row of each proven
// combination. Both proven is dualstack, v6 alone is ipv6, and everything
// else — neither proven, which is what every row written before the columns
// existed reads as, and v4 alone — is ipv4.
func TestCountProvidersByCountryIpFamilies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		nsw := &Location{
			LocationType: LocationTypeRegion,
			Region:       "New South Wales",
			Country:      "Australia",
			CountryCode:  "au",
		}
		CreateLocation(ctx, nsw)

		addProvider := func(ipv4Proven bool, ipv6Proven bool) {
			clientId := server.NewId()
			networkId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
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
					nsw.LocationId,
					nsw.CountryLocationId,
					ipv4Proven,
					ipv6Proven,
				))
			})
			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: []byte("public-secret"),
			})
		}

		addProvider(true, true)   // dualstack
		addProvider(false, true)  // ipv6 alone
		addProvider(true, false)  // ipv4
		addProvider(false, false) // written before the columns existed: ipv4

		counts := CountProvidersByCountry(ctx)
		if len(counts) != 1 || counts[0].CountryCode != "AU" {
			t.Fatalf("provider countries = %+v, want one AU", counts)
		}
		au := counts[0]
		if au.Count != 4 || au.Ipv4Count != 2 || au.Ipv6Count != 1 || au.DualstackCount != 1 {
			t.Fatalf("AU = %+v, want 4 = 2 ipv4 + 1 ipv6 + 1 dualstack", au)
		}
		if au.Ipv4Count+au.Ipv6Count+au.DualstackCount != au.Count {
			t.Fatal("the provider families do not sum to the population")
		}
	})
}
