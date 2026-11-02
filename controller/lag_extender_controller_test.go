package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests of the extender sample (lag.dev/PLAN.md A4, T2), against extenders
// holding live measurement certificates, and the fixtures the track tests
// share.

// The measurement zone of these tests.
const lagTestMeasurementZone = "extender.lag-test.example"

// The operator host the tickets name.
const lagTestNetworkHost = "ur.lag-test.example"

// Installs an extender.yml with a fresh root key and the measurement block,
// on or off, and returns the root key's public half.
func installLagTestExtenderConfig(t testing.TB, measurementEnabled bool) ed25519.PublicKey {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	installTestExtenderConfigYaml(t, strings.Join([]string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPublicKey)),
		fmt.Sprintf("network_host: %s", lagTestNetworkHost),
		"measurement:",
		fmt.Sprintf("  enabled: %t", measurementEnabled),
		"  zone_name: " + lagTestMeasurementZone,
	}, "\n"))
	Testing_ResetLagServing()
	t.Cleanup(Testing_ResetLagServing)
	return rootPublicKey
}

// The extender's request for its measurement name and the live certificate
// that answers it, issued an hour ago, with the name published and the
// certificate delivered, as the issuance task, the dns reconcile and the
// extender's next poll leave them: the request is issued, the name resolves
// and the extender serves it. Returns the certificate.
func addLagTestCert(t testing.TB, ctx context.Context, extender *lagTestExtender, names ...string) *model.NetworkExtenderMeasurementCert {
	t.Helper()
	cert := addLagTestUnpublishedCert(t, ctx, extender, names...)
	model.PublishNetworkExtenderMeasurementNames(ctx, cert.Names, server.NowUtc())
	Testing_ResetLagServing()
	return cert
}

// The request and the live certificate as addLagTestCert, delivered, before
// the dns reconcile has published the name.
func addLagTestUnpublishedCert(t testing.TB, ctx context.Context, extender *lagTestExtender, names ...string) *model.NetworkExtenderMeasurementCert {
	t.Helper()
	cert := addLagTestUndeliveredCert(t, ctx, extender, names...)
	connect.AssertEqual(t, model.MarkNetworkExtenderMeasurementCertDelivered(ctx, cert.CertId, server.NowUtc()), true)
	Testing_ResetLagServing()
	return cert
}

// The request and the live certificate as the issuance task leaves them,
// before the extender's poll has read the certificate (A8) and before the dns
// reconcile has published its name.
func addLagTestUndeliveredCert(t testing.TB, ctx context.Context, extender *lagTestExtender, names ...string) *model.NetworkExtenderMeasurementCert {
	t.Helper()
	if len(names) == 0 {
		names = []string{ExtenderMeasurementName(extender.extenderId, lagTestMeasurementZone)}
	}
	now := server.NowUtc()
	certPublicKey := server.NewId().Bytes()
	model.SetNetworkExtenderMeasurementRequest(ctx, extender.extenderId, []byte{1}, certPublicKey, names, now.Add(-2*time.Hour))
	cert := &model.NetworkExtenderMeasurementCert{
		CertId:     server.NewId(),
		ExtenderId: extender.extenderId,
		Names:      names,
		Csr:        []byte{1},
		PublicKey:  certPublicKey,
		ChainPem:   "-",
		SerialHex:  "01",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(60 * 24 * time.Hour),
		IssueTime:  now.Add(-time.Hour),
		CapCounted: true,
		RenewTime:  now.Add(40 * 24 * time.Hour),
	}
	model.AddNetworkExtenderMeasurementCert(ctx, cert)
	Testing_ResetLagServing()
	return cert
}

// An extender that serves the measurement api, placed by the GeoLite2 City
// stand-in at its first address.
func newLagServingTestExtender(t testing.TB, ctx context.Context, places map[string]*model.LagPlace, place *model.LagPlace, ips ...string) *lagTestExtender {
	t.Helper()
	extender := newLagTestExtender(t, ctx, ips...)
	addLagTestCert(t, ctx, extender)
	if place != nil {
		places[netip.MustParseAddr(ips[0]).String()] = place
	}
	return extender
}

// A lag guest's client session, from addr.
func newLagTestGuestSession(t testing.TB, ctx context.Context, clientAddress string) *session.ClientSession {
	t.Helper()
	byJwt := session.NewByJwt(server.NewId(), server.NewId(), "lag-test-guest", false, false).
		Client(server.NewId(), server.NewId())
	clientSession := session.NewLocalClientSession(ctx, clientAddress, byJwt)
	t.Cleanup(clientSession.Cancel)
	return clientSession
}

