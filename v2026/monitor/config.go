// Configuration assembly from the standard WARP_HOME resolvers: the monitor
// inventory from vault/<env>/monitor.yml, pg credentials from
// vault/<env>/pg.yml, and (lan mode) host routes from
// config/<env>/settings.yml. Shared facts are read from their source of
// truth, never duplicated in monitor.yml.
package monitor

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// monitorYaml mirrors vault/<env>/monitor.yml.
type monitorYaml struct {
	Ssh struct {
		User          string   `yaml:"user"`
		DevUser       string   `yaml:"dev_user"`
		IdentityFiles []string `yaml:"identity_files"`
		KeyPaths      []string `yaml:"key_paths"`
	} `yaml:"ssh"`
	AddressMode string            `yaml:"address_mode"`
	Routers     []RouterSettings  `yaml:"routers"`
	PublicUdp   PublicUdpSettings `yaml:"public_udp"`
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
	// Keep the raw node so an omitted block remains distinguishable from an
	// explicitly present null or malformed block. SIGNALS.md §18.3 requires
	// the former to noop and the latter to fail closed as invalid desired state.
	DNSAliases yaml.Node `yaml:"dns_aliases"`
}

type dnsAliasesYaml struct {
	ManagedDomains []string `yaml:"managed_domains"`
	ExpectedA      []string `yaml:"expected_a"`
	ExpectedAAAA   []string `yaml:"expected_aaaa"`
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
	RoutingTables any                            `yaml:"routing_tables"`
	LB            servicesLBYaml                 `yaml:"lb"`
	HostServices  map[string][]string            `yaml:"host_services"`
	Services      map[string]servicesServiceYaml `yaml:"services"`
}

type servicesServiceYaml struct {
	Blocks        []map[string]int `yaml:"blocks"`
	ExposeAliases []string         `yaml:"expose_aliases"`
	Hosts         []string         `yaml:"hosts"`
}

type servicesLBYaml struct {
	Interfaces map[string]map[string]servicesLBInterfaceYaml `yaml:"interfaces"`
}

