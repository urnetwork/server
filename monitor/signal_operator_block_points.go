// Operator blocks that expired without their points (SIGNALS.md §17.7). The
// daily points award (model.AwardDueOperatorBlockPoints) must award each
// completed operator block before the block's reliability evidence passes
// client_reliability retention, 10 days after its window opens and about 3
// days after the block is due. A block past that is recorded expired
// (operator_block_points_expired) and never gets its points: no award of it
// can be complete, and no backfill can make one. Before the record existed
// (2026-11-14) the award stopped at such a block on every run, and no later
// block was ever awarded either. This signal reads the record and warns for
// each block recorded within the window, so a person learns that a week of
// points was lost and finds why the award did not run in time.
package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The operator block points probe's tunables.
type OperatorBlockPointsSettings struct {
	// How long after its record an expired block is reported.
	RecentWindow time.Duration
	// The most expired blocks one observation reads.
	MaxBlocks int
}

// A new instance on every call.
func DefaultOperatorBlockPointsSettings() *OperatorBlockPointsSettings {
	return &OperatorBlockPointsSettings{
		// an operator block: the warning stays up until the next block's
		// award has run, whatever day the record was written
		RecentWindow: 7 * 24 * time.Hour,
		// a block expires at most once a week
		MaxBlocks: 16,
	}
}

// What the schema preflight reads: the installed migration head, and whether
// operator_block_points_expired exists. PostgreSQL resolves every relation a
// statement names before it runs, so the observation is issued only once it
// does.
const operatorBlockPointsSchemaQuery = `
/* monitor-signal-17.7-operator-block-points-schema */
SELECT coalesce(max(end_version_number), 0)::bigint,
       to_regclass('public.operator_block_points_expired') IS NOT NULL
FROM migration_audit
WHERE status = 'success'
`

// The blocks recorded expired inside the window, with their windows, the
// reliability blocks of the record, and the record's age. The table's times
// are UTC without a zone. Only block numbers, times and reliability block
// numbers leave PostgreSQL.
func operatorBlockPointsQueryWith(settings *OperatorBlockPointsSettings) string {
	return fmt.Sprintf(`
/* monitor-signal-17.7-operator-block-points */
SELECT block_number,
       to_char(window_start, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
       to_char(window_end, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
       reliability_min_block_number,
       retention_min_block_number,
       greatest(0, floor(extract(epoch FROM (now() AT TIME ZONE 'UTC') - create_time)))::bigint
FROM operator_block_points_expired
WHERE create_time > (now() AT TIME ZONE 'UTC') - interval '%d seconds'
ORDER BY block_number
LIMIT %d
`, int64(settings.RecentWindow/time.Second), settings.MaxBlocks)
}

// SIGNALS.md §17.7 maps to signal_operator_block_points.go and
// signal_operator_block_points_test.go.
func NewOperatorBlockPointsSignal() Signal {
	return &signalAdapter{
		number: "17.7",
		key:    "operator-block-points",
		name:   "Operator blocks expired without their points",
		probe:  operatorBlockPointsProbe{settings: DefaultOperatorBlockPointsSettings()},
	}
}

// The operator block points probe, at its settings.
type operatorBlockPointsProbe struct {
	settings *OperatorBlockPointsSettings
}

// The probe's alert identity.
func (self operatorBlockPointsProbe) id() string { return "pg/operator-block-points" }

// Its tier: nothing recovers an expired block, so a person is told, not paged.
func (self operatorBlockPointsProbe) tier() string { return tierWarn }

// Its cadence: a record is written at most once a day, by the daily award.
func (self operatorBlockPointsProbe) cadence() time.Duration { return 15 * time.Minute }

