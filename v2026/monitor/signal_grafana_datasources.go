package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const grafanaDatasourceResponseLimit = 64 * 1024

// Signal grafana-datasources implements SIGNALS.md §11.15. Health endpoints,
// direct Loki/Mimir reads, and provisioned database rows can all be green
// while Grafana cannot instantiate a datasource plugin. This probe executes a
// bounded query through Grafana's own /api/ds/query boundary for each required
// datasource.
func NewGrafanaDatasourcesSignal() Signal {
	return newGrafanaDatasourcesSignal(&http.Client{Timeout: 10 * time.Second}, "")
}

type grafanaDatasourceHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

func newGrafanaDatasourcesSignal(client grafanaDatasourceHTTPClient, endpoint string) Signal {
	return &signalAdapter{
		number: "11.15", key: "grafana-datasources", name: "Grafana datasource executability",
		probe: grafanaDatasourcesProbe{client: client, endpoint: endpoint},
	}
}

type grafanaDatasourcesProbe struct {
	client   grafanaDatasourceHTTPClient
	endpoint string
}

func (grafanaDatasourcesProbe) id() string             { return "observability/grafana-datasources" }
func (grafanaDatasourcesProbe) tier() string           { return tierWarn }
func (grafanaDatasourcesProbe) cadence() time.Duration { return time.Minute }

type grafanaDatasourceQuerySpec struct {
	uid      string
	typeName string
	pluginID string
	expr     string
}

var requiredGrafanaDatasourceQueries = []grafanaDatasourceQuerySpec{
	{uid: "warp-mimir", typeName: "prometheus", pluginID: "prometheus", expr: "vector(1)"},
	{uid: "warp-loki", typeName: "loki", pluginID: "loki", expr: `sum(count_over_time({service="web"}[1m]))`},
}

type grafanaDatasourceQuerySample struct {
	httpStatus   int
	resultStatus int
	resultError  string
	response     string
	parseError   string
	resultSeen   bool
}

func (p grafanaDatasourcesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	environment := strings.TrimSpace(env.cfg.env)
	domain := strings.TrimSpace(env.cfg.publicDomain)
	if environment == "" || domain == "" {
		return nil, nil
	}
	if env.cfg.grafanaAdminPassword == "" {
		return nil, fmt.Errorf("grafana datasource probe: admin password is not configured")
	}
	client := p.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	hostname := environment + "-grafana." + domain
	endpoint := p.endpoint
	if endpoint == "" {
		endpoint = "https://" + hostname + "/api/ds/query"
	}

	findings := make([]finding, 0, 2*len(requiredGrafanaDatasourceQueries))
	for _, spec := range requiredGrafanaDatasourceQueries {
		sample, err := queryGrafanaDatasource(
			ctx,
			client,
			endpoint,
			env.cfg.grafanaAdminPassword,
			spec,
		)
		target := hostname + "/" + spec.uid
		if err != nil {
			findings = append(findings, cannotObserveFinding(target, err))
			continue
		}
		findings = append(findings, evaluateGrafanaDatasourceQuery(target, spec, sample)...)
	}
	return findings, nil
}

