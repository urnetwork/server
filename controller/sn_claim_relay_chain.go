package controller

// sn_claim_relay_chain.go — the claim relay on the operator's durable transaction
// engine and the ledger the driver records. The relayer is an account of its own:
// its intents share nothing with the root and deposit keys' but the code, its
// nonces reconcile alone, and its spend is read back from them, so a restart
// neither repeats a claim nor forgets what the day has spent.
//
// Three things differ from the operator's sends, only on the relay's own client
// (stClaimRelayEngine):
//   - the operator gas policy names exactly the deposit and root accounts, and
//     re-signing it is a Ledger step. The relayer is neither. The daily cap
//     stands in for it;
//   - a transaction is signed at the gas limit the relay checked against the cap,
//     and with a fee cap close to the price, so that the most it can cost is
//     small enough to check (sn_claim_relay.go);
//   - an intent found without a signature after a restart is retired, not
//     resumed, because the facts it was checked against have gone by. The next
//     pass plans the claim again.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	stconn "github.com/urnetwork/server/st"
)

// The vault resource that holds the relayer key: a dedicated EVM key holding
// only operating TAO. Without it the relay does nothing.
//
//	address: "0x<relayer>"
//	private_key: "<hex privkey>"
//	testnet-address: "0x<relayer>"
//	testnet-private-key: "<hex privkey>"
//	testnet-batcher: "0x<claim batcher>"
const snClaimRelayVaultResource = "sn_claim_relay.yml"

// The release claim batcher on mainnet. Its code is checked against the release
// runtime before its first use, and any other answer means claims go alone.
const snClaimRelayMainnetBatcher = "0x14174e7c5671ce1499aef9c68d74bebffa58c41a"

var snClaimRelayAlphaPrecompile = common.HexToAddress("0x0000000000000000000000000000000000000808")

// getAlphaPrice(uint16): wei of TAO per α, the price the runtime's transfer
// floor uses.
var snClaimRelayGetAlphaPriceSelector = crypto.Keccak256([]byte("getAlphaPrice(uint16)"))[:4]

type snClaimRelayVaultFile struct {
	Address           string `yaml:"address"`
	PrivateKey        string `yaml:"private_key"`
	Batcher           string `yaml:"batcher"`
	TestnetAddress    string `yaml:"testnet-address"`
	TestnetPrivateKey string `yaml:"testnet-private-key"`
	TestnetBatcher    string `yaml:"testnet-batcher"`
}

// snClaimRelayKey is the relayer as the vault names it.
type snClaimRelayKey struct {
	key     *ecdsa.PrivateKey
	address common.Address
	batcher common.Address
}

// The resource's bytes, or errSnClaimRelayNoKey without one. Tests replace it.
var snClaimRelayVaultBytes = func() ([]byte, error) {
	resource, err := server.Vault.SimpleResource(snClaimRelayVaultResource)
	if err != nil {
		return nil, errSnClaimRelayNoKey
	}
	raw, err := resource.BytesE()
	if err != nil {
		return nil, errSnClaimRelayNoKey
	}
	return raw, nil
}

// loadSnClaimRelayKey reads the relayer for the selected profile on every pass,
// so adding the key takes no restart. An empty entry is no key. Anything else
// that cannot be used is refused: a malformed key, an address that is not the
// key's, or one of the operator's role keys.
func loadSnClaimRelayKey(cfg *StConfig) (*snClaimRelayKey, error) {
	raw, err := snClaimRelayVaultBytes()
	if err != nil {
		return nil, err
	}
	var file snClaimRelayVaultFile
	if yaml.Unmarshal(raw, &file) != nil {
		// never the decoder's message: it may quote the bytes
		return nil, errors.New("claim relay: the vault entry does not parse")
	}
	privateKey, address, batcher := file.PrivateKey, file.Address, file.Batcher
	if cfg.Profile == stconn.ProfileTestnet {
		privateKey, address, batcher = file.TestnetPrivateKey, file.TestnetAddress, file.TestnetBatcher
	} else if batcher == "" {
		batcher = snClaimRelayMainnetBatcher
	}
	if strings.TrimSpace(privateKey) == "" {
		return nil, errSnClaimRelayNoKey
	}
	key, err := stParsePrivateKey(privateKey)
	if err != nil {
		return nil, errors.New("claim relay: the vault entry's private_key is not a key")
	}
	relayer := &snClaimRelayKey{key: key, address: crypto.PubkeyToAddress(key.PublicKey)}
	if address != "" && (!common.IsHexAddress(address) || common.HexToAddress(address) != relayer.address) {
		return nil, fmt.Errorf("claim relay: the vault entry's private_key is not the key of address %s", address)
	}
	for _, role := range []*ecdsa.PrivateKey{cfg.DepositKey, cfg.RootKey, cfg.OpsKey, cfg.ArtifactKey} {
		if role != nil && crypto.PubkeyToAddress(role.PublicKey) == relayer.address {
			return nil, errors.New("claim relay: the relayer key is one of the operator's role keys")
		}
	}
	if batcher != "" {
		if !common.IsHexAddress(batcher) {
			return nil, fmt.Errorf("claim relay: the vault entry's batcher %q is not an address", batcher)
		}
		relayer.batcher = common.HexToAddress(batcher)
	}
	return relayer, nil
}

