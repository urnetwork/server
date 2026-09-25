package monitor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"
	stconn "github.com/urnetwork/server/v2026/st"
)

const syntheticMainSTCoordinator = "0x1111111111111111111111111111111111111111"

func stBool(value bool) *bool { return &value }

func mainSTFixture(configured mainSTVaultStatus) mainSTStatusLoader {
	return func(destination *mainSTVaultStatus) error {
		*destination = configured
		return nil
	}
}

func TestInspectMainSTConfigurationDistinguishesDesiredStates(t *testing.T) {
	t.Run("explicit disabled", func(t *testing.T) {
		observation := inspectMainSTConfiguration(mainSTFixture(mainSTVaultStatus{
			Enabled: stBool(false),
		}))
		if observation.status != STConfigurationExplicitDisabled || observation.configuredEnabled || observation.deploymentKey != "" {
			t.Fatalf("explicit disable classified as status=%s enabled=%t key_present=%t", observation.status, observation.configuredEnabled, observation.deploymentKey != "")
		}
	})

	t.Run("enabled invalid namespace", func(t *testing.T) {
		observation := inspectMainSTConfiguration(mainSTFixture(mainSTVaultStatus{
			Enabled: stBool(true), ChainID: mainSTProtocolChainID, CoordinatorAddress: "not-an-address",
		}))
		if observation.status != STConfigurationEnabledInvalid || !observation.configuredEnabled || observation.deploymentKey != "" {
			t.Fatalf("invalid enabled config classified as status=%s enabled=%t key_present=%t", observation.status, observation.configuredEnabled, observation.deploymentKey != "")
		}
	})

	t.Run("enabled wrong chain", func(t *testing.T) {
		observation := inspectMainSTConfiguration(mainSTFixture(mainSTVaultStatus{
			Enabled: stBool(true), ChainID: mainSTProtocolChainID + 1, CoordinatorAddress: syntheticMainSTCoordinator,
		}))
		if observation.status != STConfigurationEnabledInvalid || !observation.configuredEnabled || observation.deploymentKey != "" {
			t.Fatalf("wrong-chain config classified as status=%s enabled=%t key_present=%t", observation.status, observation.configuredEnabled, observation.deploymentKey != "")
		}
	})

	t.Run("healthy enabled", func(t *testing.T) {
		observation := inspectMainSTConfiguration(mainSTFixture(mainSTVaultStatus{
			Enabled: stBool(true), ChainID: mainSTProtocolChainID, CoordinatorAddress: syntheticMainSTCoordinator,
		}))
		if observation.status != STConfigurationEnabled || !observation.configuredEnabled || observation.deploymentKey == "" {
			t.Fatalf("valid enabled config classified as status=%s enabled=%t key_present=%t", observation.status, observation.configuredEnabled, observation.deploymentKey != "")
		}
	})
}

func TestLoadMainSTConfigurationDoesNotInferOrRequireLocalProfile(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv(stconn.ProfileEnvironment, "")
	pop := server.Vault.PushSimpleResource("st.yml", []byte(fmt.Sprintf(
		"enabled: true\nchain_id: %d\ncoordinator_address: %s\n",
		mainSTProtocolChainID, syntheticMainSTCoordinator,
	)))
	defer pop()

	observation := loadSTConfigurationObservation("main")
	if observation.status != STConfigurationEnabled || !observation.configuredEnabled || observation.deploymentKey == "" {
		t.Fatalf("missing watcher profile changed Main desired state: status=%s enabled=%t key_present=%t", observation.status, observation.configuredEnabled, observation.deploymentKey != "")
	}
}

func TestInspectMainSTConfigurationKeepsUnavailableDistinctFromDisabled(t *testing.T) {
	for name, load := range map[string]mainSTStatusLoader{
		"load error":      func(*mainSTVaultStatus) error { return errors.New("synthetic load failure") },
		"missing enabled": mainSTFixture(mainSTVaultStatus{}),
	} {
		t.Run(name, func(t *testing.T) {
			observation := inspectMainSTConfiguration(load)
			if observation.status != STConfigurationUnavailable || observation.configuredEnabled || observation.deploymentKey != "" {
				t.Fatalf("unavailable config classified as status=%s enabled=%t key_present=%t", observation.status, observation.configuredEnabled, observation.deploymentKey != "")
			}
		})
	}
}

