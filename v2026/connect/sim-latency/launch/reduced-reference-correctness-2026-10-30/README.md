# Reduced-reference build and correctness

The owner-approved [100 ns/byte regression reference](../reference-h1-byte-pacing-100ns-2026-10-30/README.md)
has completed native build/readback and three independent 112-test race
processes: **336 top-level passes, zero failures or skips**. The six separate
caller-regression passes are not included in that total. The failed first caller
is retained; it stopped before any of the 112 tests ran.

The exact [build qualification](worse-build-qualification.json),
[correctness qualification](worse-correctness-qualification.json) and
[source/execution binding](SOURCE-AND-EXECUTION-BINDING.json) connect the native
`2eadb973…` image and `a20a4116…` product binary to the actual tested product
bytes, dependency closure, runtime profile and completed logs. The race-test ELF
is a different executable, not a claimed byte-identical native product binary.

Root installed the two typed qualifications, their binding and five exact
evidence copies in fresh paths under the new pilot's protected inputs directory.
An independent audit verified all 46 typed evidence occurrences, hashes and
single-link/permission checks. The eight new files are Root-owned mode 0400.
The launch-profile copy is necessary because its preserved original is mode
0664 and cannot be admitted directly. `ROOT-COPIES.json` and `HANDOFF.txt` are
the original, unchanged pre-installation instructions; their `installed: false`
describes that earlier proposal, not the subsequent Root action.

The [publication readback](PUBLICATION-PROOF.json) verifies both typed artifacts
at pushed server commit `7d3cce1a54afda98b3803e05fb11874345524aec` against their
Root-installed bytes. Their two native publication receipts are installed under
the new pilot's protected inputs. Together with the four unchanged better/no-op
publications, six of the seven prerequisites are available; the new operator's
completed qualification and its publication are still pending.

`MANIFEST.sha256` verifies the five original packet members. This publication
does **not** establish measured regression direction, a successful replacement
pilot, hidden-seed calibration, operator qualification or production grading
readiness. The [rejected previous pilot](../../baseline/prelaunch-public-regression-r9-2026-10-30/README.md)
remains rejected, and submission gates, R9, significance, margin and deadlines
are unchanged.