// stClaimRelayEngine is what makes a CoreStClient the relay's. Only the relay
// builds one; every other client has none.
type stClaimRelayEngine struct {
	account common.Address

	stateLock sync.Mutex
	// the gas limit each planned transaction is signed at
	calldataHashGasLimits map[common.Hash]uint64
}

func newStClaimRelayEngine(account common.Address) *stClaimRelayEngine {
	return &stClaimRelayEngine{account: account, calldataHashGasLimits: map[common.Hash]uint64{}}
}

// Only the relay's own account escapes the operator gas policy, and only on its
// own client.
func (self *stClaimRelayEngine) exemptFromOperatorGasPolicy(signingAccounts []common.Address) bool {
	return self != nil && len(signingAccounts) == 1 && signingAccounts[0] == self.account
}

func (self *stClaimRelayEngine) plan(calldata []byte, gasLimit uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calldataHashGasLimits[crypto.Keccak256Hash(calldata)] = gasLimit
}

func (self *stClaimRelayEngine) unplan(calldata []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.calldataHashGasLimits, crypto.Keccak256Hash(calldata))
}

// The gas limit a transaction was planned with, 0 for anything else.
func (self *stClaimRelayEngine) plannedGasLimit(calldata []byte) uint64 {
	if self == nil {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calldataHashGasLimits[crypto.Keccak256Hash(calldata)]
}

// The quote a replacement may rise to: the engine bumps the previous attempt's
// fees by 12.5% and takes this when it is higher. The relay's replacement is the
// bump alone, so the most its transaction can cost is known when it is first
// signed (snClaimRelayWorstCase). Any other client keeps the quote.
func (self *stClaimRelayEngine) replacementQuote(suggested *big.Int) *big.Int {
	if self != nil {
		return nil
	}
	return suggested
}

// An intent reconciled without any signed attempt never reached the chain. What
// it was checked against has gone by, so it is retired with the ordinary
// cancellation instead of signed now.
func (self *stClaimRelayEngine) retiresUnsigned(attempts []*model.StTransactionAttempt) bool {
	return self != nil && len(attempts) == 0
}

// The fee cap of the relay's transactions: the base fee and 1/8, and the tip. The
// operator's twice the base fee would double what a claim may cost, and the cap
// holds that.
func snClaimRelayFeeCap(baseFee *big.Int, tip *big.Int) *big.Int {
	fee := new(big.Int).Mul(baseFee, big.NewInt(9))
	fee.Quo(fee, big.NewInt(8))
	return fee.Add(fee, tip)
}

// snClaimRelayCoreChain implements snClaimRelayChain.
type snClaimRelayCoreChain struct {
	cfg     *StConfig
	relayer *snClaimRelayKey
	client  *CoreStClient
	batcher common.Address

	stateLock sync.Mutex
	// what each transaction's receipt said; a finalized receipt never changes
	txHashReceipts map[string]*snClaimRelayReceipt
	// vault2's finalized roots never change
	rootIndexRoots  map[string]*StLedgerV2FinalizedRoot
	batcherVerified bool
}

var (
	snClaimRelayCoreChainLock sync.Mutex
	snClaimRelayCoreChains    = map[string]*snClaimRelayCoreChain{}
)

// One per relayer and deployment in a process, which keeps its dialed clients.
func newSnClaimRelayCoreChain(cfg *StConfig, relayer *snClaimRelayKey) *snClaimRelayCoreChain {
	snClaimRelayCoreChainLock.Lock()
	defer snClaimRelayCoreChainLock.Unlock()
	cacheKey := fmt.Sprintf("%s|%x|%x", cfg.DeploymentKey(), relayer.address, relayer.batcher)
	if chain := snClaimRelayCoreChains[cacheKey]; chain != nil {
		return chain
	}
	chain := &snClaimRelayCoreChain{
		cfg:     cfg,
		relayer: relayer,
		batcher: relayer.batcher,
		client: &CoreStClient{
			cfg:         cfg,
			coordinator: stabi.NewSTCoordinator(),
			vault:       stabi.NewSTSettlementVault(),
			reserve:     stabi.NewSTReserveSink(),
			clients:     map[string]*ethclient.Client{},
			claimRelay:  newStClaimRelayEngine(relayer.address),
		},
		txHashReceipts: map[string]*snClaimRelayReceipt{},
		rootIndexRoots: map[string]*StLedgerV2FinalizedRoot{},
	}
	snClaimRelayCoreChains[cacheKey] = chain
	return chain
}

func (self *snClaimRelayCoreChain) Reconcile(ctx context.Context) error {
	genesisHash := "0x" + hex.EncodeToString(self.cfg.GenesisHash[:])
	from := strings.ToLower(self.relayer.address.Hex())
	if len(model.GetUnresolvedStTransactionIntents(ctx, self.cfg.ChainId, genesisHash, from)) == 0 {
		return nil
	}
	var client *ethclient.Client
	if err := self.client.eachRpc(ctx, func(_ context.Context, candidate *ethclient.Client) error {
		client = candidate
		return nil
	}); err != nil {
		return fmt.Errorf("claim relay: no rpc endpoint answered: %w", err)
	}
	return self.client.reconcileAccountIntents(ctx, client, self.relayer.key, self.relayer.address)
}

func (self *snClaimRelayCoreChain) Facts(ctx context.Context) (*snClaimRelayFacts, error) {
	facts := &snClaimRelayFacts{}
	priceCall := make([]byte, 36)
	copy(priceCall[:4], snClaimRelayGetAlphaPriceSelector)
	new(big.Int).SetUint64(self.cfg.Netuid).FillBytes(priceCall[4:])
	err := self.client.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		balance, err := client.BalanceAt(ctx, self.relayer.address, nil)
		if err != nil {
			return fmt.Errorf("relayer balance: %w", err)
		}
		prepared, err := readStTransactionFeePreparation(ctx, client, self.relayer.address, self.relayer.address, nil, false, false)
		if err != nil {
			return err
		}
		price, err := client.CallContract(ctx, ethereum.CallMsg{To: &snClaimRelayAlphaPrecompile, Data: priceCall}, nil)
		if err != nil {
			return fmt.Errorf("alpha price: %w", err)
		}
		if len(price) != 32 {
			return fmt.Errorf("alpha price: %d bytes", len(price))
		}
		facts.BalanceWei = balance
		facts.AlphaPriceWei = new(big.Int).SetBytes(price)
		if prepared.legacy {
			facts.GasPriceWei, facts.FeeCapWei = prepared.fee, prepared.fee
		} else {
			facts.GasPriceWei = new(big.Int).Add(prepared.header.BaseFee, prepared.fee)
			facts.FeeCapWei = snClaimRelayFeeCap(prepared.header.BaseFee, prepared.fee)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if facts.AlphaPriceWei.Sign() <= 0 {
		return nil, errors.New("claim relay: the alpha price is zero")
	}
	if facts.FeeCapWei.Sign() <= 0 {
		return nil, errors.New("claim relay: the fee cap is zero")
	}
	return facts, nil
}

// snClaimRelayReceipt is what one finalized transaction spent and did.
type snClaimRelayReceipt struct {
	spentWei *big.Int
	outcome  *snClaimRelayOutcome
	// the accounts its calldata names, by item
	accounts []string
}

func (self *snClaimRelayCoreChain) Spend(ctx context.Context, since time.Time) (*snClaimRelaySpend, error) {
	genesisHash := "0x" + hex.EncodeToString(self.cfg.GenesisHash[:])
	from := strings.ToLower(self.relayer.address.Hex())
	spend := &snClaimRelaySpend{SpentWei: new(big.Int), RevertedAccounts: map[string]string{}}
	for _, intent := range model.GetStTransactionIntentsSince(ctx, self.cfg.ChainId, genesisHash, from, since.Add(-snClaimRelayPaidLookback)) {
		attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
		if len(attempts) == 0 {
			continue
		}
		var receipt *snClaimRelayReceipt
		switch intent.Status {
		case model.StTxFinalized, model.StTxReverted, model.StTxCanceled:
			var err error
			if receipt, err = self.receipt(ctx, intent, attempts); err != nil {
				// The next pass reads it again.
				glog.Infof("[sn]claim relay receipt of intent %s: %s\n", intent.IntentKey, err)
			}
		}
		spend.add(intent, attempts, receipt, !intent.CreateTime.Before(since))
	}
	return spend, nil
}

// add counts one of the relayer's intents. today says it was created since the
// start of the day Spend reports: only those spend, count and refuse for the day,
// while paid claims are read from the day before on. receipt is nil for an intent
// that is not final, or for a final one whose receipt could not be read, which
// then counts the most it could have cost. Intents arrive newest nonce first, so
// an account's first paid claim here is its latest. Pure, for the tests.
func (self *snClaimRelaySpend) add(intent *model.StTransactionIntent, attempts []*model.StTransactionAttempt, receipt *snClaimRelayReceipt, today bool) {
	switch intent.Status {
	case model.StTxSuperseded, model.StTxFailed, model.StTxInvalid:
		// no canonical transaction of this intent paid anything
		return
	case model.StTxFinalized, model.StTxReverted, model.StTxCanceled:
		if receipt == nil {
			// its receipt is unavailable, so the most it could have cost stands in
			if today {
				self.SpentWei.Add(self.SpentWei, snClaimRelayMostAttemptCost(attempts))
			}
			return
		}
	default:
		if today {
			self.SpentWei.Add(self.SpentWei, snClaimRelayMostAttemptCost(attempts))
		}
		return
	}
	if today {
		self.SpentWei.Add(self.SpentWei, receipt.spentWei)
	}
	if receipt.outcome == nil {
		return
	}
	if today {
		self.Griefed = self.Griefed || receipt.outcome.Griefed
	}
	for index, item := range receipt.outcome.Items {
		switch {
		case item.Paid:
			if today {
				self.ClaimsPaid += 1
			}
			if !slices.ContainsFunc(self.Paid, func(paid *snClaimRelayPaid) bool { return paid.NoId == item.NoId && paid.Index == item.Index }) {
				self.Paid = append(self.Paid, &snClaimRelayPaid{NoId: item.NoId, Index: item.Index, TxHash: receipt.outcome.TxHash, Cumulative: item.Cumulative, AmountRao: item.AmountRao})
			}
		case today && !item.NotAttempted && index < len(receipt.accounts):
			self.RevertedAccounts[receipt.accounts[index]] = item.Reason
		}
	}
}

// The most any of an intent's same-nonce attempts can cost.
func snClaimRelayMostAttemptCost(attempts []*model.StTransactionAttempt) *big.Int {
	most := new(big.Int)
	for _, attempt := range attempts {
		price := stStoredBigInt(attempt.GasFeeCap)
		if price == nil {
			price = stStoredBigInt(attempt.GasPrice)
		}
		if price == nil {
			continue
		}
		if cost := snClaimRelayLiability(attempt.GasLimit, price); 0 < cost.Cmp(most) {
			most = cost
		}
	}
	return most
}

// The receipt of an intent's canonical attempt, once.
func (self *snClaimRelayCoreChain) receipt(ctx context.Context, intent *model.StTransactionIntent, attempts []*model.StTransactionAttempt) (*snClaimRelayReceipt, error) {
	if intent.CurrentTxHash == nil {
		return nil, errors.New("no canonical transaction")
	}
	txHash := *intent.CurrentTxHash
	self.stateLock.Lock()
	cached := self.txHashReceipts[txHash]
	self.stateLock.Unlock()
	if cached != nil {
		return cached, nil
	}
	var observed *stTransactionReceiptObservation
	err := self.client.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		var err error
		observed, err = self.client.readTransactionReceipt(ctx, client, common.HexToHash(txHash))
		return err
	})
	if err != nil {
		return nil, err
	}
	if observed == nil || observed.receipt == nil {
		return nil, errors.New("no receipt")
	}
	var attempt *model.StTransactionAttempt
	for _, candidate := range attempts {
		if strings.EqualFold(candidate.TxHash, txHash) {
			attempt = candidate
		}
	}
	if attempt == nil {
		return nil, errors.New("the canonical transaction is none of the intent's attempts")
	}
	receipt := snClaimRelayReadReceipt(self.relayer.address, self.cfg.VaultV2, self.batcher, intent, attempt, observed.receipt)
	self.stateLock.Lock()
	self.txHashReceipts[txHash] = receipt
	self.stateLock.Unlock()
	return receipt, nil
}

