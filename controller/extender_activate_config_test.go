package controller

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The activation budgets of a caller's address that the operator's config sets
// (extender_activate_config.go): each family's limit, its limit for a proven
// key, and its prefix, read from config/<env>/extender.yml over the defaults,
// and the budget an activation spends under them. Before, every family's
// address had the one compiled limit, 6 an hour, and no config could change
// it; an ipv6 /64 now has its own (owner, 2026-11-20).

// The address budgets of main's config/main/extender.yml (2026-11-20).
const testExtenderActivateMainConfig = `
activation:
  address:
    ipv4:
      prefix_bit_count: 32
      rate_limit: 6
      proven_rate_limit: 6
    ipv6:
      prefix_bit_count: 64
      rate_limit: 60
      proven_rate_limit: 6
`

// The settings an operator's config resource of `body` gives, read through the
// config resolver as a process reads it.
func testExtenderActivateSettingsFromConfig(t testing.TB, body string) *ExtenderActivateSettings {
	t.Helper()
	pop := server.Config.PushSimpleResource(extenderActivateConfigResourceName, []byte(body))
	defer pop()
	return EnvExtenderActivateSettings()
}

// The address budget settings, as one value to compare.
type testExtenderActivateAddressSettings struct {
	ipv4PrefixBitCount  int
	ipv4RateLimit       int
	ipv4ProvenRateLimit int
	ipv6PrefixBitCount  int
	ipv6RateLimit       int
	ipv6ProvenRateLimit int
}

func testExtenderActivateAddressSettingsOf(settings *ExtenderActivateSettings) testExtenderActivateAddressSettings {
	return testExtenderActivateAddressSettings{
		ipv4PrefixBitCount:  settings.AddressIpv4PrefixBitCount,
		ipv4RateLimit:       settings.AddressIpv4RateLimit,
		ipv4ProvenRateLimit: settings.ProvenAddressIpv4RateLimit,
		ipv6PrefixBitCount:  settings.AddressIpv6PrefixBitCount,
		ipv6RateLimit:       settings.AddressIpv6RateLimit,
		ipv6ProvenRateLimit: settings.ProvenAddressIpv6RateLimit,
	}
}

// The defaults: 6 an hour per /32 and per /64, and 6 for a proven key.
var testExtenderActivateDefaultAddressSettings = testExtenderActivateAddressSettings{
	ipv4PrefixBitCount:  32,
	ipv4RateLimit:       6,
	ipv4ProvenRateLimit: 6,
	ipv6PrefixBitCount:  64,
	ipv6RateLimit:       6,
	ipv6ProvenRateLimit: 6,
}

// A file of the config repository: URNETWORK_CONFIG_ROOT (a config worktree),
// or the checkout next to the server's. Skips the test when there is none.
func testConfigRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	root := os.Getenv("URNETWORK_CONFIG_ROOT")
	if root == "" {
		root = filepath.Join("..", "..", "config")
	}
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Skipf("config repo not next to the server checkout: %s", err)
	}
	return data
}

// Each key of the config sets its own setting, and no other: the values here
// all differ from the defaults and from each other, so a key read into another
// family's or another budget's setting shows. The settings the config does not
// hold keep their defaults.
func TestExtenderActivateConfigSetsEachFamilysAddressBudget(t *testing.T) {
	settings := testExtenderActivateSettingsFromConfig(t, `
activation:
  address:
    ipv4:
      prefix_bit_count: 30
      rate_limit: 7
      proven_rate_limit: 8
    ipv6:
      prefix_bit_count: 56
      rate_limit: 60
      proven_rate_limit: 9
`)
	connect.AssertEqual(t, testExtenderActivateAddressSettingsOf(settings), testExtenderActivateAddressSettings{
		ipv4PrefixBitCount:  30,
		ipv4RateLimit:       7,
		ipv4ProvenRateLimit: 8,
		ipv6PrefixBitCount:  56,
		ipv6RateLimit:       60,
		ipv6ProvenRateLimit: 9,
	})

	defaults := DefaultExtenderActivateSettings()
	connect.AssertEqual(t, settings.ClientRateLimit, defaults.ClientRateLimit)
	connect.AssertEqual(t, settings.AccountRateLimit, defaults.AccountRateLimit)
	connect.AssertEqual(t, settings.RateLimitWindow, time.Hour)
	connect.AssertEqual(t, settings.ProvenHostBlockIpv4PrefixBitCount, defaults.ProvenHostBlockIpv4PrefixBitCount)
	connect.AssertEqual(t, settings.ProvenHostBlockIpv6PrefixBitCount, defaults.ProvenHostBlockIpv6PrefixBitCount)
	connect.AssertEqual(t, settings.MaxConcurrentProbes, defaults.MaxConcurrentProbes)
}

