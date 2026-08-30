// Configuration assembly from the standard WARP_HOME resolvers: the monitor
// inventory from vault/<env>/monitor.yml, pg credentials from
// vault/<env>/pg.yml, and (lan mode) host routes from
// config/<env>/settings.yml. Shared facts are read from their source of
// truth, never duplicated in monitor.yml.
package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// monitorYaml mirrors vault/<env>/monitor.yml.
type monitorYaml struct {
	Ssh struct {
		User          string   `yaml:"user"`
		DevUser       string   `yaml:"dev_user"`
		IdentityFiles []string `yaml:"identity_files"`
		KeyPaths      []string `yaml:"key_paths"`
	} `yaml:"ssh"`
	AddressMode string `yaml:"address_mode"`
	Hosts       []struct {
		Name      string   `yaml:"name"`
		OverlayIp string   `yaml:"overlay_ip"`
		Roles     []string `yaml:"roles"`
		Redis     *struct {
			EntryPort        int   `yaml:"entry_port"`
			NodePorts        []int `yaml:"node_ports"`
			ExpectedReplicas int   `yaml:"expected_replicas"`
		} `yaml:"redis"`
		Proxy *struct {
			PublicHostname   string   `yaml:"public_hostname"`
			PublicInterface  string   `yaml:"public_interface"`
			RoutingTable     int      `yaml:"routing_table"`
			LoadBalancerUnit string   `yaml:"load_balancer_unit"`
			AddressFamilies  []string `yaml:"address_families"`
		} `yaml:"proxy"`
	} `yaml:"hosts"`
	Pg struct {
		Port          int `yaml:"port"`
		PgbouncerPort int `yaml:"pgbouncer_port"`
	} `yaml:"pg"`
	SourceAttribution struct {
		IPv4URL      string `yaml:"ipv4_url"`
		IPv6URL      string `yaml:"ipv6_url"`
		ExpectedIPv4 string `yaml:"expected_ipv4"`
		ExpectedIPv6 string `yaml:"expected_ipv6"`
	} `yaml:"source_attribution"`
}

// LoadSignalSettings loads production settings from the standard WARP_HOME
// config/vault resolvers. Keeping this here makes cli/monitor a thin wrapper.
func LoadSignalSettings() (SignalSettings, error) {
	env, err := server.Env()
	if err != nil {
		return SignalSettings{}, err
	}
	monitorResource, err := server.Vault.SimpleResource("monitor.yml")
	if err != nil {
		return SignalSettings{}, fmt.Errorf("monitor.yml: %w", err)
	}
	var y monitorYaml
	if err := monitorResource.UnmarshalYamlE(&y); err != nil {
		return SignalSettings{}, err
	}

	pgResource, err := server.Vault.SimpleResource("pg.yml")
	if err != nil {
		return SignalSettings{}, fmt.Errorf("pg.yml: %w", err)
	}
	pgKeys, err := pgResource.ParseE()
	if err != nil {
		return SignalSettings{}, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	settings := SignalSettings{
		Environment:         env,
		VerificationEnabled: controller.StEnabled(),
		SSHUser:             y.Ssh.User,
		SSHDevUser:          y.Ssh.DevUser,
		SSHKeyPaths:         append(append([]string(nil), y.Ssh.IdentityFiles...), y.Ssh.KeyPaths...),
		AddressMode:         AddressMode(y.AddressMode),
		StateDir:            filepath.Join(home, ".urnetwork-monitor", env),
		SSHConnectTimeout:   10 * time.Second,
		CommandTimeout:      60 * time.Second,
		PostgreSQL: PostgreSQLSettings{
			Port:          y.Pg.Port,
			PgBouncerPort: y.Pg.PgbouncerPort,
			User:          yamlString(pgKeys["user"]),
			Password:      yamlString(pgKeys["password"]),
			Database:      yamlString(pgKeys["db"]),
		},
		SourceAttribution: SourceAttributionSettings{
			IPv4URL:      y.SourceAttribution.IPv4URL,
			IPv6URL:      y.SourceAttribution.IPv6URL,
			ExpectedIPv4: y.SourceAttribution.ExpectedIPv4,
			ExpectedIPv6: y.SourceAttribution.ExpectedIPv6,
		},
	}
	settings = settings.withDefaults()
	routes := lanRoutes()
	for _, configured := range y.Hosts {
		h := HostSettings{
			Name:           configured.Name,
			LANAddress:     routes[configured.Name],
			OverlayAddress: configured.OverlayIp,
			Roles:          append([]string(nil), configured.Roles...),
		}
		if configured.Redis != nil {
			h.RedisEntryPort = configured.Redis.EntryPort
			h.RedisExpectedReplicas = configured.Redis.ExpectedReplicas
			if len(configured.Redis.NodePorts) == 2 {
				for port := configured.Redis.NodePorts[0]; port <= configured.Redis.NodePorts[1]; port++ {
					h.RedisNodePorts = append(h.RedisNodePorts, port)
				}
			} else {
				h.RedisNodePorts = append([]int(nil), configured.Redis.NodePorts...)
			}
		}
		if configured.Proxy != nil {
			h.Proxy = &ProxyHostSettings{
				PublicHostname:   configured.Proxy.PublicHostname,
				PublicInterface:  configured.Proxy.PublicInterface,
				RoutingTable:     configured.Proxy.RoutingTable,
				LoadBalancerUnit: configured.Proxy.LoadBalancerUnit,
				AddressFamilies:  append([]string(nil), configured.Proxy.AddressFamilies...),
			}
		}
		settings.Hosts = append(settings.Hosts, h)
	}
	if err := settings.validate(); err != nil {
		return SignalSettings{}, err
	}
	return settings, nil
}

func loadConfig() *monitorConfig {
	settings, err := LoadSignalSettings()
	if err != nil {
		panic(err)
	}
	return configFromSignalSettings(settings)
}

// lanRoutes reads config/<env>/settings.yml and returns host name -> lan ip.
// The routes map is per-host but identical across hosts (yaml anchor), so any
// host's routes block is authoritative. Best-effort: returns empty on any
// shape mismatch, which only matters in lan mode.
func lanRoutes() map[string]string {
	routeIps := map[string]string{}
	resource, err := server.Config.SimpleResource("settings.yml")
	if err != nil {
		return routeIps
	}
	for _, v := range resource.Parse() {
		hostSettings, ok := v.(map[string]any)
		if !ok {
			continue
		}
		routes, ok := hostSettings["routes"].(map[string]any)
		if !ok {
			continue
		}
		for name, ip := range routes {
			routeIps[name] = yamlString(ip)
		}
		if len(routeIps) > 0 {
			break
		}
	}
	return routeIps
}

func yamlString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
