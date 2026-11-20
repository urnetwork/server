package controller

// Pay with Gamma: the biller and the watcher (mmm/BTCPAY.md §18.4).
//
// One taskworker chain, RunOnce "gamma_biller", runs this every few seconds,
// so exactly one process signs with the deposit hotkeys' keys and owns their
// nonces. Since runtime 493 a charge is one spend_gamma(our service, 25,
// price) signed by the deposit's own hotkey (gamma_chain.go); it pays no fee.
// Each run:
//
//  1. reads the finalized head and judges readiness: the runtime's shapes,
//     our published service, and (for the terms only) whether minting works:
//     gamma on network-wide and on the subnet, a TAO/USD feed at most 300
//     blocks old
//  2. the watcher: reads every finalized block after its cursor, each with the
//     runtime that executed it. A GammaSpent emitted by one of our spends
//     (the extrinsic with the hash we recorded) resolves its charge; anything
//     that must not happen raises an alarm: a spend by one of our deposit
//     hotkeys that is none of our spends (at our service or another), a spend
//     of ours that landed other than we signed it. A spend at our service from
//     a hotkey we never assigned is counted and logged: what it spent stays in
//     the chain's GammaSpent total for that hotkey. Plain TAO sent to a
//     deposit hotkey is recorded as a recoverable customer mistake. A block the
//     runtime cannot decode stops the cursor there and pauses the biller, with
//     an alarm: never a crash loop, never a guess. A block a retired runtime
//     ran (older than 493) is read only for what kept its names, and only
//     while none of our spends is in flight.
//  3. the live spend: resubmitted while its era runs; expired, its charge
//     released for another spend with its reference unchanged, once its era
//     is over, every block it could land in was read, and the deposit
//     hotkey's nonce shows it never landed; held for review when the nonce
//     moved without it.
//  4. looks at the deposits of the open intents and of the paying accounts
//  5. charges: re-arms a skipped charge once its deposit changed; records a
//     charge for each open intent its deposit can pay (spendable, nothing else
//     pending ahead of it); then, for the oldest charge the chain would take
//     now, signs one spend as the deposit's hotkey, has the chain judge it
//     (validate_transaction), records it with its exact bytes, and submits
//     it. A charge the deposit or the chain refuses before anything is signed
//     or recorded is skipped, and goes out again only once its deposit
//     changed.
//  6. credits every intent whose charge was spent (the one-shot); voids the
//     automatic intents whose switch the account turned off, unless their
//     charge is already in flight; and sends the notice of a skipped
//     automatic charge once
//  7. every few minutes, the automatic demand: renewals at the end of a term,
//     data top-ups when the balance runs out, Pro for a free account at its
//     client limit, each one intent per (account, purpose, period) and
//     within the account's monthly limit
//
// Credit is only ever on our own finalized GammaSpent: until it lands, the
// customer's coldkey can move the gamma or pause the deposit, and a spend the
// chain refuses buys nothing. A signed spend stays live until it lands or its
// era is proven over: its charge is never signed again before that, so no
// charge is ever spent twice.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Readiness reasons. "ready" sells; anything else pauses new charges (and new
// intents) until it clears; a charge already recorded waits. Runtime 493
// retired the spender's (service_spender_mismatch, billing_proxy_missing,
// fee_float_low): a spend names no spender, needs no proxy and pays no fee.
const (
	gammaReady               = "ready"
	gammaNotConfigured       = "not_configured"
	gammaChainUnreachable    = "chain_unreachable"
	gammaRuntimeUnsupported  = "runtime_unsupported"
	gammaServiceNotPublished = "service_not_published"
	gammaWatcherBehind       = "watcher_behind"
	gammaBatchInReview       = "batch_in_review"
)

// Minting reasons (the terms' minting flag).
const (
	gammaMintDisabled       = "gamma_disabled"
	gammaMintSubnetDisabled = "subnet_gamma_disabled"
	gammaMintPriceStale     = "price_stale"
)

// Alarm kinds. The kind strings are kept from runtime 492's spender model, so
// the monitor reads them unchanged (SIGNALS.md §2.22b); since runtime 493:
//   - unknown_reference: a GammaSpent by one of our deposit hotkeys that is
//     none of our spends, at our service or another: we hold those keys and
//     record every spend before we sign it, so this is the first sign of a
//     leaked deposit seed, or of a second process signing with it
//   - spend_mismatch: one of our spends landed other than we signed it
//   - unexpected_nonce: a deposit hotkey's nonce moved without our spend
//   - duplicate_reference, spender_proxy_changed, unknown_billing_transaction:
//     no longer raised (a spend carries no reference; there is no spender and
//     no billing key)
const (
	gammaAlarmUnknownReference    = "unknown_reference"
	gammaAlarmDuplicateReference  = "duplicate_reference"
	gammaAlarmSpendMismatch       = "spend_mismatch"
	gammaAlarmSpenderProxyChanged = "spender_proxy_changed"
	gammaAlarmUnknownBillingTx    = "unknown_billing_transaction"
	gammaAlarmUnexpectedNonce     = "unexpected_nonce"
	gammaAlarmRuntimeUnsupported  = "runtime_unsupported"
	gammaAlarmUndecodableBlock    = "undecodable_block"
	gammaAlarmStrayTao            = "stray_tao"
	gammaAlarmServiceChanged      = "service_changed"
	// a spent charge whose network no longer exists: the customer paid and
	// nothing can be credited
	gammaAlarmCreditUnfulfillable = "credit_unfulfillable"
	// the live batch carries a charge that can no longer be credited: it is
	// not (re)submitted
	gammaAlarmBatchUnfulfillable = "batch_unfulfillable"
)

// The watcher may trail the finalized head by this many blocks and still sell.
const gammaMaxWatcherLagBlocks = 100

// A credit that failed this many times is held for an operator: its status
// says review (credit_failed), and the biller retries only a few of those a
// run (gammaCreditReviewRetries), so they never crowd out a new credit.
const gammaMaxCreditFailures = 10

// What the monitor reads (SIGNALS.md §2.22a, gamma-payment-failures): the
// failures that hold a credit for review, and the alarm kind of a live batch
// that carries a charge no credit can reach.
const (
	GammaMaxCreditFailures           = gammaMaxCreditFailures
	GammaAlarmKindBatchUnfulfillable = gammaAlarmBatchUnfulfillable
)

