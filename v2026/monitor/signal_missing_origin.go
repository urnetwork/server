package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"
)

const (
	missingOriginRange         = "5m"
	missingOriginWarnPerMinute = 500.0
	missingOriginFreshness     = 90 * time.Second
)

// SIGNALS.md §2.17 maps to signal_missing_origin.go and
// signal_missing_origin_test.go. It measures the lossless API counter for
// originally non-companion requests that entered companion settlement without
// a usable reverse origin. No client or destination identity leaves Mimir.
func NewMissingOriginSignal() Signal {
	return &signalAdapter{
		number: "2.17", key: "missing-origin", name: "Missing-origin return-path rate",
		probe: missingOriginProbe{},
	}
}

type missingOriginProbe struct{}

func (missingOriginProbe) id() string             { return "mimir/missing-origin" }
func (missingOriginProbe) tier() string           { return tierWarn }
func (missingOriginProbe) cadence() time.Duration { return time.Minute }

func missingOriginQuery(environment string) string {
	return fmt.Sprintf(
		`sum(rate(urnetwork_connect_contract_failures_total{env=%s,cause="missing_companion_origin",companion="false"}[%s])) * 60`,
		strconv.Quote(environment),
		missingOriginRange,
	)
}

func (missingOriginProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("missing origin: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(missingOriginQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("missing origin: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("missing origin: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"missing origin: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	if len(response.Data.Result) != 1 {
		// CounterVec label families are created by traffic, not registration.
		// An absent vector therefore cannot distinguish a perfect zero from
		// missing API instrumentation or ingestion and must stay unknown.
		return nil, fmt.Errorf("missing origin: Mimir returned %d fallback series, want 1", len(response.Data.Result))
	}
	observedAt, rate, err := mimirInstantValue(response.Data.Result[0].Value)
	if err != nil {
		return nil, fmt.Errorf("missing origin: parse fallback sample: %w", err)
	}
	age := env.now().UTC().Sub(observedAt)
	if age > missingOriginFreshness || age < -30*time.Second {
		return nil, fmt.Errorf("missing origin: stale fallback sample age=%s", age.Round(time.Second))
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return nil, fmt.Errorf("missing origin: invalid fallback rate %v", rate)
	}
	if rate <= missingOriginWarnPerMinute {
		return []finding{healthyFinding(
			"mimir/missing-origin", tierWarn, "missing-origin-rate", "api-fleet",
		)}, nil
	}

	return []finding{{
		probeId: "mimir/missing-origin", tier: tierWarn,
		class: "missing-origin-rate", target: "api-fleet", frame: "fallback-from-normal", sustain: 1,
		symptom: fmt.Sprintf(
			"missing companion-origin contract failures from non-companion requests are %.1f/min over five minutes",
			rate,
		),
		mechanism: "The wire request was non-companion, but contract resolution fell back to Stream/companion settlement because the destination did not currently advertise the relationship mode. The fallback then found no reverse origin. This can be a selected stale destination, a same-network return to a Stream-only client, a provider-mode transition, or a retained client window; the request bit alone does not identify which path produced it.",
		baseline: fmt.Sprintf(
			"The companion=false partition remains at or below %.0f/min; the 2026-09-02 healthy midday control was about 130-190/min.",
			missingOriginWarnPerMinute,
		),
		observed: fmt.Sprintf(
			"companion=false rate_per_minute=%.3f range=%s sample_time=%s metrics_gateway=%s",
			rate,
			missingOriginRange,
			observedAt.Format(time.RFC3339),
			metricHost.name,
		),
		evidence: "Mimir evaluates the five-minute rate of the bounded API counter across the fleet for companion=false and exports one aggregate rate. No client, network, contract, or destination identifier leaves the source.",
		context:  "companion=false records the original wire bit, not the role of the request or the branch ultimately taken. resolveNonCompanionProvideMode can convert that request to companion fallback. Provider discovery is one producer, but provider return paths and same-network peers also create contracts. A 2026-09-02 identifier-free production cohort found the live pre-guard fault entirely in same-network returns from active top-level sources to already-inactive derived destinations; that dated result is a discriminator, not a permanent assumption about future incidents. Genuine companion=true traffic has a separate, substantially higher workload-dependent background and is context until its own healthy band is established.",
		action:   "First require §2.8, §2.9, §2.15, and §2.16 to be healthy and verify bounded score-cache samples contain only contractable active top-level providers, then run §2.20 and §2.18. If successful stale contracts remain nonzero or §2.18 is unobservable because API artifacts predate server commit c8dfe570, satisfy the selected artifact's migration prerequisite and deploy the lifecycle guard; do not wait out a legacy client cohort while the server still authorizes dead routes. After API convergence, use the bounded rejection and lifecycle/relationship dimensions to locate any stale producer, and let Connect-bearing clients age through their maximum client-window lifetime; do not infer endpoint roles from companion=false, print identifiers, edit Redis blobs, relax provider gates, or restart clients to manufacture recovery.",
		verify:   "§2.20 reports zero successful contracts to already-inactive destinations for two complete five-minute windows, §2.18 exposes both initialized partitions, the companion=false missing-origin rate remains below 500/min for the same windows after the deployed client-window lifetime, selection controls remain healthy, and an end-to-end provider route succeeds without manual state changes.",
		playbook: "SIGNALS.md §2.17 and §5.9",
	}}, nil
}
