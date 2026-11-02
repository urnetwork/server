# Extender capability cohorts (Phase B splice pilot)

Written 2026-11-02 with the server half of connect/EXTENDER-UPDATE2.md §3.2.
Nothing here has been applied. The vault resource is unchanged, no cohort is
configured, and no extender splices.

A Phase B extender splices an unauthenticated TCP hello to its borrowed site.
A client that cannot send the authenticated camouflage hello therefore
reaches the borrowed site instead of the extender, and fails the identity
check. A capability cohort is the operator's list of such extenders. Every
record signed for a listed extender requires the client capability
`tcp-camouflage-v1`, and the release route hands it only to a client that
declares the capability. A client that does not declare it is dealt the rest
of its partition. When nothing else is left, it gets outcome `incompatible`
and falls back to open records. Capabilities never change an identity's
count, partition or epoch disclosures.

## Configuration

`extender.yml` in the vault, under the existing `directory` block:

```yaml
directory:
  durable_network_ids:
    - <operator account network id>
  capability_cohorts:
    - capability: tcp-camouflage-v1
      extender_public_keys_hex:
        - <extender identity public key, hex>
```

- `extender_public_keys_hex` names an extender by its identity key, which is
  stable across re-activations. Hex is matched case-insensitively. An entry
  that is not a key never matches.
- A listed extender requires the capability only if two things hold:
  - A release alone reaches it: the gated tier, ordinary or a gated canary. An
    open or dns-canary extender in the list is ignored, because open channels
    cannot ask a client what it can do.
  - Its record carries a camouflage key (`RealityPublicKey`). Until the server
    signs the key (connect/EXTENDER.md phase 14b), every cohort is inert.
- A misspelled or unknown capability is kept as a requirement that no client
  satisfies. The api logs it once at startup, and the listed extenders are
  withheld from every release until the entry is fixed.
- The resource is read once per process. A change takes effect when every
  process that signs records has restarted: the api and alt (both serve
  activation and release; alt runs the api routes in process) and the
  taskworker (drip and dns TXT). Until then, processes that have and have not
  read the change sign the same host's records differently. Restart in waves,
  as for any api restart.

To find a host's key, use the read-only query
`SELECT encode(public_key, 'hex') FROM network_extender WHERE client_id = '<the host's client id>'`
on a replica, with a `SET LOCAL statement_timeout`. Keep the list internal. It
names operator hosts.

## Gates before the first cohort

1. Phase 14b is deployed: cohort records carry `RealityPublicKey`. Check that
   `urnetwork_extender_record_capability_requirements_total` shows `required`
   for the cohort and not `no_camouflage_key`.
2. Supported clients declare `tcp-camouflage-v1` only when they can complete
   a camouflaged dial: the record carries the key, and the fingerprint switch
   is not `go`. This is the SDK's capability decision (slice F).
3. The extender-side Phase B gate is deployed (slice F). It splices only after
   the extender's own activation record has carried both the key and the
   requirement for at least one record lifetime. Turning the extender flag off
   stops splicing for new connections at once.
4. Baselines are recorded on the extenders dashboard ("gated release" row).
   Record, for the cohort's countries and families:
   - the release outcomes;
   - incompatible answers among clients without the capability;
   - connection success.
   Also record the numeric acceptance and rollback thresholds and the
   observation period (EXTENDER-UPDATE2 "Delivery and rollout gates").

## Rollout (Phase A to Phase B for one cohort)

1. Add the cohort's keys to `capability_cohorts` and restart the api, alt and
   the taskworker.
2. Verify:
   - The extenders dashboard panel "capability requirements signed" shows
     `required` for the capability, and nothing else for these extenders.
   - `not_release_only` is a listed extender on an open channel: remove it from
     the list.
   - `unknown` is a misspelled capability: fix it now, because those extenders
     are withheld from every release.
3. Watch "releases not ok, by family and capability" for
   `incompatible capable=false`. These are legacy clients whose partitions hold
   only cohort extenders. Keep the rate under the recorded threshold, or shrink
   the cohort.
4. Settle for at least one record lifetime, 24 hours from the first
   `required`, before the extender splices. Legacy clients' cached records of
   the host were signed before the requirement, and they expire in that time.
   The extender-side gate enforces the same wait from its own activation
   record. Do not shorten it by hand.
5. Only then enable splicing on the cohort (slice F's control).

A legacy identity that was handed a cohort extender earlier in the epoch keeps
that disclosure in its budget. No release refunds it. Until the epoch ends,
that identity may receive fewer gated records, or `epoch_spent`, and fall back
to open records. Epochs are seven-day multiples of Unix time and begin on
Thursdays at 00:00 UTC. Adding a cohort shortly before an epoch boundary
shortens this window.

## Rollback (Phase B to Phase A)

1. Turn the extender-side splice flag off first. New connections are
   terminated again, as in Phase A, and working sessions are kept where
   possible.
2. Then remove the keys from `capability_cohorts` and restart the api, alt and
   the taskworker. In the opposite order, legacy clients would be handed the
   host's record while it still splices them away. That lasts until the host
   re-activates.
3. Verify that `required` stops for the capability and that
   `incompatible capable=false` returns to its baseline.

## Expanding

Expand one cohort at a time, under the same gates. A cohort expands only
after the pilot has met its thresholds over the recorded observation period
with bounded borrowed-site load, and after a rollback has been exercised.
