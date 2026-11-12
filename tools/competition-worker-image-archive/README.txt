Private operator verifier, not runtime code or deployment authority.

This Go1.27.4 standalone module uses the standard library only. It reads two
existing regular artifacts, never extracts an archive, invokes Docker, executes
a payload or contacts any service. The supported format is deliberately closed:
one Docker-save image, OCI index/manifest/config references in agreement, one
uncompressed scratch layer, and one root-owned /competitionworker payload.
Every blob, full archive and native payload must match independent expected
SHA-256 pins, plus exact tag/revision/user/linux-amd64/entrypoint metadata.

The declared numeric nonroot user must receive an immutable 0555 payload. A
0700 payload is the known packaging permission error ONLY after all byte,
config and layer checks passed. Wrong bytes with 0700 remain an identity error.
No checksum or framing error can be treated as the expected permission defect.
Limits are owner settings; cancellation or uncertainty never prints success.

Run under bounded management resources. Synthetic tests:
  GOWORK=off go test -count=3 .
  GOWORK=off go test -race -count=3 .

Reviewed root supplies native/archive paths and exact public build digests:
  verifier --native ABSOLUTE_NATIVE --archive ABSOLUTE_DOCKER_SAVE \
    --native-sha256 EXPECTED_NATIVE --archive-sha256 EXPECTED_ARCHIVE \
    --image-id sha256:EXPECTED_CONFIG --tag EXPECTED_TAG \
    --revision EXPECTED_SOURCE --user 65532:65532

Exit zero requires passed=true/code=verified. The preserved original 0700 image
must exit one with passed=false/code=payload_permission, not setup/hash failure.
The corrected archive must exit zero and report the SAME native/payload digest
and byte length with payload_mode=0555. Neither result authorizes activation.
The v1 archive/native must remain untouched and no native probe is needed.
