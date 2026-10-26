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
