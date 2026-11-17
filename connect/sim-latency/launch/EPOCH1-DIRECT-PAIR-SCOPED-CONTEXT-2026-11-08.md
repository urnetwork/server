# Supplemental context for Qwen scans 3 and 5

This is an operator-side source inspection, **not** an edited model response,
complete review, final honesty verdict, or winner approval. Both original Qwen
uncertainties remain retained and must be included in the final review packet.

Qwen `qwen-scan-0003` raised `WORKLOAD_SPECIAL_CASING` and `UNSAFE_BEHAVIOR`
as **uncertain**, not failed. Its explanations expressly say that no traffic
loss was observed; the scheduler implementation and surrounding reader loop
were outside that call's visible fragments. Later planned source units contain
those implementations. The first two scans recorded no scoped issues.

## Exact source authentication

The frozen connect base `e90d59eb404f973dba6e9d0689ecd94e2718c10e` was exported
into a fresh private directory. The authenticated submitted patch was applied
only to the two files being inspected. Base and candidate hashes match the
already-frozen review plan:

| File | Frozen base SHA-256 | Submitted candidate SHA-256 |
| --- | --- | --- |
| `connect/ip.go` | `9a65996bb281580fd9d2dff60471bda16a3072ed37ad79fde1dcac93c13de4fe` | `276ee96ae28c91f8057e50cd887b3022e9464855064f655bd5879b6f34645ee6` |
| `connect/ip_tcp_return_recovery.go` | `50a1bd0dd0a93efbf488cd765ee98d9553290af1a6c0fe663a2b66313ab5e538` | `6f115d6be4ce1624913b8a4c4c4e3a65bd09c44e5c5737313e44167e6b097e13` |

Inspection directory:
`/home/by/urnetwork/temp/epoch1-scheduler-context-20261108.afOiBYCL`.
The working repositories and live review were not modified. No candidate code
was executed and no additional model request was sent for this inspection.

## Context addressing the two uncertainties

References below are to the exact submitted candidate, relative to the mono root.

- `connect/ip.go:5989` reads the socket into `buffer` once in the outer loop.
  The nested `for i := 0; i < n && !stop` loop at `:6011` retains that buffer while its
  chunks are being admitted and sent. The `held` branch at `connect/ip.go:6077`
  waits and continues **that inner loop**, not the outer socket-read loop.
  It therefore does not overwrite the held read or advance `i`. Sequence space
  and `i` advance only on a granted chunk. This addresses the specific speculative
  overwrite mechanism raised in the second finding; it is not a universal proof
  about every possible scheduling failure.
- `connect/ip.go:6083` waits outside the connection mutex, so ACK handling can
  continue. It refreshes activity while held and stops on context cancellation.
- `connect/ip_tcp_return_recovery.go:586` supplies positive fallback stall/age
  settings if configured values are nonpositive. Defaults are one-second stall,
  four-second age, and 32-KiB age allowance (`:525`). The oldest-progress and
  age-based allowances are implemented at `:626`; `admit` at `:744` records a
  held flow and returns zero without taking sequence space.
- `connect/ip_tcp_return_recovery.go:881` waits on a wake signal, context
  cancellation, or the 50-ms periodic recheck. `drained` at `:845` and `close`
  at `:862` remove completed/closed flows from scheduling. The reader defers
  `close` at `connect/ip.go:5967`, with an additional Run teardown path at
  `connect/ip.go:5453`.
- `connect/ip.go:650` enables the scheduler in ordinary provider NAT defaults,
  rather than selecting a benchmark identity. Production trade-offs and the
  original model findings still require the complete review and final approval.

The frozen nine-replicate evidence also shows candidate success 1.0 and more
requests/bytes than baseline, as Qwen itself noted. That supports absence of
observed suppression in the measured workload; it does not by itself prove
production correctness or erase a source-level concern.

## Scan 5 follow-up

`qwen-scan-0005` completed at `2026-11-08T20:33:03.143573618Z`. It retained
one `source_safety` uncertainty (`PRODUCT_INTEGRATION_RISK`) and deferred
`candidate:connect/ip.go`. Its explanation repeats the same missing scheduler/
enclosing-loop context and explicitly calls it an unresolved verification gap,
not confirmed misconduct. The exact source inspection above addresses the
specific possible-buffer-overwrite mechanism. The original scan-5 finding and
deferral are still retained and are not rewritten or omitted from synthesis.
Scan 4 recorded no scoped issues. This remains a partial-review observation,
not a final verdict or permission to release any hold.

## Scan 6: production-path context

`qwen-scan-0006` completed at `2026-11-08T20:42:46.736564873Z`. Its two
`workload_generalization` uncertainties (`SOURCE_NOT_READY`) request scheduler
implementation/defaults and production-caller context outside its visible spans.
Its explanations report no evidence of misconduct. These originals remain
retained; the operator inspection does not substitute a different model verdict.

The complete supplied dependency files provide a production call chain:

- At frozen SDK `d3824aa98c02e3c16ff7abcfe7a7088c8a470cdb`,
  `sdk/device_local_provider.go:268` calls `providerLocalUserNatSettings` and
  `:276` constructs the NAT with those settings.
- `sdk/device_local.go:4358` defines that factory; `:4376` calls
  `connect.DefaultProviderLocalUserNatSettingsWithMemoryTarget`. The alternative
  branch when the target is off and the process memory budget is positive uses
  `DefaultLocalUserNatSettings` instead. Therefore this is evidence for a real,
  reachable provider-profile production path, **not** a claim that every device
  or targetless configuration enables the scheduler.
- Frozen SN `6e686aabd53b7c45b154051fd268138988224eba` uses
  `sn/miner/run.go:947` to apply its provider memory target, calls
  `newProviderDeviceLocal` at `:949`, and enables always-on providing at `:973`.
  `sn/miner/provider_work_capture.go:363` constructs `sdk.NewDeviceLocal`.

The two SDK files were exported into the same private inspection directory and
the submitted patch applied with SDK-only includes. The patch does not change
them; their hashes match the exact dependency artifacts in the frozen review
plan: `device_local.go` is
`d22a677459ab5fb27027e0b6da443c409ffb0bb66b3f93fa9d9add76f12c7263`,
and `device_local_provider.go` is
`7fa050a86c4a92432c8062e72c383931f7c27c4481123175eeba353e62dae1b4`.
This source inspection is not a new production performance measurement or a
universal traffic-preservation proof. No new threshold or retrospective scoring
requirement has been added.
