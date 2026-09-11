package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
)

const mcpMetricsMaxInterval = time.Minute

var mcpReadyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "ready",
	Help:      "1 after MCP migration-aware readiness and warmup pass and while the instance is not draining.",
})

var mcpCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "calls_total",
	Help:      "MCP calls by finite protocol method, registered tool, and bounded terminal outcome.",
}, []string{"method", "tool", "outcome"})

var mcpCallSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "call_duration_seconds",
	Help:      "MCP call duration by finite protocol method and registered tool.",
	Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30},
}, []string{"method", "tool"})

var mcpCallsInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "calls_inflight",
	Help:      "MCP calls currently executing by finite protocol method and registered tool.",
}, []string{"method", "tool"})

var mcpToolBytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "tool_bytes_total",
	Help:      "Bytes already present in registered tool inputs and outputs, without serializing results solely for metrics.",
}, []string{"tool", "direction"})

var mcpToolItemsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "tool_items_total",
	Help:      "Bounded tool output items by registered tool, item class, and result class.",
}, []string{"tool", "item", "result"})

var mcpFetchResultsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "fetch_results_total",
	Help:      "Fetch calls by bounded outcome, HTTP status class, truncation, continuation, and payment state.",
}, []string{"outcome", "status_class", "truncated", "continuation", "payment"})

var mcpFetchConcurrency = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "fetch_concurrency",
	Help:      "Current fetch concurrency or fixed configured capacity, without caller identity labels.",
}, []string{"scope"})

var mcpFetchWaiters = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "fetch_waiters",
	Help:      "Fetch calls currently waiting at the per-identity or global concurrency gate.",
}, []string{"stage"})

var mcpFetchWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "fetch_wait_duration_seconds",
	Help:      "Time fetch calls spend at each concurrency gate, including immediate admissions.",
	Buckets:   []float64{0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 20},
}, []string{"stage"})

var mcpFetchAdmissionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "mcp",
	Name:      "fetch_admissions_total",
	Help:      "Fetch concurrency-gate outcomes by per-identity or global stage.",
}, []string{"stage", "outcome"})

// mcpCallMaximumSample is the max duration in one wall-clock minute and the
// exact observation time used to freshness-gate it.
type mcpCallMaximumSample struct {
	bucket     int64
	seconds    float64
	observedAt time.Time
}

// mcpRuntimeCollector owns coherent max/timestamp pairs and per-process
// privacy-preserving active-caller aggregates.
type mcpRuntimeCollector struct {
	maximumDesc          *prometheus.Desc
	maximumTimestampDesc *prometheus.Desc
	activeCallersDesc    *prometheus.Desc
	stateLock            sync.Mutex
	maximums             map[string]mcpCallMaximumSample
	callerLastSeen       map[[sha256.Size]byte]time.Time
	now                  func() time.Time
}

func newMcpRuntimeCollector() *mcpRuntimeCollector {
	return &mcpRuntimeCollector{
		maximumDesc: prometheus.NewDesc(
			"urnetwork_mcp_call_interval_max_seconds",
			"Maximum call duration in the latest one-minute interval that completed for a finite method/tool pair.",
			[]string{"method", "tool"}, nil,
		),
		maximumTimestampDesc: prometheus.NewDesc(
			"urnetwork_mcp_call_interval_max_timestamp_seconds",
			"Unix time of the call observation backing the latest one-minute MCP maximum.",
			[]string{"method", "tool"}, nil,
		),
		activeCallersDesc: prometheus.NewDesc(
			"urnetwork_mcp_active_callers",
			"Distinct authenticated subjects seen by this process in the rolling window; sum across replicas may duplicate callers.",
			[]string{"window"}, nil,
		),
		maximums:       map[string]mcpCallMaximumSample{},
		callerLastSeen: map[[sha256.Size]byte]time.Time{},
		now:            time.Now,
	}
}

var mcpRuntimeMetrics = newMcpRuntimeCollector()

func init() {
	prometheus.MustRegister(
		mcpReadyGauge,
		mcpCallsTotal,
		mcpCallSeconds,
		mcpCallsInflight,
		mcpToolBytesTotal,
		mcpToolItemsTotal,
		mcpFetchResultsTotal,
		mcpFetchConcurrency,
		mcpFetchWaiters,
		mcpFetchWaitSeconds,
		mcpFetchAdmissionsTotal,
		mcpRuntimeMetrics,
	)
	mcpFetchConcurrency.WithLabelValues("global_capacity").Set(fetchMaxConcurrentCalls)
	mcpFetchConcurrency.WithLabelValues("per_identity_capacity").Set(fetchMaxConcurrentCallsPerIdentity)
}

// Describe implements prometheus.Collector.
func (self *mcpRuntimeCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.maximumDesc
	descriptions <- self.maximumTimestampDesc
	descriptions <- self.activeCallersDesc
}

// Collect implements prometheus.Collector without exposing stored caller
// digests. A digest is used only to deduplicate one process's rolling count.
func (self *mcpRuntimeCollector) Collect(metrics chan<- prometheus.Metric) {
	now := self.now()
	self.stateLock.Lock()
	maximums := make(map[string]mcpCallMaximumSample, len(self.maximums))
	for key, sample := range self.maximums {
		maximums[key] = sample
	}
	active := map[string]int{"5m": 0, "1h": 0, "24h": 0}
	for caller, seenAt := range self.callerLastSeen {
		age := now.Sub(seenAt)
		if age > 24*time.Hour {
			delete(self.callerLastSeen, caller)
			continue
		}
		if age <= 5*time.Minute {
			active["5m"]++
		}
		if age <= time.Hour {
			active["1h"]++
		}
		active["24h"]++
	}
	self.stateLock.Unlock()

	for key, sample := range maximums {
		parts := strings.SplitN(key, "\x00", 2)
		metrics <- prometheus.MustNewConstMetric(self.maximumDesc, prometheus.GaugeValue, sample.seconds, parts[0], parts[1])
		metrics <- prometheus.MustNewConstMetric(
			self.maximumTimestampDesc,
			prometheus.GaugeValue,
			float64(sample.observedAt.UnixNano())/float64(time.Second),
			parts[0], parts[1],
		)
	}
	for _, window := range []string{"5m", "1h", "24h"} {
		metrics <- prometheus.MustNewConstMetric(self.activeCallersDesc, prometheus.GaugeValue, float64(active[window]), window)
	}
}

