# Service image package inputs

The seven Ubuntu service Dockerfiles pin both the base image and every added or
updated package payload. Their `runtime-packages` stage downloads only literal
URLs with literal `ADD --checksum=sha256:...` values. The final stage installs the
selected architecture's packages from a read-only stage mount with
`RUN --network=none`. It has no APT resolution or package-repository fetch. The
downloaded archives and the other architecture's files do not enter final image
layers through that mount. Taskworker also downloads one non-Debian archive, the
Chrome for Testing headless shell, the same way, and unpacks it in that offline
step; see [Taskworker headless shell](#taskworker-headless-shell).

`runtime-packages.lock.json` records the same inputs, their versions, sizes,
architectures and signed-index parents. It is a candidate input inventory, not a
deployment approval, vulnerability assessment or proof of a reproducible image.
Tests join all seven recipes to this lock and refuse moving package resolution,
missing hashes/payloads, online installation, tag-only bases, target omissions,
loss of proxy's curl package or of the headless shell's libraries, and an
unpinned, moved or online-unpacked archive. Existing binary paths and service
commands remain unchanged.

## Selected closure

The base is the existing Ubuntu 24.04 multi-platform index
`sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea`.
Inputs were selected from Ubuntu snapshot `20260927T000000Z`.

| Services | Additional or updated packages per target |
| --- | --- |
| API, Alt, Connect, Gossip, MCP | `ca-certificates` `20260601~24.04.1`; `openssl` and `libssl3t64` `3.0.13-0ubuntu3.15` |
| Proxy | The same three packages plus curl `8.5.0-2ubuntu10.15` and its complete selected dependency/recommendation closure: 22 packages per target |
| Taskworker | The same three packages plus the headless shell's libraries, fontconfig's configuration and `unzip` `6.0-28ubuntu4.1`, with their dependency closure: 39 packages per target |

Across amd64 and arm64 there are 105 distinct package payloads: eleven `all`
packages and forty-seven per architecture. Proxy's set holds 40 and
taskworker's 70; they share the five certificate payloads. Taskworker adds two
archives, one per target. A package already supplied by the fixed base need not
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
Taskworker's set is the exception: its arm64 image was built natively and every
maintainer script ran (below).

## Taskworker headless shell

The gift card renderer prints with Chrome for Testing's `chrome-headless-shell`
`157.0.8103.78` (revision 1717349), the Stable release on 2026-11-13, on both
targets. The lock types each target's zip as an archive, kept apart from the
`.deb` packages and their 8 MiB cap:

| Target | URL | SHA256 | Size |
| --- | --- | --- | --- |
| amd64 | `https://storage.googleapis.com/chrome-for-testing-public/157.0.8103.78/linux64/chrome-headless-shell-linux64.zip` | `f90329720fb61e5a76da9b6b509b6ad12a484c9c5df0359403e935177c6d153a` | 127,965,683 bytes (289 MB unpacked) |
| arm64 | `https://storage.googleapis.com/chrome-for-testing-public/157.0.8103.78/linux-arm64/chrome-headless-shell-linux-arm64.zip` | `912fbf877e53de34d7b3cca5104f3b95607d41bd35412ccc6cd2a599ff0c0dbf` | 128,047,138 bytes (293 MB unpacked) |

Both URLs are the Stable `chrome-headless-shell` entries in Google's
`last-known-good-versions-with-downloads.json` (stamped 2026-11-13T18:13:30Z) and
in the per-version JSON. Those manifests list only version, revision and URL; they
publish no digest. Each SHA256 is of the reviewed download. Each download also
matched the bucket's stored size, MD5 and CRC32C (`x-goog-hash`: amd64
`f95da2bc238dfe990d411546eb800e28` / `qqb3Pw==`, arm64
`6f640354e63f092ec2cb9d104f767706` / `ywkacQ==`). That confirms the bytes are
Google's stored objects, but it is not an independent signature. Each listing has
287 files under one top directory and no absolute, parent or link entries.

