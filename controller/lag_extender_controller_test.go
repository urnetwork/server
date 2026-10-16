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
// each with its name and its addresses, IPv4 first, placed by its derived
// location, else by GeoLite2 City for its first address. Extenders that may
// not be measured, or hold no live certificate for their name, are not in it.
func TestLagExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)

		// derived: placed where its pings put it, whatever its address says
		derived := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "2001:db8:a::1", "192.0.2.1")
		frankfurt := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Frankfurt am Main",
			Region:       "Hesse",
			Country:      "Germany",
			CountryCode:  "de",
		}
		model.CreateLocation(ctx, frankfurt)
		model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{{
			NodeKind:          model.DerivedLocationNodeKindExtender,
			NodeId:            derived.extenderId,
			GenesisLatitude:   53.5,
			GenesisLongitude:  10.0,
			GenesisAccuracyKm: 50,
			Latitude:          50.11,
			Longitude:         8.68,
			PingCount:         20,
			PeerCount:         4,
			Reputation:        1,
			LocationId:        frankfurt.LocationId,
			CityLocationId:    frankfurt.CityLocationId,
			RegionLocationId:  frankfurt.RegionLocationId,
			CountryLocationId: frankfurt.CountryLocationId,
			UpdateTime:        server.NowUtc(),
		}})
		// placed by its first address only
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
		connect.AssertEqual(t, len(result.Extenders), 2)
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
