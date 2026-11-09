package main

// §12.5's release check (DNS-M0-01): every host label services.yml gives the
// platform is on sidecar's reserved list, so no network label, alias, client
// name or scope label can take a platform host's name. The sidecar repository
// may not read vault, so the check runs here, where release-preflight already
// has the env's services.yml.
//
// A host label is the first label of each name the active version serves or
// publishes: the services domains, the expose aliases and domains, redirect and
// dns aliases, <env>-lb and <env>-<service> for each exposed service, each
// service's own name, each lb host and each router. Hidden prefixes are left
// out: they exist to stay unlisted, and the reserved list is published.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
)

// The narrow view of services.yml the check reads.
type releaseServicesHostsYaml struct {
	Domain        string            `yaml:"domain"`
	Domains       map[string]string `yaml:"domains"`
	ExposeAliases []string          `yaml:"expose_aliases"`
	Routers       map[string]any    `yaml:"routers"`
	Versions      []struct {
		Services map[string]struct {
			Exposed         *bool             `yaml:"exposed"`
			ExposeAliases   []string          `yaml:"expose_aliases"`
			ExposeDomains   []string          `yaml:"expose_domains"`
			RedirectAliases map[string]string `yaml:"redirect_aliases"`
			DnsAliases      []string          `yaml:"dns_aliases"`
		} `yaml:"services"`
		Lb struct {
			Interfaces map[string]any `yaml:"interfaces"`
		} `yaml:"lb"`
	} `yaml:"versions"`
}

// The first label of a host name, lowercase and without a trailing dot.
func releaseHostLabel(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	label, _, _ := strings.Cut(host, ".")
	return label
}

// Every host label of the active version of a services.yml for env (see the
// file header), sorted, each once.
func releaseServicesHostLabels(servicesYamlBytes []byte, env string) ([]string, error) {
	var services releaseServicesHostsYaml
	if err := yaml.Unmarshal(servicesYamlBytes, &services); err != nil {
		return nil, fmt.Errorf("services.yml: %w", err)
	}
	if len(services.Versions) == 0 {
		return nil, errors.New("services.yml: no active version")
	}
	if strings.TrimSpace(env) == "" {
		return nil, errors.New("no env to name its lb and service hosts by")
	}
	hosts := []string{services.Domain, env + "-lb"}
	for domain := range services.Domains {
		hosts = append(hosts, domain)
	}
	hosts = append(hosts, services.ExposeAliases...)
	active := services.Versions[0]
	for name, service := range active.Services {
		hosts = append(hosts, name)
		if service.Exposed == nil || *service.Exposed {
			hosts = append(hosts, env+"-"+name)
		}
		hosts = append(hosts, service.ExposeAliases...)
		hosts = append(hosts, service.ExposeDomains...)
		hosts = append(hosts, service.DnsAliases...)
		for alias := range service.RedirectAliases {
			hosts = append(hosts, alias)
		}
	}
	for host := range active.Lb.Interfaces {
		hosts = append(hosts, host)
	}
	for router := range services.Routers {
		hosts = append(hosts, router)
	}
	labels := []string{}
	for _, host := range hosts {
		if label := releaseHostLabel(host); label != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	slices.Sort(labels)
	return labels, nil
}

// Fails naming each host label of services.yml that sidecar's reserved list
// lacks, which the list must gain before the release.
func checkReleaseDnsReservedLabels(servicesYamlBytes []byte, env string, out io.Writer) error {
	labels, err := releaseServicesHostLabels(servicesYamlBytes, env)
	if err != nil {
		return fmt.Errorf("Reserved DNS labels: %w", err)
	}
	unlisted := []string{}
	for _, label := range labels {
		if !sidecar.IsReservedDnsLabel(label) {
			unlisted = append(unlisted, label)
		}
	}
	if 0 < len(unlisted) {
		return fmt.Errorf("Reserved DNS labels: services.yml host labels missing from sidecar/dns_reserved_labels.txt: %s", strings.Join(unlisted, ", "))
	}
	fmt.Fprintf(out, "Reserved DNS labels: all %d services.yml host labels are reserved\n", len(labels))
	return nil
}
