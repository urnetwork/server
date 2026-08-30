package monitor

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"strings"
	"time"
)

// SIGNALS.md §2.9 maps to signal_selection_population.go and signal_selection_population_test.go.
func NewSelectionPopulationSignal() Signal {
	return &signalAdapter{number: "2.9", key: "selection-population", name: "Provider-selection population", probe: pgSelectionPopulationProbe{}}
}

type pgSelectionPopulationProbe struct{}

func (pgSelectionPopulationProbe) id() string             { return "pg/selection-empty" }
func (pgSelectionPopulationProbe) tier() string           { return tierPage }
func (pgSelectionPopulationProbe) cadence() time.Duration { return 5 * time.Minute }

func (pgSelectionPopulationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	redisHost := env.cfg.hostByRole("redis-cluster")
	if redisHost == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	rows, err := env.runner.pg(ctx, `
		SELECT
		 (SELECT count(DISTINCT ncc.client_id) FROM network_client_connection ncc
		  WHERE ncc.connected AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id=ncc.client_id AND pk.provide_mode=3)),
		 (SELECT count(DISTINCT nclr.client_id) FROM network_client_location_reliability nclr
		  WHERE nclr.connected AND nclr.valid AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id=nclr.client_id AND pk.provide_mode=3)),
		 (SELECT count(*) FROM provider_egress_health),
		 (SELECT count(*) FROM provider_egress_health WHERE measured_at>=now()-interval '24 hours' AND total_count>0 AND 10*ok_count>=9*total_count),
		 (SELECT count(*) FROM provider_egress_location),
		 (SELECT location_id::text FROM location WHERE location_type='country' AND country_code='us' LIMIT 1);
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0].str(5) == "" {
		return nil, fmt.Errorf("provider population query returned no target location")
	}
	connected, eligible := atoiRow(rows[0], 0), atoiRow(rows[0], 1)
	targetLocation := rows[0].str(5)
	caller := "00000000-0000-0000-0000-000000000000"
	normalKey := fmt.Sprintf("{cs_0_q_%s_%s}c_l", caller, targetLocation)
	forcedKey := fmt.Sprintf("{cs_1_q_%s_%s}c_l", caller, targetLocation)
	normalRaw, err := env.runner.redisRaw(ctx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", normalKey)
	if err != nil {
		return nil, err
	}
	forcedRaw, err := env.runner.redisRaw(ctx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", forcedKey)
	if err != nil {
		return nil, err
	}
	normalCount, normalErr := decodeProviderCount([]byte(normalRaw))
	forcedCount, forcedErr := decodeProviderCount([]byte(forcedRaw))
	if normalErr != nil || forcedErr != nil {
		return nil, fmt.Errorf("decode provider score cache: normal=%v forced=%v", normalErr, forcedErr)
	}
	if eligible <= 1000 || normalCount > 0 {
		return []finding{healthyFinding("pg/selection-empty", tierPage, "selection-empty", pgTarget(env))}, nil
	}
	mode := "upstream-empty"
	mechanism := "Both normal and ForceMinimum exports are empty, so provider supply/location input is empty before score minimums are applied."
	if forcedCount > 0 {
		mode = "gate-wipe"
		mechanism = "Connected eligible providers reach the score writer and ForceMinimum export, but a normal reliability, score, or egress predicate removes the entire market."
	}
	return []finding{{
		probeId: "pg/selection-empty", tier: tierPage,
		class: "selection-empty", target: pgTarget(env), frame: mode, sustain: 2,
		symptom:   fmt.Sprintf("eligible public providers=%d but normal score-cache export=%d (ForceMinimum=%d)", eligible, normalCount, forcedCount),
		mechanism: mechanism,
		baseline:  "The normal decoded provider sum is nonzero and tracks eligible supply; ForceMinimum is normally larger.",
		observed: fmt.Sprintf("connected=%d eligible=%d normal=%d forced=%d egress_health=%s fresh_passing=%s egress_locations=%s target=%s",
			connected, eligible, normalCount, forcedCount, rows[0].str(2), rows[0].str(3), rows[0].str(4), targetLocation),
		evidence: fmt.Sprintf("normal_key=%s bytes=%d; forced_key=%s bytes=%d", normalKey, len(normalRaw), forcedKey, len(forcedRaw)),
		action:   "Split the score predicates and inspect the deployed provider.yml enable_egress_test value before changing provider connectivity or cache TTLs.",
		verify:   "A fresh UpdateClientScores run produces a nonzero decoded normal cache count consistent with eligible supply.",
		playbook: "SIGNALS.md §2.9 and §5.9",
	}}, nil
}

func decodeProviderCount(raw []byte) (int, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "(nil)" {
		return 0, nil
	}
	var counts []int
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&counts); err != nil {
		return 0, err
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	return total, nil
}
