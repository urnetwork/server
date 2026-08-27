# Hidden reference campaign terminal checkpoint

Captured at 2026-08-27T23:42:11Z after all five precommitted v5 hidden seeds
completed. The authorized launch screen passed exactly four of five strict
shared-baseline orderings. Seed 3 is the sole failure and remains uncensored,
with no retry. All 15 reference evaluations have exactly one attempt.

The terminal audit authenticates 836 manifest entries totaling 195,988,712
bytes, every per-seed result, the canonical terminal seed-result aggregate,
all 15 evaluator security projections, the campaign commitment, the two reveal
schemas, the accepted decision, and the final attestation. It reports seven
required passes, three downstream pending gates, and zero failures.

## Terminalization defect and repair

The original systemd service completed every measurement and sealed the
accepted decision, then exited 1 because its inherited attestation function
looked for the generic independent-seed reveal path while the Python wrapper
had sealed the distinct v5 public terminal reveal. The first attempted repair
incorrectly mirrored the terminal document byte-for-byte and was rejected by
the audit because the schemas intentionally differ. That failed attempt is
retained privately for forensic review and is not exported here.

The final deterministic repair reconstructed the generic campaign reveal from
the original private rounds, rederived every UUID-bound seed commitment and
generator seed, and matched each public projection to both the premeasurement
campaign commitment and sealed terminal reveal. It then re-entered only the
frozen runner's existing-decision branch to create the attestation. Attempt
snapshots were byte-identical before and after; no statistical measurement,
seed result, worker result, or evidence manifest changed.

## Immutable bindings

- source lock: `0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838`
- frozen hidden runner: `a889248bf2b2175e79ce0f5526cfa294fea17700f56f6eec46eda8f53aae519e`
- campaign commitment: `c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69`
- terminal progress: `f2bbe8797dc463bb85d576cea29575579757df3eb7109a2170fd0566008d1e8b`
- terminal decision: `3e4cc70d783b01a87328736caf82f49016138c97ff384b26dc38864f8cede835`
- terminal result aggregate: `21c39ae83d314983c82bd84672561c22d3eba792e7009b3b70a410246f47526f`
- generic campaign reveal: `06c80eb4516125d8074a1293e0586e2928d03754776c6656da783bcb5ed465ac`
- public terminal reveal: `57435cb82f1a0d4689f1ba32d56fba6483d8fc233eb3569697171c981aad3441`
- final attestation: `b96b216022b34e2bf0e9838ca51380431d9f97e95121610951c37d0274cc5c02`
- repair script: `a5bfedfd7228b8e7c01a41334aa01b0d6a413ffadc4cca380073ac9ecdb668a0`
- repair evidence: `499efd5e6d99f4d56a55f05d3949f6107ae8fcdeb2c7dfeb5b9877207541412d`
- staging promotion amendment: `618393539636b69cfcdbd6fec14afef3e58fe20d43bda06fbcbf15693802b695`
- refreshed preview: `de87693f841325a6f97b82a6a88eac8da66bbfd6397377355188a2b6c14f5300`
- preview evidence: `e0922c26c83d486bfc0cb67485aa54d547709314e22a2a2345824909b4b369cd`

Neither reveal document, any private round file, generated provider
configuration, or raw hidden seed material is included in the evidence branch.