The download stage fetches each zip to its target's `/runtime-packages/<arch>/`,
so every platform build downloads both (about 256 MB). The install step's
`dpkg -i` installs `unzip`; then each target unpacks only its own zip offline and
moves the tree to `/opt/chrome-headless-shell`. The image sets
`WARP_GIFT_CHROMIUM=/opt/chrome-headless-shell/chrome-headless-shell`. The tests
refuse archives of different versions for different targets.

An earlier draft pinned 149.0.7827.55, the local test shell's version, for amd64
only: Chrome for Testing's linux-arm64 builds start at 153.0.8001.0. The owner
moved the pin to Stable so that every taskworker target renders. No other
Chromium source is used.

**Libraries.** `ldd` in the pinned bases reported 21 missing sonames on amd64 and
20 on arm64, where the shell does not link `libatk-bridge-2.0`. The signed noble
`Contents-amd64.gz` maps them to 18 main packages, all in the set for both
targets: `libasound2t64`, `libatk-bridge2.0-0t64`, `libatk1.0-0t64`,
`libatspi2.0-0t64`, `libdbus-1-3`, `libexpat1`, `libgbm1`, `libglib2.0-0t64`,
`libnspr4`, `libnss3`, `libx11-6`, `libxcb1`, `libxcomposite1`, `libxdamage1`,
`libxext6`, `libxfixes3`, `libxkbcommon0` and `libxrandr2`. 149.0.7827.55 needed
the same 21 on amd64, so the move changed no package. The bundled `libEGL`,
`libGLESv2`, `libvk_swiftshader` and `libvulkan` need nothing more. The shell
carries its own fontconfig and FreeType and links no system font library.

**Selection rule.** Taskworker's closure follows Depends and Pre-Depends, not
Recommends. With Recommends, apt's own simulation installs 104 packages per
target instead of 39, including systemd, the D-Bus daemon, Python 3 and ICU
(over the 8 MiB cap); the shell loads none of them. The declined
recommendations are `at-spi2-core`, `dbus`, `libglib2.0-data`, `shared-mime-info`,
`xdg-user-dirs`, `alsa-ucm-conf` and `alsa-topology-conf`.

`libgbm1` is the release pocket's `24.0.5-1ubuntu1`; every other package is the
snapshot's highest version. Noble-updates' `25.2.8-0ubuntu0.24.04.2` depends on
`mesa-libgallium` and so on `libllvm20`. That adds 16 packages on amd64 and 14 on
arm64 (55 and 53 in all). Three of them are over the cap: `libllvm20`,
`mesa-libgallium`, and `libicu74` (through `libxml2`). It also adds amd64-only
`libdrm-intel1` and `libpciaccess0`, which break the target pairs. The shell only
needs `libgbm.so.1` to load: the qualifying print ran with no Mesa driver
installed.

**Fonts.** With no fontconfig configuration and no font, Blink has no last-resort
font while a web font is still loading (`CreateLoadingFallbackFontData` hits
`NOTREACHED`). In an emulated amd64 run of the earlier 149 pin, small
`@font-face` files still printed, but text set in the 17.7 MB Noto Sans SC came
out blank, and the shell still exited 0. Whether a font wins that race depends on
timing.
`fontconfig-config` `2.15.0-1.1ubuntu2` and its first font alternative,
`fonts-dejavu-core` with `fonts-dejavu-mono` `2.37-8`, supply that fallback. The
cards still use only their own fonts.

**Qualified.** Two resolutions agreed on the same 39 packages and versions per
target. One was a small resolver over the verified tables. The other was apt's
own offline simulation against the same verified InRelease and Packages bytes.
All 70 payloads matched their signed SHA256 and size. Both images were built,
amd64 under emulation on an arm64 host and arm64 natively. In each image:

- `ldd` reports nothing `not found`, and `dpkg --audit` is clean.
- The gift renderer's own print test (`TestGiftCardsPrintPdf`, built from
  `controller/` for the target and run in the image with no network) printed all
  eight of its documents: two-card orders in en, es, de, zh, ru and ar, a lone
  friend's card and a 20-card order. Every page is 5x7 in and the text is
  selectable; the largest PDF is 1.8 MB, under the test's 4 MiB limit.
- Across the eight PDFs, Caveat, Geist, Geist Mono, League Gothic, Noto Naskh
  Arabic and Noto Sans SC are all embedded. The Arabic and Chinese cards render
  correctly, and the missing-shell test also passed.

