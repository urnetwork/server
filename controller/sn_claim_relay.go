package controller

// sn_claim_relay.go — the settlement v2 claim relay (apple BTEASY.md, "Claim
// relay"). vault2's claim is open to anyone and pays the leaf's coldkey
// whoever sends it, so URnetwork can claim for every consented network, wallet
// or local key, and pay the gas, with no tap and no permission. The relayer can
// redirect nothing.
//
// Policy (owner, question 1). Everything is in TAO value, since gas is paid in
// TAO:
//   - the relay's unit is a coldkey's account in one of the operator's pools:
//     its pool and, while capture the flag is on (stCtfPoolOn), its capture
//     the flag pool (sn CAPTURETHEFLAG.md §10);
//   - an account is claimed automatically once it is worth 0.0125 TAO. When one
//     of a coldkey's accounts is due, its other accounts above the transfer
//     floor of 0.0001 TAO go in the same claim batch: a capture the flag
//     account is often far below the threshold, and a batch adds about 150,000
//     gas a claim. Without a batcher each claim goes alone, and only due ones;
//   - "Claim now" claims every account of the coldkey above the transfer floor,
//     at most once per coldkey per 30 days. The 30 days start when the relay
//     pays a tapped claim it would not have sent by itself, so a tap that sends
//     nothing, or only moves an automatic claim ahead, uses nothing up;
//   - claims spend at most 0.01 TAO a day. Past that they wait for the next UTC
//     day, which costs nothing: v2 credit never expires.
//
// The receipts that prove each claim paid also name its transaction: the latest
// paid claim of each account is recorded, and the api names that transaction
// while it is still the account's latest claim (sn_claim_relay_api.go).
//
// Stops, all of them fail closed:
//   - no relayer key: nothing is read or sent, and the status says so. This is
//     the only gate of the relay; the capture the flag pool has its own,
//     stCtfPoolOn, and while it is off the relay neither reads nor claims it;
//   - a gas price over the ceiling;
//   - a claim's gas jumping, which is what griefing looks like: third parties
//     filling vault2's staking-hotkey list take a claim from about 150,000 gas
//     to about 1.26M. A preflight estimate over the limit stops the pass before
//     anything is sent, and a finalized claim that used over the limit or burned
//     its whole gas limit stops the rest of its UTC day;
//   - a balance that cannot cover the next transaction;
//   - any chain read failing.
//
// The relayer signs each claim at its estimate plus 20%, not at the 2,600,000
// other senders use (sn VERSION2-FINAL.md §8.1). That floor exists so that a
// claim survives a list lengthened after its estimate, and only the gas used is
// paid, but 2,600,000 gas at 5 gwei needs 0.013 TAO on hand and would burn it if
// the runtime refused the claim, which alone is over the day's cap. At the
// estimate plus 20% the most a transaction can cost is known before it is sent:
// its gas limit at its fee cap, raised by the engine's two fee replacements of
// 12.5%, which on the relay's client are never more (snClaimRelayWorstCase). The
// cap holds against that; a list lengthened after the estimate costs one
// reverted claim and trips the stop.
//
// The chain, the clock and the stores are injected. Nothing here keeps state of
// its own: what was spent comes from the durable transaction intents of the
// relayer's account, what was claimed from the chain, and a pass that dies is
// repeated by the next one (sn_claim_relay_chain.go, the task in
// taskworker/work/sn_claim_relay_work.go).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
)

const (
	// 0.0125 TAO, in rao of TAO: 1.9 α at sn's planning price, about 3 α today.
	snClaimRelayAutomaticTaoRao uint64 = 12_500_000
	// 0.0001 TAO, in rao of TAO: the runtime refuses a smaller transfer, and the
	// claim reverts (sn VERSION2-FINAL.md §7.3).
	snClaimRelayFloorTaoRao uint64 = 100_000
	// The floor is compared with a price read before the claim is included, so
	// a claim has to clear it by this much to be sent.
	snClaimRelayFloorMarginBps uint64 = 500

	// The gas a preflight estimate or a finalized claim may reach. A claim is
	// about 150,000 to 207,000 gas and about 1.26M with the list full; this is
	// the list at about 25 entries.
	snClaimRelayGriefingGas uint64 = 400_000

	snClaimRelayClaimNowPeriod = 30 * 24 * time.Hour

	// A claim's gas limit at sn's measured 155,000 gas, for judging the funds when
	// nothing is due to estimate.
	snClaimRelayPlanningGasLimit uint64 = 186_000

	// The most claims in one transaction.
	snClaimRelayBatchMaximum = onchain.ClaimBatchMaxClaims
	// Accounts verified against the chain in one pass, so that many refusals, or
	// a mirror far behind, cannot make a pass long. The next pass goes on.
	snClaimRelayPreflightsPerPass = 64
	// Requests read in one pass. Past this they wait for the next pass.
	snClaimRelayRequestsPerPass = 256
	// A claim whose preflight was refused is tried again after this. It cost
	// nothing, and most refusals clear.
	snClaimRelayRefusalRetry = 1 * time.Hour
	// Refusals carried in the status, the oldest dropped first.
	snClaimRelayRefusalsLimit = 1024

	// One pass sends at most one transaction. With more due the next pass comes
	// after the busy delay, otherwise after the idle delay.
	snClaimRelayIdleDelay = 5 * time.Minute
	snClaimRelayBusyDelay = 15 * time.Second
	// A status older than this means the relay is not running. It covers the
	// task's own limit, during which a pass may wait for one claim's finality.
	snClaimRelayStatusStale = 40 * time.Minute

	// Paid claims are read from the relayer's transactions of the day before too,
	// so a claim whose outcome was only final after midnight is still recorded.
	snClaimRelayPaidLookback = 24 * time.Hour
)

