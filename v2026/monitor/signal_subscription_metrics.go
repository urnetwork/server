package monitor

// The subscription dashboard is useful only when its singleton task, complete
// fleet-wide metric snapshot, and authenticated Grafana definition all exist.
// This probe joins those three bounded layers without exporting task payloads,
// metric identity labels, or Grafana response bodies.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	subscriptionMetricsTaskFunction         = "github.com/urnetwork/server/v2026/controller.SubscriptionMetricsSync"
	subscriptionMetricsDashboardUid         = "urnetwork-subscriptions"
	subscriptionMetricsDashboardTitle       = "urnetwork / subscriptions"
	subscriptionMetricsSnapshotFreshness    = 90 * time.Second
	subscriptionMetricsSnapshotMaximumAge   = 30 * time.Minute
	subscriptionMetricsPublicationAllowance = 2 * time.Minute
	subscriptionMetricsDashboardBodyLimit   = 64 * 1024
)

const subscriptionMetricsTaskStateQuery = `
/* monitor-signal-2.27-subscription-metrics-task-state */
WITH task_state AS (
    SELECT count(*)::bigint AS task_count,
           count(*) FILTER (WHERE reschedule_error IS NOT NULL)::bigint AS task_error_count,
           count(*) FILTER (WHERE release_time > now())::bigint AS task_claimed_count
    FROM pending_task
    WHERE function_name = 'github.com/urnetwork/server/controller.SubscriptionMetricsSync'
), latest_finished AS MATERIALIZED (
    SELECT run_end_time, post_completed, post_error IS NOT NULL AS post_error
    FROM finished_task
    WHERE function_name = 'github.com/urnetwork/server/controller.SubscriptionMetricsSync'
      AND run_end_time >= now() - interval '2 hours'
    ORDER BY run_end_time DESC
    LIMIT 1
)
SELECT task_state.task_count,
       task_state.task_error_count,
       task_state.task_claimed_count,
       COALESCE(extract(epoch FROM now() - latest_finished.run_end_time)::bigint, -1),
       CASE WHEN latest_finished.run_end_time IS NULL THEN -1
            WHEN latest_finished.post_completed THEN 1 ELSE 0 END,
       CASE WHEN latest_finished.run_end_time IS NULL THEN -1
            WHEN latest_finished.post_error THEN 1 ELSE 0 END
FROM task_state
LEFT JOIN latest_finished ON true
`

// SIGNALS.md §2.27 maps to signal_subscription_metrics.go and
// signal_subscription_metrics_test.go.
func NewSubscriptionMetricsSignal() Signal {
	return newSubscriptionMetricsSignal(&http.Client{Timeout: 10 * time.Second}, "")
}

func newSubscriptionMetricsSignal(client grafanaDatasourceHTTPClient, endpoint string) Signal {
	return &signalAdapter{
		number: "2.27", key: "subscription-metrics", name: "Subscription dashboard snapshot liveness",
		probe: subscriptionMetricsProbe{client: client, endpoint: endpoint},
	}
}

type subscriptionMetricsProbe struct {
	client   grafanaDatasourceHTTPClient
	endpoint string
}

func (subscriptionMetricsProbe) id() string             { return "observability/subscription-metrics" }
func (subscriptionMetricsProbe) tier() string           { return tierPage }
func (subscriptionMetricsProbe) cadence() time.Duration { return 5 * time.Minute }

// Retains only aggregate singleton and latest-completion state.
type subscriptionMetricsTaskState struct {
	taskCount        int64
	taskErrorCount   int64
	taskClaimedCount int64
	finishedAge      time.Duration
	finishedPresent  bool
	postCompleted    bool
	postError        bool
}

// Retains a fleet-reduced value without publisher identity labels.
type subscriptionMetricsSnapshotObservation struct {
	present      bool
	observedTime time.Time
	snapshotTime time.Time
}

// Retains only status and exact-identity comparisons, never response content.
type subscriptionMetricsDashboardObservation struct {
	httpStatus   int
	uidMatches   bool
	titleMatches bool
}

