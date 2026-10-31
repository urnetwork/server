// The core client's chain surface for the phase C sweep chain (st_v1_sweep.go).
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server/model"
)

// The EIP-1967 implementation slot of STSettlement2's UUPS proxy.
var stV1SweepImplementationSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")

// The phase C STSettlement2 surface (sn evm/src/STSettlement2PhaseC.sol), all
// owner-only: assignSurplus(noId, epoch, amount) adds escrow surplus to the
// pool's next funding snapshot, epoch being vault2.lastFundedEpoch(noId) + 1;
// recordV1Sweep(noId, epoch, artifactHash) records the committed sweep root for
// readers; v1SweepArtifactHash(noId, epoch) reads that record.
var (
	stV1SweepAssignSelector   = crypto.Keccak256([]byte("assignSurplus(uint256,uint256,uint256)"))[:4]
	stV1SweepRecordSelector   = crypto.Keccak256([]byte("recordV1Sweep(uint256,uint256,bytes32)"))[:4]
	stV1SweepRecordedSelector = crypto.Keccak256([]byte("v1SweepArtifactHash(uint256,uint256)"))[:4]
)

// StV1SweepPackAssign is the owner's assignSurplus(noId, epoch, amount) call.
func StV1SweepPackAssign(noId uint64, epoch uint64, amount *big.Int) ([]byte, error) {
	arguments := abi.Arguments{{Name: "noId", Type: stAbiUint256Type}, {Name: "epoch", Type: stAbiUint256Type}, {Name: "amount", Type: stAbiUint256Type}}
	packed, err := arguments.Pack(new(big.Int).SetUint64(noId), new(big.Int).SetUint64(epoch), amount)
	if err != nil {
		return nil, fmt.Errorf("pack assignSurplus: %w", err)
	}
	return append(append([]byte(nil), stV1SweepAssignSelector...), packed...), nil
}

// StV1SweepPackRecord is the owner's recordV1Sweep(noId, epoch, artifactHash)
// call.
func StV1SweepPackRecord(noId uint64, epoch uint64, artifactHash [32]byte) ([]byte, error) {
	arguments := abi.Arguments{{Name: "noId", Type: stAbiUint256Type}, {Name: "epoch", Type: stAbiUint256Type}, {Name: "artifactHash", Type: stAbiBytes32Type}}
	packed, err := arguments.Pack(new(big.Int).SetUint64(noId), new(big.Int).SetUint64(epoch), artifactHash)
	if err != nil {
		return nil, fmt.Errorf("pack recordV1Sweep: %w", err)
	}
	return append(append([]byte(nil), stV1SweepRecordSelector...), packed...), nil
}

func stV1SweepPackRecorded(noId uint64, epoch uint64) []byte {
	encoded := make([]byte, 68)
	copy(encoded, stV1SweepRecordedSelector)
	new(big.Int).SetUint64(noId).FillBytes(encoded[4:36])
	new(big.Int).SetUint64(epoch).FillBytes(encoded[36:68])
	return encoded
}

func (self *CoreStClient) v1SweepRequire() error {
	if self.cfg == nil || self.coordinator == nil || self.vault == nil || !self.cfg.LedgerV2Enabled() || self.cfg.VaultV2 == (common.Address{}) {
		return errors.New("st: the v1 sweep needs the v1 coordinator, vault1 and the settlement v2 pair")
	}
	return nil
}

func (self *CoreStClient) v1SweepEntitlement(ctx context.Context, block uint64, epoch uint64, noId uint64) (StV1SweepEntitlement, error) {
	e, n := new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId)
	entitlement, err := stViewAtBlock(self, ctx, self.cfg.SettlementVault, block, self.vault.PackEntitlement(e, n), self.vault.UnpackEntitlement)
	if err != nil {
		return StV1SweepEntitlement{}, fmt.Errorf("entitlement(%d): %w", epoch, err)
	}
	commit, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, block, self.coordinator.PackRootCommitments(e, n), self.coordinator.UnpackRootCommitments)
	if err != nil {
		return StV1SweepEntitlement{}, fmt.Errorf("rootCommitments(%d): %w", epoch, err)
	}
	end, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, block, self.coordinator.PackEpochEndBlock(e), self.coordinator.UnpackEpochEndBlock)
	if err != nil {
		return StV1SweepEntitlement{}, fmt.Errorf("epochEndBlock(%d): %w", epoch, err)
	}
	if !end.IsUint64() {
		return StV1SweepEntitlement{}, fmt.Errorf("epochEndBlock(%d) exceeds uint64", epoch)
	}
	return StV1SweepEntitlement{
		Epoch: epoch, End: end.Uint64(), Status: entitlement.Status,
		Funded: entitlement.Funded, Total: entitlement.Total, Claimed: entitlement.Claimed,
		PayoutRoot: entitlement.PayoutRoot, ArtifactHash: entitlement.ArtifactHash, ExpiryBlock: entitlement.ExpiryBlock,
		Root: commit.PayoutRoot, RootArtifact: commit.ArtifactHash, RootBlock: commit.CommitBlock,
	}, nil
}

