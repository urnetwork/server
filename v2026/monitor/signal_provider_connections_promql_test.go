package monitor

// Exact owning PromQL runs against raw synthetic series in the pinned local
// Mimir engine. Native vectors then enter the public adapter; no tagged totals
// are fabricated by these fixtures.
import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type providerConnectionsEnginePoint struct {
	Seconds int64   `json:"seconds"`
	Value   float64 `json:"value"`
}
type providerConnectionsEngineSeries struct {
	Labels map[string]string                `json:"labels"`
	Points []providerConnectionsEnginePoint `json:"points"`
}
type providerConnectionsEngineCase struct {
	Name               string                            `json:"name"`
	Evaluation         int64                             `json:"evaluation"`
	Series             []providerConnectionsEngineSeries `json:"series"`
	Group              string                            `json:"-"`
	Step               int                               `json:"-"`
	Native             []providerConnectionsTestProcess  `json:"-"`
	WantPage           bool                              `json:"-"`
	WantUnknown        bool                              `json:"-"`
	WantBaselinePoints int                               `json:"-"`
	WantLevel          float64                           `json:"-"`
}

// Source histories intentionally include retained stale values without stale
// markers, matching the native push receiver rather than assuming scrape zeros.
func providerConnectionsEngineCases() []providerConnectionsEngineCase {
	const base int64 = 100800
	cases := []providerConnectionsEngineCase{}
	groups := []string{"healthy", "missing-live-sibling", "stale-live-sibling", "collapse", "foreign-env", "foreign-job", "foreign-service", "foreign-host", "foreign-block", "live-overlap", "retired-overlap", "deleted-bucket", "multiple-release-buckets", "fractional-bucket", "duplicate-bucket", "missing-start", "missing-rss", "empty-live", "empty-native"}
	for _, name := range groups {
		series := []providerConnectionsEngineSeries{}
		processes := []providerConnectionsTestProcess{}
		addProcess := func(host, instance string, start int64, extra map[string]string, count func(int64) (float64, bool)) {
			for _, field := range []string{"start", "rss", "transports"} {
				metric := map[string]string{"start": "process_start_time_seconds", "rss": "process_resident_memory_bytes", "transports": "urnetwork_connect_provider_connections"}[field]
				labels := map[string]string{"__name__": metric, "env": "synthetic", "job": "connect", "service": "connect", "host": host, "block": providerConnectionsTestBlock, "instance": instance}
				if field == "transports" {
					labels["app_version"] = "2026.10.1"
				}
				for k, v := range extra {
					labels[k] = v
				}
				points := []providerConnectionsEnginePoint{}
				for at := base - 7200; at <= base+300; at += 15 {
					value, present := count(at)
					if !present {
						continue
					}
					if at >= base-600 && host == providerConnectionsTestHosts[1] {
						if name == "missing-live-sibling" || name == "empty-native" {
							continue
						}
						if name == "stale-live-sibling" && at > base-240 {
							continue
						}
					}
					if at >= base-600 {
						if name == "missing-start" && field == "start" {
							continue
						}
						if name == "missing-rss" && field == "rss" {
							continue
						}
						if name == "empty-live" && field == "transports" {
							continue
						}
						if name == "empty-native" {
							continue
						}
					}
					if field == "start" {
						value = float64(start)
					}
					if field == "rss" {
						value = 1000000
					}
					if field == "transports" && name == "fractional-bucket" && at >= base-600 {
						value += 0.5
					}
					points = append(points, providerConnectionsEnginePoint{Seconds: at, Value: value})
				}
				series = append(series, providerConnectionsEngineSeries{Labels: labels, Points: points})
			}
		}
		count := func(at int64) (float64, bool) {
			if at < base-600 || name == "healthy" {
				return 40000, true
			}
			return 1000, true
		}
		for i, host := range providerConnectionsTestHosts {
			instance := fmt.Sprintf("aaaabb%02d", i)
			addProcess(host, instance, base-14400, nil, count)
			if name != "empty-native" {
				processes = append(processes, providerConnectionsTestProcess{host: host, instance: instance, start: float64(base - 14400), transports: 1000})
			}
		}
		if strings.HasPrefix(name, "foreign-") {
			extra := map[string]string{}
			host := providerConnectionsTestHosts[0]
			switch name {
			case "foreign-env":
				extra["env"] = "other"
			case "foreign-job":
				extra["job"] = "proxy"
			case "foreign-service":
				extra["service"] = "proxy"
			case "foreign-host":
				host = "private-unconfigured-host"
			case "foreign-block":
				extra["block"] = "private-unconfigured-block"
			}
			addProcess(host, "foreign0", base-14400, extra, func(int64) (float64, bool) { return 90000, true })
		}
		if name == "live-overlap" || name == "retired-overlap" {
			addProcess(providerConnectionsTestHosts[0], "oldold00", base-18000, nil, func(at int64) (float64, bool) {
				return 40000, at >= base-600 && (name == "live-overlap" || at <= base-240)
			})
			if name == "live-overlap" {
				processes = append(processes, providerConnectionsTestProcess{host: providerConnectionsTestHosts[0], instance: "oldold00", start: float64(base - 18000), transports: 40000})
			}
		}
		if name == "multiple-release-buckets" {
			labels := map[string]string{"__name__": "urnetwork_connect_provider_connections", "env": "synthetic", "job": "connect", "service": "connect", "host": providerConnectionsTestHosts[0], "block": providerConnectionsTestBlock, "instance": "aaaabb00", "app_version": "unknown"}
			points := []providerConnectionsEnginePoint{}
			for at := base - 600; at <= base+300; at += 15 {
				points = append(points, providerConnectionsEnginePoint{Seconds: at, Value: 1000})
			}
			series = append(series, providerConnectionsEngineSeries{Labels: labels, Points: points})
		}
		if name == "deleted-bucket" {
			labels := map[string]string{"__name__": "urnetwork_connect_provider_connections", "env": "synthetic", "job": "connect", "service": "connect", "host": providerConnectionsTestHosts[0], "block": providerConnectionsTestBlock, "instance": "aaaabb00", "app_version": "2026.9"}
			series = append(series, providerConnectionsEngineSeries{Labels: labels, Points: []providerConnectionsEnginePoint{{Seconds: base - 30, Value: 100000}}})
		}
		if name == "duplicate-bucket" {
			labels := map[string]string{"__name__": "urnetwork_connect_provider_connections", "env": "synthetic", "job": "connect", "service": "connect", "host": providerConnectionsTestHosts[0], "block": providerConnectionsTestBlock, "instance": "aaaabb00", "app_version": "2026.10.1", "duplicate": "private-extra"}
			points := []providerConnectionsEnginePoint{}
			for at := base - 600; at <= base+300; at += 15 {
				points = append(points, providerConnectionsEnginePoint{Seconds: at, Value: 1000})
			}
			series = append(series, providerConnectionsEngineSeries{Labels: labels, Points: points})
		}
		wantPage := name == "collapse" || strings.HasPrefix(name, "foreign-") || name == "retired-overlap" || name == "deleted-bucket" || name == "multiple-release-buckets" || name == "empty-native"
		unknown := name != "healthy" && name != "live-overlap"
		for step := 0; step < 6; step++ {
			cases = append(cases, providerConnectionsEngineCase{Name: fmt.Sprintf("%s/%d", name, step), Evaluation: base + int64(step*60), Series: series, Group: name, Step: step, Native: processes, WantPage: wantPage && step == 5, WantUnknown: unknown && !(wantPage && step == 5)})
		}
	}
	// Twelve isolated resampled points span 55 minutes. Preserve the existing
	// point threshold and test it honestly without calling it an hour.
	for _, points := range []int{11, 12, 13} {
		series := []providerConnectionsEngineSeries{}
		for i, host := range providerConnectionsTestHosts {
			for _, field := range []string{"start", "rss", "transports"} {
				metric := map[string]string{"start": "process_start_time_seconds", "rss": "process_resident_memory_bytes", "transports": "urnetwork_connect_provider_connections"}[field]
				labels := map[string]string{"__name__": metric, "env": "synthetic", "job": "connect", "service": "connect", "host": host, "block": providerConnectionsTestBlock, "instance": fmt.Sprintf("aaaabb%02d", i)}
				if field == "transports" {
					labels["app_version"] = "2026.10.1"
				}
				samples := []providerConnectionsEnginePoint{}
				for n := 0; n < points; n++ {
					value := 1000.0
					if i == 0 {
						value += float64(n)
					}
					if field == "start" {
						value = float64(base - 14400)
					}
					if field == "rss" {
						value = 1000000
					}
					samples = append(samples, providerConnectionsEnginePoint{Seconds: base - int64((points-1-n)*300), Value: value})
				}
				series = append(series, providerConnectionsEngineSeries{Labels: labels, Points: samples})
			}
		}
		name := fmt.Sprintf("baseline-%d", points)
		cases = append(cases, providerConnectionsEngineCase{Name: name, Group: name, Evaluation: base, Series: series, WantUnknown: true, WantBaselinePoints: points, WantLevel: 2000 + 0.9*float64(points-1)})
	}
	return cases
}