// What the monitor reads (SIGNALS.md §2.22b, gamma-watcher): the watcher's
// other alarm kinds, and the readiness that sells.
const (
	GammaAlarmKindCreditUnfulfillable = gammaAlarmCreditUnfulfillable
	GammaAlarmKindUnknownReference    = gammaAlarmUnknownReference
	GammaAlarmKindDuplicateReference  = gammaAlarmDuplicateReference
	GammaAlarmKindSpendMismatch       = gammaAlarmSpendMismatch
	GammaAlarmKindSpenderProxyChanged = gammaAlarmSpenderProxyChanged
	GammaAlarmKindUnknownBillingTx    = gammaAlarmUnknownBillingTx
	GammaAlarmKindUnexpectedNonce     = gammaAlarmUnexpectedNonce
	GammaAlarmKindRuntimeUnsupported  = gammaAlarmRuntimeUnsupported
	GammaAlarmKindUndecodableBlock    = gammaAlarmUndecodableBlock
	GammaAlarmKindServiceChanged      = gammaAlarmServiceChanged
	GammaAlarmKindStrayTao            = gammaAlarmStrayTao
	GammaReadinessReady               = gammaReady
)

// GammaAlarmKinds are every alarm kind the biller records.
func GammaAlarmKinds() []string {
	return []string{
		gammaAlarmUnknownReference,
		gammaAlarmDuplicateReference,
		gammaAlarmSpendMismatch,
		gammaAlarmSpenderProxyChanged,
		gammaAlarmUnknownBillingTx,
		gammaAlarmUnexpectedNonce,
		gammaAlarmRuntimeUnsupported,
		gammaAlarmUndecodableBlock,
		gammaAlarmStrayTao,
		gammaAlarmServiceChanged,
		gammaAlarmCreditUnfulfillable,
		gammaAlarmBatchUnfulfillable,
	}
}

// GammaReadinessReasons are every readiness the biller records: ready, or
// why it charges nothing.
func GammaReadinessReasons() []string {
	return slices.Clone(gammaReadinessReasons)
}

// The credits each run tries, the fewest failures first.
const gammaCreditsPerRun = 200

// The open intents each run may record a charge for.
const gammaChargeablePerRun = 512

// The charges each run looks through for the one spend it sends: the oldest
// the deposit and the chain take now.
const gammaSpendCandidatesPerRun = 50

// The charge-results state of a GammaSpent at our service from a hotkey we
// never assigned (charges_resolved_total): no charge of ours, but what it
// spent stays in the chain's GammaSpent total for that hotkey, to be credited
// once its holder proves it is theirs.
const gammaForeignSpendState = "foreign"

// The credits held for review each run retries besides, the least retried
// first: each is retried in turn, and one whose cause was fixed is credited.
const gammaCreditReviewRetries = 10

// The renewal is charged this long before the paid term ends.
const gammaRenewLead = 1 * time.Hour

// A top-up the deposit cannot pay within this long expires; the next check
// makes a new one if the balance still needs it.
const gammaTopupTtl = 1 * time.Hour

// A seat upgrade waits this long for its charge.
const gammaSeatTtl = 24 * time.Hour

// The balance a top-up is due under, whatever the recent use: a quarter of a
// GiB.
const gammaTopupFloorBytes = 256 * model.Mib

// GammaBillerResult is what one run did.
type GammaBillerResult struct {
	// gamma.yml names everything the biller needs
	Configured bool   `json:"configured"`
	Readiness  string `json:"readiness"`
	Scanned    int    `json:"scanned"`
	Charged    int    `json:"charged"`
	Credited   int    `json:"credited"`
	// the run interval the next run waits
	Interval time.Duration `json:"interval"`
}

// gammaChains caches one chain (and its runtimes) per endpoint list.
var gammaChainsLock sync.Mutex
var gammaChains = map[string]*gammaChain{}

// The chain for a set of endpoints. Replaceable only by tests, which serve a
// simulated chain.
var gammaChainFor = func(endpoints []string) *gammaChain {
	key := strings.Join(endpoints, ",")
	gammaChainsLock.Lock()
	defer gammaChainsLock.Unlock()
	chain := gammaChains[key]
	if chain == nil {
		chain = newGammaChain(newGammaHttpRpc(endpoints))
		gammaChains[key] = chain
	}
	return chain
}

// The clock. Replaceable only by tests.
var gammaNow = server.NowUtc

// When the automatic demand last ran in this process.
var gammaAutoLock sync.Mutex
var gammaAutoLastRun time.Time

// gammaTerms is what /pay/gamma/terms shows, as the biller last judged it.
type gammaTerms struct {
	Netuid        uint16 `json:"netuid"`
	ServiceHotkey string `json:"service_hotkey"`
	// the deposit master the biller signs for (its gamma.yml's): the api
	// sells only while its own copy names the same one (gammaLoadView), so a
	// hotkey it assigns is one the biller can spend from
	DepositHotkeyMaster string `json:"deposit_hotkey_master"`
	ServicesUrl         string `json:"services_url"`
	FinalizedBlock      uint64 `json:"finalized_block"`
	SpecVersion         uint32 `json:"spec_version"`
	TaoUsdAccepted      uint64 `json:"tao_usd_accepted"`
	PriceFedAt          uint64 `json:"price_fed_at"`
	PriceAgeBlocks      uint64 `json:"price_age_blocks"`
	GammaPerTao         uint64 `json:"gamma_per_tao"`
	ServicePublished    bool   `json:"service_published"`
}

// RunGammaBiller is one run of the biller and the watcher.
func RunGammaBiller(clientSession *session.ClientSession) (result *GammaBillerResult, returnErr error) {
	ctx := clientSession.Ctx
	result = &GammaBillerResult{Interval: gammaDefaultRunInterval}
	settings, err := gammaLoadSettings()
	if err != nil {
		result.Readiness = gammaNotConfigured
		gammaRecordReadiness(ctx, nil, gammaNotConfigured, gammaNotConfigured, nil)
		gammaReports.report("not_configured", func() {
			glog.Infof("[gamma]not configured: %s\n", err)
		})
		return result, nil
	}
	if !settings.canSign {
		// a serving copy of gamma.yml (no deposit_hotkey_seed): nothing here
		// bills
		result.Readiness = gammaNotConfigured
		gammaRecordReadiness(ctx, nil, gammaNotConfigured, gammaNotConfigured, nil)
		gammaReports.report("no_deposit_seed", func() {
			glog.Infof("[gamma]not configured: gamma.yml names no deposit_hotkey_seed, so this process does not bill\n")
		})
		return result, nil
	}
	result.Configured = true
	result.Interval = settings.RunInterval
	endpoints := settings.Endpoints()
	if len(endpoints) == 0 {
		result.Readiness = gammaNotConfigured
		gammaRecordReadiness(ctx, nil, gammaNotConfigured, gammaNotConfigured, nil)
		return result, nil
	}
	chain := gammaChainFor(endpoints)
	biller := &gammaBiller{
		ctx:      ctx,
		session:  clientSession,
		settings: settings,
		chain:    chain,
		now:      gammaNow(),
	}
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("gamma biller: %v", r)
		}
	}()
	biller.run(result)
	return result, nil
}

