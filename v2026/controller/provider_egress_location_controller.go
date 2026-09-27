package controller

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/probeverdict"
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

type SubmitProviderEgressLocationArgs struct {
	ClientId server.Id `json:"client_id"`
	// The address the operator's own /ip echo (GET /my-ip-info) saw the probe
	// come from through the provider's tunnel. It is the whole
	// submission (connect/GEOMAP.md §11.3): the server places it with its own
	// GeoLite2, and it is never stored.
	ExitIp     string    `json:"exit_ip"`
	ObservedAt time.Time `json:"observed_at"`

	// The fields below are what the prober's retired vendor consensus used to
	// fill. The prober still sends country_code, country and
	// country_confident, always empty, for one release, and an older prober
	// sends all of them; they are declared so such a body decodes, and are
	// ignored. A body without exit_ip is refused whatever these say.
	CountryCode      string `json:"country_code,omitempty"`
	Country          string `json:"country,omitempty"`
	Region           string `json:"region,omitempty"`
	City             string `json:"city,omitempty"`
	ASN              int    `json:"asn,omitempty"`
	Org              string `json:"org,omitempty"`
	Hosting          bool   `json:"hosting,omitempty"`
	Proxy            bool   `json:"proxy,omitempty"`
	Mobile           bool   `json:"mobile,omitempty"`
	CountryConfident bool   `json:"country_confident,omitempty"`
	CityConfident    bool   `json:"city_confident,omitempty"`
}

type SubmitProviderEgressLocationResult struct {
	LocationId server.Id `json:"location_id"`
}

// Where GeoLite2 places a probed exit address: the location row to store it
// at, not yet created, and how precisely.
type ProviderEgressExit struct {
	// Location is the GeoLite2 place at the granularity the lookup supports:
	// the city when GeoLite2 places the address within the confident radius,
	// else its region, else its country.
	Location *model.Location
	// CountryCode is lowercase alpha-2.
	CountryCode string
	// CityConfident is whether Location is the city: GeoLite2 names one and
	// its accuracy radius is at most ProviderEgressRules.CityConfidentRadiusKm.
	CityConfident bool
	// AccuracyRadiusKm is GeoLite2's radius, 0 when it gives none.
	AccuracyRadiusKm int
}

// Places an exit address with the server's own GeoLite2 (connect/GEOMAP.md §3,
// §11.3), exactly as a connection's address is placed (GetLocationForIp), so a
// probed location and a looked-up one are the same kind of fact from the same
// database. It is the only place the exit is resolved: the location ingest
// stores its answer, and the prober's metrics label a submission with it.
//
// A city is kept only when GeoLite2 places the address within
// cityConfidentRadiusKm. provider_egress_location's documented invariant is
// that location_id is a city row exactly when city_confident is set, and
// probedLocationPreferred reads the granularity off that flag, so a place too
// coarse for its city is stored at its region, or its country, and a probe can
// correct a provider's country without pinning it to a city GeoLite2 is unsure
// of.
func ResolveProviderEgressExit(exitIp string, cityConfidentRadiusKm int) (*ProviderEgressExit, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(exitIp))
	if err != nil {
		return nil, fmt.Errorf("The exit address does not parse.")
	}
	ipInfo, err := server.GetIpInfo(addr.Unmap())
	if err != nil {
		return nil, fmt.Errorf("GeoLite2 has no place for the exit address.")
	}
	return providerEgressExitFromIpInfo(ipInfo, cityConfidentRadiusKm)
}