func lagTestPlace(city string, countryCode string, latitude float64, longitude float64) *model.LagPlace {
	return &model.LagPlace{City: city, CountryCode: countryCode, CountryName: strings.ToUpper(countryCode), Latitude: latitude, Longitude: longitude}
}

// The sample is every extender that serves the measurement api now, one entry
// each with its name and its addresses, IPv4 first, placed where the issuance
// order counts it (model.GetLagExtenderPlaces): by GeoLite2 City for its
// addresses, choosing between its two families when they disagree, and moved
// to its derived location only within that country and only while that is
// close enough. Extenders that may not be measured, hold no live certificate
// for their name, or that GeoLite2 places nowhere, are not in it.
func TestLagExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		records := map[string]*model.LagAddressPlace{}
		useLagAddressRecords(t, func(ip string) *model.LagAddressPlace {
			if record := records[ip]; record != nil {
				return record
			}
			if place := places[ip]; place != nil {
				return &model.LagAddressPlace{Place: place, AccuracyRadiusKm: 20}
			}
			return nil
		})

		// derived within its country: moved to where its pings put it, the
		// country staying GeoLite2's. Its IPv4 address is placed nowhere, so
		// its IPv6 address places it
		hamburg := &model.LagPlace{City: "Hamburg", Region: "Hamburg", CountryCode: "de", CountryName: "Germany", Latitude: 53.5, Longitude: 10.0}
		derived := newLagServingTestExtender(t, ctx, places, hamburg, "2001:db8:a::1", "192.0.2.1")
		// derived across a border, however close: GeoLite2's place
		crossed := newLagServingTestExtender(t, ctx, places, &model.LagPlace{CountryCode: "gb", CountryName: "United Kingdom", Latitude: 51.4964, Longitude: -0.1224}, "192.0.2.20")
		// derived within its country, too far off: GeoLite2's place
		unsettled := newLagServingTestExtender(t, ctx, places, &model.LagPlace{City: "Detroit", Region: "Michigan", CountryCode: "us", CountryName: "United States", Latitude: 42.33, Longitude: -83.05}, "192.0.2.21")
		location := func(city string, region string, country string, countryCode string) *model.Location {
			location := &model.Location{
				LocationType: model.LocationTypeCity,
				City:         city,
				Region:       region,
				Country:      country,
				CountryCode:  countryCode,
			}
			model.CreateLocation(ctx, location)
			return location
		}
		derivedRow := func(extenderId server.Id, city *model.Location, latitude float64, longitude float64, residualKm float32) *model.DerivedLocation {
			return &model.DerivedLocation{
				NodeKind:          model.DerivedLocationNodeKindExtender,
				NodeId:            extenderId,
				GenesisLatitude:   latitude,
				GenesisLongitude:  longitude,
				GenesisAccuracyKm: 50,
				Latitude:          latitude,
				Longitude:         longitude,
				PingCount:         20,
				PeerCount:         4,
				ResidualKm:        residualKm,
				Reputation:        1,
				LocationId:        city.LocationId,
				CityLocationId:    city.CityLocationId,
				RegionLocationId:  city.RegionLocationId,
				CountryLocationId: city.CountryLocationId,
				UpdateTime:        server.NowUtc(),
			}
		}
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			derivedRow(derived.extenderId, location("Frankfurt am Main", "Hesse", "Germany", "de"), 50.11, 8.68, 12),
			derivedRow(crossed.extenderId, location("Cahermore", "Munster", "Ireland", "ie"), 47.29, -11.73, 12),
			derivedRow(unsettled.extenderId, location("Saginaw", "Missouri", "United States", "us"), 37.03, -94.48, 45076),
		})
		// two families in two countries and two networks: its IPv6 address's
		// place, though GeoLite2 is surer of its IPv4 address
		dualStack := newLagServingTestExtender(t, ctx, places, nil, "192.0.2.22", "2001:db8:22::1")
		records["192.0.2.22"] = &model.LagAddressPlace{Place: lagTestPlace("Riga", "lv", 56.95, 24.1), AccuracyRadiusKm: 5, Asn: 402279}
		records["2001:db8:22::1"] = &model.LagAddressPlace{Place: lagTestPlace("", "hk", 22.2578, 114.1657), AccuracyRadiusKm: 1000, Asn: 150452}
		// placed by its address only
		geolite := newLagServingTestExtender(t, ctx, places, lagTestPlace("Osaka", "jp", 34.7, 135.5), "192.0.2.2")
		// placed nowhere: not in the sample
		newLagServingTestExtender(t, ctx, places, nil, "192.0.2.3")

		// not measurable: no certificate, a certificate for another name,
		// another tcp port, the gated tier, a canary, an expired certificate
		newLagTestExtender(t, ctx, "192.0.2.4")
		places["192.0.2.4"] = lagTestPlace("Lyon", "fr", 45.7, 4.8)
		otherName := newLagTestExtender(t, ctx, "192.0.2.5")
		addLagTestCert(t, ctx, otherName, ExtenderMeasurementName(server.NewId(), lagTestMeasurementZone))
		places["192.0.2.5"] = lagTestPlace("Lyon", "fr", 45.7, 4.8)
		for i, edit := range []string{
			`UPDATE network_extender SET tcp_port = 8443 WHERE extender_id = $1`,
			`UPDATE network_extender SET directory_tier = 1 WHERE extender_id = $1`,
			`UPDATE network_extender SET canary_channel = 'dns' WHERE extender_id = $1`,
			`UPDATE network_extender_measurement_cert SET not_after = now() AT TIME ZONE 'UTC' - interval '1 minute' WHERE extender_id = $1`,
		} {
			ip := fmt.Sprintf("192.0.2.%d", 10+i)
			excluded := newLagServingTestExtender(t, ctx, places, lagTestPlace("Lyon", "fr", 45.7, 4.8), ip)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender SET measurement_exposure_accepted = true WHERE extender_id = $1`, excluded.extenderId))
				server.RaisePgResult(tx.Exec(ctx, edit, excluded.extenderId))
			})
		}
		Testing_ResetLagServing()

		result, err := LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Extenders), 5)
		byId := map[server.Id]*LagExtender{}
		for _, extender := range result.Extenders {
			byId[extender.ExtenderId] = extender
		}

		connect.AssertEqual(t, byId[derived.extenderId], &LagExtender{
			ExtenderId: derived.extenderId,
			PublicKey:  hex.EncodeToString(derived.publicKey),
			Host:       derived.extenderId.String() + "." + lagTestMeasurementZone,
			Ips:        []string{"192.0.2.1", "2001:db8:a::1"},
			Location: &LagExtenderLocation{
				City:        "Frankfurt am Main",
				Region:      "Hesse",
				CountryCode: "de",
				CountryName: "Germany",
				Latitude:    50.11,
				Longitude:   8.68,
			},
		})
		connect.AssertEqual(t, byId[crossed.extenderId].Location, &LagExtenderLocation{
			CountryCode: "gb",
			CountryName: "United Kingdom",
			Latitude:    51.4964,
			Longitude:   -0.1224,
		})
		connect.AssertEqual(t, byId[unsettled.extenderId].Location, &LagExtenderLocation{
			City:        "Detroit",
			Region:      "Michigan",
			CountryCode: "us",
			CountryName: "United States",
			Latitude:    42.33,
			Longitude:   -83.05,
		})
		connect.AssertEqual(t, byId[dualStack.extenderId].Ips, []string{"192.0.2.22", "2001:db8:22::1"})
		connect.AssertEqual(t, byId[dualStack.extenderId].Location, &LagExtenderLocation{
			CountryCode: "hk",
			CountryName: "HK",
			Latitude:    22.2578,
			Longitude:   114.1657,
		})
		connect.AssertEqual(t, byId[geolite.extenderId].Host, geolite.extenderId.String()+"."+lagTestMeasurementZone)
		connect.AssertEqual(t, byId[geolite.extenderId].Ips, []string{"192.0.2.2"})
		connect.AssertEqual(t, byId[geolite.extenderId].Location, &LagExtenderLocation{
			City:        "Osaka",
			CountryCode: "jp",
			CountryName: "JP",
			Latitude:    34.7,
			Longitude:   135.5,
		})

		// with the measurement block off, nothing is measured
		installLagTestExtenderConfig(t, false)
		result, err = LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Extenders), 0)
	})
}

// The issuance order counts every extender in the country the sample places
// it in (lag.dev/PLAN.md A4, C7): each extender the sample offers in its
// location's country, a capped request's extender where the sample would
// place it once it serves, and only an extender placed nowhere in the
// country it last activated from, "de" for every extender here. The sample's
// spread across countries follows the same places. The extenders are the
// seven whose last-activated country lag.dev did not show on Main at
// 2026-11-01, with what GeoLite2 said of their addresses and their derived
// locations, and one whose families agree.
func TestLagExtendersArePlacedWhereTheOrderCountsThem(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		records := map[string]*model.LagAddressPlace{}
		useLagAddressRecords(t, func(ip string) *model.LagAddressPlace {
			return records[ip]
		})
		record := func(ip string, countryCode string, city string, latitude float64, longitude float64, accuracyRadiusKm int, asn uint32) {
			records[ip] = &model.LagAddressPlace{
				Place:            &model.LagPlace{City: city, CountryCode: countryCode, CountryName: strings.ToUpper(countryCode), Latitude: latitude, Longitude: longitude},
				AccuracyRadiusKm: accuracyRadiusKm,
				Asn:              asn,
			}
		}
		serving := func(ips ...string) server.Id {
			extender := newLagTestExtender(t, ctx, ips...)
			addLagTestCert(t, ctx, extender)
			return extender.extenderId
		}

		// 01a18ba8: a leased IPv4 block GeoLite2 places in Riga
		leased := serving("192.0.2.30", "2001:db8:30::1")
		record("192.0.2.30", "lv", "Riga", 56.9473, 24.0979, 5, 402279)
		record("2001:db8:30::1", "hk", "", 22.2578, 114.1657, 1000, 150452)
		// 01a18c5e: a leased IPv4 block GeoLite2 places only in the US
		liberally := serving("192.0.2.31", "2001:db8:31::1")
		record("192.0.2.31", "us", "", 37.751, -97.822, 1000, 152918)
		record("2001:db8:31::1", "hk", "", 22.2578, 114.1657, 1000, 150452)
		// 01a18556 and 01a1855e: one network's two families, the IPv6 address
		// placed only in the network's country
		corenet := serving("192.0.2.32", "2001:db8:32::1")
		record("192.0.2.32", "hk", "Tseung Kwan O", 22.3346, 114.2536, 20, 154321)
		record("2001:db8:32::1", "my", "", 2.5, 112.5, 1000, 154321)
		// 01a18187: derived into Shenzhen, far off
		hkt := serving("192.0.2.33")
		record("192.0.2.33", "hk", "", 22.2578, 114.1657, 10, 4760)
		// 01a17c2a: derived into the Atlantic, mapped to Ireland
		aiyun := serving("192.0.2.34")
		record("192.0.2.34", "gb", "", 51.4964, -0.1224, 1000, 152672)
		// 01a167b3: derived 5 km across the Detroit River, far off
		hostodo := serving("192.0.2.35", "2001:db8:35::1")
		record("192.0.2.35", "us", "Detroit", 42.3329, -83.0495, 20, 399804)
		record("2001:db8:35::1", "us", "", 37.751, -97.822, 1000, 399804)
		// two families that agree
		ovh := serving("192.0.2.36", "2001:db8:36::1")
		record("192.0.2.36", "fr", "", 48.8582, 2.3387, 500, 16276)
		record("2001:db8:36::1", "fr", "", 48.8582, 2.3387, 500, 16276)
		// GeoLite2 places it nowhere: not offered, and counted in its own
		// country
		nowhere := serving("192.0.2.37")
		// a capped request: one network's two families placed as surely in
		// two countries
		candidate := newLagTestExtender(t, ctx, "192.0.2.38", "2001:db8:38::1")
		record("192.0.2.38", "us", "Houston", 29.76, -95.37, 20, 393448)
		record("2001:db8:38::1", "sg", "Ayer Raja New Town", 1.32, 103.75, 20, 393448)
		model.SetNetworkExtenderMeasurementRequest(
			ctx,
			candidate.extenderId,
			[]byte{1},
			server.NewId().Bytes(),
			[]string{ExtenderMeasurementName(candidate.extenderId, lagTestMeasurementZone)},
			server.NowUtc().Add(-time.Hour),
		)

		city := func(city string, region string, country string, countryCode string) *model.Location {
			location := &model.Location{
				LocationType: model.LocationTypeCity,
				City:         city,
				Region:       region,
				Country:      country,
				CountryCode:  countryCode,
			}
			model.CreateLocation(ctx, location)
			return location
		}
		derivedRow := func(extenderId server.Id, city *model.Location, latitude float64, longitude float64, residualKm float32) *model.DerivedLocation {
			return &model.DerivedLocation{
				NodeKind:          model.DerivedLocationNodeKindExtender,
				NodeId:            extenderId,
				GenesisLatitude:   latitude,
				GenesisLongitude:  longitude,
				GenesisAccuracyKm: 1000,
				Latitude:          latitude,
				Longitude:         longitude,
				PingCount:         600,
				PeerCount:         150,
				ResidualKm:        residualKm,
				Reputation:        1,
				CrossedCountry:    true,
				LocationId:        city.LocationId,
				CityLocationId:    city.CityLocationId,
				RegionLocationId:  city.RegionLocationId,
				CountryLocationId: city.CountryLocationId,
				UpdateTime:        server.NowUtc(),
			}
		}
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{
			derivedRow(hkt, city("Longgang District", "Guangdong", "China", "cn"), 22.757, 114.809, 28192),
			derivedRow(aiyun, city("Cahermore", "Munster", "Ireland", "ie"), 47.29, -11.733, 21821),
			derivedRow(hostodo, city("Windsor", "Ontario", "Canada", "ca"), 42.332, -82.98, 47763),
		})
		Testing_ResetLagServing()

		result, err := LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
		connect.AssertEqual(t, err, nil)
		offered := map[server.Id]string{}
		for _, extender := range result.Extenders {
			offered[extender.ExtenderId] = extender.Location.CountryCode
		}
		connect.AssertEqual(t, offered, map[server.Id]string{
			leased:    "hk",
			liberally: "hk",
			corenet:   "hk",
			hkt:       "hk",
			aiyun:     "gb",
			hostodo:   "us",
			ovh:       "fr",
		})

		countries := model.Testing_GetNetworkExtenderMeasurementOrderCountries(ctx, server.NowUtc())
		for extenderId, countryCode := range offered {
			connect.AssertEqual(t, countries[extenderId], countryCode)
		}
		connect.AssertEqual(t, countries[nowhere], "de")
		connect.AssertEqual(t, countries[candidate.extenderId], "sg")
		connect.AssertEqual(t, model.GetLagExtenderPlaces(ctx, []server.Id{candidate.extenderId})[candidate.extenderId].CountryCode, "sg")

		// the sample spreads by the same places: one of each country first
		set := getLagServing(ctx, server.NowUtc(), time.Minute)
		connect.AssertEqual(t, len(set.extenders), 7)
		for _, extender := range set.extenders {
			connect.AssertEqual(t, extender.Place.CountryCode, countries[extender.ExtenderId])
		}
		for range 10 {
			codes := []string{}
			for _, extender := range sampleLagServing(set, 4) {
				codes = append(codes, extender.Place.CountryCode)
			}
			slices.Sort(codes)
			connect.AssertEqual(t, codes, []string{"fr", "gb", "hk", "us"})
		}
	})
}

// A sample takes one extender from each country before it repeats one, and
// stops at its size. Pure.
func TestSampleLagServingSpreadsAcrossCountries(t *testing.T) {
	set := &lagServingSet{byId: map[server.Id]*lagServingExtender{}}
	for _, countryCode := range []string{"de", "de", "de", "de", "jp", "jp", "br", "us", "us", "us"} {
		extender := &lagServingExtender{
			ExtenderMeasurementHost: &ExtenderMeasurementHost{ExtenderId: server.NewId()},
			Place:                   &model.LagPlace{CountryCode: countryCode},
		}
		set.extenders = append(set.extenders, extender)
		set.byId[extender.ExtenderId] = extender
	}
	for range 20 {
		countries := func(sample []*lagServingExtender) []string {
			codes := []string{}
			for _, extender := range sample {
				codes = append(codes, extender.Place.CountryCode)
			}
			return codes
		}
		four := countries(sampleLagServing(set, 4))
		slices.Sort(four)
		connect.AssertEqual(t, four, []string{"br", "de", "jp", "us"})

		seven := countries(sampleLagServing(set, 7))
		slices.Sort(seven)
		connect.AssertEqual(t, seven, []string{"br", "de", "de", "jp", "jp", "us", "us"})

		all := sampleLagServing(set, 64)
		connect.AssertEqual(t, len(all), 10)
		ids := map[server.Id]bool{}
		for _, extender := range all {
			ids[extender.ExtenderId] = true
		}
		connect.AssertEqual(t, len(ids), 10)
	}
	connect.AssertEqual(t, len(sampleLagServing(&lagServingSet{}, 64)), 0)
}

// The sample is limited to extender_sample, and randomized per request.
func TestLagExtendersSampleSize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		settings.ExtenderSample = 3
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		for i := range 6 {
			newLagServingTestExtender(t, ctx, places, lagTestPlace("City", fmt.Sprintf("c%d", i), 0, float64(i)), fmt.Sprintf("192.0.2.%d", i+1))
		}

		seen := map[server.Id]bool{}
		for range 10 {
			result, err := LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(result.Extenders), 3)
			for _, extender := range result.Extenders {
				seen[extender.ExtenderId] = true
			}
		}
		if len(seen) <= 3 {
			t.Fatalf("ten samples of 3 drew only %d of 6 extenders", len(seen))
		}
	})
}
