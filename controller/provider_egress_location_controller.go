package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/probeverdict"
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

// providerEgressVerdict marshals a submission, and the row it is about to
// replace, into probeverdict's Input. It is the only place the two are joined;
// the rules themselves live in the probeverdict package and are not restated
// here.
//
// previous is the row currently stored for this provider, or nil when the
// provider has never been probed successfully. A nil previous leaves
// PreviousCountryCode empty, which probeverdict reads as "no history to
// contradict" -- a first probe is judged on its consensus alone.
//
// Note what is NOT passed: the mmdb country for the provider's control ip. A
// probed country that disagrees with mmdb is the finding this project exists to
// produce, never a fault, and probeverdict.Input structurally has no field for
// it. Do not add one.
func providerEgressVerdict(
	args *SubmitProviderEgressLocationArgs,
	previous *model.ProviderEgressLocation,
) probeverdict.Verdict {
	in := probeverdict.Input{
		CountryConfident: args.CountryConfident,
		// stored country codes are lowercased (see
		// model.SetProviderEgressLocation), so normalize the submitted one the
		// same way before comparing it against the stored history -- otherwise
		// a prober sending "US" against a stored "us" reads as a country change
		// and every second probe would be suspect.
		CountryCode: strings.ToLower(strings.TrimSpace(args.CountryCode)),
		// the server's clock, not the prober's: the age of the stored history
		// is a server-side judgement, and args.ObservedAt is attacker-adjacent
		// input. Its skew is already bounded, but the bound is a rejection
		// rule, not a licence to measure history against it.
		Now: server.NowUtc(),
	}
	if previous != nil {
		in.PreviousCountryCode = previous.CountryCode
		in.PreviousObservedAt = previous.ObservedAt
	}
	return probeverdict.Evaluate(in)
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

	// resolve to a location row. City granularity only when the probe agreed
	// on a city AND that city already exists in the location table.
	//
	// The probe MUST NOT define new cities or regions. model.CreateLocation
	// dedupes a city on its exact location_name, so an unrecognised spelling
	// does not fail -- it silently inserts a new permanent row into the shared
	// `location` table and adds it to the search index. The three free
	// geolocation sources the prober reaches consensus over demonstrably
	// disagree on spelling ("Frankfurt am Main (Innenstadt I)" vs "Frankfurt am
	// Main" for the same host, observed), and the consensus keeps the winning
	// source's original display string -- so "Frankfurt am Main", "Frankfurt Am
	// Main" and "Frankfurt/Main" would each become their own row. Those rows
	// survive a code revert and there is no cleanup path.
	//
	// model.MatchExistingLocation therefore matches only, never creates,
	// case-insensitively and ignoring punctuation/whitespace/accents and
	// parenthesised district qualifiers, so the ordinary variants -- the
	// "(Innenstadt I)" case above included -- land on the row that is already
	// there. When it does not resolve,
	// this submission falls back to country granularity: country is the
	// granularity this design treats as trustworthy anyway, and losing city
	// precision for one probe is strictly better than permanently polluting a
	// table shared with the provider list and the location search.
	var location *model.Location
	if args.CityConfident {
		location = model.MatchExistingLocation(ctx, countryCode, region, city)
	}

	// city_confident records the granularity of the row actually stored, not
	// what the probe claimed. The schema's documented invariant is that
	// location_id is a city row exactly when city_confident is set (see the
	// provider_egress_location migration), and a city-confident probe whose
	// city did not resolve is stored at country granularity.
	cityConfident := location != nil

	if location == nil {
		// country granularity. This still goes through CreateLocation: a
		// country row is keyed on country_code, so a variant *name* can never
		// produce a second row for the same country the way a variant city name
		// can -- the pollution this guards against is not reachable here. The
		// country row is also the whole point of the fallback, so a probe from
		// a country not yet in the table must not be dropped.
		location = &model.Location{
			LocationType: model.LocationTypeCountry,
			Country:      country,
			CountryCode:  countryCode,
		}
		model.CreateLocation(ctx, location)
	}

	// judge the submission against the history it is about to replace. This is
	// the only call site probeverdict has and the only place a verdict is
	// computed: every geolocation submission already funnels through here, so
	// verdicts fall out of the existing probe cadence with no separate
	// scheduler and no separate endpoint. Before this, every row in the table
	// read the column default `unverified` -- the absence of a judgement, which
	// is indistinguishable from a judgement of "could not verify".
	//
	// The read is the only one: SetProviderEgressLocation below is an upsert,
	// so the previous row has to be fetched before it is overwritten, and the
	// verdict is the only thing that needs it.
	previous := model.GetProviderEgressLocation(ctx, args.ClientId)
	verdict := providerEgressVerdict(args, previous)

	model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
		ClientId:      args.ClientId,
		LocationId:    location.LocationId,
		CountryCode:   countryCode,
		ASN:           args.ASN,
		Org:           args.Org,
		Hosting:       args.Hosting,
		Proxy:         args.Proxy,
		Mobile:        args.Mobile,
		CityConfident: cityConfident,
		ObservedAt:    args.ObservedAt,
		Verdict:       verdict.State,
		VerdictReason: verdict.Reason,
		// assurance stays at the model default (`direct`): this probe reached
		// the provider over a single tunnel from the prober. Multi-hop is P3.
	})

	return &SubmitProviderEgressLocationResult{LocationId: location.LocationId}, nil
}

