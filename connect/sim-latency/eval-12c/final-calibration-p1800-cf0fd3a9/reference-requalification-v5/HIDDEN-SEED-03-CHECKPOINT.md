# Hidden reference seed 03 checkpoint

Captured at 2026-08-27T21:36:55Z from frozen evaluator source commit
`5ca3d5242f4a7d40efe4415635608023b05a0956` and evaluator image
`sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038`.

The third precommitted hidden seed failed the required strict shared-baseline
ordering in exactly one attempt per reference:

| Reference | Candidate score (ms) | Ratio to 40,671.910 ms shared baseline | Placeable |
| --- | ---: | ---: | --- |
| better | 46,993.398 | 1.155426 | no |
| no-op | 42,494.625 | 1.044815 | yes |
| worse | 70,818.696 | 1.741219 | no |

The observed order was `no-op < better < worse`, not the required
`better < no-op < worse`. This is a statistical ordering failure, not an
infrastructure failure. It is retained uncensored and was not retried. The
campaign therefore stands at two ordering passes from three completed seeds;
both remaining seeds must pass to satisfy the unchanged four-of-five gate.

The three evidence manifests contain 162 authenticated entries totaling
38,451,205 bytes. Every entry was replayed against its declared path, byte
count, and SHA-256 digest. Every worker result has the exact 15 true security
booleans and three non-empty isolation identifiers required by the evaluator
schema. The partial finalization audit reports six required passes, four pending
checks, and zero failures because the five-seed campaign is not terminal. At
capture time, seed 4 was active and the campaign service had zero restarts.

## Immutable bindings

- campaign commitment: `c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69`
- seed-03 result: `b25bd34552b8e467102bbb10bb6976876c227040dc8a29355f804c502383d304`
- 3/5 progress snapshot: `6327766768480860cb3d732baf03dc6ce17605d311a2cd06eb9379b02ac2c84a`
- refreshed preview: `96abe92b7f984ddab4e9cdbc8e8912e112ca4173df0b401590029a164bce0849`
- preview evidence: `dd7faa3adb20c8efc8117d20e50aeed52797eb1c8463b245c24fc40ce410a166`

No private seed, generated provider configuration, seed-reveal document, or raw
hidden seed material is included in this checkpoint.