var (
	// 0.01 TAO a day, in wei: 1 TAO is 10^18 wei on the chain's EVM.
	snClaimRelayDailyCapWei = big.NewInt(10_000_000_000_000_000)
	// 10 gwei, twice the chain's usual 5. A claim's gas stays near a tenth of a
	// claim at the automatic threshold.
	snClaimRelayGasPriceCeilingWei = big.NewInt(10_000_000_000)

	snClaimRelayPriceScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

// Relay states, in the status and on the api. Only running and cap_reached have
// claims moving; the api groups the rest for the apps (snClaimRelayClassOf).
const (
	SnClaimRelayStateRunning           = "running"
	SnClaimRelayStateCapReached        = "cap_reached"
	SnClaimRelayStateInFlight          = "in_flight"
	SnClaimRelayStateNoKey             = "no_key"
	SnClaimRelayStateBadKey            = "bad_key"
	SnClaimRelayStateNotConfigured     = "not_configured"
	SnClaimRelayStateNotRunning        = "not_running"
	SnClaimRelayStateChainUnavailable  = "chain_unavailable"
	SnClaimRelayStateLedgerUnavailable = "ledger_unavailable"
	SnClaimRelayStateSendFailed        = "send_failed"
	SnClaimRelayStateLowBalance        = "low_balance"
	SnClaimRelayStateGasPriceHigh      = "gas_price_high"
	SnClaimRelayStateGriefingStop      = "griefing_stop"
)

var snClaimRelayStates = []string{
	SnClaimRelayStateRunning, SnClaimRelayStateCapReached, SnClaimRelayStateInFlight, SnClaimRelayStateNoKey,
	SnClaimRelayStateBadKey, SnClaimRelayStateNotConfigured, SnClaimRelayStateChainUnavailable, SnClaimRelayStateLedgerUnavailable,
	SnClaimRelayStateSendFailed, SnClaimRelayStateLowBalance, SnClaimRelayStateGasPriceHigh, SnClaimRelayStateGriefingStop,
}

var (
	errSnClaimRelayNoKey = errors.New("claim relay: no relayer key is configured")
	// A claim whose outcome is not known yet. The next pass finishes it first.
	errSnClaimRelayInFlight = errors.New("claim relay: a claim's outcome is not final yet")
)

// SnClaimRelayStatus is what the last pass found, for the api and the metrics.
// Amounts are decimal strings: wei for gas and balances, wei of TAO per α for
// the price.
type SnClaimRelayStatus struct {
	State           string                          `json:"state"`
	PassTime        time.Time                       `json:"pass_time"`
	Detail          string                          `json:"detail,omitempty"`
	RelayerAddress  string                          `json:"relayer_address,omitempty"`
	BalanceWei      string                          `json:"balance_wei,omitempty"`
	GasPriceWei     string                          `json:"gas_price_wei,omitempty"`
	AlphaPriceWei   string                          `json:"alpha_price_wei,omitempty"`
	SpentTodayWei   string                          `json:"spent_today_wei,omitempty"`
	ClaimsPaidToday int                             `json:"claims_paid_today"`
	ClaimsPending   int                             `json:"claims_pending"`
	OverThreshold   int                             `json:"over_threshold"`
	LastEstimateGas uint64                          `json:"last_estimate_gas,omitempty"`
	Refusals        map[string]*SnClaimRelayRefusal `json:"refusals,omitempty"`
	// More is due now, so the next pass should not wait.
	Again bool `json:"again,omitempty"`
	// Batching says the relay claims in batches, so a coldkey's accounts above
	// the floor go with any of its accounts that is due.
	Batching bool `json:"batching,omitempty"`
	// UnavailablePools are the operator's pools whose ledger the pass could not
	// read while it claimed the others, by no_id: at its ledger read, or for a
	// capture the flag pool at a claim's proof. Their accounts wait. A pass
	// that could read no pool with a finalized root ends ledger_unavailable
	// with none named here, as before the capture the flag pool, and the api
	// pauses every pool.
	UnavailablePools []uint64 `json:"unavailable_pools,omitempty"`
	// PoolsUnavailable counts the pools whose ledger the relay's latest read
	// could not read, for the metrics: this pass's, whether it claimed the
	// others or ended there, or, for a pass that ended before reading the
	// ledger, the previous pass's.
	PoolsUnavailable int `json:"pools_unavailable,omitempty"`
}

// SnClaimRelayRefusal is why one account's claim is waiting: until the next UTC
// day after it reverted, or for a while after its preflight was refused.
type SnClaimRelayRefusal struct {
	Time   time.Time `json:"time"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

func snClaimRelayAccountKey(noId uint64, index uint64) string {
	return fmt.Sprintf("%d/%d", noId, index)
}

// snClaimRelayAccount is one account of a pool's latest finalized root: the
// relay's unit is (coldkey, pool). Claimed starts as the mirror of vault2's
// Claimed events, which can only be behind; a claim is sent only after a
// finalized read of it.
type snClaimRelayAccount struct {
	NoId       uint64
	Index      uint64
	RootIndex  uint64
	Coldkey    [32]byte
	Cumulative uint256.Int
	Claimed    uint256.Int
}

func (self *snClaimRelayAccount) key() string {
	return snClaimRelayAccountKey(self.NoId, self.Index)
}

// The rao of α a claim would pay now.
func (self *snClaimRelayAccount) claimable() *uint256.Int {
	if !self.Claimed.Lt(&self.Cumulative) {
		return new(uint256.Int)
	}
	return new(uint256.Int).Sub(&self.Cumulative, &self.Claimed)
}

// snClaimRelayClaim is one account's claim, ready to send.
type snClaimRelayClaim struct {
	Account *snClaimRelayAccount
	Proof   []merkle.LedgerNode
	// the preflight's gas
	Estimate uint64
	// asked for with Claim now
	Requested bool
	// under the threshold and over the floor, claimed with a due account of
	// its coldkey in the same batch
	Companion bool
	// a tapped claim the relay would not have sent by itself: once it is paid,
	// the coldkey's Claim now limit starts
	UnderThreshold bool
}

// snClaimRelayFacts are the live chain values one pass decides with.
type snClaimRelayFacts struct {
	BalanceWei *big.Int
	// what a transaction pays per gas now
	GasPriceWei *big.Int
	// the most a transaction signed now can pay per gas
	FeeCapWei *big.Int
	// wei of TAO per α, the runtime's own price
	AlphaPriceWei *big.Int
}

// snClaimRelaySpend is what the relayer's account spent in one UTC day: what its
// finalized transactions paid, and the most its unfinished ones can.
type snClaimRelaySpend struct {
	SpentWei   *big.Int
	ClaimsPaid int
	// a finalized claim's gas jumped, or a claim burned its whole gas limit
	Griefed bool
	// accounts whose claim reverted, by snClaimRelayAccountKey
	RevertedAccounts map[string]string
	// the claims its finalized transactions paid, from the day before on: the
	// latest of each account only, newest first
	Paid []*snClaimRelayPaid
}

// snClaimRelayPaid is one claim a finalized transaction of the relayer paid, as
// its receipt proves it.
type snClaimRelayPaid struct {
	NoId   uint64
	Index  uint64
	TxHash string
	// what vault2 now records as the account's claimed
	Cumulative *big.Int
	AmountRao  *big.Int
}

// snClaimRelayBatch is a batch sized through the claim batcher: the first Count
// claims it was offered.
type snClaimRelayBatch struct {
	Count    int
	To       common.Address
	Calldata []byte
	GasLimit uint64
}

// snClaimRelaySend is the one transaction of a pass.
type snClaimRelaySend struct {
	To       common.Address
	Calldata []byte
	GasLimit uint64
	Claims   []*snClaimRelayClaim
	Batch    bool
}

// snClaimRelayOutcome is one finalized transaction's result per claim, in the
// order of the send's claims.
type snClaimRelayOutcome struct {
	TxHash   string
	GasUsed  uint64
	Griefed  bool
	Reverted bool
	Items    []*snClaimRelayItemOutcome
}

type snClaimRelayItemOutcome struct {
	Paid      bool
	AmountRao *big.Int
	// a paid claim's account, and the cumulative vault2 now records as claimed
	NoId       uint64
	Index      uint64
	Cumulative *big.Int
	// not attempted: the batch's gas ran out before it; it is simply due again
	NotAttempted bool
	Reason       string
}

// snClaimRelayChain is everything the relay reads from and sends to the chain.
// sn_claim_relay_chain.go implements it over the durable transaction engine.
type snClaimRelayChain interface {
	// Reconcile finishes the relayer's unfinished transactions, in nonce order.
	Reconcile(ctx context.Context) error
	Facts(ctx context.Context) (*snClaimRelayFacts, error)
	// Spend reports the transactions the relayer's account created since the
	// start of a UTC day, and the claims they and those of the day before paid.
	Spend(ctx context.Context, since time.Time) (*snClaimRelaySpend, error)
	// Claimed reads vault2's claimed at the finalized head.
	Claimed(ctx context.Context, account *snClaimRelayAccount) (*uint256.Int, error)
	// VerifyProof checks an account and its proof against vault2's root.
	VerifyProof(ctx context.Context, account *snClaimRelayAccount, proof []merkle.LedgerNode) (bool, error)
	// Estimate preflights one claim; an error is its refusal.
	Estimate(ctx context.Context, claim *snClaimRelayClaim) (uint64, error)
	// Claim is the single claim's transaction.
	Claim(claim *snClaimRelayClaim) (common.Address, []byte)
	// Batching reports a usable batcher.
	Batching(ctx context.Context) bool
	// SizeBatch sizes a batch of claims, each given gasPerClaim. Nil without a
	// usable batcher.
	SizeBatch(ctx context.Context, claims []*snClaimRelayClaim, gasPerClaim uint64) (*snClaimRelayBatch, error)
	// Send signs and sends the transaction and waits for its finality. A nil
	// outcome with errSnClaimRelayInFlight leaves it to Reconcile.
	Send(ctx context.Context, send *snClaimRelaySend) (*snClaimRelayOutcome, error)
}

// snClaimRelayLedger reads the accounts of the latest finalized root of each of
// the operator's pools, each pool on its own.
type snClaimRelayLedger interface {
	// Accounts are each pool's accounts once it has a finalized, recorded
	// root, by pool and index. A pool whose ledger cannot be read is left out
	// and named in the read; err says that no pool with a finalized root
	// could be read, and the read still names the pools that could not be.
	Accounts(ctx context.Context) (*snClaimRelayLedgerRead, error)
	Proof(ctx context.Context, account *snClaimRelayAccount) ([]merkle.LedgerNode, error)
}

// snClaimRelayLedgerRead is one pass's read of the operator's pools.
type snClaimRelayLedgerRead struct {
	Accounts []*snClaimRelayAccount
	// the pools whose ledger could not be read, by no_id
	Unavailable []uint64
	// the coldkeys the account index records in those pools; nil when the
	// index could not be read either, so that any coldkey may hold one there
	UnavailableColdkeys map[[32]byte]bool
}

// mayHold reports whether a coldkey may have an account the pass could not
// read.
func (self *snClaimRelayLedgerRead) mayHold(coldkey [32]byte) bool {
	if len(self.Unavailable) == 0 {
		return false
	}
	return self.UnavailableColdkeys == nil || self.UnavailableColdkeys[coldkey]
}

// snClaimRelayRequest is one unfinished Claim now.
type snClaimRelayRequest struct {
	Coldkey [32]byte
	Time    time.Time
}

// snClaimRelayRequests holds Claim now's requests and its limit.
type snClaimRelayRequests interface {
	List(ctx context.Context, limit int) []*snClaimRelayRequest
	// Finish removes a request. startLimit starts the coldkey's 30-day limit: the
	// relay paid a tapped claim it would not have sent by itself. A request with
	// nothing sent for it leaves the coldkey free to tap again.
	Finish(ctx context.Context, coldkey [32]byte, startLimit bool)
	// Limit starts the coldkey's 30-day limit, and keeps a limit already
	// running, while its request stays for its other accounts.
	Limit(ctx context.Context, coldkey [32]byte)
}

// snClaimRelayClaimRecords keeps each account's latest paid claim for the api.
type snClaimRelayClaimRecords interface {
	// Record replaces the account's record with this claim.
	Record(ctx context.Context, paid *snClaimRelayPaid)
}

// snClaimRelaySigner is the configured relayer, without its key: the policy never
// needs the key, only that there is one.
type snClaimRelaySigner struct {
	Address common.Address
}

type snClaimRelay struct {
	now          func() time.Time
	signer       func() (*snClaimRelaySigner, error)
	chain        func(signer *snClaimRelaySigner) (snClaimRelayChain, error)
	ledger       snClaimRelayLedger
	requests     snClaimRelayRequests
	claimRecords snClaimRelayClaimRecords
	lastStatus   func(ctx context.Context) *SnClaimRelayStatus
	publish      func(ctx context.Context, status *SnClaimRelayStatus)
	claimResults func(result string, count int)
	banned       func(coldkey [32]byte) bool
}

// Records each account's latest paid claim the spend names, for the api.
func (self *snClaimRelay) recordPaid(ctx context.Context, spend *snClaimRelaySpend) {
	if self.claimRecords == nil {
		return
	}
	for _, paid := range spend.Paid {
		self.claimRecords.Record(ctx, paid)
	}
}

func snClaimRelayDayStart(now time.Time) time.Time {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// The TAO value, in rao of TAO, of an amount of α in rao at a price in wei of
// TAO per α.
func snClaimRelayValueTaoRao(amount *uint256.Int, alphaPriceWei *big.Int) *big.Int {
	if alphaPriceWei == nil {
		return new(big.Int)
	}
	value := new(big.Int).Mul(amount.ToBig(), alphaPriceWei)
	return value.Quo(value, snClaimRelayPriceScale)
}

func snClaimRelayClearsFloor(value *big.Int) bool {
	least := new(big.Int).SetUint64(snClaimRelayFloorTaoRao * (10_000 + snClaimRelayFloorMarginBps) / 10_000)
	return 0 <= value.Cmp(least)
}

func snClaimRelayOverThreshold(value *big.Int) bool {
	return 0 <= value.Cmp(new(big.Int).SetUint64(snClaimRelayAutomaticTaoRao))
}

// The α, in rao, worth a value in rao of TAO, rounded up, for the apps to show.
// Zero without a price.
func snClaimRelayAlphaOf(valueTaoRao uint64, alphaPriceWei *big.Int) *big.Int {
	if alphaPriceWei == nil || alphaPriceWei.Sign() <= 0 {
		return new(big.Int)
	}
	amount := new(big.Int).Mul(new(big.Int).SetUint64(valueTaoRao), snClaimRelayPriceScale)
	amount.Add(amount, new(big.Int).Sub(alphaPriceWei, big.NewInt(1)))
	return amount.Quo(amount, alphaPriceWei)
}

// A gas limit of an estimate: plus 20%, as the engine's.
func snClaimRelayGasLimit(estimate uint64) uint64 {
	return estimate + (estimate+4)/5
}

func snClaimRelayLiability(gasLimit uint64, feeCapWei *big.Int) *big.Int {
	return new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCapWei)
}

// The most a transaction the relay signs now can cost: its gas limit at its fee
// cap, with the engine's two same-nonce replacements of 12.5% each, rounded up.
// The relay's replacements are never priced higher than that (st_controller.go
// transactionEnvelope), and one transaction is unfinished at a time.
func snClaimRelayWorstCase(gasLimit uint64, feeCapWei *big.Int) *big.Int {
	most := new(big.Int).Mul(snClaimRelayLiability(gasLimit, feeCapWei), big.NewInt(81))
	most.Add(most, big.NewInt(63))
	return most.Quo(most, big.NewInt(64))
}

func snClaimRelayMin(values ...*big.Int) *big.Int {
	least := values[0]
	for _, value := range values[1:] {
		if value.Cmp(least) < 0 {
			least = value
		}
	}
	return least
}

// pass decides and sends at most one transaction, and reports what it found.
func (self *snClaimRelay) pass(ctx context.Context) *SnClaimRelayStatus {
	now := self.now().UTC()
	status := &SnClaimRelayStatus{State: SnClaimRelayStateRunning, PassTime: now}
	if previous := self.lastStatus(ctx); previous != nil {
		status.Refusals = snClaimRelayLiveRefusals(previous.Refusals, now)
		// what the latest read found, until this pass reads the ledger; a
		// status written before the count was kept names only its pools
		status.PoolsUnavailable = max(previous.PoolsUnavailable, len(previous.UnavailablePools))
	}
	self.run(ctx, now, status)
	// A pool that cannot be read is a fault even while the others are claimed,
	// so a pass that would end running says the ledger is unavailable, as one
	// that could read no pool does, and names the pools.
	if status.State == SnClaimRelayStateRunning && 0 < len(status.UnavailablePools) {
		status.State = SnClaimRelayStateLedgerUnavailable
		status.Detail = fmt.Sprintf("the ledger of pools %v cannot be read; the other pools are claimed", status.UnavailablePools)
	}
	snClaimRelayLimitRefusals(status)
	self.publish(ctx, status)
	return status
}

// Fills the status; every return has set its state.
func (self *snClaimRelay) run(ctx context.Context, now time.Time, status *SnClaimRelayStatus) {
	signer, err := self.signer()
	if errors.Is(err, errSnClaimRelayNoKey) {
		status.State = SnClaimRelayStateNoKey
		return
	}
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateBadKey, err.Error()
		return
	}
	status.RelayerAddress = strings.ToLower(signer.Address.Hex())
	chain, err := self.chain(signer)
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateNotConfigured, err.Error()
		return
	}

	// An unfinished transaction owns the next nonce, and its cost is not known.
	if err := chain.Reconcile(ctx); err != nil {
		status.State, status.Detail = SnClaimRelayStateInFlight, err.Error()
		return
	}
	facts, err := chain.Facts(ctx)
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
		return
	}
	status.BalanceWei = facts.BalanceWei.String()
	status.GasPriceWei = facts.GasPriceWei.String()
	status.AlphaPriceWei = facts.AlphaPriceWei.String()
	spend, err := chain.Spend(ctx, snClaimRelayDayStart(now))
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
		return
	}
	self.applySpend(status, spend, now)
	// what a transaction reconciled above paid, whatever this pass does next
	self.recordPaid(ctx, spend)

	read, err := self.ledger.Accounts(ctx)
	if read != nil {
		// whether or not the pass goes on with the other pools
		status.PoolsUnavailable = len(read.Unavailable)
	}
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateLedgerUnavailable, err.Error()
		return
	}
	if 0 < len(read.Unavailable) {
		status.UnavailablePools = read.Unavailable
		glog.Infof("[sn]claim relay pools %v are unavailable this pass; the others are claimed\n", read.Unavailable)
	}
	batching := chain.Batching(ctx)
	status.Batching = batching
	// A request ends once every account of its coldkey is settled, whatever
	// the pass does next.
	taps := newSnClaimRelayTaps()
	defer self.finishTaps(ctx, taps)
	groups := self.candidates(ctx, read, facts, batching, taps, status)

	budget := new(big.Int).Sub(snClaimRelayDailyCapWei, spend.SpentWei)
	planned := snClaimRelayWorstCase(snClaimRelayPlanningGasLimit, facts.FeeCapWei)
	switch {
	case spend.Griefed:
		status.State = SnClaimRelayStateGriefingStop
		return
	case 0 < facts.GasPriceWei.Cmp(snClaimRelayGasPriceCeilingWei):
		status.State = SnClaimRelayStateGasPriceHigh
		return
	case facts.BalanceWei.Cmp(planned) < 0:
		// Whatever is due, and whatever is tapped next, cannot be sent.
		status.State = SnClaimRelayStateLowBalance
		return
	case budget.Cmp(planned) < 0:
		status.State = SnClaimRelayStateCapReached
		return
	case len(groups) == 0:
		return
	}

	send, more := self.prepare(ctx, chain, groups, facts, budget, batching, now, taps, status)
	if send == nil {
		return
	}

	// A pass may wait minutes for finality; the status stays fresh meanwhile.
	self.publish(ctx, status)
	outcome, err := chain.Send(ctx, send)
	if errors.Is(err, errSnClaimRelayInFlight) {
		status.State, status.Detail = SnClaimRelayStateInFlight, err.Error()
		return
	}
	if err != nil {
		status.State, status.Detail = SnClaimRelayStateSendFailed, err.Error()
		return
	}
	self.applyOutcome(ctx, send, outcome, now, taps, status)

	if spend, err = chain.Spend(ctx, snClaimRelayDayStart(now)); err != nil {
		status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
		return
	}
	self.applySpend(status, spend, now)
	self.recordPaid(ctx, spend)
	if spend.Griefed || outcome.Griefed {
		status.State = SnClaimRelayStateGriefingStop
		return
	}
	status.Again = more || 1 < len(groups)
}

// snClaimRelayTaps follows one pass's Claim now requests to their end. A request
// is done once every account of its coldkey is settled: paid, by the relay or
// anyone, or under the floor. Its coldkey's limit starts once the relay pays a
// tapped claim it would not have sent by itself.
type snClaimRelayTaps struct {
	// the accounts of each requested coldkey still to settle, by key
	open map[[32]byte]map[string]bool
	// the relay paid a tapped claim of the coldkey it would not have sent by
	// itself
	limit map[[32]byte]bool
	// the coldkey may have an account in a pool the pass could not read, so
	// its request does not end
	blocked map[[32]byte]bool
}

func newSnClaimRelayTaps() *snClaimRelayTaps {
	return &snClaimRelayTaps{open: map[[32]byte]map[string]bool{}, limit: map[[32]byte]bool{}, blocked: map[[32]byte]bool{}}
}

// request follows a requested coldkey through the pass.
func (self *snClaimRelayTaps) request(coldkey [32]byte, blocked bool) {
	if self.open[coldkey] == nil {
		self.open[coldkey] = map[string]bool{}
	}
	self.blocked[coldkey] = blocked
}

func (self *snClaimRelayTaps) add(claim *snClaimRelayClaim) {
	self.open[claim.Account.Coldkey][claim.Account.key()] = true
}

func (self *snClaimRelayTaps) settle(claim *snClaimRelayClaim) {
	if claim.Requested {
		delete(self.open[claim.Account.Coldkey], claim.Account.key())
	}
}

func (self *snClaimRelayTaps) paid(claim *snClaimRelayClaim) {
	if claim.Requested {
		self.settle(claim)
		if claim.UnderThreshold {
			self.limit[claim.Account.Coldkey] = true
		}
	}
}

// Ends each request whose accounts are all settled, starting its coldkey's limit
// when the relay paid a tapped claim it would not have sent by itself. A request
// with an account still to claim stays, and its coldkey's limit runs from the
// first such payment.
func (self *snClaimRelay) finishTaps(ctx context.Context, taps *snClaimRelayTaps) {
	for coldkey, open := range taps.open {
		switch {
		case len(open) == 0 && !taps.blocked[coldkey]:
			self.requests.Finish(ctx, coldkey, taps.limit[coldkey])
		case taps.limit[coldkey]:
			self.requests.Limit(ctx, coldkey)
		}
	}
}

// Records the day's spend in the status. A claim that reverted waits for the next
// UTC day: it cost real gas, and it passed its preflight.
func (self *snClaimRelay) applySpend(status *SnClaimRelayStatus, spend *snClaimRelaySpend, now time.Time) {
	status.SpentTodayWei = spend.SpentWei.String()
	status.ClaimsPaidToday = spend.ClaimsPaid
	for key, reason := range spend.RevertedAccounts {
		if status.Refusals == nil {
			status.Refusals = map[string]*SnClaimRelayRefusal{}
		}
		status.Refusals[key] = &SnClaimRelayRefusal{Time: now, Until: snClaimRelayDayStart(now).Add(24 * time.Hour), Reason: reason}
	}
}

// The claims of a pass, in groups that go into one transaction together: first
// each Claim now, oldest first, with every account of its coldkey above the
// floor; then every account over the threshold, largest first, each with the
// other accounts of its coldkey above the floor when the relay batches, since
// one of them is due. A request with nothing above the floor is given back, so
// a claim under it is never sent.
func (self *snClaimRelay) candidates(ctx context.Context, read *snClaimRelayLedgerRead, facts *snClaimRelayFacts, batching bool, taps *snClaimRelayTaps, status *SnClaimRelayStatus) [][]*snClaimRelayClaim {
	// a coldkey's accounts, by pool and index as the ledger reads them
	coldkeyAccounts := map[[32]byte][]*snClaimRelayAccount{}
	for _, account := range read.Accounts {
		coldkeyAccounts[account.Coldkey] = append(coldkeyAccounts[account.Coldkey], account)
	}
	over := func(account *snClaimRelayAccount) bool {
		return snClaimRelayOverThreshold(snClaimRelayValueTaoRao(account.claimable(), facts.AlphaPriceWei))
	}
	clearsFloor := func(account *snClaimRelayAccount) bool {
		claimable := account.claimable()
		return !claimable.IsZero() && snClaimRelayClearsFloor(snClaimRelayValueTaoRao(claimable, facts.AlphaPriceWei))
	}
	placed := map[string]bool{}
	groups := [][]*snClaimRelayClaim{}
	for _, request := range self.requests.List(ctx, snClaimRelayRequestsPerPass) {
		if self.banned(request.Coldkey) {
			self.requests.Finish(ctx, request.Coldkey, false)
			continue
		}
		blocked := read.mayHold(request.Coldkey)
		taps.request(request.Coldkey, blocked)
		group := []*snClaimRelayClaim{}
		for _, account := range coldkeyAccounts[request.Coldkey] {
			// paid, by this relay or anyone else, or under the floor
			if placed[account.key()] || !clearsFloor(account) {
				continue
			}
			placed[account.key()] = true
			claim := &snClaimRelayClaim{Account: account, Requested: true}
			taps.add(claim)
			group = append(group, claim)
		}
		if len(group) == 0 {
			if !blocked {
				self.requests.Finish(ctx, request.Coldkey, false)
			}
			continue
		}
		// a due account first, so that a claim that has to go alone is one
		// the relay would have sent by itself
		slices.SortStableFunc(group, func(a, b *snClaimRelayClaim) int {
			switch {
			case over(a.Account) && !over(b.Account):
				return -1
			case !over(a.Account) && over(b.Account):
				return 1
			}
			return 0
		})
		groups = append(groups, group)
	}
	due := []*snClaimRelayAccount{}
	for _, account := range read.Accounts {
		if placed[account.key()] || self.banned(account.Coldkey) {
			continue
		}
		if over(account) {
			due = append(due, account)
		}
	}
	slices.SortStableFunc(due, func(a, b *snClaimRelayAccount) int {
		if c := b.claimable().Cmp(a.claimable()); c != 0 {
			return c
		}
		if a.NoId != b.NoId {
			return cmp.Compare(a.NoId, b.NoId)
		}
		return cmp.Compare(a.Index, b.Index)
	})
	status.OverThreshold = len(due)
	for _, account := range due {
		if placed[account.key()] {
			// already in the group of another of its coldkey's accounts
			continue
		}
		placed[account.key()] = true
		group := []*snClaimRelayClaim{{Account: account}}
		if batching {
			for _, other := range coldkeyAccounts[account.Coldkey] {
				if placed[other.key()] || !clearsFloor(other) {
					continue
				}
				placed[other.key()] = true
				group = append(group, &snClaimRelayClaim{Account: other, Companion: !over(other)})
			}
		}
		groups = append(groups, group)
	}
	status.ClaimsPending = 0
	for _, group := range groups {
		status.ClaimsPending += len(group)
	}
	return groups
}

// prepare verifies the groups against the chain until it has a transaction. The
// claims it refuses leave the rest alone, and a group goes whole into one
// transaction or waits for the next pass. A nil send means the status says why
// nothing is sent. more reports claims left over for the next pass.
func (self *snClaimRelay) prepare(ctx context.Context, chain snClaimRelayChain, groups [][]*snClaimRelayClaim, facts *snClaimRelayFacts, budget *big.Int, batching bool, now time.Time, taps *snClaimRelayTaps, status *SnClaimRelayStatus) (send *snClaimRelaySend, more bool) {
	funds := snClaimRelayMin(budget, facts.BalanceWei)
	claimsPerTransaction := 1
	if batching {
		claimsPerTransaction = snClaimRelayBatchMaximum
	}
	kept := []*snClaimRelayClaim{}
	// where each kept group ends in kept
	groupEnds := []int{}
	estimates := uint64(0)
	preflights := 0
	for index, group := range groups {
		if snClaimRelayPreflightsPerPass <= preflights {
			more = true
			break
		}
		// Without a batcher a transaction holds one claim, and a pass
		// estimates no more than the claim it sends.
		most := len(group)
		if !batching {
			most = 1
		}
		verified, rest, stop := self.verify(ctx, chain, group, most, facts, now, &preflights, taps, status)
		if stop {
			return nil, false
		}
		// the group's claims past most go in a later pass
		more = more || rest
		if len(verified) == 0 {
			continue
		}
		if 0 < len(kept) && claimsPerTransaction < len(kept)+len(verified) {
			more = true
			break
		}
		// The first claim has to fit by the most it can cost. The batch is sized
		// from what the others should cost, and checked as a whole below.
		if len(kept) == 0 {
			liability := snClaimRelayWorstCase(snClaimRelayGasLimit(verified[0].Estimate), facts.FeeCapWei)
			if 0 < liability.Cmp(budget) {
				status.State = SnClaimRelayStateCapReached
				return nil, false
			}
			if 0 < liability.Cmp(facts.BalanceWei) {
				status.State = SnClaimRelayStateLowBalance
				return nil, false
			}
		}
		kept = append(kept, verified...)
		groupEnds = append(groupEnds, len(kept))
		for _, claim := range verified {
			estimates += claim.Estimate
		}
		if claimsPerTransaction <= len(kept) || 0 < snClaimRelayLiability(estimates, facts.FeeCapWei).Cmp(funds) {
			more = more || index+1 < len(groups)
			break
		}
	}
	if len(kept) == 0 {
		return nil, more
	}
	send = self.transaction(ctx, chain, kept, groupEnds, facts, funds)
	return send, more || len(send.Claims) < len(kept)
}

// verify reads up to most of a group's claims against the chain and returns
// those that can be sent, each with its proof and estimate; rest says claims
// past most were left for a later pass. A companion goes only with a due
// account of its coldkey, so without one the group sends nothing. stop says the
// pass ends, and the status says why.
func (self *snClaimRelay) verify(ctx context.Context, chain snClaimRelayChain, group []*snClaimRelayClaim, most int, facts *snClaimRelayFacts, now time.Time, preflights *int, taps *snClaimRelayTaps, status *SnClaimRelayStatus) (verified []*snClaimRelayClaim, rest bool, stop bool) {
	for _, candidate := range group {
		if most <= len(verified) {
			rest = true
			break
		}
		account := candidate.Account
		if refusal := status.Refusals[account.key()]; refusal != nil && now.Before(refusal.Until) {
			continue
		}
		if slices.Contains(status.UnavailablePools, account.NoId) {
			// its pool's tree could not be read earlier in this pass
			continue
		}
		*preflights += 1
		claimed, err := chain.Claimed(ctx, account)
		if err != nil {
			status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
			return nil, false, true
		}
		// The mirror can only be behind: this is the claim's real size.
		read := *account
		read.Claimed = *claimed
		claim := &snClaimRelayClaim{Account: &read, Requested: candidate.Requested, Companion: candidate.Companion}
		value := snClaimRelayValueTaoRao(read.claimable(), facts.AlphaPriceWei)
		switch {
		case read.claimable().IsZero():
			taps.settle(claim)
			continue
		case (candidate.Requested || candidate.Companion) && !snClaimRelayClearsFloor(value):
			taps.settle(claim)
			continue
		case !candidate.Requested && !candidate.Companion && !snClaimRelayOverThreshold(value):
			continue
		}
		claim.Proof, err = self.ledger.Proof(ctx, &read)
		if err != nil {
			if _, ctfPool := amendment.CtfNoID(account.NoId); ctfPool {
				// A capture the flag pool whose tree cannot be read now waits
				// as one the ledger read could not read: the operator's pool
				// is still claimed, and its accounts read on the api as they
				// would without that pool (owner, 2026-11-11).
				snClaimRelayPoolUnavailable(status, account.NoId, err)
				continue
			}
			status.State, status.Detail = SnClaimRelayStateLedgerUnavailable, err.Error()
			return nil, false, true
		}
		valid, err := chain.VerifyProof(ctx, &read, claim.Proof)
		if err != nil {
			status.State, status.Detail = SnClaimRelayStateChainUnavailable, err.Error()
			return nil, false, true
		}
		if !valid {
			self.refuse(&read, "the proof does not verify against vault2's root", now, snClaimRelayRefusalRetry, status)
			continue
		}
		estimate, err := chain.Estimate(ctx, claim)
		if err != nil {
			self.refuse(&read, stLedgerV2RevertName(err), now, snClaimRelayRefusalRetry, status)
			continue
		}
		status.LastEstimateGas = estimate
		if snClaimRelayGriefingGas < estimate {
			status.State = SnClaimRelayStateGriefingStop
			status.Detail = fmt.Sprintf("a claim's preflight estimated %d gas, over %d", estimate, snClaimRelayGriefingGas)
			return nil, false, true
		}
		claim.Estimate = estimate
		verified = append(verified, claim)
	}
	if !slices.ContainsFunc(verified, func(claim *snClaimRelayClaim) bool { return !claim.Companion }) {
		return nil, rest, false
	}
	// The group's first claim is the one that goes alone when a batch cannot,
	// so it is never a companion.
	slices.SortStableFunc(verified, func(a, b *snClaimRelayClaim) int {
		switch {
		case !a.Companion && b.Companion:
			return -1
		case a.Companion && !b.Companion:
			return 1
		}
		return 0
	})
	return verified, rest, false
}

// transaction is a batch of the kept claims when a batcher is usable and at
// least two fit the funds, or the first alone. A batch ends where a group ends,
// so that a coldkey's claims go together or wait together, and a claim that has
// to go alone is the first of its group: a due or tapped account, never a
// companion. A batch names its gas limit: the most it can cost is checked
// against the funds before it is sent.
func (self *snClaimRelay) transaction(ctx context.Context, chain snClaimRelayChain, kept []*snClaimRelayClaim, groupEnds []int, facts *snClaimRelayFacts, funds *big.Int) *snClaimRelaySend {
	// the longest run of whole groups of at most count claims
	whole := func(count int) int {
		end := 0
		for _, groupEnd := range groupEnds {
			if groupEnd <= count {
				end = groupEnd
			}
		}
		return end
	}
	claims := kept
	gasPerClaim := uint64(0)
	for _, claim := range kept {
		gasPerClaim = max(gasPerClaim, snClaimRelayGasLimit(claim.Estimate))
	}
	var send *snClaimRelaySend
	for send == nil && 2 <= len(claims) {
		batch, err := chain.SizeBatch(ctx, claims, gasPerClaim)
		if err != nil {
			glog.Infof("[sn]claim relay batch of %d is not ready, claiming alone: %s\n", len(claims), err)
			break
		}
		if batch == nil || batch.Count < 2 {
			break
		}
		if end := whole(batch.Count); end < batch.Count {
			// the batcher fits part of a group: size the whole groups again
			claims = claims[:end]
			continue
		}
		liability := snClaimRelayWorstCase(batch.GasLimit, facts.FeeCapWei)
		if liability.Cmp(funds) <= 0 {
			send = &snClaimRelaySend{To: batch.To, Calldata: batch.Calldata, GasLimit: batch.GasLimit, Claims: claims[:batch.Count], Batch: true}
			break
		}
		// As many as fit by this size's cost a claim, and one fewer at least.
		fit := new(big.Int).Quo(new(big.Int).Mul(funds, big.NewInt(int64(batch.Count))), liability).Int64()
		claims = claims[:whole(min(int(fit), batch.Count-1))]
	}
	if send == nil {
		first := kept[0]
		to, calldata := chain.Claim(first)
		send = &snClaimRelaySend{To: to, Calldata: calldata, GasLimit: snClaimRelayGasLimit(first.Estimate), Claims: []*snClaimRelayClaim{first}}
	}
	// A tapped claim the relay would have sent by itself uses nothing up: one
	// over the threshold, or one under it that goes in this batch with a due
	// account of its coldkey.
	over := func(claim *snClaimRelayClaim) bool {
		return snClaimRelayOverThreshold(snClaimRelayValueTaoRao(claim.Account.claimable(), facts.AlphaPriceWei))
	}
	for _, claim := range send.Claims {
		withDue := send.Batch && slices.ContainsFunc(send.Claims, func(other *snClaimRelayClaim) bool {
			return other != claim && other.Account.Coldkey == claim.Account.Coldkey && over(other)
		})
		claim.UnderThreshold = !over(claim) && !withDue
	}
	return send
}

// snClaimRelayPoolUnavailable names a pool whose tree the pass could not read
// after its ledger read, as that read names the pools it could not read: the
// pool's accounts wait, a request with one of them stays, and the pass claims
// the other pools.
func snClaimRelayPoolUnavailable(status *SnClaimRelayStatus, noId uint64, err error) {
	if slices.Contains(status.UnavailablePools, noId) {
		return
	}
	glog.Infof("[sn]claim relay pool %d is unavailable this pass: %s; the others are claimed\n", noId, err)
	status.UnavailablePools = append(slices.Clone(status.UnavailablePools), noId)
	slices.Sort(status.UnavailablePools)
	status.PoolsUnavailable += 1
}

func (self *snClaimRelay) refuse(account *snClaimRelayAccount, reason string, now time.Time, wait time.Duration, status *SnClaimRelayStatus) {
	if status.Refusals == nil {
		status.Refusals = map[string]*SnClaimRelayRefusal{}
	}
	status.Refusals[account.key()] = &SnClaimRelayRefusal{Time: now, Until: now.Add(wait), Reason: reason}
	self.claimResults(SnClaimRelayResultRefused, 1)
	glog.Infof("[sn]claim relay account %s refused: %s\n", account.key(), reason)
}

func (self *snClaimRelay) applyOutcome(ctx context.Context, send *snClaimRelaySend, outcome *snClaimRelayOutcome, now time.Time, taps *snClaimRelayTaps, status *SnClaimRelayStatus) {
	glog.Infof("[sn]claim relay sent %d claims in %s: %d gas, reverted %t\n", len(send.Claims), outcome.TxHash, outcome.GasUsed, outcome.Reverted)
	for index, claim := range send.Claims {
		if len(outcome.Items) <= index {
			break
		}
		item := outcome.Items[index]
		switch {
		case item.Paid:
			self.claimResults(SnClaimRelayResultPaid, 1)
			taps.paid(claim)
		case item.NotAttempted:
			self.claimResults(SnClaimRelayResultNotAttempted, 1)
		default:
			// A refused claim is due again, but not today: it burned gas.
			self.refuse(claim.Account, item.Reason, now, snClaimRelayDayStart(now).Add(24*time.Hour).Sub(now), status)
		}
	}
}

// Drops refusals that have run out.
func snClaimRelayLiveRefusals(refusals map[string]*SnClaimRelayRefusal, now time.Time) map[string]*SnClaimRelayRefusal {
	var live map[string]*SnClaimRelayRefusal
	for key, refusal := range refusals {
		if refusal != nil && now.Before(refusal.Until) {
			if live == nil {
				live = map[string]*SnClaimRelayRefusal{}
			}
			live[key] = refusal
		}
	}
	return live
}

func snClaimRelayLimitRefusals(status *SnClaimRelayStatus) {
	for snClaimRelayRefusalsLimit < len(status.Refusals) {
		oldestKey := ""
		for key, refusal := range status.Refusals {
			if oldestKey == "" || refusal.Time.Before(status.Refusals[oldestKey].Time) {
				oldestKey = key
			}
		}
		delete(status.Refusals, oldestKey)
	}
}
