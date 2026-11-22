package nginx

// The route file: one connect generation's view of what the lb fronts on its
// host (LBALT-IMPL.md §2.1). warpctl renders it, from the inputs that build
// the nginx config, when it starts the connect block, mounts it read-only and
// never rewrites it. Connect reads it once, at start (D5).
//
// The file holds resolved values only, so nothing here applies a services.yml
// default. Its bytes are exactly json.MarshalIndent(table, "", "  ") and a
// newline. That is why every struct below declares its fields in the file's
// order under the file's keys, and why both repos compute the same revision,
// the sha256 of those bytes.
//
// Parsing is strict, so a file from another schema can't parse. A missing,
// null, extra or differently cased key, trailing data, another schema
// version, or a value the front can't serve is refused with an error naming
// the field, and a connect that can't parse its route file fails its start
// rather than come up half-configured. Route prefixes are hidden, so no error
// carries one. The one exception to "missing" is an optional field, tagged
// omitempty: warpctl leaves it out at its zero value, which means off, so a
// file without it has the bytes, and the revision, it had before the field
// existed (rate_limit's identity fields).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The route file schema this package reads. A file of any other version is
// refused.
const RouteSchemaVersion = 1

// DNS bounds on a whole name and on each of its labels, in bytes.
const (
	maxRouteNameBytes  = 253
	maxRouteLabelBytes = 63
)

// One connect generation's route file.
//
// Read-only once parsed: nothing in the package changes a table, so
// concurrent reads, through the methods included, are safe. A parsed table
// has no nil pointer anywhere and every value has passed validation.
type RouteTable struct {
	// always RouteSchemaVersion once parsed
	SchemaVersion int `json:"schema_version"`
	// the warp environment
	Env string `json:"env"`
	// the edge host this generation runs on
	Host string `json:"host"`
	// this generation's container name, reported in X-UR-Front-Generation
	Generation string `json:"generation"`
	// the service domains, the primary first and the rest sorted
	Domains []string `json:"domains"`
	// the services bridge, where every upstream block's port listens
	Upstream *RouteUpstream `json:"upstream"`
	// the lb's own names and hidden route prefixes
	Lb *RouteLb `json:"lb"`
	// exact names whose requests are never logged, a privacy promise
	NoAccessLog []string `json:"no_access_log"`
	// every non-transparent lb interface on the host, lb_alt or not, sorted by
	// name; connect binds them all
	Interfaces []*RouteInterface `json:"interfaces"`
	// this generation's internal ports, the same on every interface address
	Listen *RouteListen `json:"listen"`
	// the services the front treats specially, and the whodis tlds
	Front *RouteFront `json:"front"`
	// the services the nginx generator fronts on this host, sorted by name
	Services []*RouteService `json:"services"`
}

// The services docker network, the bridge every block's external port sits
// on.
type RouteUpstream struct {
	// the network's name, as nginx writes upstream servers: `<host>:<port>`
	// keys nginx's consistent-hash ring, so it is hashed exactly as written
	Host string `json:"host"`
	// the bridge address upstream targets dial, `<ipv4>:<block port>`. The
	// trusted listeners there must stay unreachable from clients.
	Ipv4 string `json:"ipv4"`
}

// The lb's own names, which serve lb status and the hidden per-block routes.
type RouteLb struct {
	// `<env>-lb.<domain>` for each domain, in domain order. The first is the
	// TCP default server: a missing or unknown SNI gets its certificate, and
	// an unknown authority its routes.
	Names []string `json:"names"`
	// "/" + each hidden lb prefix, or [""] when there is none. Secret: never
	// logged.
	RoutePrefixes []string `json:"route_prefixes"`
}

// One lb interface: a public address on the host, DNATed to its own docker
// bridge.
type RouteInterface struct {
	// limit state is keyed by it
	Name string `json:"name"`
	// the interface's lb block, `<host>-<iface>`
	Block string `json:"block"`
	// whether lb_alt is on for the interface. Informational: connect binds
	// every interface either way, so flipping it never changes connect's unit.
	LbAlt bool `json:"lb_alt"`
	// the bridge's IPv4 address, canonical; empty when the family is absent
	Ipv4 string `json:"ipv4"`
	// the bridge's IPv6 address, canonical; empty when the family is absent
	Ipv6 string `json:"ipv6"`
	// the interface's per-address limits
	RateLimit *RouteRateLimit `json:"rate_limit"`
}