func (self subscriptionMetricsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings := []finding{}
	var taskState *subscriptionMetricsTaskState
	rows, err := env.runner.pg(ctx, subscriptionMetricsTaskStateQuery)
	if err != nil {
		findings = append(findings, cannotObserveFinding("subscription-metrics/task-state", err))
	} else {
		parsed, parseErr := parseSubscriptionMetricsTaskState(rows)
		if parseErr != nil {
			findings = append(findings, cannotObserveFinding("subscription-metrics/task-state", parseErr))
		} else {
			taskState = &parsed
			findings = append(findings, subscriptionMetricsTaskStateFinding(parsed))
		}
	}

	observation, observationErr := observeSubscriptionMetricsSnapshot(ctx, env)
	if observationErr != nil {
		findings = append(findings, cannotObserveFinding("subscription-metrics/snapshot", observationErr))
	} else {
		findings = append(findings, subscriptionMetricsSnapshotFindings(env.now().UTC(), observation, taskState)...)
	}

	findings = append(findings, self.dashboardFindings(ctx, env)...)
	return findings, nil
}

func parseSubscriptionMetricsTaskState(rows []pgRow) (subscriptionMetricsTaskState, error) {
	if len(rows) != 1 || len(rows[0]) != 6 {
		return subscriptionMetricsTaskState{}, fmt.Errorf("subscription metrics task state returned %d rows; want one six-column row", len(rows))
	}
	values := make([]int64, 0, 6)
	for column := range 6 {
		value, err := parseStrictInt64(rows[0].str(column))
		if err != nil {
			return subscriptionMetricsTaskState{}, fmt.Errorf("subscription metrics task state column %d: %w", column, err)
		}
		values = append(values, value)
	}
	if values[0] < 0 || values[1] < 0 || values[2] < 0 || values[2] > values[0] || values[3] < -1 {
		return subscriptionMetricsTaskState{}, fmt.Errorf("subscription metrics task state has contradictory counts or age")
	}
	if values[4] < -1 || values[4] > 1 || values[5] < -1 || values[5] > 1 ||
		(values[3] == -1) != (values[4] == -1) || (values[3] == -1) != (values[5] == -1) {
		return subscriptionMetricsTaskState{}, fmt.Errorf("subscription metrics task state has contradictory completion state")
	}
	return subscriptionMetricsTaskState{
		taskCount: values[0], taskErrorCount: values[1], taskClaimedCount: values[2],
		finishedAge: time.Duration(values[3]) * time.Second, finishedPresent: values[3] >= 0,
		postCompleted: values[4] == 1, postError: values[5] == 1,
	}, nil
}

func subscriptionMetricsTaskStateFinding(state subscriptionMetricsTaskState) finding {
	if state.taskCount == 1 && state.taskErrorCount == 0 && (!state.finishedPresent || state.postCompleted && !state.postError) {
		return healthyFinding("observability/subscription-metrics", tierPage, "subscription-metrics-task-chain", "SubscriptionMetricsSync")
	}
	finishedAge := int64(-1)
	if state.finishedPresent {
		finishedAge = int64(state.finishedAge / time.Second)
	}
	return finding{
		probeId: "observability/subscription-metrics", tier: tierPage,
		class: "subscription-metrics-task-chain", target: "SubscriptionMetricsSync", sustain: 2,
		symptom:   "The subscription metrics RunOnce chain is missing, duplicated, errored, or failed in Post",
		mechanism: "The dashboard exporter has one fleet-wide recurring owner. A missing row loses future snapshots, multiple rows violate singleton ownership, a reschedule error parks execution behind backoff, and a failed Post can finish one snapshot without scheduling its successor.",
		baseline:  "Exactly one canonical SubscriptionMetricsSync pending row exists, carries no reschedule error, and every recent finished run completed Post.",
		observed:  fmt.Sprintf("task_count=%d task_error_count=%d task_claimed_count=%d latest_finished_age_seconds=%d post_completed=%t post_error=%t", state.taskCount, state.taskErrorCount, state.taskClaimedCount, finishedAge, state.postCompleted, state.postError),
		evidence:  "Only exact-function aggregate counts, completion age, and Boolean Post state leave PostgreSQL; task IDs, arguments, results, client identity, and error text are excluded.",
		action:    "Inspect the latest bounded Taskworker execution and repair its query, dependency, or Post error. Seed the canonical RunOnce task only when it is genuinely absent; never create a second owner or edit a task payload.",
		verify:    "Exactly one clean pending owner remains and two natural 15-minute runs complete, publish a snapshot, and schedule one successor each.",
		playbook:  "SIGNALS.md §2.27 and §2.5",
	}
}

