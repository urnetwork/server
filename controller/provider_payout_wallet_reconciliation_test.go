// Ambiguous Circle submissions retain their original authority when a payout
// wallet is retired. These roots drive the public writer and final send guard.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Retire the original wallet through the actual incoming promotion path.
func controllerPromotePaymentWallet(t testing.TB, owner *session.ClientSession, payment *model.AccountPayment) server.Id {
	t.Helper()
	next := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: payment.NetworkId, Blockchain: "MATIC", WalletAddress: "0x0000000000000000000000000000000000000044", DefaultTokenType: "USDC"})
	if next == nil {
		t.Fatal("replacement wallet missing")
	}
	removed := model.RemoveWallet(*payment.WalletId, owner)
	if !removed.Success || removed.PayoutWalletId == nil || *removed.PayoutWalletId != *next {
		t.Fatal("original wallet was not retired and replacement promoted", removed)
	}
	if wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId); wallet == nil || wallet.Active {
		t.Fatal("original wallet remained active")
	}
	return *next
}

// Explicit limiter ordering models retirement while the original writer is
// waiting. The other case retires after the accepted response has been lost.
func controllerRetiredPaymentReconciles(t testing.TB, retireDuringAdmission bool) {
	t.Helper()
	cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
	controllerPayoutSchedule(t, cutoff)
	owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
	var original circleTransferArguments
	var originalRequest string
	var promoted server.Id
	submits, created, reads := 0, 0, 0
	lostAck := errors.New("synthetic processor accepted; response lost")
	client := controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
		args := circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}
		return circleTransferAfterAdmission(ctx, args, func(context.Context) error {
			if submits == 0 && retireDuringAdmission {
				promoted = controllerPromotePaymentWallet(t, owner, payment)
			}
			return nil
		}, func(context.Context) (*CreateTransferTransactionResult, error) {
			submits++
			if submits == 1 {
				original = args
				created++
				server.Db(owner.Ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(owner.Ctx, `SELECT event_details FROM audit_account_payment WHERE event_id=$1 AND payment_id=$2 AND event_type='circle_attempt_request'`, key, payment.PaymentId).Scan(&originalRequest))
				})
				return nil, lostAck
			}
			if args != original || amount != 9.99 || address != "0x0000000000000000000000000000000000000012" || network != "MATIC" {
				t.Fatal("retired retry changed the original processor request", args, original)
			}
			return &CreateTransferTransactionResult{Id: "synthetic-original-wallet-transfer", State: "INITIATED"}, nil
		})
	})
	args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
	if result, err := AdvancePayment(args, owner); !errors.Is(err, lostAck) || result.Complete || result.Canceled || submits != 1 {
		t.Fatal("original accepted uncertainty not retained", result, err, submits)
	}
	if !retireDuringAdmission {
		promoted = controllerPromotePaymentWallet(t, owner, payment)
	}
	retained, err := model.GetPayment(owner.Ctx, payment.PaymentId)
	if err != nil || retained.WalletId == nil || *retained.WalletId != *payment.WalletId || retained.PaymentRecord != nil || retained.CircleIdempotencyKey == nil || *retained.CircleIdempotencyKey != original.IdempotencyKey {
		t.Fatal("promotion reassigned an ambiguous attempt", retained, err)
	}
	// Each public call reloads the durable request; no local original is passed.
	if result, err := AdvancePayment(args, owner); err != nil || result.Complete || result.Canceled || submits != 2 || created != 1 {
		t.Fatal("retired original request failed idempotent reconciliation", result, err, submits, created)
	}
	client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
		reads++
		if id != "synthetic-original-wallet-transfer" {
			t.Fatal("reconciled a different processor transaction", id)
		}
		return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "COMPLETE", TxHash: "0xsynthetic-retired-wallet"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
	}
	for call := 0; call < 2; call++ {
		if result, err := AdvancePayment(args, owner); err != nil || !result.Complete || result.Canceled {
			t.Fatal("original pre-cutoff obligation did not complete", result, err)
		}
	}
	stored, err := model.GetPayment(owner.Ctx, payment.PaymentId)
	if err != nil || submits != 2 || created != 1 || reads != 1 || *stored.WalletId != *payment.WalletId || stored.Payout != payment.Payout || stored.PaymentPlanId != payment.PaymentPlanId {
		t.Fatal("completion changed original allocation or double-paid", stored, err, submits, created, reads)
	}
	server.Db(owner.Ctx, func(conn server.PgConn) {
		var currentWallet server.Id
		var requests int
		var unchanged bool
		server.Raise(conn.QueryRow(owner.Ctx, `SELECT (SELECT wallet_id FROM payout_wallet WHERE network_id=$1),
			(SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$2 AND event_type='circle_attempt_request'),
			(SELECT event_details=$4 FROM audit_account_payment WHERE event_id=$3)`, payment.NetworkId, payment.PaymentId, original.IdempotencyKey, originalRequest).Scan(&currentWallet, &requests, &unchanged))
		if currentWallet != promoted || requests != 1 || !unchanged {
			t.Fatal("reconciliation changed future wallet selection or original journal", currentWallet, requests, unchanged)
		}
	})
}

