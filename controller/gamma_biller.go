package controller

// Pay with Gamma: the biller and the watcher (mmm/BTCPAY.md §18.4).
//
// One taskworker chain, RunOnce "gamma_biller", runs this every few seconds,
// so exactly one process owns the billing key's nonce. Each run:
//
//  1. reads the finalized head and judges readiness: the runtime's shapes,
//     the published service and its spender, the billing key's GammaSpend
//     proxy and fee float, and (for the terms only) whether minting works:
//     gamma on network-wide and on the subnet, a TAO/USD feed at most 300
//     blocks old
//  2. the watcher: reads every finalized block after its cursor, each with the
//     runtime that executed it. Our spender's GammaSpent and GammaSpendSkipped
//     resolve our charges by reference; our billing key's fee events place our
//     batch; anything that must not happen raises an alarm: a spend from our
//     spender with a reference we never issued, a reference spent twice, any
//     change to the spender's proxies, a billing-key transaction that is none
//     of our batches. Plain TAO sent to a deposit hotkey is recorded as a
//     recoverable customer mistake. A block the runtime cannot decode stops
//     the cursor there and pauses the biller, with an alarm: never a crash
//     loop, never a guess.
//  3. the live batch: resubmitted while its era runs; expired, its charges
//     released to the next batch with their references unchanged, once its
//     era is over, every block it could land in was read, and the billing
//     key's nonce shows it never landed; held for review when the nonce moved
//     without it.
//  4. looks at the deposits of the open intents and of the paying accounts
//  5. charges: re-arms a skipped charge once its deposit changed; records a
//     charge for each open intent its deposit can pay (spendable, our
//     spender, nothing else pending ahead of it); then signs at most one batch
//     of up to 512 charges, records it with its exact bytes, and submits it
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
// customer's coldkey can move the gamma, change the spender or pause the
// deposit, and a skipped spend buys nothing.

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
// intents) until it clears; a charge already recorded waits.
const (
	gammaReady                  = "ready"
	gammaNotConfigured          = "not_configured"
	gammaChainUnreachable       = "chain_unreachable"
	gammaRuntimeUnsupported     = "runtime_unsupported"
	gammaServiceNotPublished    = "service_not_published"
	gammaServiceSpenderMismatch = "service_spender_mismatch"
	gammaBillingProxyMissing    = "billing_proxy_missing"
	gammaFeeFloatLow            = "fee_float_low"
	gammaWatcherBehind          = "watcher_behind"
	gammaBatchInReview          = "batch_in_review"
)

// Minting reasons (the terms' minting flag).
const (
	gammaMintDisabled       = "gamma_disabled"
	gammaMintSubnetDisabled = "subnet_gamma_disabled"
	gammaMintPriceStale     = "price_stale"
)

