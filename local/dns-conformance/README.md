# dns-conformance

The validators §22.9's DNS row holds the dns edge's answers to (DNS-M5-24; D-DNS-5, R-DNS-10): Unbound, Knot
Resolver and BIND, each anchored to the root's KSK alone, as the anchored monitors are before the DS (§12.6), and
zonemaster over the test root. Each must validate a signed answer, a wildcard answer signed as the queried name, and
compact denial (RFC 9824) of a missing name and of a missing type; anchored to another KSK, none may.

`server/cli/dns/dns_conformance_test.go` runs them. It starts the T3 dns stack (`dns_t3_test.go`: DnsHarness over
dnshost.Store on the private Postgres and Redis), serves one edge on 127.0.0.1 at a port above 50000 beside the
harness's in-memory ones, reads the KSK the edge serves, and then:

| tool | how it runs | here |
|---|---|---|
| BIND's `delv` | the host's binary, `-a` with the KSK as `trusted-keys` and `+root=<root>` | not run: macOS's delv 9.10.6 is built without crypto support, so it loads no key |
| Unbound | `sidecar-test-dns-unbound:local`, a stub zone for the root at the edge, `trust-anchor` the KSK | not run until built |
| Knot Resolver | `sidecar-test-dns-knot-resolver:local`, `policy.STUB` to the edge, `trust_anchors.add` the KSK | not run until built |
| BIND's `named` | `sidecar-test-dns-bind:local`, a forward zone to the edge, `trusted-keys` the KSK | not run until built |
| zonemaster | not built here | not run: it asks port 53 alone, so it runs at T6 over test roots |

Each container is named `sidecar-test-dns-<tool>-<port>`, binds 127.0.0.1 at a port above 50000, reaches the edge
through `host.docker.internal` (its IP read from the container's own `/etc/hosts`), and is removed when its test ends.
The test writes each resolver's config from the templates in the test file, with the root, the edge's port and the
anchor.

`build.sh` builds the three images from `alpine:3.22`. Each build fetches its packages from Alpine's mirrors, so a
person runs it, never an agent of this build; until then each container's test says not run, which is never a pass.
