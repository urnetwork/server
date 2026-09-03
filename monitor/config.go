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
	"sort"
	"strings"
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
		Name             string   `yaml:"name"`
		LANIp            string   `yaml:"lan_ip"`
		OverlayIp        string   `yaml:"overlay_ip"`
		Roles            []string `yaml:"roles"`
		Disabled         bool     `yaml:"disabled"`
		SSHUser          string   `yaml:"ssh_user"`
		SSHIdentityFiles []string `yaml:"ssh_identity_files"`
		Redis            *struct {
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
		Subtensor *struct {
			PublicRPCURL               string `yaml:"public_rpc_url"`
			ExpectedChain              string `yaml:"expected_chain"`
			ExpectedGenesisHash        string `yaml:"expected_genesis_hash"`
			ExpectedSpecName           string `yaml:"expected_spec_name"`
			ExpectedSpecVersion        int64  `yaml:"expected_spec_version"`
			ExpectedTransactionVersion int64  `yaml:"expected_transaction_version"`
			ExpectedEVMChainID         string `yaml:"expected_evm_chain_id"`
			WarpMaxLag                 int64  `yaml:"warp_max_lag"`
			Nodes                      []struct {
				Name             string `yaml:"name"`
				SyncMode         string `yaml:"sync_mode"`
				RPCPort          int    `yaml:"rpc_port"`
				GatewayPort      int    `yaml:"gateway_port"`
				ContainerName    string `yaml:"container_name"`
				ExpectedImage    string `yaml:"expected_image"`
				ExpectedDataPath string `yaml:"expected_data_path"`
			} `yaml:"nodes"`
		} `yaml:"subtensor"`
		Backup *struct {
			PGSource    string `yaml:"pg_source"`
			PGPort      int    `yaml:"pg_port"`
			RedisSource string `yaml:"redis_source"`
			RedisPort   int    `yaml:"redis_port"`
		} `yaml:"backup"`
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

// servicesYaml is the narrow active-LB view needed by edge-ipv6. The first
// versions entry is the active warpctl configuration; older entries are
// intentionally ignored because they retain historical interface identities.
type servicesYaml struct {
	Domain        string                `yaml:"domain"`
	Domains       map[string]string     `yaml:"domains"`
	ExposeAliases []string              `yaml:"expose_aliases"`
	Versions      []servicesVersionYaml `yaml:"versions"`
}

type servicesVersionYaml struct {
	LB           servicesLBYaml                 `yaml:"lb"`
	HostServices map[string][]string            `yaml:"host_services"`
	Services     map[string]servicesServiceYaml `yaml:"services"`
}

type servicesServiceYaml struct {
	Blocks        []map[string]int `yaml:"blocks"`
	ExposeAliases []string         `yaml:"expose_aliases"`
}

type servicesLBYaml struct {
	Interfaces map[string]map[string]servicesLBInterfaceYaml `yaml:"interfaces"`
}

type servicesLBInterfaceYaml struct {
	IPv4        string `yaml:"ipv4"`
	IPv6        string `yaml:"ipv6"`
	Transparent bool   `yaml:"transparent"`
}

type grafanaVaultYaml struct {
	Grafana struct {
		AdminPassword string `yaml:"admin_password"`
	} `yaml:"grafana"`
}

type googleAppVaultYaml struct {
	Webhook struct {
		PackageName string `yaml:"package_name"`
	} `yaml:"webhook"`
}

type googlePlayReportingVault struct {
	ClientEmail  string `yaml:"client_email"`
	PrivateKey   string `yaml:"private_key"`
	PrivateKeyID string `yaml:"private_key_id"`
	TokenURL     string `yaml:"token_uri"`
}

type appleAppVaultYaml struct {
	AppStoreNotifications struct {
		AppAppleID int64 `yaml:"app_apple_id"`
	} `yaml:"app_store_notifications"`
}

type appleReportingVault struct {
	IssuerID   string `yaml:"issuer_id"`
	KeyID      string `yaml:"key_id"`
	PrivateKey string `yaml:"private_key"`
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

	grafanaResource, err := server.Vault.SimpleResource("grafana.yml")
	if err != nil {
		return SignalSettings{}, fmt.Errorf("grafana.yml: %w", err)
	}
	var grafanaVault grafanaVaultYaml
	if err := grafanaResource.UnmarshalYamlE(&grafanaVault); err != nil {
		return SignalSettings{}, err
	}
	if grafanaVault.Grafana.AdminPassword == "" {
		return SignalSettings{}, fmt.Errorf("grafana.yml: grafana.admin_password is required")
	}

	servicesResource, err := server.Vault.SimpleResource("services.yml")
	if err != nil {
		return SignalSettings{}, fmt.Errorf("services.yml: %w", err)
	}
	var services servicesYaml
	if err := servicesResource.UnmarshalYamlE(&services); err != nil {
		return SignalSettings{}, err
	}
	edgeIPv6ByHost, err := activeEdgeIPv6FromServices(services)
	if err != nil {
		return SignalSettings{}, err
	}
	publicLBByHost, err := activePublicLBFromServices(services)
	if err != nil {
		return SignalSettings{}, err
	}
	logServices, err := activeLogServicesFromServices(services)
	if err != nil {
		return SignalSettings{}, err
	}
	logServiceBlocks, err := activeLogServiceBlocksFromServices(services)
	if err != nil {
		return SignalSettings{}, err
	}
	grafanaHosts, err := activeServiceHostsFromServices(services, "grafana")
	if err != nil {
		return SignalSettings{}, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	settings := SignalSettings{
		Environment:         env,
		PublicDomain:        strings.TrimSpace(services.Domain),
		WebsiteDomain:       activeWebsiteDomainFromServices(services),
		ManagerHostname:     activeManagerHostnameFromServices(services),
		LogServices:         logServices,
		LogServiceBlocks:    logServiceBlocks,
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
		Grafana: GrafanaSettings{
			AdminPassword: grafanaVault.Grafana.AdminPassword,
		},
		SourceAttribution: SourceAttributionSettings{
			IPv4URL:      y.SourceAttribution.IPv4URL,
			IPv6URL:      y.SourceAttribution.IPv6URL,
			ExpectedIPv4: y.SourceAttribution.ExpectedIPv4,
			ExpectedIPv6: y.SourceAttribution.ExpectedIPv6,
		},
		GooglePlay:     loadGooglePlayReportingSettings(),
		AppleReporting: loadAppleReportingSettings(),
	}
	settings = settings.withDefaults()
	routes := lanRoutes()
	for _, configured := range y.Hosts {
		if configured.Disabled {
			continue
		}
		lanAddress := strings.TrimSpace(configured.LANIp)
		if lanAddress == "" {
			lanAddress = routes[configured.Name]
		}
		h := HostSettings{
			Name:           configured.Name,
			LANAddress:     lanAddress,
			OverlayAddress: configured.OverlayIp,
			Roles:          append([]string(nil), configured.Roles...),
			SSHUser:        strings.TrimSpace(configured.SSHUser),
			SSHKeyPaths:    monitorSSHKeyPaths(configured.SSHIdentityFiles),
			EdgeIPv6:       cloneEdgeIPv6Settings(edgeIPv6ByHost[configured.Name]),
			PublicLB:       clonePublicLBSettings(publicLBByHost[configured.Name]),
		}
		if grafanaHosts[configured.Name] {
			h.Roles = appendRole(h.Roles, "grafana")
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
		if configured.Subtensor != nil {
			h.Subtensor = &SubtensorHostSettings{
				PublicRPCURL:               configured.Subtensor.PublicRPCURL,
				ExpectedChain:              configured.Subtensor.ExpectedChain,
				ExpectedGenesisHash:        configured.Subtensor.ExpectedGenesisHash,
				ExpectedSpecName:           configured.Subtensor.ExpectedSpecName,
				ExpectedSpecVersion:        configured.Subtensor.ExpectedSpecVersion,
				ExpectedTransactionVersion: configured.Subtensor.ExpectedTransactionVersion,
				ExpectedEVMChainID:         configured.Subtensor.ExpectedEVMChainID,
				WarpMaxLag:                 configured.Subtensor.WarpMaxLag,
			}
			for _, node := range configured.Subtensor.Nodes {
				h.Subtensor.Nodes = append(h.Subtensor.Nodes, SubtensorNodeSettings{
					Name: node.Name, SyncMode: node.SyncMode,
					RPCPort: node.RPCPort, GatewayPort: node.GatewayPort,
					ContainerName: node.ContainerName, ExpectedImage: node.ExpectedImage,
					ExpectedDataPath: node.ExpectedDataPath,
				})
			}
		}
		if configured.Backup != nil {
			h.Backup = &BackupHostSettings{
				PGSource:    strings.TrimSpace(configured.Backup.PGSource),
				PGPort:      configured.Backup.PGPort,
				RedisSource: strings.TrimSpace(configured.Backup.RedisSource),
				RedisPort:   configured.Backup.RedisPort,
			}
		}
		settings.Hosts = append(settings.Hosts, h)
	}
	if err := settings.validate(); err != nil {
		return SignalSettings{}, err
	}
	return settings, nil
}

func monitorSSHKeyPaths(paths []string) []string {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(server.WarpHome(), path)
		}
		resolved = append(resolved, path)
	}
	return resolved
}

// loadGooglePlayReportingSettings is deliberately fail-soft only when the
// optional credential is absent. Once google-play-reporting.json exists, a
// malformed credential or missing application identity is retained on the
// provider settings so only §20.1 emits a visibility failure; it must not stop
// unrelated production probes from starting.
func loadGooglePlayReportingSettings() GooglePlayReportingSettings {
	credentialResource, err := server.Vault.SimpleResource("google-play-reporting.json")
	if err != nil {
		return GooglePlayReportingSettings{}
	}
	settings := GooglePlayReportingSettings{Enabled: true}
	var credential googlePlayReportingVault
	if err := credentialResource.UnmarshalYamlE(&credential); err != nil {
		// A YAML/JSON decoder error can quote the offending scalar. Never carry
		// credential-file contents into the monitor's Markdown visibility alert.
		settings.LoadError = fmt.Errorf("google-play-reporting.json is unreadable or malformed")
		return settings
	}
	settings.ClientEmail = strings.TrimSpace(credential.ClientEmail)
	settings.PrivateKey = credential.PrivateKey
	settings.PrivateKeyID = strings.TrimSpace(credential.PrivateKeyID)
	settings.TokenURL = strings.TrimSpace(credential.TokenURL)

	appResource, err := server.Vault.SimpleResource("google.yml")
	if err != nil {
		settings.LoadError = fmt.Errorf("google.yml: %w", err)
		return settings
	}
	var app googleAppVaultYaml
	if err := appResource.UnmarshalYamlE(&app); err != nil {
		settings.LoadError = fmt.Errorf("google.yml is unreadable or malformed")
		return settings
	}
	settings.PackageName = strings.TrimSpace(app.Webhook.PackageName)
	return settings
}

// loadAppleReportingSettings follows the same optional-resource contract as
// Google Play. App Store Connect authentication is isolated from the existing
// Sign in with Apple and App Store Server API credentials.
func loadAppleReportingSettings() AppleReportingSettings {
	credentialResource, err := server.Vault.SimpleResource("apple-reporting.yml")
	if err != nil {
		return AppleReportingSettings{}
	}
	settings := AppleReportingSettings{Enabled: true}
	var credential appleReportingVault
	if err := credentialResource.UnmarshalYamlE(&credential); err != nil {
		settings.LoadError = fmt.Errorf("apple-reporting.yml is unreadable or malformed")
		return settings
	}
	settings.IssuerID = strings.TrimSpace(credential.IssuerID)
	settings.KeyID = strings.TrimSpace(credential.KeyID)
	settings.PrivateKey = credential.PrivateKey

	appResource, err := server.Vault.SimpleResource("apple.yml")
	if err != nil {
		settings.LoadError = fmt.Errorf("apple.yml: %w", err)
		return settings
	}
	var app appleAppVaultYaml
	if err := appResource.UnmarshalYamlE(&app); err != nil {
		settings.LoadError = fmt.Errorf("apple.yml is unreadable or malformed")
		return settings
	}
	if app.AppStoreNotifications.AppAppleID != 0 {
		settings.AppID = fmt.Sprintf("%d", app.AppStoreNotifications.AppAppleID)
	}
	return settings
}

// activeServiceHostsFromServices returns the active host-service placement
// without duplicating it in monitor.yml. Host keys in services.yml carry the
// environment domain while monitor inventory uses the short host name.
func activeServiceHostsFromServices(services servicesYaml, service string) (map[string]bool, error) {
	if len(services.Versions) == 0 {
		return nil, fmt.Errorf("services.yml: no active version")
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return nil, fmt.Errorf("services.yml: active host service is required")
	}
	domainSuffix := "." + strings.TrimSpace(services.Domain)
	hosts := map[string]bool{}
	for configuredHost, configuredServices := range services.Versions[0].HostServices {
		host := strings.TrimSpace(configuredHost)
		if domainSuffix != "." {
			host = strings.TrimSuffix(host, domainSuffix)
		}
		for _, configuredService := range configuredServices {
			if strings.TrimSpace(configuredService) == service {
				hosts[host] = true
				break
			}
		}
	}
	return hosts, nil
}

func appendRole(roles []string, role string) []string {
	for _, existing := range roles {
		if existing == role {
			return roles
		}
	}
	return append(roles, role)
}

func activeWebsiteDomainFromServices(services servicesYaml) string {
	// ur.io is the product website whose Android and Apple association
	// contracts are committed with the site. Alternate environments that do
	// not manage it leave the focused probe deliberately unarmed.
	if _, ok := services.Domains["ur.io"]; ok {
		return "ur.io"
	}
	return ""
}

func activeManagerHostnameFromServices(services servicesYaml) string {
	domain := strings.TrimSpace(services.Domain)
	if domain == "" || len(services.Versions) == 0 {
		return ""
	}
	wanted := "manager." + domain
	for _, alias := range services.ExposeAliases {
		if strings.TrimSpace(alias) == wanted {
			return wanted
		}
	}
	for _, service := range services.Versions[0].Services {
		for _, alias := range service.ExposeAliases {
			if strings.TrimSpace(alias) == wanted {
				return wanted
			}
		}
	}
	return ""
}

func activeLogServicesFromServices(services servicesYaml) ([]string, error) {
	if len(services.Versions) == 0 {
		return nil, fmt.Errorf("services.yml: no active version")
	}
	configured := services.Versions[0].Services
	if len(configured) == 0 {
		return nil, fmt.Errorf("services.yml: active version has no services")
	}
	logServices := make([]string, 0, len(configured))
	for service := range configured {
		service = strings.TrimSpace(service)
		if service == "" || service == "lb" || service == "config-updater" {
			continue
		}
		logServices = append(logServices, service)
	}
	if len(logServices) == 0 {
		return nil, fmt.Errorf("services.yml: active version has no log-producing services")
	}
	sort.Strings(logServices)
	return logServices, nil
}

func activeLogServiceBlocksFromServices(services servicesYaml) (map[string][]string, error) {
	if len(services.Versions) == 0 {
		return nil, fmt.Errorf("services.yml: no active version")
	}
	configured := services.Versions[0].Services
	if len(configured) == 0 {
		return nil, fmt.Errorf("services.yml: active version has no services")
	}

	blocksByService := map[string][]string{}
	for service, serviceConfig := range configured {
		service = strings.TrimSpace(service)
		if service == "" || service == "lb" || service == "config-updater" {
			continue
		}
		seen := map[string]struct{}{}
		for _, weights := range serviceConfig.Blocks {
			for block := range weights {
				block = strings.TrimSpace(block)
				if block == "" {
					continue
				}
				seen[block] = struct{}{}
			}
		}
		blocks := make([]string, 0, len(seen))
		for block := range seen {
			blocks = append(blocks, block)
		}
		sort.Strings(blocks)
		blocksByService[service] = blocks
	}
	return blocksByService, nil
}

func activeEdgeIPv6FromServices(services servicesYaml) (map[string][]EdgeIPv6InterfaceSettings, error) {
	if len(services.Versions) == 0 {
		return nil, fmt.Errorf("services.yml: no active version")
	}
	domain := strings.TrimSpace(services.Domain)
	if domain == "" {
		return nil, fmt.Errorf("services.yml: domain is required for edge IPv6 SNI")
	}

	byHost := map[string][]EdgeIPv6InterfaceSettings{}
	for configuredHost, interfaces := range services.Versions[0].LB.Interfaces {
		host := strings.TrimSuffix(configuredHost, "."+domain)
		if !strings.Contains(host, "-edge-") {
			continue
		}
		for interfaceName, configured := range interfaces {
			address := strings.TrimSpace(configured.IPv6)
			if address == "" || configured.Transparent {
				continue
			}
			byHost[host] = append(byHost[host], EdgeIPv6InterfaceSettings{
				Interface:     interfaceName,
				Address:       address,
				ProbeHostname: "api-v6." + domain,
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

func activePublicLBFromServices(services servicesYaml) (map[string][]PublicLBInterfaceSettings, error) {
	if len(services.Versions) == 0 {
		return nil, fmt.Errorf("services.yml: no active version")
	}
	domain := strings.TrimSpace(services.Domain)
	if domain == "" {
		return nil, fmt.Errorf("services.yml: domain is required for public LB inventory")
	}

	byHost := map[string][]PublicLBInterfaceSettings{}
	for configuredHost, interfaces := range services.Versions[0].LB.Interfaces {
		host := strings.TrimSuffix(configuredHost, "."+domain)
		if !strings.Contains(host, "-edge-") {
			continue
		}
		for interfaceName, configured := range interfaces {
			if configured.Transparent {
				continue
			}
			ipv4 := strings.TrimSpace(configured.IPv4)
			ipv6 := strings.TrimSpace(configured.IPv6)
			if ipv4 == "" && ipv6 == "" {
				continue
			}
			byHost[host] = append(byHost[host], PublicLBInterfaceSettings{
				Interface:   interfaceName,
				IPv4Address: ipv4,
				IPv6Address: ipv6,
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