// maxProbeFailureLen bounds the failure class as submitted:
// provider_egress_probe_attempt.probe_failure is a varchar(64), and rejecting
// an over-long value with a clear error beats letting the insert panic on a
// Postgres "value too long" error and spin in the retry loop.
const maxProbeFailureLen = 64

type RecordProviderEgressProbeAttemptArgs struct {
	ClientId server.Id `json:"client_id"`
	// ProbeFailure is "" when the attempt succeeded, otherwise a short failure
	// class (`contract_failed`, `tunnel_failed`, `no_consensus`, ...).
	ProbeFailure string `json:"probe_failure,omitempty"`
}

type RecordProviderEgressProbeAttemptResult struct {
	AttemptAt time.Time `json:"attempt_at"`
}

// RecordProviderEgressProbeAttempt records that the prober tried this provider.
// A failed attempt defers the provider from the due queue for
// ProviderEgressProbeAttemptBackoff, exactly as a successful probe defers it
// for the (much longer) staleness window -- without this, a provider that
// always fails to probe never gets a provider_egress_location row and so stays
// permanently at the head of the queue, starving every other provider. See
// model.GetProviderEgressLocationDue.
//
// The attempt is timestamped by the server, not the prober: the prober is
// reporting something it just did, and a prober whose clock ran fast could
// otherwise defer a provider far past the backoff window.
func RecordProviderEgressProbeAttempt(
	ctx context.Context,
	args *RecordProviderEgressProbeAttemptArgs,
) (*RecordProviderEgressProbeAttemptResult, error) {
	if maxProbeFailureLen < len(args.ProbeFailure) {
		return nil, fmt.Errorf("Probe failure class is too long.")
	}
	// same check as SubmitProviderEgressLocation: without it a typo'd or stale
	// client id writes a row keyed to a client that does not exist, which
	// nothing ever reads and only the sweep ever removes.
	if networkId := model.GetNetworkClientNetwork(ctx, args.ClientId); networkId == nil {
		return nil, fmt.Errorf("Unknown client.")
	}

	attemptAt := server.NowUtc()
	model.SetProviderEgressProbeAttempt(ctx, &model.ProviderEgressProbeAttempt{
		ClientId:     args.ClientId,
		AttemptAt:    attemptAt,
		ProbeFailure: args.ProbeFailure,
	})

	return &RecordProviderEgressProbeAttemptResult{AttemptAt: attemptAt}, nil
}
