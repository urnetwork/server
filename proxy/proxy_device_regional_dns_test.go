package proxy

// A hosted proxy keeps the resolver settings auth-client stored when it was
// created, and a kz or tm proxy created without settings of its own stored
// the regional recommendation of the day. Until 2026-11-08 both named Google
// Public DNS. The owner decided on 2026-11-09 that such proxies move to the
// current recommendation, and that only an exact former recommendation
// changes. tm and tr have had none since 2026-11-11, so their proxies move to
// connect's defaults, and ir proxies move the same way to the recommendation
// connect gave ir that day. These tests read configs as older servers stored
// them and check the settings a hosted device's tun is built with
// (proxyDeviceDnsResolverSettings).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The former regional recommendations as auth-client stored them: connect
// RegionalDnsResolverSettings as json.Marshal wrote it, oldest first.
var (
	formerKzDnsResolverSettingsJsons = []string{
		// connect e61efd34, 2026-02-09: Google, and a local resolver
		`{"enable_remote_dns":true,"enable_local_dns":true,"remote_dns_ipv4":["8.8.8.8"],"local_dns_ipv4":["1.1.1.1"]}`,
		// connect ee7a476c, 2026-07-10: remote only
		`{"enable_remote_dns":true,"remote_dns_ipv4":["8.8.8.8"]}`,
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.8.8"]}`,
		// connect 588ad5b9, 2026-09-11: Google's v6 address
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.8.8"],"remote_dns_ipv6":["2001:4860:4860::8888"]}`,
		// connect b49cbc21, 2026-11-08: Cloudflare and Quad9's filtering 9.9.9.9
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["1.1.1.1","9.9.9.9"],"remote_dns_ipv6":["2606:4700:4700::1111","2620:fe::fe"]}`,
	}
	formerTmDnsResolverSettingsJsons = []string{
		// connect e61efd34, 2026-02-09: Google, and a local resolver
		`{"enable_remote_dns":true,"enable_local_dns":true,"remote_dns_ipv4":["8.8.4.4"],"local_dns_ipv4":["1.1.1.1"]}`,
		// connect c33aafe1, 2026-02-09: Turkmentelecom ahead of Google
		`{"enable_remote_dns":true,"enable_local_dns":true,"remote_dns_ipv4":["217.174.238.141","8.8.4.4"],"local_dns_ipv4":["1.1.1.1"]}`,
		// connect ee7a476c, 2026-07-10: remote only
		`{"enable_remote_dns":true,"remote_dns_ipv4":["217.174.238.141","8.8.4.4"]}`,
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["217.174.238.141","8.8.4.4"]}`,
		// connect 588ad5b9, 2026-09-11: Google's v6 address
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["217.174.238.141","8.8.4.4"],"remote_dns_ipv6":["2001:4860:4860::8844"]}`,
		// connect b49cbc21, 2026-11-08: Turkmentelecom alone
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["217.174.238.141"]}`,
	}
	formerIrDnsResolverSettingsJsons = []string{
		// connect e61efd34, 2026-02-09: Shecan and 403.online, and a local resolver
		`{"enable_remote_dns":true,"enable_local_dns":true,"remote_dns_ipv4":["178.22.122.100","10.202.10.10"],"local_dns_ipv4":["1.1.1.1"]}`,
		// connect ee7a476c, 2026-07-10: remote only
		`{"enable_remote_dns":true,"remote_dns_ipv4":["178.22.122.100","10.202.10.10"]}`,
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["178.22.122.100","10.202.10.10"]}`,
	}
	formerTrDnsResolverSettingsJsons = []string{
		// connect c33aafe1, 2026-02-09: TTNET and Turkcell Superonline, and a local resolver
		`{"enable_remote_dns":true,"enable_local_dns":true,"remote_dns_ipv4":["195.175.39.39","212.252.114.8"],"local_dns_ipv4":["1.1.1.1"]}`,
		// connect ee7a476c, 2026-07-10: remote only
		`{"enable_remote_dns":true,"remote_dns_ipv4":["195.175.39.39","212.252.114.8"]}`,
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		`{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["195.175.39.39","212.252.114.8"]}`,
	}
)

// Every country's former recommendations (formerKzDnsResolverSettingsJsons and
// the rest), by lower case country code.
func formerDnsResolverSettingsJsonsByCountry() map[string][]string {
	return map[string][]string{
		"kz": formerKzDnsResolverSettingsJsons,
		"tm": formerTmDnsResolverSettingsJsons,
		"ir": formerIrDnsResolverSettingsJsons,
		"tr": formerTrDnsResolverSettingsJsons,
	}
}

// A hosted proxy's config as proxy_device_config.config_json stores it: a
// country location and the resolver settings json as written.
func storedProxyDeviceConfigJson(t testing.TB, countryCode string, dnsResolverSettingsJson string) string {
	t.Helper()
	locationJson, err := json.Marshal(&sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{LocationId: sdk.NewId()},
		Name:              "Synthetic Country",
		LocationType:      sdk.LocationTypeCountry,
		Country:           "Synthetic Country",
		CountryCode:       countryCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(
		`{"proxy_device_mode":1,"lock_subnets":null,"http_require_auth":false,"initial_device_state":{"location":%s,"performance_profile":null,"dns_resolver_settings":%s}}`,
		locationJson,
		dnsResolverSettingsJson,
	)
}

// The initial device state of a stored config (storedProxyDeviceConfigJson).
func storedProxyDeviceState(t testing.TB, countryCode string, dnsResolverSettingsJson string) *model.ProxyDeviceState {
	t.Helper()
	configJson := storedProxyDeviceConfigJson(t, countryCode, dnsResolverSettingsJson)
	var proxyDeviceConfig model.ProxyDeviceConfig
	if err := json.Unmarshal([]byte(configJson), &proxyDeviceConfig); err != nil {
		t.Fatalf("stored config %s: %s", configJson, err)
	}
	return proxyDeviceConfig.InitialDeviceState
}

// The settings as they store: json, null for none. An empty list stores as an
// absent one.
func storedDnsResolverSettingsJson(t testing.TB, dnsResolverSettings *connect.DnsResolverSettings) string {
	t.Helper()
	dnsResolverSettingsJson, err := json.Marshal(dnsResolverSettings)
	if err != nil {
		t.Fatal(err)
	}
	return string(dnsResolverSettingsJson)
}

// A kz, tm, ir or tr proxy stored with a former recommendation of its country
// builds its tun with the current one, in whatever case the country code is
// stored (connect matched it in lower case): for tm and tr, which have none,
// with connect's defaults. The stored settings do not change.
func TestProxyDeviceDnsResolverSettingsReplaceFormerRegionalRecommendations(t *testing.T) {
	type formerRecommendation struct {
		countryCode             string
		dnsResolverSettingsJson string
	}
	formerRecommendations := []formerRecommendation{}
	for _, c := range []struct {
		dnsResolverSettingsJsons []string
		countryCodes             []string
	}{
		{dnsResolverSettingsJsons: formerKzDnsResolverSettingsJsons, countryCodes: []string{"kz", "KZ"}},
		{dnsResolverSettingsJsons: formerTmDnsResolverSettingsJsons, countryCodes: []string{"tm", "Tm"}},
		{dnsResolverSettingsJsons: formerIrDnsResolverSettingsJsons, countryCodes: []string{"ir", "IR"}},
		{dnsResolverSettingsJsons: formerTrDnsResolverSettingsJsons, countryCodes: []string{"tr", "Tr"}},
	} {
		for _, dnsResolverSettingsJson := range c.dnsResolverSettingsJsons {
			for _, countryCode := range c.countryCodes {
				formerRecommendations = append(formerRecommendations, formerRecommendation{
					countryCode:             countryCode,
					dnsResolverSettingsJson: dnsResolverSettingsJson,
				})
			}
		}
	}

	for _, former := range formerRecommendations {
		initialDeviceState := storedProxyDeviceState(t, former.countryCode, former.dnsResolverSettingsJson)
		storedJson := storedDnsResolverSettingsJson(t, initialDeviceState.DnsResolverSettings)
		currentRecommendation := connect.RegionalDnsResolverSettings(former.countryCode)
		if currentRecommendation == nil && !slices.Contains([]string{"tm", "tr"}, strings.ToLower(former.countryCode)) {
			t.Fatalf("connect has no recommendation for %s", former.countryCode)
		}
		currentJson := storedDnsResolverSettingsJson(t, hostedDnsResolverSettings(currentRecommendation))

		tunJson := storedDnsResolverSettingsJson(t, proxyDeviceDnsResolverSettings(initialDeviceState))
		if tunJson != currentJson {
			t.Errorf(
				"%s proxy stored with %s builds its tun with %s, expected the current recommendation %s",
				former.countryCode,
				storedJson,
				tunJson,
				currentJson,
			)
		}
		if afterJson := storedDnsResolverSettingsJson(t, initialDeviceState.DnsResolverSettings); afterJson != storedJson {
			t.Errorf("%s proxy's stored settings changed from %s to %s", former.countryCode, storedJson, afterJson)
		}
	}
}

// A kz, tm or tr proxy keeps stored settings that are not exactly a former
// recommendation of its country: a list in another order or with another
// server, another toggle, a field no recommendation set, a combination connect
// never shipped, another country's recommendation, and the user's own.
func TestProxyDeviceDnsResolverSettingsKeepUserSettingsInKzAndTm(t *testing.T) {
	type storedSettings struct {
		countryCode             string
		dnsResolverSettingsJson string
	}
	cases := []storedSettings{
		// the 2026-11-08 kz lists in another order
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["9.9.9.9","1.1.1.1"],"remote_dns_ipv6":["2620:fe::fe","2606:4700:4700::1111"]}`,
		},
		// Google's second address added
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.8.8","8.8.4.4"],"remote_dns_ipv6":["2001:4860:4860::8888"]}`,
		},
		// remote DoH turned on
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_doh":true,"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.8.8"],"remote_dns_ipv6":["2001:4860:4860::8888"]}`,
		},
		// remote dns turned off
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.8.8"],"remote_dns_ipv6":["2001:4860:4860::8888"]}`,
		},
		// a pinned DoH host, which no recommendation set
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.8.8"],"remote_dns_ipv6":["2001:4860:4860::8888"],"doh_server_host_addrs":{"doh.example":["192.0.2.53"]}}`,
		},
		// Google's v6 address without the mask address: never shipped
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"remote_dns_ipv4":["8.8.8.8"],"remote_dns_ipv6":["2001:4860:4860::8888"]}`,
		},
		// the local resolver without the local toggle: never shipped
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"remote_dns_ipv4":["8.8.8.8"],"local_dns_ipv4":["1.1.1.1"]}`,
		},
		// tm's recommendation
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: formerTmDnsResolverSettingsJsons[4],
		},
		// both Google addresses, the user's own
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"remote_dns_ipv4":["8.8.8.8","8.8.4.4"],"remote_dns_ipv6":["2001:4860:4860::8888","2001:4860:4860::8844"]}`,
		},
		// a DoH server of the user's own
		{
			countryCode:             "kz",
			dnsResolverSettingsJson: `{"enable_remote_doh":true,"remote_doh_urls_ipv4":["https://192.0.2.53/dns-query"]}`,
		},
		// the tm list in another order
		{
			countryCode:             "tm",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.4.4","217.174.238.141"],"remote_dns_ipv6":["2001:4860:4860::8844"]}`,
		},
		// Turkmentelecom left out
		{
			countryCode:             "tm",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["8.8.4.4"],"remote_dns_ipv6":["2001:4860:4860::8844"]}`,
		},
		// kz's Google v6 address in place of tm's: never shipped
		{
			countryCode:             "tm",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["217.174.238.141","8.8.4.4"],"remote_dns_ipv6":["2001:4860:4860::8888"]}`,
		},
		// the local toggle without the local resolver: never shipped
		{
			countryCode:             "tm",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"enable_local_dns":true,"remote_dns_ipv4":["217.174.238.141","8.8.4.4"]}`,
		},
		// kz's recommendation
		{
			countryCode:             "tm",
			dnsResolverSettingsJson: formerKzDnsResolverSettingsJsons[3],
		},
		// a DoH server of the user's own
		{
			countryCode:             "tm",
			dnsResolverSettingsJson: `{"enable_remote_doh":true,"remote_doh_urls_ipv4":["https://192.0.2.53/dns-query"]}`,
		},
		// the tr list in another order
		{
			countryCode:             "tr",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["212.252.114.8","195.175.39.39"]}`,
		},
		// TTNET alone
		{
			countryCode:             "tr",
			dnsResolverSettingsJson: `{"enable_remote_dns":true,"dns_upgrade_mask_address":"65.49.70.65","remote_dns_ipv4":["195.175.39.39"]}`,
		},
		// ir's recommendation
		{
			countryCode:             "tr",
			dnsResolverSettingsJson: formerIrDnsResolverSettingsJsons[2],
		},
	}
	defaultJson := storedDnsResolverSettingsJson(t, connect.DefaultDnsResolverSettings())
	for _, countryCode := range []string{"kz", "tm", "ir", "tr"} {
		// the default settings, sent as the user's own
		cases = append(cases, storedSettings{
			countryCode:             countryCode,
			dnsResolverSettingsJson: defaultJson,
		})
	}

	for _, c := range cases {
		initialDeviceState := storedProxyDeviceState(t, c.countryCode, c.dnsResolverSettingsJson)
		storedJson := storedDnsResolverSettingsJson(t, initialDeviceState.DnsResolverSettings)
		expectedJson := storedDnsResolverSettingsJson(t, hostedDnsResolverSettings(initialDeviceState.DnsResolverSettings))

		tunJson := storedDnsResolverSettingsJson(t, proxyDeviceDnsResolverSettings(initialDeviceState))
		if tunJson != expectedJson {
			t.Errorf(
				"%s proxy stored with %s builds its tun with %s, expected the stored settings %s",
				c.countryCode,
				storedJson,
				tunJson,
				expectedJson,
			)
		}
	}
}

