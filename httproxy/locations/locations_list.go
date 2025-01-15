package locations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type LocationsResponse struct {
	Groups    []LocationGroup `json:"groups"`
	Locations []Location      `json:"locations"`
}

type LocationGroup struct {
	LocationGroupID string `json:"location_group_id"`
	Name            string `json:"name"`
	ProviderCount   int    `json:"provider_count"`
	Promoted        bool   `json:"promoted,omitempty"`
}

type Location struct {
	LocationID        string `json:"location_id"`
	LocationType      string `json:"location_type"`
	Name              string `json:"name"`
	RegionLocationID  string `json:"region_location_id,omitempty"`
	CountryLocationID string `json:"country_location_id"`
	CountryCode       string `json:"country_code"`
	ProviderCount     int    `json:"provider_count"`
	CityLocationID    string `json:"city_location_id,omitempty"`
}

type LocationsList struct {
	locations LocationsResponse
	mu        sync.RWMutex
	cl        *http.Client
}

func NewLocationsList(
	ctx context.Context,
	log *slog.Logger,
	baseURL string,
	refreshInterval time.Duration,

) (*LocationsList, error) {

	ll := &LocationsList{
		cl: &http.Client{
			Timeout: 10 * time.Second,
		},
	}

	err := ll.fetchLocations(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch locations: %w", err)
	}

	go ll.refreshLocations(ctx, log, baseURL, refreshInterval)

	return ll, nil

}

func (ll *LocationsList) refreshLocations(
	ctx context.Context,
	log *slog.Logger,
	baseURL string,
	refreshInterval time.Duration,
) error {
	defer func() {
		log.Info("locations refresh loop exited")
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(refreshInterval):
			err := ll.fetchLocations(ctx, baseURL)
			if err != nil {
				log.Error("failed to fetch locations", "error", err)
			}
		}
	}

}

func (ll *LocationsList) fetchLocations(
	ctx context.Context,
	baseURL string,
) error {

	u, err := url.JoinPath(baseURL, "network/provider-locations")
	if err != nil {
		return fmt.Errorf("failed to join URL for locations: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for locations: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch locations: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch locations: %w", err)
	}

	var locations LocationsResponse
	err = json.NewDecoder(resp.Body).Decode(&locations)
	if err != nil {
		return fmt.Errorf("failed to decode locations: %w", err)
	}

	ll.mu.Lock()
	ll.locations = locations
	ll.mu.Unlock()

	return nil
}

func (ll *LocationsList) GetLocations() LocationsResponse {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	return ll.locations
}