type gammaBiller struct {
	ctx      context.Context
	session  *session.ClientSession
	settings *gammaSettings
	chain    *gammaChain
	now      time.Time

	head       types.Hash
	headNumber uint64
	runtime    *gammaRuntime
	terms      *gammaTerms
	readiness  string
	mint       string
	// hotkeys an event touched this run
	touched map[string]bool
}

func (self *gammaBiller) run(result *GammaBillerResult) {
	ctx := self.ctx
	self.touched = map[string]bool{}
	head, headNumber, err := self.chain.FinalizedHead(ctx)
	if err != nil {
		self.pause(result, gammaChainUnreachable, err)
		return
	}
	self.head, self.headNumber = head, headNumber
	runtime, err := self.chain.RuntimeAt(ctx, head)
	if err != nil {
		self.pause(result, gammaChainUnreachable, err)
		return
	}
	self.runtime = runtime
	if err := runtime.Supported(); err != nil {
		self.alarm(fmt.Sprintf("runtime/%d", runtime.SpecVersion), gammaAlarmRuntimeUnsupported, nil, err.Error())
		self.reportScanLag()
		self.pause(result, gammaRuntimeUnsupported, err)
		return
	}

	self.readiness, self.mint = self.judge()

	scanned, scanErr := self.scan()
	result.Scanned = scanned
	if scanErr != nil {
		self.reportScanLag()
		self.pause(result, gammaReadinessFor(scanErr), scanErr)
		return
	}
	state := model.GetGammaChainState(ctx)
	if self.readiness == gammaReady && state != nil && state.ScannedBlock != nil && uint64(*state.ScannedBlock)+gammaMaxWatcherLagBlocks < self.headNumber {
		self.readiness = gammaWatcherBehind
	}

	self.handleLiveBatch()
	// after the live batch is judged: one held for review stops all signing
	if self.readiness == gammaReady && 0 < model.CountGammaSpendBatchesInReview(ctx) {
		self.readiness = gammaBatchInReview
	}
	self.observe()
	result.Credited = self.credit()
	// before anything is charged, and before a skip notice for a charge the
	// customer turned off goes out
	self.voidSwitchedOff()
	self.sendSkipNotices()
	self.autoDemand()
	if self.readiness == gammaReady {
		result.Charged = self.charge()
	}
	result.Readiness = self.readiness
	gammaReadyGauge(self.readiness)
	gammaRecordReadiness(ctx, self.chainState(), self.readiness, self.mint, self.terms)
}

// reportScanLag sets the scan lag gauge from the cursor while the watcher
// holds still: a paused watcher falls behind, and the gauge says by how much.
func (self *gammaBiller) reportScanLag() {
	state := model.GetGammaChainState(self.ctx)
	if state == nil || state.ScannedBlock == nil || self.headNumber == 0 {
		return
	}
	gammaScanLagGauge.Set(float64(self.headNumber - min(self.headNumber, uint64(*state.ScannedBlock))))
}

func (self *gammaBiller) pause(result *GammaBillerResult, reason string, err error) {
	result.Readiness = reason
	gammaReadyGauge(reason)
	gammaReports.report("pause/"+reason, func() {
		glog.Warningf("[gamma]paused (%s): %s\n", reason, err)
	})
	gammaRecordReadiness(self.ctx, self.chainState(), reason, gammaMintFor(reason), self.terms)
}

func gammaMintFor(reason string) string {
	if reason == gammaReady {
		return gammaReady
	}
	return reason
}

func gammaReadinessFor(err error) string {
	if errors.Is(err, errGammaRuntimeUnsupported) {
		return gammaRuntimeUnsupported
	}
	return gammaChainUnreachable
}

// chainState is the watcher's state as of this run (the cursor is written by
// scan).
func (self *gammaBiller) chainState() *model.GammaChainState {
	state := model.GetGammaChainState(self.ctx)
	if state == nil {
		state = &model.GammaChainState{}
	}
	if 0 < self.headNumber {
		finalized := int64(self.headNumber)
		state.FinalizedBlock = &finalized
	}
	if self.runtime != nil {
		spec := int(self.runtime.SpecVersion)
		state.RuntimeSpec = &spec
	}
	return state
}

func gammaRecordReadiness(ctx context.Context, state *model.GammaChainState, readiness string, mint string, terms *gammaTerms) {
	if state == nil {
		state = model.GetGammaChainState(ctx)
		if state == nil {
			state = &model.GammaChainState{}
		}
	}
	state.Readiness = readiness
	state.MintReadiness = mint
	if terms != nil {
		if encoded, err := json.Marshal(terms); err == nil {
			state.Terms = encoded
		}
	}
	state.UpdateTime = gammaNow()
	model.SetGammaChainState(ctx, state)
}

// ----- readiness -----

