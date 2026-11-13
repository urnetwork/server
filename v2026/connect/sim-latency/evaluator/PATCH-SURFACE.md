# sim-latency submission patch format

Updated 2026-11-02; replaces the August single-file development policy.
The authoritative advertised surface is `patch_policy` in
[`GET /competition/info`](https://api.bringyour.com/competition/info).
Use the [source commits frozen for the round](../EPOCH-SOURCE.md), not moving `main`.

## Editable surface

Production epoch 1 uses `path_base: mono_root_v1`, a maximum decoded patch size
of **262,144 UTF-8 bytes**, and these prefixes:

```text
server/**
connect/**
sdk/**
proxy/**
glog/**
goidenticons/**
userwireguard/**
sn/**
warp/**
gvisor/**
```

**`server/connect/sim-latency/**` is protected.** Repository metadata (`.git`
at any depth), traversal, and unsafe paths are also rejected.
A patch may touch one repository or several; **there is no requirement to
modify `server`**. Product and test files in the separate `connect` repository
are eligible under `connect/**`. The server's resident contract manager is
`server/connect/resident_contract_manager.go`, not `connect/resident_contract_manager.go`.

## File operations

Explicit mono-root mode permits modifying, creating, and deleting regular text
files, including empty files. Supported regular modes are `100644` and `100755`;
modifications preserve the existing mode. Renames, copies, mode changes, binary
patches, symlinks, and submodules are rejected. Use `git diff --no-renames` so
Git does not convert a delete/add pair into rename metadata.
Build constraints are not structurally banned in mono-root mode; they do not
exempt a submission from correctness and honesty review.

The single-file `container/policy.example.json` remains a **historical restricted
development/test fixture**, not the production policy. Its additional exclusions
must not be substituted for the advertised mono-root policy.

## Canonical wire format

Submit a unified Git diff, not a branch, commit id, or `git format-patch` email.
The API hashes the exact decoded JSON string without normalization:

- nonempty valid UTF-8; no NUL, CR, or controls other than LF and tab;
- LF-only line endings, exactly one final LF, no extra trailing blank line;
- lexicographically sorted file sections, with each path appearing once;
- `diff --git a/REPO/PATH b/REPO/PATH`, matching file headers, and accurate
  unified-diff hunk counts; creation/deletion uses standard `/dev/null` headers
  and regular-file mode metadata;
- canonical, unquoted paths without whitespace, escaping, or traversal.

The trusted source applicator uses `git apply --whitespace=error-all`.
`git diff --check` must print nothing: added trailing whitespace, bad indentation
whitespace, and extra blank lines at EOF can fail application after screening.
New/deleted nonempty files require one complete hunk with canonical zero-side
coordinates. Git's standard empty-file metadata is accepted.

For existing-file changes in the separate `connect` checkout:

```bash
cd /path/to/mono-root/connect
git diff --check
git diff --no-renames --src-prefix=a/connect/ --dst-prefix=b/connect/ > ../submission.patch
```

For multiple repositories, concatenate their diffs in alphabetical repository
order, retaining per-file path ordering. `git diff` omits untracked files;
stage intended new files and use the appropriate `git diff --cached` to include
them. Do not accidentally include unrelated staged changes.

Preserve the patch file's bytes through the current Apex CLI. Direct API
integrations should use `jq --rawfile` and HTTP `--data-binary`, not shell command
substitution, which strips the final LF. Use the round's advertised v2
preflight/submit endpoints and returned status URL; see the
[integration contract](../README.md#macrocosmos-integration-v2-contract).

## Screening is not evaluation or honesty approval

An accepted patch must still apply to the exact checkpoint, build offline
against available dependencies, pass G1–G6, and meet the round's margin and
significance requirements to be eligible to win. No new dependency download is
available. An editable product path does not authorize instrument tampering,
fabricated results, disabled trusted checks, benchmark gaming, escape, or secrets
access. Isolation, read-only local config/vault mounts, resource limits, artifact
authentication, and winner review remain mandatory.

The structural implementation is `ValidateAndCanonicalizePatch`,
`validateRepositoryPath`, and `pathAllowed` in
`server/controller/competition_controller.go`, followed by the trusted
multi-repository source applicator. Single-file calibration references are
examples, not an exclusive editable surface.
