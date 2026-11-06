# Job heartbeat owner diagnostic qualification

This qualifies diagnostic reporting, not the cause or repair of every live
heartbeat failure. No live binary, configuration, claim hold, evaluation clock,
source checkpoint or scoring rule was changed during this qualification.

Production source: `58aa5680eca95255f043d8176bf4543bb950f559`, based on the
separately qualified job-lock-prefix fix `1ef57120016ac2b5a938f53a96624c837004d1de`.
The test-only successor `54e91ac1b2f2e2094891a1a3fc76052e5d903b7d` adds an
actual schema-head assertion/log to the existing late-COMMIT fixture. Its
production files are byte-identical to 58aa.

The optional final handback observation records the actual owner return branch,
last store outcome, bounded timing facts and any genuinely completed database
snapshot. It attaches only after evaluator cleanup joins. Original error text,
error digest, Is/As causes, SQL, retry predicates, policy clock reads, budgets,
call caps and authority bounds remain unchanged. Completed database facts are
separate from the existing error-proof field and confer no replay authority.
In particular, the historical plain-deadline event 452 remains unclassified.

## Independently executed scope

Goofe executed Go 1.27.3 offline with vet enabled, SN
`6e44c9892ead8b66d24c873571c46f6e1cc8c3f2`, and the same frozen eight-library
closure as the preceding qualification. At most two resource-limited groups
ran; PostgreSQL/Redis fixtures were private and their parents were joined and
retired. No production configuration, vault or credentials were transferred.

| Source and tests | Ordinary | Race |
| --- | ---: | ---: |
| Exact 58aa: seven new owner tests and nine affected failure tests | 16 passed | 16 passed |
| 58aa production plus the single 54e PG test-file overlay: actual late-COMMIT/no-replay control | 1 passed | 1 passed |

There were 34 positive executions, no failures, skips or race reports. Both
actual PostgreSQL callbacks asserted successful migration head **943**, equal
to the compiled source head. Each proved a real late successful COMMIT with
one attempt/acknowledgment, truthful owner metadata, no replay and no adopted
late lease acknowledgment. These are not full-suite, native performance,
provider, live-network or production deployment qualifications.

The new deterministic controls cover store error versus late nil success;
wait, parent, authority and total-budget return boundaries; original predicate
input and policy-clock parity; preserved error identity/digest; snapshot reset,
deep copying, closed string/size bounds and late-publication refusal; and final
worker handback only after evaluator join.

## Retained evidence

Private receipt on Sille:
`/mnt/data/low-merged-d49-plan.b9EKrHyZ/prefix-1ef-raw.cQM5Yemu/owner-qualification.json`

Receipt SHA-256:
`5c248939ac0f0152531b10723199055a2a11bc8517956efc0faa687b92971e1b`.
Sibling `ownerpureordinary.log`, `ownerpurerace.log`, `ownerpgordinary.log`,
`ownerpgrace.log` and eight original manifests retain raw evidence; root
inspected the receipt, all test pass records and both schema attestations.
The logs are native test text, not JSON.

The PG overlay file SHA-256 is
`7f31ec6a50c74f245499ec3503ee091da06878c0328410c11f60167b204b9d7a`;
overlay JSON SHA-256 is
`b50813d430c26e83a839dd7c67eecfeb63ca4b018f1331cb3695ef68fb1de8e7`.
This was explicitly a one-test-file overlay, not a full 54e physical graph.

Deployment remains separate. The previously built prospective 1ef worker does
**not** contain these diagnostics. Any new binary needs its own exact build
identity and an approved idle maintenance boundary before activation.