// V1SweepChain reads the sweep's facts at one finalized block.
func (self *CoreStClient) V1SweepChain(ctx context.Context, epoch uint64, noId uint64, firstEpoch uint64) (*StV1SweepChain, error) {
	if err := self.v1SweepRequire(); err != nil {
		return nil, err
	}
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return nil, err
	}
	defer stopRead()
	head, err := self.finalizedBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("finalized sweep head: %w", err)
	}
	block := head.Number
	chain := &StV1SweepChain{HeadBlock: block, HeadHash: head.Hash, HeadTime: head.Time}
	coordinator, vault, vault2 := self.cfg.ContractAddress, self.cfg.SettlementVault, self.cfg.VaultV2
	if chain.Paused, err = stViewAtBlock(self, ctx, coordinator, block, self.coordinator.PackPaused(), self.coordinator.UnpackPaused); err != nil {
		return nil, fmt.Errorf("paused(): %w", err)
	}
	current, err := stViewAtBlock(self, ctx, coordinator, block, self.coordinator.PackCurrentEpoch(), self.coordinator.UnpackCurrentEpoch)
	if err != nil || !current.IsUint64() {
		return nil, fmt.Errorf("currentEpoch(): %v", err)
	}
	chain.Current = current.Uint64()
	policy, err := stViewAtBlock(self, ctx, coordinator, block, self.coordinator.PackPolicyAt(new(big.Int).SetUint64(epoch)), self.coordinator.UnpackPolicyAt)
	if err != nil {
		return nil, fmt.Errorf("policyAt(%d): %w", epoch, err)
	}
	chain.CloseGrace, chain.RootWindow, chain.FinalizeOffset = policy.CloseGraceBlocks, policy.RootCommitWindowBlocks, policy.FinalizeOffsetBlocks
	readEnd := func(e uint64) (uint64, error) {
		end, err := stViewAtBlock(self, ctx, coordinator, block, self.coordinator.PackEpochEndBlock(new(big.Int).SetUint64(e)), self.coordinator.UnpackEpochEndBlock)
		if err != nil || !end.IsUint64() {
			return 0, fmt.Errorf("epochEndBlock(%d): %v", e, err)
		}
		return end.Uint64(), nil
	}
	if 0 < chain.Current {
		if chain.RecentEnd, err = readEnd(chain.Current - 1); err != nil {
			return nil, err
		}
	}
	if chain.NextEnd, err = readEnd(chain.Current); err != nil {
		return nil, err
	}
	for e := uint64(0); e < epoch; e++ {
		entitlement, err := self.v1SweepEntitlement(ctx, block, e, noId)
		if err != nil {
			return nil, err
		}
		chain.Prior = append(chain.Prior, entitlement)
	}
	if chain.Sweep, err = self.v1SweepEntitlement(ctx, block, epoch, noId); err != nil {
		return nil, err
	}
	chain.End = chain.Sweep.End
	for e := firstEpoch; e < chain.Current; e++ {
		if e == epoch {
			continue
		}
		entitlement, err := self.v1SweepEntitlement(ctx, block, e, noId)
		if err != nil {
			return nil, err
		}
		chain.PostCutover = append(chain.PostCutover, entitlement)
	}
	if chain.Carry, err = stViewAtBlock(self, ctx, vault, block, self.vault.PackCarry(new(big.Int).SetUint64(noId)), self.vault.UnpackCarry); err != nil {
		return nil, fmt.Errorf("carry(): %w", err)
	}
	if chain.Vault2Coldkey, err = stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackSelfColdkey(), stLedgerV2VaultAbi.UnpackSelfColdkey); err != nil {
		return nil, fmt.Errorf("vault2 selfColdkey(): %w", err)
	}
	if chain.Vault2Coldkey == ([32]byte{}) {
		return nil, errors.New("vault2 selfColdkey() is zero")
	}
	if chain.V1EscrowHotkey, err = stViewAtBlock(self, ctx, vault, block, self.vault.PackEscrowHotkey(), self.vault.UnpackEscrowHotkey); err != nil {
		return nil, fmt.Errorf("escrowHotkey(): %w", err)
	}
	// The local root must be the vault's own leaf.
	leaf, err := stViewAtBlock(self, ctx, vault, block, self.vault.PackPayoutLeaf(chain.Vault2Coldkey, big.NewInt(stV1SweepShareBps)), self.vault.UnpackPayoutLeaf)
	if err != nil {
		return nil, fmt.Errorf("payoutLeaf(): %w", err)
	}
	if leaf != StV1SweepRoot(chain.Vault2Coldkey) {
		return nil, errors.New("vault1 payoutLeaf differs from the sweep root")
	}
	if chain.LeafClaimed, err = stViewAtBlock(self, ctx, vault, block, self.vault.PackLeafClaimed(new(big.Int).SetUint64(epoch), stV1SweepClaimKey(noId, chain.Vault2Coldkey)), self.vault.UnpackLeafClaimed); err != nil {
		return nil, fmt.Errorf("leafClaimed(): %w", err)
	}
	if chain.ClaimCredit, err = stViewAtBlock(self, ctx, vault, block, self.vault.PackClaimCredit(chain.Vault2Coldkey), self.vault.UnpackClaimCredit); err != nil {
		return nil, fmt.Errorf("claimCredit(): %w", err)
	}
	if chain.Absorbable, err = self.stakeAtBlock(ctx, block, chain.V1EscrowHotkey, chain.Vault2Coldkey); err != nil {
		return nil, fmt.Errorf("vault2 stake on the v1 escrow: %w", err)
	}
	if chain.V2LiveEscrow, err = stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackLiveEscrowStake(), stLedgerV2VaultAbi.UnpackLiveEscrowStake); err != nil {
		return nil, fmt.Errorf("vault2 liveEscrowStake(): %w", err)
	}
	if chain.V2Accounted, err = stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackEscrowAccounted(), stLedgerV2VaultAbi.UnpackEscrowAccounted); err != nil {
		return nil, fmt.Errorf("vault2 escrowAccounted(): %w", err)
	}
	n := new(big.Int).SetUint64(noId)
	pending, err := stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackPending(n), stLedgerV2VaultAbi.UnpackPending)
	if err != nil {
		return nil, fmt.Errorf("vault2 pending(): %w", err)
	}
	if pending.Seq != 0 {
		chain.V2RootPresent, chain.V2RootThrough = true, pending.Epoch
	}
	count, err := stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackRootCount(n), stLedgerV2VaultAbi.UnpackRootCount)
	if err != nil || !count.IsUint64() {
		return nil, fmt.Errorf("vault2 rootCount(): %v", err)
	}
	if 0 < count.Uint64() {
		latest, err := stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackRoots(n, new(big.Int).SetUint64(count.Uint64()-1)), stLedgerV2VaultAbi.UnpackRoots)
		if err != nil {
			return nil, fmt.Errorf("vault2 roots(): %w", err)
		}
		if !chain.V2RootPresent || chain.V2RootThrough < latest.Epoch {
			chain.V2RootPresent, chain.V2RootThrough = true, latest.Epoch
		}
	}
	last, err := stViewAtBlock(self, ctx, vault2, block, stLedgerV2VaultAbi.PackLastFundedEpoch(n), stLedgerV2VaultAbi.UnpackLastFundedEpoch)
	if err != nil || !last.IsUint64() {
		return nil, fmt.Errorf("vault2 lastFundedEpoch(): %v", err)
	}
	chain.V2NextEpoch = last.Uint64() + 1
	if chain.AssignSelector, err = self.v1SweepAssignLive(ctx, block); err != nil {
		return nil, err
	}
	if chain.AssignSelector {
		err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
			out, err := client.CallContract(ctx, ethereum.CallMsg{To: &self.cfg.SettlementV2, Data: stV1SweepPackRecorded(noId, epoch)}, new(big.Int).SetUint64(block))
			if err != nil {
				return err
			}
			if len(out) != 32 {
				return fmt.Errorf("v1SweepArtifactHash returned %d bytes", len(out))
			}
			chain.RecordedArtifact = [32]byte(out)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("v1SweepArtifactHash(): %w", err)
		}
	}
	return chain, nil
}

