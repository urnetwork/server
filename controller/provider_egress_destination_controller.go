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

	"github.com/urnetwork/operator-proxy/egresshealth"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The pool as data (connect/GEOMAP.md §11.4): this file joins the pool's rows
// to the prober's wire. The prober's Destination is the load contract on both
// sides -- the seed is converted from its built-in table, the candidates of
// egress-sites.yml are validated with its own rules, and the route serves its
// Pool type -- so a destination the server stores is one the prober can run,
// or it is refused before it is stored.

// What a built-in destination is representative of.
type providerEgressSiteMeta struct {
	category string
	region   string
}

// providerEgressBuiltinSiteMeta assigns the built-in table's categories and
// regions, which the module's table does not carry: the refresh promotes a
// candidate of the category a retired site leaves, so every seeded row needs
// one. The non-site classes are one category each, named for what the class
// holds; the sites use the category vocabulary of egress-sites.yml.
var providerEgressBuiltinSiteMeta = func() map[string]providerEgressSiteMeta {
	meta := map[string]providerEgressSiteMeta{}
	assign := func(category string, region string, names ...string) {
		for _, name := range names {
			meta[name] = providerEgressSiteMeta{category: category, region: region}
		}
	}
	assign("resolver", "global", "cloudflare-doh", "google-doh", "adguard-doh", "dnssb-doh", "nextdns-doh")
	assign("resolver", "east-asia", "alidns-doh", "dnspod-doh")
	assign("portal", "global",
		"google-generate-204", "google-connectivitycheck", "gstatic-204", "apple-captive-portal",
		"ubuntu-connectivity-check", "firefox-detectportal", "gnome-nm-check", "cloudflare-cp-204",
		"example-com-https",
	)
	assign("echo", "global", "cloudflare-trace", "aws-checkip", "ifconfig-me", "icanhazip", "ipify")
	assign("cdn", "global",
		"cloudflare-cdn", "cloudflare-sized-1kb", "fastly", "jsdelivr-fastly-mirror", "unpkg",
		"google-hosted-libraries", "microsoft-azure-cdn", "amazon-cloudfront", "keycdn", "cdn77",
		"sucuri-cdn", "cachefly",
	)
	assign("mirror", "global", "kernel-org-mirrors", "debian-cdn", "ubuntu-archive-plain-http", "alpine-cdn", "fedora-downloads")
	assign("mirror", "europe", "ovh-proof-eu")

	assign("search", "global", "google", "bing", "yahoo", "duckduckgo", "ecosia")
	assign("search", "east-asia", "baidu", "naver")
	assign("search", "russia", "yandex", "mail-ru")
	assign("social", "global",
		"facebook", "instagram", "twitter-x", "linkedin", "tiktok", "pinterest", "snapchat", "discord",
		"telegram", "whatsapp", "mastodon", "bluesky", "reddit",
	)
	assign("social", "russia", "vk")
	assign("social", "east-asia", "line", "qq")
	assign("video", "global",
		"youtube", "netflix", "twitch", "vimeo", "hulu", "disney", "spotify", "soundcloud", "dailymotion",
	)
	assign("shopping", "global", "amazon", "ebay", "walmart", "target", "shopify", "etsy")
	assign("shopping", "east-asia", "alibaba", "aliexpress", "taobao", "jd-com")
	assign("shopping", "latin-america", "mercadolibre")
	assign("news", "global", "cnn", "bbc", "new-york-times", "the-guardian", "ap-news", "reuters")
	assign("news", "middle-east", "al-jazeera")
	assign("news", "europe", "deutsche-welle", "france-24")
	assign("news", "east-asia", "sina")
	assign("news", "south-asia", "times-of-india")
	assign("news", "latin-america", "globo")
	assign("news", "oceania", "abc-australia")
	assign("news", "africa", "news24")
	assign("documentation", "global",
		"github", "github-api", "gitlab", "docker-hub", "npm-registry", "pypi", "rubygems", "python-org",
		"go-dev", "kernel-org", "mdn", "stack-overflow", "wikipedia", "wordpress", "internet-archive",
	)
	assign("reference", "global", "imdb", "noaa-weather")
	assign("technology", "global",
		"cloudflare-one-one-one-one", "cloudflare-speed-test", "microsoft", "apple", "cloudflare", "aws",
		"google-cloud", "digitalocean", "akamai",
	)
	assign("productivity", "global", "zoom", "slack", "dropbox", "notion", "trello", "atlassian", "figma", "canva")
	assign("gaming", "global", "steam", "playstation", "xbox", "nintendo", "roblox", "riot-games", "epic-games")
	return meta
}()