// observeCall records one bounded method/tool latency sample.
func (self *mcpRuntimeCollector) observeCall(method string, tool string, duration time.Duration) {
	now := self.now()
	bucket := now.UnixNano() / mcpMetricsMaxInterval.Nanoseconds()
	key := method + "\x00" + tool
	self.stateLock.Lock()
	current, ok := self.maximums[key]
	if !ok || current.bucket != bucket || current.seconds < duration.Seconds() {
		self.maximums[key] = mcpCallMaximumSample{bucket: bucket, seconds: duration.Seconds(), observedAt: now}
	}
	self.stateLock.Unlock()
}

// observeCaller retains only a one-way digest long enough to compute bounded
// process-local active counts.
func (self *mcpRuntimeCollector) observeCaller(subject string) {
	if subject == "" {
		return
	}
	digest := sha256.Sum256([]byte(subject))
	self.stateLock.Lock()
	self.callerLastSeen[digest] = self.now()
	self.stateLock.Unlock()
}

// boundedMcpTarget rejects caller-controlled method and tool names into fixed
// fallback labels.
func boundedMcpTarget(method string, request mcpsdk.Request) (string, string) {
	switch method {
	case "initialize", "ping", "tools/list", "tools/call", "resources/list", "resources/read", "prompts/list", "prompts/get", "completion/complete", "logging/setLevel":
	default:
		method = "other"
	}
	tool := "none"
	if callParams, ok := request.GetParams().(*mcpsdk.CallToolParamsRaw); ok && callParams != nil {
		switch callParams.Name {
		case "providerLocations", "fetch":
			tool = callParams.Name
		default:
			tool = "unknown"
		}
	}
	return method, tool
}

// mcpCallOutcome maps method returns to a finite operational taxonomy.
func mcpCallOutcome(ctx context.Context, result mcpsdk.Result, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "canceled"
	}
	if err != nil {
		return "error"
	}
	if toolResult, ok := result.(*mcpsdk.CallToolResult); ok && toolResult.IsError {
		return "tool_error"
	}
	return "succeeded"
}

// createMetricsMiddleware measures protocol work without raw methods, tools,
// identities, URLs, payloads, or error text as labels.
func createMetricsMiddleware() mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, rawMethod string, request mcpsdk.Request) (result mcpsdk.Result, err error) {
			method, tool := boundedMcpTarget(rawMethod, request)
			if tokenInfo := auth.TokenInfoFromContext(ctx); tokenInfo != nil {
				mcpRuntimeMetrics.observeCaller(tokenInfo.UserID)
			}
			start := time.Now()
			mcpCallsInflight.WithLabelValues(method, tool).Inc()
			defer func() {
				mcpCallsInflight.WithLabelValues(method, tool).Dec()
				duration := time.Since(start)
				outcome := mcpCallOutcome(ctx, result, err)
				if recovered := recover(); recovered != nil {
					outcome = "panic"
					mcpCallsTotal.WithLabelValues(method, tool, outcome).Inc()
					mcpCallSeconds.WithLabelValues(method, tool).Observe(duration.Seconds())
					mcpRuntimeMetrics.observeCall(method, tool, duration)
					panic(recovered)
				}
				mcpCallsTotal.WithLabelValues(method, tool, outcome).Inc()
				mcpCallSeconds.WithLabelValues(method, tool).Observe(duration.Seconds())
				mcpRuntimeMetrics.observeCall(method, tool, duration)
			}()
			return next(ctx, rawMethod, request)
		}
	}
}

// mcpStatusClass maps an HTTP result into a bounded class.
func mcpStatusClass(status int) string {
	if status < 100 || 599 < status {
		return "none"
	}
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	default:
		return "5xx"
	}
}

// boolMetricLabel returns one of the two bounded boolean label values.
func boolMetricLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// fetchInputBytes counts string bytes already present in a decoded request;
// it never retains or labels the request values.
func fetchInputBytes(args FetchArgs) int {
	total := len(args.Url) + len(args.Method) + len(args.Location) + len(args.SignedProxyId) + len(args.Cookies) +
		len(args.Continuation) + len(args.Body) + len(args.IncludeResources) + len(args.Payment)
	for name, value := range args.Headers {
		total += len(name) + len(value)
	}
	return total
}

// mcpContentBytes counts payload bytes in content values the handler already
// created. Resource-link metadata has no response body and is not serialized
// solely to estimate its encoded size.
func mcpContentBytes(contents []mcpsdk.Content) int {
	total := 0
	for _, content := range contents {
		switch value := content.(type) {
		case *mcpsdk.TextContent:
			total += len(value.Text)
		case *mcpsdk.ImageContent:
			total += len(value.Data)
		case *mcpsdk.AudioContent:
			total += len(value.Data)
		case *mcpsdk.EmbeddedResource:
			if value.Resource != nil {
				total += len(value.Resource.Text) + len(value.Resource.Blob)
			}
		}
	}
	return total
}