// Whether STSettlement2's implementation at block is the phase C upgrade: its
// dispatcher carries the owner's assignSurplus(noId, epoch, amount).
func (self *CoreStClient) v1SweepAssignLive(ctx context.Context, block uint64) (bool, error) {
	live := false
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		at := new(big.Int).SetUint64(block)
		slot, err := client.StorageAt(ctx, self.cfg.SettlementV2, stV1SweepImplementationSlot, at)
		if err != nil {
			return err
		}
		implementation := common.BytesToAddress(slot)
		if implementation == (common.Address{}) {
			return errors.New("STSettlement2 has no implementation")
		}
		code, err := client.CodeAt(ctx, implementation, at)
		if err != nil {
			return err
		}
		// A dispatcher compares the selector with PUSH4.
		live = bytes.Contains(code, append([]byte{0x63}, stV1SweepAssignSelector...))
		return nil
	})
	return live, err
}

func (self *CoreStClient) V1SweepLatestBlock(ctx context.Context) (uint64, error) {
	return self.LedgerV2LatestBlock(ctx)
}

// V1SweepClose sends the permissionless v1 close from the deposit signer. It
// keeps the close operation key, so the engine's expired-close handling
// applies; the deposit signer's nonce sequence is never the root key's.
func (self *CoreStClient) V1SweepClose(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	if self.cfg.DepositKey == nil {
		return "", errors.New("st: the sweep close needs the deposit signer key")
	}
	return self.send(ctx, fmt.Sprintf("close:%d:%d", epoch, noId), self.cfg.DepositKey, self.cfg.ContractAddress, self.coordinator.PackCloseOperatorEpoch(new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId)))
}