// A proxy keeps its stored settings when they are another country's former
// recommendation, in a country with former recommendations of its own or
// without, as does a proxy whose location has no country and a proxy with no
// location.
func TestProxyDeviceDnsResolverSettingsKeepOtherCountries(t *testing.T) {
	formerJsonsByCountry := formerDnsResolverSettingsJsonsByCountry()
	formerJsons := slices.Concat(formerKzDnsResolverSettingsJsons, formerTmDnsResolverSettingsJsons, formerIrDnsResolverSettingsJsons, formerTrDnsResolverSettingsJsons)
	for _, countryCode := range []string{"ru", "tr", "cn", "ir", "kz", "tm", "us", ""} {
		for _, formerJson := range formerJsons {
			if slices.Contains(formerJsonsByCountry[countryCode], formerJson) {
				continue
			}
			initialDeviceState := storedProxyDeviceState(t, countryCode, formerJson)
			expectedJson := storedDnsResolverSettingsJson(t, hostedDnsResolverSettings(initialDeviceState.DnsResolverSettings))

			tunJson := storedDnsResolverSettingsJson(t, proxyDeviceDnsResolverSettings(initialDeviceState))
			if tunJson != expectedJson {
				t.Errorf(
					"proxy in %q stored with %s builds its tun with %s, expected the stored settings %s",
					countryCode,
					formerJson,
					tunJson,
					expectedJson,
				)
			}
		}
	}

	for _, formerJson := range formerJsons {
		initialDeviceState := storedProxyDeviceState(t, "kz", formerJson)
		initialDeviceState.Location = nil
		expectedJson := storedDnsResolverSettingsJson(t, hostedDnsResolverSettings(initialDeviceState.DnsResolverSettings))

		tunJson := storedDnsResolverSettingsJson(t, proxyDeviceDnsResolverSettings(initialDeviceState))
		if tunJson != expectedJson {
			t.Errorf("proxy with no location stored with %s builds its tun with %s, expected the stored settings %s", formerJson, tunJson, expectedJson)
		}
	}
}

