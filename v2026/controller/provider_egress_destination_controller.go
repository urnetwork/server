package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// URL contracts come from qualityprobe.yml. Stored rows contribute learned
// retirement and place-compatibility state, never a compiled-in target list.

// Converts one prober destination to a pool row, refusing it when the prober
// could not run it (egresshealth's own Destination.Validate) or when its name
// could not be matched back to a row from a failed_names list.
func ProviderEgressDestinationFromWire(
	destination egresshealth.Destination,
	category string,
	region string,
	revision int,
) (*model.ProviderEgressDestination, error) {
	if err := destination.Validate(); err != nil {
		return nil, fmt.Errorf("destination %q: %w", destination.Name, err)
	}
	if err := model.ValidateProviderEgressDestinationName(destination.Name); err != nil {
		return nil, err
	}
	expect, err := destination.Expect.MarshalText()
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", destination.Name, err)
	}
	var headers map[string]string
	if 0 < len(destination.Headers) {
		headers = make(map[string]string, len(destination.Headers))
		for name, value := range destination.Headers {
			headers[name] = value
		}
	}
	incompatible := []model.ProviderEgressDestinationPlace{}
	for _, place := range destination.Incompatible {
		incompatible = append(incompatible, model.ProviderEgressDestinationPlace{
			Country: strings.ToLower(strings.TrimSpace(place.Country)),
			Region:  strings.TrimSpace(place.Region),
		})
	}
	return &model.ProviderEgressDestination{
		Name:     destination.Name,
		Class:    string(destination.Class),
		Url:      destination.Url,
		Expect:   string(expect),
		Status:   destination.Status,
		MaxBytes: destination.MaxBytes,
		Headers:  headers,
		Verify: model.ProviderEgressDestinationVerify{
			Kind: string(destination.Verify.Kind),
			Text: destination.Verify.Text,
		},
		Category:     category,
		Region:       region,
		Incompatible: incompatible,
		Revision:     revision,
	}, nil
}

// A row as the prober loads it. The learned places' bookkeeping (when marked,
// since when the canaries pass) is the server's and is not served.
func ProviderEgressDestinationToWire(row *model.ProviderEgressDestination) (egresshealth.Destination, error) {
	var expect egresshealth.Expect
	if err := expect.UnmarshalText([]byte(row.Expect)); err != nil {
		return egresshealth.Destination{}, fmt.Errorf("destination %q: %w", row.Name, err)
	}
	var headers map[string]string
	if 0 < len(row.Headers) {
		headers = make(map[string]string, len(row.Headers))
		for name, value := range row.Headers {
			headers[name] = value
		}
	}
	var incompatible []egresshealth.Place
	for _, place := range row.Incompatible {
		incompatible = append(incompatible, egresshealth.Place{Country: place.Country, Region: place.Region})
	}
	destination := egresshealth.Destination{
		Name:         row.Name,
		Class:        egresshealth.Class(row.Class),
		Url:          row.Url,
		Headers:      headers,
		Expect:       expect,
		Status:       row.Status,
		MaxBytes:     row.MaxBytes,
		Verify:       egresshealth.BodyCheck{Kind: egresshealth.BodyCheckKind(row.Verify.Kind), Text: row.Verify.Text},
		Incompatible: incompatible,
	}
	if err := destination.Validate(); err != nil {
		return egresshealth.Destination{}, fmt.Errorf("destination %q: %w", row.Name, err)
	}
	return destination, nil
}

// The pool's first contents are the deployment's explicit URL contracts.
// Missing or invalid configuration is an observation failure, never a silent
// fallback to compiled-in destinations.
func ProviderEgressDestinationSeed() ([]*model.ProviderEgressDestination, error) {
	sites, err := LoadProviderEgressSites()
	if err != nil {
		return nil, err
	}
	return sites.Destinations, nil
}

// Seeds an empty pool from configured destinations, and reports whether it did.
func EnsureProviderEgressDestinationsSeeded(ctx context.Context) (bool, error) {
	seed, err := ProviderEgressDestinationSeed()
	if err != nil {
		return false, err
	}
	return model.SeedProviderEgressDestinations(ctx, seed), nil
}

