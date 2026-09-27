# Service image package inputs

The seven Ubuntu service Dockerfiles pin both the base image and every added or
updated package payload. Their `runtime-packages` stage downloads only literal
URLs with literal `ADD --checksum=sha256:...` values. The final stage installs the
selected architecture's packages from a read-only stage mount with
`RUN --network=none`. It has no APT resolution or package-repository fetch. The
downloaded archives and the other architecture's files do not enter final image
layers through that mount.

`runtime-packages.lock.json` records the same inputs, their versions, sizes,
architectures and signed-index parents. It is a candidate input inventory, not a
deployment approval, vulnerability assessment or proof of a reproducible image.
Tests join all seven recipes to this lock and refuse moving package resolution,
missing hashes/payloads, online installation, tag-only bases, target omissions and
loss of proxy's curl package. Existing binary paths and service commands remain
unchanged.

## Selected closure

The base is the existing Ubuntu 24.04 multi-platform index
`sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea`.
Inputs were selected from Ubuntu snapshot `20260927T000000Z`.

| Services | Additional or updated packages per target |
| --- | --- |
| API, Alt, Connect, Gossip, MCP, Taskworker | `ca-certificates` `20260601~24.04.1`; `openssl` and `libssl3t64` `3.0.13-0ubuntu3.15` |
| Proxy | The same three packages plus curl `8.5.0-2ubuntu10.15` and its complete selected dependency/recommendation closure: 22 packages per target |

Across amd64 and arm64 there are 40 distinct payloads: four `all` packages and
eighteen per architecture. A package already supplied by the fixed base need not
be downloaded again. `dpkg` must configure the entire selected set successfully;
there is no resolver, forced dependency acceptance or online repair fallback.
An unsupported base architecture has no matching input directory and fails.

Qualification verified Canonical's InRelease signatures using archive key
`F6ECB3762474EDA9D21B7022871920D1991BC93C`, then followed each signed SHA256 and
size to the compressed Packages table and each exact `.deb`. Every retained
payload matched. The lock records the selected signed-parent and package-table
hashes; original signature/index/package bytes are retained in the qualification
bundle. These references do not claim that the Docker build independently repeats
GPG verification: the reviewed Dockerfile checksum is its package trust boundary.

Both an actual amd64 API image probe and an actual amd64 proxy image probe passed
offline installation, package/version inspection and embedded-binary comparison.
Arm64 uses the real pinned arm64 base package database for an offline dependency
simulation, with an omitted curl dependency as a negative control. That is not
execution of arm64 maintainer scripts or an arm64 image qualification. A native
or approved emulated arm64 build remains required before publishing that target.

## Updating the inputs

1. Select and review the base image index and its target manifests. Retain the
   exact base package databases for the target architectures.
2. Select an explicit Ubuntu snapshot. Verify its InRelease signatures with the
   independently trusted Ubuntu archive keyring. Verify signed Packages hashes
   and sizes before deriving or trusting any payload hash.
3. Resolve the complete service dependency/recommendation closure against each
   pinned base. Preserve proxy's curl requirement. Retain the exact downloaded
   `.deb` bytes and check every size/SHA256 against the authenticated tables.
4. Update the lock and corresponding literal Dockerfile inputs together. Run
   `GOWORK=off go test ./local/runtimepackages` and the race/vet equivalents.
   Qualify both package sets offline on every release target, including package
   configuration and the actual application binary. Rebuild image/source locks,
   provenance, SBOMs and security assessments before deployment approval.

Do not turn the URL, digest, package version or base digest into overridable
build arguments, remove `--network=none`, add `apt-get -f install`, or substitute
a moving mirror when a retained URL fails. A package change is a new reviewed
release input, including when the old set needs security updates.

## Remaining MG-02 limits

- **Availability and archival:** checksums prevent silent byte drift, but do not
  keep upstream URLs or base manifests available. Canonical's snapshot service
  currently describes a retention intention of at least two years. The exact
  `.deb` files, signed indexes, target base manifests/layers and build tools still
  need a durable owned release archive and a tested restore path. An unavailable
  input must fail the build. This patch does not provide that archive or fallback.
- **Scope:** the scratch Competitionworker image adds no OS packages. The root
  development Dockerfile and `connect/sim-latency/evaluator/container` have
  separate moving toolchain/package inputs; they are not qualified by this service
  lock and must not be represented as covered production recipes.
- **Image reproducibility:** BuildKit/frontend/toolchain identity, target
  manifests, prebuilt application bytes, timestamps, package maintainer-script
  output and image/export metadata remain separate inputs. Immutable package
  bytes do not by themselves establish byte-identical OCI rebuilds, source-to-image
  provenance, operational readiness, or vulnerability freedom.

References: [Ubuntu snapshot service](https://snapshot.ubuntu.com/),
[Ubuntu snapshot usage](https://ubuntu.com/server/docs/how-to/software/snapshot-service/),
and [Dockerfile ADD checksum and RUN mount/network semantics](https://docs.docker.com/reference/dockerfile/).