// Alarm kinds.
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
	Netuid           uint16 `json:"netuid"`
	ServiceHotkey    string `json:"service_hotkey"`
	Spender          string `json:"spender"`
	ServicesUrl      string `json:"services_url"`
	BillingAddress   string `json:"billing_address"`
	FinalizedBlock   uint64 `json:"finalized_block"`
	SpecVersion      uint32 `json:"spec_version"`
	TaoUsdAccepted   uint64 `json:"tao_usd_accepted"`
	PriceFedAt       uint64 `json:"price_fed_at"`
	PriceAgeBlocks   uint64 `json:"price_age_blocks"`
	GammaPerTao      uint64 `json:"gamma_per_tao"`
	FloatRao         uint64 `json:"float_rao"`
	ServicePublished bool   `json:"service_published"`
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
		// a serving copy of gamma.yml (no billing_key_seed): nothing here bills
		result.Readiness = gammaNotConfigured
		gammaRecordReadiness(ctx, nil, gammaNotConfigured, gammaNotConfigured, nil)
		gammaReports.report("no_billing_key", func() {
			glog.Infof("[gamma]not configured: gamma.yml names no billing_key_seed, so this process does not bill\n")
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
	billing    *gammaAccount
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
		self.pause(result, gammaRuntimeUnsupported, err)
		return
	}

	self.readiness, self.mint = self.judge()

	scanned, scanErr := self.scan()
	result.Scanned = scanned
	if scanErr != nil {
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
		Netuid:         settings.Netuid,
		ServiceHotkey:  settings.ServiceAddress,
		Spender:        settings.SpenderAddress,
		ServicesUrl:    settings.ServicesUrl,
		BillingAddress: settings.BillingAddress,
		FinalizedBlock: self.headNumber,
		SpecVersion:    runtime.SpecVersion,
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

	service, err := self.chain.Service(ctx, runtime, at, settings.Netuid, settings.ServiceHotkey)
	if err != nil {
		return gammaChainUnreachable, mint
	}
	if service == nil {
		readiness = gammaServiceNotPublished
	} else {
		self.terms.ServicePublished = true
		if service.Spender != settings.Spender {
			readiness = gammaServiceSpenderMismatch
		} else if service.Url != settings.ServicesUrl {
			self.alarm("service_url/"+service.Url, gammaAlarmServiceChanged, nil, fmt.Sprintf("the published service's url is %q, not %q", service.Url, settings.ServicesUrl))
		}
	}
	proxies, err := self.chain.Proxies(ctx, runtime, at, settings.Spender)
	if err != nil {
		return gammaChainUnreachable, mint
	}
	proxyOk := false
	for _, proxy := range proxies {
		if proxy.Delegate == settings.BillingPublicKey && proxy.ProxyType == runtime.shape.gammaSpendProxyType && proxy.Delay == 0 {
			proxyOk = true
		}
	}
	billing, err := self.chain.Account(ctx, runtime, at, settings.BillingPublicKey)
	if err != nil {
		return gammaChainUnreachable, mint
	}
	self.billing = billing
	self.terms.FloatRao = billing.Usable()
	gammaFloatGauge.Set(float64(billing.Usable()))
	switch {
	case readiness != "":
	case !proxyOk:
		readiness = gammaBillingProxyMissing
	case billing.Usable() < settings.MinFloatRao:
		readiness = gammaFeeFloatLow
	default:
		readiness = gammaReady
	}
	return readiness, mint
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
	if err := block.Runtime.Supported(); err != nil {
		self.alarm(fmt.Sprintf("runtime/%d", block.Runtime.SpecVersion), gammaAlarmRuntimeUnsupported, gammaBlockRef(number), err.Error())
		return err
	}
	events, err := self.chain.Events(ctx, block)
	if err != nil {
		self.alarm(fmt.Sprintf("undecodable/%d", number), gammaAlarmUndecodableBlock, gammaBlockRef(number), err.Error())
		return fmt.Errorf("%w: block %d: %v", errGammaRuntimeUnsupported, number, err)
	}

	var spends []*gammaSpendEvent
	ourExtrinsics := map[int64]uint64{}
	transfers := map[string]uint64{}
	for _, event := range events {
		switch event.Name {
		case "SubtensorModule.GammaSpent", "SubtensorModule.GammaSpendSkipped":
			spend, err := gammaSpendEventFrom(block.Runtime, event)
			if err != nil {
				return fmt.Errorf("%w: block %d event %d: %v", errGammaRuntimeUnsupported, number, event.Index, err)
			}
			if spend.Spender == settings.Spender {
				spends = append(spends, spend)
			}
		case "TransactionPayment.TransactionFeePaid":
			who, err := gammaEventBytes32(event, "who")
			if err == nil && who == settings.BillingPublicKey {
				fee, _ := gammaEventUint(event, "actual_fee")
				ourExtrinsics[event.ExtrinsicIndex] = fee
			}
		case "SubtensorModule.GammaMinted", "SubtensorModule.GammaDepositSpenderSet",
			"SubtensorModule.GammaDepositDailySpendCapSet", "SubtensorModule.GammaDepositClosed":
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
		case "Proxy.ProxyAdded", "Proxy.ProxyRemoved":
			delegator, err := gammaEventBytes32(event, "delegator")
			if err == nil && delegator == settings.Spender {
				delegatee, _ := gammaEventBytes32(event, "delegatee")
				self.alarm(
					fmt.Sprintf("proxy/%d/%d", number, event.Index),
					gammaAlarmSpenderProxyChanged,
					gammaBlockRef(number),
					fmt.Sprintf("%s for delegate %s", event.Name, gammaAddress(delegatee)),
				)
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

	if 0 < len(ourExtrinsics) || 0 < len(spends) {
		if err := self.resolveBlock(block, ourExtrinsics, spends); err != nil {
			return err
		}
	}
	self.setCursor(number, block.Hash)
	return nil
}

// resolveBlock records, for one block, where our batches landed and what each
// of our spend entries did.
func (self *gammaBiller) resolveBlock(block *gammaBlock, ourExtrinsics map[int64]uint64, spends []*gammaSpendEvent) error {
	ctx := self.ctx
	settings := self.settings
	number := block.Number

	// our batches in this block, by extrinsic index
	batches := map[int64]*model.GammaSpendBatch{}
	if 0 < len(ourExtrinsics) {
		extrinsics, err := self.chain.Extrinsics(ctx, block.Hash)
		if err != nil {
			return err
		}
		for index, fee := range ourExtrinsics {
			if index < 0 || int64(len(extrinsics)) <= index {
				continue
			}
			hash := gammaExtrinsicHash(extrinsics[index]).Hex()
			batch := model.GetGammaSpendBatchByExtrinsicHash(ctx, hash)
			if batch == nil {
				self.alarm(
					fmt.Sprintf("billing_tx/%d/%d", number, index),
					gammaAlarmUnknownBillingTx,
					gammaBlockRef(number),
					fmt.Sprintf("extrinsic %s paid for by the billing key is none of our batches", hash),
				)
				continue
			}
			batch.FeeRao = gammaInt64Ref(int64(fee))
			batches[index] = batch
		}
	}

	references := [][]byte{}
	for _, spend := range spends {
		references = append(references, spend.Reference[:])
	}
	charges := model.GetGammaChargesByReferences(ctx, references)

	server.Tx(ctx, func(tx server.PgTx) {
		for _, spend := range spends {
			charge := charges[string(spend.Reference[:])]
			eventRef := fmt.Sprintf("%d/%d", number, spend.Event.Index)
			if charge == nil {
				self.alarm("reference/"+eventRef, gammaAlarmUnknownReference, gammaBlockRef(number), fmt.Sprintf(
					"%s from our spender with reference %s we never issued: %d units from %s",
					spend.Event.Name, gammaHex(spend.Reference[:]), spend.Amount, gammaAddress(spend.Hotkey),
				))
				continue
			}
			if charge.State != model.GammaChargePending {
				self.alarm("duplicate/"+eventRef, gammaAlarmDuplicateReference, gammaBlockRef(number), fmt.Sprintf(
					"%s with reference %s, whose charge is already %s",
					spend.Event.Name, gammaHex(spend.Reference[:]), charge.State,
				))
				continue
			}
			if spend.Netuid != settings.Netuid || gammaAddress(spend.Hotkey) != charge.Hotkey || int64(spend.Amount) != charge.Amount {
				self.alarm("mismatch/"+eventRef, gammaAlarmSpendMismatch, gammaBlockRef(number), fmt.Sprintf(
					"%s with reference %s spent %d units on netuid %d from %s; the charge is %d from %s",
					spend.Event.Name, gammaHex(spend.Reference[:]), spend.Amount, spend.Netuid, gammaAddress(spend.Hotkey), charge.Amount, charge.Hotkey,
				))
				continue
			}
			resolution := &model.GammaChargeResolution{
				State:       model.GammaChargeSpent,
				BlockNumber: int64(number),
				BlockHash:   block.Hash.Hex(),
				EventIndex:  spend.Event.Index,
			}
			if spend.Skipped {
				resolution.State = model.GammaChargeSkipped
				resolution.SkipReason = gammaSkipReasonCode(spend.Reason)
				// the deposit as of the skip is recorded at the first look
				// after it (charge)
			}
			if model.ResolveGammaChargeInTx(tx, ctx, spend.Reference[:], resolution, self.now) {
				gammaChargeResults.WithLabelValues(resolution.State, resolution.SkipReason).Inc()
				glog.Infof("[gamma]charge %s %s at block %d (%s)\n", gammaHex(spend.Reference[:]), resolution.State, number, resolution.SkipReason)
			}
		}
		for index, batch := range batches {
			fee := int64(0)
			if batch.FeeRao != nil {
				fee = *batch.FeeRao
			}
			if model.IncludeGammaSpendBatchInTx(tx, ctx, batch.BatchId, &model.GammaBatchInclusion{
				BlockNumber:    int64(number),
				BlockHash:      block.Hash.Hex(),
				ExtrinsicIndex: int(index),
				FeeRao:         fee,
			}, "", self.now) {
				gammaBatchFees.Observe(float64(fee))
				glog.Infof("[gamma]batch %s landed in block %d at %d (fee %d rao)\n", batch.BatchId, number, index, fee)
			}
		}
	}, server.TxReadCommitted)
	return nil
}

// gammaSkipReasonCode is the API's code for the chain's skip reason.
func gammaSkipReasonCode(reason string) string {
	switch reason {
	case "GammaDepositDoesNotExist":
		return GammaErrorCodeNoDeposit
	case "NotGammaDepositSpender":
		return GammaErrorCodeWrongSpender
	case "InsufficientGammaBalance":
		return GammaErrorCodeInsufficientGamma
	case "GammaDepositDailySpendCapExceeded":
		return GammaErrorCodeDailyCapExceeded
	case "AmountTooLow":
		return GammaErrorCodeInvalidAmount
	}
	return "skipped"
}

// ----- the live batch -----

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
		// every block the batch could have landed in is read and it is not
		// among them; the billing key's nonce must say the same
		if self.billing == nil {
			return
		}
		if int64(self.billing.Nonce) == batch.Nonce {
			if model.ExpireGammaSpendBatch(ctx, batch.BatchId, "the era ended without the batch landing", self.now) {
				glog.Infof("[gamma]batch %s expired; its charges go to the next batch\n", batch.BatchId)
			}
			return
		}
		note := fmt.Sprintf("the era ended and the batch was not seen, but the billing key's nonce is %d, not %d", self.billing.Nonce, batch.Nonce)
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
			observation.Exists = true
			observation.Coldkey = gammaAddress(deposit.Coldkey)
			observation.Spender = gammaAddress(deposit.Spender)
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
// for each intent its deposit can pay, and signs and submits one batch.
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
	for _, intent := range model.ListChargeableGammaPaymentIntents(ctx, self.now, gammaMaxSpendsPerCall) {
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

	charges := model.ListUnbatchedGammaCharges(ctx, gammaMaxSpendsPerCall)
	if len(charges) == 0 {
		return 0
	}
	if self.billing == nil {
		return 0
	}
	// the nonce at the finalized head; nothing of ours may be in the pool
	var poolNonce uint64
	if err := self.chain.rpc.Call(ctx, &poolNonce, "system_accountNextIndex", settings.BillingAddress); err == nil && poolNonce != uint64(self.billing.Nonce) {
		gammaReports.report("pool_nonce", func() {
			glog.Warningf("[gamma]the billing key's pool nonce is %d, its finalized nonce %d: waiting\n", poolNonce, self.billing.Nonce)
		})
		return 0
	}
	entries := []gammaSpendEntry{}
	references := [][]byte{}
	for _, charge := range charges {
		publicKey, err := model.DecodeBittensorAddress(charge.Hotkey)
		if err != nil || len(charge.Reference) != 32 || charge.Amount <= 0 {
			continue
		}
		var reference [32]byte
		copy(reference[:], charge.Reference)
		entries = append(entries, gammaSpendEntry{Hotkey: publicKey, Amount: uint64(charge.Amount), Reference: reference})
		references = append(references, charge.Reference)
	}
	if len(entries) == 0 {
		return 0
	}
	call, err := gammaSpendCall(self.runtime, settings.Netuid, settings.Spender, entries)
	if err != nil {
		glog.Errorf("[gamma]could not build the spend: %s\n", err)
		return 0
	}
	genesis, err := self.chain.GenesisHash(ctx)
	if err != nil {
		return 0
	}
	signed, err := gammaSignSpend(self.runtime, genesis, self.headNumber, self.head, settings.EraPeriod, self.billing.Nonce, call, settings.billingKey)
	if err != nil {
		glog.Errorf("[gamma]could not sign the spend: %s\n", err)
		return 0
	}
	fee, err := self.chain.QueryFee(ctx, signed.Encoded, self.head)
	if err != nil {
		gammaReports.report("fee", func() {
			glog.Warningf("[gamma]could not price the spend: %s\n", err)
		})
		return 0
	}
	if settings.MaxFeeRao < fee || self.billing.Usable() < fee {
		gammaReports.report("fee_limit", func() {
			glog.Errorf("[gamma]the spend would pay %d rao in fees (limit %d, float %d): not sent\n", fee, settings.MaxFeeRao, self.billing.Usable())
		})
		return 0
	}
	batch := &model.GammaSpendBatch{
		BatchId:       server.NewId(),
		Nonce:         int64(self.billing.Nonce),
		EraBirth:      int64(self.headNumber),
		EraPeriod:     int(settings.EraPeriod),
		Extrinsic:     signed.Encoded,
		ExtrinsicHash: signed.Hash.Hex(),
		CreateTime:    self.now,
	}
	// recorded before it is ever sent
	if err := model.CreateGammaSpendBatch(ctx, batch, references); err != nil {
		glog.Warningf("[gamma]batch not recorded: %s\n", err)
		return 0
	}
	if err := self.chain.Submit(ctx, signed.Encoded); err != nil && !gammaSubmitAccepted(err) {
		// the next run resubmits the recorded bytes while the era runs
		glog.Warningf("[gamma]batch %s submit: %s\n", batch.BatchId, err)
		return len(entries)
	}
	model.MarkGammaSpendBatchSubmitted(ctx, batch.BatchId, self.now)
	gammaBatches.Inc()
	glog.Infof("[gamma]batch %s: %d charges, nonce %d, fee %d rao\n", batch.BatchId, len(entries), batch.Nonce, fee)
	return len(entries)
}

// gammaDepositCanPay reports whether a hotkey's deposit, as last observed,
// names our spender and has amount spendable beyond what is already pending
// on it.
func gammaDepositCanPay(hotkey *model.GammaDepositHotkey, settings *gammaSettings, pending int64, amount int64) bool {
	if hotkey.ObservedBlock == nil || hotkey.DepositSpendable == nil || hotkey.DepositSpender == nil {
		return false
	}
	if *hotkey.DepositSpender != settings.SpenderAddress {
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
