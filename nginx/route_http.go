package nginx

// Routing by authority (owner: s-http): the front's three handlers, HTTPS
// (TCP 443, h1 and h2), HTTP (TCP 80) and h3 (api names only), and the
// servers and locations the nginx generator declares (warp/warpctl/config.go
// addLbBlock, addServiceBlocks), rebuilt from the route table.
//
// The request's authority picks the server, not the TLS SNI, which only
// picks the certificate (review §5, D9): Host on h1, each stream's own
// :authority on h2 and h3, normalized as nginx's $host. An unknown or
// missing authority goes to the default server, the first lb name's, as an
// unknown Host does in nginx whatever the SNI was.
//
//   - lb names, TCP 443: per lb prefix p, `= p/status` answers the status
//     json (§2.6); `p/by/service/<svc>/` forwards to an lb_exposed service
//     with the matched part replaced by /, and `p/by/b/<svc>/<block>/` to
//     that block only; the services' route_request_bodies have exact
//     locations under both. Anything else is a 404. No CORS, security
//     headers, route zones or /status deny.
//   - lb names, TCP 80: `= p/status` and `= p/by/b/<svc>/<block>/status` for
//     every service, then a 301 to https for everything else. Route53's
//     health check needs that 200, not a redirect.
//   - service names: per service prefix p, `= p/status` is denied (403; on
//     h3, alt's 404) for a standard-status service; exact locations for
//     route_rate_limits and route_request_bodies; `p/` forwards, with the
//     streamable_paths nested location when the service isn't streamable.
//     Every location but the deny adds the security headers and the CORS
//     headers. Connect's own names are served in-process instead of
//     forwarded, with the ingress rewrite.
//   - redirect aliases: a 301 to `<scheme>://<target><request uri>`.
//   - TCP 80 on service names: a 301 to https; on unknown names, a 403.
//   - h3: api authorities only; any other is a 421 (Misdirected Request), so
//     a connection whose SNI is an api name carries no other service (D3).
//
// The request passes nginx's phases in order: the server's own return (the
// redirects), find-config (a declared body over the cap is a 413 there),
// rewrite (the location's return, and the CORS preflight's 204), the limits
// (429), the deny (403) and the content. So redirects and preflights are
// never limited, while denies and unmatched 404s are.
//
// A location's path matching is nginx's: exact locations first, then the
// longest prefix, then the nested regex, on the normalized uri.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/router"
)

// Which socket a handler serves.
type routeHttpScheme int

const (
	// TCP 443: h1 and h2 over TLS
	routeHttpSchemeHttps routeHttpScheme = iota
	// TCP 80: plaintext h1
	routeHttpSchemeHttp
	// h3 over QUIC, for api names only
	routeHttpSchemeH3Api
)

// What a location does once its request passes the limits.
type routeHttpLocationKind int

const (
	// the lb status json
	routeHttpLocationLbStatus routeHttpLocationKind = iota
	// `deny all`
	routeHttpLocationDeny
	// proxies to the service, forwarded or, for connect's names, in-process
	routeHttpLocationProxy
	// TCP 80's `return 301 https://$host$request_uri`, in the rewrite phase
	routeHttpLocationHttpsRedirect
)

// The in-process block label, for connect's own names.
const routeHttpInProcessBlock = "local"

// The services.yml status mode that denies /status on a service's names and
// exempts its lb status paths from the limits.
const routeServiceStatusStandard = "standard"