// judge reads, at the finalized head, everything selling depends on.
func (self *gammaBiller) judge() (readiness string, mint string) {
	ctx := self.ctx
	settings := self.settings
	runtime := self.runtime
	at := self.head
	self.terms = &gammaTerms{
		Netuid:              settings.Netuid,
		ServiceHotkey:       settings.ServiceAddress,
		DepositHotkeyMaster: gammaAddress(settings.DepositHotkeyMaster),
		ServicesUrl:         settings.ServicesUrl,
		FinalizedBlock:      self.headNumber,
		SpecVersion:         runtime.SpecVersion,
	}

	mint = gammaReady
	gammaEnabled, err := self.chain.Bool(ctx, runtime, at, "GammaEnabled")
	if err != nil {
		return gammaChainUnreachable, gammaChainUnreachable
	}
	subnetEnabled, err := self.chain.Bool(ctx, runtime, at, "SubnetGammaEnabled", gammaNetuidArg(settings.Netuid))
	if err != nil {
		return gammaChainUnreachable, gammaChainUnreachable
	}
	price, err := self.chain.TaoUsdPrice(ctx, runtime, at)
	if err != nil {
		return gammaChainUnreachable, gammaChainUnreachable
	}
	if price != nil {
		self.terms.TaoUsdAccepted = price.Accepted
		self.terms.PriceFedAt = price.FedAt
		if price.FedAt <= self.headNumber {
			self.terms.PriceAgeBlocks = self.headNumber - price.FedAt
		}
	}
	switch {
	case !gammaEnabled:
		mint = gammaMintDisabled
	case !subnetEnabled:
		mint = gammaMintSubnetDisabled
	case price == nil || price.Accepted == 0 || gammaPriceMaxAgeBlocks < self.terms.PriceAgeBlocks:
		mint = gammaMintPriceStale
	}
	if mint == gammaReady {
		// what one TAO mints now, for the indicative amounts
		if sim, err := self.chain.SimMintGamma(ctx, runtime, at, settings.Netuid, 1_000_000_000); err == nil {
			if sim.Error == "" {
				self.terms.GammaPerTao = sim.Gamma
			} else if sim.Error == "TaoUsdPriceUnavailable" {
				mint = gammaMintPriceStale
			} else if sim.Error == "GammaDisabled" {
				mint = gammaMintSubnetDisabled
			}
		}
	}
	gammaPriceAgeGauge.Set(float64(self.terms.PriceAgeBlocks))

	// a spend at a hotkey with no service on the subnet is refused: our
	// service must be published (SubtensorModule.Services, its url only)
	service, err := self.chain.Service(ctx, runtime, at, settings.Netuid, settings.ServiceHotkey)
	if err != nil {
		return gammaChainUnreachable, mint
	}
	if service == nil {
		return gammaServiceNotPublished, mint
	}
	self.terms.ServicePublished = true
	if service.Url != settings.ServicesUrl {
		self.alarm("service_url/"+service.Url, gammaAlarmServiceChanged, nil, fmt.Sprintf("the published service's url is %q, not %q", service.Url, settings.ServicesUrl))
	}
	return gammaReady, mint
}

// ----- the watcher -----

// scan reads the finalized blocks after the cursor, at most a run's worth.
func (self *gammaBiller) scan() (scanned int, returnErr error) {
	ctx := self.ctx
	state := model.GetGammaChainState(ctx)
	if state == nil || state.ScannedBlock == nil {
		// the first run starts at the head: nothing before it is ours
		self.setCursor(self.headNumber, self.head)
		return 0, nil
	}
	from := uint64(*state.ScannedBlock) + 1
	to := min(self.headNumber, from+self.settings.MaxBlocksPerRun-1)
	for number := from; number <= to; number++ {
		if err := self.scanBlock(number); err != nil {
			return scanned, err
		}
		scanned++
	}
	gammaScanLagGauge.Set(float64(self.headNumber - min(self.headNumber, to)))
	return scanned, nil
}

func (self *gammaBiller) setCursor(number uint64, hash types.Hash) {
	state := model.GetGammaChainState(self.ctx)
	if state == nil {
		state = &model.GammaChainState{Readiness: gammaNotConfigured, MintReadiness: gammaNotConfigured}
	}
	scanned := int64(number)
	hashText := hash.Hex()
	state.ScannedBlock = &scanned
	state.ScannedHash = &hashText
	state.UpdateTime = gammaNow()
	if state.Readiness == "" {
		state.Readiness = gammaNotConfigured
	}
	if state.MintReadiness == "" {
		state.MintReadiness = gammaNotConfigured
	}
	model.SetGammaChainState(self.ctx, state)
}

// scanBlock reads one block's events. The cursor moves past the block only
// once everything in it is recorded.
func (self *gammaBiller) scanBlock(number uint64) error {
	ctx := self.ctx
	settings := self.settings
	block, err := self.chain.BlockAt(ctx, number)
	if err != nil {
		return err
	}
	retired := false
	if err := block.Runtime.Supported(); err != nil {
		// a block an older runtime ran (the last blocks before an upgrade):
		// read for what kept its names, while none of our spends is in
		// flight. Anything else holds the cursor here.
		if !block.Runtime.Retired() || model.GetLiveGammaSpendBatch(ctx) != nil {
			self.alarm(fmt.Sprintf("runtime/%d", block.Runtime.SpecVersion), gammaAlarmRuntimeUnsupported, gammaBlockRef(number), err.Error())
			return err
		}
		retired = true
	}
	events, err := self.chain.Events(ctx, block)
	if err != nil {
		self.alarm(fmt.Sprintf("undecodable/%d", number), gammaAlarmUndecodableBlock, gammaBlockRef(number), err.Error())
		return fmt.Errorf("%w: block %d: %v", errGammaRuntimeUnsupported, number, err)
	}
	if retired {
		gammaReports.report(fmt.Sprintf("retired/%d", block.Runtime.SpecVersion), func() {
			glog.Infof("[gamma]block %d ran retired runtime %d: read for mints, moves and transfers only\n", number, block.Runtime.SpecVersion)
		})
	}

	var spends []*gammaSpendEvent
	transfers := map[string]uint64{}
	for _, event := range events {
		switch event.Name {
		case "SubtensorModule.GammaSpent":
			if retired {
				// the spender model's spend (no service, a reference): none
				// of ours, since no spend of ours was in flight
				continue
			}
			spend, err := gammaSpendEventFrom(event)
			if err != nil {
				return fmt.Errorf("%w: block %d event %d: %v", errGammaRuntimeUnsupported, number, event.Index, err)
			}
			if spend.Netuid == settings.Netuid {
				spends = append(spends, spend)
			}
		case "SubtensorModule.GammaMinted", "SubtensorModule.GammaDepositDailySpendCapSet", "SubtensorModule.GammaDepositClosed":
			if netuid, err := gammaEventUint(event, "netuid"); err == nil && uint16(netuid) == settings.Netuid {
				if hotkey, err := gammaEventBytes32(event, "hotkey"); err == nil {
					self.touched[gammaAddress(hotkey)] = true
				}
			}
		case "SubtensorModule.GammaMoved":
			if netuid, err := gammaEventUint(event, "netuid"); err == nil && uint16(netuid) == settings.Netuid {
				for _, field := range []string{"origin_hotkey", "destination_hotkey"} {
					if hotkey, err := gammaEventBytes32(event, field); err == nil {
						self.touched[gammaAddress(hotkey)] = true
					}
				}
			}
		case "Balances.Transfer":
			to, err := gammaEventBytes32(event, "to")
			if err != nil {
				continue
			}
			amount, _ := gammaEventUint(event, "amount")
			transfers[gammaAddress(to)] += amount
		}
	}

	if 0 < len(transfers) {
		addresses := []string{}
		for address := range transfers {
			addresses = append(addresses, address)
		}
		for address, hotkey := range model.GetGammaDepositHotkeysByAddresses(ctx, addresses) {
			model.AddGammaStrayTao(ctx, address, int64(transfers[address]))
			self.alarm(
				fmt.Sprintf("stray/%d/%s", number, address),
				gammaAlarmStrayTao,
				gammaBlockRef(number),
				fmt.Sprintf("%d rao of plain TAO sent to deposit hotkey %s (index %d) of network %s: recoverable with the deposit seed", transfers[address], address, hotkey.DerivationIndex, hotkey.NetworkId),
			)
		}
	}

	if 0 < len(spends) {
		if err := self.resolveBlock(block, spends); err != nil {
			return err
		}
	}
	self.setCursor(number, block.Hash)
	return nil
}

