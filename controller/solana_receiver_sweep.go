package controller

// The receiver leg of the Solana reconcile: every USDC transfer into our Solana
// Pay receivers (solanaReceiverAddresses) is examined once, from the chain.
//
// The Helius webhook is the fast path, and the chain leg
// (reconcileSolanaChainReferences) finds a lost delivery of an intent that is
// still open. Neither sees a payment whose delivery was lost after its intent
// was swept, or one sent with an unknown reference or none: between 2025-09
// and 2026-08 seven customer payments of 40 USDC reached our receivers and were
// never credited, and nothing on Main named them (support inbox 6584, 6692,
// 6729; factury support/plans/paid-not-applied-1112.md, "Sweep").
//
// The leg reads each receiver's USDC token accounts (getTokenAccountsByOwner)
// and their history (getSignaturesForAddress, finalized) forward from a
// durable cursor per token account (solana_receiver_sweep_cursor), oldest
// first. A transaction younger than the minimum age is left to the webhook for
// now. A transaction that a record already names (an intent it paid, a renewal
// or a balance keyed by it, a crypto receipt, an unfulfilled record) is passed
// over without a read. Every other one is read (getTransaction, jsonParsed)
// and, when it moved at least the dust amount of USDC into a receiver, put
// through the webhook's own decision and acted on as the chain leg acts:
//
//   - credited only when an intent matches it, through that intent's one-shot
//     (an open intent by its reference, or an unambiguous unique amount): the
//     credit the webhook would have made;
//   - otherwise recorded once in solana_unfulfilled_payment (no_intent with
//     the sender, the amount, the on-chain time, the account keys and why
//     nothing matched; underpaid for an open intent's shortfall), which the
//     payment-solana-unfulfilled alert reports and the operator tools
//     (bringyourctl crypto credit-usdc) and the reconcile's first leg act on.
//
// The leg never credits a payment no intent names: a payment off the record
// is the owner's to credit or return (support autonomy policy; crypto returns
// only on request). The receivers' history before the sweep start was swept
// by hand on 2026-11-12 (the plan above), so a first run starts there.
//
// The cursor is checkpointed at the last transaction examined whenever the
// leg stops (a call cap, a budget, a failure), so one failed transaction never
// restarts the read; it is read again first on the next run.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The receiver leg's tunables.
type SolanaReceiverSweepSettings struct {
	// A transaction younger than this is left to the webhook for now. Zero
	// examines every finalized transaction at once.
	MinAge time.Duration
	// A transfer of less USDC than this is not examined: address-poisoning
	// dust and wallet tests. The default is under every quote the server
	// makes; zero examines every transfer.
	DustUsd float64
	// A token account with no cursor yet is read back to this time; its
	// earlier history was swept by hand (2026-11-12). The zero time reads the
	// whole history.
	SweepFrom time.Time
	// The signatures one getSignaturesForAddress page asks for, at most 1000
	// (the RPC's limit).
	PageSize int
	// The pages one token account's read may take in a run. More history than
	// this since the cursor is an error for an operator: nothing of it is
	// acted on, since an older transaction would be passed over for good.
	MaxPages int
	// The RPC calls the leg takes from the Solana store's budget in one run,
	// so a backlog never starves the other legs; the next run goes on.
	MaxCallsPerRun int
}

// A new instance on every call.
func DefaultSolanaReceiverSweepSettings() *SolanaReceiverSweepSettings {
	return &SolanaReceiverSweepSettings{
		// past the webhook's own delivery and retries
		MinAge: 30 * time.Minute,
		// the hand sweep of 2026-11-12 classed every transfer under 0.5 USDC
		// as dust or a test; every quote is 5 USDC or more
		DustUsd: 0.5,
		// the end of the hand sweep's day
		SweepFrom: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
		PageSize:  1000,
		// ten thousand transactions of history in one run
		MaxPages: 10,
		// a fifth of the store's default budget (paymentReconcileApiBudget)
		MaxCallsPerRun: 100,
	}
}

// The cursor reads and writes and the on-record check. Replaceable only by
// tests.
var (
	solanaReceiverSweepCursor     = model.GetSolanaReceiverSweepCursorInConn
	solanaReceiverSweepSaveCursor = model.SetSolanaReceiverSweepCursorInConn
	solanaReceiverSweepOnRecord   = model.ListSolanaSignaturesOnRecordInConn
)