// One location of a server, as the generator declares it. Read-only once
// built.
type routeHttpLocation struct {
	kind routeHttpLocationKind
	// the exact uri, or the prefix, matched
	match string
	// the proxied service, for routeHttpLocationProxy
	service *routeHttpService
	// a per-block lb route's block; empty for any block of the service
	blockName string
	// an exact proxy location's path, sent upstream in place of the uri
	exactPath string
	// an exact location's body settings; nil for the defaults
	requestBody *RouteRequestBody
	// a prefix proxy location's matched part, replaced by "/" upstream: the
	// prefix without its trailing slash
	stripPrefix string
	// whether the body streams unbuffered
	streamed bool
	// a prefix location that tests the rest of the uri against the service's
	// streamable_paths, as the generator's nested regex location does
	nestedStreamable bool
	// adds the security and CORS headers: a service name's location
	serviceHeaders bool
	// adds Content-Type: application/json: an lb status location
	jsonContentType bool
	// the route zone replacing the shared one; nil for the shared one
	routeZone *limitRouteZone
	// served by the in-process connect handler instead of forwarded
	inProcess bool
}

// The path and query nginx sends upstream for a request this proxy location
// matched, and whether its body streams.
func (self *routeHttpLocation) upstream(request *routeRequest) (string, bool) {
	if self.exactPath != "" {
		// proxy_pass with a uri in an exact location
		return routeUpstreamTarget(self.exactPath, request.uri.args), self.streamed
	}
	rest := request.uri.path[len(self.stripPrefix):]
	if self.nestedStreamable && self.service.streamablePath(rest) {
		if self.stripPrefix == "" {
			// the nested location proxies with no uri and no rewrite, so
			// nginx sends the client's own target
			return request.requestUri, true
		}
		return request.uri.upstreamTarget(rest), true
	}
	return request.uri.upstreamTarget(rest), self.streamed
}

// The decoded path of what upstream gets, for the in-process handler.
func (self *routeHttpLocation) upstreamPath(request *routeRequest) string {
	if self.exactPath != "" {
		return self.exactPath
	}
	return request.uri.path[len(self.stripPrefix):]
}

// A path and a raw query as a request target.
func routeUpstreamTarget(path string, args string) string {
	if args == "" {
		return path
	}
	return path + "?" + args
}

// The locations of one server. Read-only once built.
type routeHttpServer struct {
	// the service whose names the server serves; nil for the lb's servers
	service        *routeHttpService
	exactLocations map[string]*routeHttpLocation
	// the longest prefix first
	prefixLocations []*routeHttpLocation
}

// An empty server of a service's names, or of the lb's for nil.
func newRouteHttpServer(service *routeHttpService) *routeHttpServer {
	return &routeHttpServer{
		service:         service,
		exactLocations:  map[string]*routeHttpLocation{},
		prefixLocations: []*routeHttpLocation{},
	}
}

// Adds an exact location. nginx refuses two for one uri, so the first is
// kept.
func (self *routeHttpServer) addExact(location *routeHttpLocation) {
	if _, ok := self.exactLocations[location.match]; !ok {
		self.exactLocations[location.match] = location
	}
}

// Adds a prefix location, keeping the longest first.
func (self *routeHttpServer) addPrefix(location *routeHttpLocation) {
	self.prefixLocations = append(self.prefixLocations, location)
	slices.SortStableFunc(self.prefixLocations, func(a *routeHttpLocation, b *routeHttpLocation) int {
		return len(b.match) - len(a.match)
	})
}

// The location of a normalized path, or nil for none: an exact match, else
// the longest matching prefix.
func (self *routeHttpServer) find(path string) *routeHttpLocation {
	if location, ok := self.exactLocations[path]; ok {
		return location
	}
	for _, location := range self.prefixLocations {
		if len(location.match) <= len(path) && path[:len(location.match)] == location.match {
			return location
		}
	}
	return nil
}

// One service as routing sees it. Read-only once built.
type routeHttpService struct {
	service   *RouteService
	forwarder serviceForwarder
	// the server of the service's own names; nil when it isn't exposed
	server *routeHttpServer
	// nil when the service has no cors_origins
	cors *responseCors
	// `^(?:(?:p1)|(?:p2))$` over the streamable_paths; nil when there are
	// none, or the whole service streams
	streamablePathsRegexp *regexp.Regexp
	blockNames            map[string]bool
	// the exact names nginx's no_success_access_log map lists, wildcards
	// left out, and its paths
	exactNames              map[string]bool
	noSuccessAccessLogPaths map[string]bool
}

