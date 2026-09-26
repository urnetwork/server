// A bounded durable ledger sample detects empty escrow writes independently
// of CPU saturation, query IDs, and service-specific telemetry.
package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// SIGNALS.md §1.3d measures zero-byte escrow rows on fresh positive-byte
// contracts. Zero-byte anchor contracts and contracts without escrow are excluded.
func NewEscrowAmplificationSignal() Signal {
	return &signalAdapter{
		number: "1.3d", key: "escrow-amplification", name: "Empty transfer-escrow write amplification",
		probe: escrowAmplificationProbe{},
	}
}

// Stateless primary-key samples retain only aggregate counts across a run.
type escrowAmplificationProbe struct{}

func (escrowAmplificationProbe) id() string             { return "pg/escrow-amplification" }
func (escrowAmplificationProbe) tier() string           { return tierPage }
func (escrowAmplificationProbe) cadence() time.Duration { return time.Minute }

// The outer primary-key limit bounds the contract visit even when the host
// is idle. Each lateral primary-key prefix scan reads at most 513 rows; the
// extra row identifies truncation rather than silently accepting a prefix.
// ULID order selects candidates only; server create_time decides freshness.
const escrowAmplificationQuery = `
WITH candidates AS MATERIALIZED (
    SELECT contract_id, create_time, transfer_byte_count, payer_network_id
    FROM transfer_contract
    ORDER BY contract_id DESC
    LIMIT 100
), sampled AS MATERIALIZED (
    SELECT contract_id
    FROM candidates
    WHERE payer_network_id IS NOT NULL AND transfer_byte_count > 0
      AND create_time >= (now() AT TIME ZONE 'UTC') - interval '2 minutes'
      AND create_time <= (now() AT TIME ZONE 'UTC') + interval '30 seconds'
), allocations AS (
    SELECT sampled.contract_id, count(escrow.balance_byte_count)::bigint AS rows,
           count(*) FILTER (WHERE escrow.balance_byte_count = 0)::bigint AS zero_rows
    FROM sampled
    LEFT JOIN LATERAL (
        SELECT balance_byte_count
        FROM transfer_escrow
        WHERE contract_id = sampled.contract_id
        LIMIT 513
    ) AS escrow ON true
    GROUP BY sampled.contract_id
)
SELECT extract(epoch FROM clock_timestamp())::bigint,
       (SELECT count(*) FROM candidates),
       count(*), coalesce(sum(rows), 0), coalesce(sum(zero_rows), 0),
       count(*) FILTER (WHERE zero_rows > 0),
       count(*) FILTER (WHERE rows >= 513),
       count(*) FILTER (WHERE rows = 0)
FROM allocations;
`

// Missing or truncated allocations remain distinct from measured zero fanout.
type escrowAmplificationSample struct {
	candidates int64
	contracts  int64
	rows       int64
	zeroRows   int64
	affected   int64
	limited    int64
	missing    int64
}

// Checks timestamp freshness and count conservation before constructing a
// finding; raw database rows and source errors never enter alert text.
func parseEscrowAmplification(rows []pgRow, now time.Time) (escrowAmplificationSample, error) {
	if len(rows) != 1 || len(rows[0]) != 8 {
		return escrowAmplificationSample{}, fmt.Errorf("escrow amplification: incomplete aggregate source")
	}
	values := make([]int64, 8)
	for index := range values {
		value, err := strconv.ParseInt(rows[0].str(index), 10, 64)
		if err != nil || value < 0 {
			return escrowAmplificationSample{}, fmt.Errorf("escrow amplification: invalid aggregate field")
		}
		values[index] = value
	}
	age := now.Sub(time.Unix(values[0], 0))
	if age > 30*time.Second || age < -30*time.Second {
		return escrowAmplificationSample{}, fmt.Errorf("escrow amplification: stale or future observation")
	}
	sample := escrowAmplificationSample{
		candidates: values[1], contracts: values[2], rows: values[3], zeroRows: values[4],
		affected: values[5], limited: values[6], missing: values[7],
	}
	if sample.candidates > 100 || sample.contracts > sample.candidates ||
		sample.rows > 513*sample.contracts || sample.zeroRows > sample.rows ||
		sample.affected > sample.contracts || sample.affected > sample.zeroRows ||
		sample.limited > sample.contracts || sample.missing > sample.contracts ||
		(sample.zeroRows > 0 && sample.affected == 0) {
		return escrowAmplificationSample{}, fmt.Errorf("escrow amplification: inconsistent aggregate counts")
	}
	return sample, nil
}