// providerEgressBuiltinUrlReplacements re-points the two built-in cdn entries
// whose objects cost real wire bytes now that no request carries a Range header
// (the prober reads a kilobyte and closes, but the server has a receive window
// in flight by then): cachefly's 10 MB test file and the AWS SDK bundle. Each is
// replaced by a small object the same operator serves -- CacheFly's own
// robots.txt through its edge, and a CloudFront-served AWS favicon -- so the
// class keeps the same operators at a fraction of the cost. Measured
// 2026-09-24: 174 and 1150 bytes, both 200 with a body.
var providerEgressBuiltinUrlReplacements = map[string]string{
	"cachefly":          "https://www.cachefly.com/robots.txt",
	"amazon-cloudfront": "https://a0.awsstatic.com/libra-css/images/site/fav/favicon.ico",
}

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

// The pool's first contents: the prober's built-in table, with the categories
// and regions above and the two cdn entries re-pointed at small objects. A
// built-in entry the category table does not name is still seeded, filed under
// its class, so a prober release that adds a destination never makes the seed
// fail.
func ProviderEgressDestinationSeed() ([]*model.ProviderEgressDestination, error) {
	seed := []*model.ProviderEgressDestination{}
	for _, destination := range egresshealth.Destinations() {
		if url, ok := providerEgressBuiltinUrlReplacements[destination.Name]; ok {
			destination.Url = url
		}
		meta, ok := providerEgressBuiltinSiteMeta[destination.Name]
		if !ok {
			meta = providerEgressSiteMeta{category: string(destination.Class), region: "global"}
		}
		row, err := ProviderEgressDestinationFromWire(destination, meta.category, meta.region, 0)
		if err != nil {
			return nil, err
		}
		row.Source = model.ProviderEgressDestinationSourceBuiltin
		seed = append(seed, row)
	}
	return seed, nil
}

// Seeds an empty pool from the built-in table, and reports whether it did.
func EnsureProviderEgressDestinationsSeeded(ctx context.Context) (bool, error) {
	seed, err := ProviderEgressDestinationSeed()
	if err != nil {
		return false, err
	}
	return model.SeedProviderEgressDestinations(ctx, seed), nil
}

// The parsed egress-sites.yml: the refresh settings, the request profile the
// prober loads sites with, and the operator's candidates.
type ProviderEgressSites struct {
	Settings *model.ProviderEgressSiteSettings
	// Profile is the served request profile; the prober module's default when
	// the file names none.
	Profile egresshealth.RequestProfile
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
	Profile *struct {
		UserAgent string            `yaml:"user_agent"`
		Headers   map[string]string `yaml:"headers"`
	} `yaml:"profile"`
	Candidates []providerEgressSitesCandidateYaml `yaml:"candidates"`
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
	sites := &ProviderEgressSites{
		Settings: settings,
		Profile:  egresshealth.DefaultRequestProfile(),
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

	problems := []string{}
	seen := map[string]bool{}
	for i, entry := range document.Candidates {
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
		sites.Candidates = append(sites.Candidates, row)
	}
	if 0 < len(problems) {
		return nil, errors.New("egress sites: " + strings.Join(problems, "; "))
	}
	return sites, nil
}

// Reads the deployment's egress-sites.yml: the defaults and no candidates when
// it is absent, an error when it is present and unusable.
func LoadProviderEgressSites() (*ProviderEgressSites, error) {
	resource, err := server.Config.SimpleResource(model.ProviderEgressSitesResourceName)
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) {
			return &ProviderEgressSites{
				Settings: model.DefaultProviderEgressSiteSettings(),
				Profile:  egresshealth.DefaultRequestProfile(),
			}, nil
		}
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
// seeds an empty pool from the built-in table, so the route never serves
// nothing for want of a refresh having run.
func GetProviderEgressDestinationPool(ctx context.Context) (*egresshealth.Pool, error) {
	if _, err := EnsureProviderEgressDestinationsSeeded(ctx); err != nil {
		return nil, err
	}
	sites, err := LoadProviderEgressSites()
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	return BuildProviderEgressDestinationPool(
		model.GetProviderEgressDestinations(ctx),
		sites.Profile,
		server.NowUtc(),
		sites.Settings.SiteRegionCanaryShare,
		rng,
	)
}
