package controller

// Compute's refunds on Stripe (DESIGN-COMPUTE §9.12; BAL-M12-14): sidecar/controller's ComputeStripeRefunds on
// ComputeServerStripe, and the network removal's compute step.
//
//	RefundLot                   a lot's money back as a partial refund on its PaymentIntent, a self-serve refund's or a
//	                            closure's, once a lot: a refund of the PaymentIntent whose metadata names the lot is
//	                            the one asked before, and no second is made, whenever a retry comes; a new one goes
//	                            under an idempotency key of the lot. Its metadata names it the ledger's
//	                            (compute_refund), so its charge.refunded books nothing more (compute_stripe_webhook.go)
//	networkRemoveCloseCompute   the closure step of network removal (network_remove_providers.go), before the network
//	                            is removed while its billing contacts still read: a network with a compute account has
//	                            it closed (sidecar/controller's ComputeCloseNetwork), its unspent Stripe credit within
//	                            90 days of purchase refunded, the rest forfeited with notice, its plan cancelled; one
//	                            with none, as every network before compute ships, has nothing to close
//
// InstallComputeRefunds registers the process's refunds with sidecar/controller once, for the api's compute routes and
// network removal.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/stripe/stripe-go/v82"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// On a refund the ledger makes of a lot: the lot it returns, by which a retry finds the refund made before.
const computeStripeMetadataRefundLot = "compute_refund_lot"

var _ sidecarcontroller.ComputeStripeRefunds = (*ComputeServerStripe)(nil)

// See sidecarcontroller.ComputeStripeRefunds (and the file header).
func (self *ComputeServerStripe) RefundLot(ctx context.Context, request *sidecarcontroller.ComputeStripeLotRefund) error {
	if request.PaymentIntentId == "" || request.AmountCents <= 0 || request.Reason == "" {
		return fmt.Errorf("%w: a refund names its PaymentIntent, a positive amount and its reason", sidecarcontroller.ErrComputeRefundArgs)
	}
	client, err := self.client()
	if err != nil {
		return err
	}
	params := &stripe.RefundListParams{PaymentIntent: stripe.String(request.PaymentIntentId)}
	params.Limit = stripe.Int64(self.settings.RefundListMaxCount)
	read := int64(0)
	for refund, err := range client.V1Refunds.List(ctx, params) {
		if err != nil {
			return fmt.Errorf("compute stripe: the refunds of %s: %w", request.PaymentIntentId, err)
		}
		if refund.Metadata[computeStripeMetadataRefundLot] == request.LotId.String() {
			return nil
		}
		read += 1
		if self.settings.RefundListMaxCount <= read {
			break
		}
	}
	create := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(request.PaymentIntentId),
		Amount:        stripe.Int64(request.AmountCents),
		Metadata: map[string]string{
			ComputeStripeMetadataRefund:    request.Reason,
			computeStripeMetadataRefundLot: request.LotId.String(),
		},
	}
	create.SetIdempotencyKey("compute_refund_lot_" + request.LotId.String())
	if _, err := client.V1Refunds.Create(ctx, create); err != nil {
		return fmt.Errorf("compute stripe: the refund of %s: %w", request.PaymentIntentId, err)
	}
	return nil
}

var installComputeRefundsOnce sync.Once

// Registers the process's Stripe refunds with sidecar/controller, once a process: a second call does nothing.
func InstallComputeRefunds() {
	installComputeRefundsOnce.Do(func() {
		sidecarcontroller.RegisterComputeStripeRefunds(computeStripeProcess())
	})
}

// The env compute's closure runs on: the process's, which core.SetProcessEnv set at its start. A test replaces it.
var networkRemoveComputeEnv = operator.GlobalEnv

// Whether networkId has a compute account to close, read through server's own pool, so a network that never held
// compute credit closes nothing whatever the process's env holds.
func computeNetworkHasAccount(ctx context.Context, networkId server.Id) (bool, error) {
	has := false
	var readErr error
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM compute_account WHERE network_id = $1)`, networkId).Scan(&has))
		})
	}, func(err error) {
		readErr = err
	})
	return has, readErr
}

// The closure step of network removal (see the file header): an error refuses the removal, so the person retries.
func networkRemoveCloseCompute(clientSession *session.ClientSession) error {
	if !server.SidecarMgC1Linked() {
		return nil
	}
	networkId := clientSession.ByJwt.NetworkId
	has, err := computeNetworkHasAccount(clientSession.Ctx, networkId)
	if err != nil || !has {
		return err
	}
	InstallComputeRefunds()
	InstallComputeAutoRenew()
	InstallComputeNotices()
	_, err = sidecarcontroller.ComputeCloseNetwork(networkRemoveComputeEnv(), clientSession.Ctx, sidecarmodel.DefaultComputeLedgerSettings(), networkId)
	if errors.Is(err, sidecarcontroller.ErrComputeStripeRefundUnavailable) || errors.Is(err, sidecarcontroller.ErrComputeStripeRenewUnavailable) {
		return fmt.Errorf("compute's Stripe is not on this host: %w", err)
	}
	return err
}