func (self *CoreStClient) V1SweepDefer(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	return self.DeferMissedEmission(ctx, epoch, noId)
}

func (self *CoreStClient) V1SweepCommit(ctx context.Context, epoch uint64, noId uint64, root [32]byte, artifactHash [32]byte) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	return self.CommitPayoutRoot(ctx, epoch, noId, root, artifactHash[:])
}

// V1SweepCommitted reads a commit intent already reserved under root:e:no and
// returns its artifact hash. A reserved intent naming another root refuses.
func (self *CoreStClient) V1SweepCommitted(ctx context.Context, epoch uint64, noId uint64, root [32]byte) ([32]byte, bool, error) {
	if err := self.v1SweepRequire(); err != nil {
		return [32]byte{}, false, err
	}
	logicalKey, err := stTransactionLogicalKey(self.cfg, fmt.Sprintf("root:%d:%d", epoch, noId))
	if err != nil {
		return [32]byte{}, false, err
	}
	intent := model.GetStTransactionIntent(ctx, logicalKey)
	if intent == nil {
		return [32]byte{}, false, nil
	}
	expected := self.coordinator.PackCommitOperatorRoot(new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), root, [32]byte{})
	if len(intent.Calldata) != len(expected) || !bytes.Equal(intent.Calldata[:len(expected)-32], expected[:len(expected)-32]) {
		return [32]byte{}, false, fmt.Errorf("st: the reserved commit of epoch %d names another root", epoch)
	}
	return [32]byte(intent.Calldata[len(expected)-32:]), true, nil
}

func (self *CoreStClient) V1SweepFinalize(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	if noId != self.cfg.NoId {
		return "", fmt.Errorf("st: the sweep finalizes pool %d, not %d", self.cfg.NoId, noId)
	}
	return self.FinalizeEpoch(ctx, epoch)
}

// V1SweepClaim claims the single leaf with an empty proof for coldkey.
func (self *CoreStClient) V1SweepClaim(ctx context.Context, epoch uint64, noId uint64, coldkey [32]byte) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	calldata := self.vault.PackClaim(new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), coldkey, big.NewInt(stV1SweepShareBps), [][32]byte{})
	return self.send(ctx, fmt.Sprintf("v1sweepclaim:%d:%d", epoch, noId), self.cfg.RootKey, self.cfg.SettlementVault, calldata)
}

// V1SweepWithdraw retries a deferred payment of coldkey's claim credit; each
// attempt has its own operation key.
func (self *CoreStClient) V1SweepWithdraw(ctx context.Context, epoch uint64, noId uint64, attempt uint64, coldkey [32]byte) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	return self.send(ctx, fmt.Sprintf("v1sweepwithdraw:%d:%d:%d", epoch, noId, attempt), self.cfg.RootKey, self.cfg.SettlementVault, self.vault.PackWithdrawClaimCredit(coldkey))
}

// V1SweepAbsorb moves vault2's stake on the v1 escrow hotkey into vault2's
// escrow as unaccounted surplus.
func (self *CoreStClient) V1SweepAbsorb(ctx context.Context, epoch uint64, noId uint64, hotkey [32]byte) (string, error) {
	if err := self.v1SweepRequire(); err != nil {
		return "", err
	}
	return self.send(ctx, fmt.Sprintf("v1sweepabsorb:%d:%d", epoch, noId), self.cfg.RootKey, self.cfg.VaultV2, stLedgerV2VaultAbi.PackAbsorbStake(hotkey))
}
