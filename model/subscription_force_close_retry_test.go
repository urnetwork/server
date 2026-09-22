// Separates verified sibling progress from durable accounting rejections.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The behavioral contract is compile-valid before the product introduces its
// positive typed summary; ordinary or mixed errors cannot satisfy it.
type forceCloseAccountingProgress interface {
	error
	VerifiedCloseCount() int64
	AccountingRejectionCount() int64
}

// A completed batch must identify independently verified siblings without
// declaring the rejected dispute settled or discarding its reservation.
func TestForceCloseAccountingRejectionReportsVerifiedProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*escrow, escrow)
		good := newForceCloseDisputeFixture(t, ctx, true, true, 1024, 1024, escrow)
		before := bad.state(t, ctx)
		selected, err := ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 2, 1, 0)
		if selected != 2 || err == nil || !strings.Contains(err.Error(), "Escrow does not have enough value") {
			t.Fatal("the actual bounded batch did not preserve its accounting rejection")
		}
		if bad.state(t, ctx) != before {
			t.Fatal("isolated rejection changed disputed accounting or stream state")
		}
		if state := good.state(t, ctx); state.outcome != ContractOutcomeSettled || state.dispute || state.open || !state.escrowSettled || state.streamFound {
			t.Fatal("valid sibling failed terminal verification or stream cleanup")
		}
		var progress forceCloseAccountingProgress
		if !errors.As(err, &progress) {
			t.Fatal("completed accounting-only rejection lacks positive typed progress authority")
		}
		if progress.VerifiedCloseCount() != 1 || progress.AccountingRejectionCount() != 1 {
			t.Fatal("selected candidates were confused with verified completed siblings")
		}
	})
}

// An idle batch containing only the unresolved dispute remains an error with
// zero successful progress; it must not inherit a full-checkpoint cadence.
func TestForceCloseAccountingRejectionReportsZeroProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*escrow, escrow)
		before := bad.state(t, ctx)
		for pass := 0; pass < 2; pass++ {
			selected, err := ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 1, 1, 0)
			if selected != 1 || err == nil || bad.state(t, ctx) != before {
				t.Fatal("repeat rejection did not retain the same reserved dispute")
			}
			var progress forceCloseAccountingProgress
			if !errors.As(err, &progress) {
				t.Fatal("accounting-only idle batch lacks positive typed rejection authority")
			}
			if progress.VerifiedCloseCount() != 0 || progress.AccountingRejectionCount() != 1 {
				t.Fatal("rejected candidate was counted as completed progress")
			}
		}
	})
}

