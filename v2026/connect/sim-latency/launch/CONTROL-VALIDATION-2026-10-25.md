# Current-main harness validation — October 25, 2026

Three consecutive complete `connect/sim-latency/tests.sh` invocations passed
against exact clean server main
`ec02f4e5bcfada7bb541824273be53d6955303f6`. This covers the current-main
host preflight corrections and package tests, not a production deployment or
the entire repository-wide Go suite. The frozen evaluated server remains
`0d58d0d91b069c62bcfb39cd72166c26925b786a`; neither its branch nor the source
ledger or evaluator image was changed by these tests.

Command: `bash connect/sim-latency/tests.sh -count=1`, each invocation bounded
by an outer 20-minute TERM / 45-second KILL timeout. Actual Go **1.27.3** was
selected on PATH with `GOTOOLCHAIN=local`; management CPU affinity `20,22`,
`GOMAXPROCS=2`, and `GOFLAGS=-p=2` were enforced. The production manager
workspace selects this main server and the nine frozen dependency checkouts;
the separate evaluated-source three-pass proof remains unchanged.

| Pass | Exit | Wall seconds | Race package seconds | Top-level passes |
| --- | --- | --- | --- | --- |
| 1 | 0 | 274 | 140.045 | 446 |
| 2 | 0 | 142 | 134.460 | 446 |
| 3 | 0 | 142 | 134.308 | 446 |

Each pass includes the complete six-epoch lifecycle without race scheduling,
then all 445 race-enabled package tests. No external retries were used.

The proven disposable synthetic-local service fixture attested endpoints
`127.0.0.1:32830` and `127.0.0.1:32831`. Main PostgreSQL, API, MinIO and vault
were not used. The shared scoring lock covered setup, all passes and cleanup;
no frontier, scoring or other build/bomb workload overlapped. Cleanup exited 0
and left only the original persistent containers/networks. The operator source
remained clean at the exact revision above.

Full local evidence, relative to the mono root:
`temp/current-main-three-validation-20261025.jKQPPhD1/`.

| Log | SHA-256 |
| --- | --- |
| `pass-1.log` | `4cea876257d301892b87bb15850d42d6f211b47a32a51f94f4c9278493f4528f` |
| `pass-2.log` | `7e286b5a50ccb59f44d1099201541cb0439f3213882201e45713d73523a8adb3` |
| `pass-3.log` | `be5cdea02be9c700c88bbaf6602bcd5f487c9f2fa21c63a4165f78e51585f0cd` |

These are retained local test logs, not a claim of immutable MinIO upload.
Neither passing tests nor a complete exploratory point qualifies production
scale, variance, reference separability, hidden epoch control, image-bound host
readiness, provider-backed review/test clients, or live API/worker activation.
See [PRODUCTION-2026-10-28.md](PRODUCTION-2026-10-28.md) for current preparation.