// snClaimRelayReadReceipt reads what a finalized attempt paid and did from its
// receipt. A cancellation only paid. Pure, for the tests.
func snClaimRelayReadReceipt(from common.Address, vault common.Address, batcher common.Address, intent *model.StTransactionIntent, attempt *model.StTransactionAttempt, receipt *types.Receipt) *snClaimRelayReceipt {
	price := receipt.EffectiveGasPrice
	if price == nil || price.Sign() == 0 {
		// A node that does not report it paid at most what was signed.
		if price = stStoredBigInt(attempt.GasPrice); price == nil {
			price = stStoredBigInt(attempt.GasFeeCap)
		}
	}
	result := &snClaimRelayReceipt{spentWei: snClaimRelayLiability(receipt.GasUsed, price)}
	if attempt.Kind == model.StTxAttemptCancellation {
		return result
	}
	claims, batch, err := snClaimRelayDecodeCalldata(intent.Calldata)
	if err != nil {
		// Only the relay writes this account's intents. Whatever this was, it was
		// paid for and it is not a claim.
		glog.Errorf("[sn]claim relay intent %s is not a claim: %s\n", intent.IntentKey, err)
		return result
	}
	for _, claim := range claims {
		result.accounts = append(result.accounts, snClaimRelayAccountKey(claim.NoId.Uint64(), claim.Account.Index.Uint64()))
	}
	outcome := &snClaimRelayOutcome{TxHash: receipt.TxHash.Hex(), GasUsed: receipt.GasUsed}
	result.outcome = outcome
	if receipt.Status != types.ReceiptStatusSuccessful {
		outcome.Reverted = true
		// All of its gas gone is a runtime refusal or a claim that ran out of gas
		// after its preflight passed, either of which is reason to stop.
		outcome.Griefed = attempt.GasLimit-attempt.GasLimit/64 <= receipt.GasUsed
		for range claims {
			outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{Reason: "the transaction reverted"})
		}
		return result
	}
	if !batch {
		outcome.Griefed = snClaimRelayGriefingGas < receipt.GasUsed
		outcome.Items = []*snClaimRelayItemOutcome{snClaimRelayItemFromEvent(receipt, vault, claims[0], from, nil)}
		return result
	}
	outcomes, err := onchain.ClaimBatchOutcomes(receipt, batcher, len(claims))
	if err != nil {
		for range claims {
			outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{Reason: "the batch's receipt does not prove its outcome"})
		}
		return result
	}
	attempted := uint64(0)
	for index, item := range outcomes {
		switch {
		case !item.Attempted:
			outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{NotAttempted: true})
		case item.Amount != nil:
			attempted += 1
			outcome.Items = append(outcome.Items, snClaimRelayItemFromEvent(receipt, vault, claims[index], batcher, item.Amount))
		default:
			attempted += 1
			// No revert data is a claim that ran out of its gas.
			outcome.Griefed = outcome.Griefed || item.Selector == [4]byte{}
			outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{Reason: fmt.Sprintf("the batch refused the claim with 0x%x", item.Selector)})
		}
	}
	if 0 < attempted && snClaimRelayGriefingGas < receipt.GasUsed/attempted {
		outcome.Griefed = true
	}
	return result
}

