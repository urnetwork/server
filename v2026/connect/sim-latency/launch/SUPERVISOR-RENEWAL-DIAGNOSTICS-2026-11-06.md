# Supervisor renewal diagnostics

Source: `ddb3f19e38f98b2ed9e0d42c0d38d4d9a6f720f3`, parent
`7fbf5eec2169640e3f39815f21ea610d259b60d7`.

The running supervisor lost authority after database renewal deadlines at
00:40 and 00:54 UTC. The first incident includes both a pre-admission timeout
and a post-acquisition timeout. Existing logs cannot establish whether the
latter failed during session setup, BEGIN, callback, or COMMIT. They do not
prove pool exhaustion or a network fault. Subsequent recovery preserved the
submission's original evaluation clock.

This change exposes bounded, completed acquisition and transaction observations
and fixed semantic step ordinals in the supervisor report. It changes neither
SQL, operation ordering, contexts, retry predicates, lease duration, nor retry
budgets. Observations do not authorize replay of ambiguous operations.

Independent qualification used Go 1.27.3, default vet enabled, SN
`6e44c9892ead8b66d24c873571c46f6e1cc8c3f2`, and the dependency pins in the
retained receipt. Six controller unit tests, one CLI report test, and two private
PostgreSQL tests passed once ordinarily and once with the race detector:
18 positive executions, zero failures/skips/race reports. Every positive
database callback attested schema 943. Two separate controls compiled and then
failed at the intended missing-diagnostic assertions after removing only the
serializer assignment or restoring only the parent renewal method.

Receipt: `/mnt/data/low-merged-d49-plan.b9EKrHyZ/supervisor-ddb3-qualification.json`.
SHA-256: `5475fc679c8ad26d79ca6b5dc661587d42a005eddf5b60257f1a70c93c28f2cc`.
Raw test output is verbose native test-binary stdout, not JSON test events.

This is targeted source qualification, not a full-suite, live database-route,
host-performance, or production-runtime qualification. The running supervisor
and worker have not been replaced. Deployment must respect the active attempt;
this evidence does not authorize interruption or a new evaluation clock.
