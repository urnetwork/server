package controller

import (
	"errors"
	"fmt"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The extender activation budgets an operator sets in its config
// (ExtenderActivateSettings, connect#230): the config resource `extender.yml`,
// which the config repository keeps as config/<env>/extender.yml. It is not
// the vault resource of the same name, which holds the extender network's keys
// (extender_config.go). Its complete key set is:
//
//	activation:
//	  address:
//	    ipv4:
//	      prefix_bit_count: 32
//	      rate_limit: 6
//	      proven_rate_limit: 6
//	    ipv6:
//	      prefix_bit_count: 64
//	      rate_limit: 60
//	      proven_rate_limit: 6
//
// The address block is the budget of the caller's address on each family:
// rate_limit is AddressIpv4RateLimit or AddressIpv6RateLimit,
// proven_rate_limit is ProvenAddressIpv4RateLimit or ProvenAddressIpv6RateLimit,
// and prefix_bit_count is AddressIpv4PrefixBitCount or
// AddressIpv6PrefixBitCount, the /32 or /64 an address is. Every budget counts
// its activations over RateLimitWindow, an hour, which is not configured here.
//
// The resource is optional, and so is each of its keys. A key that is present
// and in range sets its setting, and every other setting is the default
// (DefaultExtenderActivateSettings): 6 an hour per /32 and per /64, and 6 for
// a proven key. A limit is in range when it is positive, so a mistake never
// refuses every activation of a family, and a prefix when it is no wider than
// the block the account's proven hosts are counted in
// (ProvenHostBlockIpv4PrefixBitCount, ProvenHostBlockIpv6PrefixBitCount) and
// no longer than the family's addresses, so a mistake never puts every caller
// in one budget. A key out of range keeps its default, and a resource that
// cannot be read or parsed keeps them all; each is logged. A key this server
// does not read is ignored, so a config written for a later server still sets
// the keys this one reads.
//
// The api, and alt, which serves the api's routes, read it once per process,
// at the process's first activation (defaultExtenderActivateService), and log
// the budgets they read. A container mounts one config version, so a change
// applies when the process restarts on a config version that has it.

// The config resource of the activation budgets, config/<env>/extender.yml.
const extenderActivateConfigResourceName = "extender.yml"

// The activation budgets as the config resource writes them. An absent key is
// nil.
type extenderActivateConfigYaml struct {
	Activation struct {
		Address struct {
			Ipv4 extenderActivateAddressConfigYaml `yaml:"ipv4"`
			Ipv6 extenderActivateAddressConfigYaml `yaml:"ipv6"`
		} `yaml:"address"`
	} `yaml:"activation"`
}

// The budget of the caller's address on one family.
type extenderActivateAddressConfigYaml struct {
	PrefixBitCount  *int `yaml:"prefix_bit_count"`
	RateLimit       *int `yaml:"rate_limit"`
	ProvenRateLimit *int `yaml:"proven_rate_limit"`
}

// EnvExtenderActivateSettings is DefaultExtenderActivateSettings with what the
// operator's config sets over them (`extender.yml`), read now.
func EnvExtenderActivateSettings() *ExtenderActivateSettings {
	return extenderActivateSettingsFromConfig(server.Config.SimpleResource(extenderActivateConfigResourceName))
}

// DefaultExtenderActivateSettings with what one lookup of the config resource
// sets over them: the resource, or the error that it could not be found or
// read.
func extenderActivateSettingsFromConfig(resource *server.SimpleResource, err error) *ExtenderActivateSettings {
	settings := DefaultExtenderActivateSettings()
	if errors.Is(err, server.ErrResourceNotFound) && !errors.Is(err, server.ErrResourceUnavailable) {
		glog.Infof(
			"[extender]the config has no %s, so the activation address budgets are the defaults: %s\n",
			extenderActivateConfigResourceName,
			settings.addressBudgetsText(),
		)
		return settings
	}
	if err != nil {
		glog.Errorf(
			"[extender]%s could not be read, so the activation address budgets are the defaults: %s. err = %s\n",
			extenderActivateConfigResourceName,
			settings.addressBudgetsText(),
			err,
		)
		return settings
	}
	var config extenderActivateConfigYaml
	if err := resource.UnmarshalYamlE(&config); err != nil {
		glog.Errorf(
			"[extender]%s could not be parsed, so the activation address budgets are the defaults: %s. err = %s\n",
			extenderActivateConfigResourceName,
			settings.addressBudgetsText(),
			err,
		)
		return settings
	}
	for _, problem := range config.apply(settings) {
		glog.Errorf("[extender]%s: %s\n", extenderActivateConfigResourceName, problem)
	}
	glog.Infof(
		"[extender]the activation address budgets from %s: %s\n",
		extenderActivateConfigResourceName,
		settings.addressBudgetsText(),
	)
	return settings
}

// Sets each key of the config that is present and in range over `settings`,
// and returns the words of each key that is present and out of range, which
// leaves its setting as it was.
func (self *extenderActivateConfigYaml) apply(settings *ExtenderActivateSettings) (problems []string) {
	for _, family := range []struct {
		name string
		yaml *extenderActivateAddressConfigYaml
		// the prefixes in range: at least the block's, and at most the
		// family's address length
		minPrefixBitCount int
		maxPrefixBitCount int
		prefixBitCount    *int
		rateLimit         *int
		provenRateLimit   *int
	}{
		{
			name:              "ipv4",
			yaml:              &self.Activation.Address.Ipv4,
			minPrefixBitCount: max(1, settings.ProvenHostBlockIpv4PrefixBitCount),
			maxPrefixBitCount: 32,
			prefixBitCount:    &settings.AddressIpv4PrefixBitCount,
			rateLimit:         &settings.AddressIpv4RateLimit,
			provenRateLimit:   &settings.ProvenAddressIpv4RateLimit,
		},
		{
			name:              "ipv6",
			yaml:              &self.Activation.Address.Ipv6,
			minPrefixBitCount: max(1, settings.ProvenHostBlockIpv6PrefixBitCount),
			maxPrefixBitCount: 128,
			prefixBitCount:    &settings.AddressIpv6PrefixBitCount,
			rateLimit:         &settings.AddressIpv6RateLimit,
			provenRateLimit:   &settings.ProvenAddressIpv6RateLimit,
		},
	} {
		if value := family.yaml.PrefixBitCount; value != nil {
			if family.minPrefixBitCount <= *value && *value <= family.maxPrefixBitCount {
				*family.prefixBitCount = *value
			} else {
				problems = append(problems, fmt.Sprintf(
					"activation.address.%s.prefix_bit_count %d is not %d-%d, so it stays %d",
					family.name,
					*value,
					family.minPrefixBitCount,
					family.maxPrefixBitCount,
					*family.prefixBitCount,
				))
			}
		}
		for _, limit := range []struct {
			key     string
			value   *int
			setting *int
		}{
			{key: "rate_limit", value: family.yaml.RateLimit, setting: family.rateLimit},
			{key: "proven_rate_limit", value: family.yaml.ProvenRateLimit, setting: family.provenRateLimit},
		} {
			if limit.value == nil {
				continue
			}
			if 0 < *limit.value {
				*limit.setting = *limit.value
			} else {
				problems = append(problems, fmt.Sprintf(
					"activation.address.%s.%s %d is not positive, so it stays %d",
					family.name,
					limit.key,
					*limit.value,
					*limit.setting,
				))
			}
		}
	}
	return problems
}

// The budgets of a caller's address in words, as a process logs the ones it
// read.
func (self *ExtenderActivateSettings) addressBudgetsText() string {
	return fmt.Sprintf(
		"ipv4 %d per /%d and %d for a proven key, ipv6 %d per /%d and %d for a proven key, each per %s",
		self.AddressIpv4RateLimit,
		self.AddressIpv4PrefixBitCount,
		self.ProvenAddressIpv4RateLimit,
		self.AddressIpv6RateLimit,
		self.AddressIpv6PrefixBitCount,
		self.ProvenAddressIpv6RateLimit,
		self.RateLimitWindow,
	)
}