// A listed signature's block time; ok = false when the chain gave none.
func solanaSignatureTime(signature *solanaSignatureInfo) (blockTime time.Time, ok bool) {
	if signature.BlockTime == nil || *signature.BlockTime <= 0 {
		return time.Time{}, false
	}
	return time.Unix(*signature.BlockTime, 0).UTC(), true
}

// A token account's finalized signatures newer than its cursor, or with no
// cursor those at or after the sweep start, oldest first. baseSignature is
// where the read starts when there is no cursor: the newest signature before
// the sweep start, nil when the account has none. complete = false when the
// listing ran out of pages or calls before it reached the cursor (or the sweep
// start): nothing of it may be acted on, since an older signature it did not
// reach would be passed over for good.
func solanaReceiverSignaturesSince(
	ctx context.Context,
	rpc solanaChainRpc,
	tokenAccount string,
	cursor *model.SolanaReceiverSweepCursor,
	settings *SolanaReceiverSweepSettings,
	spend func() bool,
) (signatures []*solanaSignatureInfo, baseSignature *solanaSignatureInfo, complete bool, returnErr error) {
	signatures = []*solanaSignatureInfo{}
	beforeSignature := ""
	for page := 0; page < settings.MaxPages; page += 1 {
		if !spend() {
			return nil, nil, false, nil
		}
		options := map[string]any{
			"commitment": "finalized",
			"limit":      settings.PageSize,
		}
		if cursor != nil {
			options["until"] = cursor.Signature
		}
		if beforeSignature != "" {
			options["before"] = beforeSignature
		}
		var pageSignatures []*solanaSignatureInfo
		if err := rpc.call(ctx, "getSignaturesForAddress", []any{tokenAccount, options}, &pageSignatures); err != nil {
			return nil, nil, false, fmt.Errorf("getSignaturesForAddress: %w", err)
		}
		for _, signature := range pageSignatures {
			if signature == nil || signature.Signature == "" {
				continue
			}
			if cursor == nil {
				if blockTime, ok := solanaSignatureTime(signature); ok && blockTime.Before(settings.SweepFrom) {
					// swept by hand: everything from here back
					slices.Reverse(signatures)
					return signatures, signature, true, nil
				}
			}
			signatures = append(signatures, signature)
		}
		if len(pageSignatures) < settings.PageSize {
			slices.Reverse(signatures)
			return signatures, nil, true, nil
		}
		beforeSignature = pageSignatures[len(pageSignatures)-1].Signature
	}
	return nil, nil, false, nil
}

