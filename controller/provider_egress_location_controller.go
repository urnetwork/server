package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// MaxProviderEgressLocationSubmissionAge rejects a submission whose probe is
// already older than this when it arrives. It bounds replay of an old probe.
const MaxProviderEgressLocationSubmissionAge = 24 * time.Hour

// MaxProviderEgressLocationSubmissionSkew rejects a submission whose
// observed_at is further in the future than this. The prober and server
// clocks should be roughly in sync, so a few minutes of allowance covers
// ordinary clock drift without opening the door to a far-future timestamp.
// Without this bound, a future observed_at would defeat every other
// safeguard at once: it always wins the monotonic upsert in
// model.SetProviderEgressLocation (so no later, legitimate probe can ever
// overwrite it), it reads as "fresh" forever against
// ProviderEgressLocationMaxAge, and it outlives the taskworker sweep in
// RemoveExpiredProviderEgressLocations -- permanently pinning a provider to
// whatever location was submitted, with no API-side recovery.
const MaxProviderEgressLocationSubmissionSkew = 5 * time.Minute

// maxLocationNameLen bounds country/city/region as submitted: these flow into
// model.CreateLocation, whose location_name column is varchar(128). Rejecting
// an over-long value here with a clear error is preferable to letting
// CreateLocation panic on a Postgres "value too long for type character
// varying(128)" error.
const maxLocationNameLen = 128

// maxOrgLen mirrors maxLocationNameLen for org, which is stored in
// provider_egress_location.org, a varchar(256) column.
const maxOrgLen = 256

type SubmitProviderEgressLocationArgs struct {
	ClientId         server.Id `json:"client_id"`
	CountryCode      string    `json:"country_code"`
	Country          string    `json:"country"`
	Region           string    `json:"region,omitempty"`
	City             string    `json:"city,omitempty"`
	ASN              int       `json:"asn,omitempty"`
	Org              string    `json:"org,omitempty"`
	Hosting          bool      `json:"hosting,omitempty"`
	Proxy            bool      `json:"proxy,omitempty"`
	Mobile           bool      `json:"mobile,omitempty"`
	CountryConfident bool      `json:"country_confident"`
	CityConfident    bool      `json:"city_confident,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
}

type SubmitProviderEgressLocationResult struct {
	LocationId server.Id `json:"location_id"`
}

// SubmitProviderEgressLocation records a probed egress location for a provider.
// Only country-confident submissions are accepted; city/region are stored only
// when the probe was also city-confident (free geolocation sources disagree on
// city often enough that an unconfirmed city is worse than none).
func SubmitProviderEgressLocation(
	ctx context.Context,
	args *SubmitProviderEgressLocationArgs,
) (*SubmitProviderEgressLocationResult, error) {
	if !args.CountryConfident {
		return nil, fmt.Errorf("Submission is not country-confident.")
	}
	countryCode := strings.ToLower(strings.TrimSpace(args.CountryCode))
	if len(countryCode) != 2 {
		return nil, fmt.Errorf("Country code must be alpha-2.")
	}
	if args.ObservedAt.IsZero() {
		return nil, fmt.Errorf("Missing observed_at.")
	}
	if args.ObservedAt.Before(server.NowUtc().Add(-MaxProviderEgressLocationSubmissionAge)) {
		return nil, fmt.Errorf("Submission is too old.")
	}
	if server.NowUtc().Add(MaxProviderEgressLocationSubmissionSkew).Before(args.ObservedAt) {
		return nil, fmt.Errorf("Submission is too far in the future.")
	}
	if networkId := model.GetNetworkClientNetwork(ctx, args.ClientId); networkId == nil {
		return nil, fmt.Errorf("Unknown client.")
	}

	// country is always used to resolve/create a location row (at minimum
	// the country-granular one), and model.CreateLocation dedupes country
	// rows on (location_type, country_code): an empty name here would create
	// a canonical row with location_name='' that every later lookup for this
	// country reuses forever, even after a subsequent real mmdb lookup. Reject
	// rather than silently falling back, so the prober learns it sent a bad
	// payload instead of the server permanently corrupting shared data.
	country := strings.TrimSpace(args.Country)
	if country == "" {
		return nil, fmt.Errorf("Missing country.")
	}
	if maxLocationNameLen < len(country) {
		return nil, fmt.Errorf("Country is too long.")
	}
	if maxOrgLen < len(args.Org) {
		return nil, fmt.Errorf("Org is too long.")
	}

	// city/region are only used (and their rows only created) when the probe
	// was city-confident; the same empty-name corruption applies to them, so
	// require both are present and reject rather than silently dropping to
	// country granularity on a bad payload.
	var city, region string
	if args.CityConfident {
		city = strings.TrimSpace(args.City)
		region = strings.TrimSpace(args.Region)
		if city == "" {
			return nil, fmt.Errorf("Missing city for a city-confident submission.")
		}
		if region == "" {
			return nil, fmt.Errorf("Missing region for a city-confident submission.")
		}
		if maxLocationNameLen < len(city) {
			return nil, fmt.Errorf("City is too long.")
		}
		if maxLocationNameLen < len(region) {
			return nil, fmt.Errorf("Region is too long.")
		}
	}

	// resolve to a canonical location row. city granularity only when the
	// probe agreed on a city; otherwise country.
	location := &model.Location{
		LocationType: model.LocationTypeCountry,
		Country:      country,
		CountryCode:  countryCode,
	}
	if args.CityConfident {
		location = &model.Location{
			LocationType: model.LocationTypeCity,
			City:         city,
			Region:       region,
			Country:      country,
			CountryCode:  countryCode,
		}
	}
	model.CreateLocation(ctx, location)

	model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
		ClientId:      args.ClientId,
		LocationId:    location.LocationId,
		CountryCode:   countryCode,
		ASN:           args.ASN,
		Org:           args.Org,
		Hosting:       args.Hosting,
		Proxy:         args.Proxy,
		Mobile:        args.Mobile,
		CityConfident: args.CityConfident,
		ObservedAt:    args.ObservedAt,
	})

	return &SubmitProviderEgressLocationResult{LocationId: location.LocationId}, nil
}