// A paid claim must have exactly its one Claimed event from vault2, naming the
// leaf's coldkey: this is what says a claim landed on the right account.
func snClaimRelayItemFromEvent(receipt *types.Receipt, vault common.Address, claim *stabi.STClaimBatcherClaim, caller common.Address, amount *big.Int) *snClaimRelayItemOutcome {
	var claimed *stabi.STSettlementVault2Claimed
	for _, log := range receipt.Logs {
		if log == nil || log.Address != vault {
			continue
		}
		event, err := stLedgerV2VaultAbi.UnpackClaimedEvent(log)
		if err != nil || event.NoId.Cmp(claim.NoId) != 0 || event.Index.Cmp(claim.Account.Index) != 0 {
			continue
		}
		if claimed != nil {
			return &snClaimRelayItemOutcome{Reason: "the receipt has two Claimed events of the account"}
		}
		claimed = event
	}
	if claimed == nil {
		return &snClaimRelayItemOutcome{Reason: "the receipt has no Claimed event of the account"}
	}
	if claimed.Coldkey != claim.Account.Coldkey || claimed.RootIndex.Cmp(claim.RootIndex) != 0 || claimed.Cumulative.Cmp(claim.Account.Cumulative) != 0 || claimed.Caller != caller || claimed.Amount.Sign() <= 0 || amount != nil && claimed.Amount.Cmp(amount) != 0 {
		return &snClaimRelayItemOutcome{Reason: "the Claimed event differs from the claim"}
	}
	// the account as the receipt's accounts name it, from the calldata the event matched
	return &snClaimRelayItemOutcome{Paid: true, AmountRao: claimed.Amount, NoId: claim.NoId.Uint64(), Index: claim.Account.Index.Uint64(), Cumulative: claimed.Cumulative}
}

