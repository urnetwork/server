// Operator-local deposit secrets authorize only their exact current on-chain
// staging position. Reads share one finalized hash; retained transactions keep
// their original nonce and bytes even when a later staging plan would differ.
package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server/model"
)

var errStDepositAlreadyCredited = errors.New("st: exact epoch deposit is already credited")

// The contract consumes two rao above principal across its two reserve moves.
// Native staging may floor its destination credit by one further rao. These
// bounds are admitted against the actual contract constants before any send.
const (
	stDepositReserveAllowanceRao  = 2
	stDepositTransferAllowanceRao = 1
)

// All fields belong to one endpoint and one canonical finalized Evm hash.
type stDepositCustody struct {
	boundary         protocol.ClientKeyEffectiveBoundary
	nonce            *big.Int
	deadline         uint64
	selfColdkey      [32]byte
	staged           *big.Int
	source           *big.Int
	deposited        *big.Int
	campaignReserved *big.Int
	policy           stabi.STCoordinatorPolicySnapshot
	minimumTao       uint64
	alphaPrice       *big.Int
}

// Does not round principal or a native transfer up to its minimum. A partial
// preload can leave a dust deficit which needs separately bounded provisioning.
func stDepositFundingAmount(principal, staged *big.Int) (*big.Int, error) {
	if principal == nil || principal.Sign() <= 0 || principal.BitLen() > 256 || staged == nil || staged.Sign() < 0 || staged.BitLen() > 256 {
		return nil, errors.New("st: invalid deposit principal or staged balance")
	}
	target := new(big.Int).Add(principal, big.NewInt(stDepositReserveAllowanceRao))
	if target.BitLen() > 256 {
		return nil, errors.New("st: deposit reserve staging amount overflows uint256")
	}
	if staged.Cmp(target) >= 0 {
		return new(big.Int), nil
	}
	amount := new(big.Int).Sub(target, staged)
	amount.Add(amount, big.NewInt(stDepositTransferAllowanceRao))
	if amount.BitLen() > 256 {
		return nil, errors.New("st: deposit source staging amount overflows uint256")
	}
	return amount, nil
}

// Deposit admission never treats the claims vault as a source of a signer.
// Every public identity derives from this operator's configured local secrets.
func (self *CoreStClient) depositDomain() (protocol.ClientKeyHistoryDomain, error) {
	if self == nil || self.cfg == nil || self.coordinator == nil || self.vault == nil || self.cfg.Netuid == 0 || self.cfg.Netuid > 65535 || self.cfg.DepositHotkey == ([32]byte{}) || self.cfg.ReserveSink == (common.Address{}) {
		return protocol.ClientKeyHistoryDomain{}, errors.New("st: deposit custody configuration is incomplete")
	}
	cfg := self.cfg
	for _, key := range []*ecdsa.PrivateKey{cfg.DepositKey, cfg.RootKey, cfg.ArtifactKey} {
		if key == nil || key.D == nil || key.Curve != crypto.S256() || key.X == nil || key.Y == nil || key.D.Sign() <= 0 || key.D.Cmp(crypto.S256().Params().N) >= 0 || !crypto.S256().IsOnCurve(key.X, key.Y) {
			return protocol.ClientKeyHistoryDomain{}, errors.New("st: deposit custody role key is missing or malformed")
		}
		derived, err := crypto.ToECDSA(crypto.FromECDSA(key))
		if err != nil || derived.X.Cmp(key.X) != 0 || derived.Y.Cmp(key.Y) != 0 {
			return protocol.ClientKeyHistoryDomain{}, errors.New("st: deposit custody private/public key differs")
		}
	}
	deposit := crypto.PubkeyToAddress(cfg.DepositKey.PublicKey)
	root := crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
	artifact := crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey)
	if deposit == root || deposit == artifact || root == artifact {
		return protocol.ClientKeyHistoryDomain{}, errors.New("st: deposit, root and artifact custody keys must be distinct")
	}
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: cfg.PolicyHash, NoID: cfg.NoId}
	return domain, domain.Validate()
}

