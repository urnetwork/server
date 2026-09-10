# The URnetwork MCP server on MCP 2026-07-28 (stateless)

The MCP server is stateless. It holds no per-caller state between requests, so
any request can land on any replica behind the load balancer and a deploy
rotation cannot strand a session.

This document is the design for that. Authorization is a separate concern with
its own document — see `server/IDP.md`; this one covers the transport, the tool
surface, and the state-threading protocol that replaces sessions.

## 1. What changed in the spec

The 2026-07-28 revision turns MCP from a bidirectional stateful protocol into a
request/response one. The parts that shape this server:

| Change | Effect here |
|---|---|
| The `initialize` / `notifications/initialized` handshake and the protocol-level session are removed | Each request carries its own protocol version, client identity, and capabilities in `_meta`; nothing is negotiated once and remembered |
| `server/discover` replaces handshake-time capability exchange | Optional — a request works without it |
| Multi Round-Trip Requests replace server-initiated `elicitation/create`, `sampling/createMessage`, `roots/list` | We issue none of these, so nothing to migrate |
| `Mcp-Method` / `Mcp-Name` headers mirror routing fields out of the JSON body | Gateways can route and meter without parsing bodies |
| `tools/list` gains `ttlMs` / `cacheScope` | Our tool list is static |
| Roots, sampling, logging, and the legacy HTTP+SSE transport are deprecated | Not used |

The SDK (`github.com/modelcontextprotocol/go-sdk` v1.7.0) only speaks
`2026-07-28` when `StreamableHTTPOptions.Stateless` is true. Stateless mode is
the gateway to the new protocol, not merely an optimization. Older clients
still negotiate down — v1.7.0 supports `2025-11-25` back through `2024-11-05`.

## 2. Transport configuration

`newMcpHandlerFunc` (server.go) sets four options, each load-bearing:

```go
&mcpsdk.StreamableHTTPOptions{
    Stateless:                    true,
    JSONResponse:                 true,
    PropagateRequestCancellation: true,
    DisableLocalhostProtection:   true,
}
```

**`Stateless`** — no session creation, retention, or `Mcp-Session-Id`
validation. `GET` and `DELETE` return 405. This is what removes the failure
class where a session created on one replica 404s on another, and where every
live session breaks mid-conversation on deploy.

**`JSONResponse`** — plain `application/json` instead of SSE framing. Nothing
here streams mid-request, so SSE would be pure overhead and would interact
badly with load-balancer buffering.

*Consequence worth knowing:* with `JSONResponse`, a non-response message has
its related-request cleared and is routed to the standalone SSE stream
(`streamable.go:1821`), which stateless mode does not offer. **Progress
notifications are therefore dropped.** There is no mid-call channel to the
client. That is why long work uses caller-driven continuation (§5) rather than
a heartbeat.

**`PropagateRequestCancellation`** — ties the handler context to the HTTP
request, so a client that disconnects cancels the database work it started.
Only applies at protocol `>= 2026-07-28`, where the POST is the whole request
lifecycle.

**`DisableLocalhostProtection`** — this is a public server behind a
TLS-terminating load balancer, not a local one. With the protection on, any
deployment that proxies to the service over loopback would 403.

Timeouts (`HttpServerOptions`): read 15s, write 30s, idle 5m. With stateless
JSON, the write timeout is also the ceiling on a tool call, which the fetch
tool budgets itself against.

## 3. Package layout

Logic lives in `server/mcp`; `server/cli/mcp` is a thin `package main` that
calls `mcp.Routes()` and listens — matching `api`/`cli/api`,
`connect`/`cli/connect`, `proxy`/`cli/proxy`.

The SDK is imported as `mcpsdk` so this package can own the name `mcp`.

| File | Holds |
|---|---|
| `server.go` | server assembly, `Routes()`, `HttpServerOptions()`, the server-level `Instructions` |
| `middleware.go` | panic recovery and request logging |
| `auth.go` | OAuth resource-server wiring (see `server/IDP.md`) |
| `tools.go` | tool registration and `providerLocations` |
| `fetch.go` | the `fetch` tool |
| `proxy.go` | egress proxy acquisition and reuse |
| `resources.go` | static resource discovery and target validation |
| `seal.go` | sealed opaque blobs for threaded state |

