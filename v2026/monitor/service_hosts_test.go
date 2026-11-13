package monitor

import (
	"reflect"
	"testing"
)

func TestActiveLogServiceHostsUsesOnlyCurrentPlacement(t *testing.T) {
	services := servicesYaml{Domain: "example.test", Versions: []servicesVersionYaml{
		{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
			"alpha.example.test": {}, "beta.example.test": {}, "disabled.example.test": {}, "unlisted.example.test": {},
		}}, HostServices: map[string][]string{
			"beta.example.test":       {"connect", "proxy"},
			"alpha.example.test":      {"connect", "connect"},
			"disabled.example.test":   {"connect"},
			"not-placed.example.test": {"connect"},
		}, Services: map[string]servicesServiceYaml{
			"connect":    {Blocks: []map[string]int{{"blue": 100}, {"green": 0}}},
			"proxy":      {Blocks: []map[string]int{{"blue": 100}}, Hosts: []string{"beta.example.test"}},
			"taskworker": {Blocks: []map[string]int{{"blue": 100}}, Hosts: []string{"absent.example.test"}},
			"lb":         {},
		}},
		{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{"historical.example.test": {}}}, HostServices: map[string][]string{"historical.example.test": {"connect"}}, Services: map[string]servicesServiceYaml{"connect": {}}},
	}}
	got, err := activeLogServiceHostsFromServices(services)
	want := map[string][]string{"connect": {"alpha", "beta", "disabled", "unlisted"}, "proxy": {"beta"}, "taskworker": {}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("active host placement=%v err=%v", got, err)
	}
	blocks, err := activeLogServiceBlocksFromServices(services)
	if err != nil || !reflect.DeepEqual(blocks["connect"], []string{"blue", "green"}) {
		t.Fatal("zero-weight desired block disappeared")
	}
	for _, invalid := range []servicesYaml{
		{},
		{Versions: []servicesVersionYaml{{}}},
		{Versions: []servicesVersionYaml{{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{"": {}}}, Services: map[string]servicesServiceYaml{"connect": {}}}}},
		{Domain: "example.test", Versions: []servicesVersionYaml{{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{"alpha": {}, "alpha.example.test": {}}}, Services: map[string]servicesServiceYaml{"connect": {}}}}},
	} {
		if _, err := activeLogServiceHostsFromServices(invalid); err == nil {
			t.Error("missing or malformed placement source accepted")
		}
	}
}

func TestSignalSettingsServiceHostsCloneAndExclusionPreserveDesiredInventory(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.LogServices = []string{"connect"}
	settings.LogServiceBlocks = map[string][]string{"connect": {"blue"}}
	settings.LogServiceHosts = map[string][]string{"connect": {"alpha.example.test", "paused.example.test"}}
	settings.Hosts = []HostSettings{{Name: "alpha.example.test"}, {Name: "paused.example.test"}}
	scoped, err := ExcludeHosts(settings, "paused.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := configFromSignalSettings(scoped)
	settings.LogServiceHosts["connect"][0] = "mutated.example.test"
	settings.LogServiceHosts["other"] = []string{"injected.example.test"}
	if !reflect.DeepEqual(cfg.logServiceHosts, map[string][]string{"connect": {"alpha.example.test", "paused.example.test"}}) {
		t.Fatalf("slot inventory alias or exclusion changed desired denominator: %v", cfg.logServiceHosts)
	}
	cfg.logServiceHosts["connect"][1] = "also-mutated.example.test"
	if settings.LogServiceHosts["connect"][1] != "paused.example.test" {
		t.Fatal("runtime inventory mutated caller settings")
	}
}

func TestSignalSettingsRejectsMalformedServiceHosts(t *testing.T) {
	for _, hosts := range []map[string][]string{
		{"unknown": {"alpha.example.test"}},
		{"connect": {""}},
		{"connect": {" alpha.example.test"}},
		{"connect": {"alpha.example.test", "alpha.example.test"}},
	} {
		settings := syntheticSettings(&syntheticSource{})
		settings.LogServices = []string{"connect"}
		settings.LogServiceHosts = hosts
		if err := settings.Validate(); err == nil {
			t.Errorf("invalid service placement accepted: %v", hosts)
		}
	}
}
