// A bounded current deadline read for v1 and v2. Unlike the settlement replay,
// its work does not grow with history and needs no producer or API enrollment.
package monitor

import (
	"context"
	"math"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/stabi"
)

type subnetRootDeadlineV2Contracts struct{ settlement, vault *abi.ABI }

var subnetRootDeadlineV2Abis = sync.OnceValues(func() (subnetRootDeadlineV2Contracts, error) {
	settlement, err := stabi.STSettlement2MetaData.ParseABI()
	if err != nil {
		return subnetRootDeadlineV2Contracts{}, err
	}
	vault, err := stabi.STSettlementVault2MetaData.ParseABI()
	return subnetRootDeadlineV2Contracts{settlement: settlement, vault: vault}, err
})

type subnetRootDeadlineMissing struct {
	noId    uint64
	version string
}
type subnetRootDeadlineState struct {
	checkpoint      subnetReplayEvmCheckpoint
	epoch, deadline uint64
	missing         []subnetRootDeadlineMissing
}

// All block and operator coordinates are checked before narrowing uint256.
func subnetRootDeadlineUint(view *subnetReplayView) (uint64, error) {
	value, err := subnetReplayValue[*big.Int](view, 0)
	if err != nil {
		return 0, err
	}
	if value == nil || !value.IsUint64() {
		return 0, newSubnetReplayUnknown("view-shape-invalid", nil)
	}
	return value.Uint64(), nil
}

