# Provider quality probes

Taskworker, the monitor, and the operator-facing API use these packages through
`github.com/urnetwork/server/qualityprobe/...`. They came, with their history,
from the retired `operator-proxy` repository (imported 2026-09-26), which no
Server service or build/all runtime depends on.

`providertunnel` opens a private userspace TUN pinned to one provider. Probe
traffic, including its DNS lookups, uses that TUN. `controlplane` owns the
separate IPv4-only API and Connect bootstrap clients. Both use the per-instance
server settings in `server/sdk.go`; neither changes Connect's process-global
address-family policy. Main's inner provider path is IPv4-only until its
data-center routing supports IPv6.

The long-running fleet is scheduled by `server/taskworker/work`; the standalone
diagnostic command is at `qualityprobe/cmd/egress-prober`. Run the in-tree tests
from the Server module with `go test ./qualityprobe/...`.
