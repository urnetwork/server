package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestBoundedMcpTargetNeverExportsCallerMethodOrTool(t *testing.T) {
	request := &mcpsdk.ServerRequest[*mcpsdk.CallToolParamsRaw]{
		Params: &mcpsdk.CallToolParamsRaw{Name: "caller-controlled-tool"},
	}
	method, tool := boundedMcpTarget("caller/controlled", request)
	if method != "other" || tool != "unknown" {
		t.Fatalf("bounded target = %q/%q, want other/unknown", method, tool)
	}

	request.Params.Name = "fetch"
	method, tool = boundedMcpTarget("tools/call", request)
	if method != "tools/call" || tool != "fetch" {
		t.Fatalf("registered target = %q/%q, want tools/call/fetch", method, tool)
	}
}

func TestMcpRuntimeCollectorReplacesMaximumAcrossIntervals(t *testing.T) {
	collector := newMcpRuntimeCollector()
	now := time.Unix(1_800_000_000, 0)
	collector.now = func() time.Time { return now }
	collector.observeCall("tools/call", "fetch", 2*time.Second)
	collector.observeCall("tools/call", "fetch", time.Second)

	key := "tools/call\x00fetch"
	if got := collector.maximums[key].seconds; got != 2 {
		t.Fatalf("same-interval maximum = %v, want 2", got)
	}

	now = now.Add(mcpMetricsMaxInterval)
	collector.observeCall("tools/call", "fetch", 500*time.Millisecond)
	if got := collector.maximums[key].seconds; got != 0.5 {
		t.Fatalf("next-interval maximum = %v, want 0.5", got)
	}
}

