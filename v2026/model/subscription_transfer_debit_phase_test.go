package model

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

// The same journal entry passes through each failure on its normal path, then
// drains. A deferred trigger fails at the real commit, after the callback exits.
func TestAsyncDebitFlushFailurePhasesAndReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, fixture, 100)
		_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		hook := &asyncDebitReleaseHook{key: redisContractReservationKeys(fixture.balanceId)[0], after: true}
		asyncDebitInstallHook(ctx, hook)

		exec := func(statement string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, statement))
			})
		}
		exec(`CREATE FUNCTION synthetic_debit_phase_failure() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic debit phase failure'; END $$`)

		read := func() map[string]float64 {
			t.Helper()
			families, err := prometheus.DefaultGatherer.Gather()
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]float64{}
			for _, family := range families {
				if family.GetName() == "urnetwork_transfer_debit_flush_phase_errors_total" {
					for _, metric := range family.Metric {
						if len(metric.Label) != 1 || metric.Label[0].GetName() != "phase" {
							t.Fatal("unexpected debit failure label")
						}
						counts[metric.Label[0].GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
			return counts
		}
		before := read()
		for _, check := range []struct {
			phase       string
			trigger     string
			appliedRows int
		}{
			{phase: "apply_callback", trigger: "TRIGGER synthetic_failure BEFORE UPDATE ON transfer_debit_journal"},
			{phase: "apply_commit", trigger: "CONSTRAINT TRIGGER synthetic_failure AFTER UPDATE ON transfer_debit_journal DEFERRABLE INITIALLY DEFERRED"},
			{phase: "redis_release", appliedRows: 1},
			{phase: "cleanup_callback", trigger: "TRIGGER synthetic_failure BEFORE DELETE ON transfer_debit_journal", appliedRows: 1},
			{phase: "cleanup_commit", trigger: "CONSTRAINT TRIGGER synthetic_failure AFTER DELETE ON transfer_debit_journal DEFERRABLE INITIALLY DEFERRED", appliedRows: 1},
			{phase: ""},
		} {
			if check.trigger != "" {
				exec("CREATE " + check.trigger + " FOR EACH ROW EXECUTE FUNCTION synthetic_debit_phase_failure()")
			}
			hook.enabled.Store(check.phase == "redis_release")
			_, _, busy, err := flushTransferDebitBalance(ctx, fixture.balanceId)
			if (err != nil) != (check.phase != "") || busy {
				t.Fatalf("%s: unexpected flush outcome", check.phase)
			}
			if check.trigger != "" {
				exec("DROP TRIGGER synthetic_failure ON transfer_debit_journal")
			}
			if check.phase == "redis_release" && hook.hits.Load() != 1 {
				t.Fatal("post-release reply loss did not occur exactly once")
			}

			credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
			expectCredit, expectPending := ByteCount(989), 0
			if check.phase == "apply_callback" || check.phase == "apply_commit" {
				expectCredit, expectPending = 1000, 1
			}
			if credit != expectCredit || pending != expectPending || applied != check.appliedRows {
				t.Fatalf("%s: debit or journal custody changed", check.phase)
			}
			after := read()
			if check.phase != "" && after[check.phase] != before[check.phase]+1 {
				t.Fatalf("%s: recovered failure did not increment its entered phase", check.phase)
			}
			for phase, count := range after {
				if phase != check.phase && count != before[phase] {
					t.Fatalf("%s: unrelated failure phase incremented", check.phase)
				}
			}
			before = after
		}
	})
}