// main's config/main/extender.yml (2026-11-20, owner): an ipv6 /64 has 60
// activations an hour, an ipv4 /32 keeps 6, and a proven key keeps 6 on
// each. The checked-in file parses, every key it holds is in range, and it
// gives these budgets.
func TestExtenderActivateConfigFile(t *testing.T) {
	data := testConfigRepoFile(t, filepath.Join("main", "extender.yml"))

	pop := server.Config.PushSimpleResource(extenderActivateConfigResourceName, data)
	defer pop()
	resource, err := server.Config.SimpleResource(extenderActivateConfigResourceName)
	if err != nil {
		t.Fatal(err)
	}
	var config extenderActivateConfigYaml
	if err := resource.UnmarshalYamlE(&config); err != nil {
		t.Fatalf("main/extender.yml does not parse: %s", err)
	}
	for _, family := range []*extenderActivateAddressConfigYaml{
		&config.Activation.Address.Ipv4,
		&config.Activation.Address.Ipv6,
	} {
		if family.PrefixBitCount == nil || family.RateLimit == nil || family.ProvenRateLimit == nil {
			t.Errorf("main/extender.yml leaves a key of a family's address budget to the default: %+v", family)
		}
	}
	if problems := config.apply(DefaultExtenderActivateSettings()); len(problems) != 0 {
		t.Errorf("main/extender.yml has keys out of range: %s", strings.Join(problems, "; "))
	}

	settings := EnvExtenderActivateSettings()
	connect.AssertEqual(t, testExtenderActivateAddressSettingsOf(settings), testExtenderActivateAddressSettings{
		ipv4PrefixBitCount:  32,
		ipv4RateLimit:       6,
		ipv4ProvenRateLimit: 6,
		ipv6PrefixBitCount:  64,
		ipv6RateLimit:       60,
		ipv6ProvenRateLimit: 6,
	})
	connect.AssertEqual(t, settings.RateLimitWindow, time.Hour)
}