type servicesLBInterfaceYaml struct {
	IPv4          string      `yaml:"ipv4"`
	IPv6          string      `yaml:"ipv6"`
	Transparent   bool        `yaml:"transparent"`
	ExternalPorts map[int]int `yaml:"external_ports"`
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

type credentialFieldSpec struct {
	name string
	path []string
}

type credentialRequirementSpec struct {
	key      string
	resource string
	purpose  string
	required bool
	fields   []credentialFieldSpec
}

// LoadSignalSettings loads production settings from the standard WARP_HOME
// config/vault resolvers. Keeping this here makes cli/monitor a thin wrapper.
func LoadSignalSettings() (SignalSettings, error) {
	settings, err := loadSignalSettingsSnapshot()
	if err != nil {
		return SignalSettings{}, err
	}
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(loadSignalSettingsSnapshot)
	return settings, nil
}

// loadSignalSettingsSnapshot deliberately omits the generation checker so a
// checker can reload current effective settings without recursively wrapping
// another checker. The returned values are otherwise identical to
// LoadSignalSettings.
func loadSignalSettingsSnapshot() (SignalSettings, error) {
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

	// Core credentials are deliberately fail-soft at assembly time. The
	// credentials signal must be able to report a missing or malformed pg.yml
	// or grafana.yml instead of the settings loader exiting before any
	// structured alert can be written. Dependent probes still fail visibly
	// with empty settings, so this never turns an unknown backend into green.
	pgKeys := map[string]any{}
	if pgResource, resourceErr := server.Vault.SimpleResource("pg.yml"); resourceErr == nil {
		if parsed, parseErr := pgResource.ParseE(); parseErr == nil {
			pgKeys = parsed
		}
	}

	var grafanaVault grafanaVaultYaml
	if grafanaResource, resourceErr := server.Vault.SimpleResource("grafana.yml"); resourceErr == nil {
		_ = grafanaResource.UnmarshalYamlE(&grafanaVault)
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
	proxyByHost, proxyServiceConfigured, err := activeProxyPathsFromServices(env, services)
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
	stConfiguration := loadSTConfigurationObservation(env)
	settings := SignalSettings{
		Routers:                cloneRouterSettings(y.Routers),
		PublicUdp:              clonePublicUdpSettings(y.PublicUdp),
		Environment:            env,
		PublicDomain:           strings.TrimSpace(services.Domain),
		WebsiteDomain:          activeWebsiteDomainFromServices(services),
		ManagerHostname:        activeManagerHostnameFromServices(services),
		LogServices:            logServices,
		LogServiceBlocks:       logServiceBlocks,
		ProxyPathExpectedHosts: len(proxyByHost),
		VerificationEnabled:    stConfiguration.configuredEnabled,
		STConfigStatus:         stConfiguration.status,
		STDeploymentKey:        stConfiguration.deploymentKey,
		SSHUser:                y.Ssh.User,
		SSHDevUser:             y.Ssh.DevUser,
		SSHKeyPaths:            append(append([]string(nil), y.Ssh.IdentityFiles...), y.Ssh.KeyPaths...),
		AddressMode:            AddressMode(y.AddressMode),
		StateDir:               filepath.Join(home, ".urnetwork-monitor", env),
		SSHConnectTimeout:      10 * time.Second,
		CommandTimeout:         60 * time.Second,
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
		DNSAliases:      dnsAliasSettingsFromMonitorYaml(y),
		MimirPublishers: loadMimirPublisherSettings(server.WarpHome(), env),
		GooglePlay:      loadGooglePlayReportingSettings(),
		AppleReporting:  loadAppleReportingSettings(),
		Credentials:     loadCredentialRequirements(env, stConfiguration.status.requiresSTCredentials(), logServices),
	}
	settings = settings.withDefaults()
	routes := lanRoutes()
	for _, configured := range y.Hosts {
		lanAddress := strings.TrimSpace(configured.LANIp)
		if lanAddress == "" && !configured.Disabled {
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
			Proxy:          cloneProxyHostSettings(proxyByHost[configured.Name]),
		}
		if configured.Disabled {
			if configured.Proxy != nil && configured.Proxy.PublicHostname != "" {
				h.scopeEndpoints = append(h.scopeEndpoints, configured.Proxy.PublicHostname)
			}
			settings.disabledHosts = append(settings.disabledHosts, h)
			continue
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
		var legacyProxy *ProxyHostSettings
		if configured.Proxy != nil {
			legacyProxy = &ProxyHostSettings{
				PublicHostname:   configured.Proxy.PublicHostname,
				PublicInterface:  configured.Proxy.PublicInterface,
				RoutingTable:     configured.Proxy.RoutingTable,
				LoadBalancerUnit: configured.Proxy.LoadBalancerUnit,
				AddressFamilies:  append([]string(nil), configured.Proxy.AddressFamilies...),
			}
		}
		// Legacy monitor.yml identity is valid only when services.yml has no
		// proxy service at all. A present service owns desired state even when it
		// currently places zero hosts, so stale duplicate config cannot resurrect
		// a disabled placement.
		h.Proxy = selectedProxyHostSettings(h.Proxy, proxyServiceConfigured, legacyProxy)
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
	if len(settings.Routers) != 0 {
		generation, err := routerDesiredInputGeneration(servicesResource)
		if err != nil {
			return SignalSettings{}, err
		}
		settings.routerDesiredGeneration = generation
	}
	if err := settings.validate(); err != nil {
		return SignalSettings{}, err
	}
	return settings, nil
}

// The existing narrow LB projection omits router intent. Compare complete
// desired resource values only in memory; never render these fingerprints.
func routerDesiredInputGeneration(servicesResource *server.SimpleResource) ([32]byte, error) {
	inputs := map[string]any{}
	if err := servicesResource.UnmarshalYamlE(&inputs); err != nil {
		return [32]byte{}, errors.New("router desired services resource unavailable")
	}
	var settingsInput any
	if resource, err := server.Config.SimpleResource("settings.yml"); err == nil {
		if err := resource.UnmarshalYamlE(&settingsInput); err != nil {
			return [32]byte{}, errors.New("router desired settings resource unavailable")
		}
	} else if !errors.Is(err, server.ErrResourceNotFound) {
		return [32]byte{}, errors.New("router desired settings resource unavailable")
	}
	encoded, err := yaml.Marshal(map[string]any{"services": inputs, "settings": settingsInput})
	if err != nil {
		return [32]byte{}, errors.New("router desired resources invalid")
	}
	return sha256.Sum256(encoded), nil
}

func dnsAliasSettingsFromMonitorYaml(y monitorYaml) DNSAliasSettings {
	if y.DNSAliases.Kind == 0 {
		return DNSAliasSettings{}
	}
	configured := dnsAliasesYaml{}
	if y.DNSAliases.Kind != yaml.MappingNode || y.DNSAliases.Decode(&configured) != nil {
		return DNSAliasSettings{Enabled: true}
	}
	return DNSAliasSettings{
		Enabled:        true,
		ManagedDomains: append([]string(nil), configured.ManagedDomains...),
		ExpectedA:      append([]string(nil), configured.ExpectedA...),
		ExpectedAAAA:   append([]string(nil), configured.ExpectedAAAA...),
	}
}

// activeProxyPathsFromServices derives SIGNALS.md §14.5's stable probe
// identity from warpctl's active topology. Dynamic container ports are still
// discovered on every run. Routing-table ownership must be reconstructed over
// all retained versions because warpctl deliberately keeps a block's original
// assignment when a new version becomes active.
func activeProxyPathsFromServices(environment string, services servicesYaml) (map[string]*ProxyHostSettings, bool, error) {
	if len(services.Versions) == 0 {
		return nil, false, fmt.Errorf("services.yml: no active version")
	}
	domain := strings.TrimSpace(services.Domain)
	if domain == "" {
		return nil, false, fmt.Errorf("services.yml: domain is required for proxy public paths")
	}
	active := services.Versions[0]
	proxy, ok := active.Services["proxy"]
	if !ok {
		return map[string]*ProxyHostSettings{}, false, nil
	}

	placed := map[string]bool{}
	for host := range active.LB.Interfaces {
		placed[host] = true
	}
	for host, enabled := range active.HostServices {
		if !containsTrimmed(enabled, "proxy") {
			delete(placed, host)
		}
	}
	if len(proxy.Hosts) > 0 {
		allowed := map[string]bool{}
		for _, host := range proxy.Hosts {
			allowed[strings.TrimSpace(host)] = true
		}
		for host := range placed {
			if !allowed[host] {
				delete(placed, host)
			}
		}
	}

	routingTables, err := assignedLBRoutingTables(services.Versions)
	if err != nil {
		return nil, true, err
	}
	byHost := map[string]*ProxyHostSettings{}
	domainSuffix := "." + domain
	for configuredHost := range placed {
		interfaces := active.LB.Interfaces[configuredHost]
		transparent := make([]string, 0, len(interfaces))
		for interfaceName, configured := range interfaces {
			if configured.Transparent {
				transparent = append(transparent, interfaceName)
			}
		}
		sort.Strings(transparent)
		if len(transparent) != 1 {
			return nil, true, fmt.Errorf("services.yml: proxy host %q has %d transparent interfaces, want exactly 1", configuredHost, len(transparent))
		}
		interfaceName := transparent[0]
		configured := interfaces[interfaceName]
		block := configuredHost + "-" + interfaceName
		table, ok := routingTables[block]
		if !ok {
			return nil, true, fmt.Errorf("services.yml: proxy block %q has no routing-table assignment", block)
		}
		families := []string{}
		if strings.TrimSpace(configured.IPv4) != "" {
			families = append(families, "ipv4")
		}
		if strings.TrimSpace(configured.IPv6) != "" {
			families = append(families, "ipv6")
		}
		if len(families) == 0 {
			return nil, true, fmt.Errorf("services.yml: proxy block %q has no public address family", block)
		}
		host := strings.TrimSuffix(strings.TrimSpace(configuredHost), domainSuffix)
		if host == "" {
			return nil, true, fmt.Errorf("services.yml: proxy host name is empty")
		}
		byHost[host] = &ProxyHostSettings{
			PublicHostname:   strings.TrimSpace(configuredHost),
			PublicInterface:  strings.TrimSpace(interfaceName),
			RoutingTable:     table,
			LoadBalancerUnit: fmt.Sprintf("warp-%s-lb-%s.service", strings.TrimSpace(environment), strings.TrimSpace(interfaceName)),
			AddressFamilies:  families,
		}
	}
	return byHost, true, nil
}

func selectedProxyHostSettings(derived *ProxyHostSettings, serviceConfigured bool, legacy *ProxyHostSettings) *ProxyHostSettings {
	if derived != nil {
		return cloneProxyHostSettings(derived)
	}
	if serviceConfigured {
		return nil
	}
	return cloneProxyHostSettings(legacy)
}

func containsTrimmed(values []string, wanted string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}

// assignedLBRoutingTables mirrors only warpctl's documented stable
// host/interface -> routing-table allocation. Keeping this narrow avoids
// importing a deployment binary into the reusable monitor package.
func assignedLBRoutingTables(versions []servicesVersionYaml) (map[string]int, error) {
	assignedByHost := map[string]map[int]string{}
	blockTable := map[string]int{}
	for versionIndex := len(versions) - 1; versionIndex >= 0; versionIndex-- {
		version := versions[versionIndex]
		tables, err := expandRoutingTableSpec(version.RoutingTables)
		if err != nil {
			return nil, fmt.Errorf("services.yml: version %d routing_tables: %w", versionIndex, err)
		}
		sort.Ints(tables)
		hosts := make([]string, 0, len(version.LB.Interfaces))
		for host := range version.LB.Interfaces {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		for _, forced := range []bool{true, false} {
			for _, host := range hosts {
				interfaces := version.LB.Interfaces[host]
				names := make([]string, 0, len(interfaces))
				for name := range interfaces {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					if (len(interfaces[name].ExternalPorts) > 0) != forced {
						continue
					}
					block := host + "-" + name
					assigned := assignedByHost[host]
					if assigned == nil {
						assigned = map[int]string{}
						assignedByHost[host] = assigned
					}
					table := 0
					for _, candidate := range tables {
						if assigned[candidate] == block {
							table = candidate
							break
						}
					}
					if table == 0 {
						for _, candidate := range tables {
							if _, used := assigned[candidate]; !used {
								table = candidate
								break
							}
						}
					}
					if table == 0 {
						return nil, fmt.Errorf("host %q has no free routing table for block %q", host, block)
					}
					assigned[table] = block
					blockTable[block] = table
				}
			}
		}
	}
	return blockTable, nil
}

func expandRoutingTableSpec(spec any) ([]int, error) {
	if number, ok := spec.(int); ok {
		return []int{number}, nil
	}
	text, ok := spec.(string)
	if !ok {
		return nil, fmt.Errorf("unsupported type %T", spec)
	}
	tables := []int{}
	for _, part := range strings.Split(text, ",") {
		bounds := strings.Split(strings.TrimSpace(part), "-")
		if len(bounds) < 1 || len(bounds) > 2 {
			return nil, fmt.Errorf("invalid range")
		}
		first, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid table")
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil || last < first {
				return nil, fmt.Errorf("invalid range")
			}
		}
		for table := first; table <= last; table++ {
			tables = append(tables, table)
		}
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("empty range")
	}
	return tables, nil
}

// loadCredentialRequirements is the proactive counterpart to SIGNALS.md
// §8.7's route-failure log classifier. Main runs every listed core and
// payment integration, so a missing resource or field is a release defect.
// Crash-report resources remain optional by contract: absence is a no-op, but
// a present partial credential is still observable.
func loadCredentialRequirements(environment string, stConfiguredEnabled bool, services []string) []CredentialRequirement {
	required := environment == "main"
	serviceEnabled := func(wanted string) bool {
		for _, service := range services {
			if service == wanted {
				return true
			}
		}
		return false
	}
	field := func(name string, path ...string) credentialFieldSpec {
		return credentialFieldSpec{name: name, path: path}
	}
	specs := []credentialRequirementSpec{
		{
			key: "database", resource: "pg.yml", purpose: "Application PostgreSQL authentication", required: required,
			fields: []credentialFieldSpec{
				field("authority", "authority"),
				field("user", "user"),
				field("password", "password"),
				field("db", "db"),
			},
		},
		{
			key: "database-maintenance", resource: "pg_maintenance.yml", purpose: "Direct PostgreSQL maintenance authentication", required: false,
			fields: []credentialFieldSpec{
				field("authority", "authority"),
				field("user", "user"),
				field("password", "password"),
				field("db", "db"),
			},
		},
		{
			key: "redis", resource: "redis.yml", purpose: "Application Redis authentication", required: required,
			// An explicitly empty password is valid for a private, deliberately
			// unauthenticated Redis deployment. Require the connection identity;
			// transport exposure/authentication policy is a separate signal.
			fields: []credentialFieldSpec{field("authority", "authority")},
		},
		{
			key: "grafana", resource: "grafana.yml", purpose: "Grafana administration, metric push, datasource, and object-storage authentication", required: required,
			fields: []credentialFieldSpec{
				field("grafana.admin_password", "grafana", "admin_password"),
				field("postgres.password", "postgres", "password"),
				field("minio.access_key", "minio", "access_key"),
				field("minio.secret_key", "minio", "secret_key"),
				field("users", "users"),
			},
		},
		{
			key: "password-auth", resource: "password.yml", purpose: "Password credential hashing", required: required,
			fields: []credentialFieldSpec{field("password.pepper", "password", "pepper")},
		},
		{
			key: "oauth-signing", resource: "auth.yml", purpose: "OAuth authorization and dedicated token signing", required: required,
			fields: []credentialFieldSpec{
				field("oauth.issuer", "oauth", "issuer"),
				field("oauth.authorization_endpoint", "oauth", "authorization_endpoint"),
				field("oauth.signer_keys", "oauth", "signer_keys"),
			},
		},
		{
			key: "apple-payment", resource: "apple.yml", purpose: "Apple subscription notification and App Store Server API reconciliation", required: required,
			fields: []credentialFieldSpec{
				field("app_store_server_api_key_id", "app_store_server_api_key_id"),
				field("issuer_id", "issuer_id"),
				field("private_key", "private_key"),
				field("app_store_notifications.bundle_id", "app_store_notifications", "bundle_id"),
				field("app_store_notifications.app_apple_id", "app_store_notifications", "app_apple_id"),
				field("app_store_notifications.environments", "app_store_notifications", "environments"),
				field("app_store_notifications.product_ids", "app_store_notifications", "product_ids"),
			},
		},
		{
			key: "google-payment", resource: "google.yml", purpose: "Google Play subscription notification and reconciliation", required: required,
			fields: []credentialFieldSpec{
				field("webhook.publisher_email", "webhook", "publisher_email"),
				field("webhook.package_name", "webhook", "package_name"),
				field("oauth.client_id", "oauth", "client_id"),
				field("oauth.client_secret", "oauth", "client_secret"),
				field("oauth.refresh_token", "oauth", "refresh_token"),
			},
		},
		{
			key: "stripe-payment", resource: "stripe.yml", purpose: "Stripe checkout, webhook verification, and reconciliation", required: required,
			fields: []credentialFieldSpec{
				field("api.token", "api", "token"),
				field("api.publishable_key", "api", "publishable_key"),
				field("webhook.signing_secret", "webhook", "signing_secret"),
			},
		},
		{
			key: "solana-payment", resource: "helius.yml", purpose: "Solana payment webhook verification and reconciliation", required: required,
			fields: []credentialFieldSpec{
				field("helius.api_key", "helius", "api_key"),
				field("helius.webhook_auth_header", "helius", "webhook_auth_header"),
			},
		},
		{
			key: "coinbase-payment", resource: "coinbase.yml", purpose: "Coinbase data-pack checkout and webhook verification", required: required,
			fields: []credentialFieldSpec{
				// The current exchange-rate client is unauthenticated and reads
				// only api.host. account_id, key_name, and private_key are retained
				// legacy configuration, not runtime credential prerequisites.
				field("api.host", "api", "host"),
				field("webhook.shared_secret", "webhook", "shared_secret"),
			},
		},
		{
			key: "circle-payout", resource: "circle.yml", purpose: "Circle wallet and provider payout submission", required: required,
			fields: []credentialFieldSpec{
				field("circle.api_token", "circle", "api_token"),
				field("circle.entity_secret", "circle", "entity_secret"),
				field("circle.wallet_set_id", "circle", "wallet_set_id"),
				field("circle.solana_wallet_id", "circle", "solana_wallet_id"),
				field("circle.polygon_wallet_id", "circle", "polygon_wallet_id"),
			},
		},
		{
			key: "jwt-signing", resource: "jwt.yml", purpose: "API and client JWT signing", required: required,
			fields: []credentialFieldSpec{field("tls_key_paths", "tls_key_paths")},
		},
		{
			key: "client-ip-hash", resource: "client.yml", purpose: "Client address privacy-preserving hashing", required: required,
			fields: []credentialFieldSpec{field("client_ip_hash_pepper", "client_ip_hash_pepper")},
		},
		{
			key: "wireguard-handoff", resource: "wireguard.yml", purpose: "WireGuard peer handoff encryption", required: required,
			fields: []credentialFieldSpec{field("handoff_encryption_key", "handoff_encryption_key")},
		},
		{
			key: "proxy-auth", resource: "proxy.yml", purpose: "Hosted proxy authentication and WireGuard identity", required: required,
			fields: []credentialFieldSpec{
				field("secrets", "secrets"),
				field("wg.private_key", "wg", "private_key"),
				field("wg.public_key", "wg", "public_key"),
			},
		},
		{
			key: "object-storage", resource: "minio.yml", purpose: "Durable object storage", required: required,
			fields: []credentialFieldSpec{
				field("access_key", "access_key"),
				field("secret_key", "secret_key"),
			},
		},
		{
			key: "account-email", resource: "aws.yml", purpose: "Transactional account email", required: required,
			fields: []credentialFieldSpec{
				field("aws.access_key_id", "aws", "access_key_id"),
				field("aws.secret_access_key", "aws", "secret_access_key"),
			},
		},
		{
			key: "product-updates", resource: "brevo.yml", purpose: "Product-update delivery and webhook authentication", required: required,
			fields: []credentialFieldSpec{
				field("brevo.api_key", "brevo", "api_key"),
				field("brevo.webhook_bearers", "brevo", "webhook_bearers"),
			},
		},
		{
			key: "provider-egress", resource: "provider_egress.yml", purpose: "Provider egress result ingestion", required: required,
			fields: []credentialFieldSpec{field("ingest_secret", "ingest_secret")},
		},
		{
			key: "stats-integrity", resource: "stats.yml", purpose: "Public statistics privacy hashing", required: required,
			fields: []credentialFieldSpec{field("hmac_salt", "hmac_salt")},
		},
		{
			key: "walletconnect", resource: "walletconnect.yml", purpose: "WalletConnect project authentication", required: required,
			fields: []credentialFieldSpec{field("project_id", "project_id")},
		},
		{
			key: "ipinfo", resource: "ipinfo.yml", purpose: "IP geolocation lookup", required: required,
			fields: []credentialFieldSpec{field("ipinfo.access_token", "ipinfo", "access_token")},
		},
		{
			key: "apple-crash-reporting", resource: "apple-reporting.yml", purpose: "Apple crash-report retrieval", required: false,
			fields: []credentialFieldSpec{
				field("issuer_id", "issuer_id"),
				field("key_id", "key_id"),
				field("private_key", "private_key"),
			},
		},
		{
			key: "google-play-crash-reporting", resource: "google-play-reporting.json", purpose: "Google Play crash-report retrieval", required: false,
			fields: []credentialFieldSpec{
				field("client_email", "client_email"),
				field("private_key", "private_key"),
				field("private_key_id", "private_key_id"),
				field("token_uri", "token_uri"),
			},
		},
	}
	if serviceEnabled("api") {
		specs = append(specs,
			credentialRequirementSpec{
				key: "apple-sign-in", resource: "apple.yml", purpose: "Apple sign-in audience validation", required: required,
				fields: []credentialFieldSpec{
					field("client_id", "client_id"),
				},
			},
			credentialRequirementSpec{
				key: "google-sign-in", resource: "google.yml", purpose: "Google sign-in audience validation and browser authorization-code exchange", required: required,
				fields: []credentialFieldSpec{
					field("client_id", "client_id"),
					field("sign_in_oauth.client_id", "sign_in_oauth", "client_id"),
					field("sign_in_oauth.client_secret", "sign_in_oauth", "client_secret"),
				},
			},
		)
	}
	if analyticsFields := enabledAnalyticsCredentialFields(); len(analyticsFields) != 0 {
		specs = append(specs, credentialRequirementSpec{
			key: "analytics", resource: "analytics.yml", purpose: "Enabled search and webmaster analytics collection",
			required: required && serviceEnabled("taskworker"), fields: analyticsFields,
		})
	}
	if serviceEnabled("mcp") {
		specs = append(specs, credentialRequirementSpec{
			key: "anthropic", resource: "anthropic.yml", purpose: "MCP Anthropic provider", required: required,
			fields: []credentialFieldSpec{field("anthropic.api_key", "anthropic", "api_key")},
		})
	}
	if stConfiguredEnabled {
		specs = append(specs,
			credentialRequirementSpec{
				key: "subnet", resource: "st.yml", purpose: "Enabled subnet signing, artifact, and settlement identities", required: true,
				fields: []credentialFieldSpec{
					field("deployment_id", "deployment_id"),
					field("genesis_hash", "genesis_hash"),
					field("policy_hash", "policy_hash"),
					field("root_key", "root_key"),
					field("artifact_key", "artifact_key"),
					field("deposit_key", "deposit_key"),
				},
			},
			credentialRequirementSpec{
				key: "subnet-verification", resource: "verify.yml", purpose: "Enabled subnet route-verification signing and egress hashing", required: true,
				fields: []credentialFieldSpec{
					field("keys", "keys"),
					field("egress_hash_key", "egress_hash_key"),
				},
			},
		)
	}

	requirements := make([]CredentialRequirement, 0, len(specs))
	for _, spec := range specs {
		requirements = append(requirements, inspectCredentialRequirement(spec))
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Key < requirements[j].Key })
	return requirements
}

// enabledAnalyticsCredentialFields mirrors the controller's actual gates: a
// provider needs a credential only when global search ingestion, its API
// adapter, and at least one matching site property are all enabled. This keeps
// a deliberately unused provider from becoming a false missing-secret page.
func enabledAnalyticsCredentialFields() []credentialFieldSpec {
	config, err := model.LoadAnalyticsConfig()
	if err != nil || !config.Enabled || !config.Search.Enabled {
		return nil
	}
	hasSiteProperty := func(property func(model.AnalyticsSiteProperties) string) bool {
		for _, site := range config.Sites {
			if strings.TrimSpace(property(site.Properties)) != "" {
				return true
			}
		}
		return false
	}
	fields := make([]credentialFieldSpec, 0, 4)
	if provider := config.Providers.Google; provider.Enabled && provider.Mode == "api" &&
		hasSiteProperty(func(properties model.AnalyticsSiteProperties) string { return properties.GoogleSearchConsole }) {
		fields = append(fields, credentialFieldSpec{
			name: "google_search_console.service_account_json", path: []string{"google_search_console", "service_account_json"},
		})
	}
	if provider := config.Providers.Bing; provider.Enabled && provider.Mode == "api" && provider.Protocol == "rest" &&
		hasSiteProperty(func(properties model.AnalyticsSiteProperties) string { return properties.BingWebmaster }) {
		fields = append(fields, credentialFieldSpec{
			name: "bing_webmaster.api_key", path: []string{"bing_webmaster", "api_key"},
		})
	}
	if provider := config.Providers.Yandex; provider.Enabled && provider.Mode == "api" &&
		hasSiteProperty(func(properties model.AnalyticsSiteProperties) string { return properties.YandexHostID }) {
		fields = append(fields,
			credentialFieldSpec{name: "yandex_webmaster.oauth_token", path: []string{"yandex_webmaster", "oauth_token"}},
			credentialFieldSpec{name: "yandex_webmaster.user_id", path: []string{"yandex_webmaster", "user_id"}},
		)
	}
	return fields
}

func inspectCredentialRequirement(spec credentialRequirementSpec) CredentialRequirement {
	requirement := CredentialRequirement{
		Key: spec.key, Resource: spec.resource, Purpose: spec.purpose, Required: spec.required,
	}
	resource, err := server.Vault.SimpleResource(spec.resource)
	if err != nil {
		for _, field := range spec.fields {
			requirement.MissingFields = append(requirement.MissingFields, field.name)
		}
		return requirement
	}
	requirement.Present = true
	values, err := resource.ParseE()
	if err != nil {
		requirement.Malformed = true
		return requirement
	}
	for _, field := range spec.fields {
		var value any = values
		for _, component := range field.path {
			object, ok := value.(map[string]any)
			if !ok {
				value = nil
				break
			}
			value = object[component]
		}
		if !credentialValuePresent(value) {
			requirement.MissingFields = append(requirement.MissingFields, field.name)
		}
	}
	sort.Strings(requirement.MissingFields)
	return requirement
}

func credentialValuePresent(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) != 0
	case []string:
		return len(typed) != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return value != nil
	}
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
		if errors.Is(err, server.ErrResourceNotFound) {
			return GooglePlayReportingSettings{}
		}
		return GooglePlayReportingSettings{
			Enabled: true, LoadError: fmt.Errorf("google-play-reporting.json is unavailable"),
		}
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
		settings.LoadError = fmt.Errorf("google.yml is unavailable")
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
		if errors.Is(err, server.ErrResourceNotFound) {
			return AppleReportingSettings{}
		}
		return AppleReportingSettings{
			Enabled: true, LoadError: fmt.Errorf("apple-reporting.yml is unavailable"),
		}
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
		settings.LoadError = fmt.Errorf("apple.yml is unavailable")
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
				Block:         configuredHost + "-" + interfaceName,
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