// A known escrow rejection already quarantined and independently verified
// terminal cannot park the unrelated sweep behind its old error count. Keep
// its diagnostic and unpaid accounting distinct from a successful settlement.
func TestForceCloseVerifiedQuarantineDoesNotParkAccountingBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*escrow, escrow)
		malformed := newForceCloseDisputeFixture(t, ctx, true, true, 2*escrow, 2*escrow, escrow)
		good := newForceCloseDisputeFixture(t, ctx, true, true, 1024, 1024, escrow)
		before := bad.state(t, ctx)
		malformedBefore := malformed.state(t, ctx)
		selected, err := ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 2, 1, 0)
		if selected != 3 || err == nil || bad.state(t, ctx) != before {
			t.Fatal("mixed failure lost the actual failed accounting boundary")
		}
		var progress forceCloseAccountingProgress
		if !errors.As(err, &progress) || progress.VerifiedCloseCount() != 2 || progress.AccountingRejectionCount() != 1 {
			t.Fatal("terminal-verified accounting quarantine parked unrelated cleanup work")
		}
		var accounting *ForceCloseAccountingError
		if !errors.As(err, &accounting) || accounting.QuarantinedAccountingRejectionCount() != 1 {
			t.Fatal("no-payout quarantine was counted as ordinary settlement")
		}
		if strings.Count(err.Error(), errContractInsufficientEscrow.Error()) != 2 || !strings.Contains(err.Error(), "contract remained non-final") {
			t.Fatal("verified quarantine hid either original accounting failure")
		}
		quarantined := malformed.state(t, ctx)
		if quarantined.outcome != ContractOutcomeSettled || quarantined.dispute || quarantined.open || quarantined.streamFound ||
			quarantined.escrowSettled || quarantined.escrowPayoutByteCount != malformedBefore.escrowPayoutByteCount ||
			quarantined.payerBalanceByteCount != malformedBefore.payerBalanceByteCount ||
			quarantined.providerPayoutByteCount != malformedBefore.providerPayoutByteCount || quarantined.netEscrowByteCount != 0 ||
			quarantined.sourceByteCount != malformedBefore.sourceByteCount || quarantined.destinationByteCount != malformedBefore.destinationByteCount {
			t.Fatal("verified quarantine changed the existing no-payout accounting policy")
		}
		if state := good.state(t, ctx); state.outcome != ContractOutcomeSettled || state.streamFound || !state.escrowSettled || state.escrowPayoutByteCount != 1024 {
			t.Fatal("valid sibling failed ordinary settlement")
		}
		selected, err = ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 2, 1, 0)
		if selected != 1 || !errors.As(err, &progress) || progress.VerifiedCloseCount() != 0 || progress.AccountingRejectionCount() != 1 ||
			bad.state(t, ctx) != before || malformed.state(t, ctx) != quarantined {
			t.Fatal("retry repeated terminal accounting or lost the still-reserved dispute")
		}
	})
}

// Even a quarantine-only page must checkpoint promptly with an error, not
// report financial success or keep the entire sweep on exponential backoff.
func TestForceCloseVerifiedQuarantineReportsTerminalAccountingProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		malformed := newForceCloseDisputeFixture(t, ctx, true, true, 2*escrow, 2*escrow, escrow)
		selected, err := ForceCloseOpenContractIds(ctx, malformed.cutoff, 10, 1, 1, 0)
		var accounting *ForceCloseAccountingError
		if selected != 1 || !errors.As(err, &accounting) || !errors.Is(err, errContractInsufficientEscrow) ||
			accounting.VerifiedCloseCount() != 1 || accounting.AccountingRejectionCount() != 0 || accounting.QuarantinedAccountingRejectionCount() != 1 {
			t.Fatal("quarantine-only page lost its bounded progress or accounting failure")
		}
	})
}

// Matching text, an unclaimed concurrent finalization, incomplete posts, and
// failed stream cleanup cannot shorten ordinary backoff for a whole batch.
func TestForceCloseQuarantineAuthorityRequiresEveryPhase(t *testing.T) {
	other := errors.New("synthetic infrastructure failure")
	cases := []struct {
		name          string
		closeErr      error
		claimed       bool
		quarantineErr error
		cleanupErr    error
		want          bool
	}{
		{name: "complete", closeErr: errContractInsufficientEscrow, claimed: true, want: true},
		{name: "single-wrapper", closeErr: fmt.Errorf("synthetic close: %w", errContractInsufficientEscrow), claimed: true, want: true},
		{name: "not-claimed", closeErr: errContractInsufficientEscrow},
		{name: "text-only", closeErr: errors.New(errContractInsufficientEscrow.Error()), claimed: true},
		{name: "mixed-close", closeErr: errors.Join(errContractInsufficientEscrow, other), claimed: true},
		{name: "single-leaf-join", closeErr: errors.Join(errContractInsufficientEscrow), claimed: true},
		{name: "different-malformed", closeErr: other, claimed: true},
		{name: "database-or-post", closeErr: errContractInsufficientEscrow, claimed: true, quarantineErr: other},
		{name: "stream-or-read", closeErr: errContractInsufficientEscrow, claimed: true, cleanupErr: other},
		{name: "disputed", closeErr: errContractInsufficientEscrow, claimed: true, cleanupErr: &forceCloseNonfinalError{disputed: true}},
		{name: "open", closeErr: errContractInsufficientEscrow, claimed: true, cleanupErr: &forceCloseNonfinalError{}},
		{name: "cancelled", closeErr: errContractInsufficientEscrow, claimed: true, cleanupErr: context.Canceled},
		{name: "deadline", closeErr: errContractInsufficientEscrow, claimed: true, quarantineErr: context.DeadlineExceeded},
		{name: "no-error", claimed: true},
	}
	for _, c := range cases {
		if got := isForceCloseQuarantinedAccountingRejection(c.closeErr, c.claimed, c.quarantineErr, c.cleanupErr); got != c.want {
			t.Errorf("%s: quarantine authority=%t, want %t", c.name, got, c.want)
		}
	}
}