// Without the resource, or with keys absent, each setting the config does not
// hold keeps its default. A config of ipv6's limit alone raises that limit and
// nothing else.
func TestExtenderActivateConfigKeepsTheDefaultsOfAbsentKeys(t *testing.T) {
	absent := extenderActivateSettingsFromConfig(nil, fmt.Errorf("%w in config (extender.yml)", server.ErrResourceNotFound))
	connect.AssertEqual(t, testExtenderActivateAddressSettingsOf(absent), testExtenderActivateDefaultAddressSettings)

	ipv6RateLimitOnly := testExtenderActivateDefaultAddressSettings
	ipv6RateLimitOnly.ipv6RateLimit = 60
	ipv4ProvenOnly := testExtenderActivateDefaultAddressSettings
	ipv4ProvenOnly.ipv4ProvenRateLimit = 3
	for _, c := range []struct {
		name string
		body string
		want testExtenderActivateAddressSettings
	}{
		{name: "an empty resource", body: "", want: testExtenderActivateDefaultAddressSettings},
		{name: "no activation block", body: "other: 1\n", want: testExtenderActivateDefaultAddressSettings},
		{name: "an empty activation block", body: "activation:\n", want: testExtenderActivateDefaultAddressSettings},
		{name: "an empty address block", body: "activation:\n  address:\n", want: testExtenderActivateDefaultAddressSettings},
		{name: "an empty ipv6 block", body: "activation:\n  address:\n    ipv6:\n", want: testExtenderActivateDefaultAddressSettings},
		{name: "ipv6's limit alone", body: "activation:\n  address:\n    ipv6:\n      rate_limit: 60\n", want: ipv6RateLimitOnly},
		{name: "ipv4's proven limit alone", body: "activation:\n  address:\n    ipv4:\n      proven_rate_limit: 3\n", want: ipv4ProvenOnly},
	} {
		got := testExtenderActivateAddressSettingsOf(testExtenderActivateSettingsFromConfig(t, c.body))
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A key out of range keeps its default and is reported, and the keys in range
// beside it still apply: a limit that is not positive would refuse every
// activation of its family, and a prefix wider than the proven hosts' block or
// longer than an address would put callers in budgets the rest of activation
// does not expect, or every caller in one.
func TestExtenderActivateConfigKeepsTheDefaultOfAKeyOutOfRange(t *testing.T) {
	for _, c := range []struct {
		name    string
		family  string
		key     string
		value   int
		problem string
	}{
		{name: "a zero ipv4 limit", family: "ipv4", key: "rate_limit", value: 0, problem: "activation.address.ipv4.rate_limit 0 is not positive, so it stays 6"},
		{name: "a negative ipv6 limit", family: "ipv6", key: "rate_limit", value: -60, problem: "activation.address.ipv6.rate_limit -60 is not positive, so it stays 6"},
		{name: "a zero ipv6 proven limit", family: "ipv6", key: "proven_rate_limit", value: 0, problem: "activation.address.ipv6.proven_rate_limit 0 is not positive, so it stays 6"},
		{name: "an ipv4 prefix wider than a /24", family: "ipv4", key: "prefix_bit_count", value: 23, problem: "activation.address.ipv4.prefix_bit_count 23 is not 24-32, so it stays 32"},
		{name: "an ipv4 prefix longer than an address", family: "ipv4", key: "prefix_bit_count", value: 33, problem: "activation.address.ipv4.prefix_bit_count 33 is not 24-32, so it stays 32"},
		{name: "an ipv6 prefix wider than a /48", family: "ipv6", key: "prefix_bit_count", value: 47, problem: "activation.address.ipv6.prefix_bit_count 47 is not 48-128, so it stays 64"},
		{name: "an ipv6 prefix of nothing", family: "ipv6", key: "prefix_bit_count", value: 0, problem: "activation.address.ipv6.prefix_bit_count 0 is not 48-128, so it stays 64"},
		{name: "an ipv6 prefix longer than an address", family: "ipv6", key: "prefix_bit_count", value: 129, problem: "activation.address.ipv6.prefix_bit_count 129 is not 48-128, so it stays 64"},
	} {
		// the key out of range in its family's block, and ipv6's limit in
		// range beside it, unless that is the key out of range
		familyKeys := map[string]string{}
		familyKeys[c.family] = fmt.Sprintf("      %s: %d\n", c.key, c.value)
		want := testExtenderActivateDefaultAddressSettings
		if c.family != "ipv6" || c.key != "rate_limit" {
			familyKeys["ipv6"] += "      rate_limit: 60\n"
			want.ipv6RateLimit = 60
		}
		body := "activation:\n  address:\n    ipv4:\n" + familyKeys["ipv4"] + "    ipv6:\n" + familyKeys["ipv6"]

		config := &extenderActivateConfigYaml{}
		pop := server.Config.PushSimpleResource(extenderActivateConfigResourceName, []byte(body))
		resource, err := server.Config.SimpleResource(extenderActivateConfigResourceName)
		if err == nil {
			err = resource.UnmarshalYamlE(config)
		}
		envSettings := EnvExtenderActivateSettings()
		pop()
		if err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		settings := DefaultExtenderActivateSettings()
		connect.AssertEqual(t, config.apply(settings), []string{c.problem})
		if got := testExtenderActivateAddressSettingsOf(settings); got != want {
			t.Errorf("%s: %+v, want %+v", c.name, got, want)
		}
		if got := testExtenderActivateAddressSettingsOf(envSettings); got != want {
			t.Errorf("%s, read through the resolver: %+v, want %+v", c.name, got, want)
		}
	}
}

// A resource that cannot be read, or that does not parse, keeps every default,
// the keys of it that could be read included. A key this server does not read
// is ignored, and the keys it does read still apply.
func TestExtenderActivateConfigThatDoesNotParseKeepsTheDefaults(t *testing.T) {
	unreadable := extenderActivateSettingsFromConfig(nil, fmt.Errorf("%w: extender.yml is not a regular file", server.ErrResourceUnavailable))
	connect.AssertEqual(t, testExtenderActivateAddressSettingsOf(unreadable), testExtenderActivateDefaultAddressSettings)

	for _, c := range []struct {
		name string
		body string
	}{
		{name: "not yaml", body: "activation: [\n"},
		{name: "a limit that is not a number", body: "activation:\n  address:\n    ipv6:\n      rate_limit: sixty\n    ipv4:\n      rate_limit: 7\n"},
		{name: "a family that is not a block", body: "activation:\n  address:\n    ipv6: 60\n    ipv4:\n      rate_limit: 7\n"},
		{name: "a key given twice", body: "activation:\n  address:\n    ipv6:\n      rate_limit: 60\n      rate_limit: 61\n"},
	} {
		got := testExtenderActivateAddressSettingsOf(testExtenderActivateSettingsFromConfig(t, c.body))
		if got != testExtenderActivateDefaultAddressSettings {
			t.Errorf("%s: %+v, want the defaults %+v", c.name, got, testExtenderActivateDefaultAddressSettings)
		}
	}

	later := testExtenderActivateSettingsFromConfig(t, `
activation:
  window: 2h
  client:
    rate_limit: 12
  address:
    ipv6:
      rate_limit: 60
      block_prefix_bit_count: 40
other: true
`)
	want := testExtenderActivateDefaultAddressSettings
	want.ipv6RateLimit = 60
	connect.AssertEqual(t, testExtenderActivateAddressSettingsOf(later), want)
	connect.AssertEqual(t, later.RateLimitWindow, time.Hour)
	connect.AssertEqual(t, later.ClientRateLimit, 6)
	connect.AssertEqual(t, later.ProvenHostBlockIpv6PrefixBitCount, 48)
}

// The budget of the caller's address that an activation spends
// (addressBudget) has its family's limit: under main's config, 60 an hour for
// an ipv6 caller's /64 and 6 for an ipv4 caller's /32, each over the hour. A
// key its network proved at the address spends its own budget there, with its
// family's limit for a proven key. Before, every address budget had the one
// limit, AddressRateLimit, whatever the family.
func TestExtenderActivateAddressBudgetIsTheFamilys(t *testing.T) {
	// the pepper the address hash is keyed with, synthetic and test only
	t.Cleanup(server.Vault.PushSimpleResource(
		"client.yml",
		[]byte("client_ip_hash_pepper: synthetic-extender-activation-config-test-only\n"),
	))
	publicKey := []byte("synthetic extender identity, 32b")
	ipv4 := netip.MustParseAddr("198.51.100.7")
	ipv6 := netip.MustParseAddr("2001:db8:1:2::1")

	settings := testExtenderActivateSettingsFromConfig(t, testExtenderActivateMainConfig)
	for _, c := range []struct {
		name            string
		clientIp        netip.Addr
		ipVersion       int
		provenAtAddress bool
		want            model.AccountActionBudget
	}{
		{
			name:      "an ipv4 caller",
			clientIp:  ipv4,
			ipVersion: 4,
			want: model.AccountActionBudget{
				Id:                settings.addressBudgetId(ipv4),
				Action:            model.AccountActionExtenderActivateAddressV4,
				Limit:             6,
				Window:            time.Hour,
				AttemptTimeSpread: time.Second,
			},
		},
		{
			name:      "an ipv6 caller",
			clientIp:  ipv6,
			ipVersion: 6,
			want: model.AccountActionBudget{
				Id:                settings.addressBudgetId(ipv6),
				Action:            model.AccountActionExtenderActivateAddressV6,
				Limit:             60,
				Window:            time.Hour,
				AttemptTimeSpread: time.Second,
			},
		},
		{
			name:            "an ipv4 key proven at the address",
			clientIp:        ipv4,
			ipVersion:       4,
			provenAtAddress: true,
			want: model.AccountActionBudget{
				Id:                settings.provenAddressBudgetId(ipv4, publicKey),
				Action:            model.AccountActionExtenderActivateProvenAddressV4,
				Limit:             6,
				Window:            time.Hour,
				AttemptTimeSpread: time.Second,
			},
		},
		{
			name:            "an ipv6 key proven at the address",
			clientIp:        ipv6,
			ipVersion:       6,
			provenAtAddress: true,
			want: model.AccountActionBudget{
				Id:                settings.provenAddressBudgetId(ipv6, publicKey),
				Action:            model.AccountActionExtenderActivateProvenAddressV6,
				Limit:             6,
				Window:            time.Hour,
				AttemptTimeSpread: time.Second,
			},
		},
	} {
		got := settings.addressBudget(c.clientIp, c.ipVersion, publicKey, c.provenAtAddress)
		if got.ExtendLimitInTx != nil {
			t.Errorf("%s: the address budget grows its limit", c.name)
		}
		if got.Id != c.want.Id || got.Action != c.want.Action || got.Limit != c.want.Limit || got.Window != c.want.Window || got.AttemptTimeSpread != c.want.AttemptTimeSpread {
			t.Errorf("%s: the budget is %+v, want %+v", c.name, got, c.want)
		}
	}

	// each limit is the family's own: one that differs from every other shows
	// which setting a budget took its limit from
	distinct := testExtenderActivateSettingsFromConfig(t, `
activation:
  address:
    ipv4:
      rate_limit: 7
      proven_rate_limit: 8
    ipv6:
      rate_limit: 60
      proven_rate_limit: 9
`)
	connect.AssertEqual(t, distinct.addressBudget(ipv4, 4, publicKey, false).Limit, 7)
	connect.AssertEqual(t, distinct.addressBudget(ipv4, 4, publicKey, true).Limit, 8)
	connect.AssertEqual(t, distinct.addressBudget(ipv6, 6, publicKey, false).Limit, 60)
	connect.AssertEqual(t, distinct.addressBudget(ipv6, 6, publicKey, true).Limit, 9)

	// the prefix the config sets is the address the budget is kept for: two
	// /64s of one /56 share a budget at a prefix of 56, and not at 64
	wide := testExtenderActivateSettingsFromConfig(t, "activation:\n  address:\n    ipv6:\n      prefix_bit_count: 56\n")
	firstSixtyFour := netip.MustParseAddr("2001:db8:1:2::1")
	secondSixtyFour := netip.MustParseAddr("2001:db8:1:3::1")
	connect.AssertEqual(t, wide.addressBudget(firstSixtyFour, 6, publicKey, false).Id, wide.addressBudget(secondSixtyFour, 6, publicKey, false).Id)
	if settings.addressBudget(firstSixtyFour, 6, publicKey, false).Id == settings.addressBudget(secondSixtyFour, 6, publicKey, false).Id {
		t.Errorf("two /64s share a budget at a prefix of 64")
	}
}

// An activation spends the budget of its caller's address with its family's
// limit (connect#230), through the activation itself. Under main's config an
// ipv4 address and an ipv6 /64 that have each had 6 activations in the last
// hour differ: the ipv4 address has spent its 6 and refuses the next post,
// unprobed, while the /64 has room for 60 and the post is probed and
// activated. Once the /64 has had 60, another key from it is refused. Before,
// both families' addresses had the one limit of 6, so the ipv6 post was
// refused as the ipv4 one was.
func TestExtenderActivateSpendsTheFamilysAddressLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		settings := testExtenderActivateSettingsFromConfig(t, testExtenderActivateMainConfig)
		connect.AssertEqual(t, settings.AddressIpv4RateLimit, 6)
		connect.AssertEqual(t, settings.AddressIpv6RateLimit, 60)
		service := newExtenderActivateService(settings)
		probes := newTestCountingProbes()
		service.testProbes = probes.probes(nil)

		const ipv4 = "198.51.100.61"
		const ipv6 = "2001:db8:61::1"
		// another address of the same /64
		const otherIpv6 = "2001:db8:61::2"
		attemptTime := server.NowUtc().Add(-time.Minute)
		seedTestAccountActionAttempts(
			t,
			ctx,
			settings.addressBudgetId(netip.MustParseAddr(ipv4)),
			model.AccountActionExtenderActivateAddressV4,
			6,
			attemptTime,
		)
		seedTestAccountActionAttempts(
			t,
			ctx,
			settings.addressBudgetId(netip.MustParseAddr(ipv6)),
			model.AccountActionExtenderActivateAddressV6,
			6,
			attemptTime,
		)

		account := newTestExtenderAccount(ctx)
		host := account.newHost(t, ctx)
		refused := testActivateHost(t, ctx, service, host, ipv4)
		if refused.Activated || !strings.Contains(refused.Error, "activating an extender from this address over IPv4") {
			t.Fatalf("the ipv4 post past its address's 6 was not refused by the address's budget: activated %t, %q", refused.Activated, refused.Error)
		}
		if refused.RetryAfterSeconds <= 0 {
			t.Fatalf("the address's refusal asked for no wait")
		}
		activated := testActivateHost(t, ctx, service, host, ipv6)
		if !activated.Activated {
			t.Fatalf("the ipv6 post was refused with its /64 at 6 of 60: %q", activated.Error)
		}
		connect.AssertEqual(t, probes.count(4), 0)
		connect.AssertEqual(t, probes.count(6), 1)

		// the /64 has had 7; 53 more spend its 60. Another key from it spends
		// the address's budget, where the activated key would spend its own
		seedTestAccountActionAttempts(
			t,
			ctx,
			settings.addressBudgetId(netip.MustParseAddr(ipv6)),
			model.AccountActionExtenderActivateAddressV6,
			53,
			attemptTime,
		)
		otherHost := account.newHost(t, ctx)
		refused = testActivateHost(t, ctx, service, otherHost, otherIpv6)
		if refused.Activated || !strings.Contains(refused.Error, "activating an extender from this address over IPv6") {
			t.Fatalf("the ipv6 post past its /64's 60 was not refused by the address's budget: activated %t, %q", refused.Activated, refused.Error)
		}
		connect.AssertEqual(t, probes.count(6), 1)
	})
}
