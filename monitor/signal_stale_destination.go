package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	staleDestinationRange         = "5m"
	staleDestinationWarnPerMinute = 50.0
	staleDestinationFreshness     = 90 * time.Second
)

// SIGNALS.md §2.18 maps to signal_stale_destination.go and
// signal_stale_destination_test.go. It measures the API lifecycle guard that
// prevents a selected inactive identity from becoming a successful contract.
func NewStaleDestinationSignal() Signal {
	return &signalAdapter{
		number: "2.18", key: "stale-destination", name: "Stale contract destination rejection rate",
		probe: staleDestinationProbe{},
	}
}

type staleDestinationProbe struct{}

func (staleDestinationProbe) id() string             { return "mimir/stale-destination" }
func (staleDestinationProbe) tier() string           { return tierWarn }
func (staleDestinationProbe) cadence() time.Duration { return time.Minute }

func staleDestinationQuery(environment string) string {
	return fmt.Sprintf(
		`sum by (companion) (rate(urnetwork_connect_contract_failures_total{env=%s,cause="inactive_destination"}[%s])) * 60`,
		strconv.Quote(environment),
		staleDestinationRange,
	)
}

func (staleDestinationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("stale destination: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(staleDestinationQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("stale destination: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("stale destination: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"stale destination: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	rates := map[string]float64{}
	observedTimes := []time.Time{}
	for _, series := range response.Data.Result {
		companion := series.Metric["companion"]
		if companion != "false" && companion != "true" {
			return nil, fmt.Errorf("stale destination: unexpected companion partition %q", companion)
		}
		if _, duplicate := rates[companion]; duplicate {
			return nil, fmt.Errorf("stale destination: duplicate companion partition %q", companion)
		}
		observedAt, rate, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("stale destination: parse companion=%s sample: %w", companion, err)
		}
		age := env.now().UTC().Sub(observedAt)
		if age > staleDestinationFreshness || age < -30*time.Second {
			return nil, fmt.Errorf("stale destination: stale companion=%s sample age=%s", companion, age.Round(time.Second))
		}
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return nil, fmt.Errorf("stale destination: invalid companion=%s rate %v", companion, rate)
		}
		rates[companion] = rate
		observedTimes = append(observedTimes, observedAt)
	}
	for _, companion := range []string{"false", "true"} {
		if _, ok := rates[companion]; !ok {
			return nil, fmt.Errorf("stale destination: missing companion partition %q", companion)
		}
	}
	sort.Slice(observedTimes, func(i, j int) bool { return observedTimes[i].Before(observedTimes[j]) })
	if !observedTimes[0].Equal(observedTimes[len(observedTimes)-1]) {
		return nil, fmt.Errorf(
			"stale destination: partition sample times differ: %s",
			strings.Join([]string{
				observedTimes[0].Format(time.RFC3339Nano),
				observedTimes[len(observedTimes)-1].Format(time.RFC3339Nano),
			}, " and "),
		)
	}

	totalRate := rates["false"] + rates["true"]
	if totalRate <= staleDestinationWarnPerMinute {
		return []finding{healthyFinding(
			"mimir/stale-destination", tierWarn, "stale-destination-rate", "api-fleet",
		)}, nil
	}

	return []finding{{
		probeId: "mimir/stale-destination", tier: tierWarn,
		class: "stale-destination-rate", target: "api-fleet", frame: "lifecycle-rejection", sustain: 1,
		symptom: fmt.Sprintf(
			"API rejected inactive contract destinations at %.1f/min over five minutes",
			totalRate,
		),
		mechanism: "The active-only contract guard found the requested destination missing or inactive before provide-mode selection or at the final write boundary. This prevents the previous failure mode in which a stale Redis provide advertisement could authorize a successful contract to an identity that could no longer receive it.",
		baseline: fmt.Sprintf(
			"The aggregate inactive-destination rejection rate stays at or below %.0f/min, both companion partitions remain explicitly observable, and successful contracts to destinations already inactive at creation remain zero.",
			staleDestinationWarnPerMinute,
		),
		observed: fmt.Sprintf(
			"rate_per_minute=%.3f companion_false_rate=%.3f companion_true_rate=%.3f range=%s sample_time=%s metrics_gateway=%s",
			totalRate,
			rates["false"],
			rates["true"],
			staleDestinationRange,
			observedTimes[0].Format(time.RFC3339),
			metricHost.name,
		),
		evidence: "The API owns this bounded counter at the rejection boundary and initializes both companion labels to zero. An absent partition is therefore rollout or ingestion loss, not a healthy zero; no client, network, contract, or destination identifier enters the metric.",
		context:  "These are prevented stale contracts, not successful routes and not a hardware-capacity signal. The API guard protects correctness immediately, but an older client can keep retrying the same dead exit. A Connect-bearing client that understands ContractError_Reliability retires only the emitting window channel and refills through the existing selection path.",
		action:   "First require every API artifact to contain server commit c8dfe570 and every affected Connect-bearing client artifact to contain Connect commit 5b33c91. If the rate remains above the boundary after two complete five-minute windows and the deployed client-window lifetime, check §2.8, §2.9, §2.15, and §2.16, then use bounded lifecycle and relationship cohorts to locate the stale producer. Do not delete Redis provide keys, weaken lifecycle checks, lengthen contract timeouts, or restart clients to manufacture recovery.",
		verify:   "Every API instance exports both initialized partitions; successful contracts to already-inactive destinations remain zero; the aggregate rejection rate stays at or below 50/min for two complete five-minute windows; and a Reliability result removes only its emitting exit before the window refills.",
		playbook: "SIGNALS.md §2.18 and §5.9",
	}}, nil
}
