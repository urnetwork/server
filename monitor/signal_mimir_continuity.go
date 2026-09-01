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
	mimirContinuityMarker       = "monitor-signal-11.20-mimir-continuity"
	mimirContinuityWindow       = 7 * 24 * time.Hour
	mimirContinuityStep         = 5 * time.Minute
	mimirContinuityLookback     = 2 * time.Minute
	mimirContinuityMissingSteps = 3
)

// Signal mimir-continuity implements SIGNALS.md §11.20. It uses an
// always-emitted service identity metric to distinguish raw Mimir sample loss
// from real zero traffic and from a Grafana panel or datasource failure.
func NewMimirContinuitySignal() Signal {
	return &signalAdapter{
		number: "11.20", key: "mimir-continuity", name: "Mimir historical sample continuity",
		probe: mimirContinuityProbe{},
	}
}

type mimirContinuityProbe struct{}

func (mimirContinuityProbe) id() string             { return "observability/mimir-continuity" }
func (mimirContinuityProbe) tier() string           { return tierWarn }
func (mimirContinuityProbe) cadence() time.Duration { return 5 * time.Minute }

type mimirRangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

type mimirContinuityGap struct {
	previous time.Time
	resumed  time.Time
	missing  int
}

func mimirContinuityQuery(environment string) string {
	return fmt.Sprintf(
		`sum(count_over_time(urnetwork_build_info{env=%s,instance!=""}[%s]))`,
		strconv.Quote(environment),
		mimirContinuityLookback,
	)
}

func (mimirContinuityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("mimir continuity: no services host in inventory for the loopback Mimir query")
	}

	end := env.now().UTC().Truncate(mimirContinuityStep)
	start := end.Add(-mimirContinuityWindow)
	queryValues := url.Values{}
	queryValues.Set("query", mimirContinuityQuery(env.cfg.env))
	queryValues.Set("start", strconv.FormatInt(start.Unix(), 10))
	queryValues.Set("end", strconv.FormatInt(end.Unix(), 10))
	queryValues.Set("step", strconv.FormatInt(int64(mimirContinuityStep/time.Second), 10))
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query_range?" + queryValues.Encode()
	command := "# " + mimirContinuityMarker + "\n" +
		"curl -fsS --max-time 20 '" + queryURL + "'"
	out, metricHost, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command)
	if err != nil {
		return nil, fmt.Errorf("mimir continuity: query Mimir through service gateways: %w", err)
	}

	timestamps, err := parseMimirContinuityTimestamps(out)
	if err != nil {
		return nil, fmt.Errorf("mimir continuity: response from %s: %w", metricHost.name, err)
	}
	gaps, err := findMimirContinuityGaps(timestamps)
	if err != nil {
		return nil, fmt.Errorf("mimir continuity: response from %s: %w", metricHost.name, err)
	}
	if len(gaps) == 0 {
		return []finding{healthyFinding(
			"observability/mimir-continuity", tierWarn, "mimir-ingestion-gap", "mimir-global-continuity",
		)}, nil
	}

	totalMissing := 0
	worst := gaps[0]
	evidence := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		totalMissing += gap.missing
		if gap.missing > worst.missing {
			worst = gap
		}
		evidence = append(evidence, fmt.Sprintf(
			"%s through %s: %d missing 5-minute evaluations (last present %s; resumed %s)",
			gap.previous.Add(mimirContinuityStep).Format(time.RFC3339),
			gap.resumed.Add(-mimirContinuityStep).Format(time.RFC3339),
			gap.missing,
			gap.previous.Format(time.RFC3339),
			gap.resumed.Format(time.RFC3339),
		))
	}

	worstMissingStart := worst.previous.Add(mimirContinuityStep)
	worstMissingEnd := worst.resumed.Add(-mimirContinuityStep)
	return []finding{{
		probeId: "observability/mimir-continuity", tier: tierWarn,
		class: "mimir-ingestion-gap", target: "mimir-global-continuity", frame: "urnetwork_build_info", sustain: 1,
		symptom: fmt.Sprintf(
			"Mimir lost %d five-minute control evaluations across %d bounded gap(s) in the public dashboard window",
			totalMissing, len(gaps),
		),
		mechanism: "The always-emitted build-info control is absent for the same stored intervals as independent Connect and taskworker metrics. The proven historical mechanism was removal of an ephemeral Grafana container while its Mimir child still had an unflushed partial TSDB head. This range detector proves permanent raw-sample loss, but it cannot infer whether each current child still renders flush_blocks_on_shutdown=false.",
		baseline: fmt.Sprintf(
			"The aggregate urnetwork_build_info control has a sample at every %s evaluation across the trailing %s; fewer than %d consecutive missing evaluations in any bounded interval remain below the alert threshold.",
			mimirContinuityStep, mimirContinuityWindow, mimirContinuityMissingSteps,
		),
		observed: fmt.Sprintf(
			"query_start=%s query_end=%s gateway=%s gaps=%d total_missing_steps=%d worst_missing_start=%s worst_missing_end=%s worst_missing_steps=%d",
			start.Format(time.RFC3339), end.Format(time.RFC3339), metricHost.name, len(gaps), totalMissing,
			worstMissingStart.Format(time.RFC3339), worstMissingEnd.Format(time.RFC3339), worst.missing,
		),
		evidence: strings.Join(evidence, "\n"),
		context:  "This is raw metric loss, not zero throughput: build-info is independent of user traffic, and the range query bypasses the Grafana panel. Existing holes are not reconstructable from Mimir and remain visible until they age out of the dashboard window. The separate §11.21 exact-process signal is the current-state gate; an old gap persisting after that fleet is healthy is not evidence that the fix is absent.",
		action:   "Run the §11.21 mimir-shutdown signal. If any exact child reports false, deploy a Grafana image from clean Warp commit 7176ccd or a clean descendant while retaining the parent's 120-second Mimir child stop allowance inside Warpctl's 3,600-second container drain. If every current child already reports true, do not redeploy solely for historical gaps; preserve the setting through the next ordinary Grafana rollout and use that shutdown as the discriminator. Do not zero-fill or span over the panel, and do not shared-mount one TSDB directory into overlapping old and new containers.",
		verify:   "Every exact active Mimir child renders flush_blocks_on_shutdown=true, and a controlled then full Grafana clean-shutdown rollout creates no new bounded build-info gap through the next block-upload window. Historical gaps clear only when they leave the seven-day range.",
		playbook: "SIGNALS.md §11.20 and §11.6",
	}}, nil
}