## 4. Middleware

Two, and the order matters: `AddReceivingMiddleware(recovery, logging)` applies
as `recovery(logging(handler))`.

**Recovery must be outermost.** The SDK dispatches method handlers on their own
jsonrpc goroutines and has **no `recover()` anywhere** in its `mcp` or
`jsonrpc2` packages. An unrecovered handler panic therefore exits the process —
the HTTP router's recover never sees it, because it is on a different
goroutine. Context-done raises (the standard model raise pattern) pass through
quietly as cancellation.

**Logging** correlates on method plus tool name. The stateless transport uses a
throwaway session per request, so there is no session id worth logging. The
request line is behind `glog.V(1)`; the response line always emits. It also
inspects `CallToolResult.IsError`, because the SDK embeds tool execution errors
in a successful result rather than returning them — without that check a failed
tool call logs as `ok`.

## 5. State threading: the protocol that replaces sessions

Statelessness moves per-caller state to the caller. Three values travel in the
tool result and come back as arguments:

| Value | Carries | Lifetime |
|---|---|---|
| `signed_proxy_id` | the egress that served the request | until the proxy is removed |
| `cookies` | the site session (logins, consent) | 6h seal |
| `continuation` | resources that did not fit in the call budget | 6h seal |

**The signed proxy id is the real one** (`model.SignProxyId`) — the same value
that is both the HTTPS proxy hostname label and the `Proxy-Authorization`
token. It is a bearer credential and is deliberately not IP-locked, matching
the default for proxies created through the API.

**Reuse guarantees the same location, not the same exit IP.** Provider
selection rebalances underneath a proxy id, so callers must not depend on IP
stability. A stale handle re-mints at the same location when a location is
available.

**The cookie jar and continuation are sealed** (`seal.go`): AES-256-GCM under a
key derived from the `proxy.yml` vault secret with a distinct label, so no new
vault entry is needed and the two uses cannot collide. Each blob is bound to a
label — a jar cannot be replayed as a continuation — and carries an expiry.
Sealing matters because these land in model context, host logs, and context
summaries; a cookie jar is a credential.

**The call budget makes the timeout a pagination boundary.** `fetchCallBudget`
is 20s, inside the 30s write timeout. Work that does not fit returns what
completed plus a continuation. Because there is no mid-call channel (§2), the
caller calling again *is* the heartbeat.

## 6. Telling the caller how to thread

A caller that has to infer the protocol gets it wrong, so it is stated at four
levels, cheapest to most reliable:

1. **`ServerOptions.Instructions`** — returned at `server/discover` and
   `initialize`. The whole protocol, including the payment retry.
2. **Tool `Description`** — the same rules scoped to that tool.
3. **Per-field `jsonschema` tags** — each threaded field says "pass back the
   value from the previous result".
4. **`next_step` in every result** — the same instruction restated against the
   values actually produced.

The fourth is the one callers follow most reliably, because it sits in the
result they just read rather than in a schema they saw earlier. It is echoed
both as a text content block and in the structured output.

## 7. Tools

Both carry a `title` and annotations, which the Claude Connectors Directory
requires.

### providerLocations

Read-only, idempotent, closed-world. Requires the `mcp:read` scope. Returns a
typed `ProviderLocationsResult` object — not a bare slice, so the SDK advertises
an output schema and `structuredContent` is an object as the spec requires. The
JSON is also mirrored into a text block, so hosts that surface only text still
deliver the data to the model.

### fetch

`<method> <url> from <location>`, optionally with the page's static resources.
Not read-only (it provisions a billed egress client), not destructive (it only
creates), open-world. Requires `mcp:fetch`.

**Egress** goes through `model.AuthNetworkClient` — the same path
`/network/auth-client` uses — so the plan's concurrent-client limit, the pro
feature gates, and the `UpgradeRequired` signal all behave identically here.

