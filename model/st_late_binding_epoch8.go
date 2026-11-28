package model

// The epoch 8 late-binding window (sn protocol/fleet_late_binding_epoch8.go)
// on the mirrored bindings: a binding signed from epoch 9 whose FleetBound
// event is in blocks 9,600,028 through 9,615,060 of the SN25 mainnet
// coordinator also counts for epoch 8. The mirrors keep each event's block, so
// they apply it exactly.

import (
	"encoding/hex"
	"strconv"
	"strings"
)

// stDeploymentIdentity parses a deployment key, "<chain id>:<coordinator>"
// (controller StConfig.DeploymentKey).
func stDeploymentIdentity(deploymentKey StDeploymentKey) (chainId uint64, coordinator [20]byte, ok bool) {
	chain, address, found := strings.Cut(string(deploymentKey), ":")
	if !found {
		return 0, [20]byte{}, false
	}
	id, err := strconv.ParseUint(chain, 10, 64)
	if err != nil {
		return 0, [20]byte{}, false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(address, "0x"))
	if err != nil || len(raw) != 20 || !strings.HasPrefix(address, "0x") {
		return 0, [20]byte{}, false
	}
	return id, [20]byte(raw), true
}