func TestLoadMainSTStatusRejectsMissingOrNonBooleanEnabled(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())

	for name, source := range map[string]string{
		"missing": fmt.Sprintf(
			"chain_id: %d\ncoordinator_address: %s\n",
			mainSTProtocolChainID, syntheticMainSTCoordinator,
		),
		"non-boolean": fmt.Sprintf(
			"enabled: definitely\nchain_id: %d\ncoordinator_address: %s\n",
			mainSTProtocolChainID, syntheticMainSTCoordinator,
		),
		"malformed": "enabled: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			pop := server.Vault.PushSimpleResource("st.yml", []byte(source))
			defer pop()

			observation := inspectMainSTConfiguration(loadMainSTStatus)
			if observation.status != STConfigurationUnavailable {
				t.Fatalf("invalid enabled source classified as %s", observation.status)
			}
		})
	}
}

func TestSTConfigurationStatusArmsCredentialsFromEnabledIntent(t *testing.T) {
	for status, expected := range map[STConfigurationStatus]bool{
		STConfigurationUnknown:          false,
		STConfigurationUnavailable:      false,
		STConfigurationExplicitDisabled: false,
		STConfigurationEnabledInvalid:   true,
		STConfigurationEnabled:          true,
	} {
		if actual := status.requiresSTCredentials(); actual != expected {
			t.Fatalf("status %s requires credentials=%t, want %t", status, actual, expected)
		}
	}
}

func TestSignalSettingsValidateSTConfigurationStatusInvariants(t *testing.T) {
	syntheticNamespace, ok := mainSTDeploymentKey(mainSTProtocolChainID, syntheticMainSTCoordinator)
	if !ok {
		t.Fatal("synthetic Main ST namespace did not validate")
	}
	tests := []struct {
		name    string
		status  STConfigurationStatus
		enabled bool
		key     string
		valid   bool
	}{
		{name: "legacy zero", valid: true},
		{name: "legacy unknown values", status: STConfigurationUnknown, enabled: true, key: syntheticNamespace, valid: true},
		{name: "unavailable", status: STConfigurationUnavailable, valid: true},
		{name: "explicit disabled", status: STConfigurationExplicitDisabled, valid: true},
		{name: "enabled invalid", status: STConfigurationEnabledInvalid, enabled: true, valid: true},
		{name: "enabled", status: STConfigurationEnabled, enabled: true, key: syntheticNamespace, valid: true},
		{name: "disabled marked enabled", status: STConfigurationExplicitDisabled, enabled: true},
		{name: "unavailable with namespace", status: STConfigurationUnavailable, key: syntheticNamespace},
		{name: "enabled invalid marked disabled", status: STConfigurationEnabledInvalid},
		{name: "enabled invalid with namespace", status: STConfigurationEnabledInvalid, enabled: true, key: syntheticNamespace},
		{name: "enabled without namespace", status: STConfigurationEnabled, enabled: true},
		{name: "unsupported", status: STConfigurationStatus("synthetic-unsupported")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := syntheticSettings(&syntheticSource{})
			settings.STConfigStatus = test.status
			settings.VerificationEnabled = test.enabled
			settings.STDeploymentKey = test.key
			err := settings.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid status tuple rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid status tuple accepted")
			}
		})
	}
}

func TestEnabledInvalidMainSTConfigurationDoesNotSuppressSubnetRequirements(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())

	hasSubnetRequirements := func(status STConfigurationStatus) bool {
		requirements := loadCredentialRequirements("main", status.requiresSTCredentials(), nil)
		foundSubnet, foundVerification := false, false
		for _, requirement := range requirements {
			switch requirement.Key {
			case "subnet":
				foundSubnet = true
			case "subnet-verification":
				foundVerification = true
			}
		}
		return foundSubnet && foundVerification
	}

	if !hasSubnetRequirements(STConfigurationEnabledInvalid) {
		t.Fatal("enabled-invalid Main ST configuration suppressed subnet credential requirements")
	}
	if hasSubnetRequirements(STConfigurationExplicitDisabled) {
		t.Fatal("explicitly disabled Main ST configuration armed subnet credential requirements")
	}
}