// The parsed qualityprobe.yml: refresh settings, request policy and the
// operator-owned destination, country and candidate catalogs.
type ProviderEgressSites struct {
	Settings *model.ProviderEgressSiteSettings
	// Only these configured URL contracts may be served. Database rows retain
	// learned retirement state, not authority to invent or retain removed URLs.
	Destinations []*model.ProviderEgressDestination
	Countries    map[string][]egresshealth.Destination
	// Profile is the served request profile; the prober module's default when
	// the file names none.
	Profile        egresshealth.RequestProfile
	UrlProbePolicy egresshealth.UrlProbePolicy
	// Candidates are the file's destinations, each validated as the prober
	// would run it.
	Candidates []*model.ProviderEgressDestination
}

// One place of a candidate's incompatible list, as the file spells it.
type providerEgressSitesPlaceYaml struct {
	Country string `yaml:"country"`
	Region  string `yaml:"region"`
}

// One candidate entry, as the file spells it.
type providerEgressSitesCandidateYaml struct {
	Name     string            `yaml:"name"`
	Class    string            `yaml:"class"`
	Category string            `yaml:"category"`
	Region   string            `yaml:"region"`
	Url      string            `yaml:"url"`
	Expect   string            `yaml:"expect"`
	Status   int               `yaml:"status"`
	MaxBytes int               `yaml:"max_bytes"`
	Headers  map[string]string `yaml:"headers"`
	Verify   struct {
		Kind string `yaml:"kind"`
		Text string `yaml:"text"`
	} `yaml:"verify"`
	Incompatible []providerEgressSitesPlaceYaml `yaml:"incompatible"`
	Revision     int                            `yaml:"revision"`
}

// The file's profile and candidates; the settings block is read apart.
type providerEgressSitesYaml struct {
	SchemaVersion  int                          `yaml:"schema_version"`
	UrlProbePolicy *egresshealth.UrlProbePolicy `yaml:"url_probe_policy"`
	Profile        *struct {
		UserAgent string            `yaml:"user_agent"`
		Headers   map[string]string `yaml:"headers"`
	} `yaml:"profile"`
	Candidates   []providerEgressSitesCandidateYaml            `yaml:"candidates"`
	Destinations []providerEgressSitesCandidateYaml            `yaml:"destinations"`
	Countries    map[string][]providerEgressSitesCandidateYaml `yaml:"countries"`
}