// An interface's per-address limits: nginx's shared limit_req and limit_conn
// zones, which are per interface (D8), and the identity limit, which only
// the lb_alt front has (LBALTOPT2 R15).
type RouteRateLimit struct {
	// the shared request rate per address. It may be fractional, since a
	// requests_per_second rate becomes 60 times as many per minute. Zero
	// means no request limit.
	RequestsPerMinute float64 `json:"requests_per_minute"`
	// requests admitted above the rate at once, as nginx's `burst` with
	// `nodelay`
	Burst int `json:"burst"`
	// concurrent requests per address. Each h2 or h3 stream counts, and an
	// upgraded connection holds its slot until it closes. Zero means no cap.
	// Each class of QUIC connection, api and connect, has a cap of this many
	// per address too.
	NetConnections int `json:"net_connections"`
	// CIDR prefixes exempt from every limit
	ExcludeSubnets []string `json:"exclude_subnets"`
	// Optional: the identity limit's request rate per client, or per network
	// for a network token, which turns the identity limit on when positive.
	// Then a request of the shared zone to the api or connect whose bearer
	// token verifies is charged to its identity at this rate and
	// identity_burst, held to net_connections in flight per identity, and
	// charged to its address at requests_per_minute, burst and
	// net_connections times identity_address_factor, a ceiling apart from the
	// shared zone that still bounds one address. Any other request is charged
	// as without it. May be fractional, as requests_per_minute. Zero, or
	// absent, means off, and then identity_burst and identity_address_factor
	// must be zero too.
	IdentityRequestsPerMinute float64 `json:"identity_requests_per_minute,omitempty"`
	// Optional: requests one identity is admitted above its rate at once, as
	// burst
	IdentityBurst int `json:"identity_burst,omitempty"`
	// Optional: how many times one address's limits a verified request's
	// address ceiling is, at least 1 with the identity limit on
	IdentityAddressFactor int `json:"identity_address_factor,omitempty"`
}

// The largest identity_address_factor, so the ceiling's burst and cap stay
// far inside an int.
const maxRouteIdentityAddressFactor = 1000

// This generation's internal ports, from WARP_PORTS. warpctl DNATs each
// interface's public ports to them (LBALT-IMPL.md §2.3).
type RouteListen struct {
	// tcp, service port 80, behind public tcp 80
	HttpPort int `json:"http_port"`
	// tcp, service port 443, behind public tcp 443
	HttpsPort int `json:"https_port"`
	// udp, service port 443, behind public udp 443
	QuicPort int `json:"quic_port"`
	// udp, service port 4053, behind public udp 4053 and 53
	WhodisPort int `json:"whodis_port"`
}

// The services the front serves specially.
type RouteFront struct {
	// the service whose names connect serves in-process on every protocol
	ConnectService string `json:"connect_service"`
	// the service whose names are forwarded, and also admitted on h3 and
	// whodis
	ApiService string `json:"api_service"`
	// the tlds whodis decodes (decode53), each ending in "."
	WhodisTlds []string `json:"whodis_tlds"`
}

// One fronted service.
type RouteService struct {
	// the services.yml key
	Name string `json:"name"`
	// whether the service's names are served
	Exposed bool `json:"exposed"`
	// whether the hidden lb routes, `/by/service/<name>/` and
	// `/by/b/<name>/<block>/`, reach the service
	LbExposed bool `json:"lb_exposed"`
	// the raw services.yml status mode. "standard" denies /status on the
	// service's names.
	Status string `json:"status"`
	// the served names, sorted, in nginx's server_name forms: exact `a.b.c`,
	// or `*.b.c`, which matches one or more leading labels
	Names []string `json:"names"`
	// served names that answer only a redirect, sorted by name
	RedirectAliases []*RouteRedirectAlias `json:"redirect_aliases"`
	// the resolved CORS origins. "*" means wildcard mode; otherwise an exactly
	// matching Origin is echoed, with Vary: Origin.
	CorsOrigins []string `json:"cors_origins"`
	// whether requests may upgrade to a websocket, relayed both ways
	Websocket bool `json:"websocket"`
	// whether every request body streams through unbuffered
	Streamable bool `json:"streamable"`
	// service-relative path patterns, anchored RE2, whose bodies stream
	StreamablePaths []string `json:"streamable_paths"`
	// whether a block is chosen by nginx's consistent hash of the client
	// address, not least_conn
	Stateful bool `json:"stateful"`
	// "/" + each hidden prefix, the top-level ones and then the service's own,
	// or [""] when there is none. Secret: never logged.
	RoutePrefixes []string `json:"route_prefixes"`
	// exact paths with a request zone of their own, sorted by path
	RouteRateLimits []*RouteRateLimitRule `json:"route_rate_limits"`
	// exact paths with body settings of their own, sorted by path
	RouteRequestBodies []*RouteRequestBody `json:"route_request_bodies"`
	// exact paths whose 2xx responses are not logged
	NoSuccessAccessLog []string `json:"no_success_access_log"`
	// the upstream keepalive pool of each block
	Keepalive *RouteKeepalive `json:"keepalive"`
	// the service's blocks, in services.ServiceBlockWeights order
	Blocks []*RouteBlock `json:"blocks"`
}

// A served name that answers `301 <request scheme>://<target><request uri>`,
// as nginx's `return 301 $scheme://<target>$request_uri` does.
type RouteRedirectAlias struct {
	// the served name
	Name string `json:"name"`
	// the host the redirect points at
	Target string `json:"target"`
}

// An exact path whose request zone replaces the interface's shared one
// there. The address's concurrency cap still applies.
type RouteRateLimitRule struct {
	// the exact path, after the service's route prefix
	Path string `json:"path"`
	// may be fractional, as the shared rate's
	RequestsPerMinute float64 `json:"requests_per_minute"`
	// requests admitted above the rate at once, `nodelay`
	Burst int `json:"burst"`
}

