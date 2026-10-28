# FindProviders2: modern providers

Design, 2026-10-28, revised the same day for the peer review in
[FP2-MODERN-REVIEW.md](FP2-MODERN-REVIEW.md) (see
[Review corrections](#review-corrections)). Every question is decided: the
owner's decisions of 2026-10-28 are recorded below, and the owner accepted the
remaining defaults the same day.

**Time-critical, independent of this work.** Until this design ships, the
modern provider gate turns on by itself at 2026-11-11 00:00 UTC. This design
retires the gate, so `main/provider.yml` must set
`quality_speed_requires_modern_provider: false` before that instant. That needs
a config release and a restart of every taskworker, api and alt block. See
[Rollout](#rollout), step 0.

## Decisions (owner, 2026-10-28)

1. **End-to-end discovery requires modern providers only.** While its profile
   enables end-to-end encryption, the multi client sends
   `require_modern_provider` and stops sending `post_quantum_encryption`.
2. **Priority is strict and covers the whole band.** Within each band, every
   modern candidate comes ahead of the family-facet preference and the evidence
   rank, wherever it sits in the target's pages. Non-modern providers only fill
   what the band's modern candidates leave. The export orders each target's
   pages modern first, and the readers traverse them that way
   ([Priority within each band](#priority-within-each-band)).
3. **Priority ships on and replaces the modern provider gate.**
   `rank_modern_first` is on by default, and `false` is its kill switch.
   Non-modern providers keep their Quality and Speed membership and are drawn
   after modern ones.
4. **The capability probe covers every Online provider**, judged with the same
   Online inputs as the export, not just Quality and Speed members.
5. **The gate is retired in config and in code.** provider.yml sets the switch
   to `false`, and an absent switch now means off. Only an explicit `true` turns
   the gate on.
6. **Non-modern native providers report `not_modern`** in provider status while
   priority is on, with the existing text.
7. **Network-only providers are judged by their declaration.** The probe can't
   reach them, so for a provider holding a Network key and no Public key,
   `require_modern_provider` accepts the `post_quantum_encryption` declaration
   instead of probe evidence.
8. **The response is unchanged.** `FindProvidersProvider` gets no `modern`
   field.
9. **Location lists are out of scope.** Provider-locations stays
   capability-blind. Counting modern supply there is a follow-up.
10. **The field is named `require_modern_provider`.**
11. **`post_quantum_encryption` keeps its meaning on the server**, for any
    client that sends it.
12. **Probe capacity starts at `concurrency: 32` and `limit: 128`.** These are
    a starting experiment, judged against measured throughput.

## Summary

- **Index.** Every score the export publishes carries `Modern`. Each target's
  pages are partitioned modern first, with metadata bound to the pages it
  describes.
- **Request filter.** `require_modern_provider` ignores non-modern providers
  in every draw: own bucket, borrowed bucket, Online fallback, `rank_mode:
  online`, `force_minimum` and providers named by `client_id`. A modern-only
  request reads only modern pages.
- **Priority.** Each band reads its modern pages, across all the requested
  families, before any non-modern page. It draws modern providers first. A
  non-modern provider only fills what the band's modern candidates leave.
  Tiers, band order and band offsets don't change. This replaces the gate.
- **Provider status.** A non-modern Quality or Speed provider reports
  `not_modern` while priority is on.
- **Capability probe.** It measures every Online provider, with the export's
  Online inputs.
- **Connect.** Every find-providers2 request the generator makes while the
  profile enables end-to-end encryption sets `require_modern_provider`.

There is no migration and no protocol message change. The request gains one
optional JSON field. The index gains one small key per target payload.

## Background

### Modern providers today

The capability probe ([capprobe/README.md](capprobe/README.md)) measures each
provider on a real client-to-provider session and records the result in
`provider_modern_evidence` (migration 836).
`classifyProviderModernEvidence` turns a row into one of five outcomes:
`modern`, `not_post_quantum`, `no_resizable_windows`, `no_evidence`, or
`stale_evidence` (seven days or older).

Today the probe measures only providers the egress rules admit to Quality or
Speed **before** the gate. It refreshes evidence every 72 hours by default.

Today the gate takes non-modern providers out of Quality and Speed from
2026-11-11 00:00 UTC, unless provider.yml sets
`quality_speed_requires_modern_provider`. Modern providers are scarce, per the
2026-10-21 postmortem
([../factury/postmortem/2026-10-21-provider-gate-churn-storage.md](../factury/postmortem/2026-10-21-provider-gate-churn-storage.md)):

| When | Quality: modern / admitted | Speed: modern / admitted |
| --- | --- | --- |
| 10-19 07:47 saved finding | 1 / 456 | 2,397 / 27,725 (8.6%) |
| 10-21 flip, score export | 1 / 146 | 203 / 2,165 (9.4%) |

### The declared capability (2026-10-26)

A provider sets `Provide.post_quantum_encryption` when its encryption session
manager serves post-quantum sessions. `SetProvideWithCapabilities` keeps the
latest declaration in `provide_capability` (migration 894), but only from a
provide that carries a Public or a Network key. A provide with neither key, such
as a window client's return-traffic provide, leaves the row as it was. So a
declaration can outlive the keys it was made with.

`FindProviders2Args.post_quantum_encryption` keeps only declaring providers,
reading declarations from Postgres per candidate.

### The index FindProviders2 reads

The taskworker export (`UpdateClientScores`, a run-once task with a 120-minute
budget publishing with a five-hour TTL) does three things:

- decides each provider once (`decideProviderEgress`);
- ranks each of its scores (`rankClientScore`);
- publishes gob pages of `*ClientScore`, each at most 200 scores, per
  force_minimum, rank mode, caller country and target.

The page sets are:

- the union pool: one counts document (a gob `[]int` of page sizes) and its
  pages per family facet, plus the legacy un-faceted set;
- the `native_v1` pages, native members only, behind a checksummed manifest;
- the forced pool.

A score with no Public provide key is `NetworkOnly`, and is served only to
callers in its own network.

FindProviders2 runs in the api, in alt (which mounts the api router) and in the
proxy for hosted devices. Both readers build a cursor that appends whole facet
groups in preference order, shuffling pages only within a group
(`newClientScoreCursor`, `loadNativeClientScoresWithCursor`). `refillPool`
stops once the band's quota is met.

### Bands

| Band | Drawn from | Tier the client sees |
| --- | --- | --- |
| Named | providers named by `client_id`, placed ahead of discovery | 0 |
| Native | the requested mode's members | the mode's tier, 0–3 |
| Borrowed | the other mode's members (Quality borrows only Speed members within its own cutoffs) | their tier + `backfill_tier_offset` (11) |
| Online fallback | the Online bucket, once both native sources run out | 2 × offset (22) |

A `rank_mode: online` answer is one band, and so is a `force_minimum` answer.
Within a band today, the draw takes family facets in preference order, then
evidence ranks (with `rank_measured_first`), then a weighted draw listed by
tier.

The client keeps an answer's providers in a map keyed by destination and ranks
them by `Tier` plus its own demerits. No client sees the order of an answer. So
priority has to decide **which** providers a band draws.

## Design

### Definition

A provider is **modern** at an instant `now` when one of these holds.

- **It holds a Public provide key.** Then `classifyProviderModernEvidence(row,
  now)` must be `modern`: evidence measured less than seven days before `now`
  of a negotiated post-quantum end-to-end session (X25519MLKEM768), and of acks
  advertising `receive_window_byte_count`. A Public provider that also holds a
  Network key still needs this evidence.
- **It holds a Network key and no Public key** (network-only). The probe can
  never contract with such a provider. Instead, its latest provide with a
  Network key must have declared post-quantum encryption (Decision 7).
- **It holds neither key.** Then it is never modern, whatever declaration a
  former provide left behind.

**Limitation: modern doesn't mean sealable.** The probe records a negotiated
post-quantum session as positive even when it never sealed
(`capprobe/reading.go`, "unsealed"). A modern result is therefore evidence that
the provider negotiates the session. It is not proof that a required-encryption
client will seal with it.

The client's `EncryptionModeRequired` still decides every send. A modern
provider that fails session validation costs the client a failed dial, never
plaintext, and `capprobe`'s tests keep an unsealed positive from becoming
plaintext delivery. Nothing here claims that every modern answer carries sealed
transport.

### The index flag

```go
	// Modern is whether the provider qualified as modern at the export's
	// instant (FP2-MODERN.md): with a Public key, measured modern by the
	// capability probe; with only a Network key, declared post-quantum
	// encryption in its latest provide. Set whatever buckets it is in. Zero,
	// as every entry written before the field decodes, is not modern. Set
	// only on the top-level score, like the location ids.
	Modern bool
```

**How the export sets it.**

- `providerEgressDecision` gains `modern`. `decideProviderEgress` sets it for
  every provider it does not hard-exclude, from `facts.modernOutcome`.
  `rankClientScore` copies it onto each score.
- A `NetworkOnly` score instead takes the provider's declaration. The source
  query only yields providers holding a Public or Network key, so a
  `NetworkOnly` score holds a Network key. The export reads the declarations
  once per run for the cohort's network-only providers, in batches of 256, on
  a connection of its own (no nested acquisition).

**Compatibility.** gob doesn't send a false bool, and an older reader drops a
field it doesn't know. A page written before the field decodes `Modern ==
false`.

**Freshness.** Decided at the export's instant. The cache's five-hour TTL bounds
staleness against a seven-day evidence window, so no expiry instant is stored.

### The modern-first page partition

The export writes each page set modern first, so a reader can take modern
candidates before any non-modern one without reading the whole pool.

**Pages.** Each facet's providers, union and native alike, are split into
modern and non-modern. Each part is shuffled and cut into balanced pages of at
most 200, then the modern pages are written first. A page holds one class only.
The counts document keeps its format, a gob `[]int` of page sizes in page
order, so an older reader, which shuffles a facet's pages, is unaffected. The
un-faceted union set isn't partitioned: only exporters that predate facets
produce one a reader uses.

**Native metadata.** `clientScoreNativeManifest` gains `ModernPages
map[ipFamilyFacet]int`, the count of leading modern pages per facet. The
manifest is already bound to its generation, and every page to its checksum.
An older manifest without the field decodes with an empty map.

**Union metadata.** A new key per payload, beside the counts documents:

```
{cs_<fm>_<rm>_<caller>_<target>}m_l   (m_g for a location group)
```

It holds a gob `clientScoreModernPartition`: for each family facet, the
SHA-256 of that facet's counts document and the SHA-256 of each of its leading
modern pages. It is written for the same callers as the payload and with the
same TTL, after the pages it describes. A reader picks the caller or the
baseline copy by the same alias decision as the counts (`selectClientScorePayload`).

**Each target's partition has one of three states, per source:**

| State | When | Its pages |
| --- | --- | --- |
| `partitioned` | native: the manifest has `ModernPages`; union: the metadata's counts checksum equals the counts bytes read | leading modern pages in the modern phase, the rest in the non-modern phase |
| `absent` | an older export wrote the target | all in the non-modern phase: their scores decode as not modern anyway |
| `stale` | union only: the counts checksum differs, or a modern-phase page's bytes fail their listed checksum | all its unread pages move into the modern phase, so no modern page is skipped |

A stale state only arises when a read straddles two exports' writes. It costs
reads, never priority.

**Readiness marker.** Each complete export sets `client_score_modern_v1_ready`
to the instant its source load started. It is written after the target fan-out
joins, as the existing schema markers are. The client rollout gate reads it
([Rollout](#rollout), step 5).

### The request filter

The new field, in `model.FindProviders2Args` and `connect.FindProviders2Args`
(connect with `omitempty`), and in the spec, which `TestSpecConformance`
checks:

```go
	// RequireModernProvider keeps only modern providers in every draw of the
	// request, named providers included (FP2-MODERN.md). Unset keeps the
	// capability-blind answer.
	RequireModernProvider bool `json:"require_modern_provider"`
```

| Draw | With `require_modern_provider` |
| --- | --- |
| Native, borrowed, Online fallback | only modern-phase pages are read, and non-modern candidates are removed before any read or count |
| `rank_mode: online` | the Online bucket's modern members, drawn uniformly |
| `force_minimum` | the forced pool's modern members |
| Named by `client_id`, with or without discovery | answered only when modern, by the [definition](#definition) |
| With `post_quantum_encryption` too | both filters apply |

**Pages.** The cursor holds only the modern phase:

- partitioned targets contribute their modern pages;
- stale ones contribute every page;
- absent ones contribute none, since their scores decode as not modern.

**A request-only refusal.** `clientScoreRequestFilter` gains `requireModern`,
and `rejection` returns a new cell 4 for `!score.Modern`.

- `unchecked` feeds every database read the request makes for its candidates.
  So no declaration, hard-exclusion, liveness or validation read is spent on a
  refused provider.
- `apply` removes refused providers before quota counting. The refill counts
  them as discarded rows.

**Named providers.** They are not in the index. One read-only query reads the
named ids in one snapshot. For each id it returns:

- whether the id holds a Public key;
- whether it holds a Network key;
- its evidence row, classified at the request's `filterTime`;
- its declaration.

The [definition](#definition) then decides each id: with Public, the evidence;
with Network only, the declaration; with neither, refused. The helper sits
beside `readProvidePostQuantumEncryption` and returns errors rather than
raising, and a failed read fails the request.

- In the discovery path, the read runs before the other named reads, and
  refused ids join `namedExcludedClientIds`.
- In the named-only path, refused ids join `hardExcludedClientIds`.

### Priority within each band

The guarantee: within a band, a non-modern provider is drawn only when the
band's modern candidates, after every request filter and live check, number
fewer than the band's need. All of them are then drawn first.

It holds across family facets and beyond the first load, wherever a target's
pages put the modern providers. The one exception is a finite union allowance
running out, covered under that point below.

**Traversal.** With `rank_modern_first` on, both readers order the cursor in
two phases:

1. The modern phase: the modern pages of every requested family, facet groups
   in preference order, each shuffled.
2. The non-modern phase: the non-modern pages, facet groups in the same order.

The first load already reads from the modern phase. With priority off, a
facet's two classes merge into one shuffled group, which is today's traversal.

**Quota.** `refillPool` counts by phase. While the cursor is in the modern
phase, the quota counts only modern survivors:

- natives after filters;
- for Quality, after live validation (`qualityReadClientIds`).

The refill continues until modern survivors meet the band's need, or the modern
phase is exhausted. Only then does it read non-modern pages, until all
survivors meet the need or the source is exhausted. A non-modern row read in
the modern phase, from a stale target, never ends that phase.

**Validation.** `validateQualityPool` runs in two passes:

- modern pending candidates first, with the need counted against validated
  modern members;
- then every pending candidate, counted against all validated members.

Otherwise non-modern validations could fill the need and leave modern
candidates unvalidated, and an unvalidated Quality native can never be drawn.

**The draws.** `selectModernFirst(candidates, n, draw)` draws `n` from the
modern candidates with the band's own draw, then the remainder from the rest.
It wraps:

- the native draw and the validation draw;
- the borrowed draw;
- the Online fallback (`selectOnlineClientScores`);
- `rank_mode: online` (`selectOnlineUniformly`, uniform within each class);
- `force_minimum`.

Named providers keep their place.

**A finite allowance, the one exception.** Union reads keep their finite row
allowance (`findProviders2RefillLoadCount`), and each phase has one of its own.

- The modern phase may spend its whole allowance.
- If the allowance ends inside the modern phase with too few modern survivors,
  the cursor skips the rest of the modern phase, and the non-modern phase
  fills the band from its own allowance. Availability doesn't depend on
  priority.
- The answer is recorded in
  `urnetwork_findproviders2_modern_priority_incomplete_total`.
- With priority off there is no modern phase, so the single allowance is
  today's.

Strict priority therefore holds whenever the band's modern survivors lie
within one allowance of modern rows: about 2,420 rows for count 20. Native
sources read to exhaustion and have no such exception.

**What the client sees.** Tiers, band offsets and band order are unchanged. The
answer lists providers in draw order, which is no contract.

**Load shift.** Speed answers move onto the roughly 9% of Speed natives that
are modern, wherever a target holds enough. Those providers carry about ten
times their share until more providers update. Non-modern providers keep their
buckets and fill every shortfall.

**The switch.** `rank_modern_first` lives in provider.yml's `egress_index`
block, next to `rank_measured_first`.

- It defaults to true, and `false` is the kill switch.
- It is read per request through `requestEgressIndexSettings`, in the api, alt
  and the proxy.
- Each process publishes its value as
  `urnetwork_findproviders2_rank_modern_first_enabled`.
- The export always partitions, whatever the switch.
- Changing it is a config release and a restart of those readers.

### The gate is retired

Priority replaces the modern provider gate (Decisions 3 and 5).

**Code.**

- `RequiresModernProvider(now)` returns the switch when it is set and false
  when it is absent. The signature stays as it is.
- `QualitySpeedModernProviderStart`, `DefaultQualitySpeedModernProviderStart`
  and the `quality_speed_modern_provider_start` key retire. A provider.yml that
  still carries the key decodes it as ignored, and logs that once.
- The gate stays behind an explicit `true`. The `not_modern` exclusion, the
  shadow counts and the counts policy key are unchanged.
- `urnetwork_stats_provider_modern_gate_start_seconds` is no longer published.
- The export's log line and `bringyourctl provider inspect` report the switch
  and priority, and no start.

**Config.** `main/provider.yml` sets the switch false and drops the start
(config 808328a).

**Monitor.** SIGNALS.md §2.29 loses its start windows (`modern-gate-not-ready`,
`modern-gate-flip-drop`). It keeps the fleet-agreement check and the decode
WARN, for an explicit switch.

**Docs.** FP2FIX.md records the decision at its top. Its matrix row becomes
"not required; modern providers are drawn first within each band".

### Provider status

While `rank_modern_first` is on and the gate is off, a provider gets
`not_modern` when all of these hold:

- it holds a Public key;
- it is a Quality or Speed member;
- it is not modern.

It keeps its buckets and numbers.

- **Position.** The reason sits just after `not_eligible` for ARIN non-quality,
  and before `speed_test_missing` and `slow`.
- **Why after ARIN non-quality.** A provider ARIN classifies non-quality reads
  `not_eligible` whatever its modern status, like an ARIN-risk or TLS one. If
  `not_modern` came first, a non-modern provider could tell those anti-abuse
  signals apart (`TestProviderStatusAntiAbuseSignalsAreIndistinguishable`).
- **With the gate on** (an explicit `true`), the gate's own `not_modern` keeps
  its place before ARIN non-quality, whatever priority is set to.
- **With the gate off and priority off**, no provider reports `not_modern`.

Network-only providers keep `network_only`. Online-only providers keep the
reasons that already explain their place.

### The capability probe covers every Online provider

**Eligibility.** `FilterProviderCapabilityProbeEligible` evaluates
`decision.online` with the export's Online inputs, as the provider-status path
does:

- each provider's published country, from its location rollup, passed to
  `egressFacts`;
- the URL-success guard the export last published
  (`getProviderOnlineSuccessBaseline`), set on the filter.

So `no_success` and `low_success` providers, which the export places in no
bucket, stay out. So do hard exclusions.

- Unprobed providers become eligible, and so do providers with a failing ratio
  whose countries keep them Online.
- Without a published guard (absent or expired), every country is exempt, as
  provider status reads it. The probe then measures at most those otherwise
  excluded providers, and the selection metric counts the passes that ran
  without a guard.
- The page's Public-key requirement stays, so network-only providers are never
  measured.

**Population.** Online held 49,417 providers on 10-13, about 1.8× the 27,725
Speed providers of 10-19.

**Capacity is an estimate.** At one measurement per provider per 72-hour
refresh, the probe needs about 690 measurements an hour, against about 580 an
hour today at concurrency 16. That figure is a floor, not a forecast:

- inconclusive and missing evidence stays due, so unsuccessful measurements
  recur every sweep;
- the settings validate refresh plus minimum sweep interval against the
  seven-day window, not the actual sweep duration.

`concurrency: 32` and `limit: 128` are the starting experiment. Acceptance
reads:

- attempted against recorded measurements;
- the measured sweep duration;
- the tail of evidence age;
- the balance burn of prober shard 255, unsuccessful attempts included.

**Identity churn.** One derived client per measurement attempt is a fraction
of a second's churn per second, against 250–760 a second on 10-21.

### Connect multi client

- `FindProviders2Args.RequireModernProvider` goes in `api.go`, and
  `require_modern_provider` goes in the spec.
- While its post-quantum flag is set, `ApiMultiClientGenerator` sends
  `RequireModernProvider: true` from `nextDestinationsContext` and
  `NextDestinationsForClientId`. It no longer sends `PostQuantumEncryption`
  (Decision 1).
- **The guarantee covers the requests the generator makes, and no more.**
  Destinations from fixed `spec.ClientId` specs, and restored window
  identities, bypass discovery and are not filtered. The window clients'
  `EncryptionModeRequired` still decides them.
- The sticky Fixed IP redial and the recent-exit lookups also go through
  `NextDestinationsForClientId`. Under the profile, a recent exit that is not
  modern comes back as not found, and the window discovers again.
- The capability probe's pinned tunnels never set the flag. They must reach
  providers that have no evidence yet.
- An older server ignores the field. Discovery is then capability-blind, and
  the client fails closed per provider.
- **Hosted proxy devices can enable end-to-end encryption.**
  - The proxy applies each device's profile through the sdk
    (`toConnectPerformanceProfile` passes `PostQuantumEncryption` through), and
    the spec documents `post_quantum_encryption` for hosted profiles.
  - So a proxy built with the new connect sends `require_modern_provider` from
    its in-process discovery.
  - That is why readers deploy only after the index is rewritten
    ([Rollout](#rollout), step 2).
- The sdk, the apps and the JS remote device need only the connect bump.

### Observability

- **Outcomes.** `urnetwork_findproviders2_outcomes_total` gains `capability`:
  `none`, `modern`, `post_quantum` or `modern_post_quantum`.
  `monitor/signal_provider_count.go` keeps its zero-share cohorts to
  `capability!~"modern.*|post_quantum"`, which still counts series without the
  label.
- **Selection.** Selection outcomes gain the reason `filtered_capability`, for
  modern and declaration refusals; the latter count as `filtered_hard` today.
  A modern-only request whose targets hold no modern page reads nothing. It
  reports `filtered_capability`, or `returned_small_filtered_capability`, from
  the rows it left unread, not `cache_empty`.
  The selection schema goes from 2 to 3, and `monitor/signal_provider_selection.go`
  learns the reason.
- **New counters.**
  - `urnetwork_findproviders2_answered_modern_total{rank_mode, band, modern}`:
    providers answered per band (`native`, `borrowed`, `online`). Named
    providers are not drawn and are not counted.
  - `urnetwork_findproviders2_modern_partition_total{source, state}`: targets
    read per source (`native`, `union`) and state (`partitioned`, `absent`,
    `stale`).
  - `urnetwork_findproviders2_modern_priority_incomplete_total{rank_mode}`:
    answers whose allowance ended inside a modern phase.
- **Shadow counts** gain an `online` bucket, in the export's
  `[nclm]modern online providers:` log line and in
  `urnetwork_stats_provider_modern_gate_shadow{bucket="online"}`.
  - The dashboard snapshot carries it as `ModernOnlineCounts`.
  - The snapshot decoder refuses unknown fields, so the counts policy key goes
    to schema 6, and each version reads its own snapshots.
- **Probe paging.** The probe's stall page (SIGNALS.md §2.19g) fires past six
  hours when any of these holds:
  - priority is on in any reader;
  - modern requests were answered in the last day;
  - the gate is on.

  A stalled probe ages every provider out of priority and modern requests
  within seven days.

### Rollout

0. **Before 2026-11-11 00:00 UTC, whatever this work's schedule.**
   - Done on 2026-10-28:
     - config 808328a was pushed;
     - config-updater `2026.10.28-outerwerld-1084072720` (`restart: false`)
       was deployed at 18:28Z, and edge-0, edge-1, edge-3 and edge-4 had
       published it by 18:41Z;
     - no block restarted for it.
   - Pending, for the owner: recreate every taskworker and api block, and alt
     on crisp and fireside, by a restart or a deploy. Alt finished a deploy at
     18:27:56Z, seconds before the config landed, so it needs one too.
   - Then check that every block reports config `1084072720` or later, and that
     `urnetwork_stats_provider_modern_gate_on` reads 0 everywhere.
1. **connect.** Merge the args field, the spec entry and the generator change.
   Hold the release until step 5.
2. **server.** Bump connect, then ship the index flag, the partition, the
   filter, priority (on), status, the probe, the gate default, the metrics and
   the monitor changes. There is no migration.
   - **Taskworkers first** (export, probe, gate).
     - Wait until every old taskworker is gone and `client_score_modern_v1_ready`
       holds an export start later than the last old one's exit.
     - Then deploy the **readers** (api, alt, proxy).
   - The proxy needs this order: it is a reader, and it carries the new connect
     generator for hosted devices. If it came first, it would read old pages as
     `absent` and answer its end-to-end devices empty.
   - An old reader ignores the flag and the metadata, so the taskworkers can go
     first safely.
   - Roll the readers out in stages, watching
     `urnetwork_findproviders2_answered_modern_total` and load on modern
     providers. The kill switch is `rank_modern_first: false`, with a config
     release and a reader restart.
3. **capability_probe.yml.** Set `concurrency: 32` and `limit: 128`, with a
   taskworker restart. Accept on the probe metrics above.
4. **Docs.** FP2FIX.md, capprobe/README.md and SIGNALS.md §2.19g and §2.29
   change with the merge.
5. **Clients.** Release connect, the sdk and the apps only once all of these
   hold:
   1. every reader runs the new server;
   2. no taskworker on the old code remains, so no old export can still be
      writing;
   3. `client_score_modern_v1_ready` holds an export start later than the last
      old taskworker's exit, so one complete new export ran after the last old
      writer;
   4. `modern_partition_total{state="absent"}` has stopped growing;
   5. modern-only requests answer with modern providers for one native-heavy
      target, one target answered from the union and Online fallback, and one
      `force_minimum` request.

   Record that marker value and those answers in the release notes. Elapsed
   time and reader versions alone are not the gate.

| Client | Server | Result |
| --- | --- | --- |
| new (modern) | new, after step 5's checks | modern providers only |
| new (modern) | old | field ignored: capability-blind, the client fails closed per provider |
| sends `post_quantum_encryption` | new | declaring providers only, as today |
| neither | new | unchanged, plus priority |

### Tests

Each change has a test at the layer where its behavior shows.

**server/model, the index and partition**

- `Modern` is set in every page set, and is false for every non-modern outcome,
  Online-only providers included.
- A network-only provider is modern exactly when it declares.
- Pages are single-class, modern first, with the counts format unchanged.
- The manifest's `ModernPages` and the union metadata's checksums describe the
  pages written.
- An old reader decodes the new pages and manifest.
- A new reader decodes an old page as not modern and an old target as `absent`.
- Mismatched counts or a page failing its checksum read as `stale`.

**server/model, priority**, through `FindProviders2` with both native and
legacy readers:

- The cross-facet target: 1,000 eligible non-modern dualstack natives and 100
  modern v4-only natives, count 20. The answer holds 20 modern providers.
- Modern providers placed beyond the first load.
- Modern candidates refused by live checks (offline, ARIN, a failed
  validation), which the refill reads past.
- Genuine modern exhaustion, then non-modern fill.
- An allowance ending inside the modern phase, recorded as incomplete and still
  filled.
- Two-pass validation.
- Uniform draws within each class for `rank_mode: online`.
- With `rank_modern_first: false`, every existing selection test passes
  unchanged.

**server/model, the request filter**

- Non-modern providers are absent from every band, from `rank_mode: online`,
  from `force_minimum` and from named answers.
- No read is spent on a refused candidate.
- A zero answer reports `filtered_capability`.
- A named read error fails the request.
- With `post_quantum_encryption` too, the answer is the intersection.
- Named classification, in both named-only and named-plus-discovery requests:
  - Announce Public with post-quantum, then a return-only provide: the
    declaration remains, and the id is refused.
  - Valid network-only controls are answered.
  - Public plus Network requires evidence.

**server/model, the gate**

- An absent switch is off at every instant, 2026-11-11 included.
- An explicit `true` is on, and the start key is ignored.

**server/model, probe eligibility.** Compared against exported Online membership
through the production loader, with a published guard:

- not eligible: a non-exempt provider with no URL success, one below the Online
  ratio, and a hard exclusion;
- eligible: an exempt-country provider, an unprobed one, and an ordinary
  failing-ratio provider that stays Online.

Also covered: a pass without a guard.

**server/model, provider status.** Every combination of priority and gate:

- `not_modern` appears with the gate on;
- with the gate off and priority on, it appears only for non-modern natives;
- with both off, it never appears.

**Rollout**

- New readers with old pages, and with mixed publication.
- The marker is written only after a complete export.

**server/api.** `TestSpecConformance` with the new field.

**connect.**

- With the profile's encryption on, window discovery, family discovery and the
  named redial carry `require_modern_provider` and not
  `post_quantum_encryption`.
- With it off, neither is sent.
- Fixed and restored destinations are unchanged.

**monitor.**

- The provider-count cohorts exclude capability requests.
- The selection signal knows `filtered_capability`.
- §2.29 works without a start.
- The probe page follows priority, modern requests and the gate.

## Review corrections

The peer review (FP2-MODERN-REVIEW.md) found four issues and asked for four
clarifications. Each is addressed here.

1. **A draw wrapper can't enforce priority ahead of facets.** Adopted the
   modern-first page partition, the phased traversal and quota, two-pass
   validation, and an explicit allowance policy. Dropped the 2% coverage claim.
   See [the partition](#the-modern-first-page-partition) and
   [priority](#priority-within-each-band).
2. **The widened probe lacked the Online inputs.** Eligibility now loads the
   published country and the published URL-success guard, and defines the
   no-guard case. See [the probe](#the-capability-probe-covers-every-online-provider).
3. **No Public key doesn't prove a Network provider.** The named query reads
   Network-key presence, and a client with neither key is refused. See
   [named providers](#the-request-filter).
4. **Client rollout needs a completed export.** Step 5 gates on drained old
   exporters, the readiness marker after the last old writer, and verified
   modern-only answers. See [Rollout](#rollout).
5. **Clarifications.**
   - Probe capacity is an estimate with acceptance metrics.
   - Modern doesn't imply sealable.
   - The generator's guarantee covers only the requests it makes.
   - The status tests cover every priority and gate combination.

## Alternatives considered

- **Keep the gate beside priority.** The owner chose to replace it.
- **Prioritize the loaded sample only.** This would be simpler, but a
  facet-ordered first load can hold no modern provider while a whole family of
  them goes unread. That breaks Decision 2. Rejected in review.
- **Dedicated modern pools.** These duplicate the modern subset. Memory would
  grow toward twice the index as the fleet updates. The partition reorders
  pages instead.
- **Read the evidence per candidate per request.** That puts a Postgres read on
  the hot path. The flag in the index is free at request time.
- **A tier demerit for non-modern providers.** This would cross bands.
- **A weight boost instead of strict priority.** Declined (Decision 2).

## Risks

- **Load concentration** on modern providers until the fleet updates. The kill
  switch reverts it per reader; roll the readers out in stages.
- **Supply for end-to-end users.** It is the modern natives plus the modern
  Online-only providers the widened probe finds. A country without one answers
  empty, and the location list doesn't show that (Decision 9).
- **Probe load** is about 1.8× today's at least. It is accepted on measured
  throughput, not on the estimate.
- **A stalled probe** ages evidence out over seven days. Priority then draws as
  today, and modern-only requests answer empty. The page covers this.
- **The declaration is self-reported**, and it only applies to network-only
  providers. The client verifies every session.
- **Partition reads.** A stale partition costs reads, never priority. The
  counters show the states.
- **Step 0 has a deadline.** A taskworker still on the old code reads an absent
  switch as the 2026-11-11 start.

## Files

**server:**

- `model/network_client_location_model.go`: `ClientScore.Modern`, the
  request field, the export's declaration read and marker, the named read, the
  phased quotas, and the band draws in `FindProviders2`.
- `model/network_client_score_targets.go`: the modern-first split and the
  union metadata.
- `model/network_client_score_native.go`: the manifest's `ModernPages` and the
  native traversal.
- `model/network_client_score_read.go` and
  `model/network_client_score_cursor.go`: the metadata read, the states, the
  phased cursor and the stale detection.
- `model/egress_index.go`: `providerEgressDecision.modern`, `rank_modern_first`
  and its gauge, and the gate default.
- `model/network_client_score_rank.go`: `rankClientScore`.
- `model/egress_modern_provider.go`: the named classification, and the Online
  shadow bucket.
- `model/provider_capability_probe.go`: eligibility with the Online inputs.
- `model/provider_status_model.go`: `not_modern` under priority.
- `model/network_client_selection_prefilter.go`,
  `model/network_client_selection_evidence.go`,
  `model/network_client_selection_metrics.go` and
  `model/network_client_location_model_stats.go`: the refusal,
  `selectModernFirst` and the metrics.
- `controller/stats_collector.go`, `bringyourctl`, and the `monitor` signals.
- `FP2FIX.md`, `capprobe/README.md` and `monitor/SIGNALS.md`.

**connect:** `api.go`, `ip_remote_multi_client_api.go`,
`ip_remote_multi_client.go` (the comments), `api/bringyour.yml`, and the
discovery test.

**config:** `main/provider.yml` (step 0, pushed) and
`main/capability_probe.yml` (step 3).

**sdk, apps:** the connect bump only.
