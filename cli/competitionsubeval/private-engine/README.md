# Private epoch-two engine templates

These are inert, uninstalled inputs for a reviewed private-engine harness. They do
not stop, restart, reconfigure, share state with, or prune the user's Docker daemon.
No unit is enabled by this source change.

Render every `@TOKEN@` explicitly, reject unresolved tokens, hash the resulting
root-owned files and executables, and put the resulting pins in `boundary.json`.
`BASE_DIRECTORY` is the requested logical runtime base (on the remote evaluation
host, `/home/by/sim-latency`). It may already be backed by an operator-provisioned
data volume. Agent journals, public client sockets and `control/` remain outside
`candidate/`. Configuration belongs in a root-protected directory under `/etc`.

Provisioning prerequisites, performed only under a separately reviewed activation
plan:

- Use the explicit locked worker account `urnetwork-eval-worker` and separate
  control account `urnetwork-eval-control`; neither receives generic sudo, Docker
  group membership, the private engine socket, or coordinator secrets.
- Choose a separate remap account and reviewed, non-overlapping `/etc/subuid` and
  `/etc/subgid` ranges. Do not allocate ranges from these templates. Record
  `CONTAINER_ROOT_UID`, `CONTAINER_ROOT_GID`, and `CONTAINER_ID_MAP_LENGTH` exactly;
  the range must include container ids 999 and 65532 without mapping host root or
  either worker identity.
- Place the pinned `dockerd`, `containerd`, `containerd-shim-runc-v2` and `runc`
  binaries in the one protected `ENGINE_BIN` directory. The qualified runtime is
  stock `runc`, with explicitly empty arguments; do not install or enable `runsc`
  and do not define the reserved `runtimes.runc` name.
- Freeze the actual private containerd executable SHA, semantic version and
  source commit explicitly on both hosts. The version/source fields have no
  deployment default and are not covered by the kernel/Docker minor waiver.
- Pin the actual `sleep`, `systemctl`, `nsenter` and `ip` binaries and all four
  private unit files. The namespace keeper is the fixed `sleep infinity` process;
  it is not a shell or an evaluator-owned process.
- Reserve a distinct loop device against an existing, fully allocated, root-only
  regular backing file. Formatting and loop attachment are deliberately not
  automated here: inspect exact targets and preserve existing data. The file and
  loop-device capacity must not exceed `MAXIMUM_BYTES`. The verifier checks the
  actual loop backing device/inode, allocation, capacity, ext4 mount, headroom,
  and absence of nested/alias mounts.
- Name the rendered mount unit using `systemd-escape --path --suffix=mount` for the
  exact candidate mount target. The source filename `candidate.mount.in` is not
  itself an installable mount-unit name. Arrange private mount propagation; a
  shared/slave candidate mount refuses admission.
- Create root-owned protected `candidate/{docker,containerd,exec,jobs,scratch,
  daemon-logs}`, `candidate/scratch/{engine-tmp,docker-tmp,engine-home}`, `state/`
  and `control/` parents. Individual worker run directories remain private.
- Create a fresh root-owned `candidate/exec/containerd-sockets` directory.
  Containerd's shim socket source currently uses `/run/containerd` independently
  of configured root/state. Both private daemon units privately bind this quota
  source over the existing `/run/containerd` in their mount views; they never
  write the user's containerd tree. Admission verifies actual device/inode
  identity in both peers and every managed shim. This prospective compatibility
  requirement needs source/version and native qualification, not an assumption.
- Review engine memory/CPU/task/descriptor and start/stop limits. The default
  verifier settings expect container log limits `8m` and `2`; changing those
  settings requires matching pinned engine content. Disable CRI and keep every
  growing containerd/engine/temp/log root inside the bounded filesystem.

The engine explicitly selects classic `overlay2` and disables the Docker29
containerd image store. The verifier independently queries the actual private
daemon and checks stock runc, remapped root, systemd/cgroup-v2 and private
containerd identity. Root-owned on-disk metadata alone is not proof: the actual
socket peer executable, configuration argv and helper/temp environment must
match. The exact protected `chmod` helper makes both control sockets root-only;
`UMask` alone does not override Docker's explicit socket mode.