// An exact path with body settings of its own, such as a long upload.
type RouteRequestBody struct {
	// the exact path, after the service's route prefix
	Path string `json:"path"`
	// bytes of each body held in memory before the rest spills (D19)
	ClientBodyBufferSize int64 `json:"client_body_buffer_size"`
	// the longest wait between two reads of the body
	ClientBodyTimeoutSeconds int `json:"client_body_timeout_seconds"`
}

// A service's upstream keepalive, per block: nginx's keepalive,
// keepalive_requests, keepalive_time and keepalive_timeout.
type RouteKeepalive struct {
	// idle connections kept per block. Zero means no pool.
	Connections int `json:"connections"`
	// requests one upstream connection serves before it closes
	Requests int `json:"requests"`
	// the longest one upstream connection is reused
	TimeSeconds int `json:"time_seconds"`
	// how long an idle upstream connection is kept
	TimeoutSeconds int `json:"timeout_seconds"`
}

// One block of a service.
type RouteBlock struct {
	// the services.yml block name
	Name string `json:"name"`
	// the block's relative share of new requests
	Weight int `json:"weight"`
	// the block's external port for service port 80, on the upstream bridge
	Port int `json:"port"`
}

// Reads and parses the route file at path. The revision is the lowercase hex
// sha256 of the file's exact bytes: connect reports it in
// X-UR-Front-Route-Revision, and warpctl compares it with the file it
// rendered. On an error both results are empty.
func LoadRouteTable(path string) (routeTable *RouteTable, revision string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("route file: %w", err)
	}
	routeTable, err = ParseRouteTable(raw)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(raw)
	return routeTable, hex.EncodeToString(digest[:]), nil
}

// Decodes and validates a route file, strictly:
//   - the json is one object with nothing after it but whitespace;
//   - schema_version is RouteSchemaVersion;
//   - every field of the schema is present under its exact key and is not
//     null, and no other key appears. encoding/json alone would fill a
//     missing field with its zero value, read null as zero and match keys in
//     any case;
//   - every value is one the front can serve (validate).
//
// A refusal names the field, as json keys and array indexes from the root.
func ParseRouteTable(raw []byte) (*RouteTable, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("route file: empty")
		}
		return nil, fmt.Errorf("route file: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("route file: trailing data after the table")
	}
	object, ok := tree.(map[string]any)
	if !ok {
		return nil, routeFieldErrorf("", "must be a json object")
	}
	// the version first, so a file of another schema is refused for that
	// rather than for a field it renamed
	if schemaVersion, ok := object["schema_version"]; ok && schemaVersion != nil {
		if number, ok := schemaVersion.(json.Number); !ok || number.String() != strconv.Itoa(RouteSchemaVersion) {
			return nil, routeFieldErrorf("schema_version", "must be %d", RouteSchemaVersion)
		}
	}
	if err := checkRouteShape(tree, reflect.TypeFor[RouteTable](), ""); err != nil {
		return nil, err
	}

	routeTable := &RouteTable{}
	tableDecoder := json.NewDecoder(bytes.NewReader(raw))
	tableDecoder.DisallowUnknownFields()
	if err := tableDecoder.Decode(routeTable); err != nil {
		return nil, fmt.Errorf("route file: %w", err)
	}
	if err := routeTable.validate(); err != nil {
		return nil, err
	}
	return routeTable, nil
}

// The service named name, as the table holds it, or false.
func (self *RouteTable) Service(name string) (*RouteService, bool) {
	for _, service := range self.Services {
		if service.Name == name {
			return service, true
		}
	}
	return nil, false
}

// The interface named name, as the table holds it, or false.
func (self *RouteTable) Interface(name string) (*RouteInterface, bool) {
	for _, routeInterface := range self.Interfaces {
		if routeInterface.Name == name {
			return routeInterface, true
		}
	}
	return nil, false
}

// The service named by front.connect_service. Never nil on a parsed table.
func (self *RouteTable) ConnectService() *RouteService {
	service, _ := self.Service(self.Front.ConnectService)
	return service
}

// The service named by front.api_service. Never nil on a parsed table.
func (self *RouteTable) ApiService() *RouteService {
	service, _ := self.Service(self.Front.ApiService)
	return service
}

// The class of the service named serviceName: NameClassConnect for
// front.connect_service, NameClassApi for front.api_service, and
// NameClassForwarded for every other.
func (self *RouteTable) serviceClass(serviceName string) NameClass {
	switch serviceName {
	case self.Front.ConnectService:
		return NameClassConnect
	case self.Front.ApiService:
		return NameClassApi
	default:
		return NameClassForwarded
	}
}

// Every name the front serves, each once, and each needing a certificate:
// the lb names first, in their order (the first is the TCP default server),
// then every exposed service's names and redirect alias names, sorted. A
// service that isn't exposed serves none of them, as nginx emits no server
// block for it and warpctl requests no certificate for it (LBALT-IMPL.md
// §7); the table still lists it for the hidden lb routes. A new slice on
// every call.
func (self *RouteTable) ServedNames() []string {
	seenNames := map[string]bool{}
	servedNames := []string{}
	for _, name := range self.Lb.Names {
		if !seenNames[name] {
			seenNames[name] = true
			servedNames = append(servedNames, name)
		}
	}
	otherNames := []string{}
	addOtherName := func(name string) {
		if !seenNames[name] {
			seenNames[name] = true
			otherNames = append(otherNames, name)
		}
	}
	for _, service := range self.Services {
		if !service.Exposed {
			continue
		}
		for _, name := range service.Names {
			addOtherName(name)
		}
		for _, redirectAlias := range service.RedirectAliases {
			addOtherName(redirectAlias.Name)
		}
	}
	slices.Sort(otherNames)
	return append(servedNames, otherNames...)
}

