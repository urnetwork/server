// The location list a client in post-quantum mode reads
// (FindLocationsArgs.RequireModernProvider, FP2-MODERN.md "The location
// list"): only modern supply, as the score export counted it in each
// location's filter, so the client is never offered a location it cannot
// connect to. A filter from an export before the count is listed as before.
package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The filter document the export writes, under a type of the test's own:
// gob matches fields by name, so the fixture writes and reads the same
// document whatever fields ClientFilter holds.
type providerPickerModernTestFilter struct {
	Count                      int
	NetReliabilityWeight       float64
	ModernCount                int
	ModernNetReliabilityWeight float64
	ModernCounted              bool
}

// The filter document of an export before the modern count.
type providerPickerModernTestOldFilter struct {
	Count                int
	NetReliabilityWeight float64
}

// The request body as the sdk sends it, decoded the way the api does.
func providerPickerModernTestArgs(t testing.TB, body string) *FindLocationsArgs {
	t.Helper()
	var findLocations FindLocationsArgs
	if err := json.Unmarshal([]byte(body), &findLocations); err != nil {
		t.Fatal(err)
	}
	return &findLocations
}

// The listed locations by id.
func providerPickerModernTestListed(result *FindLocationsResult) map[server.Id]*LocationResult {
	locationResults := map[server.Id]*LocationResult{}
	for _, locationResult := range result.Locations {
		locationResults[locationResult.LocationId] = locationResult
	}
	return locationResults
}

// Four countries: modern supply that is stable, modern supply below the
// stability bar, no modern supply, and a filter from an export before the
// count. A client that requires modern providers is offered the two with
// modern supply, counted and judged by their modern providers, and the
// uncounted one as before; never the one without modern supply. Without the
// field the list is the capability-blind one.
func TestFindProviderLocationsRequiringModernListsOnlyModernSupply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		country := func(name string, countryCode string, clientCount int) *ClientLocation {
			return &ClientLocation{
				LocationId:   server.NewId(),
				LocationType: LocationTypeCountry,
				Name:         name,
				CountryCode:  countryCode,
				ClientCount:  clientCount,
			}
		}
		modernCountry := country("Synthetic modern country", "xa", 3)
		thinModernCountry := country("Synthetic thin modern country", "xb", 4)
		classicalCountry := country("Synthetic classical country", "xc", 2)
		uncountedCountry := country("Synthetic uncounted country", "xd", 5)
		filters := map[server.Id]any{
			modernCountry.LocationId: &providerPickerModernTestFilter{
				Count:                      3,
				NetReliabilityWeight:       3 * MinStableNetReliabilityWeight,
				ModernCount:                2,
				ModernNetReliabilityWeight: 2 * MinStableNetReliabilityWeight,
				ModernCounted:              true,
			},
			thinModernCountry.LocationId: &providerPickerModernTestFilter{
				Count:                      4,
				NetReliabilityWeight:       4 * MinStableNetReliabilityWeight,
				ModernCount:                1,
				ModernNetReliabilityWeight: MinStableNetReliabilityWeight / 2,
				ModernCounted:              true,
			},
			classicalCountry.LocationId: &providerPickerModernTestFilter{
				Count:                2,
				NetReliabilityWeight: 2 * MinStableNetReliabilityWeight,
				ModernCounted:        true,
			},
			uncountedCountry.LocationId: &providerPickerModernTestOldFilter{
				Count:                5,
				NetReliabilityWeight: 5 * MinStableNetReliabilityWeight,
			},
		}
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "picker-modern", false, false))
		// the filters are written for the caller location the list reads them
		// for, as the export writes one per caller location
		clientIp, _, err := clientSession.ParseClientIpPort()
		connect.AssertEqual(t, err, nil)
		ipInfo, err := server.GetIpInfo(clientIp)
		connect.AssertEqual(t, err, nil)
		callerLocationId := countryCodeLocationIds()[ipInfo.CountryCode]
		server.Redis(ctx, func(r server.RedisClient) {
			initial := &InitialClientLocations{
				Locations: []*ClientLocation{modernCountry, thinModernCountry, classicalCountry, uncountedCountry},
			}
			server.Raise(r.Set(ctx, initialClientLocationsKey(), gobEncodeForTest(t, initial), time.Minute).Err())
			for locationId, filter := range filters {
				key := clientScoreLocationFilterKey(false, RankModeQuality, locationId, callerLocationId)
				server.Raise(r.Set(ctx, key, gobEncodeForTest(t, filter), time.Minute).Err())
			}
		})

		result, err := FindProviderLocations(providerPickerModernTestArgs(t, `{"query":"","require_modern_provider":true}`), clientSession)
		connect.AssertEqual(t, err, nil)
		listed := providerPickerModernTestListed(result)
		if location := listed[modernCountry.LocationId]; location == nil || location.ProviderCount != 2 || !location.Stable {
			t.Errorf("modern supply: listed %+v, want 2 providers, stable", location)
		}
		if location := listed[thinModernCountry.LocationId]; location == nil || location.ProviderCount != 1 || location.Stable {
			t.Errorf("thin modern supply: listed %+v, want 1 provider, not stable", location)
		}
		if location := listed[classicalCountry.LocationId]; location != nil {
			t.Errorf("no modern supply: listed %+v to a client that requires modern providers", location)
		}
		if location := listed[uncountedCountry.LocationId]; location == nil || location.ProviderCount != 5 || !location.Stable {
			t.Errorf("uncounted supply: listed %+v, want it as before, 5 providers, stable", location)
		}
		connect.AssertEqual(t, len(result.Locations), 3)
		connect.AssertEqual(t, result.CountryCount, 3)

		result, err = FindProviderLocations(providerPickerModernTestArgs(t, `{"query":""}`), clientSession)
		connect.AssertEqual(t, err, nil)
		listed = providerPickerModernTestListed(result)
		for _, clientLocation := range []*ClientLocation{modernCountry, thinModernCountry, classicalCountry, uncountedCountry} {
			location := listed[clientLocation.LocationId]
			if location == nil || location.ProviderCount != clientLocation.ClientCount || !location.Stable {
				t.Errorf("capability-blind list: %s listed %+v, want %d providers, stable", clientLocation.Name, location, clientLocation.ClientCount)
			}
		}
		connect.AssertEqual(t, len(result.Locations), 4)
	})
}
