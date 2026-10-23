// The §22.0 independently pinned deployment identity for the subnet family
// readers. Discovery starts from the reviewed public Config resource sn.yml,
// not from the producer's runtime configuration or its mirrored feed. The
// pinned values are compared in memory only; findings carry fixed states.
package monitor

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
)

const (
	subnetReplayPinReviewed = "reviewed"
	subnetReplayPinBlocked  = "blocked"
	subnetReplayPinMissing  = "missing"
	subnetReplayPinInvalid  = "invalid"
)

// SubnetReplayPin is the reviewed mainnet deployment identity. Every field is
// public chain configuration, but none is rendered: alerts name only the
// fixed status and ordinal targets.
type SubnetReplayPin struct {
	status               string
	chainId              uint64
	genesisHash          common.Hash
	netuid               uint16
	deploymentId         string
	coordinator          common.Address
	settlementVault      common.Address
	policyHash           common.Hash
	settlementV2Declared bool
}

// Parses sn.yml bytes with the server's strict reviewed-schedule parser. A
// blocked activation keeps only its chain identity; it pins no deployment.
func NewSubnetReplayPin(data []byte) *SubnetReplayPin {
	transition, err := server.ParseProviderPayoutTransition(data)
	if err != nil {
		return &SubnetReplayPin{status: subnetReplayPinInvalid}
	}
	mainnet := transition.Mainnet
	pin := &SubnetReplayPin{
		status:  subnetReplayPinBlocked,
		chainId: mainnet.ChainId,
		netuid:  mainnet.Netuid,
	}
	genesis, ok := subnetReplayHash(mainnet.GenesisHash)
	if !ok {
		return &SubnetReplayPin{status: subnetReplayPinInvalid}
	}
	pin.genesisHash = genesis
	if mainnet.Activation != "reviewed" {
		return pin
	}
	policyHash, ok := subnetReplayHash(mainnet.PolicyHash)
	if !ok || !common.IsHexAddress(mainnet.Coordinator) || !common.IsHexAddress(mainnet.SettlementVault) || mainnet.DeploymentId == "" {
		return &SubnetReplayPin{status: subnetReplayPinInvalid}
	}
	pin.status = subnetReplayPinReviewed
	pin.deploymentId = mainnet.DeploymentId
	pin.coordinator = common.HexToAddress(mainnet.Coordinator)
	pin.settlementVault = common.HexToAddress(mainnet.SettlementVault)
	pin.policyHash = policyHash
	pin.settlementV2Declared = mainnet.SettlementV2Address != "" || mainnet.VaultV2Address != ""
	return pin
}

// Absent sn.yml is distinct from an unreadable or malformed one.
func loadSubnetReplayPin() *SubnetReplayPin {
	resource, err := server.Config.SimpleResource("sn.yml")
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) {
			return &SubnetReplayPin{status: subnetReplayPinMissing}
		}
		return &SubnetReplayPin{status: subnetReplayPinInvalid}
	}
	data, err := resource.BytesE()
	if err != nil {
		return &SubnetReplayPin{status: subnetReplayPinInvalid}
	}
	return NewSubnetReplayPin(data)
}

// Values are immutable after construction; a copy keeps snapshots independent.
func cloneSubnetReplayPin(value *SubnetReplayPin) *SubnetReplayPin {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// The fixed public status of a possibly absent pin.
func (self *SubnetReplayPin) publicStatus() string {
	if self == nil || self.status == "" {
		return subnetReplayPinMissing
	}
	return self.status
}

// Accepts a 0x-prefixed nonzero 32-byte digest in either case.
func subnetReplayHash(value string) (common.Hash, bool) {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if !strings.HasPrefix(value, "0x") || err != nil || len(raw) != 32 {
		return common.Hash{}, false
	}
	hash := common.BytesToHash(raw)
	return hash, hash != (common.Hash{})
}

// One family's deployment decision: observe, an expected absence, or unknown.
type subnetReplayDeployment struct {
	pin         *SubnetReplayPin
	notDeployed bool
	reason      string
}

// A reviewed pin is always observed: custody, claims and history outlive a
// disabled producer. Without a reviewed pin, only an explicit disable is an
// expected absence; every other state is unknown and never green.
func subnetReplayDeploymentOf(env *probeEnv) subnetReplayDeployment {
	pin := env.cfg.subnetReplayPin
	switch pin.publicStatus() {
	case subnetReplayPinReviewed:
		return subnetReplayDeployment{pin: pin}
	case subnetReplayPinMissing, subnetReplayPinBlocked:
		if env.cfg.stConfigStatus.normalized() == STConfigurationExplicitDisabled {
			return subnetReplayDeployment{notDeployed: true}
		}
		return subnetReplayDeployment{reason: "deployment-pin-" + pin.publicStatus()}
	default:
		return subnetReplayDeployment{reason: "deployment-pin-invalid"}
	}
}

// Selects the first inventory archive node configured for the pinned chain.
// Only a full node serves the historical state the replay needs; the genesis
// comparison stays in memory. Selection never contacts the host.
func selectSubnetReplayEndpoint(env *probeEnv, pin *SubnetReplayPin) (*host, int, string) {
	for _, target := range env.cfg.hostsWithRole("subtensor") {
		if target.subtensor == nil {
			continue
		}
		genesis, ok := subnetReplayHash(strings.ToLower(target.subtensor.ExpectedGenesisHash))
		if !ok || genesis != pin.genesisHash {
			continue
		}
		for _, node := range target.subtensor.Nodes {
			if node.SyncMode == "full" && 0 < node.RPCPort && node.RPCPort <= 65535 {
				return target, node.RPCPort, ""
			}
		}
	}
	return nil, 0, "archive-endpoint-unconfigured"
}
