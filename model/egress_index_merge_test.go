// Provider configuration retains yaml merge precedence without letting one
// duplicate or malformed setting erase independent valid settings.
package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// Aliases, direct mappings and merge sequences carry inherited kill switches
// and complete egress-index blocks through the actual resource reader.
func TestEgressIndexSettingsYamlMerges(t *testing.T) {
	for _, config := range []string{
		"defaults: &defaults\n  online_requires_url_success: false\n  online_success_country_floor: 0.5\n  online_success_country_floor_margin: 0.07\n  egress_index:\n    max_failure_index: 9\n<<: *defaults\n",
		"<<: {online_requires_url_success: false, online_success_country_floor: 0.5, online_success_country_floor_margin: 0.07, egress_index: {max_failure_index: 9}}\n",
		"base: &base\n  online_requires_url_success: false\n  egress_index: {max_failure_index: 9}\nfirst: &first\n  <<: *base\n  online_success_country_floor: 0.5\nsecond: &second\n  online_requires_url_success: true\n  online_success_country_floor: 0.9\n  online_success_country_floor_margin: 0.07\n<<: [*first, *second]\n",
	} {
		func() {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			defer pop()
			settings, failure := decodeEgressIndexSettings()
			if settings.OnlineRequiresUrlSuccess || settings.OnlineSuccessCountryFloor != 0.5 || settings.OnlineSuccessCountryFloorMargin != 0.07 || settings.MaxFailureIndex != 9 || failure != "" {
				t.Errorf("merged settings lost inherited values for %q: %+v, failure %q", config, settings, failure)
			}
		}()
	}
}

// An explicit key takes precedence regardless of the merge key's position.
// A quoted << is an ordinary key and must not introduce inherited settings.
func TestEgressIndexSettingsYamlMergeExplicitOverrides(t *testing.T) {
	for _, config := range []string{
		"<<: {online_requires_url_success: false, online_success_country_floor: 0.9, egress_index: {max_failure_index: 9}}\nonline_requires_url_success: true\nonline_success_country_floor: 0.5\negress_index: {max_failure_index: 8}\n",
		"online_requires_url_success: true\nonline_success_country_floor: 0.5\negress_index: {max_failure_index: 8}\n<<: {online_requires_url_success: false, online_success_country_floor: 0.9, egress_index: {max_failure_index: 9}}\n",
		"'<<': {online_requires_url_success: false, online_success_country_floor: 0.9}\nonline_requires_url_success: true\nonline_success_country_floor: 0.5\negress_index: {max_failure_index: 8}\n",
	} {
		func() {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			defer pop()
			settings := egressIndexSettings()
			if !settings.OnlineRequiresUrlSuccess || settings.OnlineSuccessCountryFloor != 0.5 || settings.MaxFailureIndex != 8 {
				t.Errorf("explicit settings lost precedence for %q: %+v", config, settings)
			}
		}()
	}
}

// Duplicate explicit keys remain unreadable at their effective source, while
// other inherited keys stay usable. An explicit override replaces that source.
func TestEgressIndexSettingsYamlMergeDuplicateKeys(t *testing.T) {
	for _, c := range []struct {
		config  string
		enabled bool
	}{
		{config: "<<: {online_success_country_floor: 0.5, egress_index: {max_failure_index: 9}}\nonline_requires_url_success: true\nonline_requires_url_success: true\n"},
		{config: "<<:\n  online_requires_url_success: true\n  online_requires_url_success: true\n  online_success_country_floor: 0.5\n  egress_index: {max_failure_index: 9}\n"},
		{config: "<<:\n  online_requires_url_success: false\n  online_requires_url_success: false\n  online_success_country_floor: 0.5\n  egress_index: {max_failure_index: 9}\nonline_requires_url_success: true\n", enabled: true},
	} {
		func() {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
			defer pop()
			settings := egressIndexSettings()
			if settings.OnlineRequiresUrlSuccess != c.enabled || settings.OnlineSuccessCountryFloor != 0.5 || settings.MaxFailureIndex != 9 {
				t.Errorf("duplicate merge settings for %q: %+v", c.config, settings)
			}
		}()
	}
}

// Malformed or cyclic merges fail closed like an unreadable configuration;
// they cannot leave the switches enabled or recurse indefinitely.
func TestEgressIndexSettingsYamlMergeUnreadable(t *testing.T) {
	for _, config := range []string{
		"<<: 7\n",
		"<<: [7]\n",
		"<<: {online_requires_url_success: true}\n<<: {online_requires_url_success: false}\n",
		"cycle: &cycle\n  <<: *cycle\n<<: *cycle\n",
	} {
		func() {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			defer pop()
			settings, failure := decodeEgressIndexSettings()
			if settings.OnlineRequiresUrlSuccess || settings.QualitySpeedRequiresModernProvider == nil || *settings.QualitySpeedRequiresModernProvider || failure == "" {
				t.Errorf("unreadable merge left a gate enabled for %q: %+v, failure %q", config, settings, failure)
			}
		}()
	}
}
