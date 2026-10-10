package monitor

import (
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	stconn "github.com/urnetwork/server/v2026/st"
)

// STConfigurationStatus preserves Main's desired ST state without asking the
// controller's intentionally fail-soft runtime loader to explain a false
// result. The value is safe to render; deployment identities remain separate.
type STConfigurationStatus string

const (
	// mainSTProtocolChainID is Subtensor mainnet's EVM chain namespace.
	mainSTProtocolChainID uint64 = 964

	STConfigurationUnknown          STConfigurationStatus = "unknown"
	STConfigurationUnavailable      STConfigurationStatus = "unavailable"
	STConfigurationExplicitDisabled STConfigurationStatus = "explicit-disabled"
	STConfigurationEnabledInvalid   STConfigurationStatus = "enabled-invalid"
	STConfigurationEnabled          STConfigurationStatus = "enabled"
)

func (s STConfigurationStatus) normalized() STConfigurationStatus {
	switch s {
	case STConfigurationUnavailable,
		STConfigurationExplicitDisabled,
		STConfigurationEnabledInvalid,
		STConfigurationEnabled:
		return s
	default:
		return STConfigurationUnknown
	}
}

// requiresSTCredentials follows source enablement intent rather than a
// fail-soft controller boolean. An enabled but invalid namespace must not hide
// missing signing credentials.
func (s STConfigurationStatus) requiresSTCredentials() bool {
	switch s.normalized() {
	case STConfigurationEnabledInvalid, STConfigurationEnabled:
		return true
	default:
		return false
	}
}

func (s STConfigurationStatus) validate(configuredEnabled bool, deploymentKey string) error {
	switch s {
	case "", STConfigurationUnknown:
		// Preserve callers that predate the status field. Their existing Boolean
		// and optional key combinations remain valid but render as unknown.
		return nil
	case STConfigurationUnavailable, STConfigurationExplicitDisabled:
		if configuredEnabled || deploymentKey != "" {
			return fmt.Errorf("monitor: ST configuration status %s requires enabled=false and no deployment namespace", s)
		}
	case STConfigurationEnabledInvalid:
		if !configuredEnabled || deploymentKey != "" {
			return fmt.Errorf("monitor: ST configuration status %s requires enabled=true and no deployment namespace", s)
		}
	case STConfigurationEnabled:
		if !configuredEnabled || deploymentKey == "" {
			return fmt.Errorf("monitor: ST configuration status %s requires enabled=true and a deployment namespace", s)
		}
	default:
		return fmt.Errorf("monitor: unsupported ST configuration status")
	}
	return nil
}

type stConfigurationObservation struct {
	status            STConfigurationStatus
	configuredEnabled bool
	deploymentKey     string
}

type mainSTVaultStatus struct {
	Enabled            *bool  `yaml:"enabled"`
	ChainID            uint64 `yaml:"chain_id"`
	CoordinatorAddress string `yaml:"coordinator_address"`
}

type mainSTStatusLoader func(*mainSTVaultStatus) error

// inspectMainSTConfiguration reads desired state only. In particular it does
// not consult URNETWORK_ST_PROFILE: the monitor launcher intentionally carries
// only WARP_ENV, while deployed host settings own the runtime profile. A false
// flag is conclusive without validating inactive identity fields.
func inspectMainSTConfiguration(load mainSTStatusLoader) stConfigurationObservation {
	var configured mainSTVaultStatus
	if err := load(&configured); err != nil || configured.Enabled == nil {
		return stConfigurationObservation{status: STConfigurationUnavailable}
	}
	if !*configured.Enabled {
		return stConfigurationObservation{status: STConfigurationExplicitDisabled}
	}

	observation := stConfigurationObservation{
		status: STConfigurationEnabledInvalid, configuredEnabled: true,
	}
	deploymentKey, ok := mainSTDeploymentKey(configured.ChainID, configured.CoordinatorAddress)
	if !ok {
		return observation
	}
	observation.status = STConfigurationEnabled
	observation.deploymentKey = deploymentKey
	return observation
}

// mainSTDeploymentKey mirrors StConfig.DeploymentKey's public namespace shape
// without invoking the controller loader or retaining any other st.yml field.
func mainSTDeploymentKey(chainID uint64, coordinatorAddress string) (string, bool) {
	if chainID != mainSTProtocolChainID || !common.IsHexAddress(coordinatorAddress) {
		return "", false
	}
	address := common.HexToAddress(coordinatorAddress)
	if address == (common.Address{}) {
		return "", false
	}
	return fmt.Sprintf("%d:%s", chainID, strings.ToLower(address.Hex())), true
}

// loadMainSTStatus uses a typed, narrow view. Missing/non-Boolean enabled and
// malformed YAML are errors rather than silently becoming disabled. Unknown
// fields, including identities and keys, are discarded and never reach
// SignalSettings or an alert.
func loadMainSTStatus(status *mainSTVaultStatus) error {
	resource, err := server.Vault.SimpleResource(stconn.VaultResourceName)
	if err != nil {
		return err
	}
	return resource.UnmarshalYamlE(status)
}

func loadSTConfigurationObservation(environment string) stConfigurationObservation {
	if environment == "main" {
		return inspectMainSTConfiguration(loadMainSTStatus)
	}

	// Keep non-Main behavior on the pre-existing controller path. This change
	// neither selects nor interprets testnet desired state.
	runtimeEnabled := controller.StEnabled()
	observation := stConfigurationObservation{
		status: STConfigurationUnknown, configuredEnabled: runtimeEnabled,
	}
	if !runtimeEnabled {
		return observation
	}
	if key, ok := controller.StDeploymentKey(); ok && key != "" {
		observation.status = STConfigurationEnabled
		observation.deploymentKey = string(key)
	} else {
		observation.status = STConfigurationEnabledInvalid
	}
	return observation
}