// The place and precision one GeoLite2 record gives an exit, with no lookup:
// the city only within the confident radius, else the region, else the
// country.
func providerEgressExitFromIpInfo(ipInfo *server.IpInfo, cityConfidentRadiusKm int) (*ProviderEgressExit, error) {
	countryCode := strings.ToLower(strings.TrimSpace(ipInfo.CountryCode))
	if len(countryCode) != 2 || strings.TrimSpace(ipInfo.Country) == "" {
		return nil, fmt.Errorf("GeoLite2 places the exit address in no country.")
	}

	cityConfident := strings.TrimSpace(ipInfo.City) != "" &&
		0 < ipInfo.AccuracyRadiusKm && ipInfo.AccuracyRadiusKm <= cityConfidentRadiusKm

	location := &model.Location{
		Region:           ipInfo.Region,
		Country:          ipInfo.Country,
		CountryCode:      countryCode,
		Continent:        ipInfo.Continent,
		ContinentCode:    ipInfo.ContinentCode,
		Latitude:         ipInfo.Latitude,
		Longitude:        ipInfo.Longitude,
		Timezone:         ipInfo.Timezone,
		RegionGeonameId:  ipInfo.RegionGeonameId,
		CountryGeonameId: ipInfo.CountryGeonameId,
	}
	if cityConfident {
		location.City = ipInfo.City
		location.CityGeonameId = ipInfo.CityGeonameId
	}
	locationType, err := location.GuessLocationType()
	if err != nil {
		return nil, err
	}
	location.LocationType = locationType

	return &ProviderEgressExit{
		Location:         location,
		CountryCode:      countryCode,
		CityConfident:    cityConfident,
		AccuracyRadiusKm: ipInfo.AccuracyRadiusKm,
	}, nil
}

// Joins the submission's resolved country and the row it
// is about to replace into probeverdict's Input. It is the only place the two
// are joined; the rules themselves live in the probeverdict package.
//
// The country is GeoLite2's own answer for the exit, so it is always
// confident: there is no second source for it to disagree with any more
// (GEOMAP D24). What remains is the history rule -- a country that flips
// within a day of the last probe is suspect.
//
// previous is the row currently stored for this provider, or nil when the
// provider has never been probed successfully.
func providerEgressVerdict(
	countryCode string,
	previous *model.ProviderEgressLocation,
) probeverdict.Verdict {
	in := probeverdict.Input{
		CountryConfident: true,
		CountryCode:      countryCode,
		// the server's clock, not the prober's: the age of the stored history
		// is a server-side judgement
		Now: server.NowUtc(),
	}
	if previous != nil {
		in.PreviousCountryCode = previous.CountryCode
		in.PreviousObservedAt = previous.ObservedAt
	}
	return probeverdict.Evaluate(in)
}

// Records where a provider's traffic exits: the
// address the operator's own /ip echo saw through the provider's tunnel,
// placed with the server's GeoLite2 (connect/GEOMAP.md §11.3).
//
// The address itself is never stored -- only the location row it resolves
// to, its country and whether it was placed to a city. No ip-intelligence
// verdict is recorded either: hosting, proxy and mobile are not known and are
// written false, and the columns go one release later.
//
// A submission without exit_ip is the retired vendor-consensus shape. It is
// refused rather than stored: the prober and the server deploy together, and a
// country an older prober asserted is not the server's own lookup.
func SubmitProviderEgressLocation(
	ctx context.Context,
	args *SubmitProviderEgressLocationArgs,
) (*SubmitProviderEgressLocationResult, error) {
	if strings.TrimSpace(args.ExitIp) == "" {
		return nil, fmt.Errorf("Missing exit_ip: submit the address the operator's /ip echo saw through the tunnel; vendor-consensus locations are no longer accepted.")
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

	exit, err := ResolveProviderEgressExit(args.ExitIp, model.GetProviderEgressRules().CityConfidentRadiusKm)
	if err != nil {
		// the message names no address: the error reaches the prober's logs
		// and the address is never kept anywhere
		return nil, err
	}

	// GeoLite2's names are the canonical ones the location table is seeded
	// from (connect/GEOMAP.md §4), so the row is created exactly as the mmdb
	// path in SetConnectionLocation creates it. The vendor spellings the
	// consensus path had to match against existing rows have no source here
	// any more, and that matcher is gone with them.
	model.CreateLocation(ctx, exit.Location)

	// judge the submission against the history it is about to replace; the
	// upsert below overwrites that history, so it is read first
	previous := model.GetProviderEgressLocation(ctx, args.ClientId)
	verdict := providerEgressVerdict(exit.CountryCode, previous)

	model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
		ClientId:      args.ClientId,
		LocationId:    exit.Location.LocationId,
		CountryCode:   exit.CountryCode,
		CityConfident: exit.CityConfident,
		ObservedAt:    args.ObservedAt,
		Verdict:       verdict.State,
		VerdictReason: verdict.Reason,
		// assurance stays at the model default (`direct`): this probe reached
		// the provider over a single tunnel from the prober
	})

	return &SubmitProviderEgressLocationResult{LocationId: exit.Location.LocationId}, nil
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
