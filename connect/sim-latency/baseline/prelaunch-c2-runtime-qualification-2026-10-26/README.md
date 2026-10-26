# C2 runtime-tool qualification

Independent test execution at the exact source and binary in `INDEX.json`.
The builder group passed 19 tests three times (9/9/10 seconds), and the C2
runtime group passed 39 tests three times (115/115/116 seconds), without skips.
The separately completed CLI group passed 20 tests three times; those logs are
not represented as part of this dataset's 58-test selection.

`prefix-loop.log` retains the deterministic pre-fix failure: the second fixture
commit loses its configured global Git identity. The correction restores the
original global configuration before each fixture, rather than injecting a
command identity that would violate the historical fixture contract.

Logs and source receipts are retained verbatim. The compiled test binary stays
in restricted local artifacts; its digest is recorded, not a claim that every
current-main change was tested. No provider inference or timed scoring ran.
This evidence does not qualify broad-container capacity, Snow review, the
future 5 GiB release, or production activation.

## Failed actual four-GiB container capacity qualification

`broad-4g-failed/` preserves the actual attempted container qualification,
including its reviewed script, observed allocated capacity, output and terminal.
It failed before candidate building or scoring: projected use was 4,391,722,866
bytes against the 4,294,967,296-byte hard cap. Cleanup completed. This failure
is not omitted, retried under the same profile, or counted as a passing runtime
qualification. A prospective five-GiB profile must qualify independently.

## Actual five-GiB container qualification passed

`broad-5g-passed/` retains the distinct prospective attempt, which completed
at 2026-10-26T17:09:02Z in 277 seconds with exit 0 and verified cleanup.
The same observed 4,391,722,866-byte capacity projection fits its 5,368,709,120-byte
cap, leaving 976,986,254 bytes. Container ceilings and management reserve were
not reduced. The old failed attempt above remains unchanged.

The real offline builder ran vet, required tests and compilation, authenticated
all ten candidate commits, verified direct server/connect descendants and the
protected instrument tree, and passed the actual unprivileged C2 source check.
Candidate image: `sha256:2aaea2d83fdfdf348f96e9991318087ea4dcf4b7385c1f7a411620af4083d202`.
No evaluator controls, scoring or provider inference ran. This qualifies the
tested broad build/source path, not statistical calibration or launch readiness.
