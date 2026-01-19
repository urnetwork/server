package main

import (
	"encoding/json"

	"github.com/urnetwork/connect"
)

type FindLocationsArgs struct {
	Query string `json:"query"`
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction       float32 `json:"max_distance_fraction,omitempty"`
	EnableMaxDistanceFraction bool    `json:"enable_max_distance_fraction,omitempty"`
}

// TODO: this seems to be broken

type FindLocationsResult struct {
	Specs *ProviderSpecList `json:"specs"`
	// this includes groups that show up in the location results
	// all `ProviderCount` are from inside the location results
	// groups are suggestions that can be used to broaden the search
	Groups *LocationGroupResultList `json:"groups"`
	// this includes all parent locations that show up in the location results
	// every `CityId`, `RegionId`, `CountryId` will have an entry
	Locations *LocationResultList       `json:"locations"`
	Devices   *LocationDeviceResultList `json:"devices"`
}

// type FindLocationsResult map[string]interface{}

type ProviderSpecList struct {
	exportedList[*ProviderSpec]
}

type exportedList[T any] struct {
	values []T
}

func (self *exportedList[T]) Values() []T {
	return self.values
}

func (self *exportedList[T]) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, &self.values)
}

func (self *exportedList[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(self.values)
}

type ProviderSpec struct {
	LocationId      *connect.Id `json:"location_id,omitempty"`
	LocationGroupId *connect.Id `json:"location_group_id,omitempty"`
	ClientId        *connect.Id `json:"client_id,omitempty"`
}

type LocationGroupResultList struct {
	exportedList[*LocationGroupResult]
}
type LocationGroupResult struct {
	LocationGroupId *connect.Id `json:"location_group_id"`
	Name            string      `json:"name"`
	ProviderCount   int         `json:"provider_count,omitempty"`
	Promoted        bool        `json:"promoted,omitempty"`
	MatchDistance   int         `json:"match_distance,omitempty"`
}

type LocationResultList struct {
	exportedList[*LocationResult]
}

type LocationType = string

type LocationResult struct {
	LocationId   *connect.Id  `json:"location_id"`
	LocationType LocationType `json:"location_type"`
	Name         string       `json:"name"`
	// FIXME
	City string `json:"city,omitempty"`
	// FIXME
	Region string `json:"region,omitempty"`
	// FIXME
	Country           string      `json:"country,omitempty"`
	CountryCode       string      `json:"country_code,omitempty"`
	CityLocationId    *connect.Id `json:"city_location_id,omitempty"`
	RegionLocationId  *connect.Id `json:"region_location_id,omitempty"`
	CountryLocationId *connect.Id `json:"country_location_id,omitempty"`
	ProviderCount     int         `json:"provider_count,omitempty"`
	MatchDistance     int         `json:"match_distance,omitempty"`
}

type LocationDeviceResultList struct {
	exportedList[*LocationDeviceResult]
}

type LocationDeviceResult struct {
	ClientId   *connect.Id `json:"client_id"`
	DeviceName string      `json:"device_name"`
}