// Checks every value the front relies on and returns the first refusal. The
// order follows the file, except that front's references to services are
// checked once the services are.
func (self *RouteTable) validate() error {
	if err := checkRouteIdentity("env", self.Env); err != nil {
		return err
	}
	if err := checkRouteIdentity("host", self.Host); err != nil {
		return err
	}
	if err := checkRouteIdentity("generation", self.Generation); err != nil {
		return err
	}

	if len(self.Domains) == 0 {
		return routeFieldErrorf("domains", "must list at least the primary domain")
	}
	for i, domain := range self.Domains {
		if err := checkRouteName(fmt.Sprintf("domains[%d]", i), domain, false); err != nil {
			return err
		}
	}
	if err := checkRouteUnique("domains", self.Domains); err != nil {
		return err
	}

	if err := checkRouteIdentity("upstream.host", self.Upstream.Host); err != nil {
		return err
	}
	if err := checkRouteAddr("upstream.ipv4", self.Upstream.Ipv4, false); err != nil {
		return err
	}

	// each served name routes one way, so it appears once across the lb
	// names, the services' names and the redirect aliases
	servedNamePaths := newRouteValuePaths()

	if len(self.Lb.Names) == 0 {
		return routeFieldErrorf("lb.names", "must list at least one name, the default server")
	}
	for i, name := range self.Lb.Names {
		namePath := fmt.Sprintf("lb.names[%d]", i)
		if err := checkRouteName(namePath, name, false); err != nil {
			return err
		}
		if err := servedNamePaths.add(namePath, name); err != nil {
			return err
		}
	}
	if err := checkRoutePrefixes("lb.route_prefixes", self.Lb.RoutePrefixes); err != nil {
		return err
	}

	for i, name := range self.NoAccessLog {
		if err := checkRouteName(fmt.Sprintf("no_access_log[%d]", i), name, false); err != nil {
			return err
		}
	}
	if err := checkRouteUnique("no_access_log", self.NoAccessLog); err != nil {
		return err
	}

	if len(self.Interfaces) == 0 {
		return routeFieldErrorf("interfaces", "must list at least one interface")
	}
	interfaceNamePaths := newRouteValuePaths()
	interfaceBlockPaths := newRouteValuePaths()
	// each interface has a bridge of its own
	interfaceAddrPaths := newRouteValuePaths()
	for i, routeInterface := range self.Interfaces {
		interfacePath := fmt.Sprintf("interfaces[%d]", i)
		if err := checkRouteIdentity(interfacePath+".name", routeInterface.Name); err != nil {
			return err
		}
		if err := interfaceNamePaths.add(interfacePath+".name", routeInterface.Name); err != nil {
			return err
		}
		if err := checkRouteIdentity(interfacePath+".block", routeInterface.Block); err != nil {
			return err
		}
		if err := interfaceBlockPaths.add(interfacePath+".block", routeInterface.Block); err != nil {
			return err
		}

		if routeInterface.Ipv4 == "" && routeInterface.Ipv6 == "" {
			return routeFieldErrorf(interfacePath, "needs an ipv4 or ipv6 address")
		}
		interfaceAddrs := []*routeAddr{
			{fieldPath: interfacePath + ".ipv4", value: routeInterface.Ipv4, ipv6: false},
			{fieldPath: interfacePath + ".ipv6", value: routeInterface.Ipv6, ipv6: true},
		}
		for _, interfaceAddr := range interfaceAddrs {
			if interfaceAddr.value == "" {
				continue
			}
			if err := checkRouteAddr(interfaceAddr.fieldPath, interfaceAddr.value, interfaceAddr.ipv6); err != nil {
				return err
			}
			// public sockets bind interface addresses; the services bridge
			// holds the trusted listeners, which clients must never reach
			if interfaceAddr.value == self.Upstream.Ipv4 {
				return routeFieldErrorf(interfaceAddr.fieldPath, "must not be the services bridge address, upstream.ipv4")
			}
			if err := interfaceAddrPaths.add(interfaceAddr.fieldPath, interfaceAddr.value); err != nil {
				return err
			}
		}

		rateLimitPath := interfacePath + ".rate_limit"
		rateLimit := routeInterface.RateLimit
		if rateLimit.RequestsPerMinute < 0 {
			return routeFieldErrorf(rateLimitPath+".requests_per_minute", "must not be negative (got %v)", rateLimit.RequestsPerMinute)
		}
		if err := checkRouteCount(rateLimitPath+".burst", rateLimit.Burst); err != nil {
			return err
		}
		if err := checkRouteCount(rateLimitPath+".net_connections", rateLimit.NetConnections); err != nil {
			return err
		}
		for j, excludeSubnet := range rateLimit.ExcludeSubnets {
			if _, err := netip.ParsePrefix(excludeSubnet); err != nil {
				return routeFieldErrorf(fmt.Sprintf("%s.exclude_subnets[%d]", rateLimitPath, j), "must be a cidr prefix")
			}
		}
		if err := checkRouteUnique(rateLimitPath+".exclude_subnets", rateLimit.ExcludeSubnets); err != nil {
			return err
		}
		if err := checkRouteIdentityLimit(rateLimitPath, rateLimit); err != nil {
			return err
		}
	}

	listenPorts := []*routePort{
		{fieldPath: "listen.http_port", port: self.Listen.HttpPort},
		{fieldPath: "listen.https_port", port: self.Listen.HttpsPort},
		{fieldPath: "listen.quic_port", port: self.Listen.QuicPort},
		{fieldPath: "listen.whodis_port", port: self.Listen.WhodisPort},
	}
	for _, listenPort := range listenPorts {
		if err := checkRoutePort(listenPort.fieldPath, listenPort.port); err != nil {
			return err
		}
	}
	if self.Listen.HttpsPort == self.Listen.HttpPort {
		return routeFieldErrorf("listen.https_port", "must differ from listen.http_port, since both are tcp")
	}
	if self.Listen.WhodisPort == self.Listen.QuicPort {
		return routeFieldErrorf("listen.whodis_port", "must differ from listen.quic_port, since both are udp")
	}

	serviceNamePaths := newRouteValuePaths()
	// every block of every service has an external port of its own
	blockPortPaths := newRouteValuePaths()
	for i, service := range self.Services {
		servicePath := fmt.Sprintf("services[%d]", i)
		if err := checkRouteIdentity(servicePath+".name", service.Name); err != nil {
			return err
		}
		if err := serviceNamePaths.add(servicePath+".name", service.Name); err != nil {
			return err
		}
		if service.Status == "" {
			return routeFieldErrorf(servicePath+".status", "must not be empty")
		}

		for j, name := range service.Names {
			namePath := fmt.Sprintf("%s.names[%d]", servicePath, j)
			if err := checkRouteName(namePath, name, true); err != nil {
				return err
			}
			if err := servedNamePaths.add(namePath, name); err != nil {
				return err
			}
		}
		for j, redirectAlias := range service.RedirectAliases {
			redirectAliasPath := fmt.Sprintf("%s.redirect_aliases[%d]", servicePath, j)
			if err := checkRouteName(redirectAliasPath+".name", redirectAlias.Name, true); err != nil {
				return err
			}
			if err := servedNamePaths.add(redirectAliasPath+".name", redirectAlias.Name); err != nil {
				return err
			}
			if err := checkRouteName(redirectAliasPath+".target", redirectAlias.Target, false); err != nil {
				return err
			}
		}

		for j, corsOrigin := range service.CorsOrigins {
			corsOriginPath := fmt.Sprintf("%s.cors_origins[%d]", servicePath, j)
			if corsOrigin == "" {
				return routeFieldErrorf(corsOriginPath, "must not be empty")
			}
			if strings.ContainsFunc(corsOrigin, isRouteSpaceOrControl) {
				return routeFieldErrorf(corsOriginPath, "must not hold whitespace or control characters (got %q)", corsOrigin)
			}
		}
		if err := checkRouteUnique(servicePath+".cors_origins", service.CorsOrigins); err != nil {
			return err
		}

		for j, streamablePath := range service.StreamablePaths {
			if err := checkRouteStreamablePath(fmt.Sprintf("%s.streamable_paths[%d]", servicePath, j), streamablePath); err != nil {
				return err
			}
		}
		if err := checkRouteUnique(servicePath+".streamable_paths", service.StreamablePaths); err != nil {
			return err
		}

		if err := checkRoutePrefixes(servicePath+".route_prefixes", service.RoutePrefixes); err != nil {
			return err
		}

		routeRateLimitPaths := newRouteValuePaths()
		for j, routeRateLimit := range service.RouteRateLimits {
			routeRateLimitPath := fmt.Sprintf("%s.route_rate_limits[%d]", servicePath, j)
			if err := checkRoutePath(routeRateLimitPath+".path", routeRateLimit.Path); err != nil {
				return err
			}
			if err := routeRateLimitPaths.add(routeRateLimitPath+".path", routeRateLimit.Path); err != nil {
				return err
			}
			// a route zone always has a rate: nginx refuses a zero one
			if !(0 < routeRateLimit.RequestsPerMinute) {
				return routeFieldErrorf(routeRateLimitPath+".requests_per_minute", "must be positive (got %v)", routeRateLimit.RequestsPerMinute)
			}
			if err := checkRouteCount(routeRateLimitPath+".burst", routeRateLimit.Burst); err != nil {
				return err
			}
		}

		routeRequestBodyPaths := newRouteValuePaths()
		for j, routeRequestBody := range service.RouteRequestBodies {
			routeRequestBodyPath := fmt.Sprintf("%s.route_request_bodies[%d]", servicePath, j)
			if err := checkRoutePath(routeRequestBodyPath+".path", routeRequestBody.Path); err != nil {
				return err
			}
			if err := routeRequestBodyPaths.add(routeRequestBodyPath+".path", routeRequestBody.Path); err != nil {
				return err
			}
			// resolved values: an unset one would have been rendered as the default
			if routeRequestBody.ClientBodyBufferSize <= 0 {
				return routeFieldErrorf(routeRequestBodyPath+".client_body_buffer_size", "must be positive (got %d)", routeRequestBody.ClientBodyBufferSize)
			}
			if routeRequestBody.ClientBodyTimeoutSeconds <= 0 {
				return routeFieldErrorf(routeRequestBodyPath+".client_body_timeout_seconds", "must be positive (got %d)", routeRequestBody.ClientBodyTimeoutSeconds)
			}
		}

		for j, noSuccessAccessLogPath := range service.NoSuccessAccessLog {
			if err := checkRoutePath(fmt.Sprintf("%s.no_success_access_log[%d]", servicePath, j), noSuccessAccessLogPath); err != nil {
				return err
			}
		}
		if err := checkRouteUnique(servicePath+".no_success_access_log", service.NoSuccessAccessLog); err != nil {
			return err
		}

		keepalivePath := servicePath + ".keepalive"
		keepaliveCounts := []*routeCount{
			{fieldPath: keepalivePath + ".connections", count: service.Keepalive.Connections},
			{fieldPath: keepalivePath + ".requests", count: service.Keepalive.Requests},
			{fieldPath: keepalivePath + ".time_seconds", count: service.Keepalive.TimeSeconds},
			{fieldPath: keepalivePath + ".timeout_seconds", count: service.Keepalive.TimeoutSeconds},
		}
		for _, keepaliveCount := range keepaliveCounts {
			if err := checkRouteCount(keepaliveCount.fieldPath, keepaliveCount.count); err != nil {
				return err
			}
		}

		blockNamePaths := newRouteValuePaths()
		for j, block := range service.Blocks {
			blockPath := fmt.Sprintf("%s.blocks[%d]", servicePath, j)
			if err := checkRouteIdentity(blockPath+".name", block.Name); err != nil {
				return err
			}
			if err := blockNamePaths.add(blockPath+".name", block.Name); err != nil {
				return err
			}
			if err := checkRouteCount(blockPath+".weight", block.Weight); err != nil {
				return err
			}
			if err := checkRoutePort(blockPath+".port", block.Port); err != nil {
				return err
			}
			if err := blockPortPaths.add(blockPath+".port", strconv.Itoa(block.Port)); err != nil {
				return err
			}
		}
	}

	if _, ok := self.Service(self.Front.ConnectService); !ok {
		return routeFieldErrorf("front.connect_service", "must name a service in services (got %q)", self.Front.ConnectService)
	}
	if _, ok := self.Service(self.Front.ApiService); !ok {
		return routeFieldErrorf("front.api_service", "must name a service in services (got %q)", self.Front.ApiService)
	}
	if self.Front.ApiService == self.Front.ConnectService {
		return routeFieldErrorf("front.api_service", "must differ from front.connect_service")
	}
	if len(self.Front.WhodisTlds) == 0 {
		return routeFieldErrorf("front.whodis_tlds", "must list at least one tld")
	}
	for i, whodisTld := range self.Front.WhodisTlds {
		whodisTldPath := fmt.Sprintf("front.whodis_tlds[%d]", i)
		name, ok := strings.CutSuffix(whodisTld, ".")
		if !ok {
			return routeFieldErrorf(whodisTldPath, "must end with . (got %q)", whodisTld)
		}
		if err := checkRouteName(whodisTldPath, name, false); err != nil {
			return err
		}
	}
	if err := checkRouteUnique("front.whodis_tlds", self.Front.WhodisTlds); err != nil {
		return err
	}

	return nil
}