Two no-cache builds per target, with the stub and the same epoch, reproduced each
platform manifest, its config and every layer: amd64
`sha256:bd2961670bed00bfd9271fb1780dab7fa817275cd43ebf976503da755a6be274` and
arm64 `sha256:1ae1583d5e72e21630d25d67cf6e68460c01f745ea8c00b6747c784dcdbb84ef`.
They used BuildKit 0.34.2 (docker driver) with Buildx 0.38.1.

**Not qualified:**

- a run on amd64 hardware, not emulation;
- the real taskworker binary or a real order: a static stub stood in for the
  service, and the renderer ran from its package test;
- Chromium's sandbox (the renderer passes `--no-sandbox`);
- the shell's vulnerability status beyond its being Stable on 2026-11-13.

### The printer's user

Chromium runs without its sandbox, and it lays out text a gift's buyer typed, so
it never runs as root. The same offline install step adds one unprivileged user,
`giftcard` (uid and gid 10001, home `/nonexistent`, shell `/usr/sbin/nologin`,
locked password, no supplementary groups), as four fixed lines appended to
`/etc/passwd`, `/etc/group`, `/etc/shadow` and `/etc/gshadow`. `useradd` would
write the build day into `/etc/shadow` and break reproducibility; fixed lines do
not. The renderer (`controller/gift_cards_pdf.go`) starts Chromium as this user
whenever the taskworker runs as root, gives it its own scratch directory, and
refuses to print if the user is missing. The recipe test pins the four lines and
refuses a root uid or gid, a login shell, a password, `useradd`, or their
absence.

The vault is out of this user's reach. warpctl bind-mounts the host's
`/srv/warp/main/vault` read-only at `/srv/warp/vault`; the directory is `0700
root:root` (its files are `0644 root:root`), so a non-root uid cannot traverse it.
`/root` (where `.aws` links into the vault) is `0700`, and the taskworker's own
`/proc/<pid>` entries (environ, mem, fds) are closed to another uid. The
renderer's package test checks each of these as the printer's uid inside both
images.

## Deterministic platform images

Pinned inputs alone did not reproduce the original image. Two independent API
builds changed `/var/log/dpkg.log` (wall-clock entries) and
`/var/cache/ldconfig/aux-cache` (filesystem-specific metadata), plus hundreds of
file timestamps. The install step now removes only those two disposable outputs
after package configuration and certificate generation, within the same layer.
It retains `/etc/ld.so.cache`, the package database, libraries and certificates.
Build logs retain package installation output for review. `SOURCE_DATE_EPOCH`
alone does not normalize bytes inside a package log or an inode cache.

All seven `warp_build_image` recipes pass a numeric `SOURCE_DATE_EPOCH` and use
the explicit image exporter with `push=true,rewrite-timestamp=true,unpack=false`.
Explicitly disabling unpacking is required on Docker-backed builders: their
default local unpack conflicts with timestamp rewriting because the original
build reference does not point to the rewritten layers. Images are still stored
and pushed with their attestations; only the builder's eager unpack is disabled.
The default epoch is the current source commit's Unix timestamp. An explicit
override is another release input and must be recorded; empty and nonnumeric
values fail before the builder runs. This value controls image config/history
timestamps, while the exporter normalizes layer file timestamps. Use a clean
checkout and the same recorded epoch, build context, toolchain and export
settings when reproducing.
Direct `docker buildx` invocations must supply both options as well:

```sh
docker buildx build --no-cache --platform linux/amd64 \
  --build-arg warp_env=review --build-arg SOURCE_DATE_EPOCH=1700000000 \
  --provenance=mode=max \
  --output type=oci,dest=review.oci.tar,rewrite-timestamp=true .
```

The example epoch is synthetic; use the recorded release value. Do not invoke
the publishing Makefile target for a local review build.