// Reuses the production authority reader's native genesis, evidence anchor,
// graph, policy and current operator proof, then reads every economic field at
// that same canonical hash. Another endpoint must restart this entire read.
func (self *CoreStClient) readDepositCustody(ctx context.Context, client *ethclient.Client, epoch *uint64) (*stDepositCustody, error) {
	var result *stDepositCustody
	err := self.readTransactionRpc(ctx, client, func(ctx context.Context) error {
		var err error
		result, err = self.readDepositCustodyOnce(ctx, client, epoch)
		return err
	})
	return result, err
}

// One attempt owns the complete authority and economic snapshot. No partial
// result survives a failed read into the next bounded attempt.
func (self *CoreStClient) readDepositCustodyOnce(ctx context.Context, client *ethclient.Client, epoch *uint64) (*stDepositCustody, error) {
	domain, err := self.depositDomain()
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, stReadAttemptBudget)
	defer cancel()
	boundary, operator, err := readStClientKeyAuthorityAt(callCtx, client, domain, nil)
	if err != nil {
		return nil, fmt.Errorf("st: deposit current operator authority: %w", err)
	}
	if epoch != nil && boundary.Epoch != *epoch || operator.DepositSigner != crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey) || operator.RootSigner != crypto.PubkeyToAddress(self.cfg.RootKey.PublicKey) || operator.DepositHotkey != self.cfg.DepositHotkey || operator.DepositSigner == operator.RootSigner {
		return nil, errors.New("st: deposit epoch, signer or isolated hotkey differs from current operator authority")
	}
	coordinator := self.coordinator
	n := new(big.Int).SetUint64(self.cfg.NoId)
	e := new(big.Int).SetUint64(boundary.Epoch)
	type view struct {
		address common.Address
		data    []byte
		words   int
	}
	views := []view{
		{address: self.cfg.ContractAddress, data: coordinator.PackSelfColdkey(), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackReserveSink(), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackNextDepositNonce(n), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackEpochEndBlock(e), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackEpochDeposits(e, n), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackCampaignReserved(), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackPolicyAt(e), words: 13},
		{address: self.cfg.ContractAddress, data: coordinator.PackRESERVEROUNDINGALLOWANCERAO(), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackRUNTIMESHAREROUNDINGALLOWANCERAO(), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackPaused(), words: 1},
		{address: self.cfg.ContractAddress, data: coordinator.PackOwner(), words: 1},
		{address: self.cfg.SettlementVault, data: self.vault.PackMinimumTransferTaoRao(), words: 1},
	}
	selfColdkey := ss58.EvmMirrorPubkey(self.cfg.ContractAddress)
	for _, coldkey := range [][32]byte{selfColdkey, ss58.EvmMirrorPubkey(operator.DepositSigner)} {
		data, err := stPackGetStake(operator.DepositHotkey, coldkey, self.cfg.Netuid)
		if err != nil {
			return nil, err
		}
		views = append(views, view{address: stStakingPrecompileAddress, data: data, words: 1})
	}
	priceCall := make([]byte, 36)
	copy(priceCall[:4], crypto.Keccak256([]byte("getAlphaPrice(uint16)"))[:4])
	new(big.Int).SetUint64(self.cfg.Netuid).FillBytes(priceCall[4:])
	views = append(views, view{address: common.HexToAddress("0x0000000000000000000000000000000000000808"), data: priceCall, words: 1})
	results := make([]hexutil.Bytes, len(views))
	batch := make([]rpc.BatchElem, len(views))
	for index, field := range views {
		batch[index] = rpc.BatchElem{Method: "eth_call", Args: []any{map[string]any{"to": field.address, "data": hexutil.Bytes(field.data)}, rpc.BlockNumberOrHashWithHash(common.Hash(boundary.Hash), true)}, Result: &results[index]}
	}
	if err := client.Client().BatchCallContext(callCtx, batch); err != nil {
		return nil, err
	}
	for index, field := range views {
		if batch[index].Error != nil || len(results[index]) != field.words*32 {
			return nil, errors.Join(fmt.Errorf("st: deposit custody field %d unavailable or malformed", index), batch[index].Error)
		}
	}
	coldkey, coldkeyErr := coordinator.UnpackSelfColdkey(results[0])
	reserve, reserveErr := coordinator.UnpackReserveSink(results[1])
	end := new(big.Int).SetBytes(results[3])
	policy, policyErr := coordinator.UnpackPolicyAt(results[6])
	paused, pausedErr := coordinator.UnpackPaused(results[9])
	owner, ownerErr := coordinator.UnpackOwner(results[10])
	minimumTao, minimumErr := self.vault.UnpackMinimumTransferTaoRao(results[11])
	if err := errors.Join(coldkeyErr, reserveErr, policyErr, pausedErr, ownerErr, minimumErr); err != nil {
		return nil, err
	}
	if coldkey != selfColdkey || reserve != self.cfg.ReserveSink || paused || owner == operator.DepositSigner || owner == (common.Address{}) ||
		policy.PolicyHash != self.cfg.PolicyHash || new(big.Int).SetBytes(results[7]).Cmp(big.NewInt(stDepositReserveAllowanceRao)) != 0 || new(big.Int).SetBytes(results[8]).Cmp(big.NewInt(stDepositTransferAllowanceRao)) != 0 || minimumTao == 0 || !end.IsUint64() {
		return nil, errors.New("st: deposit graph, owner separation, pause or rounding policy differs")
	}
	deadline, err := stDepositDeadline(boundary.Epoch, end.Uint64(), boundary.Block)
	if err != nil {
		return nil, err
	}
	limits := stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: protocol.MaxClientKeyObservationBatchRpcRequests, MaximumMethods: protocol.MaxClientKeyObservationBatchRpcMethods, MaximumBytes: protocol.MaxClientKeyObservationBatchControlBytes}
	if _, err := stabi.WitnessClientKeyAuthorityBoundariesContext(callCtx, client, domain, []protocol.ClientKeyEffectiveBoundary{boundary}, limits); err != nil {
		return nil, err
	}
	result := &stDepositCustody{boundary: boundary, nonce: new(big.Int).SetBytes(results[2]), deadline: deadline, selfColdkey: selfColdkey, staged: new(big.Int).SetBytes(results[12]), source: new(big.Int).SetBytes(results[13]), deposited: new(big.Int).SetBytes(results[4]), campaignReserved: new(big.Int).SetBytes(results[5]), policy: policy, minimumTao: minimumTao, alphaPrice: new(big.Int).SetBytes(results[14])}
	if result.alphaPrice.Sign() <= 0 {
		return nil, errors.New("st: deposit canonical alpha price is zero")
	}
	return result, callCtx.Err()
}