// A route file refused at one field.
type routeFieldError struct {
	// json keys and array indexes from the root, as
	// `services[0].blocks[1].port`; empty for the root itself
	fieldPath string
	message   string
}

// A refusal at fieldPath, its message formatted as fmt.Sprintf does.
func routeFieldErrorf(fieldPath string, format string, a ...any) error {
	return &routeFieldError{
		fieldPath: fieldPath,
		message:   fmt.Sprintf(format, a...),
	}
}

// `route file: <field path>: <message>`, or without the path for the root.
func (self *routeFieldError) Error() string {
	if self.fieldPath == "" {
		return fmt.Sprintf("route file: %s", self.message)
	}
	return fmt.Sprintf("route file: %s: %s", self.fieldPath, self.message)
}

// Checks a rate_limit's identity limit fields (RouteRateLimit): none
// negative; with identity_requests_per_minute zero, the identity limit is
// off and the other two must be zero; with it on, identity_address_factor is
// 1 through maxRouteIdentityAddressFactor, and the ceiling it makes of burst
// and net_connections fits an int.
func checkRouteIdentityLimit(rateLimitPath string, rateLimit *RouteRateLimit) error {
	if rateLimit.IdentityRequestsPerMinute < 0 {
		return routeFieldErrorf(rateLimitPath+".identity_requests_per_minute", "must not be negative (got %v)", rateLimit.IdentityRequestsPerMinute)
	}
	if err := checkRouteCount(rateLimitPath+".identity_burst", rateLimit.IdentityBurst); err != nil {
		return err
	}
	if err := checkRouteCount(rateLimitPath+".identity_address_factor", rateLimit.IdentityAddressFactor); err != nil {
		return err
	}
	if rateLimit.IdentityRequestsPerMinute == 0 {
		if rateLimit.IdentityBurst != 0 {
			return routeFieldErrorf(rateLimitPath+".identity_burst", "must be zero without identity_requests_per_minute (got %d)", rateLimit.IdentityBurst)
		}
		if rateLimit.IdentityAddressFactor != 0 {
			return routeFieldErrorf(rateLimitPath+".identity_address_factor", "must be zero without identity_requests_per_minute (got %d)", rateLimit.IdentityAddressFactor)
		}
		return nil
	}
	factor := rateLimit.IdentityAddressFactor
	if factor < 1 || maxRouteIdentityAddressFactor < factor {
		return routeFieldErrorf(rateLimitPath+".identity_address_factor", "must be in 1-%d with identity_requests_per_minute (got %d)", maxRouteIdentityAddressFactor, factor)
	}
	if math.MaxInt/factor < rateLimit.Burst || math.MaxInt/factor < rateLimit.NetConnections {
		return routeFieldErrorf(rateLimitPath+".identity_address_factor", "makes burst or net_connections overflow (got %d)", factor)
	}
	return nil
}

