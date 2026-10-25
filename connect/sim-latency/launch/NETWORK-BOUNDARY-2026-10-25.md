# Job network host-gateway containment

## Confirmed root cause

An internal Docker bridge still has a host gateway address. A constrained,
non-root, read-only, capability-dropped instance of the qualified evaluator
completed a nonce echo with an explicitly owned host listener. No main port
was scanned and no sensitive data was sent.

Original evidence: `temp/network-negative-independent-20261025.ytP5xUGQ/`;
launch SHA-256 `e07fb84866640a0ec0fa94a5171f37636d149b5cc623defb2312ebe411feba19`.
This is a demonstrated containment defect, not a simulation instability.
[Docker's gateway-mode documentation](https://docs.docker.com/engine/network/port-publishing/#gateway-modes)
describes the distinction between internal and isolated gateways.

## Correction and adjacent IPv6 behavior

The per-job Compose network now requires both gateway modes `isolated`, remains
internal/non-attachable and disables external IPv6 networking. No global daemon,
host firewall or host sysctl is changed. The live helper checks actual network
options and the actual host bridge before migration or candidate preflight,
after only trusted backing images have started, then checks again after the run.

Attaching peers creates an automatic host IPv6 link-local address even on this
IPv4-only network. An initial address-free assertion failed closed before any
candidate code. That failed attempt is preserved, not counted as a successful
qualification: log SHA-256
`5c8718058f97d6652a05cf49e3cd98701165f961d9589ff00e01bc7ce2032601`.
The read-only diagnosis captured an empty bridge before attachment and the
kernel-generated link-local address afterwards. It was not a jq failure.

Every job container now explicitly sets namespace-local
`net.ipv6.conf.all.disable_ipv6=1` and `net.ipv6.conf.default.disable_ipv6=1`
before its processes run. Live inspection requires those settings for runner,
PostgreSQL and Redis; Compose tests require them for migration and scorer too.
All host IPv4 and global IPv6 addresses remain rejected. Only the automatic
`fe80::/64` link-scope host address is allowed, with job IPv6 disabled. Failed
network checks retain the observed network/interface JSON for diagnosis.
Container OS networking is IPv4-only; this does not change userspace simulated
packet formats or grant new application permissions.

## Independent runtime proofs

- Same live owned IPv4 host listener: normal internal bridge echoed; isolated
  bridge could not reach it. Peer-to-peer IPv4 traffic still worked.
  Evidence `temp/network-isolated-independent-20261025.I4PVsVEz/`, launch SHA
  `2aa3ba6af8c0e6c8cc6a666f45dc0456889982e57bb0a1222ff6fe273b38ac7a`.
- Both peers had actual `all/default/eth0=1`, no IPv6 addresses/routes and
  successful IPv4 nonce traffic using a Docker DNS alias.
  Evidence `temp/network-ipv6-disabled-20261025.unfNwrYM/`, launch SHA
  `65429898b0bdbd4cad194f47ab14a5096ae0d7b3271b5b0cf98d06891e67e7d5`.
- A scoped host IPv6 link-local listener echoed from the host, but the same
  live listener was unreachable from the IPv6-disabled container. The IPv4
  Docker-DNS peer control still passed. Evidence
  `temp/network-ipv6-nonce-corrected-20261025.zO02FwNb/`, launch SHA
  `1e12395ca6c84af9e4e2e335afcf1b8ae2f445903501e6bb00719858ac0a1dd8`.
  An earlier unscoped positive dial failed with `invalid argument`; it is kept
  separately as test setup failure, not containment proof.

All tests used owned temporary resources and exact image
`sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3`.
The evaluated server `c1f9dda60`, image and source lock were not changed.
External DNS recursion is not yet directly proved by these tests; Docker alias
resolution is an internal positive control, not an external-DNS negative proof.

## Deterministic tests and updated lifecycle smoke

Five new top-level Go tests cover the actual Compose policy, rejected network
metadata, IPv4/global-IPv6 host addresses, admission before runner execution and
required disabled-IPv6 settings in every live job namespace. Existing synthetic
inspection fixtures add only the new required policy fields.

The targeted race suite passed **74 top-level tests**, exit 0, whole 106 seconds,
package 15.980 seconds; log SHA-256
`44ebc3f13c067477e053196f686bac956564ad4eda16efbed98da2a1cdcc7416`.
Restoring only the old Compose template deterministically failed the new
host-gateway assertion; log SHA-256
`0938f49c5402a015e8a36bafb96081eec38e9b8ac6c247bc3f95292c0cf822ae`.

The corrected real 32-provider/8-client Docker smoke passed in **183 seconds**,
exit 0: fresh PostgreSQL/Redis, migrations, preflight, complete simulation,
accounting/resource evidence, baseline/candidate score and networkless pristine
scorer. Log SHA-256
`648a207f543d8d4030327170729a17e8179262efd63e84502852043e0ab7ca84`.
Evidence: `temp/network-boundary-v2-smoke-20261025.0fvPWzbw/`.

Every terminal cleanup restored original three containers/four networks; owned
listeners were joined, the canonical slot was free and the exact image/tag was
unchanged. Go 1.27.3/local and management CPUs 20,22 were used. These are trusted
host-script changes, not a candidate patch or newly accepted baseline. Publish
the smoke-qualified changes before larger validation, install a new immutable
script bundle including `network-boundary.sh`, and bind its exact identities in
the prospective fresh measurement protocol. Main maintenance remains a hold on
production activation and main database/service mutations.

## Additive local release installation

The exact published source `737dd237129671921a2f19b21d2d90cc9d4e0e95`
was archived into the new root-owned, non-writable release directory
`/usr/local/libexec/urnetwork/competition-737dd2371-go1273-isolated-v1/`.
No previous release, config pin, running worker or main service was changed.
Readback identities:

- `container/evaluator.sh`:
  `263ee68ab058889c22de51d47d8779d95fd219e92ea7642430f61740d15d8d5e`
- `container/network-boundary.sh`:
  `9ab82a9a8bb2272934461472509c15df4242f14859351dadd6dfcb2a1cd27cb7`
- `container/compose.yml`:
  `f4ca3a5eedfd5f081fbb507fc0102fa03ce11c095b406bcc0f7dc0119b5e0464`

The installed evaluator passes Bash syntax validation. No release entry is
group/world writable. Installation is preparation, not activation or formal
calibration acceptance. A new campaign must bind this bundle rather than
resuming the failed campaign pinned to the previous script.

## Complete script/control validation

Three consecutive full suites on exact published source `737dd2371` passed
**514 top-level tests each**, exit 0, with Go 1.27.3/local and management CPUs
20,22. Whole-suite durations were 321, 163 and 162 seconds. Original evidence:
`temp/network-boundary-full-three-20261025.CnXzer9z/`.

- Pass 1: `702c99625305054c9871f412172e789a5beda5fe45ee2ee893d4288747c6ac84`.
- Pass 2: `287364682fdf69317176cfb1739421b5a74b2eb92c7653a571f822bb2dd7c63e`.
- Pass 3: `523a60927d3919fbec5c182ac695c5e632615ada610db76cc3316524caeb9616`.

The original manager terminated with `final_cleanup_exit=0`; the private source
is clean, the original three containers/four networks are restored and the
canonical scoring lock is free. No scoring workload overlapped these suites.
This closes the three-suite release gate, not calibration or live readiness.

## External DNS follow-up: exact engine source review

Read-only `docker version` identifies the installed engine as **29.9.0**, build
`a5b58c9`. Its exact upstream
[resolver initialization](https://github.com/moby/moby/blob/a5b58c9/daemon/libnetwork/sandbox_dns_unix.go#L55)
sets external-proxy eligibility from sandbox external access. Its
[forwarding implementation](https://github.com/moby/moby/blob/a5b58c9/daemon/libnetwork/resolver.go#L476)
skips host-namespace loopback forwarding when that eligibility is false;
non-loopback upstream dialing runs in the container namespace. This supports
the intended containment reasoning, but is **source evidence, not runtime
qualification**. Internal alias resolution alone still does not prove blocked
external queries.

At a clean scoring intermission, qualify DNS without changing this running
campaign or host resolver/firewall settings:

1. Use an owned, bounded UDP DNS listener and a generated non-sensitive nonce
   under `.invalid`; answer only that nonce with an RFC-reserved test address.
   Bind no existing service and never reuse main credentials or private names.
2. Prove an ordinary private bridge control can reach the live owned resolver
   through Docker's embedded resolver using an explicit upstream setting.
3. Use the same live listener and upstream on a separate exact-policy isolated
   bridge. Require refusal/unreachability and no successful answer; retain
   server-side query counts alongside the positive control. Keep a working
   Docker peer-alias query as a separate internal-resolution control.
4. Record actual engine/network/container/resolver settings and bounded
   deadlines; distinguish explicit-upstream coverage from default host resolver
   behavior. Do not claim a non-loopback upstream test proves the separate
   host-loopback forwarding branch.
5. Join all listeners/processes and remove only owned test resources. Require
   original inventory and free canonical slot before the next cohort.

Do not create probe containers, compile helpers or run this test during the
current full-window cohort. A demonstrated new defect would require its own
deterministic regression, immutable correction and explicit protocol review;
do not swap an instrument into an active measurement.
