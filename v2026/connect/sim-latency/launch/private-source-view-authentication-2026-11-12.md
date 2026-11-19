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

## Qualified correction and completed view

The missing-only regression caught a second Git behavior: `--ignore-missing`
must precede `--really-refresh`. Its original failure is preserved separately.
The corrected source-owned predicates are versioned in
`tools/competition-qualification-index`; tests invoke those actual shell
functions against synthetic Git repositories, not a second implementation.
Independent final-path qualification passed all seven tests three times
ordinarily and three times under the race detector: 21 + 21 passes, zero skips.

- Predicate SHA-256: `ca834144859c3c696ca1f330f52ccf754caafb6f0da3bdadf069a71602b250fb`.
- Ordinary log: `118da2fdeaed7fe04b5b4bf2f3b4069a08a68954a5bfc90b04aa83881b75f150`.
- Race log: `8727a6c8eea3656c60b362ad3b30eedacf1dc7736700561a12af3e3ef2d041af`.
- Private test evidence: `/dev/shm/competition-qualification-index-finalpath-qa-20261112.6aiH7nu3`.

The reviewed v3 continuation then completed all ten repository checkouts:

- Source SHA-256: `2428bef02498682a06bf264fad6a57e6e72c264ac8706d7c73f693c14a688a15`.
- Invocation: `019853f486814d15b0bdb8b1e4cd678a`; actual exit 0 and monitor exit 0.
- Elapsed: 231.878 seconds; peak memory 512 MiB; swap peak 0.
- View allocation: 933,433,344 bytes; owned root Git metadata: 7,143,424 bytes.
- Remaining space: root 36,243,841,024 bytes; data 4,884,041,728 bytes.
- Private evidence: `continuation-20261112-v3` beside the preserved v2 receipt.
- Repository-manifest SHA-256: `1e16d03973171fcaa8d9ee2307fbd283de0f9ea9bdd4a6daa74cb2f07f557bf3`.

This prepares a **recovery-tool qualification workspace**, not a new competition
source checkpoint or a deployment. All ten pinned worktrees passed their
complete-source and clean-status checks. No forced overwrite, shared source HEAD
change, evaluation restart, or competition clock extension occurred. Full ABI,
API-contract, migration and recovery runtime qualification remain separate work.