// gammaLandedSpend is one of our spends found in a block, with its charge.
type gammaLandedSpend struct {
	spend  *gammaSpendEvent
	batch  *model.GammaSpendBatch
	charge *model.GammaCharge
}

// resolveBlock records what each GammaSpent on our subnet in one block means
// to us. A spend by one of our deposit hotkeys is ours only when the
// extrinsic that emitted it is one of our recorded spends, by hash: it
// resolves that spend's one charge and places the spend in the block. Any
// other spend by one of our deposit hotkeys, at our service or another, is
// one we never issued: an alarm. A spend at our service from a hotkey we
// never assigned is counted and logged; what it spent stays in the chain's
// GammaSpent total for that hotkey.
func (self *gammaBiller) resolveBlock(block *gammaBlock, spends []*gammaSpendEvent) error {
	ctx := self.ctx
	settings := self.settings
	number := block.Number

	addresses := []string{}
	for _, spend := range spends {
		addresses = append(addresses, gammaAddress(spend.Hotkey))
	}
	ours := model.GetGammaDepositHotkeysByAddresses(ctx, addresses)

	var extrinsics [][]byte
	landed := []*gammaLandedSpend{}
	for _, spend := range spends {
		address := gammaAddress(spend.Hotkey)
		eventRef := fmt.Sprintf("%d/%d", number, spend.Event.Index)
		if _, ok := ours[address]; !ok {
			if spend.Service == settings.ServiceHotkey {
				gammaChargeResults.WithLabelValues(gammaForeignSpendState, "").Inc()
				glog.Warningf("[gamma]%s gamma spent at our service by %s, a hotkey we never assigned (block %d, event %d): no charge of ours; it stays in that hotkey's GammaSpent total\n", gammaFormatUnits(int64(spend.Amount)), address, number, spend.Event.Index)
			}
			continue
		}
		// the extrinsic that emitted it, by hash
		var batch *model.GammaSpendBatch
		if 0 <= spend.Event.ExtrinsicIndex {
			if extrinsics == nil {
				var err error
				if extrinsics, err = self.chain.Extrinsics(ctx, block.Hash); err != nil {
					return err
				}
			}
			if spend.Event.ExtrinsicIndex < int64(len(extrinsics)) {
				batch = model.GetGammaSpendBatchByExtrinsicHash(ctx, gammaExtrinsicHash(extrinsics[spend.Event.ExtrinsicIndex]).Hex())
			}
		}
		if batch == nil {
			self.alarm("spend/"+eventRef, gammaAlarmUnknownReference, gammaBlockRef(number), fmt.Sprintf(
				"deposit hotkey %s spent %s gamma at %s in block %d (event %d), and it is none of our spends: we hold that hotkey's key and record every spend before we sign it, so the deposit seed may have leaked",
				address, gammaFormatUnits(int64(spend.Amount)), gammaAddress(spend.Service), number, spend.Event.Index,
			))
			continue
		}
		charges := model.ListGammaChargesForBatch(ctx, batch.BatchId)
		if len(charges) != 1 || charges[0].State != model.GammaChargePending || charges[0].Hotkey != address ||
			charges[0].Amount != int64(spend.Amount) || spend.Service != settings.ServiceHotkey {
			// our model of the chain is wrong: the spend is held for an
			// operator with its charge, and nothing is signed meanwhile
			note := fmt.Sprintf(
				"our spend %s landed in block %d (event %d) as %s gamma from %s at %s, not as its %d charge(s) were recorded",
				batch.ExtrinsicHash, number, spend.Event.Index, gammaFormatUnits(int64(spend.Amount)), address, gammaAddress(spend.Service), len(charges),
			)
			self.alarm("mismatch/"+eventRef, gammaAlarmSpendMismatch, gammaBlockRef(number), note)
			model.ReviewGammaSpendBatch(ctx, batch.BatchId, note, self.now)
			continue
		}
		landed = append(landed, &gammaLandedSpend{spend: spend, batch: batch, charge: charges[0]})
	}
	if len(landed) == 0 {
		return nil
	}

	server.Tx(ctx, func(tx server.PgTx) {
		for _, our := range landed {
			resolution := &model.GammaChargeResolution{
				State:       model.GammaChargeSpent,
				BlockNumber: int64(number),
				BlockHash:   block.Hash.Hex(),
				EventIndex:  our.spend.Event.Index,
			}
			if model.ResolveGammaChargeInTx(tx, ctx, our.charge.Reference, resolution, self.now) {
				gammaChargeResults.WithLabelValues(resolution.State, "").Inc()
				glog.Infof("[gamma]charge %s spent at block %d\n", gammaHex(our.charge.Reference), number)
			}
			// a spend pays no fee
			if model.IncludeGammaSpendBatchInTx(tx, ctx, our.batch.BatchId, &model.GammaBatchInclusion{
				BlockNumber:    int64(number),
				BlockHash:      block.Hash.Hex(),
				ExtrinsicIndex: int(our.spend.Event.ExtrinsicIndex),
			}, "", self.now) {
				gammaBatchFees.Observe(0)
				glog.Infof("[gamma]spend %s landed in block %d at %d\n", our.batch.BatchId, number, our.spend.Event.ExtrinsicIndex)
			}
		}
	}, server.TxReadCommitted)
	return nil
}

// ----- the live spend -----