// Reads one egress-sites.yml. Every candidate must be one the prober can run
// -- the same Destination.Validate it applies to a served pool -- with a
// category, and a name unique in the file: a broken candidate is refused with
// the file rather than promoted into a pool the prober would then refuse
// whole.
func ParseProviderEgressSites(unmarshal func(any) error) (*ProviderEgressSites, error) {
	settings, err := model.ProviderEgressSiteSettingsFromYaml(unmarshal)
	if err != nil {
		return nil, err
	}
	var document providerEgressSitesYaml
	if err := unmarshal(&document); err != nil {
		return nil, err
	}
	if document.SchemaVersion != 1 || len(document.Destinations) == 0 {
		return nil, errors.New("qualityprobe.yml requires schema_version 1 and a nonempty destinations catalog")
	}
	sites := &ProviderEgressSites{
		Settings:       settings,
		Profile:        egresshealth.DefaultRequestProfile(),
		UrlProbePolicy: egresshealth.DefaultUrlProbePolicy(),
	}
	if document.UrlProbePolicy != nil {
		sites.UrlProbePolicy = *document.UrlProbePolicy
	}
	if err := sites.UrlProbePolicy.Validate(); err != nil {
		return nil, err
	}
	if document.Profile != nil {
		if strings.TrimSpace(document.Profile.UserAgent) == "" {
			// an empty user agent would fall back to the module's default on
			// the prober anyway; refusing it here says so where it was written
			return nil, errors.New("egress sites: profile.user_agent is empty")
		}
		sites.Profile = egresshealth.RequestProfile{
			UserAgent: document.Profile.UserAgent,
			Headers:   document.Profile.Headers,
		}
	}

	parseDestinations := func(entries []providerEgressSitesCandidateYaml) ([]*model.ProviderEgressDestination, error) {
		rows := []*model.ProviderEgressDestination{}
		problems := []string{}
		seen := map[string]bool{}
		for i, entry := range entries {
			var expect egresshealth.Expect
			expectWord := entry.Expect
			if expectWord == "" {
				expectWord = "body"
			}
			if err := expect.UnmarshalText([]byte(expectWord)); err != nil {
				problems = append(problems, fmt.Sprintf("candidate %d (%q): %s", i, entry.Name, err))
				continue
			}
			destination := egresshealth.Destination{
				Name:     entry.Name,
				Class:    egresshealth.Class(entry.Class),
				Url:      entry.Url,
				Headers:  entry.Headers,
				Expect:   expect,
				Status:   entry.Status,
				MaxBytes: entry.MaxBytes,
				Verify:   egresshealth.BodyCheck{Kind: egresshealth.BodyCheckKind(entry.Verify.Kind), Text: entry.Verify.Text},
			}
			for _, place := range entry.Incompatible {
				destination.Incompatible = append(destination.Incompatible, egresshealth.Place{
					Country: strings.TrimSpace(place.Country),
					Region:  strings.TrimSpace(place.Region),
				})
			}
			if strings.TrimSpace(entry.Category) == "" {
				problems = append(problems, fmt.Sprintf("candidate %d (%q): no category", i, entry.Name))
				continue
			}
			if seen[entry.Name] {
				problems = append(problems, fmt.Sprintf("candidate %d repeats the name %q", i, entry.Name))
				continue
			}
			seen[entry.Name] = true
			region := strings.TrimSpace(entry.Region)
			if region == "" {
				region = "global"
			}
			row, err := ProviderEgressDestinationFromWire(destination, strings.TrimSpace(entry.Category), region, entry.Revision)
			if err != nil {
				problems = append(problems, fmt.Sprintf("candidate %d: %s", i, err))
				continue
			}
			row.Source = model.ProviderEgressDestinationSourceCandidates
			rows = append(rows, row)
		}
		if 0 < len(problems) {
			return nil, errors.New("qualityprobe catalog: " + strings.Join(problems, "; "))
		}
		return rows, nil
	}
	if sites.Destinations, err = parseDestinations(document.Destinations); err != nil {
		return nil, err
	}
	if sites.Candidates, err = parseDestinations(document.Candidates); err != nil {
		return nil, err
	}
	sites.Countries = map[string][]egresshealth.Destination{}
	for country, entries := range document.Countries {
		rows, err := parseDestinations(entries)
		if err != nil {
			return nil, fmt.Errorf("country %s: %w", country, err)
		}
		for _, row := range rows {
			destination, err := ProviderEgressDestinationToWire(row)
			if err != nil {
				return nil, err
			}
			sites.Countries[country] = append(sites.Countries[country], destination)
		}
	}
	pool := &egresshealth.Pool{Countries: sites.Countries}
	for _, row := range sites.Destinations {
		destination, err := ProviderEgressDestinationToWire(row)
		if err != nil {
			return nil, err
		}
		pool.Destinations = append(pool.Destinations, destination)
	}
	if err := egresshealth.ValidatePool(pool); err != nil {
		return nil, err
	}
	return sites, nil
}

// Reads qualityprobe.yml; absent or unusable catalogs return an error.
func LoadProviderEgressSites() (*ProviderEgressSites, error) {
	resource, err := server.Config.SimpleResource(model.ProviderEgressSitesResourceName)
	if err != nil {
		return nil, err
	}
	return ParseProviderEgressSites(resource.UnmarshalYamlE)
}