func readSubnetRootDeadline(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin, settings *SubnetReplaySettings) (*subnetRootDeadlineState, error) {
	v1, err := subnetSettlementAbis()
	if err != nil {
		return nil, newSubnetReplayUnknown("binding-unavailable", err)
	}
	v2, err := subnetRootDeadlineV2Abis()
	if err != nil {
		return nil, newSubnetReplayUnknown("binding-unavailable", err)
	}
	checkpoint, err := readSubnetReplayEvmCheckpoint(ctx, rpc, pin)
	if err != nil {
		return nil, err
	}
	state := &subnetRootDeadlineState{checkpoint: checkpoint}
	current := newSubnetReplayView(v1.coordinator, pin.coordinator, "currentEpoch")
	count := newSubnetReplayView(v1.coordinator, pin.coordinator, "operatorCount")
	type addressLink struct {
		view *subnetReplayView
		want common.Address
	}
	links := []addressLink{
		{newSubnetReplayView(v1.coordinator, pin.coordinator, "settlementVault"), pin.settlementVault},
		{newSubnetReplayView(v1.vault, pin.settlementVault, "coordinator"), pin.coordinator},
	}
	netuids := []*subnetReplayView{
		newSubnetReplayView(v1.coordinator, pin.coordinator, "netuid"),
		newSubnetReplayView(v1.vault, pin.settlementVault, "netuid"),
	}
	if pin.settlementV2Declared {
		links = append(links,
			addressLink{newSubnetReplayView(v2.settlement, pin.settlementV2, "v1Coordinator"), pin.coordinator},
			addressLink{newSubnetReplayView(v2.settlement, pin.settlementV2, "vault"), pin.settlementVaultV2},
			addressLink{newSubnetReplayView(v2.vault, pin.settlementVaultV2, "coordinator"), pin.settlementV2})
		netuids = append(netuids, newSubnetReplayView(v2.vault, pin.settlementVaultV2, "netuid"))
	}
	views := append([]*subnetReplayView{current, count}, netuids...)
	for _, link := range links {
		views = append(views, link.view)
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, err
	}
	for _, link := range links {
		got, err := subnetReplayValue[common.Address](link.view, 0)
		if err != nil {
			return nil, err
		}
		if got != link.want {
			return nil, newSubnetReplayUnknown("deployment-link-mismatch", nil)
		}
	}
	for _, view := range netuids {
		got, err := subnetReplayValue[uint16](view, 0)
		if err != nil {
			return nil, err
		}
		if got != pin.netuid {
			return nil, newSubnetReplayUnknown("deployment-link-mismatch", nil)
		}
	}
	currentEpoch, err := subnetRootDeadlineUint(current)
	if err != nil {
		return nil, err
	}
	operatorCount, err := subnetRootDeadlineUint(count)
	if err != nil {
		return nil, err
	}
	if currentEpoch == 0 || operatorCount == 0 {
		return state, nil
	}
	if uint64(settings.MaximumOperators) < operatorCount {
		return nil, newSubnetReplayUnknown("operator-census-bound", nil)
	}
	state.epoch = currentEpoch - 1
	epoch := new(big.Int).SetUint64(state.epoch)
	policyView := newSubnetReplayView(v1.coordinator, pin.coordinator, "policyAt", epoch).revertible()
	endView := newSubnetReplayView(v1.coordinator, pin.coordinator, "epochEndBlock", epoch).revertible()
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, policyView, endView); err != nil {
		return nil, err
	}
	// A just-activated deployment may have no policy for the preceding epoch.
	if policyView.revertedWith(v1.coordinator, "InvalidPolicy") && endView.revertedWith(v1.coordinator, "InvalidPolicy") {
		return state, nil
	}
	policy, err := subnetReplayValue[stabi.STCoordinatorPolicySnapshot](policyView, 0)
	if err != nil {
		return nil, err
	}
	end, err := subnetRootDeadlineUint(endView)
	if err != nil {
		return nil, err
	}
	if policy.RootCommitWindowBlocks == 0 || policy.EpochBlocks <= policy.RootCommitWindowBlocks || math.MaxUint64-end < policy.RootCommitWindowBlocks || checkpoint.number < end {
		return nil, newSubnetReplayUnknown("epoch-clock-invalid", nil)
	}
	state.deadline = end + policy.RootCommitWindowBlocks
	if checkpoint.number < state.deadline && settings.RootDeadlineLeadBlocks < state.deadline-checkpoint.number {
		return state, nil
	}

	// Only the latest closed epoch can still commit under the contract policy.
	// Keep a miss visible past its deadline until the next epoch becomes due.
	ids := make([]*subnetReplayView, operatorCount)
	for i := range ids {
		ids[i] = newSubnetReplayView(v1.coordinator, pin.coordinator, "operatorIdAt", big.NewInt(int64(i)))
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, ids...); err != nil {
		return nil, err
	}
	type operatorRead struct {
		noId                                             uint64
		operator, pool                                   *subnetReplayView
		root, pending, count, lastFunded, funded, latest *subnetReplayView
		version                                          string
	}
	rows := make([]operatorRead, len(ids))
	views = nil
	seen := map[uint64]bool{}
	for i, idView := range ids {
		noId, err := subnetRootDeadlineUint(idView)
		if err != nil {
			return nil, err
		}
		if noId == 0 || seen[noId] {
			return nil, newSubnetReplayUnknown("operator-census-invalid", nil)
		}
		seen[noId] = true
		row := &rows[i]
		row.noId = noId
		id := new(big.Int).SetUint64(noId)
		row.operator = newSubnetReplayView(v1.coordinator, pin.coordinator, "operatorAt", id, epoch).revertible()
		views = append(views, row.operator)
		if pin.settlementV2Declared {
			row.pool = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "pools", id)
			views = append(views, row.pool)
		}
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, err
	}
	views = nil
	for i := range rows {
		row := &rows[i]
		id := new(big.Int).SetUint64(row.noId)
		if row.pool != nil {
			pool, err := subnetReplayValue[stabi.ISTSettlementVault2Pool](row.pool, 0)
			if err != nil {
				return nil, err
			}
			if pool.Registered != (pool.FirstEpoch != 0) {
				return nil, newSubnetReplayUnknown("pool-shape-invalid", nil)
			}
			if pool.Registered && pool.FirstEpoch <= state.epoch {
				row.version = "v2"
			}
		}
		if row.operator.revert != nil {
			if !row.operator.revertedWith(v1.coordinator, "UnknownOperator") || row.version == "v2" {
				return nil, newSubnetReplayUnknown("operator-read-reverted", nil)
			}
			continue
		}
		op, err := subnetReplayValue[stabi.STCoordinatorOperatorVersion](row.operator, 0)
		if err != nil {
			return nil, err
		}
		if row.version != "v2" {
			if !op.Active {
				continue
			}
			row.version = "v1"
			row.root = newSubnetReplayView(v1.coordinator, pin.coordinator, "rootCommitments", epoch, id)
			views = append(views, row.root)
		} else {
			row.pending = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "pending", id)
			row.count = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "rootCount", id)
			row.lastFunded = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "lastFundedEpoch", id)
			row.funded = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "fundedThrough", id, epoch)
			views = append(views, row.pending, row.count, row.lastFunded, row.funded)
		}
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, err
	}
	views = nil
	for i := range rows {
		row := &rows[i]
		if row.version != "v2" {
			continue
		}
		count, err := subnetRootDeadlineUint(row.count)
		if err != nil {
			return nil, err
		}
		if count != 0 {
			row.latest = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "roots", new(big.Int).SetUint64(row.noId), new(big.Int).SetUint64(count-1))
			views = append(views, row.latest)
		}
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		switch row.version {
		case "":
			continue
		case "v1":
			root, err := subnetSettlementRootOf(row.root)
			if err != nil {
				return nil, err
			}
			if root.PayoutRoot != [32]byte{} {
				continue
			}
		case "v2":
			pending, err := subnetReplayValue[stabi.ISTSettlementVault2Pending](row.pending, 0)
			if err != nil {
				return nil, err
			}
			if pending.Seq != 0 && pending.Epoch == state.epoch && pending.Root != [32]byte{} {
				continue
			}
			finalizedTotal := new(big.Int)
			if row.latest != nil {
				latest, err := subnetReplayValue[stabi.ISTSettlementVault2Root](row.latest, 0)
				if err != nil {
					return nil, err
				}
				if latest.Root == [32]byte{} || latest.Epoch > state.epoch || latest.Total == nil {
					return nil, newSubnetReplayUnknown("root-shape-invalid", nil)
				}
				if latest.Epoch == state.epoch {
					continue
				}
				finalizedTotal = latest.Total
			}
			lastFunded, err := subnetRootDeadlineUint(row.lastFunded)
			if err != nil {
				return nil, err
			}
			funded, err := subnetReplayValue[*big.Int](row.funded, 0)
			if err != nil {
				return nil, err
			}
			// An absent close is not evidence of zero funding. Only a recorded
			// close with no increase over the last finalized root can exempt it.
			if lastFunded >= state.epoch && funded.Cmp(finalizedTotal) == 0 {
				continue
			}
		}
		state.missing = append(state.missing, subnetRootDeadlineMissing{noId: row.noId, version: row.version})
	}
	return state, nil
}
