// Real financial owners must reuse only facts protected by their contract lock.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Count real SQL reads after the exact grant ownership gate, then check the
// committed debit, escrow release and durable payout before the causal assertion.
func TestLegacySettlementOwnerReadsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		owner := &legacyGrantOwnerDiagnosticTx{afterGrant: func() {}}
		trace := &legacyFinancialLatencyTx{PgTx: owner, owner: owner}
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			owner.PgTx = tx
			var completed, busy bool
			var err error
			posts, completed, busy, _, err = flushLegacySettlementInTx(ctx, trace, id)
			server.Raise(err)
			if !completed || busy {
				t.Fatal("synthetic owner did not reach the financial commit")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
		if trace.reportReads != 1 || trace.participantHeaderReads != 1 {
			t.Fatalf("financial owner reread locked contract facts: reports=%d participant_headers=%d", trace.reportReads, trace.participantHeaderReads)
		}
		t.Logf("one complete legacy financial owner: owned_round_trips=%d report_reads=%d participant_headers=%d", trace.ownedRoundTrips, trace.reportReads, trace.participantHeaderReads)
	})
}
