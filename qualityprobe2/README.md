# Provider quality probes

This is the in-tree copy of the former `operator-proxy` Go packages. Taskworker,
the monitor, and the operator-facing API use these packages through
`github.com/urnetwork/server/qualityprobe/...`; the standalone
`operator-proxy` checkout remains in place for history, but it is not a Server
or build/all runtime dependency.

`providertunnel` opens a private userspace TUN pinned to one provider. Probe
traffic, including its DNS lookups, uses that TUN. `controlplane` owns the
separate IPv4-only API and Connect bootstrap clients. Both use the per-instance
server settings in `server/sdk.go`; neither changes Connect's process-global
address-family policy. Main's inner provider path is IPv4-only until its
data-center routing supports IPv6.

The long-running fleet is scheduled by `server/taskworker/work`; the standalone
diagnostic command is at `qualityprobe/cmd/egress-prober`. Run the in-tree tests
from the Server module with `go test ./qualityprobe/...`.