// The receiver leg (see the file header). It takes at most MaxCallsPerRun of
// the store's budget and keeps its own cursors, so a leg that ran out of calls
// continues next run without holding the store's watermark back; an RPC or
// write failure is an error event, which does.
func reconcileSolanaReceivers(run *paymentReconcileRun) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketSolana
	rpcUrl, ok := solanaChainRpcUrl()
	if !ok {
		return
	}
	settings := run.solanaReceiverSweepSettings
	if settings == nil {
		settings = DefaultSolanaReceiverSweepSettings()
	}
	rpc := solanaChainRpcFor(rpcUrl)
	lookups := solanaChainLookupsFor(run)
	storeDetailValues := run.storeDetail(store)

	callCount := 0
	spend := func() bool {
		if settings.MaxCallsPerRun <= callCount {
			storeDetailValues["receiver_calls_exhausted"] = true
			return false
		}
		if !run.spend(store) {
			return false
		}
		callCount += 1
		return true
	}
	fail := func(evidence string, err error) {
		run.record(store, model.PaymentReconcileActionError, nil, evidence, map[string]any{
			"error": err.Error(),
			"leg":   solanaReceiverLegName,
		})
	}
	// the receiver's USDC token accounts, sorted
	tokenAccountsOf := func(receiver string) ([]string, error) {
		var result struct {
			Value []struct {
				Pubkey string `json:"pubkey"`
			} `json:"value"`
		}
		if err := rpc.call(ctx, "getTokenAccountsByOwner", []any{
			receiver,
			map[string]any{"mint": solanaUsdcMint},
			map[string]any{"encoding": "jsonParsed", "commitment": "finalized"},
		}, &result); err != nil {
			return nil, err
		}
		tokenAccounts := []string{}
		for _, account := range result.Value {
			if account.Pubkey != "" && !slices.Contains(tokenAccounts, account.Pubkey) {
				tokenAccounts = append(tokenAccounts, account.Pubkey)
			}
		}
		slices.Sort(tokenAccounts)
		return tokenAccounts, nil
	}
	// the largest USDC transfer into one of our receivers: the amount the
	// webhook's decision judges
	receivedUsd := func(transaction *SolanaTransaction) float64 {
		amountUsd := 0.0
		for _, transfer := range solanaReceivedUsdcTransfers(transaction) {
			amountUsd = max(amountUsd, transfer.TokenAmount)
		}
		return amountUsd
	}

	readCount := 0
	defer func() {
		storeDetailValues["receiver_calls"] = callCount
		storeDetailValues["receiver_transactions"] = readCount
	}()

	for _, receiver := range solanaReceiverAddresses {
		if !spend() {
			return
		}
		tokenAccounts, err := tokenAccountsOf(receiver)
		if err != nil {
			fail(receiver, fmt.Errorf("getTokenAccountsByOwner: %w", err))
			continue
		}
		for _, tokenAccount := range tokenAccounts {
			cursor := solanaReceiverSweepCursor(run.connOwner, ctx, tokenAccount)
			signatures, baseSignature, complete, err := solanaReceiverSignaturesSince(ctx, rpc, tokenAccount, cursor, settings, spend)
			if err != nil {
				fail(tokenAccount, err)
				continue
			}
			if !complete {
				if callCount < settings.MaxCallsPerRun && 0 < run.budget {
					// the pages ran out, not the calls: more history since
					// the cursor than one run reads
					fail(tokenAccount, fmt.Errorf("more than %d pages of history since the cursor; raise the receiver leg's page limit", settings.MaxPages))
				}
				return
			}

			// where the cursor moves: the newest signature examined, in order
			var advancedCursor *model.SolanaReceiverSweepCursor
			if cursor == nil && baseSignature != nil {
				advancedCursor = solanaReceiverCursorAt(tokenAccount, receiver, baseSignature)
			}

			unreadSignatures := []string{}
			for _, signature := range signatures {
				if signature.Err == nil {
					unreadSignatures = append(unreadSignatures, signature.Signature)
				}
			}
			onRecordSignatures := solanaReceiverSweepOnRecord(run.connOwner, ctx, unreadSignatures)

			stopped := false
			for _, signature := range signatures {
				blockTime, ok := solanaSignatureTime(signature)
				if !ok || run.now.Sub(blockTime) < settings.MinAge {
					// left to the webhook for now; the next run reads on
					// from here
					break
				}
				if signature.Err == nil && !onRecordSignatures[signature.Signature] {
					if !spend() {
						stopped = true
						break
					}
					parsed, err := solanaFinalizedTransaction(ctx, rpc, signature.Signature)
					if err != nil {
						fail(tokenAccount, fmt.Errorf("getTransaction: %w", err))
						stopped = true
						break
					}
					if parsed == nil {
						// listed as finalized but not served yet: read again
						// next run
						stopped = true
						break
					}
					transaction := solanaTransactionFromParsed(signature.Signature, parsed)
					if transaction.Type == "TRANSFER" && settings.DustUsd <= receivedUsd(transaction) {
						readCount += 1
						run.examine(store)
						if !solanaChainAct(run, store, signature.Signature, solanaDecidePayment(transaction, lookups), solanaReceiverLegName) {
							stopped = true
							break
						}
					}
				}
				advancedCursor = solanaReceiverCursorAt(tokenAccount, receiver, signature)
			}

			if advancedCursor != nil && !run.dryRun {
				solanaReceiverSweepSaveCursor(run.connOwner, ctx, advancedCursor)
			}
			if stopped {
				return
			}
		}
	}
	if 0 < readCount {
		glog.Infof("[reconcile]solana: the receiver leg examined %d transfers\n", readCount)
	}
}

// A token account's cursor at a listed signature.
func solanaReceiverCursorAt(tokenAccount string, receiver string, signature *solanaSignatureInfo) *model.SolanaReceiverSweepCursor {
	cursor := &model.SolanaReceiverSweepCursor{
		TokenAccount: tokenAccount,
		Receiver:     receiver,
		Signature:    signature.Signature,
		Slot:         signature.Slot,
	}
	if blockTime, ok := solanaSignatureTime(signature); ok {
		cursor.BlockTime = &blockTime
	}
	return cursor
}