// A matching leaf inside an arbitrary join, or matching text without source
// identity, cannot authorize retry. Cleanup must be the fresh disputed verifier.
func TestForceCloseAccountingAuthorityRequiresEveryPhase(t *testing.T) {
	verified := &forceCloseNonfinalError{disputed: true}
	other := errors.New("synthetic database or stream cleanup failure")
	cases := []struct {
		name          string
		closeErr      error
		quarantineErr error
		cleanupErr    error
		want          bool
	}{
		{name: "exact", closeErr: errContractInsufficientEscrow, cleanupErr: verified, want: true},
		{name: "single-close-wrapper", closeErr: fmt.Errorf("synthetic context: %w", errContractInsufficientEscrow), cleanupErr: verified, want: true},
		{name: "same-text", closeErr: errors.New(errContractInsufficientEscrow.Error()), cleanupErr: verified},
		{name: "joined-close", closeErr: errors.Join(errContractInsufficientEscrow, other), cleanupErr: verified},
		{name: "single-leaf-join", closeErr: errors.Join(errContractInsufficientEscrow), cleanupErr: verified},
		{name: "quarantine-database-error", closeErr: errContractInsufficientEscrow, quarantineErr: other, cleanupErr: verified},
		{name: "cleanup-error", closeErr: errContractInsufficientEscrow, cleanupErr: other},
		{name: "untyped-nonfinal", closeErr: errContractInsufficientEscrow, cleanupErr: errors.New(verified.Error())},
		{name: "not-disputed", closeErr: errContractInsufficientEscrow, cleanupErr: &forceCloseNonfinalError{disputed: false}},
		{name: "joined-verifier", closeErr: errContractInsufficientEscrow, cleanupErr: errors.Join(verified, other)},
		{name: "canceled", closeErr: errors.Join(errContractInsufficientEscrow, context.Canceled), cleanupErr: verified},
		{name: "deadline", closeErr: errContractInsufficientEscrow, cleanupErr: context.DeadlineExceeded},
		{name: "no-close-rejection", cleanupErr: verified},
	}
	for _, c := range cases {
		if got := isForceCloseAccountingRejection(c.closeErr, c.quarantineErr, c.cleanupErr); got != c.want {
			t.Errorf("%s: authority=%t, want %t", c.name, got, c.want)
		}
	}
}

