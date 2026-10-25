# Calibration analyzer smoke qualification

**Passed smoke qualification, not formal calibration or launch acceptance.**
The file-only analyzer authenticates retained evidence and reuses the production
reducers. It does not modify live scoring policy, evaluated source or the frozen
evaluator image. Three complete suites at the published code revision and the
predeclared fresh measurement campaign remain required.

## Exact tested snapshot

Independent execution used a fresh private checkout of server main
`af75616320e8fb43ee3021fe6d3e2480b08e7b83` plus the pending `calibration*.go`
and `main.go` changes. Actual Go 1.27.3/local, management CPUs 20,22,
GOMAXPROCS=2 and `-p=2` were used with synthetic disposable test services.
No main database or production vault was test infrastructure.

The latest targeted race invocation passed **55/55 named tests**, including one
default-no-op retained-artifact helper. That default helper is not an extra
regression or a genuine image qualification. Package time was 23.005 seconds;
whole managed smoke invocation was 113 seconds, exit 0, cleanup 0.

Full source-digest receipt SHA-256:
`c326a4f3bd1a36234bbd7729e9162c6f308082d0eed61736f1b67fc3843424f2`.
The operator files were independently checked against every receipt entry and
matched. Smoke log SHA-256:
`7c7f5bceb8a761c6ddf580dc3a63f714d7f3cf6e0f97249eb277bf612503af7d`.

Private evidence root:
`temp/calibration-control-id-independent-20261025.bAKHqEby/`.
Earlier proofs are retained separately under
`temp/calibration-prefix-independent-20261025.AVq8hbBI/`.

## Deterministic root-cause and adjacent proofs

Compatible Go AST body-only transplants reproduced **nine behavioral failures**
against the preserved old logic. Exact fixed-body restoration then passed every
corresponding regression; compilation failures were never counted as proof.

| Root cause | Reproduced broken behavior | Corrected invariant |
| --- | --- | --- |
| JSON null normalization | Null source identity or missing/null zero-valued policy field became an apparently valid zero value | Required identity/policy fields must exist and be non-null before typed canonicalization |
| Cleanup ownership | Failed removal discarded a corpus still mounted by an unreconciled container | Retain the private corpus and report its path until owned cleanup is proven |
| Host umask | Copied directories became 0700 and inaccessible to the remapped scorer | Explicit 0755 copied directories/0444 files beneath an unchanged private 0700 parent |
| Declared failure shortcut | Another malformed cohort bypassed validation; failed partial records inflated the validated count | Validate successful complete cohorts and never count failed partial observations as validated |
| Control identity omission | Reused controls, control/candidate aliases and missing identities could masquerade as fresh R9 data | Bind original control sidecar IDs and reject reuse across cohorts and both comparison roles |

Initial fixture failures were also retained: private roots were not explicitly
chmodded, and tiny-request fixtures lacked the official throughput metric tier.
The fixtures now establish their private boundary and use the existing official
baseline fixture options. No production gate or privacy check was relaxed.

## Actual frozen-image replay

After restoring fixed bodies, an explicitly selected root-run helper acquired
the existing singleton lock itself, without an outer flock, and ran the original
immutable evaluator image against the retained completed cold cohort. Its fresh
read-only corpus copies preserved the original `/artifacts` namespace; no original
sidecar or evidence file was edited. Both baseline reconstruction and candidate
scoring reproduced the retained JSON **byte-for-byte**.

Latest replay: exit 0, 53 seconds wall time, 52.41 seconds test time. Final census
contained only the original persistent containers/networks, with no owned replay
container left. Replay log SHA-256:
`8a1b7f3d3a364faa6c85b69c850d4adf54ada48fd9dfa67db8a37706bb553e93`.
Executed race test binary SHA-256:
`1a22d9f7dd4493aa2a9bdc9d2d4d648d89ca00d6632e46b07052d7ef20fa34ae`.

The earlier snapshot's independent replay also passed (54 seconds), log SHA-256
`58e37e905087a32f1aa650b53f825fb3736db5bc7ba1615e79b0fd52a794ca98`.
These are replay qualification receipts, not new measurements, provider/host
attestations, an official baseline, or production activation.

Next: publish the smoke-qualified code, complete three full suites, bind the
actual analyzer executable and freeze the statistical protocol before collecting
any formal observation. The warm-reader pilot remains a failed faster-reference
screen and is excluded from the formal sample.