Only the engine and containerd services use `UMask=0022`: runtime-created mount
destination parents inside a container must be traversable by its remapped
non-root user. Their host data/state roots remain explicitly root-only, control
sockets remain explicitly restricted, and keeper/network/recovery services keep
`0077`. This does not expose any additional configuration or vault mount.

Both daemon units set `LimitCORE=0` as defense in depth, not as proof for a piped
host core collector. Linux ignores that limit for piped `core_pattern` handlers.
Without a separately selected native class, admission rejects every pipe handler,
including a seemingly bounded or drop-only handler. That unchanged default admits
only the exact non-piped `core.%P` filename, with actual daemon soft/hard core
limits both zero and an actual quota-backed working directory. There is no
expected-string or Boolean waiver.

The optional `tracked-create-core-one/v1` source contract is a separate path, not
evidence that the current host is qualified. Its protected acceptance must name
the exact kernel/raw handler bytes, runtime/release, thirteen helper assets,
twelve retained native trials and separate operator review. It is loaded and
rechecked by the concrete native owner; probe output or a caller-supplied pass
cannot mint acceptance. This class also requires its actual tracked compiler,
candidate core=1:1 and derived inherited seccomp controls to be proved. Missing,
stale or mismatched evidence refuses admission, with no fallback to either class.

These templates do not change global core policy. A separately approved host
policy and native child/core, crash-storm, kernel/journal, storage and cleanup
proof are required before a host with an unqualified pipe collector can be used.
The pipe exception follows the [Linux core(5) contract](https://man7.org/linux/man-pages/man5/core.5.html).

The stable namespace keeper uses `PrivateNetwork=yes` and initially has only
loopback, with no external link setup. Engine, managed containerd/shims and the
network guard share that namespace through the pinned lifetime dependency group.
`Requires`, `BindsTo` and `After` prevent a replacement daemon from silently
escaping when the keeper dies. Native proof compares the three live namespace
identities to a captured keeper descriptor and positively rejects the host
namespace; systemd metadata alone is insufficient.

The network unit creates only `inet urnetwork_subeval` inside that private
namespace; it neither flushes a ruleset nor reads or changes user Docker or host
FORWARD rules, routes or sysctls. Candidate networks must
be internal, non-attachable, IPv6-disabled, isolated-gateway bridges named exactly
`unev-` plus the first ten run-id hex characters. `iptables=false` is not the
network boundary: admission checks bounded link/route inventory and the live
input/forward/output policy inside the captured namespace. Only loopback, at most
one address-free cohort bridge and its enslaved container veths are permitted.
The explicit within-that-one-bridge forwarding rule preserves PG/Redis/workload
traffic; all other scoped ingress/egress still drops.
Never remove the guard while any private workload exists.

Read-only native network tools enter only the captured descriptor via pinned
`nsenter`. Callers cannot select a PID, namespace path, tool or ip/nft command.
The same unit invocation, process births and daemon UDS peers are rechecked around
every fact consumer. Every successful verification owns a receipt: defer
`boundary.Close()` and call `boundary.Recheck(ctx)` after consuming native facts.
No receipt is serialized into a job plan and no host-namespace fallback exists.

Native qualification remains mandatory before use: prove ordinary same-bridge
evaluation traffic with this host's bridge-netfilter settings, deny host/outside
ingress and egress, exercise mapped-output ownership from actual kernel namespace
maps, verify classic-overlay2 buildx/resource behavior, fill/exhaust only the
private quota, and hard-kill only owned private services through cleanup. An
unexpected network interaction must fail closed, not silently relax the guard or
change host bridge-netfilter sysctls. The coordinator must then run the frozen
shared-baseline no-op screen; these templates claim no host equivalence.

Primary contracts: [Docker multiple-daemon isolation](https://docs.docker.com/reference/cli/dockerd/#run-multiple-daemons),
[Docker29 containerd image store](https://docs.docker.com/engine/storage/containerd/),
[Moby29 runtime reservation](https://github.com/moby/moby/blob/docker-v29.9.2/daemon/runtime_unix.go),
[systemd private-network sharing](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#PrivateNetwork=),
[containerd2.2.1 shim socket root](https://github.com/containerd/containerd/blob/v2.2.1/pkg/shim/util_unix.go),
and [nftables interface matching](https://www.netfilter.org/projects/nftables/manpage.html).
