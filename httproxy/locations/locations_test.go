package locations_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"log/slog"

	"github.com/stretchr/testify/require"
	"github.com/urnetwork/server/httproxy/locations"
)

func TestNewLocationsList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mock server to simulate the API response
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"groups": [
				{
					"location_group_id": "group1",
					"name": "Group 1",
					"provider_count": 10
				}
			],
			"locations": [
				{
					"location_id": "loc1",
					"location_type": "type1",
					"name": "Location 1",
					"country_location_id": "country1",
					"country_code": "US",
					"provider_count": 5
				}
			]
		}`))
	}))
	defer mockServer.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))
	baseURL := mockServer.URL
	refreshInterval := 1 * time.Minute

	ll, err := locations.NewLocationsList(ctx, log, baseURL, refreshInterval)
	require.NoError(t, err)
	require.NotNil(t, ll)

	locationsData := ll.GetLocations()
	require.Len(t, locationsData.Groups, 1)
	require.Equal(t, "group1", locationsData.Groups[0].LocationGroupID)
	require.Len(t, locationsData.Locations, 1)
	require.Equal(t, "loc1", locationsData.Locations[0].LocationID)
}
