# Corrected frontier preparation — October 24

This is exploratory capacity evidence, not an official hidden baseline or
production runtime qualification. The corrected image is
`sha256:79400f3375c64767f5021e43ef852e9ff9da3ca5a4b79dc03e8e9ed381247eb9`;
its frozen server source is `3d96d16ddb4fc2fb08d3f9003c52e30b9a47241d`.
All ten source repositories are extracted afresh from that image. No operator
checkout or main/all configuration is mounted into the evaluation container.

## Retained preparation failure

The first 1,800-provider, 200-client impairment-off attempt stopped before
simulation at OCI container initialization: `mkdir /workspace/server: permission
denied`. The standalone frontier controller called the source extractor but
omitted the production evaluator's subsequent `seal_evaluation_source` step.
Extraction deliberately creates a mode-0700 operator-owned directory; it is
not directly usable as the remapped non-root runner's source bind.

PostgreSQL migration completed, but no candidate measurement ran. The
controller returned 1 and removed its PostgreSQL, Redis and Compose network.
This attempt must not count as a measurement or as submission failure evidence.
Its log and service diagnostics remain under the mono-root-relative directory
`temp/corrected-frontier-20261024.tKi1emrK/p1800-off` and adjacent
`p1800-off.log`. No production evaluator code was changed: it already seals
source before starting a runner.

## Corrected standalone controller

A separate immutable controller applies the same directory/file read-only
permissions, preserves executable bits and sets the source's mapped ownership
to UID/GID 165532 before creating the read-only Compose bind. The old controller
and failed attempt are retained unchanged. The new controller is
`temp/corrected-frontier-controller-v2.7E1xvLv2/run-case.sh`, SHA-256
`a3af6c55ff940dfeb4319a83376e33eabcf9d8bc943aeb7d4378083375635b60`.
Its root-owned directory is 0750 and script is 0440.

The next attempt has the distinct label `source-seal-fix`. Its status must be
established from the actual process exit, CSV/final marker, mode metadata,
service diagnostics and cleanup evidence; launch does not imply success.
It uses the same global exclusive scoring lock, refuses an active staging
supervisor, and retains the 40-minute outer safety bound. Builds and tests must
not overlap timed scoring. Neither attempt activates production or writes the
main control plane during the maintenance hold.

At 19:58 UTC the corrected attempt reached simulation. The actual parent and
all four child fleet process argument lists carried `--no-impair`. The retained
`process-argv.txt` SHA-256 is
`08500936495575747d22cb009c9b32905ed15c2cccdd1c7ef849f5bc76250d92`.
Docker inspection independently showed `/workspace`, `/input`,
`/runtime/config/local` and `/runtime/vault/local` read-only; only `/artifacts`
was a writable bind. This proves the observed mode forwarding and bind layout,
not completion, statistical stability or full containment qualification.