// handleLiveBatch looks after the one live spend (a batch of one charge,
// signed by that charge's deposit hotkey): resubmitted while its era runs,
// expired once its era is over and the hotkey's nonce shows it never landed,
// held for review when that nonce moved without it. A spend the chain now
// refuses (the deposit was emptied after it was signed) stays live until its
// era ends: it could still land if the deposit is refilled, so its charge is
// never signed again before then.
func (self *gammaBiller) handleLiveBatch() {
	ctx := self.ctx
	batch := model.GetLiveGammaSpendBatch(ctx)
	if batch == nil {
		return
	}
	eraEnd := uint64(batch.EraBirth) + uint64(batch.EraPeriod)
	state := model.GetGammaChainState(ctx)
	scannedThrough := uint64(0)
	if state != nil && state.ScannedBlock != nil {
		scannedThrough = uint64(*state.ScannedBlock)
	}
	if eraEnd <= self.headNumber && eraEnd <= scannedThrough {
		// every block the spend could have landed in is read and it is not
		// among them; its deposit hotkey's nonce must say the same
		charges := model.ListGammaChargesForBatch(ctx, batch.BatchId)
		if len(charges) != 1 {
			note := fmt.Sprintf("the era ended and the spend was not seen; it carries %d charges, not 1", len(charges))
			model.ReviewGammaSpendBatch(ctx, batch.BatchId, note, self.now)
			return
		}
		signer, err := model.DecodeBittensorAddress(charges[0].Hotkey)
		if err != nil {
			model.ReviewGammaSpendBatch(ctx, batch.BatchId, "the era ended and the spend was not seen; its charge's hotkey does not decode", self.now)
			return
		}
		account, err := self.chain.Account(ctx, self.runtime, self.head, signer)
		if err != nil {
			return
		}
		if int64(account.Nonce) == batch.Nonce {
			if model.ExpireGammaSpendBatch(ctx, batch.BatchId, "the era ended without the spend landing", self.now) {
				glog.Infof("[gamma]spend %s expired; its charge can be spent again\n", batch.BatchId)
			}
			return
		}
		note := fmt.Sprintf("the era ended and the spend was not seen, but deposit hotkey %s's nonce is %d, not %d", charges[0].Hotkey, account.Nonce, batch.Nonce)
		self.alarm("nonce/"+batch.BatchId.String(), gammaAlarmUnexpectedNonce, nil, note)
		model.ReviewGammaSpendBatch(ctx, batch.BatchId, note, self.now)
		return
	}
	// a charge in the batch that can no longer be credited (its account was
	// deleted another way than account deletion, which waits for the batch, or
	// its intent was closed by hand): the batch is never sent again. Unless the
	// pool still has it, its era runs out and it expires, and its other charges
	// go out in the next batch with their references unchanged.
	if unfulfillable := model.CountUnpayableGammaChargesInBatch(ctx, batch.BatchId); 0 < unfulfillable {
		self.alarm("batch_unfulfillable/"+batch.BatchId.String(), gammaAlarmBatchUnfulfillable, nil, fmt.Sprintf(
			"live batch %s (nonce %d) carries %d charge(s) whose intent is closed or whose network no longer exists: not resubmitted; if it lands from the pool, those spends cannot be credited",
			batch.BatchId, batch.Nonce, unfulfillable,
		))
		return
	}
	// within its era: (re)submit the exact bytes. The pool knows them by hash,
	// and a landed batch cannot land twice: its nonce is used.
	if err := self.chain.Submit(ctx, batch.Extrinsic); err != nil {
		if batch.State == model.GammaBatchSigned {
			gammaReports.report("submit", func() {
				glog.Warningf("[gamma]batch %s submit: %s\n", batch.BatchId, err)
			})
		}
		if !gammaSubmitAccepted(err) {
			return
		}
	}
	if batch.State == model.GammaBatchSigned {
		model.MarkGammaSpendBatchSubmitted(ctx, batch.BatchId, self.now)
	}
}

// gammaSubmitAccepted reports whether a submit error still means the pool has
// (or had) the extrinsic: already imported, or its nonce used by itself.
func gammaSubmitAccepted(err error) bool {
	var rpcErr *gammaRpcError
	if !errors.As(err, &rpcErr) {
		return false
	}
	text := strings.ToLower(rpcErr.Message + " " + string(rpcErr.Data))
	return strings.Contains(text, "already imported") || strings.Contains(text, "temporarily banned") ||
		strings.Contains(text, "priority is too low") || strings.Contains(text, "outdated") || strings.Contains(text, "stale")
}

// ----- deposits -----

// observe reads the deposits the biller and the API need.
func (self *gammaBiller) observe() {
	ctx := self.ctx
	settings := self.settings
	hotkeys := model.ListGammaHotkeysToObserve(ctx, self.now, 500)
	for hotkey := range self.touched {
		if !slices.Contains(hotkeys, hotkey) {
			hotkeys = append(hotkeys, hotkey)
		}
	}
	for _, hotkey := range hotkeys {
		publicKey, err := model.DecodeBittensorAddress(hotkey)
		if err != nil {
			continue
		}
		deposit, err := self.chain.Deposit(ctx, self.runtime, self.head, settings.Netuid, publicKey)
		if err != nil {
			gammaReports.report("observe", func() {
				glog.Warningf("[gamma]could not read deposit %s: %s\n", hotkey, err)
			})
			return
		}
		owner, err := self.chain.AccountId(ctx, self.runtime, self.head, "Owner", publicKey[:])
		if err != nil {
			return
		}
		observation := &model.GammaDepositObservation{Block: int64(self.headNumber)}
		if owner != nil && *owner != ([32]byte{}) {
			observation.HotkeyOwner = gammaAddress(*owner)
		}
		if deposit != nil {
			// since runtime 493 a deposit names no spender
			observation.Exists = true
			observation.Coldkey = gammaAddress(deposit.Coldkey)
			observation.Balance = int64(deposit.Balance)
			observation.Spendable = int64(deposit.Spendable(self.headNumber))
			if deposit.DailySpendCap != nil {
				dailyCap := int64(*deposit.DailySpendCap)
				observation.DailyCap = &dailyCap
			}
		}
		if model.GetGammaDepositHotkey(ctx, hotkey) != nil {
			model.SetGammaDepositObservation(ctx, hotkey, observation, self.now)
		}
	}
}

// ----- charging -----