var (
	snClaimRelayVaultClaim = func() abi.Method {
		parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
		if err != nil {
			panic(err)
		}
		return parsed.Methods["claim"]
	}()
	snClaimRelayBatcherClaimMany = func() abi.Method {
		parsed, err := stabi.STClaimBatcherMetaData.ParseABI()
		if err != nil {
			panic(err)
		}
		return parsed.Methods["claimMany"]
	}()
	snClaimRelayBatcherAbi = stabi.NewSTClaimBatcher()
)

// The claims of a claim's or a batch's calldata.
func snClaimRelayDecodeCalldata(calldata []byte) (claims []*stabi.STClaimBatcherClaim, batch bool, err error) {
	if len(calldata) < 4 {
		return nil, false, errors.New("short calldata")
	}
	switch {
	case bytes.Equal(calldata[:4], snClaimRelayVaultClaim.ID):
		values, err := snClaimRelayVaultClaim.Inputs.Unpack(calldata[4:])
		if err != nil {
			return nil, false, err
		}
		return []*stabi.STClaimBatcherClaim{{
			NoId:      abi.ConvertType(values[0], new(big.Int)).(*big.Int),
			RootIndex: abi.ConvertType(values[1], new(big.Int)).(*big.Int),
			Account:   *abi.ConvertType(values[2], new(stabi.LedgerTreeAccount)).(*stabi.LedgerTreeAccount),
			Proof:     *abi.ConvertType(values[3], new([]stabi.LedgerTreeProofNode)).(*[]stabi.LedgerTreeProofNode),
		}}, false, nil
	case bytes.Equal(calldata[:4], snClaimRelayBatcherClaimMany.ID):
		values, err := snClaimRelayBatcherClaimMany.Inputs.Unpack(calldata[4:])
		if err != nil {
			return nil, false, err
		}
		items := *abi.ConvertType(values[1], new([]stabi.STClaimBatcherClaim)).(*[]stabi.STClaimBatcherClaim)
		claims := make([]*stabi.STClaimBatcherClaim, len(items))
		for index := range items {
			claims[index] = &items[index]
		}
		return claims, true, nil
	}
	return nil, false, errors.New("not a claim or a claim batch")
}

