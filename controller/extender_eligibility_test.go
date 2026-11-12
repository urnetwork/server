package controller

// The operator's own networks as both censuses read them
// (extender_eligibility.go): one list from extender.yml's durable fleet,
// st.yml's and ctf.yml's declared networks alike, and the networks of its
// canaries; and none of them is ever dropped for not parsing.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func setTestStExtenderCanaryNetworks(t *testing.T, networkIds ...server.Id) {
	t.Helper()
	previous := stExtenderCanaryNetworkIdsRead
	stExtenderCanaryNetworkIdsRead = func(context.Context) []server.Id { return networkIds }
	t.Cleanup(func() { stExtenderCanaryNetworkIdsRead = previous })
}

func setTestCtfOwnNetworkIds(t *testing.T, networkIds ...string) {
	t.Helper()
	// census off: its own networks still belong to the one list
	model.Testing_SetCtfDataSettings(&model.CtfDataConfig{Census: model.CtfCensusConfig{OwnNetworkIds: networkIds}})
	t.Cleanup(func() { model.Testing_SetCtfDataSettings(nil) })
}

func TestStExtenderOwnNetworksAreOneList(t *testing.T) {
	durable, declared, ctfOwn, canary := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	installTestExtenderConfigYaml(t, "directory:\n  durable_network_ids:\n    - "+durable.String()+"\n")
	setTestCtfOwnNetworkIds(t, " "+ctfOwn.String()+" ")
	setTestStExtenderCanaryNetworks(t, canary)
	cfg := stAmendmentTestConfig(t, "enabled: false\nextender_pool:\n  declared_network_ids:\n    - "+declared.String()+"\n")
	own, err := readStExtenderOwnNetworks(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Durable) != 1 || !own.Durable[durable] || len(own.Declared) != 2 || !own.Declared[declared] || !own.Declared[ctfOwn] || len(own.Canary) != 1 || !own.Canary[canary] {
		t.Fatalf("own networks %+v", own)
	}
	all := own.All()
	if len(all) != 4 || !all[durable] || !all[declared] || !all[ctfOwn] || !all[canary] {
		t.Fatalf("all own networks %v", all)
	}
}

// Fail closed: a list that cannot be read, or an id in one that does not
// parse, stops the census rather than let an operator's own network compete.
func TestStExtenderOwnNetworksFailClosed(t *testing.T) {
	setTestStExtenderCanaryNetworks(t)
	cfg := stAmendmentTestConfig(t, "enabled: false\n")
	for _, c := range []struct {
		name      string
		extender  string
		ctfOwnIds []string
		cfg       *StConfig
	}{
		{"a durable network that does not parse", "directory:\n  durable_network_ids:\n    - fleet\n", nil, cfg},
		{"an unreadable extender.yml", "directory: [\n", nil, cfg},
		{"a ctf.yml own network that does not parse", "directory: {}\n", []string{"own"}, cfg},
		{"a declared network that does not parse", "directory: {}\n", nil, stAmendmentTestConfig(t, "enabled: false\nextender_pool:\n  declared_network_ids:\n    - ours\n")},
	} {
		installTestExtenderConfigYaml(t, c.extender)
		setTestCtfOwnNetworkIds(t, c.ctfOwnIds...)
		if own, err := readStExtenderOwnNetworks(t.Context(), c.cfg); err == nil {
			t.Fatalf("%s: read %+v", c.name, own)
		}
	}
	installTestExtenderConfigYaml(t, "directory: {}\n")
	setTestCtfOwnNetworkIds(t)
	if own, err := readStExtenderOwnNetworks(t.Context(), cfg); err != nil || len(own.All()) != 0 {
		t.Fatalf("no own networks: %+v, %v", own, err)
	}
}
