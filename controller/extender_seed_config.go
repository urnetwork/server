package controller

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// The public seed bundle (connect/EXTENDER-UPDATE2.md §2): the `seed` block of
// `extender.yml`.
//
//	seed:
//	  enabled: false
//	  record_count: 8
//	  mirrors:
//	    - name: origin
//	      url: https://api.ur.example/v1/seed.bin
//	      dial_addresses:
//	        - 203.0.113.10
//	        - 2001:db8::10
//
// enabled is the control. Off, the default, the publisher signs and stores
// nothing, the api answers 404 at ExtenderSeedPath and the mirror check probes
// nothing. The block holds no key material: the bundle is signed with the root
// key the rest of `extender.yml` already holds, and a mirror receives only the
// signed artifact.
//
// record_count is the cohort size: at most the envelope's limit of eight
// records, and at least the client's low-water mark of four (connect
// ExtenderNetworkClientSettings.LowWaterCount), since a client holding fewer
// usable records runs its dns bootstrap first, which can stall for 30 seconds
// where DoH is black-holed. Unset is eight. A pool with fewer open extenders
// gets a shorter cohort, which grows within the hour once the pool does.
//
// record_lifetime_hours is how long the bundle's records, and so the bundle,
// stay valid. A bundle never outlives its records (§2.3), so the publisher
// signs the cohort's records fresh at each publish with this lifetime, and the
// bundle's validity is the same span. Unset is ExtenderRecordExpireTimeout, the
// day every other record lives, which keeps the seed's exposure exactly the
// record's. It is clamped to at least twice ExtenderSeedRefreshTimeout, so a
// bundle always outlives the publish interval, and at most
// connect.ExtenderSeedMaxValidity. A longer lifetime is what makes a bundle
// embedded in an application release usable after it was built; it also keeps
// a seed record dialable that long after its host fails or turns bad, with
// revocations as the only remedy (an owner decision).
//
// mirrors are what the mirror check probes and reports on (§2.3). A name is
// the only thing about a mirror that reaches a metric label or an alert, so it
// is a short fixed word; the url and the addresses stay in this file. Each
// dial address is a DNS-independent route: it is dialed directly, with the
// url's host as the TLS server name, and the certificate is verified under the
// roots a client verifies mirrors with, connect's pinned Let's Encrypt set
// (taskworker/work/extender_seed_mirror_check.go), so a mirror needs a Let's
// Encrypt certificate. A client given no address for a mirror resolves its
// name with the device's system resolver, not DoH, so the address routes are
// the ones that matter. A mirror carries at most one address per family,
// because the check reports a route by its family and two addresses of one
// family would share a series.
type ExtenderSeedConfig struct {
	Enabled             bool                       `yaml:"enabled"`
	RecordCount         int                        `yaml:"record_count"`
	RecordLifetimeHours int                        `yaml:"record_lifetime_hours"`
	Mirrors             []ExtenderSeedMirrorConfig `yaml:"mirrors"`
}

// How often the publisher replaces the stored bundle when nothing else asks
// for a new one. A quarter of the default validity: a healthy mirror always
// serves a bundle with at least three quarters of its validity left, and the
// origin can miss three publishes before a bundle lapses.
const ExtenderSeedRefreshTimeout = 6 * time.Hour

// The validity of the bundle and the lifetime of its records, from
// record_lifetime_hours with its default and clamps.
func (self *ExtenderSeedConfig) RecordLifetime() time.Duration {
	if self.RecordLifetimeHours <= 0 {
		return ExtenderRecordExpireTimeout
	}
	lifetime := time.Duration(self.RecordLifetimeHours) * time.Hour
	return min(max(lifetime, 2*ExtenderSeedRefreshTimeout), connect.ExtenderSeedMaxValidity)
}

// One configured mirror of the seed bundle.
type ExtenderSeedMirrorConfig struct {
	Name          string   `yaml:"name"`
	Url           string   `yaml:"url"`
	DialAddresses []string `yaml:"dial_addresses"`
}

// The fixed path every mirror serves the bundle at, the api's included: the
// raw serialized envelope, `application/octet-stream`. One host serves one
// network's bundle there. The version in the path lets a later envelope format
// move to its own path while older clients keep reading theirs.
const ExtenderSeedPath = "/v1/seed.bin"

