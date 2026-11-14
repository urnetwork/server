// Disposable PostgreSQL composes a real authenticated local credential with
// the existing immutable terminal/native-proof/single-use grant transaction.
package controller

import (
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Authentication changes no grant semantics: exact replay is receipt-only,
// while wrong job scope refuses before observations or persistent mutation.
func TestCompetitionPatchRetryCredentialPgAuthenticatedGrantAndReplayAreExact(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixtureWithPatchRetry(t, tb, true, "main")
		if fixture.settings.AutomationPolicy.Environment != "main" {
			tb.Fatal("distributed fixture omitted the explicit main control environment")
		}
		registration, bearer, now := competitionPatchRetryCredentialForTest(tb, fixture.settings, fixture.request.Authorization)
		registration.ExpiresAt = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)
		local, err := WithCompetitionOperatorPatchRetryCredential(fixture.settings, registration, now)
		if err != nil {
			tb.Fatal(err)
		}
		principal, ok := authenticateCompetitionAt(bearer, local, now)
		if !ok {
			tb.Fatal("actual scoped credential did not authenticate")
		}
		fixture.settings, fixture.principal = local, principal
		before := fixture.snapshot(tb.Context())
		local.Tokens[len(local.Tokens)-1].Revoked = true
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || fixture.calls != 0 || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("revoked credential reached native observations or grant mutation")
		}
		local.Tokens[len(local.Tokens)-1].Revoked = false
		expired := *principal
		expired.ExpiresAt = time.Unix(1, 0).UTC()
		fixture.principal = &expired
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || fixture.calls != 0 || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("expired credential reached native observations or grant mutation")
		}
		wrong := *principal
		wrong.JobId = server.NewId()
		fixture.principal = &wrong
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || fixture.calls != 0 || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("wrong credential scope observed hosts or mutated a grant")
		}
		fixture.principal = principal
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil || receipt == nil || receipt.Cycle != 1 || fixture.calls != 3 {
			tb.Fatal("authenticated exact grant refused", err)
		}
		after := fixture.snapshot(tb.Context())
		replayed, err := fixture.authorize(tb, tb.Context())
		if err != nil || replayed == nil || fixture.calls != 3 || !reflect.DeepEqual(after, fixture.snapshot(tb.Context())) {
			tb.Fatal("credential replay re-observed hosts or mutated audit", err)
		}
		requireCompetitionDistributedRecoveryReceiptEqual(tb, local, receipt, replayed)
		requireCompetitionReevaluationOriginalEvidence(tb, before.job, after.job)
	})
}
