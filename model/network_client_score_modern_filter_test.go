// The score export counts each location's modern public supply in its filter
// (ClientFilter.ModernCount), the count the location list of a client in
// post-quantum mode publishes (FP2-MODERN.md "The location list").
package model

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A country whose providers are all classical is offered to a client that
// requires modern providers no more, while a country with one modern provider
// is offered with that one. The export counts the public providers it counts
// today, so a network-only provider measured modern adds nothing; and it
// marks every filter counted, the modern-free country's too.
func TestUpdateClientScoresCountsModernSupplyForTheLocationList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		modernCity := egressTestCity(ctx, "Modern List City", "Modern List Region", "United States", "us")
		classicalCity := egressTestCity(ctx, "Classical List City", "Classical List Region", "Canada", "ca")

		modernPublic := egressTestConnect(ctx, t, modernCity, egressTestFast, nil, nil)
		egressTestProbed(ctx, modernPublic, modernCity, 0, "us")
		classicalPublic := egressTestConnect(ctx, t, modernCity, egressTestFast, nil, nil)
		egressTestProbed(ctx, classicalPublic, modernCity, 0, "us")
		egressTestForgetModernEvidence(ctx, classicalPublic.clientId)
		// measured modern, but it provides only to its network: no public supply
		networkMeasured := egressTestConnect(ctx, t, modernCity, egressTestFast, modernTestNetworkOnlyKeys, nil)
		modernTestMeasure(ctx, networkMeasured.clientId, true, true)
		otherClassicalPublic := egressTestConnect(ctx, t, classicalCity, egressTestFast, nil, nil)
		egressTestProbed(ctx, otherClassicalPublic, classicalCity, 0, "ca")
		egressTestForgetModernEvidence(ctx, otherClassicalPublic.clientId)

		egressTestPasses(ctx, t)

		readFilter := func(locationId server.Id) *providerPickerModernTestFilter {
			t.Helper()
			var filterBytes []byte
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				filterBytes, err = r.Get(ctx, clientScoreLocationFilterKey(false, RankModeQuality, locationId, server.Id{})).Bytes()
				server.Raise(err)
			})
			var filter providerPickerModernTestFilter
			if err := gob.NewDecoder(bytes.NewReader(filterBytes)).Decode(&filter); err != nil {
				t.Fatal(err)
			}
			return &filter
		}
		for _, locationId := range []server.Id{modernCity.LocationId, modernCity.CountryLocationId} {
			filter := readFilter(locationId)
			if !filter.ModernCounted || filter.Count != 2 || filter.ModernCount != 1 {
				t.Errorf("modern supply: filter %+v, want counted, 2 public providers, 1 modern", filter)
			}
		}
		for _, locationId := range []server.Id{classicalCity.LocationId, classicalCity.CountryLocationId} {
			filter := readFilter(locationId)
			if !filter.ModernCounted || filter.Count != 1 || filter.ModernCount != 0 {
				t.Errorf("classical supply: filter %+v, want counted, 1 public provider, none modern", filter)
			}
		}

		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "modern-list", false, false))
		result, err := FindProviderLocations(providerPickerModernTestArgs(t, `{"query":"","require_modern_provider":true}`), clientSession)
		connect.AssertEqual(t, err, nil)
		listed := providerPickerModernTestListed(result)
		if location := listed[modernCity.CountryLocationId]; location == nil || location.ProviderCount != 1 {
			t.Errorf("modern supply: listed %+v, want 1 provider", location)
		}
		if location := listed[classicalCity.CountryLocationId]; location != nil {
			t.Errorf("no modern supply: listed %+v to a client that requires modern providers", location)
		}

		result, err = FindProviderLocations(providerPickerModernTestArgs(t, `{"query":""}`), clientSession)
		connect.AssertEqual(t, err, nil)
		listed = providerPickerModernTestListed(result)
		for _, locationId := range []server.Id{modernCity.CountryLocationId, classicalCity.CountryLocationId} {
			if listed[locationId] == nil {
				t.Errorf("capability-blind list: a country with public supply is not listed")
			}
		}
	})
}