func queryGrafanaDatasource(
	ctx context.Context,
	client grafanaDatasourceHTTPClient,
	endpoint string,
	adminPassword string,
	spec grafanaDatasourceQuerySpec,
) (grafanaDatasourceQuerySample, error) {
	payload := map[string]any{
		"from": "now-1m",
		"to":   "now",
		"queries": []any{
			map[string]any{
				"refId":      "A",
				"datasource": map[string]string{"uid": spec.uid, "type": spec.typeName},
				"expr":       spec.expr,
				"instant":    true,
				"range":      false,
				"queryType":  "instant",
				"format":     "time_series",
				"maxLines":   1,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return grafanaDatasourceQuerySample{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return grafanaDatasourceQuerySample{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("admin", adminPassword)
	response, err := client.Do(request)
	if err != nil {
		return grafanaDatasourceQuerySample{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, grafanaDatasourceResponseLimit+1))
	if err != nil {
		return grafanaDatasourceQuerySample{}, err
	}
	if len(responseBody) > grafanaDatasourceResponseLimit {
		return grafanaDatasourceQuerySample{}, fmt.Errorf("Grafana datasource response exceeded %d bytes", grafanaDatasourceResponseLimit)
	}
	sample := grafanaDatasourceQuerySample{
		httpStatus: response.StatusCode,
		response:   strings.Join(strings.Fields(string(responseBody)), " "),
	}
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		return sample, nil
	}
	var envelope struct {
		Results map[string]struct {
			Status int    `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		sample.parseError = err.Error()
		return sample, nil
	}
	result, ok := envelope.Results["A"]
	sample.resultSeen = ok
	if ok {
		sample.resultStatus = result.Status
		sample.resultError = strings.Join(strings.Fields(result.Error), " ")
	}
	return sample, nil
}

func grafanaDatasourceQueryHealthy(sample grafanaDatasourceQuerySample) bool {
	return 200 <= sample.httpStatus && sample.httpStatus < 300 &&
		sample.resultSeen && sample.parseError == "" && sample.resultError == "" &&
		200 <= sample.resultStatus && sample.resultStatus < 300
}

func evaluateGrafanaDatasourceQuery(target string, spec grafanaDatasourceQuerySpec, sample grafanaDatasourceQuerySample) []finding {
	pluginHealthy := healthyFinding(
		"observability/grafana-datasources", tierWarn, "grafana-plugin-unregistered", target,
	)
	queryHealthy := healthyFinding(
		"observability/grafana-datasources", tierWarn, "grafana-datasource-query", target,
	)
	if grafanaDatasourceQueryHealthy(sample) {
		return []finding{pluginHealthy, queryHealthy}
	}

	detail := firstNonempty(sample.resultError, sample.parseError, sample.response, "empty response")
	observed := fmt.Sprintf(
		"datasource=%s type=%s plugin=%q http_status=%d result_status=%d result_seen=%t detail=%s",
		spec.uid,
		spec.typeName,
		spec.pluginID,
		sample.httpStatus,
		sample.resultStatus,
		sample.resultSeen,
		detail,
	)
	pluginMissing := strings.Contains(strings.ToLower(detail), "plugin.notregistered") ||
		strings.Contains(strings.ToLower(detail), "plugin not registered")
	if pluginMissing {
		return []finding{
			{
				probeId: "observability/grafana-datasources", tier: tierWarn,
				class: "grafana-plugin-unregistered", target: target, frame: spec.pluginID, sustain: 1,
				symptom:   fmt.Sprintf("Grafana cannot execute the provisioned %s datasource", spec.uid),
				mechanism: fmt.Sprintf("Grafana retained the provisioned %s row, but its custom image does not register native datasource plugin %q. Health endpoints and direct backend reads bypass plugin loading, so dashboards can render empty while the observability storage plane remains healthy.", spec.uid, spec.pluginID),
				baseline:  "Grafana /api/ds/query returns HTTP 200 and a successful result for both warp-mimir and warp-loki on every probe.",
				observed:  observed,
				evidence:  "bounded Grafana response: " + sample.response,
				context:   "This is an image/plugin packaging failure, not missing Loki events, a missing datasource database row, or a direct Mimir/Loki availability failure.",
				action:    fmt.Sprintf("Bake the signed, catalog-checksum-pinned %s plugin into the Grafana image for every supported architecture, retain disabled runtime downloads, and deploy a new image. Do not recreate %s or restart the same artifact.", spec.pluginID, spec.uid),
				verify:    fmt.Sprintf("The image packaging regression passes, %s returns a successful result through Grafana /api/ds/query, the Grafana deploy-readiness gate passes on every active block, and no new grafana-plugin-unregistered log appears after ingestion delay.", spec.uid),
				playbook:  "SIGNALS.md §11.15",
			},
			queryHealthy,
		}
	}

	return []finding{
		pluginHealthy,
		{
			probeId: "observability/grafana-datasources", tier: tierWarn,
			class: "grafana-datasource-query", target: target, frame: spec.uid, sustain: 2,
			symptom:   fmt.Sprintf("Grafana's provisioned %s datasource query is failing", spec.uid),
			mechanism: "The authenticated Grafana query boundary did not return a successful datasource result. This path includes Grafana authentication, plugin execution, the provisioned datasource URL, and the backend query; its bounded response distinguishes the failing layer.",
			baseline:  "Grafana /api/ds/query returns HTTP 200 and a successful result for both warp-mimir and warp-loki on every probe.",
			observed:  observed,
			evidence:  "bounded Grafana response: " + sample.response,
			context:   "A green /api/health or successful direct backend request does not clear this finding because both bypass the dashboard datasource boundary.",
			action:    fmt.Sprintf("Inspect the bounded %s result and the same-window Grafana log, then repair the named plugin, provisioned URL, or backend path. Do not recreate a correct datasource row or suppress the query error before identifying the failed layer.", spec.uid),
			verify:    fmt.Sprintf("%s returns a successful result through Grafana /api/ds/query on two consecutive probes and the deploy-readiness datasource check passes on every active Grafana block.", spec.uid),
			playbook:  "SIGNALS.md §11.15 and §11.9",
		},
	}
}