// Checks decoded json against the shape of valueType, before the typed
// decode: an object for each struct, with every field under its exact key and
// no other key, an array for each slice, and no null anywhere. Scalar types
// are left to the typed decode. A field tagged omitempty may be absent, never
// null.
func checkRouteShape(value any, valueType reflect.Type, fieldPath string) error {
	if valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	switch valueType.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return routeFieldErrorf(fieldPath, "must be an object")
		}
		fieldNameTypes := map[string]reflect.Type{}
		fieldNames := []string{}
		optionalFieldNames := map[string]bool{}
		for i := range valueType.NumField() {
			field := valueType.Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if !field.IsExported() || name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fieldNameTypes[name] = field.Type
			fieldNames = append(fieldNames, name)
			if slices.Contains(strings.Split(options, ","), "omitempty") {
				optionalFieldNames[name] = true
			}
		}
		// an extra key first, since a misspelled or recased key also leaves
		// its field missing
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if _, ok := fieldNameTypes[key]; !ok {
				return routeFieldErrorf(joinRouteFieldPath(fieldPath, key), "is not a field of schema version %d", RouteSchemaVersion)
			}
		}
		for _, name := range fieldNames {
			childPath := joinRouteFieldPath(fieldPath, name)
			childValue, ok := object[name]
			if !ok {
				if optionalFieldNames[name] {
					continue
				}
				return routeFieldErrorf(childPath, "is missing")
			}
			if childValue == nil {
				return routeFieldErrorf(childPath, "must not be null")
			}
			if err := checkRouteShape(childValue, fieldNameTypes[name], childPath); err != nil {
				return err
			}
		}
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return routeFieldErrorf(fieldPath, "must be an array")
		}
		for i, element := range array {
			elementPath := fmt.Sprintf("%s[%d]", fieldPath, i)
			if element == nil {
				return routeFieldErrorf(elementPath, "must not be null")
			}
			if err := checkRouteShape(element, valueType.Elem(), elementPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// The path of key inside the object at fieldPath.
func joinRouteFieldPath(fieldPath string, key string) string {
	if fieldPath == "" {
		return key
	}
	return fieldPath + "." + key
}

// The first field path at which each value appeared, to refuse a repeat.
type routeValuePaths struct {
	valuePaths map[string]string
}

// An empty record.
func newRouteValuePaths() *routeValuePaths {
	return &routeValuePaths{
		valuePaths: map[string]string{},
	}
}

// Records value at fieldPath, refusing one already recorded. The refusal
// names both fields and never the value, so it is safe for secret values.
func (self *routeValuePaths) add(fieldPath string, value string) error {
	if firstPath, ok := self.valuePaths[value]; ok {
		return routeFieldErrorf(fieldPath, "duplicates %s", firstPath)
	}
	self.valuePaths[value] = fieldPath
	return nil
}

// An address field and its family, for checking in a loop.
type routeAddr struct {
	fieldPath string
	value     string
	ipv6      bool
}

// A port field, for checking in a loop.
type routePort struct {
	fieldPath string
	port      int
}

// A count field, for checking in a loop.
type routeCount struct {
	fieldPath string
	count     int
}

// Refuses a repeated entry of a list.
func checkRouteUnique(fieldPath string, values []string) error {
	valuePaths := newRouteValuePaths()
	for i, value := range values {
		if err := valuePaths.add(fmt.Sprintf("%s[%d]", fieldPath, i), value); err != nil {
			return err
		}
	}
	return nil
}

// Checks an identity the front routes or keys state by: an env, host,
// generation, upstream network, interface, block or service name. It is
// non-empty and holds only letters, digits, '.', '-' and '_', so it is safe
// in a path segment, a hash key and a header.
func checkRouteIdentity(fieldPath string, value string) error {
	if value == "" {
		return routeFieldErrorf(fieldPath, "must not be empty")
	}
	for _, r := range value {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '.' || r == '-' || r == '_') {
			return routeFieldErrorf(fieldPath, "may hold only letters, digits, '.', '-' and '_' (got %q)", value)
		}
	}
	return nil
}

// Checks a name in nginx's server_name forms: exact, `a.b.c`, or when
// allowWildcard, a single leading wildcard label, `*.b.c`. Labels are
// lowercase letters, digits, '-' and '_', with no empty label (so no trailing
// dot), within DNS lengths.
func checkRouteName(fieldPath string, name string, allowWildcard bool) error {
	exactName, wildcard := strings.CutPrefix(name, "*.")
	if wildcard && !allowWildcard {
		return routeFieldErrorf(fieldPath, "must be an exact name, not a wildcard (got %q)", name)
	}
	if exactName == "" {
		return routeFieldErrorf(fieldPath, "must name a host (got %q)", name)
	}
	if maxRouteNameBytes < len(exactName) {
		return routeFieldErrorf(fieldPath, "must be at most %d bytes", maxRouteNameBytes)
	}
	for _, label := range strings.Split(exactName, ".") {
		if label == "" {
			return routeFieldErrorf(fieldPath, "has an empty label (got %q)", name)
		}
		if maxRouteLabelBytes < len(label) {
			return routeFieldErrorf(fieldPath, "has a label over %d bytes", maxRouteLabelBytes)
		}
		for _, r := range label {
			if !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '-' || r == '_') {
				return routeFieldErrorf(fieldPath, "must be dot-separated labels of lowercase letters, digits, '-' and '_' (got %q)", name)
			}
		}
	}
	return nil
}

