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

Both daemon units set `LimitCORE=0` as defense in depth, not as proof for a piped
host core collector. Linux ignores that limit for piped `core_pattern` handlers.
Admission currently rejects every pipe handler, including a seemingly bounded or
drop-only handler; there is no expected-string or Boolean waiver. The only
supported policy is the exact non-piped `core.%P` filename, with actual daemon
soft/hard core limits both zero and an actual quota-backed working directory.
These templates do not change global core policy. A separately approved
maintenance/handler plan and native child/core proof are required before a host
with an unqualified pipe collector can be used.
The pipe exception follows the [Linux core(5) contract](https://man7.org/linux/man-pages/man5/core.5.html).

The network unit creates only `inet urnetwork_subeval`; it neither flushes a
ruleset nor changes user Docker tables, routes or sysctls. Candidate networks must
be internal, non-attachable, IPv6-disabled, isolated-gateway bridges named exactly
`unev-` plus the first ten run-id hex characters. `iptables=false` is not the
network boundary: admission checks the live, scoped input/forward/output drops.
Never remove the guard while any private workload exists.

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
and [nftables interface matching](https://www.netfilter.org/projects/nftables/manpage.html).
