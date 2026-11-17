# Epoch 2: Goofe restored

Observed 2026-11-12 00:12:19 UTC. The round remains open. Queue: two scored,
two failed, two running and ten queued. A successful score is not a claim that
its acceptance gates or significance threshold passed.

## Completed recovery

The original Goofe attempt `ce04036c…` never executed its candidate. Its original
23:52:17.050115 UTC deadline expired without extension. The approved recovery
completed the unchanged full cleanup and recorded a prelaunch infrastructure
failure, with both cleanup flags true and recovery inactive. No evaluator was
restarted and no fresh evaluation allowance was granted to that submission.

The original canonical recovery service and ingress wrapper were restored by
exact content hash. The temporary Sille rule blocking only root-owned SSH to
Goofe was removed by its complete exact rule; absence was independently checked.
No other firewall rule was changed.

The original authorization, starting receipt, clock and every failed operator
attempt remain intact. The qualified one-hop amendment replaces only recovery
executable/service authority and binds the exact original starting receipt.
The actual cleanup was completed under that amended authority; it was not
inferred from missing units or empty containers.

## Root causes and qualification

The recovery's SSH parser rejected legitimate repeated non-authority `sshd -T`
keys. The correction permits those lists while keeping exact, unique routing
authority for `forcecommand`, `authorizedkeysfile` and `authorizedkeyscommand`.
The parser and narrowly authenticated one-hop resume were independently tested:
132 ordinary and 132 race executions passed, with source unchanged.

- Source fix: `5356a4cfa341e93b94126f25eaa6709640589ad0`.
- Merged, pulled and pushed main build:
  `12bfab9fe1b824c9a3315f27c9939b20c2df1a82`.
- Native recovery executable SHA-256:
  `0b26c5dbccab422e75bc9d9006773a4b8ec6e6d5647f16a8e5d9b9ad003123aa`.
- Ordinary log SHA-256:
  `a1ce62431080881ff0a9e3f5a51394a60a842ce46924bac3223a7a6980c8e3ec`.
- Race log SHA-256:
  `582ab0e0059118f4b77353caa93fb538a89563f2136eb2e6b5e8c040ea860b82`.
- Logs on Sille:
  `/dev/shm/competition-parser-independent-qa-20261111.O4YW9C8T/`.

An additional failed control unit from the earlier connection race remained in
PID1 after its registry plan disappeared. Its approved executable/config,
fragment, stopped manager generation, absent admission and native emptiness
were freshly audited before retiring that exact unit. The full control-unit
namespace was then independently observed empty. No unknown generation was
ignored and no registry proof was fabricated.

## Retained proof references

On Goofe, operator evidence is under
`/etc/urnetwork/competition-goofe-legacy-parser-amendment-operator-20261111-v1/`.

- `continuation-after-d5b-result.json` SHA-256:
  `af8bdccf8be6907942e54111c73972f1828b1d8a48536d4429c32d3fff608001`.
- `d5b-retirement-result.json` SHA-256:
  `74074f9774569e8eefb8fbf8e209e1b186ee05a360c63a4b190fb30840a6a302`.
- Native run `ce04036c…`'s `legacy-prelaunch-closed.json` SHA-256:
  `619af308f6eb7ed2174c860f8e7ef30f1e933b11da7c83cf9e4450d8cafd8118`.
- Sille's root-sealed firewall removal receipt:
  `/etc/urnetwork/competition-goofe-legacy-connection-fence-20261111-v1/removal-receipt.json`,
  SHA-256 `3e28717cd51babd31844d4372d1ea81d88fb22ec43e094f0aeeafca7c64c8b03`.

These are local retained operational proofs. This report does not claim that
they have additionally been archived to MinIO.

## Verified service and remaining work

Goofe claimed the next FIFO submission `01a1c6e5-7759-5904-7e77-f34c73c81840`
at 00:06:23.919882 UTC. Its native execution started at fence 14 with a live
database lease and its own original 03:06:23.919882 UTC deadline. A separate
Goofe-only signed availability observation authenticated the occupied state;
it sent no check or evaluation operation to Sille.

Sille's current candidate has completed eight of nine replicates. Its database
custody was handed back after PostgreSQL lock timeouts (`55P03`) during heartbeat
scope-lock acquisition. The retained event shows three attempts returned while
the parent was live and confirmed lease/retry time remained. Same-attempt
reconciliation and retry resilience are being investigated; no measurement or
clock reset is authorized by this report. The managed-control registry also
needs a deterministic fix so loaded failed units retain their authority until
positive PID1 retirement.