// Checks an address of one family, written canonically (netip.Addr.String),
// so connect's readiness report names it exactly as warpctl rendered it. An
// IPv4-mapped IPv6 address and a zone are refused.
func checkRouteAddr(fieldPath string, value string, ipv6 bool) error {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return routeFieldErrorf(fieldPath, "must be an ip address (got %q)", value)
	}
	if ipv6 {
		if !addr.Is6() || addr.Is4In6() {
			return routeFieldErrorf(fieldPath, "must be an ipv6 address (got %q)", value)
		}
	} else if !addr.Is4() {
		return routeFieldErrorf(fieldPath, "must be an ipv4 address (got %q)", value)
	}
	if addr.Zone() != "" {
		return routeFieldErrorf(fieldPath, "must not carry a zone (got %q)", value)
	}
	if addr.String() != value {
		return routeFieldErrorf(fieldPath, "must be written %s (got %q)", addr.String(), value)
	}
	return nil
}

// Checks a tcp or udp port: 1 through 65535.
func checkRoutePort(fieldPath string, port int) error {
	if port < 1 || math.MaxUint16 < port {
		return routeFieldErrorf(fieldPath, "must be in 1-%d (got %d)", math.MaxUint16, port)
	}
	return nil
}

// Checks a count, a weight or a duration in seconds: zero or more.
func checkRouteCount(fieldPath string, count int) error {
	if count < 0 {
		return routeFieldErrorf(fieldPath, "must not be negative (got %d)", count)
	}
	return nil
}

