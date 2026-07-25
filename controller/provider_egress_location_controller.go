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
	if networkId := model.GetNetworkClientNetwork(ctx, args.ClientId); networkId == nil {
		return nil, fmt.Errorf("Unknown client.")
	}

	// resolve to a canonical location row. city granularity only when the
	// probe agreed on a city; otherwise country.
	location := &model.Location{
		LocationType: model.LocationTypeCountry,
		Country:      args.Country,
		CountryCode:  countryCode,
	}
	if args.CityConfident && args.City != "" {
		location = &model.Location{
			LocationType: model.LocationTypeCity,
			City:         args.City,
			Region:       args.Region,
			Country:      args.Country,
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
