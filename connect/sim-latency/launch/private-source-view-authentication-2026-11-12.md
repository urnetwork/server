# Private qualification checkout authentication

The recovery release needs real sibling repository checkouts for API contract
tests. This preparation is separate from the frozen competition source and does
not change its evaluator, baseline, scoring policy, submissions, or deadlines.

The original materializer stopped at its 90-second checkout bound. The next
continuation used bounded 20-minute I/O phases, a 1 GiB view cap, 64 MiB of
owned Git metadata, and minimum free space of 33 GiB on root and 4 GiB on data.
It stopped before copying any missing file:

- Script SHA-256: `ec3538e94877afb0b1102ac9d9420963def0586478e93f12dd75db8a57866e66`.
- Invocation: `a94a8efe4ca347468c2ecd818763ca9d`.
- Actual exit: 1; resource monitor exit: 0; elapsed: 5.292 seconds.
- Failing phase: `server-authenticate-present`.
- Private receipt directory: `/mnt/data/competition-recovery-main-qualification-20261112.1WMPyzn0/continuation-20261112-v2`.

The check used a new private index populated by `git read-tree`. Its stat cache
was empty. `git diff-files --quiet` reported modifications; a bounded sample of
six flagged files nevertheless had content hashes and executable modes matching
the pinned source. Even patch-format output did not establish authentication.
This sample diagnoses a false-positive mechanism; it does not prove the whole
checkout correct.

The root-owned private diagnosis receipt is
`read-only-stat-cache-diagnosis.json` in the receipt directory above, SHA-256
`51c928cbd9fa6c91baff9cd3427f195e4afb3f2f38f3d28929071d4db800288e`.
The failed verification index remains unchanged, SHA-256
`27ae52c24a905602f0718637982c357c4af34a10e8d9f49070993cd23574522b`.

The correction must refresh only a new private verification index before strict
comparison, preserving the failed index and all receipts. Deterministic Go
regressions must distinguish unchanged content from real byte or mode changes,
allow genuinely missing files for missing-only population, and keep the exact
approved baseline exclusion from hiding sibling paths. No corrected continuation
is qualified merely by this diagnosis.

An inactive transient unit with no current PID is not proof that it never ran.
The retained original command result and invocation receipt establish this
attempt's actual failure. There was no automatic replay, real-index initialization,
new repository registration, compiler invocation, or live runtime change.
