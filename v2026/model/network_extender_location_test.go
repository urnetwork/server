package model

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Where an activation says the extender is (connect/EXTENDER.md M1): the four
// location ids the handler resolves from the activating address and stores
// beside the country code, which the map and the by-country gauge read.
//
// Every address is an RFC 5737 or RFC 3849 documentation address and every id
// is generated, so nothing here names anything real.

// One extender activated from ip, located at location (nil for an activation
// whose lookup found nothing).
func testStatsActivate(
	ctx context.Context,
	name string,
	ipVersion int,
	ip string,
	countryCode string,
	location *Location,
) *NetworkExtenderWithAddresses {
	sign, _ := testRecordSigner()
	activation := &NetworkExtenderActivation{
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   []byte("extender-stats-" + name),
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: countryCode,
		IpVersion:   ipVersion,
		Ip:          netip.MustParseAddr(ip),
		Carriers:    []string{connect.ExtenderCarrierTcp},
	}
	return ActivateNetworkExtender(ctx, activation.WithLocation(location), sign)
}

// A city location, created so its four ids exist.
func testStatsCityLocation(
	ctx context.Context,
	city string,
	region string,
	country string,
	countryCode string,
) *Location {
	location := &Location{
		LocationType: LocationTypeCity,
		City:         city,
		Region:       region,
		Country:      country,
		CountryCode:  countryCode,
	}
	CreateLocation(ctx, location)
	return location
}

// M1: an activation stores the four location ids of the address it was
// activated from, a country-only lookup stores the country and leaves the city
// and region null, and the last activation of either family wins.
func TestExtenderActivationStoresLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")

		activated := testStatsActivate(ctx, "city", 4, "192.0.2.10", "au", sydney)
		if activated == nil {
			t.Fatal("the activation was not stored")
		}
		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		for name, got := range map[string]*server.Id{
			"location_id":         stored.Extender.LocationId,
			"city_location_id":    stored.Extender.CityLocationId,
			"region_location_id":  stored.Extender.RegionLocationId,
			"country_location_id": stored.Extender.CountryLocationId,
		} {
			if got == nil {
				t.Fatalf("a city activation stored no %s", name)
			}
		}
		if *stored.Extender.LocationId != sydney.LocationId ||
			*stored.Extender.CityLocationId != sydney.CityLocationId ||
			*stored.Extender.RegionLocationId != sydney.RegionLocationId ||
			*stored.Extender.CountryLocationId != sydney.CountryLocationId {
			t.Fatalf("a city activation stored the wrong ids: %+v", stored.Extender)
		}
		// the returned extender carries what was written, so a caller need not
		// re-read to know where it put the extender
		if activated.Extender.CountryLocationId == nil ||
			*activated.Extender.CountryLocationId != sydney.CountryLocationId {
			t.Fatal("the activation result does not carry its country location")
		}

		// a country-only lookup has neither a city nor a region, and must
		// still store
		japan := &Location{
			LocationType: LocationTypeCountry,
			Country:      "Japan",
			CountryCode:  "jp",
		}
		CreateLocation(ctx, japan)
		countryOnly := testStatsActivate(ctx, "country", 4, "192.0.2.11", "jp", japan)
		if countryOnly == nil {
			t.Fatal("a country-only activation was not stored")
		}
		stored = Testing_GetNetworkExtender(ctx, countryOnly.Extender.ExtenderId)
		if stored.Extender.CountryLocationId == nil ||
			*stored.Extender.CountryLocationId != japan.CountryLocationId {
			t.Fatal("a country-only activation stored no country location")
		}
		if stored.Extender.LocationId == nil ||
			*stored.Extender.LocationId != japan.LocationId {
			t.Fatal("a country-only activation stored no location")
		}
		if stored.Extender.CityLocationId != nil || stored.Extender.RegionLocationId != nil {
			t.Fatalf(
				"a country-only activation invented a city or region: %+v",
				stored.Extender,
			)
		}

		// a lookup that found nothing must still store the activation
		nowhere := testStatsActivate(ctx, "nowhere", 4, "192.0.2.12", "", nil)
		if nowhere == nil {
			t.Fatal("an activation with no location was not stored")
		}
		stored = Testing_GetNetworkExtender(ctx, nowhere.Extender.ExtenderId)
		if stored.Extender.LocationId != nil || stored.Extender.CountryLocationId != nil {
			t.Fatalf("an activation with no location invented one: %+v", stored.Extender)
		}

		// the second family of the same identity key rewrites the four ids
		// from its own address: an extender's location is the one it activated
		// last (M1)
		tokyo := testStatsCityLocation(ctx, "Tokyo", "Tokyo", "Japan", "jp")
		again := testStatsActivate(ctx, "city", 6, "2001:db8::10", "jp", tokyo)
		if again == nil || again.Extender.ExtenderId != activated.Extender.ExtenderId {
			t.Fatal("re-activating the same key created a second extender")
		}
		stored = Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		if stored.Extender.CityLocationId == nil ||
			*stored.Extender.CityLocationId != tokyo.CityLocationId ||
			*stored.Extender.CountryLocationId != tokyo.CountryLocationId {
			t.Fatalf("the last activation did not win: %+v", stored.Extender)
		}
		// and the first location is gone, not merged
		if *stored.Extender.RegionLocationId == sydney.RegionLocationId {
			t.Fatal("the last activation kept the previous region")
		}
	})
}
