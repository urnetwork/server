package controller

// The stores' compute purchases (DESIGN-COMPUTE §9.5, §9.11, §9.12, D-BAL-8; BAL-M12-09, BAL-M12-10): what the App
// Store's and Google Play's compute paths share (compute_store_apple.go, compute_store_play.go).
//
//	ComputeStoreCredit       the one place a store's compute purchase becomes a lot: the network checked to exist
//	                         under the payment network lock's read, then sidecar/controller's ComputeCreditLot as a
//	                         store purchase (kind purchase for a consumable, subscription for a month's grant; market
//	                         app_store or google_play; market_ref the App Store's transaction id or Play's order id,
//	                         never the original transaction id or the purchase token, which renewals share), so a
//	                         report and a notification of one purchase credit it once, whichever comes first. The
//	                         store took the money before its payer was known, so the purchase is credited whatever the
//	                         limits say, and one past its payer's or network's limits is flagged for review (§9.5).
//	                         A network that is gone credits nothing: Play refunds it through its orders API, and on the
//	                         App Store, where only Apple refunds, support answers it from the recorded event.
//	ComputeStoreReverse      a store's refund, revocation or voided purchase reverses its lot (ComputeReverseLot), what
//	                         it paid for staying charged, so the account may go negative (§9.12).
//	computeStorePlanChange   §9.5's plan change taking effect at once: the new plan's credits less the old plan's for
//	                         the part of its period the store gives back, so stepping through the plans in one sitting
//	                         grants about the largest plan's credits, not their sum.
//
// A store's products are sidecar's ComputeStoreProducts, the compute.* family, which neither store path grants Pro
// for, as Pro's products credit no compute.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/urnetwork/glog"
	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// What one store path reads the ledger and the network from. A field left nil takes the process's own: operator's
// global Env, the vault's ledger keys, the default settings, MG-C1 linked, and server's network table.
type ComputeStoreDeps struct {
	Env      func() operator.Env
	Secrets  func() sidecarcontroller.ComputeLedgerSecrets
	Ledger   *sidecarmodel.ComputeLedgerSettings
	Products *sidecar.ComputeStoreProductSettings
	// Whether this binary links MG-C1, the ledger's tables: false credits and reverses nothing, answering an error so
	// the store or the app asks again.
	Active func() bool
	// Whether a network still exists, read under the payment network lock as Pro's credits read it.
	NetworkExists func(ctx context.Context, networkId server.Id) (bool, error)
}

// The deps with the process's own in every field left nil.
func computeStoreDepsOf(deps *ComputeStoreDeps) ComputeStoreDeps {
	filled := ComputeStoreDeps{}
	if deps != nil {
		filled = *deps
	}
	if filled.Env == nil {
		filled.Env = operator.GlobalEnv
	}
	if filled.Secrets == nil {
		secrets := NewComputeServerSecrets(nil)
		filled.Secrets = func() sidecarcontroller.ComputeLedgerSecrets { return secrets }
	}
	if filled.Ledger == nil {
		filled.Ledger = sidecarmodel.DefaultComputeLedgerSettings()
	}
	if filled.Products == nil {
		filled.Products = sidecar.DefaultComputeStoreProductSettings()
	}
	if filled.Active == nil {
		filled.Active = server.SidecarMgC1Linked
	}
	if filled.NetworkExists == nil {
		filled.NetworkExists = computeStoreNetworkExists
	}
	return filled
}

// Whether networkId's row stands, read under the payment network lock in a transaction of its own.
func computeStoreNetworkExists(ctx context.Context, networkId server.Id) (exists bool, returnErr error) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			err := model.LockPaymentNetworkInTx(tx, ctx, networkId)
			if errors.Is(err, model.ErrPaymentNetworkNotFound) {
				exists = false
				return
			}
			server.Raise(err)
			exists = true
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// The ledger is not in this binary: compute purchases wait for one that links MG-C1.
var errComputeStoreInactive = errors.New("compute store: this binary links no MG-C1, so a compute purchase waits for one that does")

// One store purchase to credit (§9.5).
type ComputeStorePurchase struct {
	// app_store or google_play.
	Market    sidecarmodel.ComputeMarket
	NetworkId server.Id
	// The App Store's transaction id or Play's order id: new at every purchase and renewal.
	MarketRef string
	Product   *sidecar.ComputeStoreProduct
	// What it credits: the product's credits, less an immediate plan change's give-back (computeStorePlanChange).
	AmountNanoUsd int64
	// The App Store account's keyed hash (appTransactionId); nil for a Play purchase, which counts to its network alone.
	PayerHash []byte
}

// What a store credit did.
type ComputeStoreCreditAnswer struct {
	// The lot of the purchase's reference: credited now, or by the report or notification that came first.
	Lot *sidecarmodel.ComputeLot
	// This call credited it.
	Credited bool
	// Past a limit: credited and flagged for review.
	Flagged bool
	// The network is gone, or its account closed: nothing credited.
	Gone bool
	// A plan change whose give-back covers the new plan: nothing to credit, and nothing owed.
	Nothing bool
}

