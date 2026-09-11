package router

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const httpMetricsInterval = time.Minute

// httpMetrics owns the bounded route-level metrics shared by every HTTP
// service. The process pusher supplies env/service/block/host/instance; route
// is always a configured Route id or one of the two fixed unmatched labels.
type httpMetrics struct {
	requests      *prometheus.CounterVec
	requestBytes  *prometheus.CounterVec
	responseBytes *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	inflight      *prometheus.GaugeVec

	intervalMaxDesc      *prometheus.Desc
	intervalMaxTimeDesc  *prometheus.Desc
	intervalMaxStateLock sync.Mutex
	intervalMaxByRoute   map[string]httpIntervalMax
	now                  func() time.Time
}

// httpIntervalMax is the maximum completed duration in one wall-clock
// interval and the time of the observation that established that maximum.
type httpIntervalMax struct {
	bucket     int64
	seconds    float64
	observedAt time.Time
}

// newHttpMetrics constructs and registers one independent metric family.
func newHttpMetrics(registerer prometheus.Registerer) *httpMetrics {
	metrics := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "HTTP requests by configured route, exact response status, and bounded terminal outcome.",
		}, []string{"route", "status", "outcome"}),
		requestBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "http",
			Name:      "request_bytes_total",
			Help:      "HTTP request-body bytes actually consumed by handlers, by configured route.",
		}, []string{"route"}),
		responseBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "http",
			Name:      "response_bytes_total",
			Help:      "HTTP response-body bytes successfully written, by configured route.",
		}, []string{"route"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "urnetwork",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP handler duration by configured route.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"route"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "http",
			Name:      "requests_inflight",
			Help:      "HTTP requests currently inside a configured route or fixed unmatched handler.",
		}, []string{"route"}),
		intervalMaxDesc: prometheus.NewDesc(
			"urnetwork_http_request_interval_max_seconds",
			"Maximum completed HTTP handler duration in the latest one-minute wall-clock interval that had a request.",
			[]string{"route"}, nil,
		),
		intervalMaxTimeDesc: prometheus.NewDesc(
			"urnetwork_http_request_interval_max_timestamp_seconds",
			"Unix time of the request observation backing the latest one-minute HTTP interval maximum.",
			[]string{"route"}, nil,
		),
		intervalMaxByRoute: map[string]httpIntervalMax{},
		now:                time.Now,
	}
	registerer.MustRegister(
		metrics.requests,
		metrics.requestBytes,
		metrics.responseBytes,
		metrics.duration,
		metrics.inflight,
		metrics,
	)
	return metrics
}

var defaultHttpMetrics = newHttpMetrics(prometheus.DefaultRegisterer)

// Describe implements prometheus.Collector for the paired interval maximum.
func (self *httpMetrics) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.intervalMaxDesc
	descriptions <- self.intervalMaxTimeDesc
}

// Collect implements prometheus.Collector. A maximum and its timestamp are
// copied under one lock so a scrape cannot pair different interval generations.
func (self *httpMetrics) Collect(metrics chan<- prometheus.Metric) {
	self.intervalMaxStateLock.Lock()
	samples := make(map[string]httpIntervalMax, len(self.intervalMaxByRoute))
	for route, sample := range self.intervalMaxByRoute {
		samples[route] = sample
	}
	self.intervalMaxStateLock.Unlock()

	for route, sample := range samples {
		metrics <- prometheus.MustNewConstMetric(
			self.intervalMaxDesc,
			prometheus.GaugeValue,
			sample.seconds,
			route,
		)
		metrics <- prometheus.MustNewConstMetric(
			self.intervalMaxTimeDesc,
			prometheus.GaugeValue,
			float64(sample.observedAt.UnixNano())/float64(time.Second),
			route,
		)
	}
}

// observe records one completed request without retaining a path, identity,
// header, or error string.
func (self *httpMetrics) observe(route string, status string, outcome string, duration time.Duration, requestBytes int64, responseBytes int64) {
	self.requests.WithLabelValues(route, status, outcome).Inc()
	self.requestBytes.WithLabelValues(route).Add(float64(requestBytes))
	self.responseBytes.WithLabelValues(route).Add(float64(responseBytes))
	self.duration.WithLabelValues(route).Observe(duration.Seconds())

	now := self.now()
	bucket := now.UnixNano() / httpMetricsInterval.Nanoseconds()
	self.intervalMaxStateLock.Lock()
	current, ok := self.intervalMaxByRoute[route]
	if !ok || current.bucket != bucket || current.seconds < duration.Seconds() {
		self.intervalMaxByRoute[route] = httpIntervalMax{
			bucket:     bucket,
			seconds:    duration.Seconds(),
			observedAt: now,
		}
	}
	self.intervalMaxStateLock.Unlock()
}

// requestMetricsObservation owns one request's counters and wrapped streams.
type requestMetricsObservation struct {
	metrics *httpMetrics
	route   string
	start   time.Time
	body    *requestMetricsBody
	writer  *requestMetricsWriter
}