// Each country's recommendations end with connect's current one. When connect
// changes a recommendation again this fails until the new one is appended, and
// then the proxies stored with the one it replaced move to it too.
func TestCountryCodeRegionalDnsRecommendationsEndWithTheCurrentOne(t *testing.T) {
	for _, countryCode := range []string{"kz", "tm", "ir", "tr"} {
		if _, ok := countryCodeRegionalDnsRecommendations[countryCode]; !ok {
			t.Errorf("no recommendations for %s", countryCode)
		}
	}
	for countryCode, recommendations := range countryCodeRegionalDnsRecommendations {
		if len(recommendations) == 0 {
			t.Errorf("no recommendations for %s", countryCode)
			continue
		}
		newestJson := storedDnsResolverSettingsJson(t, recommendations[len(recommendations)-1])
		currentJson := storedDnsResolverSettingsJson(t, connect.RegionalDnsResolverSettings(countryCode))
		if newestJson != currentJson {
			t.Errorf(
				"the newest %s recommendation is %s, but connect's current one is %s: append it to countryCodeRegionalDnsRecommendations",
				countryCode,
				newestJson,
				currentJson,
			)
		}
	}
}

// A kz, tm, ir or tr proxy stored with the current recommendation keeps it,
// and one stored with no settings keeps none, which builds its tun with
// connect's defaults.
func TestProxyDeviceDnsResolverSettingsKeepCurrentRecommendationAndNone(t *testing.T) {
	for _, countryCode := range []string{"kz", "tm", "ir", "tr"} {
		currentJson := storedDnsResolverSettingsJson(t, connect.RegionalDnsResolverSettings(countryCode))
		initialDeviceState := storedProxyDeviceState(t, countryCode, currentJson)
		expectedJson := storedDnsResolverSettingsJson(t, hostedDnsResolverSettings(initialDeviceState.DnsResolverSettings))
		if tunJson := storedDnsResolverSettingsJson(t, proxyDeviceDnsResolverSettings(initialDeviceState)); tunJson != expectedJson {
			t.Errorf("%s proxy stored with the current recommendation builds its tun with %s, expected %s", countryCode, tunJson, expectedJson)
		}

		initialDeviceState = storedProxyDeviceState(t, countryCode, "null")
		if tunDnsResolverSettings := proxyDeviceDnsResolverSettings(initialDeviceState); tunDnsResolverSettings != nil {
			t.Errorf("%s proxy stored with no settings builds its tun with %+v, expected none", countryCode, tunDnsResolverSettings)
		}
	}
}

