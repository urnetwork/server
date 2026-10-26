# Launch critical path — October 26 checkpoint

Planning observation: **2026-10-26 04:48 UTC**. Fixed public opening:
**2026-10-28 00:00 UTC**, approximately 43 hours 12 minutes away. This is not
an activation receipt or permission to relax the frozen acceptance rules.

## Measurement path

The corrected-release [freeze](CALIBRATION-RESTART-FREEZE-2026-10-25.md)
remains authoritative. Positions 1 and 2 completed in 10,498 and 10,300 seconds
respectively, with complete R9/R9 evidence. Position 3's original runner and
evaluator were live at this checkpoint, 7/9 controls complete and no candidates
complete. Its original whole-job deadline is 06:41:45 UTC. Positions 4 and 5
remain unstarted; neither may overlap another measurement.

After positions 3–5, authenticate and preserve every result, run the qualified
Stage A analyzer and freeze its accepted margin. Only then publish the complete
five-seed Stage B commitment/order/build/input census before observing any
held-out result. Stage B requires 15 serial reference jobs: five cold jobs with
R9 controls plus R9 candidates, and ten jobs with nine fresh candidates and
their own round's authenticated shared control. It is not 15 new control sets.

There is **no defensible guaranteed finish time** yet. The three-hour bound is
a failure ceiling, not expected runtime. Fifteen jobs each consuming that
ceiling would take 45 hours before intermissions, already longer than the
remaining launch window. Candidate-only jobs should avoid control measurement
work, but their actual duration on fresh seeds has not been measured. Do not
convert that expected saving into a verified throughput claim.

## Work that can proceed without measurement interference

Prepare and inspect the held-out generator, request commitment and baseline
late-binding tooling; keep drafts explicitly unqualified. Reserve an exclusive
idle interval for their deterministic tests, not time inside a measured job.
Do not generate replacement inputs after seeing held-out outcomes. Publish
completed evidence promptly and keep intermissions limited to required
authentication, preservation, qualification and launch preparation.

Independent control-plane deployment and provisioning can proceed once the
maintenance hold is explicitly cleared. They must not inject builds, tests or
pressure workloads into this measurement host. Required external inputs remain
the dedicated review-provider key and an approved public patch-edit scope.

## Final release boundary

Passing calibration alone does not prove public readiness. Complete the live
artifact-storage/retention proof, host containment and rebaseline evidence,
provider-backed honesty-review/test qualification, exact runtime-manifest
installation, API/worker acknowledgments, supervisor singleton and monitoring
delivery. Activate admission for the announced opening only after inspecting
the corresponding real evidence. Do not silently waive a failed gate, reduce
replicates, substitute seeds, overlap scoring, or move the announced date.

This checkpoint records schedule risk explicitly so unverified assumptions do
not become a readiness claim. It does not change the protocol or its scope.