// Capturing the public command makes the same test compile over the old source
// for a causal RED: it runs that source's actual env-only query, too.
func providerConnectionsEngineQuery(t *testing.T) string {
	t.Helper()
	captured := ""
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		captured = command
		return providerConnectionsTestJson(t, providerConnectionsTestRows(providerConnectionsTestNow, providerConnectionsTestProcesses(40000), 80000, 80000), nil), nil
	}}
	_, _ = providerConnectionsSignalForTest(t).Run(context.Background(), providerConnectionsTestSettings(source, func() time.Time { return providerConnectionsTestNow }))
	if _, rest, ok := strings.Cut(captured, "--data-urlencode 'query="); ok {
		query, _, ok := strings.Cut(rest, "' --data-urlencode ")
		if !ok {
			t.Fatal("query framing")
		}
		return query
	}
	if _, rest, ok := strings.Cut(captured, "?query="); ok {
		encoded, _, _ := strings.Cut(rest, "'")
		query, err := url.QueryUnescape(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return query
	}
	t.Fatal("owning public query was not captured")
	return ""
}

func TestProviderConnectionsPromqlEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("required provider qualification needs the pinned local MONITOR_PROMQL_ENGINE_SOURCE; a skip is not a semantic PASS")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	query, cases := providerConnectionsEngineQuery(t), providerConnectionsEngineCases()
	request, err := json.Marshal(struct {
		Query string                          `json:"query"`
		Cases []providerConnectionsEngineCase `json:"cases"`
	}{Query: query, Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	requestPath, outputPath := filepath.Join(directory, "request.json"), filepath.Join(directory, "vectors.json")
	if err := os.WriteFile(requestPath, request, 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/provider_connections_promql_engine_test.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	engineTest := filepath.Join(directory, "provider_connections_promql_engine_test.go")
	if err := os.WriteFile(engineTest, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestProviderConnectionsNativeVectors$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "MONITOR_PROVIDER_ENGINE_REQUEST="+requestPath, "MONITOR_PROVIDER_ENGINE_OUTPUT="+outputPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("provider native-engine qualification failed: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	vectors := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(cases) {
		t.Fatal("native engine omitted cases")
	}
	signals := map[string]Signal{}
	for _, c := range cases {
		payload, ok := vectors[c.Name]
		if !ok {
			t.Fatalf("missing native case %s", c.Name)
		}
		var response mimirInstantResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			t.Fatal(err)
		}
		values := map[string]float64{}
		for _, series := range response.Data.Result {
			field := series.Metric["monitor_metric"]
			if field == "p90_24h" || field == "points_24h" || field == "best_5m" {
				_, value, err := mimirInstantValue(series.Value)
				if err != nil {
					t.Fatal(err)
				}
				values[field] = value
			}
		}
		if c.WantBaselinePoints > 0 && (values["points_24h"] != float64(c.WantBaselinePoints) || math.Abs(values["p90_24h"]-c.WantLevel) > 0.00001) {
			t.Fatalf("%s: actual baseline points/quantile=%v", c.Name, values)
		}
		if strings.HasPrefix(c.Group, "foreign-") && (values["p90_24h"] != 80000 || values["best_5m"] != 2000) {
			t.Fatalf("%s: foreign publisher altered the exact query totals", c.Name)
		}
		if c.Group == "multiple-release-buckets" && values["best_5m"] != 3000 {
			t.Fatal("simultaneous release buckets did not sum within the process")
		}
		if c.Group == "deleted-bucket" && c.Step == 0 && values["best_5m"] != 2000 {
			t.Fatal("a deleted release bucket survived the same-push query join")
		}
		signal := signals[c.Group]
		if signal == nil {
			signal = providerConnectionsSignalForTest(t)
			signals[c.Group] = signal
		}
		now := time.Unix(c.Evaluation, 0).UTC()
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor-provider-connections-native") {
				return providerConnectionsTestNative(t, now, host.Name, c.Native), nil
			}
			return string(payload), nil
		}}
		alerts, err := signal.Run(context.Background(), providerConnectionsTestSettings(source, func() time.Time { return now }))
		if err != nil {
			t.Fatalf("%s: public adapter error", c.Name)
		}
		page := requireAlertClassCount(alerts, "provider-connections-collapse") > 0
		unknown := requireAlertClassCount(alerts, "provider-connections-unobservable") > 0
		wantUnknown := c.WantUnknown
		if c.Group == "retired-overlap" && c.Step < 5 {
			wantUnknown = values["best_5m"] < 40000
		}
		if page != c.WantPage || unknown != wantUnknown {
			t.Fatalf("%s: page=%t unknown=%t, want page=%t unknown=%t", c.Name, page, unknown, c.WantPage, wantUnknown)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, providerConnectionsPrivate, "foreign0", "oldold00", "private-extra")
		}
	}
}