// Whether a service-relative path is one of the service's streamable_paths.
func (self *routeHttpService) streamablePath(path string) bool {
	return self.streamablePathsRegexp != nil && self.streamablePathsRegexp.MatchString(path)
}

// What one front routes, built once from the route table. Read-only once
// built.
type routeHttpTable struct {
	lbNames map[string]bool
	// the first lb name: the default server's, and $host when a request
	// names none
	defaultLbName string
	lbHttpsServer *routeHttpServer
	lbHttpServer  *routeHttpServer
	// TCP 80's default server, for unknown names: `location / { deny all; }`
	defaultHttpServer *routeHttpServer
	// the lb status paths whose requests on an lb name skip both limits
	// (rateLimitStatusPaths): p/status, and p/by/service/<svc>/status and
	// p/by/b/<svc>/<block>/status for lb_exposed standard-status services.
	// Lowercased: nginx's `map $uri` lowercases its exact keys and what it
	// looks up, so a case variant is exempt too, though no location serves
	// it
	limitExemptLbPaths map[string]bool
	services           map[string]*routeHttpService
	noAccessLogHosts   map[string]bool
	// every hidden route prefix, without its leading /, to keep out of logs
	secretPrefixes []string
}

// Builds the servers and locations of a parsed route table. Every service
// needs a forwarder.
func newRouteHttpTable(routeTable *RouteTable, serviceForwarders map[string]serviceForwarder) (*routeHttpTable, error) {
	routes := &routeHttpTable{
		lbNames:            map[string]bool{},
		defaultLbName:      routeTable.Lb.Names[0],
		lbHttpsServer:      newRouteHttpServer(nil),
		lbHttpServer:       newRouteHttpServer(nil),
		defaultHttpServer:  newRouteHttpServer(nil),
		limitExemptLbPaths: map[string]bool{},
		services:           map[string]*routeHttpService{},
		noAccessLogHosts:   map[string]bool{},
		secretPrefixes:     []string{},
	}
	for _, name := range routeTable.Lb.Names {
		routes.lbNames[name] = true
	}
	for _, name := range routeTable.NoAccessLog {
		routes.noAccessLogHosts[name] = true
	}
	addSecretPrefixes := func(routePrefixes []string) {
		for _, routePrefix := range routePrefixes {
			if secretPrefix := routePrefix[min(1, len(routePrefix)):]; secretPrefix != "" && !slices.Contains(routes.secretPrefixes, secretPrefix) {
				routes.secretPrefixes = append(routes.secretPrefixes, secretPrefix)
			}
		}
	}
	addSecretPrefixes(routeTable.Lb.RoutePrefixes)

	for _, service := range routeTable.Services {
		forwarder, ok := serviceForwarders[service.Name]
		if !ok || forwarder == nil {
			return nil, fmt.Errorf("lbalt front: no forwarder for service %s", service.Name)
		}
		routeService := &routeHttpService{
			service:                 service,
			forwarder:               forwarder,
			cors:                    newResponseCors(service.CorsOrigins),
			blockNames:              map[string]bool{},
			exactNames:              map[string]bool{},
			noSuccessAccessLogPaths: map[string]bool{},
		}
		if 0 < len(service.StreamablePaths) && !service.Streamable {
			alternatives := ""
			for i, streamablePath := range service.StreamablePaths {
				if 0 < i {
					alternatives += "|"
				}
				alternatives += "(?:" + streamablePath + ")"
			}
			streamablePathsRegexp, err := regexp.Compile("^(?:" + alternatives + ")$")
			if err != nil {
				return nil, fmt.Errorf("lbalt front: service %s streamable_paths: %w", service.Name, err)
			}
			routeService.streamablePathsRegexp = streamablePathsRegexp
		}
		for _, block := range service.Blocks {
			routeService.blockNames[block.Name] = true
		}
		for _, name := range service.Names {
			if !routeNameIsWildcard(name) {
				routeService.exactNames[name] = true
			}
		}
		for _, path := range service.NoSuccessAccessLog {
			routeService.noSuccessAccessLogPaths[path] = true
		}
		if service.Exposed {
			addSecretPrefixes(service.RoutePrefixes)
			routeService.server = newRouteHttpServiceServer(routeService, service.Name == routeTable.Front.ConnectService)
		}
		routes.services[service.Name] = routeService
	}

	for _, lbRoutePrefix := range routeTable.Lb.RoutePrefixes {
		addLbRoutes(routes, routeTable, lbRoutePrefix)
	}
	routes.lbHttpServer.addPrefix(&routeHttpLocation{
		kind:  routeHttpLocationHttpsRedirect,
		match: "/",
	})
	routes.defaultHttpServer.addPrefix(&routeHttpLocation{
		kind:  routeHttpLocationDeny,
		match: "/",
	})
	return routes, nil
}

