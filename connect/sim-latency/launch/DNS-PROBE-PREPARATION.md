# DNS proof preparation — not executed qualification

The [probe draft](dns-boundary-probe.go.txt) and
[deterministic tests](dns-boundary-probe-tests.go.txt) are **uncompiled and
unexecuted**. They do not change the evaluator, installed scripts, host resolver,
daemon configuration or active calibration. The original private uncompiled
draft remains preserved separately; its SHA-256 was
`fecaaa438dd06f0681bc6eb293182f69d96b7a037c74ce08f45a5e17f819b848`.

## Correct the measurement boundary before trusting a denial

Static review found that the original draft compared the entire DNS response
with one compressed wire encoding. A valid proxy can expand name compression,
change TTLs or preserve equivalent name case. Byte inequality could therefore
mislabel a received owned answer as denied. The revised draft uses
`golang.org/x/net/dns/dnsmessage`, already pinned at v0.60.0 in the frozen server
module, to authenticate the transaction, question and documentation-only A
answer semantically. No new dependency version is selected.

The original server also counted only successful reply writes. A query may
arrive even if the reply cannot be written. Both transport paths now observe
validated nonce reception **before** sending the answer. A denial requires both
no valid nonce answer at the client and zero nonce queries at the joined owned
server, with a successful same-server positive control. Neither an error message
nor a zero counter from an unready/dead server proves containment.

## Idle-slot qualification

After the current scoring attempt is terminal and preserved, use an isolated
checkout/workspace of the frozen source, actual Go 1.27.3/local, management CPUs
20,22 and the canonical exclusive scoring slot. Do not run these tests, builds,
listeners or Docker probes concurrently with measured evaluation.

Copy the two drafts into their own temporary `package main` directory inside
that isolated module, as `main.go` and `main_test.go`. Do not copy the probe's
`main` into the simulator package. Use the existing offline module cache;
do not update go.mod/go.sum or fetch another dependency graph.

Run all seven top-level tests with race instrumentation three consecutive times.
They use synthetic packets and an in-process `net.Pipe`, not host DNS. Preserve
each command, exit status, count, duration and full log digest. Demonstrate two
expected failures in separate retained runs:

1. Restoring byte-wise answer comparison must fail
   `TestNonceAnswerAcceptsUncompressedEquivalent`.
2. Restoring TCP observation only after a successful response write must fail
   `TestServeTcpRecordsQueryWhenResponseWriteFails`.

Restore and verify the exact revised source after each proof; rerun the normal
tests before building a CGO-disabled Linux probe. An opt-in skip, compilation,
static review or expected-failure run is not a passing runtime proof.

## Runtime scope and remaining default-resolver proof

Use a fresh synthetic nonce, explicit bounded lifetimes/deadlines, actual image
`sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3`,
owned temporary networks/containers and a joined host listener. Require ordinary
bridge positive forwarding, isolated bridge denial, and internal peer/Docker
alias positives for both UDP and TCP. Preserve actual network/container
inspection, ready/query/receive/join events, exact image and probe identities,
and restoration of the original resource inventory. Do not scan main ports or
alter host firewall, resolver, daemon or global network settings.

A custom non-loopback DNS upstream exercises Docker's container-namespace
upstream branch; it does **not** prove its default host-loopback branch.
Read-only inspection on October 26 found the host nameserver `127.0.0.53`.
The installed-engine source review in
[NETWORK-BOUNDARY-2026-10-25.md](NETWORK-BOUNDARY-2026-10-25.md) supports disabled
proxying for internal networks, but remains source evidence. Default resolver
runtime forwarding/denial needs its own controlled observation before that
coverage can be claimed. Do not label peer alias success, a custom-DNS denial,
or the corrected packet parser as completion of that separate proof.
