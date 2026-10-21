// The server names the alt front dispatches on (EXTENDER.md L3).
//
// A client never presents an alt name. It presents the api or connect name,
// the -v4/-v6 family forms and the <env>- forms included, so the dispatch
// lists are exactly the names the existing api and connect certificates
// already cover and alt issues no certificate of its own. The names are read
// from the same services.yml that warpctl renders the lb and certificate
// configuration from, so an alias added there reaches alt with no code change
// and the two fronts can never claim the same name.
package alt

import (
	"fmt"
	"slices"
	"strings"

	"github.com/urnetwork/server/v2026"
)

const (
	// the services.yml services whose exposed names alt dispatches
	ApiServiceName     = "api"
	ConnectServiceName = "connect"

	servicesResourceName = "services.yml"
)

// Reads the running environment's services config out of the vault.
func LoadServicesConfig() (*ServicesConfig, error) {
	resource, err := server.Vault.SimpleResource(servicesResourceName)
	if err != nil {
		return nil, err
	}
	servicesConfig := &ServicesConfig{}
	if err := resource.UnmarshalYamlE(servicesConfig); err != nil {
		return nil, err
	}
	if len(servicesConfig.Versions) == 0 {
		return nil, fmt.Errorf("%s has no versions", servicesResourceName)
	}
	return servicesConfig, nil
}

// The exposed names of one service in the running environment.
func ServiceHosts(service string) ([]string, error) {
	servicesConfig, err := LoadServicesConfig()
	if err != nil {
		return nil, err
	}
	env, err := server.Env()
	if err != nil {
		return nil, err
	}
	hosts := serviceHosts(servicesConfig, env, service)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s exposes no host for %s", servicesResourceName, service)
	}
	return hosts, nil
}

// `<env>-<service>.<domain>` for every domain, plus the service's own
// aliases and domains. The bare domain and the lb names are deliberately
// excluded: they belong to no service front and alt must not answer for them.
func serviceHosts(servicesConfig *ServicesConfig, env string, service string) []string {
	serviceConfig, ok := servicesConfig.Versions[0].Services[service]
	if !ok || serviceConfig == nil || !serviceConfig.IsExposed() {
		return nil
	}
	hosts := []string{}
	for _, domain := range servicesConfig.DomainNames() {
		hosts = append(hosts, fmt.Sprintf("%s-%s.%s", env, service, domain))
	}
	hosts = append(hosts, serviceConfig.ExposeAliases...)
	hosts = append(hosts, serviceConfig.ExposeDomains...)
	return hosts
}

// One front's set of server names. A `*.x` entry matches exactly one label,
// which is what the wildcard certificate behind the same name covers.
// Matching is case insensitive and ignores a trailing dot, both of which a
// client may put in the sni. Safe for concurrent use after construction.
type HostSet struct {
	names            map[string]bool
	wildcardSuffixes []string
}

func NewHostSet(hosts []string) *HostSet {
	hostSet := &HostSet{
		names:            map[string]bool{},
		wildcardSuffixes: []string{},
	}
	for _, host := range hosts {
		name := normalizeServerName(host)
		if name == "" {
			continue
		}
		if suffix, ok := strings.CutPrefix(name, "*."); ok {
			if suffix != "" {
				hostSet.wildcardSuffixes = append(hostSet.wildcardSuffixes, suffix)
			}
			continue
		}
		hostSet.names[name] = true
	}
	return hostSet
}

func (self *HostSet) Contains(serverName string) bool {
	name := normalizeServerName(serverName)
	// a presented name is never a wildcard, and must not be read as one
	if name == "" || strings.Contains(name, "*") {
		return false
	}
	if self.names[name] {
		return true
	}
	for _, suffix := range self.wildcardSuffixes {
		label, ok := strings.CutSuffix(name, "."+suffix)
		// one label, as in the certificate: a.b.example does not match *.example
		if ok && label != "" && !strings.Contains(label, ".") {
			return true
		}
	}
	return false
}

// The names in a stable order, for logs and tests.
func (self *HostSet) Names() []string {
	names := make([]string, 0, len(self.names)+len(self.wildcardSuffixes))
	for name := range self.names {
		names = append(names, name)
	}
	for _, suffix := range self.wildcardSuffixes {
		names = append(names, "*."+suffix)
	}
	slices.Sort(names)
	return names
}

func normalizeServerName(serverName string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(serverName), "."))
}