func parseMimirContinuityTimestamps(output string) ([]time.Time, error) {
	var response mimirRangeResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "matrix" {
		return nil, fmt.Errorf(
			"Mimir status=%q result_type=%q error=%q",
			response.Status, response.Data.ResultType, response.Error,
		)
	}
	if len(response.Data.Result) != 1 {
		return nil, fmt.Errorf("Mimir returned %d aggregate continuity series, want 1", len(response.Data.Result))
	}

	timestamps := make([]time.Time, 0, len(response.Data.Result[0].Values))
	for _, raw := range response.Data.Result[0].Values {
		observedAt, value, err := mimirInstantValue(raw)
		if err != nil {
			return nil, fmt.Errorf("parse continuity sample: %w", err)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return nil, fmt.Errorf("continuity control has invalid value %v at %s", value, observedAt.Format(time.RFC3339Nano))
		}
		timestamps = append(timestamps, observedAt.UTC())
	}
	if len(timestamps) == 0 {
		return nil, fmt.Errorf("Mimir returned no continuity control samples")
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i].Before(timestamps[j]) })
	return timestamps, nil
}

func findMimirContinuityGaps(timestamps []time.Time) ([]mimirContinuityGap, error) {
	if len(timestamps) == 0 {
		return nil, fmt.Errorf("no continuity timestamps")
	}
	gaps := []mimirContinuityGap{}
	for index := 1; index < len(timestamps); index++ {
		delta := timestamps[index].Sub(timestamps[index-1])
		stepCount := math.Round(float64(delta) / float64(mimirContinuityStep))
		if delta <= 0 || math.Abs(float64(delta)-stepCount*float64(mimirContinuityStep)) > float64(time.Millisecond) {
			return nil, fmt.Errorf(
				"irregular evaluation timestamps %s then %s",
				timestamps[index-1].Format(time.RFC3339Nano), timestamps[index].Format(time.RFC3339Nano),
			)
		}
		missing := int(stepCount) - 1
		if missing >= mimirContinuityMissingSteps {
			gaps = append(gaps, mimirContinuityGap{
				previous: timestamps[index-1],
				resumed:  timestamps[index],
				missing:  missing,
			})
		}
	}
	return gaps, nil
}