// The content type of the bundle on every mirror.
const ExtenderSeedContentType = "application/octet-stream"

// How long a mirror or a cache may hold one answer. The bundle is replaced
// every ExtenderSeedRefreshTimeout, and a client rejects a sequence lower than
// one it accepted, so a short cache costs only how soon a revocation in a new
// bundle is seen.
const ExtenderSeedCacheMaxAge = 5 * time.Minute

// Bounds of the mirror list the check probes.
const (
	ExtenderSeedMaxMirrorCount = 16
	extenderSeedMirrorNameMax  = 32
)

// A mirror name: lower case letters, digits and inner hyphens, at most
// extenderSeedMirrorNameMax long. It is a metric label value, so it is never
// an address or a host.
var extenderSeedMirrorNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)

// One mirror route the check probes: the mirror's DNS route, or one dial
// address. Route is "dns", "ipv4" or "ipv6", a bounded label.
type ExtenderSeedMirrorRoute struct {
	Mirror string
	Route  string
	Url    *url.URL
	// the address dialed in place of resolving the url's host; invalid for
	// the dns route
	DialAddress netip.Addr
}

// The route labels a mirror check reports.
const (
	ExtenderSeedRouteDns  = "dns"
	ExtenderSeedRouteIpv4 = "ipv4"
	ExtenderSeedRouteIpv6 = "ipv6"
)

// MirrorRoutes is every route of every configured mirror, or an error naming
// the first mirror that is not usable. Nothing is probed for a configuration
// that does not parse whole: a partial list would report the dropped mirror as
// absent rather than as misconfigured.
func (self *ExtenderSeedConfig) MirrorRoutes() ([]*ExtenderSeedMirrorRoute, error) {
	if ExtenderSeedMaxMirrorCount < len(self.Mirrors) {
		return nil, fmt.Errorf("the seed block lists %d mirrors, more than %d", len(self.Mirrors), ExtenderSeedMaxMirrorCount)
	}
	routes := []*ExtenderSeedMirrorRoute{}
	names := map[string]bool{}
	for i, mirror := range self.Mirrors {
		name := strings.TrimSpace(mirror.Name)
		if extenderSeedMirrorNameMax < len(name) || !extenderSeedMirrorNamePattern.MatchString(name) {
			return nil, fmt.Errorf("seed mirror %d has no usable name", i)
		}
		if names[name] {
			return nil, fmt.Errorf("seed mirror %s is listed twice", name)
		}
		names[name] = true
		mirrorUrl, err := url.Parse(strings.TrimSpace(mirror.Url))
		if err != nil || mirrorUrl.Scheme != "https" || mirrorUrl.Hostname() == "" || mirrorUrl.User != nil || mirrorUrl.Fragment != "" {
			return nil, fmt.Errorf("seed mirror %s has no usable https url", name)
		}
		if _, err := netip.ParseAddr(mirrorUrl.Hostname()); err == nil {
			// a bare address has no server name to verify a certificate for;
			// an address route is a dial address under a named url
			return nil, fmt.Errorf("seed mirror %s names an address as its host", name)
		}
		routes = append(routes, &ExtenderSeedMirrorRoute{
			Mirror: name,
			Route:  ExtenderSeedRouteDns,
			Url:    mirrorUrl,
		})
		families := map[string]bool{}
		for _, dialAddress := range mirror.DialAddresses {
			address, err := netip.ParseAddr(strings.TrimSpace(dialAddress))
			if err != nil || !address.IsValid() || address.IsUnspecified() || address.Zone() != "" {
				return nil, fmt.Errorf("seed mirror %s has an unusable dial address", name)
			}
			address = address.Unmap()
			route := ExtenderSeedRouteIpv6
			if address.Is4() {
				route = ExtenderSeedRouteIpv4
			}
			if families[route] {
				return nil, fmt.Errorf("seed mirror %s has two %s dial addresses", name, route)
			}
			families[route] = true
			routes = append(routes, &ExtenderSeedMirrorRoute{
				Mirror:      name,
				Route:       route,
				Url:         mirrorUrl,
				DialAddress: address,
			})
		}
	}
	return routes, nil
}