// Reads the preflight, then the observation, and warns for each block
// recorded expired inside the window.
func (self operatorBlockPointsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultOperatorBlockPointsSettings()
	}
	if settings.RecentWindow < time.Second || settings.MaxBlocks < 1 {
		return nil, fmt.Errorf("operator block points: invalid settings")
	}
	version := server.OperatorBlockPointsExpiredVersion()
	if version <= 0 {
		return nil, fmt.Errorf("operator block points: this build carries no expiry migration")
	}
	rows, err := env.runner.pg(ctx, operatorBlockPointsSchemaQuery)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("operator block points schema observation is incomplete")
	}
	head, err := parseStrictInt64(rows[0].str(0))
	if err != nil || head < 0 || (rows[0].str(1) != "t" && rows[0].str(1) != "f") {
		return nil, fmt.Errorf("operator block points schema observation is invalid")
	}
	if head < int64(version) {
		// before the migration no expiry is recorded
		return nil, nil
	}
	if rows[0].str(1) != "t" {
		return nil, fmt.Errorf("operator_block_points_expired unavailable at the installed migration head")
	}

	rows, err = env.runner.pg(ctx, operatorBlockPointsQueryWith(settings))
	if err != nil {
		return nil, err
	}
	if settings.MaxBlocks < len(rows) {
		return nil, fmt.Errorf("operator block points returned %d blocks, more than %d", len(rows), settings.MaxBlocks)
	}
	findings := []finding{}
	previousBlock := int64(0)
	for _, row := range rows {
		if len(row) != 6 {
			return nil, fmt.Errorf("operator block points returned %d columns, want 6", len(row))
		}
		block, err := parseStrictInt64(row.str(0))
		if err != nil || block < 1 || block <= previousBlock {
			return nil, fmt.Errorf("operator block points returned an invalid block")
		}
		previousBlock = block
		windowStart, startErr := time.Parse(time.RFC3339, row.str(1))
		windowEnd, endErr := time.Parse(time.RFC3339, row.str(2))
		if startErr != nil || endErr != nil || !windowStart.Before(windowEnd) {
			return nil, fmt.Errorf("operator block points returned an invalid window")
		}
		reliabilityMinBlockNumber, reliabilityErr := parseStrictInt64(row.str(3))
		retentionMinBlockNumber, retentionErr := parseStrictInt64(row.str(4))
		if reliabilityErr != nil || retentionErr != nil || reliabilityMinBlockNumber < 0 || retentionMinBlockNumber <= reliabilityMinBlockNumber {
			return nil, fmt.Errorf("operator block points returned invalid reliability blocks")
		}
		ageSeconds, err := parseStrictInt64(row.str(5))
		if err != nil || ageSeconds < 0 {
			return nil, fmt.Errorf("operator block points returned an invalid age")
		}
		findings = append(findings, finding{
			probeId: "pg/operator-block-points", tier: tierWarn, class: "operator-block-points-expired",
			target: fmt.Sprintf("operator-block-%d", block), sustain: 1,
			symptom: fmt.Sprintf("Operator block %d [%s, %s) expired without its points: no award of it ran before its reliability evidence passed client_reliability retention", block, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339)),
			mechanism: "A block's award reads its window's client_reliability evidence, which retention keeps for 10 days from the window's start (model.ClientReliabilityRetention): about 3 days after the block is due, its Sunday 00:00 UTC close plus the 2-hour settle time. " +
				"The daily award (controller.AwardOperatorBlockPoints, 12:00 UTC, retried within about an hour after an error) did not award the block in that time, so any award of it now would be computed from partial evidence; it fails closed instead. " +
				"The award recorded the block expired (operator_block_points_expired) and goes on to the later blocks. Providers get no points for the block's traffic, GET /account/epochs counts no bytes served for it, and every Streak that ran through it ends there.",
			baseline: "No block expires: the daily award awards each block the Sunday it closes.",
			observed: fmt.Sprintf("block=%d window_start=%s window_end=%s reliability_min_block=%d retention_min_block=%d recorded_age_hours=%d",
				block, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), reliabilityMinBlockNumber, retentionMinBlockNumber, ageSeconds/3600),
			evidence: "One read-only statement on operator_block_points_expired; only block numbers, times and reliability block numbers leave PostgreSQL. The taskworker counts each record in urnetwork_operator_block_points_expired_blocks_total.",
			context:  "The award stops at the first error and retries; an error on every run for three days, no taskworker running the award, or a client_reliability drain that stayed behind (ErrOperatorBlockPointsReliabilityPending) expires a block. While the award fails, task health (§2.5) reports the award_operator_block_points task's repeated errors.",
			action: "No backfill exists for an expired block: its evidence is gone, the backfill's dry run (bringyourctl account-points backfill-blocks) lists it as expired and refuses it, and a commit of any award of it is refused. " +
				"Find why the award did not run in time: the award_operator_block_points task in pending_task and finished_task, and the taskworker's [obp] log lines. Fix that so the next block is awarded on time. " +
				"If the next block is due and its award fails too, award it by hand before its own deadline (its window start plus 10 days): bringyourctl account-points backfill-blocks --blocks=<block> --file=<file> is a read-only dry run that prints a digest, and bringyourctl account-points backfill-blocks --commit --expect=<digest> --file=<file> commits exactly that, with the owner's go. " +
				"Never write payment_report, account_payment or account_point rows for the expired block by hand, and never delete its record to award it without the owner's go.",
			verify:   "The next block is awarded (a payment_report row with its plan id, model.OperatorBlockPointsPlanId) and no other block is recorded expired. This warning clears once the record is older than 7 days; the block stays unawarded.",
			playbook: "SIGNALS.md §17.7",
		})
	}
	return findings, nil
}
