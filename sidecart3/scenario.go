package sidecart3

// §22.7's scenarios on T3 (TEST-M2-11; D-TEST-5): sidecartest's runner on a T3 Net, its stores captured whole
// (CaptureStores, CC3), the keys written through its host given to the schema guard, and §7.4's route allowlist taken from
// server's own route table, the routes api.Routes and oauth.Routes serve with no strict_exp refusal, so the T2 list
// the fake api holds a scenario to and the one server enforces are the same list, or a run shows where they part.
// What the runner checks is sidecartest's (scenario.go); this adds T3's stores and table and no mechanism.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
)

// One route server serves to a strict_exp bearer.
type t3SidecarRoute struct {
	method string
	path   *regexp.Regexp
}

// server's allowlist, read once from its route tables.
var t3SidecarRoutes = sync.OnceValue(func() []*t3SidecarRoute {
	routes := []*t3SidecarRoute{}
	for _, route := range slices.Concat(api.Routes(), oauth.Routes()) {
		if route.RefusesStrictExpCredentials() {
			continue
		}
		routes = append(routes, &t3SidecarRoute{method: route.Method(), path: regexp.MustCompile("^" + route.Pattern() + "$")})
	}
	return routes
})

// Whether server's route table lets a strict_exp bearer reach method path: a route no refusal gates.
func ServerRouteAllowed(method string, path string) bool {
	return slices.ContainsFunc(t3SidecarRoutes(), func(route *t3SidecarRoute) bool {
		return route.method == method && route.path.MatchString(path)
	})
}

// The api's router over server's own routes, which a scenario's flow calls on T3 as api's listener would serve it.
func ApiRouter(ctx context.Context) *router.Router {
	return router.NewRouter(ctx, api.Routes())
}

// The settings sidecartest's runner takes on T3: a T3 Net from settings, its stores captured, the keys its host wrote
// for the guard, server's allowlist. settings nil takes the defaults.
func ScenarioSettings(t testing.TB, settings *Settings) *sidecartest.ScenarioSettings {
	return &sidecartest.ScenarioSettings{
		Net: NetSettings(t, settings),
		CaptureStores: func(net *sidecartest.Net) error {
			_, err := CaptureStores(net, nil)
			return err
		},
		// the keys written through the Net's host: the test's database also holds server's own platform families,
		// provide modes and tier caches, which §2.8's list does not cover and CaptureStores still scans
		RedisKeys: func(ctx context.Context, net *sidecartest.Net) ([]string, error) {
			wrapper, ok := netFaultRedis.Load(net)
			if !ok {
				return nil, fmt.Errorf("the Net is no open T3 Net")
			}
			return wrapper.(*faultRedis).WrittenKeys(), nil
		},
		RouteAllowed: ServerRouteAllowed,
	}
}

// Runs scenario on T3 under §22.7's harness and guard, failing t with everything its report holds.
func RunScenario(t testing.TB, scenario *sidecartest.Scenario, settings *Settings) *sidecartest.ScenarioReport {
	t.Helper()
	return sidecartest.RunScenario(t, scenario, ScenarioSettings(t, settings))
}