// The server of an exposed service's names (addServiceBlocks): per route
// prefix, the /status deny, the exact routes and the prefix location. All
// but the deny add the service's headers. inProcess marks connect's own
// names.
func newRouteHttpServiceServer(routeService *routeHttpService, inProcess bool) *routeHttpServer {
	service := routeService.service
	server := newRouteHttpServer(routeService)

	// the route_rate_limits, then the route_request_bodies of other paths;
	// a path in both is one location with both
	type exactRoute struct {
		path        string
		routeZone   *limitRouteZone
		requestBody *RouteRequestBody
	}
	exactRoutes := []*exactRoute{}
	pathExactRoutes := map[string]*exactRoute{}
	for _, routeRateLimit := range service.RouteRateLimits {
		route := &exactRoute{
			path:      routeRateLimit.Path,
			routeZone: newLimitRouteZone(service.Name, routeRateLimit),
		}
		exactRoutes = append(exactRoutes, route)
		pathExactRoutes[route.path] = route
	}
	for _, routeRequestBody := range service.RouteRequestBodies {
		if route, ok := pathExactRoutes[routeRequestBody.Path]; ok {
			route.requestBody = routeRequestBody
			continue
		}
		route := &exactRoute{
			path:        routeRequestBody.Path,
			requestBody: routeRequestBody,
		}
		exactRoutes = append(exactRoutes, route)
		pathExactRoutes[route.path] = route
	}

	for _, routePrefix := range service.RoutePrefixes {
		if service.Status == routeServiceStatusStandard {
			server.addExact(&routeHttpLocation{
				kind:  routeHttpLocationDeny,
				match: routePrefix + "/status",
			})
		}
		for _, route := range exactRoutes {
			server.addExact(&routeHttpLocation{
				kind:           routeHttpLocationProxy,
				match:          routePrefix + route.path,
				service:        routeService,
				exactPath:      route.path,
				requestBody:    route.requestBody,
				streamed:       service.Streamable || routeService.streamablePath(route.path),
				serviceHeaders: true,
				routeZone:      route.routeZone,
				inProcess:      inProcess,
			})
		}
		server.addPrefix(&routeHttpLocation{
			kind:             routeHttpLocationProxy,
			match:            routePrefix + "/",
			service:          routeService,
			stripPrefix:      routePrefix,
			streamed:         service.Streamable,
			nestedStreamable: routeService.streamablePathsRegexp != nil,
			serviceHeaders:   true,
			inProcess:        inProcess,
		})
	}
	return server
}