func TestProviderTransitionRetiredWalletReconcilesLostAcknowledgement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { controllerRetiredPaymentReconciles(t, false) })
}

func TestProviderTransitionWalletPromotionDuringAdmissionKeepsOriginalRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { controllerRetiredPaymentReconciles(t, true) })
}

// Historical key-only rows cannot tell a never-sent reservation from a lost
// response. Neither an active wallet nor promotion can replace the missing body.
func TestProviderTransitionMissingOriginalRequestRemainsUnknown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		for _, retired := range []bool{false, true} {
			owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
			wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
			basis, err := model.ReserveProviderPaymentBasis(owner.Ctx, payment, wallet)
			if err != nil {
				t.Fatal(err)
			}
			if retired {
				controllerPromotePaymentWallet(t, owner, payment)
				if err := model.RetainProviderPaymentRequest(owner.Ctx, basis, 9.99, "MATIC"); !errors.Is(err, model.ErrProviderPaymentAttemptChanged) {
					t.Fatal("retired wallet manufactured a first request", err)
				}
			}
			sends := 0
			controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
				sends++
				return nil, errors.New("unexpected send without an original")
			})
			result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
			if !errors.Is(err, model.ErrProviderPaymentAttemptChanged) || result.Complete || result.Canceled || sends != 0 {
				t.Fatal("missing original was guessed or released", retired, result, err, sends)
			}
			stored, err := model.GetPayment(owner.Ctx, payment.PaymentId)
			if err != nil || stored.CircleIdempotencyKey == nil || *stored.CircleIdempotencyKey != basis.IdempotencyKey || *stored.WalletId != basis.WalletId || stored.PaymentRecord != nil {
				t.Fatal("unknown original markers were changed", stored, err)
			}
			server.Db(owner.Ctx, func(conn server.PgConn) {
				var requests int
				server.Raise(conn.QueryRow(owner.Ctx, `SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_type='circle_attempt_request'`, payment.PaymentId).Scan(&requests))
				if requests != 0 {
					t.Fatal("unknown original was fabricated")
				}
			})
		}
	})
}

// Paired forged context and HTTP arguments must still match the stored body.
func TestProviderTransitionRetiredRequestBindsEveryOriginalField(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
		original, err := model.ReserveProviderPaymentRequest(owner.Ctx, payment, wallet, 9.99, "MATIC")
		if err != nil {
			t.Fatal(err)
		}
		controllerPromotePaymentWallet(t, owner, payment)
		for _, field := range []string{"payment", "key", "network_id", "wallet", "payout", "address", "blockchain", "amount", "network"} {
			changed := *original
			switch field {
			case "payment":
				changed.Basis.PaymentId = server.NewId()
			case "key":
				changed.Basis.IdempotencyKey = server.NewId()
			case "network_id":
				changed.Basis.NetworkId = server.NewId()
			case "wallet":
				changed.Basis.WalletId = server.NewId()
			case "payout":
				changed.Basis.Payout++
			case "address":
				changed.Basis.WalletAddress = "0x0000000000000000000000000000000000000044"
			case "blockchain":
				changed.Basis.Blockchain = "SOL"
			case "amount":
				changed.Amount = 10.99
			case "network":
				changed.Network = "SOL"
			}
			ctx := context.WithValue(owner.Ctx, providerUsdcPaymentContextKey{}, providerPaymentSubmission{Basis: changed.Basis, Amount: changed.Amount, Network: changed.Network})
			args := circleTransferArguments{IdempotencyKey: changed.Basis.IdempotencyKey, Amount: changed.Amount, Destination: changed.Basis.WalletAddress, Network: changed.Network}
			waits, sends := 0, 0
			_, err := circleTransferAfterAdmission(ctx, args, func(context.Context) error { waits++; return nil }, func(context.Context) (*CreateTransferTransactionResult, error) { sends++; return nil, nil })
			if err == nil || waits != 1 || sends != 0 {
				t.Fatal("changed retained request reached send", field, err, sends)
			}
			if _, err := (&CoreCircleApiClient{}).CreateTransferTransaction(ctx, args.IdempotencyKey, args.Amount, args.Destination, args.Network); err == nil {
				t.Fatal("Core admitted changed retained request before secrets", field)
			}
		}
		if err := model.RequireProviderPaymentRequest(owner.Ctx, &original.Basis, original.Amount, original.Network); err != nil {
			t.Fatal("exact inactive-wallet request was refused", err)
		}
	})
}