// Exact principal is governed separately from the at-most-three-rao staging
// headroom. Neither existing demand nor campaign principal can be deposited twice.
func (self *stDepositCustody) validatePrincipal(cfg *StConfig, amount *big.Int) error {
	if amount == nil || amount.Sign() <= 0 || !amount.IsUint64() || cfg.DepositEpochCapRao == 0 || amount.Cmp(new(big.Int).SetUint64(cfg.DepositEpochCapRao)) > 0 {
		return errors.New("st: deposit principal exceeds the configured epoch allowance")
	}
	if self.deposited.Sign() != 0 {
		if self.deposited.Cmp(amount) == 0 {
			return errStDepositAlreadyCredited
		}
		return errors.New("st: epoch already has a different deposit principal")
	}
	if amount.Cmp(self.policy.EpochDepositCapRao) > 0 || new(big.Int).Add(self.campaignReserved, amount).Cmp(self.policy.CampaignDepositCapRao) > 0 {
		return errors.New("st: deposit principal exceeds current on-chain policy allowance")
	}
	// The first reserve move can lose one rao before the second native
	// transfer. That smaller second amount must still clear the native floor.
	return self.validateTransfer(new(big.Int).Add(amount, big.NewInt(stDepositReserveAllowanceRao-stDepositTransferAllowanceRao)))
}

