package router

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
)

// regex table approach inspired by https://benhoyt.com/writings/go-routing/

type Route struct {
	method  string
	regex   *regexp.Regexp
	handler http.HandlerFunc
}

func NewRoute(method string, pattern string, handler http.HandlerFunc) *Route {
	return &Route{
		method:  method,
		regex:   regexp.MustCompile("^" + pattern + "$"),
		handler: handler,
	}
}

func (self *Route) String() string {
	return fmt.Sprintf("%s %s", self.method, self.regex)
}

type pathValuesKey struct{}

type Router struct {
	ctx    context.Context
	routes []*Route
}

func NewRouter(ctx context.Context, routes []*Route) *Router {
	return &Router{
		ctx:    ctx,
		routes: routes,
	}
}

// `http.Handler`
func (self *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, route := range self.routes {
		matches := route.regex.FindStringSubmatch(r.URL.Path)
		if len(matches) > 0 {
			if r.Method != route.method {
				allow = append(allow, route.method)
				continue
			}
			ctx := context.WithValue(r.Context(), pathValuesKey{}, matches[1:])
			func() {
				defer func() {
					if r := recover(); r != nil {
						// suppress the error
						if server.IsDoneError(r) {
							// standard pattern to raise on context done. ignore
						} else {
							glog.Infof(
								"[h]unhandled error from route %s: %s\n",
								route.String(),
								server.ErrorJson(r, debug.Stack()),
							)
						}
						func() {
							// note the connection might be hijacked in this case
							defer recover()
							http.Error(w, "Error. Please email support@ur.io for help.", http.StatusInternalServerError)
						}()
					}
				}()
				route.handler(w, r.WithContext(ctx))
			}()
			return
		}
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(w, r)
}

func GetPathValues(r *http.Request) []string {
	return r.Context().Value(pathValuesKey{}).([]string)
}