// A partial sample may prove waste but cannot prove its absence. The page
// threshold uses confirmed zero-row counts, so truncation cannot invent it.
func (escrowAmplificationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	host := env.cfg.hostByRole("pg-primary")
	if host == nil {
		return nil, fmt.Errorf("escrow amplification: no enabled database host")
	}
	rows, err := env.runner.pg(ctx, escrowAmplificationQuery)
	var sample escrowAmplificationSample
	if err == nil {
		sample, err = parseEscrowAmplification(rows, env.now())
	}
	findings := []finding{}
	complete := err == nil && sample.contracts > 0 && sample.limited == 0 && sample.missing == 0
	if !complete {
		observed := "source_status=unavailable"
		if err == nil {
			observed = fmt.Sprintf("candidate_contracts=%d fresh_positive_escrow_contracts=%d capped_contracts=%d missing_allocations=%d", sample.candidates, sample.contracts, sample.limited, sample.missing)
		}
		findings = append(findings, finding{
			probeId: "pg/escrow-amplification", tier: tierWarn, class: "escrow-amplification-unavailable", target: host.name, sustain: 2,
			symptom:   "The bounded current-contract sample cannot establish complete escrow write coverage.",
			mechanism: "The direct database source failed, returned stale/inconsistent counts, contained no eligible fresh contracts, or hit a per-contract scan bound. Missing coverage is unknown, not zero write amplification.",
			baseline:  "Up to 100 newest contract candidates contain at least one current positive-byte escrow contract, with all allocations inside the 512-row per-contract bound.",
			observed:  observed,
			action:    "Check direct PostgreSQL visibility and current contract traffic. If a bounded sample is capped, preserve its confirmed waste and inspect that workload with a separately bounded discriminator. Do not scan all escrow history or infer recovery from no current sample.",
			verify:    "Two consecutive current samples are complete, or an explicit no-traffic observation accounts for the missing workload; recovery of a prior fanout finding requires a complete sample.", playbook: "SIGNALS.md §1.3d",
		})
	} else {
		findings = append(findings, healthyFinding("pg/escrow-amplification", tierWarn, "escrow-amplification-unavailable", host.name))
	}
	if err != nil {
		return findings, nil
	}
	if sample.zeroRows == 0 {
		if complete {
			findings = append(findings, healthyFinding("pg/escrow-amplification", tierWarn, "escrow-zero-byte-writes", host.name))
		}
		return findings, nil
	}
	tier := tierWarn
	if sample.zeroRows >= 100 && sample.affected >= 10 {
		tier = tierPage
	}
	findings = append(findings, finding{
		probeId: "pg/escrow-amplification", tier: tier, class: "escrow-zero-byte-writes", target: host.name, sustain: 2,
		symptom:   "Positive-byte transfer contracts are persisting escrow rows that reserve no bytes.",
		mechanism: "A PostgreSQL-active balance may be fully reserved by its Redis mirror. Admitting that empty grant still creates escrow/index/WAL writes and mirror refreshes, and includes it in the paid/unpaid priority average. Many exhausted grants amplify each contract into many useless writes.",
		baseline:  "Zero zero-byte escrow rows on current positive-byte escrow contracts; WARN on any confirmed row, PAGE on at least 100 rows across at least 10 contracts for two one-minute observations.",
		observed:  fmt.Sprintf("candidate_contracts=%d fresh_positive_escrow_contracts=%d observed_escrow_rows=%d zero_byte_escrow_rows=%d affected_contracts=%d rows_per_sampled_contract=%.2f capped_contracts=%d complete=%t", sample.candidates, sample.contracts, sample.rows, sample.zeroRows, sample.affected, float64(sample.rows)/float64(sample.contracts), sample.limited, complete),
		evidence:  "Direct PostgreSQL aggregate over at most 100 primary-key contract candidates and at most 513 escrow rows per eligible contract. No customer, balance, contract, query ID, or SQL text is exported.",
		context:   "False-positive qualifier: zero-byte anchor contracts and no-escrow contracts are excluded; legitimate splitting across positive balances is healthy at any observed fanout. False-negative qualifiers: this bounded two-minute sample can miss older, rare, or interleaved affected contracts; capped samples prove only the confirmed zero-row lower bound. WALInsert/BufferContent waits and cumulative statement execution time are not CPU time, so a healthy §1.3c does not clear this finding.",
		action:    "Trace grant selection after Redis reservation subtraction in createTransferEscrowInTx and its origin/companion callers. Skip exhausted grants for positive-byte requests while preserving zero-byte anchors, funding totals, priority, and mirror behavior. Correlate bounded statement call deltas and direct WAL/buffer waits; do not delete ledger rows, fund the payer, or restart PostgreSQL to hide the repeated work.",
		verify:    "Two complete fresh samples contain no zero-byte escrow rows for positive-byte contracts; origin/companion allocation tests pass, contract throughput remains healthy, and related WAL/buffer contention recedes.", playbook: "SIGNALS.md §1.3d",
	})
	return findings, nil
}
