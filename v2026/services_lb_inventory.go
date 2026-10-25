package server

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The public edge inventory of the active vault `services.yml` version: each
// edge host's interfaces that clients reach, from `versions[0].lb.interfaces`.
// The monitor probes these addresses and connect points server-directed moves
// at them (connect/MOVE.md §2.6, D17), so both read the inventory through the
// rules here. Older versions are ignored because they keep historical interface
// identities. A host is an edge when its name holds "-edge-". An interface
// marked transparent, or with no address, is not one clients reach.
//
// `versions[0].lb.move_weights` optionally maps a configured host to its move
// capacity weight (D3, D17). It sits beside `interfaces` rather than inside a
// host's entry, where warp would read it as an interface.

// warp's `dns_weight` when an interface sets none (warp services.DefaultDnsWeight)
const ServicesLbDefaultDnsWeight = 100

// One configured lb interface, the fields the inventory reads.
type ServicesLbInterfaceConfig struct {
	Ipv4        string `yaml:"ipv4"`
	Ipv6        string `yaml:"ipv6"`
	DnsWeight   int    `yaml:"dns_weight"`
	Transparent bool   `yaml:"transparent"`
}

// One public interface of an edge host.
type ServicesLbInterface struct {
	Interface string
	// empty when the interface has no address of the family
	Ipv4 string
	Ipv6 string
	// the weight of the interface's records in the route53 weighted sets,
	// ServicesLbDefaultDnsWeight when unset
	DnsWeight int
}

// The edge inventory of one services.yml.
type ServicesLbInventory struct {
	Domain string
	// inventory host -> its public interfaces, sorted by interface name
	Interfaces map[string][]ServicesLbInterface
	// inventory host -> its configured move weight, for the hosts that set one
	MoveWeights map[string]float64
}

// The narrow view of services.yml the inventory reads.
type servicesLbInventoryYaml struct {
	Domain   string `yaml:"domain"`
	Versions []struct {
		Lb struct {
			Interfaces  map[string]map[string]ServicesLbInterfaceConfig `yaml:"interfaces"`
			MoveWeights map[string]float64                              `yaml:"move_weights"`
		} `yaml:"lb"`
	} `yaml:"versions"`
}

// The inventory name of a configured lb host: the configured name without the
// services domain.
func ServicesLbHostName(domain string, configuredHost string) string {
	return strings.TrimSuffix(configuredHost, "."+strings.TrimSpace(domain))
}

// Each edge host's public interfaces, by inventory host. configured is the
// active version's `lb.interfaces`.
func ActiveEdgeLbInterfaces(
	domain string,
	configured map[string]map[string]ServicesLbInterfaceConfig,
) (map[string][]ServicesLbInterface, error) {
	if strings.TrimSpace(domain) == "" {
		return nil, fmt.Errorf("services.yml: domain is required for public LB inventory")
	}

	byHost := map[string][]ServicesLbInterface{}
	for configuredHost, interfaces := range configured {
		host := ServicesLbHostName(domain, configuredHost)
		if !strings.Contains(host, "-edge-") {
			continue
		}
		for interfaceName, interfaceConfig := range interfaces {
			if interfaceConfig.Transparent {
				continue
			}
			ipv4 := strings.TrimSpace(interfaceConfig.Ipv4)
			ipv6 := strings.TrimSpace(interfaceConfig.Ipv6)
			if ipv4 == "" && ipv6 == "" {
				continue
			}
			dnsWeight := interfaceConfig.DnsWeight
			if dnsWeight == 0 {
				dnsWeight = ServicesLbDefaultDnsWeight
			}
			byHost[host] = append(byHost[host], ServicesLbInterface{
				Interface: interfaceName,
				Ipv4:      ipv4,
				Ipv6:      ipv6,
				DnsWeight: dnsWeight,
			})
		}
	}
	for host := range byHost {
		sort.Slice(byHost[host], func(i, j int) bool {
			return byHost[host][i].Interface < byHost[host][j].Interface
		})
	}
	return byHost, nil
}

// Parses the edge inventory of a services.yml document.
func ParseServicesLbInventory(servicesYamlBytes []byte) (*ServicesLbInventory, error) {
	var services servicesLbInventoryYaml
	if err := yaml.Unmarshal(servicesYamlBytes, &services); err != nil {
		return nil, fmt.Errorf("services.yml: %w", err)
	}
	if len(services.Versions) == 0 {
		return nil, fmt.Errorf("services.yml: no active version")
	}
	active := services.Versions[0].Lb
	interfaces, err := ActiveEdgeLbInterfaces(services.Domain, active.Interfaces)
	if err != nil {
		return nil, err
	}
	moveWeights := map[string]float64{}
	for configuredHost, moveWeight := range active.MoveWeights {
		moveWeights[ServicesLbHostName(services.Domain, configuredHost)] = moveWeight
	}
	return &ServicesLbInventory{
		Domain:      strings.TrimSpace(services.Domain),
		Interfaces:  interfaces,
		MoveWeights: moveWeights,
	}, nil
}
