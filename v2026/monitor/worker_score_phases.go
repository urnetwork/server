package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	workerScorePhaseActive uint8 = 1 << iota
	workerScorePhaseDuration
	workerScorePhaseExits
	workerScorePhaseItems
	workerScorePhaseBytes
	workerScorePhaseAll = workerScorePhaseActive |
		workerScorePhaseDuration |
		workerScorePhaseExits |
		workerScorePhaseItems |
		workerScorePhaseBytes
)

var workerScorePhaseNames = [...]string{
	"source_load",
	"target_export",
	"target_map",
	"gob_encode",
	"cache_write",
}

type workerScorePhaseMetric struct {
	active       float64
	durationRate float64
	exitRate     float64
	itemRate     float64
	byteRate     float64
	mask         uint8
}

type workerScorePhaseObservation struct {
	phases map[string]workerScorePhaseMetric
}

func workerScorePhaseKey(host, block, instance string) string {
	return host + "\x00" + block + "\x00" + instance
}

func loadWorkerScorePhaseObservations(
	ctx context.Context,
	env *probeEnv,
	metricHosts []*host,
	preferred *host,
) (map[string]workerScorePhaseObservation, error) {
	queries := []string{
		fmt.Sprintf(`label_replace(urnetwork_update_client_scores_phase_active{env=%s,job="taskworker"},"monitor_score_phase_metric","active","__name__",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(urnetwork_update_client_scores_phase_duration_seconds_total{env=%s,job="taskworker"}[1m]),"monitor_score_phase_metric","duration","__name__",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(urnetwork_update_client_scores_phase_exits_total{env=%s,job="taskworker"}[1m]),"monitor_score_phase_metric","exits","__name__",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(urnetwork_update_client_scores_phase_work_items_total{env=%s,job="taskworker"}[1m]),"monitor_score_phase_metric","items","__name__",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(urnetwork_update_client_scores_phase_work_bytes_total{env=%s,job="taskworker"}[1m]),"monitor_score_phase_metric","bytes","__name__",".*")`, strconv.Quote(env.cfg.env)),
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(strings.Join(queries, " or "))
	out, _, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		preferred,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("query phase metrics: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("decode phase metrics: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"phase metrics status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	validPhase := map[string]bool{}
	for _, phase := range workerScorePhaseNames {
		validPhase[phase] = true
	}
	now := env.now().UTC()
	byWorker := map[string]map[string]workerScorePhaseMetric{}
	for _, series := range response.Data.Result {
		phase := series.Metric["phase"]
		if !validPhase[phase] {
			continue
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			continue
		}
		age := now.Sub(observedAt)
		if age > workerChurnFreshness || age < -30*time.Second {
			continue
		}
		host := series.Metric["host"]
		if host == "" {
			continue
		}
		key := workerScorePhaseKey(host, series.Metric["block"], series.Metric["instance"])
		phases := byWorker[key]
		if phases == nil {
			phases = map[string]workerScorePhaseMetric{}
			byWorker[key] = phases
		}
		metric := phases[phase]
		switch series.Metric["monitor_score_phase_metric"] {
		case "active":
			metric.active = value
			metric.mask |= workerScorePhaseActive
		case "duration":
			metric.durationRate = value
			metric.mask |= workerScorePhaseDuration
		case "exits":
			metric.exitRate = value
			metric.mask |= workerScorePhaseExits
		case "items":
			metric.itemRate = value
			metric.mask |= workerScorePhaseItems
		case "bytes":
			metric.byteRate = value
			metric.mask |= workerScorePhaseBytes
		default:
			continue
		}
		phases[phase] = metric
	}

	observations := map[string]workerScorePhaseObservation{}
	for key, phases := range byWorker {
		complete := true
		for _, phase := range workerScorePhaseNames {
			if phases[phase].mask != workerScorePhaseAll {
				complete = false
				break
			}
		}
		if complete {
			observations[key] = workerScorePhaseObservation{phases: phases}
		}
	}
	return observations, nil
}

func formatWorkerScorePhaseValues(observation workerScorePhaseObservation, value func(workerScorePhaseMetric) float64, format string) string {
	values := make([]string, 0, len(workerScorePhaseNames))
	for _, phase := range workerScorePhaseNames {
		values = append(values, phase+":"+fmt.Sprintf(format, value(observation.phases[phase])))
	}
	return strings.Join(values, ",")
}

func (observation workerScorePhaseObservation) summary() string {
	return fmt.Sprintf(
		"score_phase_observability=ready score_phase_active=%s score_phase_seconds_per_s_1m=%s score_phase_exits_per_s_1m=%s score_phase_items_per_s_1m=%s score_phase_mib_per_s_1m=%s",
		formatWorkerScorePhaseValues(observation, func(metric workerScorePhaseMetric) float64 { return metric.active }, "%.0f"),
		formatWorkerScorePhaseValues(observation, func(metric workerScorePhaseMetric) float64 { return metric.durationRate }, "%.3f"),
		formatWorkerScorePhaseValues(observation, func(metric workerScorePhaseMetric) float64 { return metric.exitRate }, "%.3f"),
		formatWorkerScorePhaseValues(observation, func(metric workerScorePhaseMetric) float64 { return metric.itemRate }, "%.1f"),
		formatWorkerScorePhaseValues(observation, func(metric workerScorePhaseMetric) float64 { return metric.byteRate / float64(1<<20) }, "%.2f"),
	)
}

func (observation workerScorePhaseObservation) discriminator() string {
	// target_export is a parent span. Prefer the four mutually meaningful work
	// phases so its expected overlap cannot hide the current inner owner.
	phases := []string{"source_load", "target_map", "gob_encode", "cache_write"}
	activePhase := ""
	active := float64(0)
	for _, phase := range phases {
		if value := observation.phases[phase].active; active < value {
			activePhase = phase
			active = value
		}
	}
	if activePhase != "" {
		return fmt.Sprintf("the scrape found %.0f active %s spans", active, activePhase)
	}
	dominantPhase := phases[0]
	for _, phase := range phases[1:] {
		if observation.phases[dominantPhase].durationRate < observation.phases[phase].durationRate {
			dominantPhase = phase
		}
	}
	return fmt.Sprintf(
		"no inner span was active at the instant scrape; %s had the highest completed-span occupancy over the minute at %.3f seconds/second",
		dominantPhase,
		observation.phases[dominantPhase].durationRate,
	)
}
