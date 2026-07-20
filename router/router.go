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

func (self *Route) String() string {
	return self.id
}

type pathValuesKey struct{}

type Router struct {
	ctx    context.Context
	routes []*Route
	trie   *routeTrie
	stats  *RouterStats
}

func NewRouter(ctx context.Context, routes []*Route) *Router {
	return &Router{
		ctx:    ctx,
		routes: routes,
		trie:   newRouteTrie(routes),
		stats:  NewRouterStats(ctx, 60*time.Second),
	}
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
			w.Header().Set("Allow", strings.Join(allowedMethods, ", "))
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		} else {
			http.NotFound(w, r)
		}
		return
	}

	defer func() {
		if err := recover(); err != nil {
			// suppress the error
			if server.IsDoneError(err) {
				// standard pattern to raise on context done. ignore
			} else {
				self.stats.Error(route.id)
				glog.Infof(
					"[h]unhandled error from route %s: %s\n",
					route.id,
					server.ErrorJson(err, debug.Stack()),
				)
			}
			func() {
				// note the connection might be hijacked in this case
				defer recover()
				http.Error(w, "Error. Please email support@ur.io for help.", http.StatusInternalServerError)
			}()
		}
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

	startTime := time.Now()
	route.handler(w, req)
	endTime := time.Now()
	self.stats.Success(route.id, endTime.Sub(startTime))
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
