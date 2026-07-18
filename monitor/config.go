// Configuration assembly from the standard WARP_HOME resolvers: the monitor
// inventory from vault/<env>/monitor.yml, pg credentials from
// vault/<env>/pg.yml, and (lan mode) host routes from
// config/<env>/settings.yml. Shared facts are read from their source of
// truth, never duplicated in monitor.yml.
package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server"
)

// monitorYaml mirrors vault/<env>/monitor.yml.
type monitorYaml struct {
	Ssh struct {
		User    string `yaml:"user"`
		DevUser string `yaml:"dev_user"`
	} `yaml:"ssh"`
	AddressMode string `yaml:"address_mode"`
	Hosts       []struct {
		Name      string   `yaml:"name"`
		OverlayIp string   `yaml:"overlay_ip"`
		Roles     []string `yaml:"roles"`
		Redis     *struct {
			EntryPort int   `yaml:"entry_port"`
			NodePorts []int `yaml:"node_ports"`
		} `yaml:"redis"`
	} `yaml:"hosts"`
	Pg struct {
		Port          int `yaml:"port"`
		PgbouncerPort int `yaml:"pgbouncer_port"`
	} `yaml:"pg"`
}

func loadConfig() *monitorConfig {
	var y monitorYaml
	server.Vault.RequireSimpleResource("monitor.yml").UnmarshalYaml(&y)

	cfg := &monitorConfig{
		env:               server.RequireEnv(),
		sshUser:           y.Ssh.User,
		sshDevUser:        y.Ssh.DevUser,
		addressMode:       y.AddressMode,
		pgPort:            y.Pg.Port,
		pgbouncerPort:     y.Pg.PgbouncerPort,
		sshConnectTimeout: 10 * time.Second,
		commandTimeout:    60 * time.Second,
	}
	if cfg.addressMode == "" {
		cfg.addressMode = addressModeOverlay
	}
	if cfg.pgPort == 0 {
		cfg.pgPort = 5432
	}

	// baselines and other local persistence
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	cfg.stateDir = filepath.Join(home, ".urnetwork-monitor", cfg.env)

	// pg credentials from vault pg.yml (same source the app uses)
	pgKeys := server.Vault.RequireSimpleResource("pg.yml").Parse()
	cfg.pgUser = yamlString(pgKeys["user"])
	cfg.pgPassword = yamlString(pgKeys["password"])
	cfg.pgDb = yamlString(pgKeys["db"])

	// lan routes (config settings.yml) — only needed in lan mode; overlay
	// uses the overlay_ip carried in monitor.yml
	routes := lanRoutes()

	for _, hostYaml := range y.Hosts {
		h := &host{
			name:      hostYaml.Name,
			overlayIp: hostYaml.OverlayIp,
			lanIp:     routes[hostYaml.Name],
			roles:     hostYaml.Roles,
		}
		if hostYaml.Redis != nil {
			h.redisEntryPort = hostYaml.Redis.EntryPort
			if len(hostYaml.Redis.NodePorts) == 2 {
				h.redisNodeLo = hostYaml.Redis.NodePorts[0]
				h.redisNodeHi = hostYaml.Redis.NodePorts[1]
			}
		}
		cfg.hosts = append(cfg.hosts, h)
	}
	return cfg
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
