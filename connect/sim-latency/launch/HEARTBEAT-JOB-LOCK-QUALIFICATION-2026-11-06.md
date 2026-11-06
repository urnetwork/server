# Job-heartbeat read-only-prefix continuation

Qualified source: `1ef57120016ac2b5a938f53a96624c837004d1de`.
Parent: `55deb96666f39a29b81e5097b5de107054b53165`.

The retained event at 02:42 UTC shows a heartbeat deadline at its job-row lock
after earlier acquisition/setup work consumed most of the call budget. Exactly
one transaction entered the callback; rollback was acknowledged and no COMMIT
was attempted. This identifies a failure class, not a competing lock holder or
network root cause. Later plain-deadline events remain unclassified.

The fix permits continuation only for the source-authenticated three-statement
locking prefix, one admitted transaction, acknowledged rollback and returned
protocol-idle connection ownership, with no writes, unknown work or COMMIT.
Earlier failed acquisitions cannot hide another admitted callback. It retains
the existing parent/job/host expiry bounds, total retry allowance, per-call
deadline and shared three-call cap. The successor rechecks authority.
Protocol-idle eligibility does not promise physical pool reinsertion or joined
destruction: pgxpool may dispose of a connection at its maximum lifetime.

Independent Go 1.27.3 qualification on Goofe, vet enabled, used SN
`6e44c9892ead8b66d24c873571c46f6e1cc8c3f2` and the frozen dependency closure.
Fifteen targeted roots passed once ordinarily and once with the race detector:
30 positive executions, zero failures/skips/race reports. Four new PostgreSQL
callbacks attested schema 943. Existing late-COMMIT and post-write controls also
passed, without claiming individual schema witnesses they did not emit.
Two controls removed only the new predicate branch, compiled successfully and
failed at the intended worker-preservation and actual PostgreSQL continuation
assertions. These expected failures are controls, not positive test passes.

Receipt: `/mnt/data/low-merged-d49-plan.b9EKrHyZ/prefix-1ef-qualification.json`.
SHA-256: `f83500e6c4842df09c300c964ad90f64341d9eeb616d85971581656ab252e337`.
Small raw test-text copies and original manifests:
`/mnt/data/low-merged-d49-plan.b9EKrHyZ/prefix-1ef-raw.cQM5Yemu`.

This is targeted source qualification, not a full suite or native production
qualification. PostgreSQL mechanism tests shorten their own call to 1.5 seconds;
production's eight-second call remains unchanged. The two-acquisition history
is covered by deterministic unit tests, not forced by these PostgreSQL tests.
Supplemental test-only source `f76cf435c7c8d23fc799e0d4174c6fbbcf9dff98`
has separate qualification and must not borrow this receipt.

No deployment, interruption, new recovery grant, evaluation-clock reset,
scoring-policy change or acceptance of an unclassified incident is authorized
by this record. The current evaluator continues unchanged until a safe,
authorized maintenance boundary.