// beginHttpRequest starts one bounded route observation.
func beginHttpRequest(metrics *httpMetrics, route string, writer http.ResponseWriter, request *http.Request) (*requestMetricsObservation, http.ResponseWriter) {
	body := &requestMetricsBody{ReadCloser: request.Body}
	if request.Body != nil {
		request.Body = body
	}
	base := &requestMetricsWriter{ResponseWriter: writer}
	metrics.inflight.WithLabelValues(route).Inc()
	return &requestMetricsObservation{
		metrics: metrics,
		route:   route,
		start:   time.Now(),
		body:    body,
		writer:  base,
	}, wrapRequestMetricsWriter(base)
}

// finish records the terminal outcome exactly once for the owning ServeHTTP.
func (self *requestMetricsObservation) finish(outcome string) {
	self.metrics.inflight.WithLabelValues(self.route).Dec()
	status := "none"
	if self.writer.status != 0 {
		status = strconv.Itoa(self.writer.status)
	} else if !self.writer.hijacked && outcome == "completed" {
		status = strconv.Itoa(http.StatusOK)
	}
	self.metrics.observe(
		self.route,
		status,
		outcome,
		time.Since(self.start),
		self.body.bytes,
		self.writer.bytes,
	)
}

// requestMetricsBody counts bytes handlers actually consume.
type requestMetricsBody struct {
	io.ReadCloser
	bytes int64
}

// Read implements io.Reader.
func (self *requestMetricsBody) Read(buffer []byte) (int, error) {
	if self.ReadCloser == nil {
		return 0, io.EOF
	}
	count, err := self.ReadCloser.Read(buffer)
	self.bytes += int64(count)
	return count, err
}

// requestMetricsWriter counts response status and bytes while retaining an
// Unwrap chain for http.ResponseController.
type requestMetricsWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

// Unwrap exposes the native writer to http.ResponseController.
func (self *requestMetricsWriter) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}

// WriteHeader implements http.ResponseWriter.
func (self *requestMetricsWriter) WriteHeader(status int) {
	if self.status == 0 {
		self.status = status
	}
	self.ResponseWriter.WriteHeader(status)
}

// Write implements http.ResponseWriter.
func (self *requestMetricsWriter) Write(buffer []byte) (int, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.Write(buffer)
	self.bytes += int64(count)
	return count, err
}

// requestMetricsWriterFlusher retains http.Flusher only when the native writer
// supports it.
type requestMetricsWriterFlusher struct{ *requestMetricsWriter }

// Flush implements http.Flusher.
func (self *requestMetricsWriterFlusher) Flush() {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	self.ResponseWriter.(http.Flusher).Flush()
}

// requestMetricsWriterHijacker retains http.Hijacker only when supported.
type requestMetricsWriterHijacker struct{ *requestMetricsWriter }

// Hijack implements http.Hijacker.
func (self *requestMetricsWriterHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, readWriter, err := self.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		self.hijacked = true
	}
	return conn, readWriter, err
}

// requestMetricsWriterFlusherHijacker retains Flusher and Hijacker.
type requestMetricsWriterFlusherHijacker struct{ *requestMetricsWriterFlusher }

// Hijack implements http.Hijacker.
func (self *requestMetricsWriterFlusherHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, readWriter, err := self.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		self.hijacked = true
	}
	return conn, readWriter, err
}

// requestMetricsWriterPusher retains http.Pusher only when supported.
type requestMetricsWriterPusher struct{ *requestMetricsWriter }

// Push implements http.Pusher.
func (self *requestMetricsWriterPusher) Push(target string, options *http.PushOptions) error {
	return self.ResponseWriter.(http.Pusher).Push(target, options)
}

// requestMetricsWriterFlusherPusher retains Flusher and Pusher.
type requestMetricsWriterFlusherPusher struct{ *requestMetricsWriterFlusher }

// Push implements http.Pusher.
func (self *requestMetricsWriterFlusherPusher) Push(target string, options *http.PushOptions) error {
	return self.ResponseWriter.(http.Pusher).Push(target, options)
}

// requestMetricsWriterHijackerPusher retains Hijacker and Pusher.
type requestMetricsWriterHijackerPusher struct{ *requestMetricsWriterHijacker }

// Push implements http.Pusher.
func (self *requestMetricsWriterHijackerPusher) Push(target string, options *http.PushOptions) error {
	return self.ResponseWriter.(http.Pusher).Push(target, options)
}

// requestMetricsWriterFlusherHijackerPusher retains all three HTTP optional
// capabilities.
type requestMetricsWriterFlusherHijackerPusher struct {
	*requestMetricsWriterFlusherHijacker
}

// Push implements http.Pusher.
func (self *requestMetricsWriterFlusherHijackerPusher) Push(target string, options *http.PushOptions) error {
	return self.ResponseWriter.(http.Pusher).Push(target, options)
}

