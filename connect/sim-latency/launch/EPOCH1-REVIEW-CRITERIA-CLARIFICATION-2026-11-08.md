# Epoch-1 review criteria clarification

Operator clarification: “We have to judge the submission based on the criteria we established with ttfb and throughput.”

Performance eligibility and ranking use epoch 1's frozen TTFB and throughput/volume criteria, numerical gates, significance test and sealed evaluation results. Do not introduce retrospective high-RTT, single-flow goodput, scheduling-window or other performance thresholds.

Use the actual frozen scorer, not a replacement formula inferred from shorthand: its existing raw latency calculation uses p95 `total_ms` with failed-row timeout penalties. This clarification does not substitute a TTFB-only score.

A generic scheduling cap or QoS trade-off is not, by itself, dishonesty or disqualification when the agreed gates pass. Review must not demand improvement under every conceivable network condition.

The established honesty requirements remain: no fabricated measurements, protected-instrument tampering, benchmark-specific shortcuts, intentional useful-traffic suppression, or substitution of simulation-only behavior for production-applicable behavior. Investigate concrete correctness and safety defects and report uncertainties without inventing new numerical gates.

This clarification does not change the running qualification's source, prompts, controls, model membership, limits, owner or deadline. A direct source review is not a fabricated Qwen/Gemma verdict, and neither it nor this document authorizes winner approval, rewards or source promotion.
