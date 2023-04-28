package router

import (
	"context"
	"net/http"
	"regexp"
	"strings"
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


type ctxKey struct{}


type Router struct {
	routes []*Route
}
func NewRouter(routes []*Route) *Router {
	return &Router{
		routes: routes,
	}
}

func (self Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, route := range self.routes {
		matches := route.regex.FindStringSubmatch(r.URL.Path)
		if len(matches) > 0 {
			if r.Method != route.method {
				allow = append(allow, route.method)
				continue
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, matches[1:])
			route.handler(w, r.WithContext(ctx))
			return
		}
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(w, r)
}


func GetPathValues(r *http.Request) []string {
	return r.Context().Value(ctxKey{}).([]string)
}


