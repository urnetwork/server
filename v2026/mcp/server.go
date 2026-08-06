package mcp

// mcp server assembly, shared by main and tests.
//
// The transport is stateless streamable http:
//   - no session creation, retention, or Mcp-Session-Id validation; every
//     request is self-contained, so any request can go to any replica behind
//     the lb and deploy rotation cannot strand a session
//   - GET and DELETE return 405; responses are plain application/json
//   - protocol 2026-07-28 requests work with no handshake; older clients
//     negotiate down through per-request temporary sessions
//
// The public endpoint is the root of mcp.bringyour.com (see docs/mcp), so the
// mcp handler stays mounted as a catch-all; /status is the warp health check.

import (
	"net/http"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

const McpVersion = "0.0.1-beta.1"

// Protocol guidance returned to clients at discover/initialize. The threading
// rules are repeated at three levels -- here, in each tool description, and in
// the next_step of every result -- because a caller that has to infer them
// gets them wrong, and the cost of restating them is a few hundred tokens.
const McpInstructions = `URnetwork lets you browse and fetch from anywhere on the network's provider footprint.

Authentication: this server is an OAuth 2.1 protected resource. An unauthenticated request is answered with 401 and a WWW-Authenticate challenge naming the protected resource metadata, which points at the authorization server. Complete the authorization code flow with PKCE and send the resulting access token as an Authorization: Bearer header on every request. Scopes: mcp:read for providerLocations, mcp:fetch for fetch. A tool that needs a scope you were not granted says so; re-authorize requesting that scope in addition to the ones you already hold.

This server keeps no session state between calls. Anything that must persist is returned in the tool result and passed back on the next call. Every result includes a next_step describing exactly what to carry forward. The values are:

1. signed_proxy_id -- the egress that served the request. Pass it back on follow-on calls to keep using the same location instead of opening a new egress client each time. Also keep passing the same location, so the egress can be re-established if it has expired. Reuse guarantees the same location, not the same exit IP.

2. cookies -- the site session (logins, consent). Pass it back unchanged; it is opaque and must not be edited.

3. continuation -- present when a page referenced more resources than fit in one call. Call the same tool again passing continuation to collect the remainder. This is also how a long-running load finishes: each call returns what it completed, and the continuation resumes it.

4. payment_required -- present when the network has hit its plan's concurrent client limit and the limit can be settled by payment. Sign the payment described in that object and repeat the identical call with the additional argument payment set to the signed payment. The upgrade settles inline and the call proceeds. Do not start a separate purchase flow.

Discard these values when you move to an unrelated task; they expire on their own.`

// Creates the mcp server with middleware and tools registered.
func newMcpServer() *mcpsdk.Server {
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "urnetwork-mcp",
		Version: McpVersion,
	}, &mcpsdk.ServerOptions{
		Instructions: McpInstructions,
	})

	// recovery must be first (outermost), see middleware.go
	mcpServer.AddReceivingMiddleware(
		createRecoveryMiddleware(),
		createLoggingMiddleware(),
	)

	registerTools(mcpServer)

	return mcpServer
}

// Wraps the mcp server in a stateless streamable http handler.
func newMcpHandlerFunc(mcpServer *mcpsdk.Server) http.HandlerFunc {
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(
		func(req *http.Request) *mcpsdk.Server {
			return mcpServer
		},
		&mcpsdk.StreamableHTTPOptions{
			Stateless: true,
			// plain json responses; nothing here streams mid-request
			JSONResponse: true,
			// cancel handler work (db calls) when the client goes away
			PropagateRequestCancellation: true,
			// this is a public server behind the tls-terminating lb, not a
			// local server, so dns rebinding protection does not apply. With
			// the protection on, any deployment that proxies to the service
			// over loopback would 403.
			DisableLocalhostProtection: true,
		},
	)

	// every mcp request must present an access token minted for this resource
	// (IDP.md §10). The middleware emits the 401 and the WWW-Authenticate
	// challenge that points a client at the authorization server.
	authorizedHandler := requireAccessToken(mcpHandler)

	return func(w http.ResponseWriter, r *http.Request) {
		// tool handlers only see headers (`req.Extra.Header`). Thread the
		// connection remote address through as a header for the client
		// address fallback in auth.go. Set unconditionally so a caller
		// cannot spoof it.
		r.Header.Set("X-UR-Remote-Addr", r.RemoteAddr)
		authorizedHandler.ServeHTTP(w, r)
	}
}

// The service's http routes: the warp health check plus the mcp endpoint,
// which is mounted as a catch-all because the published endpoint is the root
// of the mcp host (see docs/mcp).
func Routes() []*router.Route {
	mcpHandlerFunc := newMcpHandlerFunc(newMcpServer())

	metadataHandler := protectedResourceMetadataHandler()

	return []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		// rfc 9728: unauthenticated by definition -- it is what a client reads
		// after the 401 to discover where to log in, so it must precede the
		// authenticated catch-all
		router.NewRoute("GET", "/\\.well-known/oauth-protected-resource", metadataHandler.ServeHTTP),
		router.NewRoute("OPTIONS", "/\\.well-known/oauth-protected-resource", metadataHandler.ServeHTTP),
		router.NewRoute("*", ".*", mcpHandlerFunc),
	}
}

// The http server settings the service runs with. With stateless json
// responses the write timeout is also the ceiling on a tool call, which the
// fetch tool budgets itself against.
func HttpServerOptions() server.HttpServerOptions {
	return server.HttpServerOptions{
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  5 * time.Minute,
	}
}
