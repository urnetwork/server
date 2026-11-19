// Package nginx is the lb_alt front: the nginx compatibility code connect
// runs when it serves an edge interface's public ports itself (LBALT.md §4.5,
// LBALT-IMPL.md §2.7). It reproduces what the warp lb image's nginx does for
// every fronted name, so the parity harness can check it against the pinned
// image.
//
// Data flows in one direction, and each stage is built once per process:
//
//   - The route table (route.go) is the file warpctl renders for one connect
//     generation. LoadRouteTable reads it at start, with its revision (the
//     sha256 of its bytes). It never changes after that: a route change takes
//     effect on the next deploy (D5).
//   - The certificate store (tls.go) loads a certificate and key for every
//     name the table serves, by warpctl's findLatestTls rule, before the block
//     reports ready (D6).
//   - The Front (front.go) is built from the table and the store. It owns all
//     mutable front state: the name matcher (name.go), the two long-lived TLS
//     configs, the upstream pools (upstream.go, forward.go), the body store
//     (body.go) and the limit state (D8). One Front serves every interface.
//   - The per-interface handlers come from the Front: HttpsHandler for TCP
//     443, HttpHandler for TCP 80, and H3ApiHandler for api names on h3.
//     Connect binds the sockets and dispatches QUIC by ClassifyName; the
//     handlers do everything per request: the ingress rewrite, the limits
//     (keyed by interface name and normalized client IP), body handling,
//     routing by authority, forwarding with retries, and the added response
//     headers.
//
// Connect names never leave the process: the Front wraps connect's own
// handler with the ingress rewrite and limits. Every other fronted name is
// forwarded as plaintext HTTP/1.1 to its service's blocks, with nginx's
// request contract (§4.2).
//
// Ownership: the package keeps no process-wide state. Every budget, pool and
// limit belongs to one Front and the FrontSettings it was built with, and
// Close releases them.
//
// Goroutine safety: package-level functions are safe for concurrent use. A
// RouteTable and a CertificateStore are read-only once loaded, so concurrent
// reads of them are safe. Front's methods are safe for concurrent use, as
// front.go documents, and so are the handlers they return. Other types are
// not, unless their doc says so.
//
// Diagnostics carry no client address and no lb hidden route prefix: no log,
// metric label or error body contains either.
//
// The package must not import server/connect, server/alt or server/api.
package nginx
