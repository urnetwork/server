package router

import (
	"context"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// regex table approach inspired by https://benhoyt.com/writings/go-routing/
// Matching is done by a path-segment trie (see route_trie.go) to avoid scanning
// every route per request; the per-route regex remains the source of truth for
// dynamic routes.

// Route is one method+pattern handler. id and captures are precomputed in
// NewRoute so the request path neither formats the stats key nor re-inspects the
// regex per call. index is the route's position in the table, assigned when the
// trie is built; it breaks match ties by first-match-wins.
type Route struct {
	method   string
	pattern  string
	regex    *regexp.Regexp
	handler  http.HandlerFunc
	id       string
	captures bool
	index    int
	// the body policy of a route that streams its body on every front, lb or
	// not (NewStreamingRoute); nil streams only on the lb's mark
	streaming *StreamingBody
}

func NewRoute(method string, pattern string, handler http.HandlerFunc) *Route {
	regex := regexp.MustCompile("^" + pattern + "$")
	return &Route{
		method:   method,
		pattern:  pattern,
		regex:    regex,
		handler:  handler,
		id:       method + " " + regex.String(),
		captures: 0 < regex.NumSubexp(),
	}
}

// NewStreamingRoute is a route whose request body streams from the client
// on every front, with its own StreamingBody policy. The pattern syntax is
// the one a warp `streamable_paths` entry uses, so the lb entry for the
// route is this pattern.
func NewStreamingRoute(method string, pattern string, handler http.HandlerFunc, streaming StreamingBody) *Route {
	route := NewRoute(method, pattern, handler)
	route.streaming = &streaming
	return route
}

func (self *Route) String() string {
	return self.id
}

type pathValuesKey struct{}

type Router struct {
	ctx     context.Context
	routes  []*Route
	trie    *routeTrie
	stats   *RouterStats
	metrics *httpMetrics
	// the policy for a request the lb marks as streamed (StreamingBody)
	streaming StreamingBody
}

func NewRouter(ctx context.Context, routes []*Route) *Router {
	return &Router{
		ctx:     ctx,
		routes:  routes,
		trie:    newRouteTrie(routes),
		stats:   NewRouterStats(ctx, 60*time.Second),
		metrics: defaultHttpMetrics,
	}
}

// SetStreamingBody sets the policy applied to a request whose body the lb
// streams (RequestBufferingHeader). The zero policy only drains a body the
// handler left unread after an early response; a service sets deadlines
// that match its HttpServerOptions.
func (self *Router) SetStreamingBody(streaming StreamingBody) {
	self.streaming = streaming
}

// ServeHTTP matches the request via the trie, then dispatches. Only routes with
// capture groups pay for the path-values context and request clone; static
// routes are handed the original request untouched. The panic recover is
// deferred once at function scope (not inside a loop) so it is open-coded and
// does not allocate.
//
// `http.Handler`
func (self *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, allowedMethods := self.trie.match(r.Method, r.URL.Path)
	if route == nil {
		if 0 < len(allowedMethods) {
			observation, writer := beginHttpRequest(self.metrics, "method_not_allowed", w, r)
			writer.Header().Set("Allow", strings.Join(allowedMethods, ", "))
			http.Error(writer, "Method not allowed.", http.StatusMethodNotAllowed)
			observation.finish("method_not_allowed")
		} else {
			observation, writer := beginHttpRequest(self.metrics, "not_found", w, r)
			http.NotFound(writer, r)
			observation.finish("not_found")
		}
		return
	}

	observation, writer := beginHttpRequest(self.metrics, route.id, w, r)

	// set once the route streams its body; the recover below drains it too
	var body *streamingBody

	defer func() {
		if err := recover(); err != nil {
			// Preserve net/http's exact transport-abort signal, including after
			// a flushed prefix. An error response would forge a clean EOF.
			if err == http.ErrAbortHandler {
				observation.finish("aborted")
				panic(err)
			}
			if server.IsDoneError(err) {
				// A Done panic is the standard cancellation path. In particular,
				// Connect can observe it after Gorilla has hijacked the H1 socket;
				// net/http no longer owns that connection and any attempted error
				// response produces a write-after-hijack warning. Consume the
				// lifecycle signal without falling through to http.Error.
				observation.finish("canceled")
				return
			}
			self.stats.Error(route.id)
			glog.Infof(
				"[h]unhandled error from route %s: %s\n",
				route.id,
				server.ErrorJson(err, debug.Stack()),
			)
			func() {
				// note the connection might be hijacked in this case
				defer recover()
				http.Error(writer, "Error. Please email support@ur.io for help.", http.StatusInternalServerError)
			}()
			if body != nil && !observation.writer.hijacked {
				func() {
					defer recover()
					body.drain()
				}()
			}
			observation.finish("panic")
			return
		}
		observation.finish(httpRequestOutcome(r.Context()))
	}()

	req := r
	if route.captures {
		// only capture-group routes need path values threaded through the context
		matches := route.regex.FindStringSubmatch(r.URL.Path)
		var pathValues []string
		if 1 < len(matches) {
			pathValues = matches[1:]
		}
		req = r.WithContext(context.WithValue(r.Context(), pathValuesKey{}, pathValues))
	}

	if policy, ok := self.streamingPolicy(route, req); ok {
		body = newStreamingBody(req.Body, writer, policy)
		req.Body = body
	}

	startTime := time.Now()
	route.handler(writer, req)
	endTime := time.Now()
	self.stats.Success(route.id, endTime.Sub(startTime))
	if body != nil && !observation.writer.hijacked {
		body.drain()
	}
}

// FlushStats logs the current stats buckets immediately. Called at drain end
// so the final window's requests are reported before the process exits.
func (self *Router) FlushStats() {
	self.stats.Flush()
}

// GetPathValues returns the regex capture groups for the matched route, or nil
// if the route has none. The nil case is safe: static routes do not populate the
// context, and only capture-group handlers call this.
func GetPathValues(r *http.Request) []string {
	pathValues, _ := r.Context().Value(pathValuesKey{}).([]string)
	return pathValues
}
