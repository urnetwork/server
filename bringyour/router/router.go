package router

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"fmt"

	"bringyour.com/bringyour"
)


// regex table approach inspired by https://benhoyt.com/writings/go-routing/

type Route struct {
	method  string
	regex   *regexp.Regexp
	handler http.HandlerFunc
}

func NewRoute(method string, pattern string, handler http.HandlerFunc) *Route {
	return &Route{
		method: method,
		regex: regexp.MustCompile("^" + pattern + "$"),
		handler: handler,
	}
}

func (self *Route) String() string {
	return fmt.Sprintf("%s %s", self.method, self.regex)
}




type ctxKey struct{}


type Router struct {
	ctx context.Context
	routes []*Route
}
func NewRouter(ctx context.Context, routes []*Route) *Router {
	return &Router{
		ctx: ctx,
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
			ctx := context.WithValue(self.ctx, ctxKey{}, matches[1:])
			func() {
				defer func() {
					if err := recover(); err != nil {
						// suppress the error
						bringyour.Logger().Printf("Unhandled error from route %s (%s)\n", route.String(), err)
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
	return r.Context().Value(ctxKey{}).([]string)
}