// The pool the prober is served: every active row, probation included (a site
// on probation is loaded and recorded; what it may not do is count, which
// ingest decides), in name order, with the profile. A row with places it is
// incompatible with is served as a canary in canaryShare of pool fetches --
// each pass fetches once, so about that share of the runs from a marked place
// load it, unscored, which is how the refresh learns it works there again
// (GEOMAP §11.4).
//
// Version identifies the contents, canary draws aside, so a log line can say
// which pool a pass ran. The pool is validated as the prober validates it: a
// pool it would refuse whole is refused here, so the prober's fallback is the
// server's decision and says why.
func BuildProviderEgressDestinationPool(
	rows []*model.ProviderEgressDestination,
	profile egresshealth.RequestProfile,
	now time.Time,
	canaryShare float64,
	rng *rand.Rand,
) (*egresshealth.Pool, error) {
	active := []*model.ProviderEgressDestination{}
	for _, row := range rows {
		if row.Active {
			active = append(active, row)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Name < active[j].Name })

	destinations := make([]egresshealth.Destination, 0, len(active))
	for _, row := range active {
		destination, err := ProviderEgressDestinationToWire(row)
		if err != nil {
			return nil, err
		}
		destinations = append(destinations, destination)
	}
	if err := egresshealth.ValidateDestinations(destinations); err != nil {
		return nil, err
	}

	versionJson, err := json.Marshal(struct {
		Destinations []egresshealth.Destination  `json:"destinations"`
		Profile      egresshealth.RequestProfile `json:"profile"`
	}{Destinations: destinations, Profile: profile})
	if err != nil {
		return nil, err
	}
	hash := fnv.New32a()
	hash.Write(versionJson)
	version := int(hash.Sum32() & 0x7fffffff)

	for i := range destinations {
		if 0 < len(destinations[i].Incompatible) && rng != nil && rng.Float64() < canaryShare {
			destinations[i].Canary = true
		}
	}

	return &egresshealth.Pool{
		Version:      version,
		GeneratedAt:  now.UTC(),
		Destinations: destinations,
		Profile:      profile,
	}, nil
}

// Serves the pool (GET /network/provider-egress-destinations). A first request
// seeds an empty pool from the configured contracts without waiting for the
// refresh task; every response intersects stored state with current config.
func GetProviderEgressDestinationPool(ctx context.Context) (*egresshealth.Pool, error) {
	if _, err := EnsureProviderEgressDestinationsSeeded(ctx); err != nil {
		return nil, err
	}
	sites, err := LoadProviderEgressSites()
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	pool, err := BuildProviderEgressDestinationPool(
		sites.configuredRows(model.GetProviderEgressDestinations(ctx)),
		sites.Profile,
		server.NowUtc(),
		sites.Settings.SiteRegionCanaryShare,
		rng,
	)
	if err != nil {
		return nil, err
	}
	pool.Countries = sites.Countries
	pool.UrlProbePolicy = &sites.UrlProbePolicy
	if err := egresshealth.ValidatePool(pool); err != nil {
		return nil, err
	}
	// Country publication is part of the same generation as global contracts.
	contents, err := json.Marshal(struct {
		GeneralVersion int                                   `json:"general_version"`
		Countries      map[string][]egresshealth.Destination `json:"countries"`
		UrlProbePolicy egresshealth.UrlProbePolicy           `json:"url_probe_policy"`
	}{GeneralVersion: pool.Version, Countries: pool.Countries, UrlProbePolicy: sites.UrlProbePolicy})
	if err != nil {
		return nil, err
	}
	hash := fnv.New32a()
	hash.Write(contents)
	pool.Version = int(hash.Sum32() & 0x7fffffff)
	return pool, nil
}

// Config owns URL contracts; stored rows contribute only learned pool state.
// Removing an entry takes effect immediately even if a stale DB row is active.
func (self *ProviderEgressSites) configuredRows(stored []*model.ProviderEgressDestination) []*model.ProviderEgressDestination {
	byName := map[string]*model.ProviderEgressDestination{}
	for _, row := range stored {
		byName[row.Name] = row
	}
	configured := map[string]*model.ProviderEgressDestination{}
	for _, row := range self.Destinations {
		configured[row.Name] = row
	}
	for _, row := range self.Candidates {
		if _, active := configured[row.Name]; !active {
			if existing := byName[row.Name]; existing == nil || !existing.Active {
				continue
			}
		}
		configured[row.Name] = row
	}
	rows := make([]*model.ProviderEgressDestination, 0, len(configured))
	for name, row := range configured {
		copy := *row
		copy.Active = true
		if existing := byName[name]; existing != nil {
			copy.Active, copy.Probation = existing.Active, existing.Probation
			for _, place := range existing.Incompatible {
				if place.MarkedAt != nil {
					copy.Incompatible = append(append([]model.ProviderEgressDestinationPlace(nil), copy.Incompatible...), place)
				}
			}
		}
		rows = append(rows, &copy)
	}
	return rows
}