// charge re-arms the skipped charges whose deposit changed, records a charge
// for each intent its deposit can pay, and signs and submits one spend: the
// oldest charge the deposit and the chain take now.
func (self *gammaBiller) charge() (charged int) {
	ctx := self.ctx
	settings := self.settings
	if model.GetLiveGammaSpendBatch(ctx) != nil {
		return 0
	}

	pending := model.SumPendingGammaCharges(ctx)
	// a skipped charge goes out again, same reference, only once its deposit
	// changed after the skip and can pay it
	for _, charge := range model.ListSkippedGammaCharges(ctx, self.now, 500) {
		hotkey := model.GetGammaDepositHotkey(ctx, charge.Hotkey)
		if hotkey == nil || hotkey.ObservedBlock == nil || charge.BlockNumber == nil || *hotkey.ObservedBlock < *charge.BlockNumber {
			continue
		}
		fingerprint := hotkey.Fingerprint()
		if charge.SkipDeposit == "" {
			model.SetGammaChargeSkipDeposit(ctx, charge.Reference, fingerprint)
			continue
		}
		if fingerprint == charge.SkipDeposit || !gammaDepositCanPay(hotkey, settings, pending[charge.Hotkey], charge.Amount) {
			continue
		}
		if intent := model.GetGammaPaymentIntent(ctx, charge.IntentId); intent == nil || gammaPlanCovered(ctx, intent, self.now) {
			// another Pro subscription pays for these days: not again
			continue
		}
		if model.ReArmGammaCharge(ctx, charge.Reference) {
			pending[charge.Hotkey] += charge.Amount
			glog.Infof("[gamma]charge %s re-armed: its deposit changed\n", gammaHex(charge.Reference))
		}
	}

	hotkeys := map[string]*model.GammaDepositHotkey{}
	for _, intent := range model.ListChargeableGammaPaymentIntents(ctx, self.now, gammaChargeablePerRun) {
		hotkey, ok := hotkeys[intent.Hotkey]
		if !ok {
			hotkey = model.GetGammaDepositHotkey(ctx, intent.Hotkey)
			hotkeys[intent.Hotkey] = hotkey
		}
		if hotkey == nil || !gammaDepositCanPay(hotkey, settings, pending[intent.Hotkey], intent.AmountGamma()) {
			continue
		}
		if gammaPlanCovered(ctx, intent, self.now) {
			// another Pro subscription pays for (or still bills) the term this
			// would buy: never two charges for the same days; it waits
			continue
		}
		if model.CreateGammaCharge(ctx, intent, self.now) {
			pending[intent.Hotkey] += intent.AmountGamma()
		}
	}

	charges := model.ListUnbatchedGammaCharges(ctx, gammaSpendCandidatesPerRun)
	if len(charges) == 0 {
		return 0
	}
	genesis, err := self.chain.GenesisHash(ctx)
	if err != nil {
		return 0
	}
	for _, charge := range charges {
		sent, stop := self.spend(charge, genesis)
		if sent {
			return 1
		}
		if stop {
			return 0
		}
	}
	return 0
}

// spend sends one charge: it reads the deposit and the hotkey's nonce at the
// finalized head, signs spend_gamma(our service, netuid, amount) as the
// deposit's hotkey, has the chain judge it, records it with its exact bytes,
// and submits it. sent when it went out (or was recorded to go out); stop
// when no other charge should be tried this run. A charge the deposit or the
// chain refuses before anything is recorded is skipped: nothing was sent, so
// it can be signed again later, once its deposit changed.
func (self *gammaBiller) spend(charge *model.GammaCharge, genesis types.Hash) (sent bool, stop bool) {
	ctx := self.ctx
	settings := self.settings
	hotkey := model.GetGammaDepositHotkey(ctx, charge.Hotkey)
	if hotkey == nil || len(charge.Reference) != 32 || charge.Amount <= 0 {
		return false, false
	}
	publicKey, err := model.DecodeBittensorAddress(charge.Hotkey)
	if err != nil {
		return false, false
	}
	signer, err := gammaDepositSigner(settings, hotkey.DerivationIndex, publicKey)
	if err != nil {
		// a hotkey of another deposit master: nothing here can spend it
		gammaReports.report("unsignable/"+charge.Hotkey, func() {
			glog.Errorf("[gamma]charge %s: deposit hotkey %s (index %d) is not derivable from this deposit seed\n", gammaHex(charge.Reference), charge.Hotkey, hotkey.DerivationIndex)
		})
		return false, false
	}
	amount := uint64(charge.Amount)

	deposit, err := self.chain.Deposit(ctx, self.runtime, self.head, settings.Netuid, publicKey)
	if err != nil {
		return false, true
	}
	if refusal := gammaSpendRefusal(deposit, amount, self.headNumber); refusal != "" {
		self.skip(charge, hotkey, refusal)
		return false, false
	}
	account, err := self.chain.Account(ctx, self.runtime, self.head, publicKey)
	if err != nil {
		return false, true
	}
	// the nonce at the finalized head; nothing of this hotkey's may be in the
	// pool
	var poolNonce uint64
	if err := self.chain.rpc.Call(ctx, &poolNonce, "system_accountNextIndex", charge.Hotkey); err != nil {
		return false, true
	}
	if poolNonce != uint64(account.Nonce) {
		gammaReports.report("pool_nonce/"+charge.Hotkey, func() {
			glog.Warningf("[gamma]deposit hotkey %s's pool nonce is %d, its finalized nonce %d: waiting\n", charge.Hotkey, poolNonce, account.Nonce)
		})
		return false, false
	}

	call, err := gammaSpendCall(self.runtime, settings.ServiceHotkey, settings.Netuid, amount)
	if err != nil {
		glog.Errorf("[gamma]could not build the spend: %s\n", err)
		return false, true
	}
	signed, err := gammaSignSpend(self.runtime, genesis, self.headNumber, self.head, settings.EraPeriod, account.Nonce, call, signer)
	if err != nil {
		glog.Errorf("[gamma]could not sign the spend: %s\n", err)
		return false, true
	}
	// the chain's own judgment, before anything is recorded or sent
	validity, err := self.chain.ValidateTransaction(ctx, signed.Encoded, self.head)
	if err != nil {
		gammaReports.report("validate", func() {
			glog.Warningf("[gamma]could not have the spend judged: %s\n", err)
		})
		return false, true
	}
	if !validity.Valid {
		if validity.Refused() {
			// the chain refuses the spend itself (the deposit, the amount or
			// the service as it sees them)
			self.skip(charge, hotkey, "refused")
			return false, false
		}
		// a signature, nonce or era the chain does not take: not sent
		gammaReports.report("invalid/"+validity.Verdict, func() {
			glog.Errorf("[gamma]the chain judges our spend for %s %s: not sent\n", charge.Hotkey, validity.Verdict)
		})
		return false, true
	}

	batch := &model.GammaSpendBatch{
		BatchId:       server.NewId(),
		Nonce:         int64(account.Nonce),
		EraBirth:      int64(self.headNumber),
		EraPeriod:     int(settings.EraPeriod),
		Extrinsic:     signed.Encoded,
		ExtrinsicHash: signed.Hash.Hex(),
		CreateTime:    self.now,
	}
	// recorded before it is ever sent
	if err := model.CreateGammaSpendBatch(ctx, batch, [][]byte{charge.Reference}); err != nil {
		glog.Warningf("[gamma]spend not recorded: %s\n", err)
		return false, true
	}
	if err := self.chain.Submit(ctx, signed.Encoded); err != nil && !gammaSubmitAccepted(err) {
		// the next run resubmits the recorded bytes while the era runs
		glog.Warningf("[gamma]spend %s submit: %s\n", batch.BatchId, err)
		return true, true
	}
	model.MarkGammaSpendBatchSubmitted(ctx, batch.BatchId, self.now)
	gammaBatches.Inc()
	glog.Infof("[gamma]spend %s: charge %s, %s gamma from %s, nonce %d\n", batch.BatchId, gammaHex(charge.Reference), gammaFormatUnits(charge.Amount), charge.Hotkey, batch.Nonce)
	return true, true
}

