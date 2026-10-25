# Completed public-seed cold R9 qualification

**Passed locally; not production activation or an official epoch baseline.**

The unchanged trusted evaluator completed nine fresh controls, an offline
comment-only candidate build, nine candidate runs, scoring and exact cleanup.
Start: `2026-10-25T03:15:24Z`; end: `2026-10-25T06:12:12Z`; exit 0.
Wall time: **10,608 seconds (2h56m48s)**, leaving **192 seconds** inside the
10,800-second execution limit. This is one observed cold execution, not a
guarantee that every seed will fit. Formal independent-seed qualification remains.

## Bound release and inputs

- Server: `0d58d0d91b069c62bcfb39cd72166c26925b786a`; ten-repository canonical
  lock: `37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`.
- Evaluator image:
  `sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`.
- Actual Go 1.27.3; simulator/scorer:
  `4993081b4076693f231ca2e5d3ee4468646c54d2d6cbc89a01fe325fccf57dbc`.
- Trusted evaluator script equals the frozen server's script, SHA-256
  `d55eac73bebd5a3be1ef01c0ad024633a3c71d01b481350386b06810e64feed2`.
- 3,600 providers, 200 clients, impairment enabled, public seed 20261024.
  All timers, settings, resource gates and R9 counts follow the
  [predeclared protocol](../../../launch/COLD-R9-PROTOCOL-2026-10-25.md).
- Candidate patch: `1076be3ed61df0e14ae90c13bbe4c35442de6371064fd7d78f61ed6834e170f1`.
  Candidate image:
  `sha256:a4770f83335c8860f6e499bab97e6ffc1101ad347350b566efc7321c4cd6ebe3`.
- Whole invocation used UID 0, matching its whole-root preflight. Only direct
  synthetic local leaves entered candidate execution. No permission was relaxed.

## Verdict and statistical interpretation

All six gates passed: success, volume, path/accounting integrity, matchmaking,
stability and resources. All typed security fields passed, including offline
build, default-deny networking, reserved management resources, immutable reports
and cleanup. Original containers/networks were verified after terminal; no
owned container/network or evidence tmpfs remained.

The no-op is **placeable**, **not statistically significant**, and **not eligible
for takeover**. One-sided Welch p = **0.532133**. Its candidate mean score was
18,680.02 ms versus the control mean 18,535.18 ms: **0.78% worse**. The scorer's
practical margin was the provisional 16.1%; the variance-dependent improvement
fringe was 16.73614%, so the combined mean-score line was **15,433.11 ms** or
lower for this particular comparison. The practical-only mean line was
15,551.02 ms. These are diagnostic thresholds, not the final season margin.

The control median raw score was 18,509.81 ms and candidate median 17,037.68 ms.
The provisional median-score practical line was 15,529.73 ms. A lower candidate
median alone does not establish significance: the Welch test uses run-level
means/variances and remained nonsignificant. These raw scores are not TTFB
measurements and must not be labeled TTFB in reports.

Control raw-replicate sample SD was 3,080.34 ms (CV 16.62%). That is not an
estimate of the repeated nine-run aggregate's SD. The scorer's supported
20.2174% next-margin recommendation is retained as a diagnostic; there is no
eligible winner, so it does not automatically alter the source ledger or season.
Fresh predeclared stability, margin and reference work remains necessary.

## Preserved bytes and verification

`artifacts.tar.gz.part-00` through `-03`, concatenated in that order, contain
all **440 original regular artifact files**, including both run and scorer-input
copies, all 18 raw CSV/log/marker/accounting/resource sets and samples, workload,
request, score, completion, evidence manifest and typed worker result. The four
ordinary Git blobs are at most 32 MiB each, below GitHub's single-file limit.
The original root-owned source tree is untouched.

Reconstructed gzip SHA-256:
`edbd5340afe6facfc6b92bfb1a933e3ceac84bbf7e280249874ceaf9a388f8aa`.
The archive contains only relative directories/regular files, no links.
Its evidence manifest binds 429 original files; the typed result additionally
binds its seven declared artifacts. Selected small JSON/terminal/provenance files
are also copied alongside it for direct inspection. Manager stdout/stderr are
separately preserved with deterministic gzip compression.

Worker result SHA-256:
`4a3bfc5725f4ad12993e137f99a550f7594d7bb8b4a5221a4a419c3f7ded3c38`.
Evidence manifest SHA-256:
`9207ac4571b3826e8e2be3dbcc4911bc4b8d6d9abcdd1c152ccd581290acb992`.
Completion SHA-256:
`e47709058ec846c1c04db87a5925674006d9b39d5c39c93295eaec70b265920d`.
Manager terminal SHA-256:
`a1be923b0f0277c19a7bdbf6b0f5dc08209bdf1de12f94055df10cf9bcf65b4d`.

The enclosing baseline manifest covers the chunk files and readable companions.
Independent Go-only streaming verification **passed**: all 440 original files,
429 manifest bindings, seven result bindings, seven readable companions and
9+9/G1–G6 verdicts matched, with no missing/extra/unsafe/link/duplicate entries.
The retained [verification log](archive-verification.log) SHA-256 is
`0fe0bcb6d7ebb68e1c8b7c96d1d39f2b0670298ff3d39ab1d60bb3de8375bf89`.
Private executed helper SHA-256:
`eefc0e2cd5f104f54bba85e3e90f2fd9b5aaa325ea7b4aaee74a7efe4b67cbbb`.

## Scope limits

API/worker image and hardware qualification labels in the request are explicitly
synthetic offline fixture identities, never actual live deployment or provider
receipts. No live submission, API/DB mutation, artifact upload, hidden-seed
baseline, production host rebaseline, source promotion or activation occurred.
The no-op's success does not qualify the separate warm-reader optimization.
