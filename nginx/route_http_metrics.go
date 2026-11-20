package nginx

// What the front records of its requests (owner: s-http; D17): bounded
// counters and response-head latency histograms by service, protocol,
// status class and upstream block, a counter of limit refusals by limit,
// zone and the class of the name, the earliest expiry of the certificates it
// serves, a counter of the
// QUIC ClientHellos its QUIC TLS config reads, by port kind, the class of the
// name and whether h3 was offered, and an optional access log at V(1),
// sampled, that honors no_access_log and no_success_access_log.
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
//
// A request's metrics cost no allocation and no lookup by labels: the label
// values are constants, and each label set's series are resolved once and
// kept per service and block (frontRequestMetrics).

import (
	"fmt"
	"strings"
	"sync/atomic"
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
	// every ClientHello the QUIC TLS config reads (newTlsConfigs)
	quicClientHellos *prometheus.CounterVec
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
			Help:      "Requests and QUIC connections the lb_alt front refused under its D8 limits, by limit (request, route, concurrency, quic, state; identity, identity_concurrency, ceiling and ceiling_concurrency for the identity limit; interface for a QUIC connection on an interface the route table doesn't list, class for one of a class the front doesn't serve on QUIC), zone: the request zone the request was charged to, whichever limit refused it, nginx's route_<service>_<path> for a route zone from the route file, identity for a request charged to its verified identity, or default for the interface's shared zone, which a refused QUIC connection is counted in too, - for interface and class; and name_class, the class of the name the request or connection named (connect, api, lb, forwarded, redirect, unknown): a QUIC connection's is api or connect, each class with a cap of its own",
		}, []string{"limit", "zone", "name_class"}),
		certificateExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "front",
			Name:      "certificate_expiry_timestamp_seconds",
			Help:      "The earliest NotAfter of the certificates the lb_alt front serves, as a Unix time; in the past when a served name's certificate has lapsed, which the front serves all the same, as nginx does. Absent from a process that builds no front",
		}, nil),
		quicClientHellos: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "front",
			Name:      "quic_client_hellos_total",
			Help:      "QUIC ClientHellos the lb_alt front read, by port kind (quic, whodis, other), the class of the server name (connect, api, lb, forwarded, redirect for a served name; alt for alt, alt-v4 or alt-v6 on any domain, unknown for another name and none for a missing or invalid one, which the front doesn't serve on QUIC, so their handshakes fail) and whether the client offered h3 (true, false). The name itself is never a label",
		}, []string{"port", "name_class", "h3_offered"}),
	}
	registerer.MustRegister(
		metrics.requests,
		metrics.responseHeaderSeconds,
		metrics.limitRefusals,
		metrics.certificateExpiry,
		metrics.quicClientHellos,
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

// The status class labels, by routeHttpStatusClassIndex: "none" when no
// head was written, as when the handler panicked, 1xx to 5xx, and "other"
// for a status outside them.
var routeHttpStatusClasses = [...]string{"none", "1xx", "2xx", "3xx", "4xx", "5xx", "other"}

// The index of a response's status class label in routeHttpStatusClasses.
func routeHttpStatusClassIndex(statusCode int) int {
	switch {
	case statusCode == 0:
		return 0
	case 100 <= statusCode && statusCode < 600:
		return statusCode / 100
	default:
		return len(routeHttpStatusClasses) - 1
	}
}

// The protocol labels, by routeHttpProtocolIndex: "other", then h1, h2 and
// h3 at their major versions.
var routeHttpProtocols = [...]string{"other", "h1", "h2", "h3"}

// The index of a request's protocol label in routeHttpProtocols.
func routeHttpProtocolIndex(protoMajor int) int {
	if 1 <= protoMajor && protoMajor < len(routeHttpProtocols) {
		return protoMajor
	}
	return 0
}

// The requests counter's and the latency histogram's series of one label
// set.
type frontRequestSeries struct {
	requests              prometheus.Counter
	responseHeaderSeconds prometheus.Observer
}

// The request metrics of one service and block label: the series of each
// protocol and status class, each resolved by its labels on its first
// request and kept, so a request's metrics take no lookup by labels. The
// labels come from the route table and the closed sets above, so the series
// are as bounded as before. Safe for concurrent use.
type frontRequestMetrics struct {
	metrics *frontMetrics
	service string
	block   string
	series  [len(routeHttpProtocols)][len(routeHttpStatusClasses)]atomic.Pointer[frontRequestSeries]
}

// Request metrics for a service and block label of metrics, with nothing
// resolved yet.
func newFrontRequestMetrics(metrics *frontMetrics, service string, block string) *frontRequestMetrics {
	return &frontRequestMetrics{
		metrics: metrics,
		service: service,
		block:   block,
	}
}

// The series of a protocol and status class index. Requests that race to
// resolve one get the same series: the vectors keep one per label set, and
// the first stored is kept.
func (self *frontRequestMetrics) requestSeries(protocolIndex int, statusClassIndex int) *frontRequestSeries {
	slot := &self.series[protocolIndex][statusClassIndex]
	if series := slot.Load(); series != nil {
		return series
	}
	protocol := routeHttpProtocols[protocolIndex]
	statusClass := routeHttpStatusClasses[statusClassIndex]
	series := &frontRequestSeries{
		requests:              self.metrics.requests.WithLabelValues(self.service, protocol, statusClass, self.block),
		responseHeaderSeconds: self.metrics.responseHeaderSeconds.WithLabelValues(self.service, protocol, statusClass, self.block),
	}
	if !slot.CompareAndSwap(nil, series) {
		return slot.Load()
	}
	return series
}

// A service's request metrics, by the block label of the block that
// answered: each of the service's blocks, and local for connect in-process.
type frontServiceRequestMetrics struct {
	blockRequestMetrics map[string]*frontRequestMetrics
	// for a request no block of the service answered
	noBlockRequestMetrics *frontRequestMetrics
}

// The request metrics of every service of a route table, by service name,
// and of a request for no service. Only a block of the service, or connect
// in-process, is a block label. Read-only once built.
type frontRequestMetricsTable struct {
	serviceRequestMetrics   map[string]*frontServiceRequestMetrics
	noServiceRequestMetrics *frontRequestMetrics
}

// The request metrics of a route table's services, over metrics.
func newFrontRequestMetricsTable(metrics *frontMetrics, routes *routeHttpTable) *frontRequestMetricsTable {
	table := &frontRequestMetricsTable{
		serviceRequestMetrics:   map[string]*frontServiceRequestMetrics{},
		noServiceRequestMetrics: newFrontRequestMetrics(metrics, routeHttpNoLabel, routeHttpNoLabel),
	}
	for serviceName, routeService := range routes.services {
		service := routeService.service.Name
		serviceRequestMetrics := &frontServiceRequestMetrics{
			blockRequestMetrics: map[string]*frontRequestMetrics{
				routeHttpInProcessBlock: newFrontRequestMetrics(metrics, service, routeHttpInProcessBlock),
			},
			noBlockRequestMetrics: newFrontRequestMetrics(metrics, service, routeHttpNoLabel),
		}
		for blockName := range routeService.blockNames {
			serviceRequestMetrics.blockRequestMetrics[blockName] = newFrontRequestMetrics(metrics, service, blockName)
		}
		table.serviceRequestMetrics[serviceName] = serviceRequestMetrics
	}
	return table
}

// The request metrics of a request for serviceName answered by
// upstreamBlock: the service's, or no service's when the table has none of
// that name, and the block's, or no block's when it is neither a block of
// the service nor local.
func (self *frontRequestMetricsTable) requestMetrics(serviceName string, upstreamBlock string) *frontRequestMetrics {
	serviceRequestMetrics, ok := self.serviceRequestMetrics[serviceName]
	if !ok {
		return self.noServiceRequestMetrics
	}
	if blockRequestMetrics, ok := serviceRequestMetrics.blockRequestMetrics[upstreamBlock]; ok {
		return blockRequestMetrics
	}
	return serviceRequestMetrics.noBlockRequestMetrics
}

// Records one ended request: its metrics, and its access log line when one
// is due.
func (self *Front) observe(exchange *routeHttpExchange) {
	response := exchange.response
	requestMetrics := self.requestMetrics.requestMetrics(exchange.serviceName, response.upstreamBlock)
	series := requestMetrics.requestSeries(routeHttpProtocolIndex(exchange.r.ProtoMajor), routeHttpStatusClassIndex(response.statusCode))
	series.requests.Inc()
	if !response.headerTime.IsZero() {
		series.responseHeaderSeconds.Observe(response.headerTime.Sub(exchange.startTime).Seconds())
	}

	if !self.accessLog.enabled() {
		return
	}
	service := requestMetrics.service
	block := requestMetrics.block
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