// The claim's arguments, from the leaf's own coldkey and cumulative. vault2 pays
// the leaf's coldkey whoever sends it.
func snClaimRelayItem(claim *snClaimRelayClaim) stabi.STClaimBatcherClaim {
	account := claim.Account
	proof := make([]stabi.LedgerTreeProofNode, len(claim.Proof))
	for index, node := range claim.Proof {
		proof[index] = stabi.LedgerTreeProofNode{Hash: node.Hash, Sum: node.Sum.ToBig()}
	}
	return stabi.STClaimBatcherClaim{
		NoId:      stBig(account.NoId),
		RootIndex: stBig(account.RootIndex),
		Account:   stabi.LedgerTreeAccount{Index: stBig(account.Index), Coldkey: account.Coldkey, Cumulative: account.Cumulative.ToBig()},
		Proof:     proof,
	}
}

func snClaimRelayItems(claims []*snClaimRelayClaim) []stabi.STClaimBatcherClaim {
	items := make([]stabi.STClaimBatcherClaim, len(claims))
	for index, claim := range claims {
		items[index] = snClaimRelayItem(claim)
	}
	return items
}

func (self *snClaimRelayCoreChain) Claimed(ctx context.Context, account *snClaimRelayAccount) (*uint256.Int, error) {
	claimed, err := self.client.LedgerV2Claimed(ctx, account.NoId, account.Index)
	if err != nil {
		return nil, err
	}
	return &claimed, nil
}

func (self *snClaimRelayCoreChain) VerifyProof(ctx context.Context, account *snClaimRelayAccount, proof []merkle.LedgerNode) (bool, error) {
	key := snClaimRelayAccountKey(account.NoId, account.RootIndex)
	self.stateLock.Lock()
	root := self.rootIndexRoots[key]
	self.stateLock.Unlock()
	if root == nil {
		var err error
		if root, err = self.client.LedgerV2Root(ctx, account.NoId, account.RootIndex); err != nil {
			return false, err
		}
		self.stateLock.Lock()
		self.rootIndexRoots[key] = root
		self.stateLock.Unlock()
	}
	return merkle.VerifyLedgerProof(root.Root, &root.Total, root.LeafCount, account.Index, account.Coldkey, &account.Cumulative, proof), nil
}

func (self *snClaimRelayCoreChain) Claim(claim *snClaimRelayClaim) (common.Address, []byte) {
	item := snClaimRelayItem(claim)
	return self.cfg.VaultV2, stLedgerV2VaultAbi.PackClaim(item.NoId, item.RootIndex, item.Account, item.Proof)
}

func (self *snClaimRelayCoreChain) Estimate(ctx context.Context, claim *snClaimRelayClaim) (uint64, error) {
	to, calldata := self.Claim(claim)
	var estimate uint64
	err := self.client.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		var err error
		estimate, err = client.EstimateGas(ctx, ethereum.CallMsg{From: self.relayer.address, To: &to, Data: calldata})
		return err
	})
	return estimate, err
}

// Batching reports a batcher with exactly the release's code. A verified batcher
// is kept: it is immutable. Anything else is asked again, and claims go alone.
func (self *snClaimRelayCoreChain) Batching(ctx context.Context) bool {
	if self.batcher == (common.Address{}) {
		return false
	}
	self.stateLock.Lock()
	verified := self.batcherVerified
	self.stateLock.Unlock()
	if verified {
		return true
	}
	err := self.client.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		return onchain.VerifyClaimBatcher(ctx, client, self.batcher)
	})
	if err != nil {
		glog.Infof("[sn]claim relay claims go alone: %s\n", err)
		return false
	}
	self.stateLock.Lock()
	self.batcherVerified = true
	self.stateLock.Unlock()
	return true
}