// Adds one lb prefix's locations to the lb servers (addLbBlock), and its
// status paths to the limit exemptions (rateLimitStatusPaths).
func addLbRoutes(routes *routeHttpTable, routeTable *RouteTable, lbRoutePrefix string) {
	statusPath := lbRoutePrefix + "/status"
	routes.limitExemptLbPaths[strings.ToLower(statusPath)] = true
	for _, server := range []*routeHttpServer{routes.lbHttpsServer, routes.lbHttpServer} {
		server.addExact(&routeHttpLocation{
			kind:            routeHttpLocationLbStatus,
			match:           statusPath,
			jsonContentType: true,
		})
	}

	for _, service := range routeTable.Services {
		routeService := routes.services[service.Name]

		// TCP 80: each block's status, for every service
		for _, block := range service.Blocks {
			routes.lbHttpServer.addExact(&routeHttpLocation{
				kind:            routeHttpLocationProxy,
				match:           lbRoutePrefix + "/by/b/" + service.Name + "/" + block.Name + "/status",
				service:         routeService,
				blockName:       block.Name,
				exactPath:       "/status",
				streamed:        service.Streamable,
				jsonContentType: true,
			})
		}

		if !service.LbExposed {
			continue
		}
		if service.Status == routeServiceStatusStandard {
			routes.limitExemptLbPaths[strings.ToLower(lbRoutePrefix+"/by/service/"+service.Name+"/status")] = true
			for _, block := range service.Blocks {
				routes.limitExemptLbPaths[strings.ToLower(lbRoutePrefix+"/by/b/"+service.Name+"/"+block.Name+"/status")] = true
			}
		}

		// TCP 443: by/service and by/b, with the exact body routes
		addRoutes := func(locationPrefix string, blockName string) {
			for _, routeRequestBody := range service.RouteRequestBodies {
				routes.lbHttpsServer.addExact(&routeHttpLocation{
					kind:        routeHttpLocationProxy,
					match:       locationPrefix + routeRequestBody.Path,
					service:     routeService,
					blockName:   blockName,
					exactPath:   routeRequestBody.Path,
					requestBody: routeRequestBody,
					streamed:    service.Streamable || routeService.streamablePath(routeRequestBody.Path),
				})
			}
			routes.lbHttpsServer.addPrefix(&routeHttpLocation{
				kind:             routeHttpLocationProxy,
				match:            locationPrefix + "/",
				service:          routeService,
				blockName:        blockName,
				stripPrefix:      locationPrefix,
				streamed:         service.Streamable,
				nestedStreamable: routeService.streamablePathsRegexp != nil,
			})
		}
		addRoutes(lbRoutePrefix+"/by/service/"+service.Name, "")
		for _, block := range service.Blocks {
			addRoutes(lbRoutePrefix+"/by/b/"+service.Name+"/"+block.Name, block.Name)
		}
	}
}

// Whether a served name is a leading wildcard, `*.b.c`.
func routeNameIsWildcard(name string) bool {
	return strings.HasPrefix(name, "*.")
}

// The service of a matched name, when it serves its names; nil for an lb
// name or a service that isn't exposed.
func (self *routeHttpTable) matchedService(hostMatch nameMatch) *routeHttpService {
	if hostMatch.service == nil {
		return nil
	}
	routeService, ok := self.services[hostMatch.service.Name]
	if !ok || routeService.server == nil {
		return nil
	}
	return routeService
}

// One interface's handler for one kind of socket. Safe for concurrent use.
type routeHttpHandler struct {
	front          *Front
	frontInterface *frontInterface
	scheme         routeHttpScheme
}

var _ http.Handler = (*routeHttpHandler)(nil)

// See http.Handler.
func (self *routeHttpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	exchange := &routeHttpExchange{
		front:          self.front,
		frontInterface: self.frontInterface,
		r:              r,
		response:       newResponseState(self.front, w, r),
		startTime:      self.front.now(),
	}
	defer exchange.end()
	exchange.response.closesConnection = self.front.lastRequestOnConnection(r)
	exchange.response.start()
	self.serve(exchange, r)
	exchange.response.finish()
}

