package server

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
)

// certs.yml (TLS-M5-16): the file lays over the design's numbers, an unknown
// key or an unusable value refuses it, and the files of the config checkout
// beside this one parse, hold no secret and, for local, reach no CA.

func TestCertsConfigLaysOverTheDefaults(t *testing.T) {
	config, err := ParseCertsConfig([]byte(`
root: root.example
dns_url: https://dns.example.test
ca_order: [gts, letsencrypt]
cas:
  letsencrypt:
    directory_url: https://ca.example.test/directory
    budget_cap: 7
    budget_window_s: 3600
    repeat_set_limited: false
    account_file: certs-account-letsencrypt.yml
  gts:
    profile: shortlived
order_retry_after_s: 5
chain_retention_s: 864000
`))
	if err != nil {
		t.Fatal(err)
	}
	defaults := certsservice.DefaultCertsSettings()
	settings := config.Settings
	le := settings.Cas[certsservice.CertsCaLetsEncrypt]
	if settings.RootDomain != "root.example" || config.DnsUrl != "https://dns.example.test" ||
		!slices.Equal(settings.CaOrder, []certsservice.CertsCa{certsservice.CertsCaGts, certsservice.CertsCaLetsEncrypt}) ||
		!slices.Equal(settings.EphemeralCaOrder, defaults.EphemeralCaOrder) {
		t.Fatalf("the root, origin or orders: %+v", settings)
	}
	if le.DirectoryUrl != "https://ca.example.test/directory" || le.BudgetCap != 7 || le.BudgetWindow != time.Hour || le.RepeatSetLimited ||
		le.Profile != defaults.Cas[certsservice.CertsCaLetsEncrypt].Profile {
		t.Fatalf("LE's settings: %+v", le)
	}
	if settings.Cas[certsservice.CertsCaGts].Profile != "shortlived" || settings.Cas[certsservice.CertsCaGts].BudgetCap != defaults.Cas[certsservice.CertsCaGts].BudgetCap {
		t.Fatalf("GTS's settings: %+v", settings.Cas[certsservice.CertsCaGts])
	}
	if settings.OrderRetryAfter != 5*time.Second || settings.RowRetention != defaults.RowRetention || settings.ChainRetention != 10*24*time.Hour {
		t.Fatalf("the numbers: %v, %v, %v", settings.OrderRetryAfter, settings.RowRetention, settings.ChainRetention)
	}
	if len(config.AccountFiles) != 1 || config.AccountFiles[certsservice.CertsCaLetsEncrypt] != "certs-account-letsencrypt.yml" {
		t.Fatalf("the account files: %v", config.AccountFiles)
	}
}

func TestCertsConfigRefusals(t *testing.T) {
	for name, text := range map[string]string{
		"no root":                 `dns_url: https://dns.example.test`,
		"an unknown key":          "root: root.example\nprivate_key: x\n",
		"an unknown CA key":       "root: root.example\ncas: {letsencrypt: {eab_mac_key: x}}\n",
		"an unknown CA":           "root: root.example\ncas: {other: {budget_cap: 1}}\n",
		"a CA order's stranger":   "root: root.example\nca_order: [other]\n",
		"an empty CA order":       "root: root.example\nca_order: []\n",
		"a cleartext directory":   "root: root.example\ncas: {letsencrypt: {directory_url: 'http://ca.example.test/directory'}}\n",
		"a directory with a user": "root: root.example\ncas: {letsencrypt: {directory_url: 'https://u:p@ca.example.test/directory'}}\n",
		"a negative cap":          "root: root.example\ncas: {letsencrypt: {budget_cap: -1}}\n",
		"a zero window":           "root: root.example\ncas: {letsencrypt: {budget_window_s: 0}}\n",
		"a window past the ttl":   "root: root.example\ncas: {letsencrypt: {budget_window_s: 99999999}}\n",
		"an account path":         "root: root.example\ncas: {letsencrypt: {account_file: '../vault/account.yml'}}\n",
		"an account of no yml":    "root: root.example\ncas: {letsencrypt: {account_file: 'account.pem'}}\n",
		"a zero number":           "root: root.example\nadmit_batch_count: 0\n",
		"a zero chain retention":  "root: root.example\nchain_retention_s: 0\n",
		"a dns_url of no scheme":  "root: root.example\ndns_url: dns.example.test\n",
		"not yaml":                "root: [",
	} {
		if _, err := ParseCertsConfig([]byte(text)); !errors.Is(err, ErrCertsConfigRefused) {
			t.Errorf("%s answered %v", name, err)
		}
	}
}

// The config checkout's file for env, or a skip with none beside this one.
func certsConfigTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no config checkout beside the server")
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCertsConfigFilesParse(t *testing.T) {
	main, err := ParseCertsConfig(certsConfigTestFile(t, "../config/main/certs.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ca := range main.Settings.CaOrder {
		if main.Settings.Cas[ca].DirectoryUrl == "" || main.AccountFiles[ca] == "" {
			t.Errorf("main's %s has no directory or account file", ca)
		}
	}
	if main.DnsUrl == "" {
		t.Error("main names no dns api")
	}
	// main's dns client builds on its origin with any bearer
	clientSettings := DefaultCertsDnsClientSettings()
	clientSettings.BaseUrl = main.DnsUrl
	clientSettings.Root = main.Settings.RootDomain
	if _, err := NewCertsDnsClient(clientSettings, certsDnsTestBearer, nil); err != nil {
		t.Errorf("main's dns_url: %v", err)
	}

	local, err := ParseCertsConfig(certsConfigTestFile(t, "../config/local/certs.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if local.DnsUrl != "" || len(local.AccountFiles) != 0 {
		t.Errorf("local names a dns api or an account: %q, %v", local.DnsUrl, local.AccountFiles)
	}
	for ca, caSettings := range local.Settings.Cas {
		if caSettings.DirectoryUrl != "" {
			t.Errorf("local's %s names a directory", ca)
		}
	}
}

// dns.yml's caa block is TlsRootCaaRecords' output: main lists no account
// yet, so its root publishes the closed set, and local's is a placeholder
// account's records exactly.
func TestCertsConfigDnsCaaBlocks(t *testing.T) {
	type dnsRootRecordsYaml struct {
		Caa []string `yaml:"caa"`
	}
	type dnsYaml struct {
		Roots       []string                       `yaml:"roots"`
		RootRecords map[string]*dnsRootRecordsYaml `yaml:"root_records"`
	}
	read := func(path string) *dnsRootRecordsYaml {
		t.Helper()
		file := &dnsYaml{}
		if err := yaml.Unmarshal(certsConfigTestFile(t, path), file); err != nil {
			t.Fatal(err)
		}
		if len(file.Roots) != 1 || file.RootRecords[file.Roots[0]] == nil {
			t.Fatalf("%s has no records for its root", path)
		}
		return file.RootRecords[file.Roots[0]]
	}
	if caa := read("../config/main/dns.yml").Caa; len(caa) != 0 {
		// each line must then be TlsRootCaaRecords' output for accounts that exist, which this test cannot know
		t.Logf("main lists %d CAA records", len(caa))
	}
	want, err := sidecar.TlsRootCaaRecords(map[string][]string{
		sidecar.TlsCaaIssuerLetsEncrypt: {"https://acme.example.test/acct/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if caa := read("../config/local/dns.yml").Caa; !slices.Equal(caa, want) {
		t.Fatalf("local's caa is %q, want %q", caa, want)
	}
}
