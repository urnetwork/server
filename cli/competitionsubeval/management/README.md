# Prospective management reserve ledger

These four slice templates are inert source inputs, not installed units. Nothing
here writes a host, starts a service, moves a process, or accepts native evidence.
Epoch-one releases and the user's Docker/containerd services are not changed.

The explicit `urnetwork-management-24g/v1` protected policy accounts for the same
24 GiB already reserved by the frozen resource helper:

| Owner | Aggregate ceiling/reserve | Exact cgroup |
| --- | --- | --- |
| Coordinator | 8 GiB | `/urnetwork.slice/urnetwork-management.slice/urnetwork-management-coordinator.slice` |
| Private engine, containerd, shims and management helpers together | 8 GiB total, not per daemon | `/urnetwork.slice/urnetwork-management.slice/urnetwork-management-private.slice` |
| Independent cleanup | 2 GiB | `/urnetwork.slice/urnetwork-recovery.slice` |
| Existing core handler | 256 MiB | `/urnetwork.slice/urnetwork-core.slice/urnetwork-core-handler.slice` |
| OS and all other admitted ambient ownership | 5.75 GiB, counted once | No fictitious bounded slice |

The ordinary management parent is **16 GiB**, the sum of coordinator and private
children. It is a second-level constraint, not another reserve allocation.
Independent cleanup must not descend from any binding OOM or task-limit ancestor
shared with ordinary management. In particular, native admission must prove that
the common `urnetwork.slice` ancestor has no binding memory or PID limit, and
must examine every further common ancestor. An 18 GiB parent containing cleanup
is deliberately not this architecture. Existing core-handler limits stay
256 MiB, 128 tasks, CPU quota 200%, and a 100 ms quota period; this directory
does not replace its unit or dispatcher.

`ManagementBoundarySettings` owns the explicit expected values. Its defaults are
the only numeric source for rendering these templates. Ordinary parent tasks are
20,480 = 4,096 coordinator + 16,384 private; independent cleanup has 1,024 tasks.
These are prospective finite settings, not accepted pressure measurements. The
approved memory/core profile is fixed; changing its partition requires a new
reviewed profile, not a matching change to an untrusted document. Swap is
explicitly zero for every bounded role.

Render every token from the protected owner settings and the policy's explicit
`management_cpuset`, which must match the independently pinned resource helper.
CPU indices are parsed canonically with the owner's finite `MaximumCpuIndex`
bound before range expansion. Reject unknown, duplicate and unresolved tokens.
`AllowedCPUs` is mandatory. Do not set inherited `CPUAffinity` on daemon units:
native container descendants must receive their separate frozen evaluation CPU
sets, rather than inherit a management scheduler affinity they cannot widen.
There are no install sections, execution commands, `OnFailure` handlers or
activation tools in these templates.

The root-owned enclosing configuration must pin the policy document itself.
Every bounded row, including the existing core handler and ordinary parent,
must explicitly pin the exact rendered unit path and SHA-256. The OS reserve
must have no unit, path, task count or swap allowance. Loaders must reject
unknown JSON fields and verify protected root-owned, non-symlink paths and
actual bytes before using the pure ledger validator. A matching name or digest
in peer/candidate input is not authority.

Daemon service wiring must place **all** ordinary private management processes
in the single private aggregate, including namespace/network keepers, dockerd,
containerd, their shims and any retained management helpers. Coordinator child
processes and independent recovery must have their own exact role ancestry.
Candidate evaluation, scorer/preflight and BuildKit/run/build cgroups stay
outside the binding management parent, under their existing separately pinned
run parents. No service text alone proves those actual placements.

`ValidateManagementResourcePolicy` checks only explicit identities, pin shapes,
limits and overflow-safe accounting. It performs no native admission. Before any
future activation, an independent native owner must still establish all of:

- Fresh actual host headroom and the frozen resource helper's exact host,
  active, evidence and management facts, including retained-build accounting.
- Fresh observed trusted ambient load and available headroom fitting the one
  OS/reserved allowance, followed by pressure qualification under the declared
  workload. This is not an instruction to resize or cap unrelated host services.
  Unexpected load refuses admission; a 5.75 GiB ledger line alone proves nothing
  about future trusted-service demand or candidate-induced kernel pressure.
- Manager plus kernel readback for every intended ancestor/child ceiling,
  zero swap, effective management CPU set, process births and exact cgroup
  membership. This must include native service children and container scopes,
  not only main PIDs or matching configuration strings.
- Complete ordinary/private closure and proof that candidate/build descendants
  cannot be charged to the management aggregate or escape their run parents.
- Resource-exhaustion/pressure trials demonstrating coordinator progress and
  independently dispatched cleanup while ordinary management is saturated or
  OOM-killed. Recovery must stay outside every shared binding OOM/task ancestor.
- Durable, bounded failure/status evidence outside candidate quota, with
  cleanup runnable after management loss and no reset of the original job clock.

No Boolean waiver, fake-kernel regression, static pin, or successful arithmetic
check substitutes for those held native observations and pressure results.
Native acceptance and activation require a separate reviewed plan.

The prospective agent now observes the ordinary parent's exact 16 GiB cap and
both 8 GiB children independently in PID1 and native cgroup v2 before admission.
Held descriptor inventories bound all examined descendant groups and require
their effective management CPU set; fixed slices must contain no direct tasks,
and the common ancestor must remain nonbinding for independent recovery. Changed
generations, extra sibling roles, inherited/unbounded caps, swap and insufficient
current runnable margins refuse admission. These observations are not a proof of
ambient headroom, every external coordinator's process placement, SSH ingress
placement, worst-case memory peaks or actual pressure recovery. Those remaining
contracts must compose before native acceptance; the synthetic tests do not
activate or qualify any host.