// Routes one request.
func (self *routeHttpHandler) serve(exchange *routeHttpExchange, r *http.Request) {
	if self.front.closed.Load() {
		exchange.respondError(http.StatusServiceUnavailable)
		return
	}
	request, statusCode := readRouteRequest(r, self.front.names, self.front.settings.Http)
	if statusCode != 0 {
		exchange.respondError(statusCode)
		return
	}
	exchange.request = request
	routes := self.front.routes

	switch self.scheme {
	case routeHttpSchemeHttps:
		host := request.host
		server := routes.lbHttpsServer
		if request.hostMatched {
			if request.hostMatch.class == NameClassRedirect {
				// the server's return: no limits, no added headers
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				exchange.redirect(scheme + "://" + request.hostMatch.redirectTarget + request.requestUri)
				return
			}
			if routeService := routes.matchedService(request.hostMatch); routeService != nil && request.hostMatch.class != NameClassLb {
				server = routeService.server
			}
		}
		if host == "" {
			// nginx's $host is the default server's name
			host = routes.defaultLbName
		}
		self.serveLocation(exchange, r, request, host, server)

	case routeHttpSchemeHttp:
		if request.hostMatched {
			if request.hostMatch.class == NameClassLb {
				self.serveLocation(exchange, r, request, request.host, routes.lbHttpServer)
				return
			}
			// a service's port 80 server lists its redirect aliases too
			if request.hostMatch.class == NameClassRedirect || routes.matchedService(request.hostMatch) != nil {
				// the server's return: no limits, no added headers
				exchange.redirect("https://" + request.host + request.requestUri)
				return
			}
		}
		self.serveLocation(exchange, r, request, request.host, routes.defaultHttpServer)

	case routeHttpSchemeH3Api:
		routeService := routes.matchedService(request.hostMatch)
		if !request.hostMatched || request.hostMatch.class != NameClassApi || routeService == nil {
			exchange.respondError(http.StatusMisdirectedRequest)
			return
		}
		self.serveLocation(exchange, r, request, request.host, routeService.server)
	}
}

// Runs a request through a server's location, in nginx's phase order.
func (self *routeHttpHandler) serveLocation(exchange *routeHttpExchange, r *http.Request, request *routeRequest, host string, server *routeHttpServer) {
	routes := self.front.routes
	location := server.find(request.uri.path)
	// the service the request is for, for the metrics and the access log,
	// whether the front or the service answers it
	if server.service != nil {
		exchange.serviceName = server.service.service.Name
	}
	if location != nil && location.service != nil {
		exchange.serviceName = location.service.service.Name
	}
	if location != nil {
		if location.serviceHeaders {
			exchange.response.headers.security = true
			exchange.response.headers.cors = location.service.cors
		}
		exchange.response.headers.jsonContentType = location.jsonContentType
	}

	// find-config: a declared body over client_max_body_size
	if maxBodyBytes := self.front.settings.Body.MaxBodyBytes; 0 < maxBodyBytes && maxBodyBytes < r.ContentLength {
		exchange.respondError(http.StatusRequestEntityTooLarge)
		return
	}

	// rewrite: the location's return, and the CORS origin and preflight
	if location != nil && location.kind == routeHttpLocationHttpsRedirect {
		exchange.redirect("https://" + host + request.requestUri)
		return
	}
	if location != nil && location.serviceHeaders && location.service.cors != nil {
		exchange.response.headers.corsAllowOrigin = location.service.cors.allowOrigin(r)
		if r.Method == http.MethodOptions {
			exchange.response.headers.preflight = true
			exchange.writer().WriteHeader(http.StatusNoContent)
			return
		}
	}

	// preaccess: the limits, which an lb status path on an lb name skips,
	// in any case. The host is normalized, so already lowercase.
	if !(routes.lbNames[host] && routes.limitExemptLbPaths[strings.ToLower(request.uri.path)]) {
		var routeZone *limitRouteZone
		if location != nil {
			routeZone = location.routeZone
		}
		release, outcome := self.front.limits.acquireRequest(self.frontInterface.limits, request.clientAddrPort.Addr(), routeZone)
		if outcome != limitOutcomeAdmitted {
			self.front.metrics.limitRefusals.WithLabelValues(outcome.String()).Inc()
			exchange.respondError(http.StatusTooManyRequests)
			return
		}
		exchange.response.holdSlot(release)
	}

	// access and content
	if location == nil {
		exchange.respondError(http.StatusNotFound)
		return
	}
	switch location.kind {
	case routeHttpLocationDeny:
		if self.scheme == routeHttpSchemeH3Api {
			// alt's public api answered /status as an unknown path
			exchange.respondError(http.StatusNotFound)
		} else {
			exchange.respondError(http.StatusForbidden)
		}
	case routeHttpLocationLbStatus:
		exchange.respondLbStatus()
	case routeHttpLocationProxy:
		self.serveProxy(exchange, r, request, host, location)
	default:
		exchange.respondError(http.StatusNotFound)
	}
}