func subscriptionMetricsSnapshotQuery(environment string) string {
	selector := `urnetwork_subscription_snapshot_timestamp_seconds{env=` + strconv.Quote(environment) + `,service="taskworker"}`
	return `topk(1, ` + selector + ` and on(env,service,block,host,instance) (` +
		`timestamp(` + selector + `) >= time() - ` + strconv.FormatInt(int64(subscriptionMetricsSnapshotFreshness/time.Second), 10) + `))`
}

func observeSubscriptionMetricsSnapshot(ctx context.Context, env *probeEnv) (subscriptionMetricsSnapshotObservation, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("no services host is configured for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(subscriptionMetricsSnapshotQuery(env.cfg.env))
	output, _, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("query Mimir through service gateways: %w", err)
	}
	return parseSubscriptionMetricsSnapshot(output, env.now().UTC())
}

func parseSubscriptionMetricsSnapshot(output string, now time.Time) (subscriptionMetricsSnapshotObservation, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("Mimir status=%q result_type=%q error=%q", response.Status, response.Data.ResultType, response.Error)
	}
	if len(response.Data.Result) == 0 {
		return subscriptionMetricsSnapshotObservation{}, nil
	}
	if len(response.Data.Result) != 1 {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("Mimir returned %d top snapshot samples; want at most one", len(response.Data.Result))
	}
	observedTime, value, err := mimirInstantValue(response.Data.Result[0].Value)
	if err != nil {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("parse snapshot sample: %w", err)
	}
	observedAge := now.Sub(observedTime)
	if observedAge > subscriptionMetricsSnapshotFreshness || observedAge < -30*time.Second {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("snapshot sample source age %s is outside the freshness bound", observedAge.Round(time.Second))
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value != math.Trunc(value) {
		return subscriptionMetricsSnapshotObservation{}, fmt.Errorf("snapshot timestamp value is invalid")
	}
	if value == 0 {
		return subscriptionMetricsSnapshotObservation{observedTime: observedTime}, nil
	}
	return subscriptionMetricsSnapshotObservation{
		present: true, observedTime: observedTime, snapshotTime: time.Unix(int64(value), 0).UTC(),
	}, nil
}

func subscriptionMetricsSnapshotFindings(
	now time.Time,
	observation subscriptionMetricsSnapshotObservation,
	taskState *subscriptionMetricsTaskState,
) []finding {
	healthy := func(class string) finding {
		return healthyFinding("observability/subscription-metrics", tierPage, class, "subscription-metrics-snapshot")
	}
	findings := []finding{
		healthy("subscription-metrics-snapshot-stale"),
		healthy("subscription-metrics-publication-gap"),
		healthy("subscription-metrics-snapshot-future"),
	}
	snapshotAge := time.Duration(-1)
	if observation.present {
		snapshotAge = now.Sub(observation.snapshotTime)
		if snapshotAge < -30*time.Second {
			findings[2] = subscriptionMetricsSnapshotFinding(
				"subscription-metrics-snapshot-future",
				"The newest subscription snapshot timestamp is in the future",
				"The exporter clock or timestamp publication is outside the fleet's bounded clock discipline, so freshness comparisons could accept an arbitrarily stale business snapshot.",
				snapshotAge,
				taskState,
			)
			return findings
		}
	}
	if taskState != nil && taskState.finishedPresent && taskState.postCompleted && !taskState.postError &&
		taskState.finishedAge <= subscriptionMetricsSnapshotMaximumAge {
		finishedTime := now.Add(-taskState.finishedAge)
		if !observation.present || observation.snapshotTime.Before(finishedTime.Add(-subscriptionMetricsPublicationAllowance)) {
			findings[1] = subscriptionMetricsSnapshotFinding(
				"subscription-metrics-publication-gap",
				"A recent completed subscription task did not reach the fleet-wide snapshot metric",
				"PostgreSQL records a successful recent execution, but Mimir has no snapshot within the two-minute publication allowance. The loss is after task execution: in-process collection, scrape, remote write, or ingestion.",
				snapshotAge,
				taskState,
			)
			return findings
		}
	}
	if !observation.present || snapshotAge > subscriptionMetricsSnapshotMaximumAge {
		findings[0] = subscriptionMetricsSnapshotFinding(
			"subscription-metrics-snapshot-stale",
			"The subscription dashboard has no current complete Taskworker snapshot",
			"No fleet Taskworker has published a completed snapshot inside the liveness bound. When task completion is also old, the query or recurring execution path owns the outage rather than Mimir publication alone.",
			snapshotAge,
			taskState,
		)
	}
	return findings
}

