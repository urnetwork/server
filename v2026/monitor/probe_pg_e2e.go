// pg e2e-encryption probe: key-publication coverage (SIGNALS.md 15.1).
// Providers running the e2e-enabled build publish their TLS cert commitment on
// connect (oob `EncryptedKey` -> `client_tls_certificate`); coverage among
// recently-connected clients is the provider-rollout/health proxy. The platform
// cannot see inside sessions (by design) — publications are the pg-visible
// footprint. The probe stays quiet pre-rollout (arming floor on its own
// trailing median) and alerts only on regression, never on absolute coverage.
package main

import (
	"context"
	"fmt"
	"time"
)

type pgE2eKeyProbe struct{}

const e2eCoverageMetric = "pg/e2e-key-coverage"

func (self pgE2eKeyProbe) id() string             { return "pg/e2e-key-publication" }
func (self pgE2eKeyProbe) tier() string           { return tierWarn }
func (self pgE2eKeyProbe) cadence() time.Duration { return 5 * time.Minute }

func (self pgE2eKeyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, `
        SELECT count(DISTINCT ncc.client_id) AS active,
               count(DISTINCT ctc.client_id) AS covered
        FROM network_client_connection ncc
        LEFT JOIN client_tls_certificate ctc ON ctc.client_id = ncc.client_id
        WHERE ncc.connect_time >= now() - interval '1 hour';
    `)
	if err != nil {
		return nil, err
	}
	active := atoiRow(rows[0], 0)
	covered := atoiRow(rows[0], 1)
	coverage := 0.0
	if 0 < active {
		coverage = float64(covered) / float64(active)
	}

	// arm on the trailing 24h median, not the spot value: pre-rollout both
	// counts sit at ~0 and must never alert; after the fleet ratchets up, a
	// collapse below half the median is the regression signal (15.1)
	armed := false
	var median float64
	if env.baseline != nil {
		if m, _, ok := env.baseline.trailingMedian(e2eCoverageMetric, 24*time.Hour, 12); ok {
			median = m
			armed = 0.05 <= median
		}
		env.baseline.record(e2eCoverageMetric, time.Now(), coverage)
	}

	if armed && coverage < 0.5*median {
		return []finding{{
			probeId: "pg/e2e-key-publication", tier: tierWarn,
			class: "e2e-key-coverage", target: target, sustain: 3,
			symptom: fmt.Sprintf(
				"e2e key coverage %.1f%% (%d/%d active clients) < half its trailing 24h median %.1f%%",
				100*coverage, covered, active, 100*median),
			baseline: "coverage ratchets up with the provider rollout and holds (diurnal wobble is fine); a collapse = providers stopped publishing (EncryptedKey oob regression, bad client build, or fleet rollback)",
			observed: fmt.Sprintf("covered=%d active=%d coverage=%.3f median=%.3f", covered, active, coverage, median),
			context:  "publication happens on provider connect (oob EncryptedKey -> client_tls_certificate, set_time refreshed); correlate with the tls-cert-publish-invalid log class and deploys (§8); freshness spot-check: SELECT count(*) FROM client_tls_certificate WHERE set_time >= now() - interval '1 hour'",
			playbook: "SIGNALS.md 15.1",
		}}, nil
	}
	return nil, nil
}
