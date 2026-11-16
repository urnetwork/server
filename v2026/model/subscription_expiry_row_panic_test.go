// An unexpected row panic is interruption, not a completed operational failure.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type expiryVisitRuntimePanic struct{}

func (*expiryVisitRuntimePanic) Error() string { return "synthetic runtime continuation panic" }
func (*expiryVisitRuntimePanic) RuntimeError() {}

// The existing continuation seam runs after proof commits and before any close.
// Both unexpected panic kinds must retain the exact prior cursor and custody in
// both modes. Removing the interruption restores the existing settlement owner.
func TestForceCloseUnexpectedRowPanicKeepsPriorCursor(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, kind := range []string{"non-error", "runtime"} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, kind), func(t *testing.T) {
				env := server.DefaultTestEnv()
				env.RerunCount = 0
				env.Run(t, func(t testing.TB) {
					ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 120*time.Second)
					defer cancel()
					fixture := newForceCloseOperationalFixture(t, ctx, legacy)
					before := fixture.state(t, ctx)
					prior := &ContractExpiryCursor{ScanBefore: server.NowUtc()}
					calls := 0
					callCtx := context.WithValue(ctx, forceCloseContinuationContextKey{}, func(parent context.Context, id server.Id) context.Context {
						if id != fixture.contractId {
							return parent
						}
						calls++
						if kind == "runtime" {
							panic(&expiryVisitRuntimePanic{})
						}
						panic("synthetic non-error continuation panic")
					})
					var proof []byte
					for attempt := 1; attempt <= 2; attempt++ {
						count, cursor, err := forceCloseOpenContractIdsBudgetedPage(callCtx, fixture.cutoff, 2, 1, 1, 0,
							prior, time.Minute, 1)
						if count != 1 || cursor != prior || calls != attempt || ctx.Err() != nil || err == nil ||
							!strings.Contains(err.Error(), "continuation panic") || prior.Open != nil || prior.Dispute != nil {
							t.Fatal("unexpected row panic gained a completed checkpoint or lost its failure", count, cursor, calls, err)
						}
						if kind == "runtime" {
							var programming runtime.Error
							if !errors.As(err, &programming) {
								t.Fatal("row interruption erased its runtime cause", err)
							}
						}
						retained := requireForceCloseOperationalPreserved(t, ctx, fixture, before, err)
						if attempt == 2 && !bytes.Equal(proof, retained) {
							t.Fatal("repeated row interruption replaced the retained proof")
						}
						proof = retained
					}
					requireForceCloseOperationalRecovery(t, ctx, fixture, legacy, proof)
				})
			})
		}
	}
}