// Proxies a request: forwarded to its service's blocks with nginx's request
// contract, or, for connect's own names, served in-process.
func (self *routeHttpHandler) serveProxy(exchange *routeHttpExchange, r *http.Request, request *routeRequest, host string, location *routeHttpLocation) {
	routeService := location.service
	upstreamTarget, streamed := location.upstream(request)
	if location.blockName != "" {
		exchange.response.upstreamBlock = location.blockName
	}
	// nginx's `proxy_set_header Host $host`
	r.Host = host

	if self.front.Draining() && routeHttpUpgradeRequested(r) {
		// a new tunnel would hold this generation for the whole drain
		exchange.respondError(http.StatusServiceUnavailable)
		return
	}
	if location.inProcess {
		self.serveInProcess(exchange, r, request, location, upstreamTarget)
		return
	}

	forwardSettings := self.front.settings.Forward
	bodySettings := self.front.settings.Body
	clientBodyTimeout := bodySettings.ClientBodyTimeout
	clientBodyBufferSize := bodySettings.MemoryPrefixBytes
	if location.requestBody != nil {
		clientBodyTimeout = time.Duration(location.requestBody.ClientBodyTimeoutSeconds) * time.Second
		clientBodyBufferSize = location.requestBody.ClientBodyBufferSize
	}
	readTimeout := forwardSettings.ProxyReadTimeout
	if routeService.service.Websocket || streamed {
		readTimeout = forwardSettings.StreamTimeout
	}
	routeService.forwarder.serveHttp(exchange.writer(), r, forwardOptions{
		interfaceName:        self.frontInterface.limits.name,
		clientAddrPort:       request.clientAddrPort,
		host:                 host,
		blockName:            location.blockName,
		upstreamPath:         upstreamTarget,
		streamed:             streamed,
		websocket:            routeService.service.Websocket,
		clientBodyTimeout:    clientBodyTimeout,
		clientBodyBufferSize: clientBodyBufferSize,
		readTimeout:          readTimeout,
	})
}