// skip resolves a charge nothing was signed or recorded for as skipped, as
// of the finalized head, with the deposit as observed then: it goes out again,
// same reference, only once its deposit changed and can pay it.
func (self *gammaBiller) skip(charge *model.GammaCharge, hotkey *model.GammaDepositHotkey, reason string) {
	ctx := self.ctx
	server.Tx(ctx, func(tx server.PgTx) {
		if model.ResolveGammaChargeInTx(tx, ctx, charge.Reference, &model.GammaChargeResolution{
			State:       model.GammaChargeSkipped,
			BlockNumber: int64(self.headNumber),
			BlockHash:   self.head.Hex(),
			EventIndex:  -1,
			SkipReason:  reason,
			SkipDeposit: hotkey.Fingerprint(),
		}, self.now) {
			gammaChargeResults.WithLabelValues(model.GammaChargeSkipped, reason).Inc()
			glog.Infof("[gamma]charge %s skipped at block %d (%s): nothing was sent\n", gammaHex(charge.Reference), self.headNumber, reason)
		}
	})
}

// gammaDepositCanPay reports whether a hotkey's deposit, as last observed,
// has amount spendable beyond what is already pending on it, and the hotkey
// is one the configured deposit master derives (we hold its key).
func gammaDepositCanPay(hotkey *model.GammaDepositHotkey, settings *gammaSettings, pending int64, amount int64) bool {
	if hotkey.ObservedBlock == nil || hotkey.DepositSpendable == nil {
		return false
	}
	if !gammaDepositHotkeyCurrent(settings, hotkey) {
		return false
	}
	return amount <= *hotkey.DepositSpendable-pending
}

// ----- credit and notices -----

// credit credits the intents whose charge was spent. A credit that fails is
// retried every run, the fewest failures first, so a credit that keeps
// failing (a spend for a network that is gone fails for good) never keeps a
// new one waiting; past gammaMaxCreditFailures it is held for review and
// retried only a few a run.
func (self *gammaBiller) credit() (credited int) {
	ctx := self.ctx
	charges := model.ListSpentUncreditedGammaCharges(ctx, gammaMaxCreditFailures, gammaCreditsPerRun)
	charges = append(charges, model.ListSpentUncreditedGammaChargesInReview(ctx, gammaMaxCreditFailures, gammaCreditReviewRetries)...)
	for _, charge := range charges {
		intent := model.GetGammaPaymentIntent(ctx, charge.IntentId)
		if intent == nil {
			continue
		}
		ok, err := gammaCreditIntent(self.session, intent, charge)
		if err != nil {
			count := model.AddGammaPaymentIntentCreditFailure(ctx, intent.IntentId)
			glog.Errorf("[gamma]credit of intent %s failed (%d): %s\n", intent.IntentId, count, err)
			if errors.Is(err, model.ErrPaymentNetworkNotFound) {
				// account deletion waits for a charge in flight, so only a
				// network removed another way gets here: the customer paid and
				// nothing can be credited, and gamma cannot be refunded on chain
				block := "?"
				if charge.BlockNumber != nil {
					block = fmt.Sprintf("%d", *charge.BlockNumber)
				}
				self.alarm("credit_unfulfillable/"+intent.IntentId.String(), gammaAlarmCreditUnfulfillable, charge.BlockNumber, fmt.Sprintf(
					"intent %s (%s %s) spent %s gamma from %s at block %s, reference %s, but its network %s no longer exists: nothing was credited; the payment needs an authorized disposition",
					intent.IntentId, intent.Purpose, intent.Item, gammaFormatUnits(intent.AmountGamma()), intent.Hotkey, block, gammaHex(intent.Reference), intent.NetworkId,
				))
			}
			continue
		}
		if ok {
			credited++
		}
	}
	return
}

// voidSwitchedOff voids the automatic intents whose switch the account
// turned off (POST /pay/gamma/settings): a renewal with automatic renewal
// off, a top-up with automatic top-up off, a seat upgrade with either off.
// It runs in the biller's own step, before charge, which is the only place a
// spend batch is made: a charge in no batch now stays in none, and one in a
// batch that can still land is left to land (the settings answer said so).
// A switch turned off after this step is read again by the batch itself
// (model.CreateGammaSpendBatch).
func (self *gammaBiller) voidSwitchedOff() {
	for _, intent := range model.VoidSwitchedOffGammaIntents(self.ctx, self.now) {
		glog.Infof("[gamma]%s intent %s of network %s voided: its switch is off\n", intent.Purpose, intent.IntentId, intent.NetworkId)
	}
}

func (self *gammaBiller) sendSkipNotices() {
	ctx := self.ctx
	for _, charge := range model.ListSkippedGammaCharges(ctx, self.now, 200) {
		if charge.NoticeSentAt != nil {
			continue
		}
		intent := model.GetGammaPaymentIntent(ctx, charge.IntentId)
		if intent == nil || !intent.Automatic() {
			continue
		}
		gammaSendSkipNotice(ctx, intent, charge, self.settings, self.now)
	}
}

// ----- alarms -----

func (self *gammaBiller) alarm(key string, kind string, block *int64, detail string) {
	if model.AddGammaAlarm(self.ctx, key, kind, block, detail, self.now) {
		gammaAlarms.WithLabelValues(kind).Inc()
		if kind == gammaAlarmStrayTao {
			glog.Warningf("[gamma]alarm %s: %s\n", kind, detail)
		} else {
			glog.Errorf("[gamma]ALARM %s: %s\n", kind, detail)
		}
	}
}

func gammaBlockRef(number uint64) *int64 {
	value := int64(number)
	return &value
}

func gammaInt64Ref(value int64) *int64 {
	return &value
}

// gammaAddress is a public key's SS58 address.
func gammaAddress(publicKey [32]byte) string {
	return gammaSS58(publicKey)
}

// gammaNewReference is a fresh random 32-byte spend reference. References are
// public on chain, so they say nothing about the intent.
func gammaNewReference() []byte {
	reference := make([]byte, 32)
	if _, err := rand.Read(reference); err != nil {
		panic(err)
	}
	return reference
}

// One default-level report a minute per problem.
var gammaReports = newCryptoPayKeyedReport(time.Minute)
