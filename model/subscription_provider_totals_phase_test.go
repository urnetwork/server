package model

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

type providerPhasePrivateError struct{}

func (*providerPhasePrivateError) Error() string { panic("cause text must not be formatted") }

func TestLegacyProviderTotalsPhasePreservesTypedCausesWithoutPrivateText(t *testing.T) {
	private := "SELECT private_allocation FROM private_table WHERE network_id='private-network'"
	pgErr := &pgconn.PgError{Code: "55P03", Message: private, Detail: private, Where: private}
	cause := errors.Join(pgErr, context.DeadlineExceeded, io.ErrUnexpectedEOF, &providerPhasePrivateError{})
	err := withLegacyProviderTotalsPhase(legacyProviderTotalsAccountWrite, cause)
	var actual *pgconn.PgError
	if !errors.As(err, &actual) || actual != pgErr || !errors.Is(err, pgErr) ||
		!errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("static provider phase changed the exact typed cause graph")
	}
	want := "legacy provider totals phase=account_write failed (SQLSTATE 55P03); context deadline exceeded"
	if err.Error() != want || !strings.Contains(server.ErrorJsonNoStack(err), want) ||
		strings.Contains(server.ErrorJsonNoStack(err), "private") {
		t.Fatal("provider phase leaked cause text or lost its bounded discriminator")
	}
	if !server.InspectErrorCauses(err).Complete {
		t.Fatal("static provider wrapper made the ordinary cause inspection incomplete")
	}
	if withLegacyProviderTotalsPhase(legacyProviderTotalsBody, err) != err ||
		withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, nil) != nil {
		t.Fatal("transaction boundary replaced a more precise statement phase")
	}
	joined := withLegacyProviderTotalsPhase(legacyProviderTotalsBody, errors.Join(err, server.DbContextDoneError, context.Canceled))
	if !strings.Contains(joined.Error(), "phase=account_write") || !errors.Is(joined, server.DbContextDoneError) ||
		!errors.Is(joined, context.Canceled) || !errors.As(joined, &actual) || actual != pgErr {
		t.Fatal("DB context join replaced the statement phase or a typed cause")
	}
	invalid := withLegacyProviderTotalsPhase(255, &pgconn.PgError{Code: private, Message: private})
	if invalid.Error() != "legacy provider totals phase=unknown failed" {
		t.Fatal("invalid phase or SQLSTATE escaped the finite log vocabulary")
	}
}

// This seam does no database work. It isolates wrapper classification and exact
// transaction-owner invocation; native controls below own rollback/commit proof.
type providerPhaseConfigureTx struct {
	server.PgTx
	err error
}

func (self *providerPhaseConfigureTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SET"), self.err
}

func TestLegacyProviderTotalsPhaseKeepsTransactionBoundaryAndNoRetry(t *testing.T) {
	for _, phase := range []legacyProviderTotalsPhase{
		legacyProviderTotalsTransactionStart, legacyProviderTotalsConfigure,
		legacyProviderTotalsBody, legacyProviderTotalsCommit,
	} {
		t.Run(phase.String(), func(t *testing.T) {
			cause := io.ErrUnexpectedEOF
			ownerCalls, bodyCalls := 0, 0
			err := runLegacyProviderTotalsTxWithOwner(t.Context(), func(server.PgTx) error {
				bodyCalls++
				if phase == legacyProviderTotalsBody {
					return cause
				}
				return nil
			}, func(ctx context.Context, body func(server.PgTx), options ...any) {
				ownerCalls++
				if ctx != t.Context() || len(options) != 2 || options[0] != server.TxReadCommitted || options[1] != server.OptNoRetry() {
					t.Fatal("provider phase changed transaction context, isolation or retry policy")
				}
				if phase == legacyProviderTotalsTransactionStart {
					panic(cause)
				}
				tx := &providerPhaseConfigureTx{}
				if phase == legacyProviderTotalsConfigure {
					tx.err = cause
				}
				body(tx)
				panic(cause)
			})
			wantBodyCalls := 0
			if phase == legacyProviderTotalsBody || phase == legacyProviderTotalsCommit {
				wantBodyCalls = 1
			}
			if ownerCalls != 1 || bodyCalls != wantBodyCalls || !errors.Is(err, cause) ||
				!strings.Contains(err.Error(), "phase="+phase.String()) {
				t.Fatal("provider phase replayed or misclassified its transaction boundary")
			}
		})
	}
}
