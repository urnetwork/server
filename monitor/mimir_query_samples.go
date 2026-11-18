// The monitor's Mimir instant query for a caller outside it: the authenticated native HTTPS read the provider signals
// use (mimir_query_http.go), with its answer parsed into samples. `sidecarctl evs observe-report` reads
// urnetwork_evs_versionless_observe_total through it (owner decision 513, QD-987), so an operations command reads a
// counter where §19.8's rows do, with the query role's credential, the fixed evaluation time, the body bound and the
// settings fence that read enforces, and nothing of SSH.
package monitor

import (
	"context"
	"encoding/json"
	"time"
)

// One series of an instant query's vector answer.
type MimirQuerySample struct {
	Labels map[string]string
	Value  float64
}

// Runs one instant query evaluated at `at` against the env settings name, and returns its vector's samples; an empty
// vector is no sample and no error. maxByteCount bounds the answer's body. An error means the source did not answer
// with a vector: its text names a closed phase and never the query, the credential or the response.
//
// A settings Source that owns HTTP answers the request, as it does for the signals; with no Source the request goes
// out, and only under a SettingsGenerationCheck, as LoadSignalSettings returns, which fences the credential before
// the request and before its answer is taken.
func ReadMimirQuerySamples(ctx context.Context, settings SignalSettings, query string, at time.Time, maxByteCount int) ([]*MimirQuerySample, error) {
	env := &probeEnv{cfg: configFromSignalSettings(settings), now: settings.Now}
	if settings.Source != nil {
		env.mimirQueryClient, _ = settings.Source.(grafanaHTTPClient)
		if env.mimirQueryClient == nil {
			env.mimirQueryClient = unavailableMimirQuerySource{}
		}
	}
	body, err := readProviderMimirQuery(ctx, env, query, at, maxByteCount)
	if err != nil {
		return nil, err
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		return nil, &mimirQueryError{phase: "response-body"}
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, &mimirQueryError{phase: "response-body"}
	}
	samples := make([]*MimirQuerySample, 0, len(response.Data.Result))
	for _, series := range response.Data.Result {
		_, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, &mimirQueryError{phase: "response-body"}
		}
		labels := make(map[string]string, len(series.Metric))
		for name, labelValue := range series.Metric {
			labels[name] = labelValue
		}
		samples = append(samples, &MimirQuerySample{Labels: labels, Value: value})
	}
	return samples, nil
}