Two no-cache amd64 API builds and two no-cache amd64 proxy builds reproduced
their respective platform manifest, config and all compressed layer bytes with
BuildKit 0.33.0 / frontend 1.27.0 / Buildx 0.37.1. This is one-builder
qualification for two package sets, not every host or target. Taskworker's set
and archives reproduced the same way on both targets with BuildKit 0.34.2 /
Buildx 0.38.1 (above). Their top-level
OCI indexes differ because each retained provenance attestation describes a
different build invocation. Compare and retain platform descriptors and
attestations separately; do not drop or falsify attestations to make an index
hash repeat. API's existing maximum-provenance and SBOM publication flags remain
enabled. The other six recipes retain their prior policy and are not claimed to
produce API-equivalent attestations; aligning that release policy is still open.

## Updating the inputs

1. Select and review the base image index and its target manifests. Retain the
   exact base package databases for the target architectures.
2. Select an explicit Ubuntu snapshot. Verify its InRelease signatures with the
   independently trusted Ubuntu archive keyring. Verify signed Packages hashes
   and sizes before deriving or trusting any payload hash.
3. Resolve the complete service dependency/recommendation closure against each
   pinned base; taskworker follows its Depends-only rule and records any
   non-candidate version with its reason. Preserve proxy's curl requirement and
   taskworker's headless-shell libraries. Retain the exact downloaded
   `.deb` bytes and check every size/SHA256 against the authenticated tables.
4. Update the lock and corresponding literal Dockerfile inputs together. Run
   `GOWORK=off go test ./local/runtimepackages` and the race/vet equivalents.
   Qualify every package set offline on every release target, including package
   configuration and the actual application binary. Repeat no-cache builds with
   the same recorded epoch and compare the platform descriptors separately from
   attestations. Rebuild image/source locks, provenance, SBOMs and security
   assessments before deployment approval.
5. Change the headless shell only to another Chrome for Testing version, for
   every target at once, by each literal URL and the SHA256 of the reviewed
   download. Compare the bucket's size, MD5 and CRC32C, inspect each listing, and
   repeat the `ldd`, `dpkg --audit` and renderer print checks in each built image.

Do not turn the URL, digest, package or archive version or base digest into
overridable build arguments, remove `--network=none`, add `apt-get -f install`, or
substitute a moving mirror or another Chromium source when a retained URL fails.
A package or archive change is a new reviewed release input, including when the
old set needs security updates.

## Remaining MG-02 limits

- **Availability and archival:** checksums prevent silent byte drift, but do not
  keep upstream URLs or base manifests available. Canonical's snapshot service
  currently describes a retention intention of at least two years; the Chrome
  for Testing bucket's retention was not reviewed. The exact
  `.deb` files, the headless shell zips, signed indexes, target base
  manifests/layers and build tools still
  need a durable owned release archive and a tested restore path. An unavailable
  input must fail the build. This patch does not provide that archive or fallback.
- **Headless shell:** 157.0.8103.78 was Stable when pinned. Later fixes arrive
  only by moving the pin, which is as slow as any package update, while Stable
  moves every few weeks.
- **Scope:** the scratch Competitionworker image adds no OS packages. The root
  development Dockerfile and `connect/sim-latency/evaluator/container` have
  separate moving toolchain/package inputs; they are not qualified by this service
  lock and must not be represented as covered production recipes.
- **Image reproducibility:** BuildKit/frontend/toolchain identity, target
  manifests, prebuilt application bytes, the source epoch, package maintainer
  behavior and export metadata remain separate inputs. Future package changes
  can add different volatile output and require the same causal rebuild checks.
  The verified amd64 platform matches (and taskworker's arm64 match) do not
  establish repeatable attestation indexes, arm64 reproducibility for the other
  recipes, independent-builder reproduction, source-to-image provenance,
  operational readiness or vulnerability freedom.

References: [Ubuntu snapshot service](https://snapshot.ubuntu.com/),
[Ubuntu snapshot usage](https://ubuntu.com/server/docs/how-to/software/snapshot-service/),
[Chrome for Testing availability](https://googlechromelabs.github.io/chrome-for-testing/)
and [Dockerfile ADD checksum and RUN mount/network semantics](https://docs.docker.com/reference/dockerfile/).
Timestamp behavior is documented in
[BuildKit 0.33.0 reproducibility](https://github.com/moby/buildkit/blob/v0.33.0/docs/build-repro.md).