func (self *snClaimRelayCoreChain) SizeBatch(ctx context.Context, claims []*snClaimRelayClaim, gasPerClaim uint64) (*snClaimRelayBatch, error) {
	if !self.Batching(ctx) {
		return nil, nil
	}
	var batch *snClaimRelayBatch
	err := self.client.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		count, calldata, gasLimit, err := onchain.SizeClaimBatch(ctx, client, self.relayer.address, self.batcher, self.cfg.VaultV2, snClaimRelayItems(claims), gasPerClaim)
		if err != nil {
			return err
		}
		batch = &snClaimRelayBatch{Count: count, To: self.batcher, Calldata: calldata, GasLimit: gasLimit}
		return nil
	})
	return batch, err
}

// snClaimRelayOperation names a transaction's logical intent by what it claims,
// so that a repeat finds the same intent. A retired intent cannot run again, so
// each try after one has its own name.
func snClaimRelayOperation(send *snClaimRelaySend, try int) string {
	if send.Batch {
		digest := sha256.Sum256(send.Calldata)
		return fmt.Sprintf("v2relaybatch:%x:%d", digest[:16], try)
	}
	account := send.Claims[0].Account
	// a claim's calldata follows its cumulative, and a root may raise it
	return fmt.Sprintf("v2relay:%d:%d:%d:%d", account.NoId, account.Index, account.RootIndex, try)
}

func (self *snClaimRelayCoreChain) Send(ctx context.Context, send *snClaimRelaySend) (*snClaimRelayOutcome, error) {
	self.client.claimRelay.plan(send.Calldata, send.GasLimit)
	defer self.client.claimRelay.unplan(send.Calldata)
	var logicalKey string
	hash, err := self.client.sendPrepared(ctx, self.relayer.key, func(ctx context.Context, _ *ethclient.Client) (string, common.Address, []byte, error) {
		for try := 0; ; try += 1 {
			operation := snClaimRelayOperation(send, try)
			key, err := stTransactionLogicalKey(self.cfg, operation)
			if err != nil {
				return "", common.Address{}, nil, err
			}
			existing := model.GetStTransactionIntent(ctx, key)
			if existing != nil && (existing.Status == model.StTxCanceled || existing.Status == model.StTxSuperseded || existing.Status == model.StTxFailed || existing.Status == model.StTxInvalid || !bytes.Equal(existing.Calldata, send.Calldata)) {
				continue
			}
			logicalKey = key
			return operation, send.To, send.Calldata, nil
		}
	})
	// The pass is ending. Whatever became of this transaction, the next pass
	// reconciles it.
	if ctx.Err() != nil && logicalKey != "" {
		return nil, errSnClaimRelayInFlight
	}
	var intent *model.StTransactionIntent
	if logicalKey != "" {
		intent = model.GetStTransactionIntent(ctx, logicalKey)
	}
	if intent == nil {
		// nothing was reserved: reconciliation or admission refused first
		return nil, errors.Join(errors.New("claim relay: no transaction was reserved"), err)
	}
	if err != nil && intent.Status != model.StTxReverted {
		if intent.Status == model.StTxFinalized {
			return nil, fmt.Errorf("claim relay: %w", err)
		}
		glog.Infof("[sn]claim relay %s: %s\n", hash, err)
		return nil, errSnClaimRelayInFlight
	}
	attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
	receipt, err := self.receipt(ctx, intent, attempts)
	if err != nil {
		// final, and Spend counts the most it can cost until its receipt reads
		glog.Infof("[sn]claim relay receipt: %s\n", err)
		return nil, errSnClaimRelayInFlight
	}
	if receipt.outcome == nil {
		return nil, errors.New("claim relay: the transaction was not a claim")
	}
	return receipt.outcome, nil
}

// snClaimRelayCoreLedger reads each of the operator's pools, its pool and its
// capture the flag pool: the pool's account index and its latest finalized
// root's tree. A coldkey in the index that the root does not hold at its
// index, which vault2 would refuse anyway, is left out and logged. A pool whose
// ledger cannot be read is left out of the pass and named, so that the other
// pools' accounts are still claimed, once another pool with a finalized root
// was read.
type snClaimRelayCoreLedger struct {
	cfg *StConfig
}

func (self *snClaimRelayCoreLedger) Accounts(ctx context.Context) (*snClaimRelayLedgerRead, error) {
	read := &snClaimRelayLedgerRead{Accounts: []*snClaimRelayAccount{}}
	pools := stLedgerV2OwnPools(self.cfg)
	var errs []error
	// the pools read that have a finalized root
	rooted := 0
	for _, noId := range pools {
		var poolAccounts []*snClaimRelayAccount
		hasRoot := false
		err := stLedgerV2Guard(func() (err error) {
			poolAccounts, hasRoot, err = self.poolAccounts(ctx, noId)
			return
		})
		if err != nil {
			read.Unavailable = append(read.Unavailable, noId)
			errs = append(errs, fmt.Errorf("pool %d: %w", noId, err))
			continue
		}
		if hasRoot {
			rooted += 1
		}
		read.Accounts = append(read.Accounts, poolAccounts...)
	}
	// A pool without a finalized root, like the capture the flag pool before
	// its first or with capture the flag off, holds nothing to claim. When it
	// is the only one read, the ledger is unavailable and the pass ends there,
	// as it did with the operator's pool alone.
	if 0 < len(read.Unavailable) && rooted == 0 {
		return nil, errors.Join(errs...)
	}
	for _, err := range errs {
		glog.Infof("[sn]claim relay ledger: %s\n", err)
	}
	if 0 < len(read.Unavailable) {
		// who may hold an account the pass cannot read: the coldkeys the account
		// index records there; unknown when the index cannot be read either
		coldkeys := map[[32]byte]bool{}
		if stLedgerV2Guard(func() error {
			for _, noId := range read.Unavailable {
				for _, account := range model.GetStLedgerV2Accounts(ctx, self.cfg.DeploymentKey(), noId) {
					coldkeys[account.Coldkey] = true
				}
			}
			return nil
		}) == nil {
			read.UnavailableColdkeys = coldkeys
		}
	}
	return read, nil
}