// Stored malformed, oversized or noncanonical originals stay in custody and
// refuse before the processor; retry never overwrites them with current values.
func TestProviderTransitionInvalidOriginalRequestRemainsRetained(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
		request, err := model.ReserveProviderPaymentRequest(owner.Ctx, payment, wallet, 9.99, "MATIC")
		if err != nil {
			t.Fatal(err)
		}
		controllerPromotePaymentWallet(t, owner, payment)
		canonical, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		changed := *request
		changed.Basis.WalletId = server.NewId()
		mismatched, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		sends := 0
		controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return nil, nil
		})
		for _, body := range []string{"{", " " + string(canonical), string(mismatched), strings.Repeat(" ", 1024*1024+1)} {
			server.Tx(owner.Ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE audit_account_payment SET event_details=$2 WHERE event_id=$1`, request.Basis.IdempotencyKey, body))
			})
			result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
			if !errors.Is(err, model.ErrProviderPaymentAttemptChanged) || result.Complete || result.Canceled || sends != 0 {
				t.Fatal("invalid original was rebuilt or submitted", result, err, sends)
			}
			server.Db(owner.Ctx, func(conn server.PgConn) {
				var unchanged bool
				server.Raise(conn.QueryRow(owner.Ctx, `SELECT event_details=$2 FROM audit_account_payment WHERE event_id=$1`, request.Basis.IdempotencyKey, body).Scan(&unchanged))
				if !unchanged {
					t.Fatal("invalid original was erased")
				}
			})
		}
	})
}

// A journal refusal must roll back the key, making a later fresh retry possible.
func TestProviderTransitionFreshRequestJournalFailureRollsBackKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `CREATE FUNCTION synthetic_refuse_fresh_request() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
			CREATE TRIGGER synthetic_refuse_fresh_request BEFORE INSERT ON audit_account_payment FOR EACH ROW WHEN(NEW.event_type='circle_attempt_request') EXECUTE FUNCTION synthetic_refuse_fresh_request()`))
		})
		sends := 0
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				t.Fatal("successful retry had no durable exact request", err)
			}
			sends++
			return nil, errors.New("synthetic retry response lost")
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if !errors.Is(err, model.ErrProviderPaymentAttemptChanged) || result.Complete || result.Canceled || sends != 0 {
			t.Fatal("journal refusal crossed processor boundary", result, err, sends)
		}
		stored, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || stored.CircleIdempotencyKey != nil || stored.PaymentRecord != nil {
			t.Fatal("journal refusal left a key-only crash window", stored, err)
		}
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `DROP TRIGGER synthetic_refuse_fresh_request ON audit_account_payment; DROP FUNCTION synthetic_refuse_fresh_request()`))
		})
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 1 {
			t.Fatal("valid fresh retry did not reach its exact durable request", err, sends)
		}
	})
}

// An old request does not override subsequently selected earning authority.
func TestProviderTransitionRetiredRequestCannotAuthorizeCutoffFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		owner, payment := controllerPayoutFixture(t, cutoff)
		sends := 0
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				return nil, err
			}
			sends++
			return nil, errors.New("synthetic pre-policy uncertain response")
		})
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 1 {
			t.Fatal("old-policy original was not retained", err, sends)
		}
		before, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil {
			t.Fatal(err)
		}
		controllerPromotePaymentWallet(t, owner, payment)
		controllerPayoutSchedule(t, cutoff)
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if !errors.Is(err, model.ErrProviderUsdcAttributionUnresolved) || result.Complete || result.Canceled || sends != 1 {
			t.Fatal("retained request authorized inclusive-cutoff USDC fallback", result, err, sends)
		}
		stored, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || stored.CircleIdempotencyKey == nil || *stored.CircleIdempotencyKey != *before.CircleIdempotencyKey || stored.PaymentRecord != nil || *stored.WalletId != *payment.WalletId {
			t.Fatal("refusal released the original uncertain attempt", stored, err)
		}
	})
}

// A peer reserves after the public read but before fresh admission. The stale
// worker must reload that peer's exact body instead of recomputing its amount.
func TestProviderTransitionConcurrentFreshReservationReloadsOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		originalCtx := owner.Ctx
		defer func() { owner.Ctx = originalCtx }()
		var peer *model.ProviderPaymentRequest
		owner.Ctx = context.WithValue(originalCtx, providerPaymentReadObserverKey{}, func(observed *model.AccountPayment) {
			if observed.CircleIdempotencyKey != nil {
				t.Fatal("barrier did not retain the original unreserved read")
			}
			wallet := model.GetAccountWallet(originalCtx, *observed.WalletId)
			var err error
			peer, err = model.ReserveProviderPaymentRequest(originalCtx, observed, wallet, 9.98, "MATIC")
			if err != nil {
				t.Fatal(err)
			}
		})
		sends := 0
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				return nil, err
			}
			if key != peer.Basis.IdempotencyKey || amount != peer.Amount || address != peer.Basis.WalletAddress || network != peer.Network {
				t.Fatal("retry replaced the peer's original request")
			}
			sends++
			return nil, errors.New("synthetic peer response unknown")
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if !errors.Is(err, model.ErrProviderPaymentBasisChanged) || result.Complete || result.Canceled || sends != 0 || peer == nil {
			t.Fatal("stale fresh admission did not yield to the original reservation", result, err, sends)
		}
		owner.Ctx = originalCtx
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 1 {
			t.Fatal("peer's original request was not reloaded", err, sends)
		}
	})
}

// Retirement before any request exists is not send authority. A new public
// read may select the promoted wallet only because no attempt was committed.
func TestProviderTransitionInactiveFreshWalletCannotCreateRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		originalCtx := owner.Ctx
		defer func() { owner.Ctx = originalCtx }()
		owner.Ctx = context.WithValue(originalCtx, providerPaymentReadObserverKey{}, func(*model.AccountPayment) {
			controllerPromotePaymentWallet(t, owner, payment)
		})
		sends := 0
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				return nil, err
			}
			if address != "0x0000000000000000000000000000000000000044" {
				t.Fatal("inactive fresh wallet reached the processor", address)
			}
			sends++
			return nil, errors.New("synthetic new-wallet response unknown")
		})
		if result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || result.Complete || result.Canceled || sends != 0 {
			t.Fatal("inactive fresh wallet acquired submission authority", result, err, sends)
		}
		stored, err := model.GetPayment(originalCtx, payment.PaymentId)
		if err != nil || stored.CircleIdempotencyKey != nil || stored.PaymentRecord != nil {
			t.Fatal("inactive fresh refusal reserved an attempt", stored, err)
		}
		owner.Ctx = originalCtx
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 1 {
			t.Fatal("unreserved retry did not select the promoted wallet", err, sends)
		}
	})
}

// A late original acknowledgement wins before the retry's final send check.
// The next public call must GET that record instead of submitting again.
func TestProviderTransitionRetiredRetryReconcilesLateAcceptance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		calls, sends, reads := 0, 0, 0
		client := controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			calls++
			args := circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}
			return circleTransferAfterAdmission(ctx, args, func(context.Context) error {
				if calls == 2 {
					original := ctx.Value(providerUsdcPaymentContextKey{}).(providerPaymentSubmission)
					return model.SetProviderPaymentRecord(owner.Ctx, &original.Basis, amount, "synthetic-late-original-acceptance")
				}
				return nil
			}, func(context.Context) (*CreateTransferTransactionResult, error) {
				sends++
				return nil, errors.New("synthetic original acknowledgement lost")
			})
		})
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		if _, err := AdvancePayment(args, owner); err == nil || sends != 1 {
			t.Fatal("initial unknown submission missing", err, sends)
		}
		controllerPromotePaymentWallet(t, owner, payment)
		if result, err := AdvancePayment(args, owner); !errors.Is(err, model.ErrProviderPaymentBasisChanged) || result.Complete || result.Canceled || sends != 1 || calls != 2 {
			t.Fatal("late acceptance did not fence the retired retry", result, err, calls, sends)
		}
		client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
			reads++
			if id != "synthetic-late-original-acceptance" {
				t.Fatal("late original acknowledgement was replaced", id)
			}
			return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "COMPLETE", TxHash: "0xsynthetic-late-acceptance"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		if result, err := AdvancePayment(args, owner); err != nil || !result.Complete || result.Canceled || sends != 1 || reads != 1 || calls != 2 {
			t.Fatal("late accepted original failed GET reconciliation", result, err, calls, sends, reads)
		}
	})
}