// Checks an exact route path: it starts with / and holds no whitespace or
// control character.
func checkRoutePath(fieldPath string, routePath string) error {
	if !strings.HasPrefix(routePath, "/") {
		return routeFieldErrorf(fieldPath, "must start with / (got %q)", routePath)
	}
	if strings.ContainsFunc(routePath, isRouteSpaceOrControl) {
		return routeFieldErrorf(fieldPath, "must not hold whitespace or control characters (got %q)", routePath)
	}
	return nil
}

// Whether r can't appear in a route path or an origin: whitespace or a
// control character.
func isRouteSpaceOrControl(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// Checks a route_prefixes list in nginx's shape: "/" + each hidden prefix, or
// [""] when there is none. The values are secret, so no refusal names one.
func checkRoutePrefixes(fieldPath string, routePrefixes []string) error {
	if len(routePrefixes) == 0 {
		return routeFieldErrorf(fieldPath, "must list at least one prefix, [\"\"] when there is none")
	}
	for i, routePrefix := range routePrefixes {
		if routePrefix != "" && !strings.HasPrefix(routePrefix, "/") {
			return routeFieldErrorf(fmt.Sprintf("%s[%d]", fieldPath, i), "must be empty or start with /")
		}
	}
	return checkRouteUnique(fieldPath, routePrefixes)
}

// Checks a streamable_paths pattern by warp's rule
// (services.ValidateStreamablePath): it starts with /, holds no control
// character, and compiles as RE2 when anchored, `^(?:<pattern>)$`.
func checkRouteStreamablePath(fieldPath string, streamablePath string) error {
	if !strings.HasPrefix(streamablePath, "/") {
		return routeFieldErrorf(fieldPath, "must start with / (got %q)", streamablePath)
	}
	for _, r := range streamablePath {
		if r < 0x20 || r == 0x7f {
			return routeFieldErrorf(fieldPath, "must not hold a control character (got %q)", streamablePath)
		}
	}
	if _, err := regexp.Compile("^(?:" + streamablePath + ")$"); err != nil {
		return routeFieldErrorf(fieldPath, "must compile as an anchored regular expression: %s", err)
	}
	return nil
}
