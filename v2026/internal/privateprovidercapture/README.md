# Private provider group capture

The API owns an initially unarmed diagnostic in the known Main API placement.
It uses the existing private heap diagnostic's native process identity and
kernel root-peer checks, with a separate Unix socket. There is no public route,
metric label, log payload, stats stream or upload containing the tuple.

The root-owned directory is `/run/urnetwork-private-provider` (0700). A process
uses `capture-<native-pid>-<native-start-ticks>.sock` (0600). The read owner must
derive this exact path from its independently qualified native process identity;
PID namespace translation must agree with that identity. Multiple API processes
can coexist in one directory. An occupied path, unsafe directory, unavailable
identity, unsupported platform or unqualified placement does not capture.
Startup reports only a fixed unavailable message. Crash-stale sockets are never
removed or replaced automatically; a fresh process generation has a different
path. A missing socket is diagnostic unavailability, not absence of the incident.

Commands are single-line JSON, maximum 2048 bytes, with fields `schema: 1`,
`expected` (the complete existing privateheapprofile.Identity), `operation`
(`arm`, `status`, or `read`), and `capture_id` (32 lowercase hexadecimal digits
chosen by the read owner). Unknown fields/commands and mismatched native
identities are refused. The sequential socket handler has a two-second I/O
deadline and a 16 KiB response cap. Private inputs and output belong only in a
root-owned 0600 evidence carrier, never shell arguments or public logs.

An exact arm is single-use for the API process. It permits at most eight
accepted completed requests, with at least one minute between acceptances,
within ten minutes of the arm. The API-owned recorder erases its retained
references fifteen minutes after the arm, on a successful consuming read, or on
close. There is no background export or retry. `status` contains counts/times
only. `read` returns the bounded tuple sample once and consumes before writing;
a failed transport may lose that sample and does not authorize another export.
The empty state is `no_sample_yet`, not a successful or healthy control. The
socket owner closes and joins at API lifecycle completion. The retention bound
governs owned references, not cryptographic erasure of historical Go heap bytes.

Only the natural POST `/network/find-providers2` receives the armed owner. The
selector must finish an ordinary (`default_minimum`), group-only, Quality or
Speed, any/v4/dualstack request with zero results and `cache_missing`. The tuple
contains the first group whose existing legacy metadata read actually returned
no usable facet or unfaceted count bytes, its already-resolved caller location,
request and load modes, finite source class, model start/completion times,
sample sequence and target counts. Real zero group/caller values are retained
as observed, since an invalid/obsolete group intent is itself a possible cause;
a country code is never used to invent a caller ID. Nothing reads a
JWT, account, client address, headers, body text or provider identifiers.
Admission uses TryLock and fixed memory; the model path performs no added
lookup, network call, disk write, wait or background launch. Its response,
fallback order, security filters and metrics remain unchanged.

The first observed missing group among multiple requested groups is only an
observed cache target, not a sole-cause claim. A sample does not establish HTTP
delivery, the watcher country cohort, publisher failure, membership, cache
freshness or global scarcity. Mixed/location requests, native-only failures,
nonempty results, errors, cancellation, intentional counts, disarmed/expired
owners and sample refusal remain unsampled/unknown. Match a captured tuple to
the separate bounded group-metadata read before attributing publication or
membership. An unrelated nonempty group does not establish recovery.
