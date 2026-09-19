package alt

// The subset of warp's services.yml that alt reads. warp owns the document
// and its full model (warp/services); server does not depend on the warp
// module, so alt carries the shape of just the fields it dispatches and
// limits on, with warp's yaml keys and defaults. The decoder ignores the rest
// of the document, and a field alt comes to need is added here.

import (
	"maps"
	"net/netip"
	"slices"
	"strings"
)

type ServicesConfig struct {
	Domain string `yaml:"domain,omitempty"`
	// domain to registrar map
	Domains map[string]string `yaml:"domains,omitempty"`
	// the document-level limits the lb blocks alias
	DefaultRateLimit *RateLimit               `yaml:"default_rate_limit,omitempty"`
	Versions         []*ServicesConfigVersion `yaml:"versions,omitempty"`
}

type ServicesConfigVersion struct {
	Services map[string]*ServiceConfig `yaml:"services,omitempty"`
}

type ServiceConfig struct {
	ExposeAliases []string `yaml:"expose_aliases,omitempty"`
	ExposeDomains []string `yaml:"expose_domains,omitempty"`
	Exposed       *bool    `yaml:"exposed,omitempty"`
}

// DomainNames is every domain of the document, the primary first and the
// rest in name order, as warpctl orders them.
func (self *ServicesConfig) DomainNames() []string {
	domains := map[string]bool{}
	if self.Domain != "" {
		domains[self.Domain] = true
	}
	for domain := range self.Domains {
		domains[domain] = true
	}
	orderedDomains := slices.Collect(maps.Keys(domains))
	slices.SortFunc(orderedDomains, func(a string, b string) int {
		if a == b {
			return 0
		}
		if a == self.Domain {
			return -1
		}
		if b == self.Domain {
			return 1
		}
		return strings.Compare(a, b)
	})
	return orderedDomains
}

func (self *ServiceConfig) IsExposed() bool {
	// default true
	return self.Exposed == nil || *self.Exposed
}

// One `rate_limit` block, in the shape warpctl renders into nginx.
type RateLimit struct {
	RequestsPerSecond int      `yaml:"requests_per_second,omitempty"`
	RequestsPerMinute int      `yaml:"requests_per_minute,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
	Delay             int      `yaml:"delay,omitempty"`
	NetConnections    int      `yaml:"net_connections,omitempty"`
	ExcludeSubnets    []string `yaml:"exclude_subnets,omitempty"`
}

// DefaultRateLimit is what warpctl applies to an lb block that declares no
// rate limit of its own.
func DefaultRateLimit() *RateLimit {
	return &RateLimit{
		RequestsPerMinute: 120,
		Burst:             120,
		Delay:             30,
	}
}

func (self *RateLimit) ExcludePrefixes() []netip.Prefix {
	prefixes := []netip.Prefix{}
	for _, subnet := range self.ExcludeSubnets {
		prefix := netip.MustParsePrefix(subnet)
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}