// Even a known underfunded dispute cannot shorten ordinary backoff if its
// otherwise-no-op quarantine statement encounters another database failure.
func TestForceCloseAccountingDatabaseFailureKeepsOrdinaryError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*escrow, escrow)
		before := bad.state(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
                CREATE FUNCTION synthetic_reject_quarantine() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    RAISE EXCEPTION 'synthetic quarantine database failure';
                END;
                $$;
                CREATE TRIGGER synthetic_reject_quarantine
                BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH STATEMENT EXECUTE FUNCTION synthetic_reject_quarantine();
            `))
		})
		selected, err := ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 1, 1, 0)
		if selected != 1 || err == nil || !strings.Contains(err.Error(), "synthetic quarantine database failure") ||
			!strings.Contains(err.Error(), errContractInsufficientEscrow.Error()) || bad.state(t, ctx) != before {
			t.Fatal("mixed database/accounting failure or protected state was lost")
		}
		var progress forceCloseAccountingProgress
		if errors.As(err, &progress) {
			t.Fatal("extra database failure gained accounting-only retry authority")
		}
	})
}

// A dispute created while finalizing checkpoints needs one fresh post-failure
// verification in the same pass; an already-large error count must not impose
// another hour-scale global pause while unrelated backlog is draining.
func TestForceCloseAccountingNewDisputeRequiresFirstPassVerification(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const escrow = ByteCount(32 * 1024 * 1024)
		bad := newForceCloseDisputeFixture(t, ctx, true, true, 0, 4*escrow, escrow)
		before := bad.state(t, ctx)
		if !before.sourceCheckpoint || !before.destinationCheckpoint || !before.open || before.dispute || before.outcome != "" || !before.streamFound {
			t.Fatal("synthetic initial checkpoint lifecycle or stream precondition failed")
		}
		selected, err := ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 1, 1, 0)
		if selected != 1 || err == nil || !errors.Is(err, errContractInsufficientEscrow) {
			t.Fatal("newly created dispute lost its underfunded rejection")
		}
		var progress forceCloseAccountingProgress
		disputed := bad.state(t, ctx)
		if disputed.outcome != "" || !disputed.dispute || disputed.open || disputed.sourceCheckpoint || disputed.destinationCheckpoint {
			t.Fatalf("checkpoint lifecycle flags: nonfinal=%t disputed=%t open=%t source_checkpoint=%t destination_checkpoint=%t",
				disputed.outcome == "", disputed.dispute, disputed.open, disputed.sourceCheckpoint, disputed.destinationCheckpoint)
		}
		if disputed.sourceByteCount != before.sourceByteCount || disputed.destinationByteCount != before.destinationByteCount {
			t.Fatalf("checkpoint usage changed: source_equal=%t destination_equal=%t",
				disputed.sourceByteCount == before.sourceByteCount, disputed.destinationByteCount == before.destinationByteCount)
		}
		if disputed.escrowSettled || disputed.escrowPayoutByteCount != before.escrowPayoutByteCount ||
			disputed.payerBalanceByteCount != before.payerBalanceByteCount || disputed.netEscrowByteCount != before.netEscrowByteCount ||
			disputed.providerPayoutByteCount != before.providerPayoutByteCount {
			t.Fatalf("checkpoint accounting changed: escrow_settled=%t payout_equal=%t payer_equal=%t net_escrow_equal=%t provider_equal=%t",
				disputed.escrowSettled, disputed.escrowPayoutByteCount == before.escrowPayoutByteCount,
				disputed.payerBalanceByteCount == before.payerBalanceByteCount, disputed.netEscrowByteCount == before.netEscrowByteCount,
				disputed.providerPayoutByteCount == before.providerPayoutByteCount)
		}
		// Existing final-close semantics remove the stream on dispute entry,
		// before the later escrow guard rejects settlement. Retry adds no change.
		if disputed.streamFound {
			t.Fatal("checkpoint finalization did not remove the disputed stream")
		}
		if !errors.As(err, &progress) || progress.VerifiedCloseCount() != 0 || progress.AccountingRejectionCount() != 1 {
			t.Fatal("new disputed rejection lacks same-pass fresh typed verification authority")
		}
		selected, err = ForceCloseOpenContractIds(ctx, bad.cutoff, 10, 1, 1, 0)
		if selected != 1 || err == nil || !errors.As(err, &progress) || progress.VerifiedCloseCount() != 0 || progress.AccountingRejectionCount() != 1 {
			t.Fatal("next scan did not independently verify the existing reserved dispute")
		}
		if bad.state(t, ctx) != disputed {
			t.Fatal("verified retry changed the preserved disputed state")
		}
	})
}
