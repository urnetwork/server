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
	"github.com/urnetwork/server/model"
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
	stEnabled := controller.StEnabled()
	stDeploymentKey := ""
	if stEnabled {
		if key, ok := controller.StDeploymentKey(); ok {
			stDeploymentKey = string(key)
		}
	}
	settings := SignalSettings{
		Environment:         env,
		PublicDomain:        strings.TrimSpace(services.Domain),
		WebsiteDomain:       activeWebsiteDomainFromServices(services),
		ManagerHostname:     activeManagerHostnameFromServices(services),
		LogServices:         logServices,
		LogServiceBlocks:    logServiceBlocks,
		VerificationEnabled: stEnabled,
		STDeploymentKey:     stDeploymentKey,
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
		Credentials:    loadCredentialRequirements(env, stEnabled, logServices),
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

// loadCredentialRequirements is the proactive counterpart to SIGNALS.md
// §8.7's route-failure log classifier. Main runs every listed core and
// payment integration, so a missing resource or field is a release defect.
// Crash-report resources remain optional by contract: absence is a no-op, but
// a present partial credential is still observable.
func loadCredentialRequirements(environment string, stEnabled bool, services []string) []CredentialRequirement {
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
				field("api.account_id", "api", "account_id"),
				field("api.key_name", "api", "key_name"),
				field("api.private_key", "api", "private_key"),
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
	if stEnabled {
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
