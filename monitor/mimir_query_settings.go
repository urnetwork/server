// Native query-role selection through the server-owned Config/Vault Resolver.
// This narrow model mirrors Warp's ordinary role / vault password separation;
// it neither imports Warp nor consults a second configuration search path.
package monitor

import (
	"slices"
	"sort"
	"strings"

	"github.com/urnetwork/server"
)

// Unknown fields belong to other Grafana consumers and do not grant query role.
type mimirQueryConfig struct {
	Users []*mimirQueryUser `yaml:"users"`
}

// The two resources share a shape but have disjoint authority for these fields.
type mimirQueryUser struct {
	Name     string   `yaml:"name"`
	Password string   `yaml:"password"`
	Roles    []string `yaml:"roles"`
}

// Fail soft at settings assembly, retaining no parser text or partial identity.
// Dependent probes report configuration unavailable instead of contacting SSH.
func loadMimirQuerySettings(environment string, services servicesYaml) (string, string, string) {
	ordinaryResource, err := server.Config.SimpleResource("grafana.yml")
	if err != nil {
		return "", "", ""
	}
	secretResource, err := server.Vault.SimpleResource("grafana.yml")
	if err != nil {
		return "", "", ""
	}
	var ordinary, secret mimirQueryConfig
	if ordinaryResource.UnmarshalYamlE(&ordinary) != nil || secretResource.UnmarshalYamlE(&secret) != nil {
		return "", "", ""
	}
	domain := mimirQueryPrimaryDomain(services)
	username, password, ok := mimirQueryUserFromConfigs(ordinary, secret)
	if !ok || !validMimirQueryHostname(environment+"-grafana."+domain) {
		return "", "", ""
	}
	return domain, username, password
}

// Match native DomainNames: an explicit primary wins, otherwise sort map keys.
func mimirQueryPrimaryDomain(services servicesYaml) string {
	if services.Domain != "" {
		return services.Domain
	}
	domains := make([]string, 0, len(services.Domains))
	for domain := range services.Domains {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}

// Match LoadGrafanaConfigFrom + QueryUser without mutating either input:
// reject ambiguous users, select only ordinary query roles with vault secrets,
// prefer warpctl, otherwise retain ordinary order rather than secret order.
func mimirQueryUserFromConfigs(ordinary, secret mimirQueryConfig) (string, string, bool) {
	users := map[string]*mimirQueryUser{}
	for _, user := range ordinary.Users {
		if user == nil || user.Name == "" || user.Password != "" || users[user.Name] != nil {
			return "", "", false
		}
		users[user.Name] = user
	}
	passwords := map[string]string{}
	for _, user := range secret.Users {
		if user == nil || user.Name == "" || len(user.Roles) != 0 || users[user.Name] == nil {
			return "", "", false
		}
		if _, exists := passwords[user.Name]; exists {
			return "", "", false
		}
		passwords[user.Name] = user.Password
	}
	username, password := "", ""
	for _, user := range ordinary.Users {
		if !slices.Contains(user.Roles, "query") || passwords[user.Name] == "" {
			continue
		}
		if user.Name == "warpctl" {
			return user.Name, passwords[user.Name], true
		}
		if username == "" {
			username, password = user.Name, passwords[user.Name]
		}
	}
	return username, password, username != ""
}

// Only a DNS hostname can carry the in-memory Basic credential. No URL user,
// port, path, wildcard, whitespace or literal address is accepted as a domain.
func validMimirQueryHostname(hostname string) bool {
	if len(hostname) == 0 || len(hostname) > 253 || !strings.Contains(hostname, ".") {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-') {
				return false
			}
		}
	}
	return true
}
