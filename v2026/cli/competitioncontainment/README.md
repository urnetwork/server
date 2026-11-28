# Private shared-baseline containment evidence

`competitioncontainment` is a root-only evidence authenticator, not an evaluator
or readiness override. It verifies the signed terminal response, exact run
manifest/role, explicitly approved operator patch and shared-baseline digests,
bounded artifact bytes, placeable worker score and exact completion identity.
The native score projection deliberately omits provenance fields; the baseline
is authenticated through the signed completion artifact and materialized bytes.

It writes a new mode-0600 binding under protected root-owned ancestors. Authority
and artifact reads reject intermediate symlinks and use held directory
descriptors. Goofe performs this locally; no coordinator keys or main vault
material need to be copied to it.

For this explicit private class, `promote-host-containment.sh` accepts the binding
and agent configuration with their exact SHA-256 pins. It checks the imported
baseline and exact local candidate-container count instead of requiring a second
local control measurement. Every existing score, security, resource-bomb,
artifact, mount, map and cleanup check still applies. Legacy promotion continues
to require local baseline and candidate evidence.

The binding grants no grading readiness. Full native qualification, retained
evidence, the final executor amendment, official R9 baseline and two-host
admission remain separate activation requirements.