// requestMetricsWriterReaderFrom retains io.ReaderFrom and accounts for its
// optimized copy path.
type requestMetricsWriterReaderFrom struct{ *requestMetricsWriter }

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterFlusherReaderFrom retains Flusher and ReaderFrom.
type requestMetricsWriterFlusherReaderFrom struct{ *requestMetricsWriterFlusher }

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterFlusherReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterHijackerReaderFrom retains Hijacker and ReaderFrom.
type requestMetricsWriterHijackerReaderFrom struct{ *requestMetricsWriterHijacker }

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterHijackerReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterFlusherHijackerReaderFrom retains Flusher, Hijacker,
// and ReaderFrom.
type requestMetricsWriterFlusherHijackerReaderFrom struct {
	*requestMetricsWriterFlusherHijacker
}

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterFlusherHijackerReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterPusherReaderFrom retains Pusher and ReaderFrom.
type requestMetricsWriterPusherReaderFrom struct{ *requestMetricsWriterPusher }

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterPusherReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterFlusherPusherReaderFrom retains Flusher, Pusher, and
// ReaderFrom.
type requestMetricsWriterFlusherPusherReaderFrom struct {
	*requestMetricsWriterFlusherPusher
}

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterFlusherPusherReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterHijackerPusherReaderFrom retains Hijacker, Pusher, and
// ReaderFrom.
type requestMetricsWriterHijackerPusherReaderFrom struct {
	*requestMetricsWriterHijackerPusher
}

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterHijackerPusherReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// requestMetricsWriterFlusherHijackerPusherReaderFrom retains every optional
// capability used by the server's handlers.
type requestMetricsWriterFlusherHijackerPusherReaderFrom struct {
	*requestMetricsWriterFlusherHijackerPusher
}

// ReadFrom implements io.ReaderFrom.
func (self *requestMetricsWriterFlusherHijackerPusherReaderFrom) ReadFrom(reader io.Reader) (int64, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	count, err := self.ResponseWriter.(io.ReaderFrom).ReadFrom(reader)
	self.bytes += count
	return count, err
}

// wrapRequestMetricsWriter preserves precisely the native optional interface
// set. It never advertises Hijacker, Flusher, Pusher, or ReaderFrom when the
// underlying writer lacks that capability.
func wrapRequestMetricsWriter(base *requestMetricsWriter) http.ResponseWriter {
	mask := 0
	if _, ok := base.ResponseWriter.(http.Flusher); ok {
		mask |= 1
	}
	if _, ok := base.ResponseWriter.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := base.ResponseWriter.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := base.ResponseWriter.(io.ReaderFrom); ok {
		mask |= 8
	}

	switch mask {
	case 1:
		return &requestMetricsWriterFlusher{base}
	case 2:
		return &requestMetricsWriterHijacker{base}
	case 3:
		return &requestMetricsWriterFlusherHijacker{&requestMetricsWriterFlusher{base}}
	case 4:
		return &requestMetricsWriterPusher{base}
	case 5:
		return &requestMetricsWriterFlusherPusher{&requestMetricsWriterFlusher{base}}
	case 6:
		return &requestMetricsWriterHijackerPusher{&requestMetricsWriterHijacker{base}}
	case 7:
		return &requestMetricsWriterFlusherHijackerPusher{&requestMetricsWriterFlusherHijacker{&requestMetricsWriterFlusher{base}}}
	case 8:
		return &requestMetricsWriterReaderFrom{base}
	case 9:
		return &requestMetricsWriterFlusherReaderFrom{&requestMetricsWriterFlusher{base}}
	case 10:
		return &requestMetricsWriterHijackerReaderFrom{&requestMetricsWriterHijacker{base}}
	case 11:
		return &requestMetricsWriterFlusherHijackerReaderFrom{&requestMetricsWriterFlusherHijacker{&requestMetricsWriterFlusher{base}}}
	case 12:
		return &requestMetricsWriterPusherReaderFrom{&requestMetricsWriterPusher{base}}
	case 13:
		return &requestMetricsWriterFlusherPusherReaderFrom{&requestMetricsWriterFlusherPusher{&requestMetricsWriterFlusher{base}}}
	case 14:
		return &requestMetricsWriterHijackerPusherReaderFrom{&requestMetricsWriterHijackerPusher{&requestMetricsWriterHijacker{base}}}
	case 15:
		return &requestMetricsWriterFlusherHijackerPusherReaderFrom{&requestMetricsWriterFlusherHijackerPusher{&requestMetricsWriterFlusherHijacker{&requestMetricsWriterFlusher{base}}}}
	default:
		return base
	}
}

// httpRequestOutcome classifies a normal return without inspecting any error
// text or caller-controlled value.
func httpRequestOutcome(ctx context.Context) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	return "completed"
}
