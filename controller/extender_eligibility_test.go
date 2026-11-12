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

// Makes the canary networks read networkIds until the test ends.
func setTestStExtenderCanaryNetworks(t *testing.T, networkIds ...server.Id) {
	t.Helper()
	previous := stExtenderCanaryNetworkIdsRead
	stExtenderCanaryNetworkIdsRead = func(context.Context) []server.Id { return networkIds }
	t.Cleanup(func() { stExtenderCanaryNetworkIdsRead = previous })
}

// Makes ctf.yml declare networkIds, with its census off, until the test ends.
func setTestCtfOwnNetworkIds(t *testing.T, networkIds ...string) {
	t.Helper()
	// census off: its own networks still belong to the one list
	model.Testing_SetCtfDataSettings(&model.CtfDataConfig{Census: model.CtfCensusConfig{OwnNetworkIds: networkIds}})
	t.Cleanup(func() { model.Testing_SetCtfDataSettings(nil) })
}

func TestStExtenderOwnNetworksAreOneList(t *testing.T) {
	durableNetworkId, declaredNetworkId, ctfOwnNetworkId, canaryNetworkId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	installTestExtenderConfigYaml(t, "directory:\n  durable_network_ids:\n    - "+durableNetworkId.String()+"\n")
	setTestCtfOwnNetworkIds(t, " "+ctfOwnNetworkId.String()+" ")
	setTestStExtenderCanaryNetworks(t, canaryNetworkId)
	cfg := stAmendmentTestConfig(t, "enabled: false\nextender_pool:\n  declared_network_ids:\n    - "+declaredNetworkId.String()+"\n")
	ownNetworks, err := readStExtenderOwnNetworks(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownNetworks.DurableNetworkIds) != 1 || !ownNetworks.DurableNetworkIds[durableNetworkId] ||
		len(ownNetworks.DeclaredNetworkIds) != 2 || !ownNetworks.DeclaredNetworkIds[declaredNetworkId] || !ownNetworks.DeclaredNetworkIds[ctfOwnNetworkId] ||
		len(ownNetworks.CanaryNetworkIds) != 1 || !ownNetworks.CanaryNetworkIds[canaryNetworkId] {
		t.Fatalf("own networks %+v", ownNetworks)
	}
	allNetworkIds := ownNetworks.AllNetworkIds()
	if len(allNetworkIds) != 4 || !allNetworkIds[durableNetworkId] || !allNetworkIds[declaredNetworkId] || !allNetworkIds[ctfOwnNetworkId] || !allNetworkIds[canaryNetworkId] {
		t.Fatalf("all own networks %v", allNetworkIds)
	}
}

// Fail closed: a list that cannot be read, or an id in one that does not
// parse, stops the census rather than let an operator's own network compete.
func TestStExtenderOwnNetworksFailClosed(t *testing.T) {
	setTestStExtenderCanaryNetworks(t)
	cfg := stAmendmentTestConfig(t, "enabled: false\n")
	for _, c := range []struct {
		name             string
		extender         string
		ctfOwnNetworkIds []string
		cfg              *StConfig
	}{
		{"a durable network that does not parse", "directory:\n  durable_network_ids:\n    - fleet\n", nil, cfg},
		{"an unreadable extender.yml", "directory: [\n", nil, cfg},
		{"a ctf.yml own network that does not parse", "directory: {}\n", []string{"own"}, cfg},
		{"a declared network that does not parse", "directory: {}\n", nil, stAmendmentTestConfig(t, "enabled: false\nextender_pool:\n  declared_network_ids:\n    - ours\n")},
	} {
		installTestExtenderConfigYaml(t, c.extender)
		setTestCtfOwnNetworkIds(t, c.ctfOwnNetworkIds...)
		if ownNetworks, err := readStExtenderOwnNetworks(t.Context(), c.cfg); err == nil {
			t.Fatalf("%s: read %+v", c.name, ownNetworks)
		}
	}
	installTestExtenderConfigYaml(t, "directory: {}\n")
	setTestCtfOwnNetworkIds(t)
	if ownNetworks, err := readStExtenderOwnNetworks(t.Context(), cfg); err != nil || len(ownNetworks.AllNetworkIds()) != 0 {
		t.Fatalf("no own networks: %+v, %v", ownNetworks, err)
	}
}
