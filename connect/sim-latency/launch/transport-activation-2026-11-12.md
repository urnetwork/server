# Transport worker activation — 2026-11-12

The operator authorized activation at a verified idle boundary. Both native
executors had completed with exit zero and verified cleanup; fresh authenticated
status and availability responses were verified before replacing the controller.

The evaluation hold advanced from revision 27 (inactive), through 28 (active),
to 29 (inactive). Intake remained open. The old systemd invocation and its
processes closed before the replacement started. The old controlled-stop exit
was nonzero and is retained, not described as a clean exit.

Activated worker:

- Source: `0aa7513825f7f1cfa1cc62bb88ca1d87244d1c48`.
- Binary SHA-256: `009dce62e7ddd90b377252122bb0b262e7e697e0c0488bb829af214a3ac26719`.
- Runtime image: `sha256:2e6d6c076589a0f3602726cbe4f348ed94dee7e90e5362fac6d1ce45196e5b36`.
- Supervisor invocation: `ecfd3b4b026a46b5ad4065012baddf54`.
- Private handoff evidence: `/var/lib/urnetwork/competition-transport-handoff-20261112.iqaSCVwA`.

The hold prevents fresh per-host worker preflight as well as claims. Accordingly,
the activation proof distinguishes the global baseline/retained-qualification
checks completed under hold from the fresh per-host preflight enforced by the
worker before claims after release. It does not claim the latter ran under hold.

At 15:17 UTC, both central execution slots were empty. Sille's expired job
`01a1cde8-1ba2-0703-1cc6-0b1ce3ac796c` reconciled at fence 19 to **failed**, with
its original submission timestamp, 09:20:10 start, and 12:20:10 deadline retained.
Native completion alone is not a centrally retained successful score. No new
recovery cycle or deadline extension was granted during this activation.

The API advertised open epoch 2, intake ready, grading ready, and publication
not held. The supervisor had zero restarts. Aggregate central status was
16 succeeded, 4 failed, none running or queued. These are point-in-time checks,
not a guarantee about future submissions.

The separate distributed-recovery feature remains unqualified for deployment:
183 of its original 260 top-level tests were qualified, with the remaining 77
resumed after handoff. This transport activation does not deploy that feature
or authorize schema-2 recovery grants with the transport-only worker.