**The proxy dial is IPv4-only.** A custom `DialContext` rewrites `tcp`/`tcp6`
to `tcp4`. This governs only the server-to-server hop: with a proxy configured
the transport dials the proxy and tunnels via CONNECT, so the target host is
resolved at the egress location and is unaffected.

**Resource discovery is static.** The HTML is parsed for element references
(`img`, `source`, `video`/`audio`, `script`, `link`, Open Graph images), with
no script execution — so SPA content and lazy-loaded media are invisible. This
is stated in the tool description rather than left for the caller to discover.

**Content blocks are typed to their kind**: text bodies as `TextContent`,
images and audio as `ImageContent`/`AudioContent` so hosts render them natively
and vision models can actually see them, everything else as
`EmbeddedResource`, and anything too large or unfetched as `ResourceLink` with
its size. Nothing is silently dropped.

**Target validation** refuses non-HTTP schemes and, by default, loopback,
private, link-local, and metadata addresses. Only literal addresses are
checked: a hostname is resolved at the egress location, so resolving it here
would describe a different network. Validation happens **before** the egress is
acquired, so a request that will be refused never provisions a billed client.

### x402 payment, in band

A tool call has no HTTP status, so a `402` cannot be returned. When
`AuthNetworkClient` reports `UpgradeRequired`, the tool returns an error result
whose `structuredContent` carries the terms from
`controller.X402PaymentRequiredFor`, and accepts an optional `payment`
argument. The agent signs the payment and repeats the identical call; the
upgrade settles inline via `controller.X402Purchase` and the fetch proceeds.
The whole round trip stays inside MCP.

## 8. Authorization

The server is an OAuth 2.1 resource server. Network JWTs and `urn_` API keys
are **not** accepted — the spec requires a server to accept only tokens issued
by its own authorization server. Scopes gate tools: `mcp:read` at the transport
(the floor), `mcp:fetch` inside the tool. The RFC 9728 protected resource
metadata is served unauthenticated ahead of the authenticated catch-all,
because it is what a client reads after the 401.

Full design, including the signing-key separation that keeps an OAuth token
from verifying as a platform credential: `server/IDP.md`.

## 9. Testing

`startTestServer` serves the production `Routes()` through the real router —
not the bare handler — so the route table, the metadata route, and the auth
wrapping are all exercised. An earlier version mounted the handler directly and
silently tested none of them.

The end-to-end tests stand up the real stack in-process — connect exchange and
handler, the full `api.Routes()`, a provider built from connect primitives, and
the real proxy ingress — plus an `httptest` web server, and drive `fetch`
through an MCP client over streamable HTTP. They cover the page body, static
discovery, image blocks, cookie threading (asserting the site sees the session
with the jar and not without), auth enforcement, audience rejection, scope
enforcement, and SSRF refusal.

**Provider egress blocks loopback and private ranges by default**
(`connect/ip_security.go`, `isPublicUnicast`). Reaching a local test server
requires disabling the security policy at *two* inspection points — the
provider's NAT settings and the proxy device manager's client policy — because
the provider evaluates the reversed client policy.

## 10. Known gaps

- **No mid-call progress channel** (§2). Long work uses continuation. If that
  ever needs to change, the spec's answer is the Tasks extension
  (`io.modelcontextprotocol/tasks`), which is **not implemented in go-sdk
  v1.7.0** — there are no task files and no `tasks/` methods.
- **Per-tool scope failures are not a real challenge.** The transport returns
  `403` with `WWW-Authenticate: insufficient_scope` for the floor scope, but a
  per-tool failure can only be a tool result, so `fetch` returns text naming
  the scope to request.
- **Cookie scoping is per origin.** The jar is keyed by origin rather than
  replayed verbatim, which loses cross-subdomain nuance but keeps the sealed
  blob small and predictable.
- **`McpResource` is a package var**, not config. A second environment would
  need it from config, and it must match what the load balancer serves.
- **The x402 path has never executed end to end** — x402 is disabled in the
  local vault, so the `payment_required` → `payment` retry is unverified.