// Serves a connect name in-process, as the request reaches a connect block
// through nginx: the ingress headers set from the accepted peer
// (router.SetIngressHeaders, applied once), Early-Data removed, Host
// normalized, and the uri nginx would send upstream. The body is capped as
// nginx caps it. A websocket or H1+ upgrade hijacks through the front's
// writer, which holds the limit slot until the tunnel closes; on h2 the
// writer can't hijack, and the upgrade fields can't be sent, so connect
// answers as it would a request without an upgrade, as it does behind
// nginx, which forwards h2 requests without them.
func (self *routeHttpHandler) serveInProcess(exchange *routeHttpExchange, r *http.Request, request *routeRequest, location *routeHttpLocation, upstreamTarget string) {
	router.SetIngressHeaders(r)
	r.Header.Del("Early-Data")

	path := location.upstreamPath(request)
	upstreamUrl := &url.URL{
		Path:     path,
		RawQuery: request.uri.args,
	}
	if escapedPath := escapeRouteUriPath(path); escapedPath != path {
		upstreamUrl.RawPath = escapedPath
	}
	r.URL = upstreamUrl
	r.RequestURI = upstreamTarget

	writer := exchange.writer()
	if maxBodyBytes := self.front.settings.Body.MaxBodyBytes; 0 < maxBodyBytes && r.Body != nil {
		r.Body = http.MaxBytesReader(writer, r.Body, maxBodyBytes)
	}
	exchange.response.upstreamBlock = routeHttpInProcessBlock
	self.front.connectHandler.ServeHTTP(writer, r)
}

// Whether an h1 request asks to switch protocols: an Upgrade header, and a
// Connection header with the upgrade token. h2 and h3 carry neither.
func routeHttpUpgradeRequested(r *http.Request) bool {
	if r.ProtoMajor != 1 || r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, connection := range r.Header.Values("Connection") {
		for _, token := range strings.Split(connection, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// One request through a front handler: its response, and what the metrics
// and the access log record. Used by the request's handler goroutine.
type routeHttpExchange struct {
	front          *Front
	frontInterface *frontInterface
	r              *http.Request
	response       *responseState
	startTime      time.Time
	// set once the request is validated
	request *routeRequest
	// the service the request is for: its names' server's, or an lb
	// route's; empty when it is for the front itself
	serviceName string
}

// The writer to pass on.
func (self *routeHttpExchange) writer() http.ResponseWriter {
	return self.response.writer()
}

// Answers with the front's own response for a status: text, not nginx's
// html pages (LBALT-IMPL.md §4). On h1, a 400, 413, 414, 500 or 501 closes
// the connection, as nginx's own responses of those statuses do.
func (self *routeHttpExchange) respondError(statusCode int) {
	writer := self.writer()
	header := writer.Header()
	body := http.StatusText(statusCode) + "\n"
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	if self.r.ProtoMajor == 1 {
		switch statusCode {
		case http.StatusBadRequest,
			http.StatusRequestEntityTooLarge,
			http.StatusRequestURITooLong,
			http.StatusInternalServerError,
			http.StatusNotImplemented:
			header.Set("Connection", "close")
		}
	}
	writer.WriteHeader(statusCode)
	writer.Write([]byte(body))
}

// Answers a 301 to location.
func (self *routeHttpExchange) redirect(location string) {
	writer := self.writer()
	header := writer.Header()
	body := http.StatusText(http.StatusMovedPermanently) + "\n"
	header.Set("Location", location)
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(http.StatusMovedPermanently)
	writer.Write([]byte(body))
}

// Answers the lb status route: the status json to GET and HEAD, a 405 to
// any other method, as nginx's static module does.
func (self *routeHttpExchange) respondLbStatus() {
	if self.r.Method != http.MethodGet && self.r.Method != http.MethodHead {
		self.respondError(http.StatusMethodNotAllowed)
		return
	}
	writer := self.writer()
	header := writer.Header()
	header.Set("Content-Type", responseStatusJsonContentType)
	header.Set("Content-Length", strconv.Itoa(len(self.front.lbStatusBody)))
	writer.WriteHeader(http.StatusOK)
	writer.Write(self.front.lbStatusBody)
}

// Lets go of the request's hold on its limit slot and records the request,
// whether its handler returned or panicked.
func (self *routeHttpExchange) end() {
	self.response.releaseSlot()
	self.front.observe(self)
}

// An error for a front that was asked for an interface the route table
// doesn't list.
var errRouteHttpUnknownInterface = errors.New("lbalt front: interface not in the route table")