// The carry contract's coldkey a capture the flag ledger names, by ledger hash:
// a published header never changes.
var snClaimRelayCarryColdkeys sync.Map

// snClaimRelayCarryColdkey is the carry contract's coldkey that a capture the
// flag ledger's header names, nil for a ledger of another kind.
func snClaimRelayCarryColdkey(ctx context.Context, ledgerHash [32]byte) (*[32]byte, error) {
	if carry, ok := snClaimRelayCarryColdkeys.Load(ledgerHash); ok {
		return carry.(*[32]byte), nil
	}
	data, err := GetStLedgerV2Object(ctx, ledger.Ref(ledgerHash).String())
	if err != nil {
		return nil, err
	}
	var carry *[32]byte
	if header, err := ctf.DecodeHeader(data); err == nil {
		carry = &header.CarryColdkey
	}
	snClaimRelayCarryColdkeys.Store(ledgerHash, carry)
	return carry, nil
}

// One pool's accounts, empty until it has a finalized root, and whether it
// has one.
func (self *snClaimRelayCoreLedger) poolAccounts(ctx context.Context, noId uint64) ([]*snClaimRelayAccount, bool, error) {
	deploymentKey := self.cfg.DeploymentKey()
	root := model.GetLatestStLedgerV2FinalizedRoot(ctx, deploymentKey, noId)
	if root == nil || root.RootIndex == nil {
		return nil, false, nil
	}
	_, leaves, err := stLedgerV2AccountTree(ctx, root)
	if err != nil {
		return nil, true, err
	}
	// The carry contract's account in a capture the flag pool holds the pots:
	// its recycle claims it and moves the stake back onto the pool (sn
	// CAPTURETHEFLAG.md §4.6), and a claim of the relay's would only spend the
	// day's cap.
	var carry *[32]byte
	if _, ctfPool := amendment.CtfNoID(noId); ctfPool {
		if carry, err = snClaimRelayCarryColdkey(ctx, root.LedgerHash); err != nil {
			return nil, true, err
		}
	}
	claimed := model.GetStLedgerV2ClaimedCumulatives(ctx, deploymentKey, noId)
	indexAccounts := model.GetStLedgerV2Accounts(ctx, deploymentKey, noId)
	accounts := make([]*snClaimRelayAccount, 0, len(indexAccounts))
	for _, indexAccount := range indexAccounts {
		if uint64(len(leaves)) <= indexAccount.Index {
			continue
		}
		leaf := &leaves[indexAccount.Index]
		if leaf.Coldkey != indexAccount.Coldkey {
			glog.Errorf("[sn]claim relay account %d of pool %d is another coldkey in root %d\n", indexAccount.Index, noId, *root.RootIndex)
			continue
		}
		if carry != nil && leaf.Coldkey == *carry {
			continue
		}
		account := &snClaimRelayAccount{NoId: noId, Index: indexAccount.Index, RootIndex: *root.RootIndex, Coldkey: leaf.Coldkey, Cumulative: leaf.Cumulative}
		if mirrored, ok := claimed[indexAccount.Index]; ok && mirrored.Lt(&leaf.Cumulative) {
			account.Claimed = mirrored
		} else if ok {
			account.Claimed = leaf.Cumulative
		}
		accounts = append(accounts, account)
	}
	return accounts, true, nil
}

func (self *snClaimRelayCoreLedger) Proof(ctx context.Context, account *snClaimRelayAccount) ([]merkle.LedgerNode, error) {
	root := model.GetStLedgerV2FinalizedRoot(ctx, self.cfg.DeploymentKey(), account.NoId, account.RootIndex)
	if root == nil {
		return nil, fmt.Errorf("claim relay: root %d of pool %d is not mirrored", account.RootIndex, account.NoId)
	}
	tree, _, err := stLedgerV2AccountTree(ctx, root)
	if err != nil {
		return nil, err
	}
	return tree.Proof(account.Index)
}
