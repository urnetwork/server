# sidecar-lb-fixture

The lb in front of evs, as warp generates it, for §22.8's lb drain row (TEST-M2-15, D-TEST-7). A streamed PUT's
no-retry is nginx's own behaviour, so the row runs real nginx on this config rather than a model of it: a fake would
only assert the design back. `server/sidecart3/disruption.go`'s `LbFixture` renders `nginx.conf` with Go's
text/template, starts nginx on it with its own prefix directory, and puts evs's blocks behind it as loopback
upstreams; `LbFixture.Drain` is the disruption.

Its rows: `TestT3LbDrainCutsAStreamedPutWith502` and its buffered twin hold nginx to what it does with a PUT whose
block a drain cuts, and `TestT3LbDrainCutSegmentPutRetriesFromTheClientAndLandsOnce` is the row's client half
(QD-758): an evs world's instances serve as the fixture's upstreams (`fault.EvsWorld.InstanceHandler`), a writer's
segment PUTs go through nginx (`fault.EvsWorldSettings.DeviceTransport`), the first is cut 1 KiB into its body by a
drain that also kills its instance, and evs's client reads nginx's 502 as no answer and sends the same bytes again,
which land once on the sibling.

## Where each line comes from

| fixture | warp |
|---|---|
| the http block's proxy settings, from `keepalive_timeout 1m` to `send_timeout 30s` | `NginxConfig`'s "Http Settings" in `warp/warpctl/config.go`, every lb block's |
| `upstream service-block-evs` with `weight=1 max_fails=0 max_conns=32768` and the keepalive pool | the service-block upstreams in `config.go`, with `services.DefaultKeepalive()` |
| `location /` and its proxy headers, `X-UR-Request-Buffering ""` | a service location's `addProxyRequestHeaders(false)` |
| the nested regex location for `/evs/v1/repos/[^/]+/segments/[^/]+` | `addStreamablePathsLocation` over `warp/services/sidecar-blocks/evs.yml`'s `streamable_paths`, with `addProxyRequestHeaders(true)` and `addStreamableProxy` |

Left out, since the row does not reach them: TLS and the SNI split (`warp/lb`), the stream block, route rate limits
(W2's 429s, which OPS-M2-01's through-nginx test covers), status maps, gzip, h2 and h3. `$warp_client_addr`, a map
warp defines, is `$remote_addr` here.

`sidecart3`'s `TestLbFixtureKeepsWarpsDirectives` reads `warp/warpctl/config.go` and `evs.yml` from the sibling checkout
and fails when a directive this file copies is no longer what warp writes, or evs's streamable path changes, so the
fixture cannot drift from the lb it stands for without a test saying so.

## Running it

The rows that use it (`server/sidecart3/disruption_t3_test.go`) build with `sidecar_disruption` and run in
`sidecar-t3.sh nightly` only: they use real time and real processes (§22.8's beyond-the-schedules rows), never at
merge. The client half also needs the private test stores and the local stack's MinIO (`sidecar/local/stack.sh up`),
as every T3 evs world does. nginx is `LbFixtureSettings.NginxBinary`: `SIDECAR_LB_NGINX` when set, else `nginx` on
`PATH` (homebrew's 1.31.6 on the build host). The fixture binds 127.0.0.1 on ports the OS assigns and refuses one
that could be a production data tunnel's.

Not built here: the plan's `sidecar-test-lb` container. This host's `nginx:stable` image could run the same file, but
its upstreams would have to be reachable from Docker's VM rather than on loopback, which the host binary avoids. The
real-lb runs on production's own lb image stay T6's (PROTO's external list).