func subscriptionMetricsSnapshotFinding(
	class string,
	symptom string,
	mechanism string,
	snapshotAge time.Duration,
	taskState *subscriptionMetricsTaskState,
) finding {
	snapshotAgeSeconds := int64(-1)
	if snapshotAge >= 0 {
		snapshotAgeSeconds = int64(snapshotAge / time.Second)
	}
	finishedAgeSeconds := int64(-1)
	if taskState != nil && taskState.finishedPresent {
		finishedAgeSeconds = int64(taskState.finishedAge / time.Second)
	}
	return finding{
		probeId: "observability/subscription-metrics", tier: tierPage,
		class: class, target: "subscription-metrics-snapshot", sustain: 2,
		symptom: symptom, mechanism: mechanism,
		baseline: "The fleet-wide top Taskworker snapshot is from an actual scrape no more than 90 seconds old, has a value no more than 30 minutes old or 30 seconds future, and follows the latest successful task completion within two minutes.",
		observed: fmt.Sprintf("snapshot_age_seconds=%d latest_finished_age_seconds=%d", snapshotAgeSeconds, finishedAgeSeconds),
		evidence: "The Mimir query returns one fleet-wide top timestamp only. Host, block, instance, task, customer, payment, network, user, and client identities are not rendered.",
		action:   "Use the task-state discriminator first. If completion is current, repair Taskworker collection, scrape, remote write, or Mimir ingestion; if both are old, repair the recurring task or its aggregate query. Do not replace absent telemetry with zero.",
		verify:   "Two natural snapshots arrive within 30 minutes, each follows its successful task completion within two minutes, and the dashboard renders from the same fresh publisher identity.",
		playbook: "SIGNALS.md §2.27",
	}
}

func (self subscriptionMetricsProbe) dashboardFindings(ctx context.Context, env *probeEnv) []finding {
	environment := strings.TrimSpace(env.cfg.env)
	domain := strings.TrimSpace(env.cfg.publicDomain)
	if environment == "" || domain == "" {
		return nil
	}
	hostname := environment + "-grafana." + domain
	target := hostname + "/" + subscriptionMetricsDashboardUid
	if env.cfg.grafanaAdminPassword == "" {
		return []finding{subscriptionMetricsDashboardFinding("subscription-dashboard-auth", target, 0, false, false)}
	}
	client := self.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := strings.TrimRight(self.endpoint, "/")
	if endpoint == "" {
		endpoint = "https://" + hostname
	}
	observation, err := observeSubscriptionMetricsDashboard(
		ctx,
		client,
		endpoint+"/api/dashboards/uid/"+subscriptionMetricsDashboardUid,
		env.cfg.grafanaAdminPassword,
	)
	if err != nil {
		return []finding{cannotObserveFinding(target, err)}
	}
	if 200 <= observation.httpStatus && observation.httpStatus < 300 && observation.uidMatches && observation.titleMatches {
		return []finding{healthyFinding("observability/subscription-metrics", tierPage, "subscription-dashboard-identity", target)}
	}
	class := "subscription-dashboard-http"
	switch observation.httpStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		class = "subscription-dashboard-auth"
	case http.StatusNotFound:
		class = "subscription-dashboard-missing"
	default:
		if 200 <= observation.httpStatus && observation.httpStatus < 300 {
			class = "subscription-dashboard-identity"
		}
	}
	return []finding{subscriptionMetricsDashboardFinding(class, target, observation.httpStatus, observation.uidMatches, observation.titleMatches)}
}

