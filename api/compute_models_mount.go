package api

// The api's model routes (DESIGN-COMPUTE §14.3, §6.5; CO-49; CSRV-M11-01), sidecar/handler's ComputeModelRoutes
// mounted outside the session: a local model file's chunk index and its chunks, from the mirror in the blob store
// (server.ComputeModelBlobStore), each request counted against its caller address's peppered /29 or /56 hash
// (server.CountComputeModelRouteAddress), the index route's and the chunk route's apart.
//
//	GET|HEAD /compute/models/{sha256}/index
//	GET|HEAD /compute/models/{sha256}/chunks/{n}
//
// Public and read with no credential (route_authz.go's credentialUnreadRoutes): no gate in front of them reads a
// bearer, and neither does the handler, so a sidecar's token, a forwarding client's, a lag.dev guest's or none is
// answered alike, and the api learns a file and an address, never a member. The only thing read of the request
// besides its path is its address, as the ingress gave it (session.ResolveClientAddress), and only as the peppered
// hash the count keys on.

import (
	"context"
	"net/http"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/handler"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Counts a model route's request against its caller's address. Tests replace it.
var computeModelAddressCount = server.CountComputeModelRouteAddress

// The time a model route's request is counted at. Tests replace it.
var computeModelNow = server.NowUtc

// The routes' limiter: the caller's address, as the ingress gave it, counted by its peppered hash.
type computeModelAddressLimiter struct{}

func (self computeModelAddressLimiter) Count(ctx context.Context, request *http.Request, limit *handler.ComputeModelAddressLimit, now time.Time) (*handler.ComputeModelLimitAnswer, error) {
	clientAddress, err := session.ResolveClientAddress(request)
	if err != nil {
		return nil, err
	}
	count, err := computeModelAddressCount(ctx, clientAddress, limit.Route, limit.Window, limit.Count, now)
	if err != nil {
		return nil, err
	}
	return &handler.ComputeModelLimitAnswer{Allowed: count.Allowed, RetryAfter: count.RetryAfter}, nil
}

// The routes' clock: computeModelNow, read at each request.
type computeModelClock struct {
	sidecar.RealClock
}

func (self computeModelClock) Now() time.Time {
	return computeModelNow()
}

// glog as the routes' sidecar.Logger.
type computeModelLogger struct{}

func (self computeModelLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self computeModelLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self computeModelLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self computeModelLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self computeModelLogger) V(level int32) sidecar.Verbose {
	return computeModelVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the routes' sidecar.Verbose.
type computeModelVerbose glog.Verbose

func (self computeModelVerbose) Enabled() bool {
	return bool(self)
}

func (self computeModelVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self computeModelVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}

// The four routes, over one handler and so one set of served files kept in memory.
func computeModelRoutes() []*router.Route {
	routes := handler.NewComputeModelRoutes(
		server.NewComputeModelBlobStore(server.DefaultComputeModelBlobStoreSettings()),
		computeModelAddressLimiter{},
		handler.DefaultComputeModelRouteSettings(),
		computeModelClock{},
		computeModelLogger{},
	)
	index := func(w http.ResponseWriter, r *http.Request) {
		pathValues := router.GetPathValues(r)
		routes.ServeIndex(w, r, pathValues[0])
	}
	chunk := func(w http.ResponseWriter, r *http.Request) {
		pathValues := router.GetPathValues(r)
		routes.ServeChunk(w, r, pathValues[0], pathValues[1])
	}
	return []*router.Route{
		router.NewRoute("GET", "/compute/models/([^/]+)/index", index),
		router.NewRoute("HEAD", "/compute/models/([^/]+)/index", index),
		router.NewRoute("GET", "/compute/models/([^/]+)/chunks/([^/]+)", chunk),
		router.NewRoute("HEAD", "/compute/models/([^/]+)/chunks/([^/]+)", chunk),
	}
}
