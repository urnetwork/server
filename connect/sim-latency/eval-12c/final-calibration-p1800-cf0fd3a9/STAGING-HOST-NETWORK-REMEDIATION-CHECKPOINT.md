# Staging host-network remediation checkpoint

Production staging attempt 5 stopped before round creation, submission, or benchmark execution. The remediated worker had passed the earlier strict JSON contract issue, then correctly rejected a live host-qualification mismatch.

The mismatch was reproduced exactly. A force-killed `run-local.sh` wrapper had left `net.core.somaxconn=65535` and `net.ipv4.ip_local_port_range=10240 65535`; substituting only their frozen values (`4096` and `32768 60999`) changes the computed qualification from `983eedd15d0cb19135060e6601c6c4ac8c17da6aa56f6e5c620ce300cc9b902d` to the expected `9cb7a977f171babafb5ff35c045799cbd54ec734ecfdebe7ebd106e482683d2f`. The same prior run also left the helper's SYN backlog, netdev backlog, and conntrack expansion active.

The staging and service-gate launchers now pass the frozen network values into `run-local.sh`, reject a non-frozen host before work, and clean up only values that exactly match the known local-test tuning. An unrelated value is refused rather than overwritten, and conntrack cannot be lowered below its live entry count. These values are ample for the frozen p1800 scale and keep the runtime congruent with the season host attestation.

Both scripts parse and pass deterministic static tests for frozen, recognized-local, and external-change decisions. Root staging preflight passes. After restoring the host, three clean self-checks were byte-identical (`90f663d9ec73cafb5f3a382e63adaa568795a63ad0a2cf75d63994e24d4794a1`) and all returned the frozen qualification with every gate true.

The failed raw attempt remains retained locally as `production-readiness/.staging-round-attempt-05`; it is deliberately excluded from version control because `services.json` contains credentials. Its authenticated hashes and the full remediation contract are recorded in `production-staging-host-network-remediation.json`.

Frozen measurement source, evaluator, simulator, scorer, p1800 scale, R9 replicate count, baseline, takeover threshold, hidden-seed evidence, reference decision, and local-only secret boundary are unchanged. This checkpoint ends the host-network remediation phase; production staging is the next phase.