func observeSubscriptionMetricsDashboard(
	ctx context.Context,
	client grafanaDatasourceHTTPClient,
	endpoint string,
	password string,
) (subscriptionMetricsDashboardObservation, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return subscriptionMetricsDashboardObservation{}, err
	}
	request.SetBasicAuth("admin", password)
	response, err := client.Do(request)
	if err != nil {
		return subscriptionMetricsDashboardObservation{}, err
	}
	defer response.Body.Close()
	observation := subscriptionMetricsDashboardObservation{httpStatus: response.StatusCode}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, subscriptionMetricsDashboardBodyLimit))
		return observation, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, subscriptionMetricsDashboardBodyLimit+1))
	if err != nil {
		return subscriptionMetricsDashboardObservation{}, err
	}
	if len(body) > subscriptionMetricsDashboardBodyLimit {
		return subscriptionMetricsDashboardObservation{}, fmt.Errorf("Grafana dashboard response exceeded %d bytes", subscriptionMetricsDashboardBodyLimit)
	}
	var envelope struct {
		Dashboard struct {
			UID   string `json:"uid"`
			Title string `json:"title"`
		} `json:"dashboard"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return subscriptionMetricsDashboardObservation{}, fmt.Errorf("decode Grafana dashboard identity: %w", err)
	}
	observation.uidMatches = envelope.Dashboard.UID == subscriptionMetricsDashboardUid
	observation.titleMatches = envelope.Dashboard.Title == subscriptionMetricsDashboardTitle
	return observation, nil
}

func subscriptionMetricsDashboardFinding(class, target string, status int, uidMatches, titleMatches bool) finding {
	symptom := "The live subscription dashboard definition is unavailable"
	mechanism := "Grafana returned an unexpected HTTP status for the authenticated stable-UID lookup. The service and datasource can remain healthy while the dashboard definition is unreadable."
	if class == "subscription-dashboard-auth" {
		symptom = "The subscription dashboard probe cannot authenticate to Grafana"
		mechanism = "The configured Grafana admin credential is absent or the authenticated stable-UID lookup was denied; dashboard presence is unknown rather than healthy."
	} else if class == "subscription-dashboard-missing" {
		symptom = "The live Grafana instance does not contain the subscription dashboard UID"
		mechanism = "The exporter may be healthy while `grafana load-defaults` omitted or failed to load the authenticated subscription dashboard. A healthy Grafana process and datasource do not prove this definition exists."
	} else if class == "subscription-dashboard-identity" {
		symptom = "The live subscription dashboard UID or title does not match the source contract"
		mechanism = "The stable-UID lookup returned a document, but its bounded identity is not the expected authenticated subscription dashboard. A stale or colliding definition can silently show the wrong operational contract."
	}
	return finding{
		probeId: "observability/subscription-metrics", tier: tierPage,
		class: class, target: target, sustain: 2,
		symptom: symptom, mechanism: mechanism,
		baseline: "Authenticated GET /api/dashboards/uid/urnetwork-subscriptions returns HTTP 2xx with UID urnetwork-subscriptions and title urnetwork / subscriptions.",
		observed: fmt.Sprintf("http_status=%d uid_matches=%t title_matches=%t", status, uidMatches, titleMatches),
		evidence: "Only HTTP status and two identity-match Booleans are retained; the admin credential and Grafana response body never enter the finding.",
		action:   "Repair Grafana authentication or run the supported load-defaults workflow from the intended server dashboard artifact. Do not make the dashboard public, hand-edit a colliding UID, or treat a healthy datasource as proof that the definition loaded.",
		verify:   "Two authenticated reads return the exact UID and title after every active Grafana generation loads defaults; the fresh Taskworker snapshot remains independently healthy.",
		playbook: "SIGNALS.md §2.27 and §11.15",
	}
}
