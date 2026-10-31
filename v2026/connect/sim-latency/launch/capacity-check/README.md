# Read-only competition storage check

This host-side Go command checks native `server/blob` retention configuration
and counts **all object versions and delete markers** under `blob/competition/`
against the operator-approved decimal 10 TB allocation. It does not load the
competition policy or import the controller. A storage diagnostic must remain
usable when that independently evolving policy is unavailable to an older tool.

The command accepts one existing Root-owned mode-0700 output directory and writes
one exclusive mode-0400 `capacity.private.json`. Run it only through the reviewed
host `main` environment and Go 1.27.3 dependency graph. Do not paste credentials
into arguments or use this host utility inside a submission container.

The remote operations are native retention-configuration HEAD/GET checks and
streaming version-list reads, sharing one 90-second deadline. There are no blob
uploads, deletions, lifecycle/replication changes, database accesses, API calls,
model calls, scoring runs or source changes. Missing remote configuration, local
fallback, cancellation, partial listings and allocation exhaustion fail closed.

## Qualification

Sol 6.1 max source review and an independent max review covered both files.
Sol 6.1 low compiled the actual named files with the qualified cached Go 1.27.3
toolchain, ran four top-level race-enabled tests in three fresh processes, and
compiled the static native helper. All **12 tests passed**, with zero failures
or skips; source, resolved dependencies, module, compiler and ELF postchecks passed.

The tests exercise the real native constructor without policy loading, copied
configuration ownership, the actual version accumulator with historical bodies
and delete markers, error/cancellation refusal, and the embedded actual source's
policy-independent/read-only boundary. No production storage was accessed by
the tests. This is named-file qualification, not a claim that the complete
repository test wrapper ran.

- Helper source SHA-256: `f50c9e86d88e8b9f2143f0de342fb482efb3b2428acb9aa56b20725c98bb8007`.
- Test source SHA-256: `0b5c28003a6c6291cfd562a2138bb2088a05ee38a693db1cfb8499b1be8d4c47`.
- Native helper SHA-256: `37adfebce748e63863a6507932847aeaeebe3276e2b7544b3943bcfe318cd88b`.
- Private QA: `/mnt/data/minio-current-capacity-qa.yGSGHlHD`.
- Terminal receipt SHA-256: `bd0334f23d3c422b22e3e1b18f03c56cd7df64de42fde1e4a9b82e6b1c0522b9`.

The earlier policy-coupled utility is preserved separately. Its October 30
invocation failed at competition settings loading, before version accounting;
that failure is not evidence that MinIO was unhealthy.

## Actual worker-host observation

The [scoped observation](LIVE-PROOF-2026-10-30.json) completed from sille at
07:01:15 UTC on October 30, using the same protected configuration/vault roots
and hostname as the R2 worker. Authenticated retention checks passed. The
namespace contained **585 versions/markers and 1,893,410,419 body bytes**, or
**0.01893410419% of the approved allocation**. No remote object was written or
deleted. The raw receipt remains private and its SHA-256 is recorded.

This does **not** prove physical disk free space, a new retained upload or exact
version readback, delivery of these artifacts to a replica, off-site custody,
API-host readiness, baseline acceptance or scoring activation. The unexplained
later failure to retain the original baseline-launch failure remains unknown;
neither a health endpoint nor this accounting receipt reconstructs that cause.
