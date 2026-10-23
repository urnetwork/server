// SIGNALS.md §22.0 public deployment identity for the subnet correctness
// readers. The reviewed config sn.yml is the independent pin for chain,
// genesis, netuid, deployment, coordinator, settlement vault and policy hash;
// its values are compared in memory and never rendered.
package monitor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
)

const (
	subnetDeploymentAvailable = "available"
	subnetDeploymentMissing   = "missing"
	subnetDeploymentInvalid   = "invalid"
	subnetDeploymentBlocked   = "blocked"
)

// Status is fixed public text. The identity fields are private: they are
// public chain identities, but alerts carry ordinals and states only.
type SubnetDeploymentIdentity struct {
	Status          string
	ChainId         uint64
	GenesisHash     string
	Netuid          uint16
	DeploymentId    string
	Coordinator     string
	SettlementVault string
	PolicyHash      string
}

// A fixed reason for a deployment that cannot pin the readers.
func (self SubnetDeploymentIdentity) validate() string {
	switch self.Status {
	case subnetDeploymentAvailable:
	case subnetDeploymentBlocked:
		return "deployment-identity-blocked"
	case subnetDeploymentInvalid:
		return "deployment-identity-invalid"
	default:
		return "deployment-identity-missing"
	}
	if self.ChainId == 0 || self.Netuid == 0 || self.DeploymentId == "" || !subnetCorrectnessHex(self.GenesisHash, 32) || !subnetArtifactAddress(self.Coordinator) || !subnetArtifactAddress(self.SettlementVault) || !subnetCorrectnessHex(self.PolicyHash, 32) {
		return "deployment-identity-invalid"
	}
	return ""
}

// The coordinator and vault as typed addresses; callers validate first.
func (self SubnetDeploymentIdentity) coordinatorAddress() common.Address {
	return common.HexToAddress(self.Coordinator)
}

func (self SubnetDeploymentIdentity) settlementVaultAddress() common.Address {
	return common.HexToAddress(self.SettlementVault)
}

// The namespace the St mirror reports, compared in memory with st.yml's.
func (self SubnetDeploymentIdentity) deploymentKey() string {
	return fmt.Sprintf("%d:%s", self.ChainId, self.Coordinator)
}

// Reads config sn.yml through the strict payout-transition parser. A blocked
// activation has no reviewed deployment identity to pin.
func loadSubnetDeploymentIdentity() SubnetDeploymentIdentity {
	resource, err := server.Config.SimpleResource("sn.yml")
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) {
			return SubnetDeploymentIdentity{Status: subnetDeploymentMissing}
		}
		return SubnetDeploymentIdentity{Status: subnetDeploymentInvalid}
	}
	data, err := resource.BytesE()
	if err != nil {
		return SubnetDeploymentIdentity{Status: subnetDeploymentInvalid}
	}
	return subnetDeploymentIdentityFromBytes(data)
}

// Parse errors are discarded: they may quote configuration bytes.
func subnetDeploymentIdentityFromBytes(data []byte) SubnetDeploymentIdentity {
	transition, err := server.ParseProviderPayoutTransition(data)
	if err != nil || transition == nil {
		return SubnetDeploymentIdentity{Status: subnetDeploymentInvalid}
	}
	mainnet := transition.Mainnet
	if mainnet.Activation != "reviewed" {
		return SubnetDeploymentIdentity{Status: subnetDeploymentBlocked}
	}
	return SubnetDeploymentIdentity{
		Status:          subnetDeploymentAvailable,
		ChainId:         mainnet.ChainId,
		GenesisHash:     strings.ToLower(mainnet.GenesisHash),
		Netuid:          mainnet.Netuid,
		DeploymentId:    mainnet.DeploymentId,
		Coordinator:     strings.ToLower(mainnet.Coordinator),
		SettlementVault: strings.ToLower(mainnet.SettlementVault),
		PolicyHash:      strings.ToLower(mainnet.PolicyHash),
	}
}