func TestMcpDurationSummariesExportExactSumCountWithoutBuckets(t *testing.T) {
	tests := []struct {
		name       string
		metricName string
		newSummary func() *prometheus.SummaryVec
		labels     []string
		want       string
	}{
		{
			name:       "call",
			metricName: "urnetwork_mcp_call_duration_seconds",
			newSummary: newMcpCallSeconds,
			labels:     []string{"tools/call", "fetch"},
			want: `# HELP urnetwork_mcp_call_duration_seconds MCP call duration by finite protocol method and registered tool.
# TYPE urnetwork_mcp_call_duration_seconds summary
urnetwork_mcp_call_duration_seconds_sum{method="tools/call",tool="fetch"} 3.25
urnetwork_mcp_call_duration_seconds_count{method="tools/call",tool="fetch"} 2
`,
		},
		{
			name:       "fetch wait",
			metricName: "urnetwork_mcp_fetch_wait_duration_seconds",
			newSummary: newMcpFetchWaitSeconds,
			labels:     []string{"global"},
			want: `# HELP urnetwork_mcp_fetch_wait_duration_seconds Time fetch calls spend at each concurrency gate, including immediate admissions.
# TYPE urnetwork_mcp_fetch_wait_duration_seconds summary
urnetwork_mcp_fetch_wait_duration_seconds_sum{stage="global"} 3.25
urnetwork_mcp_fetch_wait_duration_seconds_count{stage="global"} 2
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := prometheus.NewPedanticRegistry()
			duration := test.newSummary()
			registry.MustRegister(duration)
			duration.WithLabelValues(test.labels...).Observe(2.25)
			duration.WithLabelValues(test.labels...).Observe(1)

			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			if len(families) != 1 || families[0].GetName() != test.metricName ||
				families[0].GetType() != dto.MetricType_SUMMARY || len(families[0].Metric) != 1 {
				t.Fatalf("%s duration families = %+v, want one summary", test.name, families)
			}
			summary := families[0].Metric[0].GetSummary()
			if summary.GetSampleCount() != 2 || summary.GetSampleSum() != 3.25 || len(summary.Quantile) != 0 {
				t.Fatalf("%s duration summary = %+v, want count=2 sum=3.25 and no quantiles", test.name, summary)
			}
			if err := testutil.GatherAndCompare(registry, strings.NewReader(test.want), test.metricName); err != nil {
				t.Fatalf("%s duration exposition contains more than exact sum/count: %v", test.name, err)
			}
		})
	}
}

func TestMcpRuntimeCollectorKeepsOnlyCallerDigests(t *testing.T) {
	collector := newMcpRuntimeCollector()
	now := time.Unix(1_800_000_000, 0)
	collector.now = func() time.Time { return now }
	collector.observeCaller("synthetic-caller")

	wantDigestCount := 1
	if len(collector.callerLastSeen) != wantDigestCount {
		t.Fatalf("caller digest count = %d, want %d", len(collector.callerLastSeen), wantDigestCount)
	}
	now = now.Add(25 * time.Hour)
	metricChannel := make(chan prometheus.Metric, 8)
	go func() {
		collector.Collect(metricChannel)
		close(metricChannel)
	}()
	for range metricChannel {
	}
	if len(collector.callerLastSeen) != 0 {
		t.Fatalf("expired caller digests retained = %d, want 0", len(collector.callerLastSeen))
	}
}

func TestFetchMetricsCountExistingPayloadsAndBoundedResults(t *testing.T) {
	args := FetchArgs{
		Url:              "https://fixture.example/resource",
		Method:           "POST",
		Location:         "synthetic-location",
		Headers:          map[string]string{"X-Fixture": "value"},
		Body:             "payload",
		IncludeResources: includeResourcesEmbed,
		Continuation:     "opaque-continuation",
		MaxResources:     2,
		MaxResourceBytes: 128,
		SignedProxyId:    "opaque-proxy",
		Cookies:          "opaque-cookie",
		Payment:          "opaque-payment",
	}
	wantInputBytes := 0
	for _, value := range []string{
		args.Url, args.Method, args.Location, "X-Fixture", "value", args.Body,
		args.IncludeResources, args.Continuation, args.SignedProxyId, args.Cookies, args.Payment,
	} {
		wantInputBytes += len(value)
	}
	if got := fetchInputBytes(args); got != wantInputBytes {
		t.Fatalf("fetch input bytes = %d, want %d", got, wantInputBytes)
	}

	contents := []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "text"},
		&mcpsdk.ImageContent{Data: []byte{1, 2}},
		&mcpsdk.AudioContent{Data: []byte{3, 4, 5}},
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{Text: "body", Blob: []byte{6}}},
		&mcpsdk.ResourceLink{URI: "https://fixture.example/not-counted"},
	}
	if got := mcpContentBytes(contents); got != 14 {
		t.Fatalf("MCP content bytes = %d, want 14", got)
	}

	if got := mcpStatusClass(429); got != "4xx" {
		t.Fatalf("status class = %q, want 4xx", got)
	}
	if got := mcpCallOutcome(context.Background(), &mcpsdk.CallToolResult{IsError: true}, nil); got != "tool_error" {
		t.Fatalf("tool outcome = %q, want tool_error", got)
	}
}

func TestFetchConcurrencyMetricsRecordDeterministicCancellation(t *testing.T) {
	limiter := newFetchConcurrencyLimiter()
	admittedBefore := testutil.ToFloat64(mcpFetchAdmissionsTotal.WithLabelValues("per_identity", "admitted"))
	canceledBefore := testutil.ToFloat64(mcpFetchAdmissionsTotal.WithLabelValues("per_identity", "canceled"))

	releaseOne, err := limiter.acquire(context.Background(), "synthetic-caller")
	if err != nil {
		t.Fatal(err)
	}
	releaseTwo, err := limiter.acquire(context.Background(), "synthetic-caller")
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := limiter.acquire(waitCtx, "synthetic-caller"); err == nil {
		t.Fatal("saturated per-identity admission did not honor cancellation")
	}
	releaseOne()
	releaseTwo()

	if got := testutil.ToFloat64(mcpFetchAdmissionsTotal.WithLabelValues("per_identity", "admitted")) - admittedBefore; got != 2 {
		t.Fatalf("per-identity admitted delta = %v, want 2", got)
	}
	if got := testutil.ToFloat64(mcpFetchAdmissionsTotal.WithLabelValues("per_identity", "canceled")) - canceledBefore; got != 1 {
		t.Fatalf("per-identity canceled delta = %v, want 1", got)
	}
	if len(limiter.identities) != 0 || len(limiter.globalSemaphore) != 0 {
		t.Fatalf("limiter retained identities/global slots = %d/%d", len(limiter.identities), len(limiter.globalSemaphore))
	}
}