// The transfer floor constrains execution, never the usage-based principal.
func (self *stDepositCustody) validateTransfer(amount *big.Int) error {
	if amount == nil || amount.Sign() <= 0 || amount.BitLen() > 256 {
		return errors.New("st: invalid deposit native transfer amount")
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	tao := new(big.Int).Quo(new(big.Int).Mul(amount, self.alphaPrice), scale)
	if tao.Cmp(new(big.Int).SetUint64(self.minimumTao)) < 0 {
		return errStDepositBelowRuntimeMinimum
	}
	return nil
}

// A raw staging entry or recovered attempt has the same bounded source debit.
// Existing epoch demand cannot authorize another staging transfer.
func (self *stDepositCustody) validateFunding(cfg *StConfig, amount *big.Int) error {
	maximum := new(big.Int).Add(new(big.Int).SetUint64(cfg.DepositEpochCapRao), big.NewInt(stDepositReserveAllowanceRao+stDepositTransferAllowanceRao))
	if amount == nil || cfg.DepositEpochCapRao == 0 || amount.Cmp(maximum) > 0 || self.source.Cmp(amount) < 0 || self.deposited.Sign() != 0 {
		return errors.New("st: deposit staging exceeds current source custody or allowance")
	}
	return self.validateTransfer(amount)
}

// Retried business inputs cannot overwrite the original durable transaction.
// A mismatch is a recoverable controller error instead of a model-level panic.
func stDepositRetainedCalldata(cfg *StConfig, operation string, to common.Address, data []byte, prior *model.StTransactionIntent) error {
	if prior == nil {
		return nil
	}
	logicalKey, err := stTransactionLogicalKey(cfg, operation)
	if err != nil || prior.LogicalKey != logicalKey || prior.DeploymentKey != cfg.DeploymentKey() || prior.ChainId != cfg.ChainId || !strings.EqualFold(prior.GenesisHash, hexutil.Encode(cfg.GenesisHash[:])) || !strings.EqualFold(prior.FromAddress, crypto.PubkeyToAddress(cfg.DepositKey.PublicKey).Hex()) || !strings.EqualFold(prior.ToAddress, to.Hex()) || !strings.EqualFold(prior.CalldataHash, crypto.Keccak256Hash(data).Hex()) || !bytes.Equal(prior.Calldata, data) {
		return errors.New("st: deposit retry differs from original immutable intent; original intent preserved")
	}
	return nil
}

// A newly signed execution, including a fee replacement after restart, must
// still name the admitted operator and exact encoded coordinator nonce. A
// cancellation or receipt read never acquires a fresh deposit capability.
func (self *CoreStClient) validateDepositAttempt(ctx context.Context, client *ethclient.Client, intent *model.StTransactionIntent, kind string) error {
	if self.coordinator == nil || self.cfg == nil || self.cfg.DepositKey == nil || kind != model.StTxAttemptExecution || !strings.EqualFold(intent.FromAddress, crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey).Hex()) {
		return nil
	}
	state, err := self.readDepositCustody(ctx, client, nil)
	if err != nil {
		return err
	}
	operation := ""
	if strings.EqualFold(intent.ToAddress, stStakingPrecompileAddress.Hex()) && len(intent.Calldata) == 164 {
		amount := new(big.Int).SetBytes(intent.Calldata[132:164])
		canonical, err := stPackTransferStake(state.selfColdkey, self.cfg.DepositHotkey, self.cfg.Netuid, amount)
		if err != nil || !bytes.Equal(canonical, intent.Calldata) {
			return errors.New("st: retained deposit staging intent differs from current isolated custody or allowance")
		}
		if err := state.validateFunding(self.cfg, amount); err != nil {
			return err
		}
		operation = fmt.Sprintf("deposit-fund:%d:%d:%s", state.boundary.Epoch, self.cfg.NoId, state.nonce)
	} else if strings.EqualFold(intent.ToAddress, self.cfg.ContractAddress.Hex()) && len(intent.Calldata) == 132 {
		amount := new(big.Int).SetBytes(intent.Calldata[36:68])
		canonical := self.coordinator.PackDeposit(new(big.Int).SetUint64(self.cfg.NoId), amount, state.nonce, state.deadline)
		if !bytes.Equal(canonical, intent.Calldata) {
			return errors.New("st: retained deposit calldata differs from current operator, nonce or deadline")
		}
		if err := state.validatePrincipal(self.cfg, amount); err != nil {
			return err
		}
		if state.staged.Cmp(new(big.Int).Add(amount, big.NewInt(stDepositReserveAllowanceRao))) < 0 {
			return errors.New("st: deposit staging lacks principal plus exact reserve rounding allowance")
		}
		operation = fmt.Sprintf("deposit:%d:%d:%s", state.boundary.Epoch, self.cfg.NoId, state.nonce)
	} else {
		return errors.New("st: deposit custody key cannot sign an unrelated execution intent")
	}
	logicalKey, err := stTransactionLogicalKey(self.cfg, operation)
	if err != nil || intent.LogicalKey != logicalKey || intent.DeploymentKey != self.cfg.DeploymentKey() || intent.ChainId != self.cfg.ChainId || !strings.EqualFold(intent.GenesisHash, hexutil.Encode(self.cfg.GenesisHash[:])) || !strings.EqualFold(intent.CalldataHash, crypto.Keccak256Hash(intent.Calldata).Hex()) {
		return errors.New("st: retained deposit intent belongs to another immutable operation")
	}
	return nil
}

