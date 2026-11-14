# Preparation-tool qualification — October 26

**Unit guards passed; runtime DNS and formal calibration remain open.** This
intermission followed publication of cohort 2 as server `a16396abf`. All work
used actual Go 1.27.3/local, root, management CPUs 20,22, offline dependencies
and the canonical exclusive idle scoring slot. No measured evaluation overlapped.

The [versioned dataset](../baseline/prelaunch-input-tool-qualification-2026-10-26/README.md)
and [index](../baseline/prelaunch-input-tool-qualification-2026-10-26/INDEX.json)
preserve exact commands, source/log digests, workspace bindings, expected-failure
artifacts and readbacks. Full caches/checkouts, synthetic service configuration
and compiled binaries are deliberately excluded.

## Discovery/assembly helper

- Exact analyzer source: `30f340a2779699225ab40ec7f3c5d978525276d2` in a separate
  temporary checkout; all nine dependency commits independently reread.
- Unchanged helper SHA-256:
  `618a6e00a2545e404b275547c656cd888d892f444e5cac393ddc9eb32030f250`.
- Qualification source SHA-256:
  `e9b2f2b8633f1d446365b5c7d2a639c5a508f4ba86ec7f14b6888e945db7111f`.
- **14 race tests × 3 consecutive passes**, zero skips; wall 99/16/15 seconds.
  Positive synthetic discovery and all prescribed specific refusal assertions
  passed. Real-campaign opt-ins were unset; only synthetic qualification enabled.

This qualifies discovery/assembly guards, not score authenticity. It did not
assemble the actual incomplete campaign, create its analysis-inputs.json,
invoke the installed analyzer or select a margin. The real analyzer must still
authenticate all originals and replay all five fixed cohorts after completion.

## DNS measurement helper

- Probe source SHA-256:
  `1dbfd7099643b2f0f71f2273248d277296f22001246779e674129401afabe193`.
- Test source SHA-256:
  `1d110e7c6c1c983ac93c9d3b4d751189254b87abee925160586f55c1258c8ce3`.
- Seven race tests initially passed. Restoring byte-wise reply comparison
  deterministically failed the uncompressed-equivalent assertion. Restoring
  post-write observation deterministically failed the received-query assertion.
  Neither expected failure was a compilation failure.
- Exact source was restored after each control, then all seven race tests
  passed **three consecutive final runs**, zero skips: 28 positive executions
  and two expected behavioral failures in total. Both mutant source copies
  were reconstructed byte-exactly to their recorded execution digests.
- The restored Linux/amd64 CGO-disabled probe built successfully with Go 1.27.3
  and x/net v0.60.0. Binary SHA-256:
  `43a9771b2bbdfd30d8b4ded0d6182a1d1e32a211ac0c95ed8d18ac9830c3bab0`.
  Build metadata's modified flag truthfully reflects the untracked private
  qualification package, not a pristine evaluated application.

Only synthetic packet data and net.Pipe were used. No live DNS query, listener
or network containment probe ran. Default host-loopback forwarding remains
separate unproved runtime coverage. See [the execution scope](DNS-PROBE-PREPARATION.md).

## Existing reference build identities

Three existing immutable images were read using only trusted sha256sum and
Go build-metadata readers, with overridden entrypoint and no app execution,
rebuild, host mount or network. The receipts retain the requested nonroot,
read-only, capability and 128 MiB/PID/management-CPU bounds. Image inspection
binds each ID; this is not a new daemon-observed containment attestation.

| Reference | Candidate commit | Binary SHA-256 |
| --- | --- | --- |
| No-op | `33420e8d987d50b74818e40cc0d1c5d589521309` | `6bb3ebf65313cae72ab39d3a815680436e1cb8f9cf0042280626e1548943f865` |
| Proposed better snapshot | `0ecf8f90ccf1516f807b20f39f3e5f55d22f4b0c` | `468e0fb04984a2fce6c28bc2ec10171e26e25a0e5ebf969ec26eebf4a571c2cf` |
| Proposed worse warm-reader | `e445ffb75fc3305df6c1ccd5d99e768e45e1142e` | `fc3709dc5dcd2448f903902d801d0e5dff6ce0274142b012fd5c029892dc9b83` |

All three application readbacks report Go 1.27.3, the expected candidate
revision and `vcs.modified=false`. Full image IDs are bound in the index.
These current single-file-policy builds support prospective held-out input
preparation; they do not approve the public editable scope, create a hidden
seed plan, establish independent-seed ordering or promote a source winner.

The original three containers/four networks were restored and the slot was
free. Evaluated c1 source/image, installed scorer/analyzer/scripts, statistical
rules and all real observations are unchanged. After publishing this phase,
continue only the next original fixed cohort; never repeat completed cohorts
or select outcomes by direction/p-value.