// Credits purchase (see the file header).
func ComputeStoreCredit(env operator.Env, ctx context.Context, deps *ComputeStoreDeps, purchase *ComputeStorePurchase) (*ComputeStoreCreditAnswer, error) {
	filled := computeStoreDepsOf(deps)
	if purchase.Market != sidecarmodel.ComputeMarketAppStore && purchase.Market != sidecarmodel.ComputeMarketGooglePlay {
		return nil, fmt.Errorf("%w: a store purchase is the App Store's or Play's", sidecarcontroller.ErrComputeLedgerArgs)
	}
	if purchase.Product == nil || purchase.MarketRef == "" {
		return nil, fmt.Errorf("%w: a store purchase names its product and its reference", sidecarcontroller.ErrComputeLedgerArgs)
	}
	if !filled.Active() {
		return nil, errComputeStoreInactive
	}
	exists, err := filled.NetworkExists(ctx, purchase.NetworkId)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &ComputeStoreCreditAnswer{Gone: true}, nil
	}
	if purchase.AmountNanoUsd <= 0 {
		return &ComputeStoreCreditAnswer{Nothing: true}, nil
	}
	kind := sidecarmodel.ComputeLotKindPurchase
	if purchase.Product.Monthly {
		kind = sidecarmodel.ComputeLotKindSubscription
	}
	answer, err := sidecarcontroller.ComputeCreditLot(env, ctx, filled.Ledger, &sidecarcontroller.ComputeCreditRequest{
		NetworkId:     purchase.NetworkId,
		Kind:          kind,
		Market:        purchase.Market,
		MarketRef:     purchase.MarketRef,
		AmountNanoUsd: purchase.AmountNanoUsd,
		PayerHash:     purchase.PayerHash,
		Store:         true,
	})
	if errors.Is(err, sidecarcontroller.ErrComputeAccountClosed) {
		return &ComputeStoreCreditAnswer{Gone: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ComputeStoreCreditAnswer{Lot: answer.Lot, Credited: answer.Credited, Flagged: answer.Flagged}, nil
}

// Reverses the lot of a store's refunded, revoked or voided purchase (§9.12): nil where none was credited for it.
func ComputeStoreReverse(env operator.Env, ctx context.Context, deps *ComputeStoreDeps, market sidecarmodel.ComputeMarket, marketRef string) (*sidecarcontroller.ComputeReverseAnswer, error) {
	filled := computeStoreDepsOf(deps)
	if !filled.Active() {
		return nil, errComputeStoreInactive
	}
	return sidecarcontroller.ComputeReverseLot(env, ctx, filled.Ledger, &sidecarcontroller.ComputeReverseRequest{Market: market, MarketRef: marketRef})
}

// What an immediate plan change credits (§9.5): the new plan's credits less the old plan's for the part of its
// period, oldStart to oldEnd, that the change at changeTime gives back; nothing below zero. A change outside the old
// period gives nothing back, and the new plan is credited whole.
func computeStorePlanChange(newCredits int64, oldCredits int64, oldStart time.Time, oldEnd time.Time, changeTime time.Time) int64 {
	full := newCredits * sidecarmodel.ComputeCreditNanoUsd
	period := oldEnd.Sub(oldStart)
	unused := oldEnd.Sub(changeTime)
	if period <= 0 || unused <= 0 {
		return full
	}
	unused = min(unused, period)
	// the old plan's credits times the part given back, exactly: credits in nano-USD times nanoseconds passes int64
	deduction := new(big.Int).Mul(big.NewInt(oldCredits*sidecarmodel.ComputeCreditNanoUsd), big.NewInt(int64(unused)))
	deduction.Quo(deduction, big.NewInt(int64(period)))
	return max(full-deduction.Int64(), 0)
}

// Records, once per store object, a compute purchase no live network can be credited for, for support: the App
// Store's, which only Apple refunds, or a Play purchase its refund failed for. Never fails the caller.
func computeStoreRecordUnowned(ctx context.Context, market model.SubscriptionMarket, evidence string, reason string, networkId *server.Id) {
	added, err := model.AddPaymentReconciliationEventOnce(ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     string(market),
		NetworkId: networkId,
		Action:    model.PaymentReconcileActionCreditUnfulfillable,
		Evidence:  evidence,
		Details:   map[string]any{"reason": reason, "leg": "compute"},
	})
	if err != nil {
		glog.Errorf("[compute]store purchase %s: could not record the unowned payment: %s\n", evidence, err)
		return
	}
	if added {
		glog.Errorf("[compute]store purchase %s is paid and no live network can be credited (%s); recorded for support\n", evidence, reason)
	}
}
