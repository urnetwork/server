package server

// config/<env>/certs.yml (TLS-M5-16; §13.8, D-TLS-7): what certs' host lays
// over certsservice.DefaultCertsSettings: the root, the dns api's origin, the
// CA order, and for each CA its ACME directory, profile, budget and the name
// of its account file. Names and numbers only: an account's key and EAB key
// are in the account file, a secret_file of taskworker's vault, and the dns
// bearer in internal-certs.yml beside it. A key this release does not know
// refuses the file, so a mistyped cap never runs as the default.
//
// What is left out keeps the design's number. A CA with no directory_url is
// in the policy and reaches no CA; no dns_url leaves certs on
// certsservice.ClosedCertsDns, which reaches no zone.

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar/certsservice"
)

// A certs.yml no host can run on. The text names the key, never a value.
var ErrCertsConfigRefused = errors.New("certs.yml refused")

type certsCaConfigYaml struct {
	DirectoryUrl        *string `yaml:"directory_url"`
	Profile             *string `yaml:"profile"`
	BudgetCap           *int    `yaml:"budget_cap"`
	BudgetWindowSeconds *int    `yaml:"budget_window_s"`
	RepeatSetLimited    *bool   `yaml:"repeat_set_limited"`
	AccountFile         string  `yaml:"account_file"`
}

type certsConfigYaml struct {
	Root                    string                        `yaml:"root"`
	DnsUrl                  string                        `yaml:"dns_url"`
	CaOrder                 []string                      `yaml:"ca_order"`
	EphemeralCaOrder        []string                      `yaml:"ephemeral_ca_order"`
	Cas                     map[string]*certsCaConfigYaml `yaml:"cas"`
	RepeatSetCount          *int                          `yaml:"repeat_set_count"`
	RepeatSetRefillSeconds  *int                          `yaml:"repeat_set_refill_s"`
	BudgetTtlSeconds        *int                          `yaml:"budget_ttl_s"`
	RowRetentionSeconds     *int                          `yaml:"row_retention_s"`
	ChainRetentionSeconds   *int                          `yaml:"chain_retention_s"`
	AdmitBatchCount         *int                          `yaml:"admit_batch_count"`
	OrderRetryAfterSeconds  *int                          `yaml:"order_retry_after_s"`
	AccountRolloverAgeHours *int                          `yaml:"account_rollover_age_h"`
}

// What certs.yml gives a host.
type CertsConfig struct {
	// The engine's settings: the defaults with the file laid over them, valid.
	Settings *certsservice.CertsSettings
	// The dns api's origin for NewCertsDnsClient. Empty: certs reaches no zone.
	DnsUrl string
	// Each CA's account file, a secret_file's name in taskworker's vault.
	AccountFiles map[certsservice.CertsCa]string
}

// Parses a certs.yml. An unknown key, a URL that is not an https origin or
// path, an account file that is not a bare file name, a number that is not
// positive, and settings the engine refuses are ErrCertsConfigRefused.
func ParseCertsConfig(data []byte) (*CertsConfig, error) {
	refuse := func(format string, arguments ...any) (*CertsConfig, error) {
		return nil, fmt.Errorf("%w: %s", ErrCertsConfigRefused, fmt.Sprintf(format, arguments...))
	}
	file := &certsConfigYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(file); err != nil {
		return refuse("it does not parse: %v", err)
	}
	settings := certsservice.DefaultCertsSettings()
	settings.RootDomain = file.Root
	config := &CertsConfig{Settings: settings, DnsUrl: file.DnsUrl, AccountFiles: map[certsservice.CertsCa]string{}}
	if file.DnsUrl != "" {
		// the client checks the origin again when it is built, loopback included
		if parsed, err := url.Parse(file.DnsUrl); err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return refuse("dns_url is not an origin")
		}
	}
	order := func(names []string) []certsservice.CertsCa {
		cas := make([]certsservice.CertsCa, 0, len(names))
		for _, name := range names {
			cas = append(cas, certsservice.CertsCa(name))
		}
		return cas
	}
	if file.CaOrder != nil {
		settings.CaOrder = order(file.CaOrder)
	}
	if file.EphemeralCaOrder != nil {
		settings.EphemeralCaOrder = order(file.EphemeralCaOrder)
	}
	for name, caFile := range file.Cas {
		ca := certsservice.CertsCa(name)
		caSettings, ok := settings.Cas[ca]
		if !ok || caFile == nil {
			return refuse("cas names %q, which is no CA with settings", name)
		}
		if caFile.DirectoryUrl != nil {
			parsed, err := url.Parse(*caFile.DirectoryUrl)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
				return refuse("cas.%s.directory_url is not an https URL", name)
			}
			caSettings.DirectoryUrl = *caFile.DirectoryUrl
		}
		if caFile.Profile != nil {
			caSettings.Profile = *caFile.Profile
		}
		if caFile.BudgetCap != nil {
			if *caFile.BudgetCap < 0 {
				return refuse("cas.%s.budget_cap is negative", name)
			}
			caSettings.BudgetCap = *caFile.BudgetCap
		}
		if caFile.BudgetWindowSeconds != nil {
			if *caFile.BudgetWindowSeconds <= 0 {
				return refuse("cas.%s.budget_window_s is not positive", name)
			}
			caSettings.BudgetWindow = time.Duration(*caFile.BudgetWindowSeconds) * time.Second
		}
		if caFile.RepeatSetLimited != nil {
			caSettings.RepeatSetLimited = *caFile.RepeatSetLimited
		}
		if caFile.AccountFile != "" {
			if strings.ContainsAny(caFile.AccountFile, `/\`) || strings.HasPrefix(caFile.AccountFile, ".") || !strings.HasSuffix(caFile.AccountFile, ".yml") {
				return refuse("cas.%s.account_file is not a bare .yml file name", name)
			}
			config.AccountFiles[ca] = caFile.AccountFile
		}
	}
	for _, field := range []struct {
		key   string
		value *int
		set   func(value int)
	}{
		{"repeat_set_count", file.RepeatSetCount, func(value int) { settings.RepeatSetCount = value }},
		{"repeat_set_refill_s", file.RepeatSetRefillSeconds, func(value int) { settings.RepeatSetRefill = time.Duration(value) * time.Second }},
		{"budget_ttl_s", file.BudgetTtlSeconds, func(value int) { settings.BudgetTtl = time.Duration(value) * time.Second }},
		{"row_retention_s", file.RowRetentionSeconds, func(value int) { settings.RowRetention = time.Duration(value) * time.Second }},
		{"chain_retention_s", file.ChainRetentionSeconds, func(value int) { settings.ChainRetention = time.Duration(value) * time.Second }},
		{"admit_batch_count", file.AdmitBatchCount, func(value int) { settings.AdmitBatchCount = value }},
		{"order_retry_after_s", file.OrderRetryAfterSeconds, func(value int) { settings.OrderRetryAfter = time.Duration(value) * time.Second }},
		{"account_rollover_age_h", file.AccountRolloverAgeHours, func(value int) { settings.AccountRolloverAge = time.Duration(value) * time.Hour }},
	} {
		if field.value == nil {
			continue
		}
		if *field.value <= 0 {
			return refuse("%s is not positive", field.key)
		}
		field.set(*field.value)
	}
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCertsConfigRefused, err)
	}
	return config, nil
}
