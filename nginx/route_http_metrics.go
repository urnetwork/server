package nginx

// What the front records of its requests (owner: s-http; D17): bounded
// counters and response-head latency histograms by service, protocol,
// status class and upstream block, a counter of limit refusals by limit, the
// earliest expiry of the certificates it serves, and an optional access log
// at V(1), sampled, that honors no_access_log and no_success_access_log.
//
// The service and block labels are upstream_service and upstream_block: the
// process's stats push adds its own service and block to every series, and
// the push client refuses a whole push, every metric of the process, when a
// metric has a label of a name it adds (server.StatsPushReservedLabelNames).
//
// No label or log line carries a client address or a hidden route prefix:
// labels come from the route table or fixed sets, never from a request, and
// the access log replaces every hidden prefix it finds in what the client
// sent and never writes the peer, X-UR-Forwarded-For or the query.

import (
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
)

// The label value for a request the front answered without a service or a
// block.
const routeHttpNoLabel = "-"

// The front's metric families. Safe for concurrent use.
type frontMetrics struct {
	requests *prometheus.CounterVec
	// from the request to its response head, or to the hijack of an
	// upgrade, so a stream or a tunnel doesn't skew it
	responseHeaderSeconds *prometheus.HistogramVec
	limitRefusals         *prometheus.CounterVec
	// the earliest NotAfter of the certificates the front serves
	// (CertificateStore.EarliestExpiry), set when NewFront builds it. A
	// vector without labels has no value until then, so a process that
	// builds no front, as connect without a route file, exports none: a
	// gauge's 0 would read as a certificate that lapsed in 1970
	certificateExpiry *prometheus.GaugeVec
}

// Builds and registers one independent set of the front's metric families.
func newFrontMetrics(registerer prometheus.Registerer) *frontMetrics {
	labels := []string{"upstream_service", "protocol", "status_class", "upstream_block"}
	metrics := &frontMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "front",
			Name:      "requests_total",
			Help:      "Requests the lb_alt front answered, by the service they are for (- for the lb's own routes and unknown names), protocol, status class and upstream block (local for connect in-process, - for none)",
		}, labels),
		responseHeaderSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "urnetwork",
			Subsystem: "front",
			Name:      "response_header_seconds",
			Help:      "Time from a request reaching the lb_alt front to its response head, or to the hijack of an upgrade, by the requests_total labels",
			// 1 ms to 32 s
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
		}, labels),
		limitRefusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "front",
			Name:      "limit_refusals_total",
			Help:      "Requests and QUIC connections the lb_alt front refused under its D8 limits, by limit: request, route, concurrency, quic, state, or interface for a QUIC connection on an interface the route table doesn't list",
		}, []string{"limit"}),
		certificateExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "front",
			Name:      "certificate_expiry_timestamp_seconds",
			Help:      "The earliest NotAfter of the certificates the lb_alt front serves, as a Unix time; in the past when a served name's certificate has lapsed, which the front serves all the same, as nginx does. Absent from a process that builds no front",
		}, nil),
	}
	registerer.MustRegister(
		metrics.requests,
		metrics.responseHeaderSeconds,
		metrics.limitRefusals,
		metrics.certificateExpiry,
	)
	return metrics
}

// The process's front metrics, registered once, as the router's are.
var defaultFrontMetrics = newFrontMetrics(prometheus.DefaultRegisterer)

// Where the sampled access log goes.
type frontAccessLog interface {
	// whether lines are wanted: checked before any work is done for one
	enabled() bool
	write(line string)
}

// The access log at glog's V(1).
type glogFrontAccessLog struct{}

// See frontAccessLog.
func (self *glogFrontAccessLog) enabled() bool {
	return bool(glog.V(1))
}

// See frontAccessLog.
func (self *glogFrontAccessLog) write(line string) {
	glog.Infof("[front]%s\n", line)
}

// The status class label of a response: 1xx to 5xx, "none" when no head
// was written, as when the handler panicked.
func routeHttpStatusClass(statusCode int) string {
	switch {
	case 100 <= statusCode && statusCode < 600:
		return fmt.Sprintf("%dxx", statusCode/100)
	case statusCode == 0:
		return "none"
	default:
		return "other"
	}
}

// The protocol label of a request.
func routeHttpProtocol(protoMajor int) string {
	switch protoMajor {
	case 1:
		return "h1"
	case 2:
		return "h2"
	case 3:
		return "h3"
	default:
		return "other"
	}
}

// Records one ended request: its metrics, and its access log line when one
// is due.
func (self *Front) observe(exchange *routeHttpExchange) {
	response := exchange.response
	service := routeHttpNoLabel
	block := routeHttpNoLabel
	if routeService, ok := self.routes.services[exchange.serviceName]; ok {
		service = routeService.service.Name
		// only a block of the service, or connect in-process, is a label
		switch {
		case response.upstreamBlock == routeHttpInProcessBlock:
			block = routeHttpInProcessBlock
		case routeService.blockNames[response.upstreamBlock]:
			block = response.upstreamBlock
		}
	}
	protocol := routeHttpProtocol(exchange.r.ProtoMajor)
	statusClass := routeHttpStatusClass(response.statusCode)
	self.metrics.requests.WithLabelValues(service, protocol, statusClass, block).Inc()
	if !response.headerTime.IsZero() {
		self.metrics.responseHeaderSeconds.WithLabelValues(service, protocol, statusClass, block).Observe(
			response.headerTime.Sub(exchange.startTime).Seconds(),
		)
	}

	if !self.accessLog.enabled() {
		return
	}
	request := exchange.request
	host := exchange.r.Host
	path := routeHttpNoLabel
	if request != nil {
		host = request.host
		path = request.uri.path
	} else if normalizedHost, ok := self.names.normalizeHost(host); ok {
		// a refused request still names its host, as nginx's $host does
		host = normalizedHost
	}
	if self.routes.noAccessLogHosts[host] {
		// a privacy promise: no line at all
		return
	}
	if request != nil {
		if routeService, ok := self.routes.services[exchange.serviceName]; ok &&
			200 <= response.statusCode && response.statusCode < 300 &&
			routeService.exactNames[request.host] &&
			routeService.noSuccessAccessLogPaths[request.uri.path] {
			return
		}
	}
	accessLogSampleInterval := self.settings.Http.AccessLogSampleInterval
	if accessLogSampleInterval <= 0 || self.accessLogCount.Add(1)%uint64(accessLogSampleInterval) != 0 {
		return
	}
	seconds := self.now().Sub(exchange.startTime).Seconds()
	self.accessLog.write(fmt.Sprintf(
		"access %s %q %q %d %d %.3f %s %s %q",
		exchange.frontInterface.limits.name,
		self.redact(host),
		self.redact(exchange.r.Method+" "+path+" "+exchange.r.Proto),
		response.statusCode,
		response.bodyBytes,
		seconds,
		service,
		block,
		self.redact(exchange.r.Header.Get("User-Agent")),
	))
}

// Replaces every hidden route prefix in a client-sent string, so no log
// line holds one.
func (self *Front) redact(value string) string {
	for _, secretPrefix := range self.routes.secretPrefixes {
		value = strings.ReplaceAll(value, secretPrefix, "<prefix>")
	}
	return value
}

// The time the front reads, for tests that drive it.
func (self *Front) now() time.Time {
	return self.clock()
}
