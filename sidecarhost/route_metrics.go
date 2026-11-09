package sidecarhost

// §19.8's route metrics for one block's HttpEngine: urnetwork_<svc>_http_requests_total{route,code} and
// urnetwork_<svc>_http_request_seconds{route}. The route is the http.ServeMux pattern that served the
// request, which the mux leaves in http.Request.Pattern on the request it was handed, read once the
// handler returns, so an engine routing on ServeMux patterns gets its route templates. Anything else,
// a path no pattern matched, or an engine whose mux saw a copy of the request, counts as unmatched.
// The raw path never reaches a label (I-13, D-OPS-8). A handler's panic is counted as a 500 and goes on
// up to the router, which answers it.

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/sidecar"
)

// The route label of a request no pattern matched.
const unmatchedRoute = "unmatched"

// One block's route metrics.
type routeMetrics struct {
	clock    sidecar.Clock
	requests sidecar.MetricsCounter
	seconds  sidecar.MetricsHistogram
}

// Declares service's route metrics on metrics, timed on clock.
func newRouteMetrics(metrics sidecar.Metrics, service string, clock sidecar.Clock) *routeMetrics {
	return &routeMetrics{
		clock: clock,
		requests: metrics.Counter(
			"urnetwork_"+service+"_http_requests_total",
			"Requests the engine served, by route template and status code.",
			"route", "code",
		),
		seconds: metrics.Histogram(
			"urnetwork_"+service+"_http_request_seconds",
			"How long the engine took to answer, by route template.",
			nil,
			"route",
		),
	}
}

// The route a served request counts under: its pattern, or unmatched.
func requestRoute(r *http.Request) string {
	if r.Pattern == "" {
		return unmatchedRoute
	}
	return r.Pattern
}

// handler with each request counted and timed by the route that served it.
func (self *routeMetrics) wrap(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := self.clock.Now()
		recorder := &routeStatusRecorder{ResponseWriter: w}
		completed := false
		defer func() {
			code := recorder.status
			if !completed && !recorder.wroteHeader {
				// a panic before any answer: the router answers 500
				code = http.StatusInternalServerError
			} else if code == 0 {
				code = http.StatusOK
			}
			route := requestRoute(r)
			self.requests.Add(1, route, strconv.Itoa(code))
			self.seconds.Observe(self.clock.Now().Sub(startTime).Seconds(), route)
		}()
		handler.ServeHTTP(recorder, r)
		completed = true
	})
}

// The status a handler answered with. It forwards everything else, and Unwrap gives
// http.ResponseController the writer underneath, so flushes and deadlines still reach it.
type routeStatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// See http.ResponseWriter.
func (self *routeStatusRecorder) WriteHeader(statusCode int) {
	if !self.wroteHeader && 200 <= statusCode {
		self.status = statusCode
		self.wroteHeader = true
	}
	self.ResponseWriter.WriteHeader(statusCode)
}

// See http.ResponseWriter.
func (self *routeStatusRecorder) Write(data []byte) (int, error) {
	if !self.wroteHeader {
		self.status = http.StatusOK
		self.wroteHeader = true
	}
	return self.ResponseWriter.Write(data)
}

// See http.Flusher.
func (self *routeStatusRecorder) Flush() {
	if !self.wroteHeader {
		self.status = http.StatusOK
		self.wroteHeader = true
	}
	http.NewResponseController(self.ResponseWriter).Flush()
}

// The writer underneath, for http.ResponseController.
func (self *routeStatusRecorder) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}