// Reconciliation finishes before sizing new funding. An already completed
// funding intent that left a deficit is retained for diagnosis; it is never
// silently rewritten or followed by another transfer under a new nonce.
func (self *CoreStClient) stageDepositPrincipal(ctx context.Context, epoch uint64, principal *big.Int) (string, error) {
	return self.sendPrepared(ctx, self.cfg.DepositKey, func(ctx context.Context, client *ethclient.Client) (string, common.Address, []byte, error) {
		state, err := self.readDepositCustody(ctx, client, &epoch)
		if err != nil {
			return "", common.Address{}, nil, err
		}
		if err := state.validatePrincipal(self.cfg, principal); err != nil {
			return "", common.Address{}, nil, err
		}
		amount, err := stDepositFundingAmount(principal, state.staged)
		if err != nil || amount.Sign() == 0 {
			return "", common.Address{}, nil, err
		}
		operation := fmt.Sprintf("deposit-fund:%d:%d:%s", epoch, self.cfg.NoId, state.nonce)
		logicalKey, err := stTransactionLogicalKey(self.cfg, operation)
		if err != nil {
			return "", common.Address{}, nil, err
		}
		prior := model.GetStTransactionIntent(ctx, logicalKey)
		if prior != nil && prior.Status != model.StTxReverted {
			return "", common.Address{}, nil, errors.New("st: retained deposit funding intent still has insufficient staged balance; original intent preserved")
		}
		if err := state.validateFunding(self.cfg, amount); err != nil {
			return "", common.Address{}, nil, err
		}
		data, err := stPackTransferStake(state.selfColdkey, self.cfg.DepositHotkey, self.cfg.Netuid, amount)
		if err == nil {
			err = stDepositRetainedCalldata(self.cfg, operation, stStakingPrecompileAddress, data, prior)
		}
		return operation, stStakingPrecompileAddress, data, err
	})
}