// Configs as older servers stored them, read the way the proxy device manager
// reads them (model.GetProxyDeviceConfigForAdmission: postgres until there is
// a redis mirror, then the mirror). A kz or tm device builds its tun with the
// current recommendation in place of a former one, settings that are not a
// former recommendation of the proxy's country read as stored, and the stored
// rows keep what they held.
func TestProxyDeviceDnsResolverSettingsFromStoredConfigs(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		type storedConfig struct {
			proxyId      server.Id
			configJson   string
			expectedJson string
		}
		storedConfigs := []*storedConfig{}
		store := func(countryCode string, dnsResolverSettingsJson string, expectedDnsResolverSettings *connect.DnsResolverSettings) {
			stored := &storedConfig{
				proxyId:      server.NewId(),
				configJson:   storedProxyDeviceConfigJson(t, countryCode, dnsResolverSettingsJson),
				expectedJson: storedDnsResolverSettingsJson(t, hostedDnsResolverSettings(expectedDnsResolverSettings)),
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO proxy_device_config (
						proxy_id,
						client_id,
						instance_id,
						config_json
					)
					VALUES ($1, $2, $3, $4)
					`,
					stored.proxyId,
					server.NewId(),
					server.NewId(),
					stored.configJson,
				))
			})
			storedConfigs = append(storedConfigs, stored)
		}
		asStored := func(dnsResolverSettingsJson string) *connect.DnsResolverSettings {
			return storedProxyDeviceState(t, "", dnsResolverSettingsJson).DnsResolverSettings
		}
		userJson := `{"enable_remote_dns":true,"remote_dns_ipv4":["8.8.8.8","8.8.4.4"]}`
		store("kz", formerKzDnsResolverSettingsJsons[3], connect.RegionalDnsResolverSettings("kz"))
		store("tm", formerTmDnsResolverSettingsJsons[4], connect.RegionalDnsResolverSettings("tm"))
		store("kz", userJson, asStored(userJson))
		store("ru", formerKzDnsResolverSettingsJsons[3], asStored(formerKzDnsResolverSettingsJsons[3]))

		checkTuns := func(source string) {
			for _, stored := range storedConfigs {
				proxyDeviceConfig := model.GetProxyDeviceConfigForAdmission(ctx, stored.proxyId)
				if proxyDeviceConfig == nil || proxyDeviceConfig.InitialDeviceState == nil {
					t.Fatalf("%s: proxy %s read no initial device state", source, stored.proxyId)
				}
				tunJson := storedDnsResolverSettingsJson(t, proxyDeviceDnsResolverSettings(proxyDeviceConfig.InitialDeviceState))
				if tunJson != stored.expectedJson {
					t.Errorf("%s: proxy stored as %s builds its tun with %s, expected %s", source, stored.configJson, tunJson, stored.expectedJson)
				}
			}
		}

		checkTuns("postgres")

		model.MigrateProxyDeviceConfig(ctx, len(storedConfigs))
		for _, stored := range storedConfigs {
			var configJson string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT config_json FROM proxy_device_config WHERE proxy_id = $1`,
					stored.proxyId,
				).Scan(&configJson))
			})
			if configJson != stored.configJson {
				t.Errorf("proxy %s's stored config changed from %s to %s", stored.proxyId, stored.configJson, configJson)
			}
		}

		// with the rows gone, only the mirror answers
		server.Tx(ctx, func(tx server.PgTx) {
			for _, stored := range storedConfigs {
				server.RaisePgResult(tx.Exec(
					ctx,
					`DELETE FROM proxy_device_config WHERE proxy_id = $1`,
					stored.proxyId,
				))
			}
		})
		checkTuns("redis mirror")
	})
}
