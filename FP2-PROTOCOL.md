# Capability-aware FindProviders2 and connect placement

Design for peer review. Nothing here is built yet.

| | |
|---|---|
| Status | ready for peer review, 2026-11-12 |
| Server | origin/main b8e99de07; every server file cited is identical at cbb7d7306 |
| Connect | origin/main fc949b78 |
| SDK | origin/main 447ed899 |
| Apps | apple origin/main 4254667d; android origin/main 1820ea290 (§5.5 only) |

References are `path:line` at those commits. Paths without a repo prefix are
in the server repo; `connect/`, `sdk/`, `apple/` and `android/` name the
other repos.

## Terms

| Term | Meaning |
|---|---|
| IP axis | `dualstack` (the provider can carry IPv4 and IPv6) or `ipv4` |
| protocol axis | `dualproto` (TCP and UDP) or `tcp` |
| labels | a provider's value on both axes, as FindProviders2 serves it |
| class | one of the four label pairs, in the decided order: **A** `dualstack+dualproto` (full), **B** `ipv4+dualproto`, **C** `dualstack+tcp`, **D** `ipv4+tcp` |
| advertised set | the labels a provider would have with no negative evidence: IP `dualstack` when its connections prove both families, else `ipv4`; protocol `dualproto` |
| negative evidence | a URL run that failed only on later legs judged the provider's (§3.2) |
| downgraded | a provider whose labels are below its advertised set |
| strict decision | today's bucket decision, the hard gate |
| classified decision | the bucket decision judged on the legs of the capabilities the provider keeps (§3.4) |
| legacy mode | a FindProviders2 request without `select`: exactly today's behaviour |
| selector mode | a request with `select` |
| normal windows | connect's quality and speed windows |
| limited window | connect's new window of `tcp` providers, requested with `ipv4+tcp` selectors (§7.4) |
| IPv6 need | how many `dualstack` clients a window still wants: its IPv6 minimum less those it holds (D11.1, §6.3) |
| overflow | `tcp` providers given to a `dualproto` request that the requested protocol cannot fill (§6.3) |
| compatible | a window client whose labels can carry a flow (§7.2) |

## 1. Summary, goals, non-goals

**Summary.**
- FindProviders2 (FP2) classifies providers instead of excluding them. Each
  provider gets two labels and one set of buckets:
  - the IP label (`dualstack` or `ipv4`);
  - the protocol label (`dualproto` or `tcp`);
  - its Quality, Speed and Online membership.
- A provider that fails the URL ratio only because of its HTTP/3 (UDP) legs
  becomes `tcp`. A dual-stack provider that fails only because of its IPv6
  legs becomes `ipv4`. Its buckets are then judged on the legs it keeps.
- A request that names capabilities (`select`) is filled in the decided order
  (D7.2), as refined by D11.1 and D11.2:
  1. the window's IPv6 need, from `dualstack` providers;
  2. then the requested protocol's providers;
  3. then, only when those cannot reach the window's target, `tcp` providers
     as overflow.

  Every answered provider carries both labels.
- A request without `select` gets exactly today's answer: the hard gate, no
  overflow, no new fields.
- In connect, each window client keeps the labels FP2 gave it. A flow is only
  ever placed on a compatible client: UDP needs `dualproto`, IPv6 needs
  `dualstack`.
- A new limited window holds `tcp` providers. Its clients join the race of
  every flow they can carry.
  - Size: half the largest normal window's target, at least 1 (D8). On mobile
    it is 1 if the memory gate fails (D11.5).
  - Rank mode: the performance profile's, `quality` in auto (D10).
- The provider status tells a downgraded provider why (D11.6).

**Goals.**
1. Bring the providers that pass everything but UDP (about 20,700 on Main on
   2026-11-12) back into use for the traffic they can carry, without ever
   routing UDP to them.
2. Make IPv6 placement read the same probe-backed labels.
3. Keep every answer to an existing client byte-identical.

**Non-goals.**
- **No local downgrade on either axis** (owner, decision D7.5). This design
  adds no capability state derived from a client's own flows. Whether the
  shipped local IPv6 downgrade (IPV6.md B5) goes too is the one open question
  (Q8; both options in §7.9).
- **The hard gate stays the default.** Old clients, and new clients talking
  to old servers, get the hard gate.
- **No change to SN25.** Scoring, weights and payouts read no URL outcome and
  do not change (qualityprobe/H3.md, "SN25"). Providers that regain traffic
  earn more through traffic alone.
- **No change to the prober.** Its legs, verdicts, place rules or catalog do
  not change; the record line the monitor parses (monitor/tailer.go:1222-1250)
  is unchanged.
- **No new provider.yml knobs, and no off-by-default settings.**
- **No change to the location list counts, the location stables or the apps'
  Connected status.** They keep reading the strict decision.
- **Out of scope for v1:**
  - serving v6-only providers in selector mode (§3.6);
  - stripping HTTP/3 hints from DNS answers when UDP has no exit.

## 2. Decisions log

All dates are 2026-11-12 unless marked. Quotes are verbatim, typos included.

| # | Decision | Owner's words | Where it lands |
|---|---|---|---|
| D1 (2026-11-11) | HTTP/3 legs count | "tests https and h3 for each provider where the url support both https and h3. A provider should pass the quality probe only if both tcp and udp pass." | qualityprobe/H3.md |
| D2 (2026-11-11) | IP families count | "Pass means passing all probes. This applies to TCP probes and UDP probes." | qualityprobe/IPFAMILY.md |
| D3 (2026-11-11) | per-country UDP switch | "an optional field per country that lets udp probes be disabled for the country" | `udp_probes: false` for cn, ir, ru, tm |
| D4 | the hard UDP gate stays; the Speed/Online split is rejected | "In auto, any udp flow should be kept on quality not speed. I think the hard udp gate is correct right now. A lot of speed/streaming use cases use udp, and a provider not routing udp is an issue." | §1, §6, §7.6. Its "kept on quality not speed" wording is superseded by D11.4. |
| D5 | a protocol-aware FP2 | "have a protocol selector "all" or "tcp-only" in the find providers 2 index, and the ip multi client makes sure that the provider window selected can actually route the traffic" | superseded in detail by D6 |
| D6.1 | two axes; default selectors; bias, then overflow; labels on every result | "I want to also fold in ipv4/ipv4 with udp/tcp. dualstack means dual ipv4/6. dualproto means tcp/ucp. 1. by default windows use dualstack and dualproto selectors. The FP2 biases towards the requested, but if there is not enough, overflows to ipv4-only and tcp-only. The returned results mark each provider with the ip and protocol capabilities." | §3, §5, §6 |
| D6.2 | compatible placement | "2. when selecting a client, the capabilities of the currently active cleints are used. Only compatible capabilities with the flow are considered." | §7.2-7.3 |
| D6.3 | the limited window and merged races | "3. there will be a fallback window that is set up with ipv4 only and tcp only that has a minimum count. The default should be 1. This allows us to maintain the degraded/limited clients to be used when the flows are eligible. Call it the limited window. The flow races merge the normal windows with the limited window clients so that they can race across all clients." | §7.4-7.5; size superseded by D8 |
| D7.1 | no selectors means today's hard gate exactly: strict, no overflow, byte-identical | accepted | §5.4, §6 |
| D7.2 | overflow order: full → `ipv4+dualproto` → `dualstack+tcp` → `ipv4+tcp` | accepted | §6 |
| D7.3 | "not enough" means fewer matching providers in the location than the window's target count | "correct" | §6.3, refined by D11.1 and D11.2 |
| D7.4 | labels come only from negative probe evidence | "if no negative measurement assume dual" (for both axes; unmeasured `udp_probes: false` countries count as `dualproto`) | §3.2-3.3 |
| D7.5 | no local downgrade state on either axis | "it's impossible to tell per-flow failures from provider failures. also it's very hard to tell a failed udp flow" | §1 non-goals. Whether it also covers the shipped B5 is open question Q8 (§7.9). |
| D8 | limited window size | "I want to change the limited window to half the normal window size, min 1." Supersedes "min count 1, default 1" (D6.3). | §7.4 |
| D9 | process | "Ok fully finish FP2-PROTOCOL.md and then i will have a peer review before we start implementation." | this document |
| D10 | the limited window's rank mode follows the performance profile | "the limited window should match the same rank mode as the performance profile. if auto, the limited window should be quality" (answers open question Q3) | §6.2, §7.4, §7.6 |
| D11 | open questions Q1, Q2 and Q4-Q7 settled as recommended | "Q1,Q2,Q4,Q5,Q6,Q7 accept recommendations" | rows D11.1-D11.7 |
| D11.1 (Q1) | the IP axis is enough once the window holds one IPv6-capable client (its IPv6 minimum); only the protocol axis uses the window's target | accepted | §5.1, §6.2-6.3, §7.4 |
| D11.2 (Q2) | "matching providers in the location" means the window's held clients of the requested class plus the candidates the request can still use | accepted | §6.3 |
| D11.4 (Q4) | in auto, UDP goes to quality first, then to compatible speed clients. Supersedes D4's "kept on quality not speed". | accepted | §7.6 |
| D11.5 (Q5) | if the mobile memory gate fails, mobile sizes the limited window at 1 | accepted | §7.4, §7.8, §7.10, §9, §10 |
| D11.6 (Q6) | the provider status tells a downgraded provider why | accepted | §5.5, §7.8 |
| D11.7 (Q7) | a /mainnet note when the apps ship | accepted | §10 |

Whether D7.5 also retires the shipped IPv6 local downgrade (B5) is not
confirmed. It is the one open question (Q8, §11), and §7.9 sets out both
options.

## 3. Capability model

### 3.1 Axes, advertised set, labels

**The IP axis.**
- The advertised IP capability is `dualstack` when the provider's reliability
  row proves both families (`ipv4_proven` and `ipv6_proven`). That is the
  category FP2 already serves as `ip_family: dualstack`
  (model/network_client_ip_family.go:71-92).
- Otherwise it is `ipv4`. That covers v4-only providers, and legacy providers
  that prove nothing.
- A provider whose connections never proved IPv6 is `ipv4` whatever its host
  could do: the URL probe makes no IPv6 leg for it (qualityprobe/IPFAMILY.md).

**The protocol axis.** Every provider advertises `dualproto`; UDP is not
something a provider declares.

**Labels.** A provider's labels are its advertised set, minus each capability
that the URL evidence takes away (§3.3). With no negative evidence the labels
are the advertised set (D7.4).

### 3.2 Which leg outcomes are negative

URL runs are stored and judged as today:
- the HTTP/3 leg's `udp`: qualityprobe/egresshealth/url_h3.go:74-105, 125-140;
- the IPv6 legs: qualityprobe/egresshealth/url_evidence.go:180-201, 500-587;
- place verdicts: model/provider_egress_h3_place.go:149-197,
  model/provider_egress_ipv6_place.go:150-257.

A failed run's **later legs** are the legs made after its IPv4 HTTPS leg
passed. Only later legs judged the provider's are negative evidence.

| Leg outcome, after the place judgement | Negative evidence | Mask bit (§4.1) |
|---|---|---|
| IPv4 HTTPS leg failed | none on either axis; the run fails in every class | none |
| IPv4 HTTP/3 `passed` or `answered` | none (UDP proven) | none |
| IPv4 HTTP/3 `silent`, verdict `provider` | protocol axis | 1 |
| IPv4 HTTP/3 `silent`, verdict `place` | none: the run already counts as its HTTPS leg | none |
| IPv4 HTTP/3 `not_measured` | none: the whole load is not measured | none |
| No HTTP/3 leg: the URL is not `h3`, or `udp_probes_disabled` (cn, ir, ru, tm) | none | none |
| IPv6 HTTPS `passed` | none | none |
| IPv6 HTTPS `failed`, verdict `provider` | IP axis | 2 |
| IPv6 HTTPS `failed`, verdict `place` | none | none |
| IPv6 HTTPS `not_measured` (including `no_aaaa`) | none: the load stands on its IPv4 legs | none |
| IPv6 HTTP/3 `silent`, verdict `provider`, after a passed IPv6 HTTPS leg | UDP over IPv6: removed by dropping either capability | 4 |
| IPv6 HTTP/3 `silent`, verdict `place` | none | none |

How runs combine:
- A run with `ok_count` 0 and a non-zero mask failed **only** on later legs.
  Later legs are only made after the IPv4 HTTPS leg passed (url_evidence.go
  ValidateOutcome).
- The judge stores a run as a pass when every failed later leg was judged the
  place's (model/provider_egress_ipv6_place.go:240-257). So a stored failure
  with a non-zero mask has at least one leg judged the provider's.
- The masks that can occur are 1, 2, 4, 1|2 and 1|4. 2|4 cannot occur: an
  IPv6 HTTP/3 leg is only made after a passed IPv6 HTTPS leg.

Which runs each class forgives, over the window's runs (`ok_count` 0, mask
not 0):

| Class | Capabilities dropped | Runs it forgives |
|---|---|---|
| A (advertised, dual-stack provider) | none | none |
| B from a dual-stack provider | IPv6 | mask & 1 = 0 (masks 2, 4) |
| C (dual-stack provider) | UDP | mask & 2 = 0 (masks 1, 4, 1\|4) |
| D from a dual-stack provider | IPv6 and UDP | every non-zero mask |
| B (advertised, v4-only provider) | none | none |
| D from a v4-only provider | UDP | mask & 2 = 0 (always: no IPv6 legs) |

### 3.3 Classification

The export runs this per provider per pass, from the 8-hour counts (§4.2):

```
classify(p):
    adv := advertised(p)                         // A for dual-stack, B for v4-only
    if p.total < ProviderOnlineMinMeasuredUrlRuns (2):
        return adv                               // no verdict, so no negative measurement
    for class in decidedOrderFrom(adv):          // A,B,C,D, or B,D for a v4-only provider
        if passes4of5(p.ok + forgiven(p, class), p.total):
            return class                         // the first class whose ratio passes
    // no class passes the ratio: drop only what has negative evidence
    labels := adv
    if p.runsWithMaskBit(2) > 0:          labels.ip = ipv4
    if p.runsWithMaskBits(1 or 4) > 0:    labels.protocol = tcp
    return labels
```

- `passes4of5(ok, total)` is today's comparison:
  QualityOkNumerator·total ≤ QualityOkDenominator·ok
  (model/egress_index.go:681-695).
- A provider that passes today's ratio is never downgraded. So the class-A
  and advertised-B providers are exactly today's hard-gate admissions (D7.1).
- The classification reads all accepted outcomes, as the Quality/Speed ratio
  does. The Online rule then reads the general half of the same runs (§3.4).

**Why "the first class whose ratio passes".** It classifies instead of
excluding (D6.1): a provider is demoted only as far as its evidence requires.
It follows the decided order (D7.2), so IPv6 is dropped before UDP. A failed
run that only one of the two drops forgives (mask 4) goes to the IPv6 drop.

### 3.4 Buckets on the kept capabilities' legs

The classified decision is today's `decideProviderEgress`
(model/egress_index.go:894-1004), fed with counts in which the class's
forgiven runs are passes. A runs-only sketch of how those facts are built:

```
classifiedFacts(p, class):
    ok'      := p.ok + forgiven(p, class)                   // Quality/Speed ratio, index, URL weight
    okGen'   := p.general.ok + forgivenGeneral(p, class)    // the Online rule (D4 general half)
    totals, min runs, span, common gates, guard, modern:    unchanged
```

What follows from it:
- **Quality and Speed.** The ratio is 4·T ≤ 5·OK' with today's minimum runs:
  3 spanning 30 minutes for Quality, 2 for Speed
  (model/egress_native_min_runs.go:29, 40, 46). Quality also needs the
  provider not to be ARIN non-quality.
- **Online.** `no_success` is OKg' = 0 with at least 2 general runs.
  `low_success` is 5·OKg' < 4·Tg with OKg' > 0 at the 0.8 ratio, under the
  same country guard and exemption (model/egress_online_success.go:87-100,
  174-190).
- **The guard** is computed and published from the strict decision, as
  today, and both decisions apply it. It counts providers with any success,
  and a UDP-less provider still passes about 47% of its runs.
- **The ranking index and the URL weight** (0.1 + 0.9·ratio,
  model/egress_index.go:699-705) use OK'.
- **Strict and classified agree** for every provider that is not downgraded,
  because its forgiven runs are zero.

### 3.5 Freshness

- **The window** is the 8-hour window of accepted runs
  (`ProviderEgressHealthMaxAge`, model/provider_egress_health_model.go:637).
- **Turns.** A provider gets a URL turn about every 24 minutes; a failed turn
  is retried sooner. The median provider has 20 accepted runs in the window.
- **The export** decides every provider about 15 times an hour. It
  reschedules 30 s after each run (taskworker/work/network_client_location_work.go:20-31;
  model/network_client_score_rank.go:140-150).
- **A cached verdict** vouches until the earlier of its newest run leaving the
  window and 30 minutes past the window the export read (:151-167). Labels
  ride the same verdict.
- **A provider that loses UDP**, where its place carries QUIC, fails about 53%
  of its turns: the share of turns that draw an h3 URL. It is labelled `tcp`
  once its strict ratio falls under 4/5, typically within a few hours.
- **A provider that regains UDP** becomes `dualproto` again once its strict
  ratio passes, as failed runs leave the window: at most 8 hours.
- **IPv6** works the same way, for the IPv6 legs of dual-stack providers.

### 3.6 Edge cases

- **Too few runs** (fewer than 2): advertised labels, and today's treatment
  (Online with a neutral URL weight).
- **Mixed evidence** (some UDP legs pass, some are silent): the ratio decides.
  On Main, 2,521 providers that pass the hard gate had at least one
  UDP-leg-only failure. They stay `dualproto`, exactly as the hard gate admits
  them today.
- **No class passes** (HTTPS failures as well): the provider is in no native
  bucket. The Online rule decides it as today, on its kept legs. Its labels
  drop only the axes with negative evidence. 63,132 providers were in this
  state on Main.
- **`udp_probes: false` countries** (cn, ir, ru, tm): no UDP leg is made, so
  the protocol label stays `dualproto` (D7.4).
- **Place verdicts** are passes; they never lower a label.
- **Unadvertised IPv6:** `ipv4` (§3.1).
- **v6-only providers** (1 connected on Main on 2026-11-11):
  - the URL probe does not claim them, so they have no evidence;
  - the two-axis model assumes IPv4, so selector mode never returns them;
  - legacy requests keep today's `ip_family` facets, which do reach them.
- **IPv6 stand-ins** (IPFAMILY.md): a stand-in's IPv6 verdicts set bits 2 and
  4 exactly as the URL's own would.
- **Recheck and probation runs** never enter the window (`total_count` 0 or
  `url_probation`). Their mask is stored as 0.

## 4. Data model and migration

### 4.1 The history column

One `smallint` mask of the later legs judged the provider's covers both axes.
Two booleans (`udp_only_failure`, `ipv6_only_failure`) do not: a run that
failed on its IPv4 HTTP/3 leg and its IPv6 HTTPS leg (mask 1|2) is forgiven
only by class D, and both booleans would read false for it.

```sql
SET LOCAL lock_timeout = '5s';
ALTER TABLE provider_egress_health_history
    ADD COLUMN IF NOT EXISTS failed_later_legs smallint NOT NULL DEFAULT 0;
```

- **The bits:**
  - 1: IPv4 HTTP/3 silent, judged `provider`;
  - 2: IPv6 HTTPS failed, judged `provider`;
  - 4: IPv6 HTTP/3 silent, judged `provider`.
- **The lock.** On PostgreSQL 18, a column with a constant default changes
  only the catalog of the parent and of every daily partition. Nothing is
  rewritten. ACCESS EXCLUSIVE is held on each table for the catalog change
  only, so ingest waits milliseconds behind it.
- **The 5-second bound** fails the migration rather than queue ingest behind a
  long transaction. `db migrate` is then rerun.
- **No CHECK constraint.** One would scan every partition to validate. The
  store writes only valid masks, and the tests pin them.
- **Ordering.** The migration is appended after main's head when this lands.
  Main is at 963, and migration 964 (the SN25 extender-pool amendment) is
  pending on another branch.
- **Readiness.** A server built with it requires it before it runs
  (MinimumRuntimeMigrationVersion).
- **Older builds** insert with an explicit column list
  (model/provider_egress_health_model.go:401-408), so their rows get 0.

**Ingest.**
- `storeProviderEgressHealth` (:299) sets the mask from the judged evidence:
  `UrlProbeEvidence.FailedLaterLegs(ok, total)`, a pure function in
  qualityprobe/egresshealth/url_evidence.go.
- The mask is non-zero only for a measured run (`total_count` 1) with
  `ok_count` 0. Rechecks and probation get 0.
- The judgement already runs before the store
  (`JudgeProviderEgressLegs`, controller ingest). The mask reads its verdicts.
- `urnetwork_egress_url_failed_later_legs_total{legs}` counts the masks (§8).
- Rows written before the deploy read 0, so they count as plain failures. The
  classification fills in over the first 8 hours, in the safe direction.

**Alternative considered: no column.**
- The same masks can be computed in the 8-hour aggregate from
  `url_probe_evidence`, on failed rows only.
- Measured on Main on 2026-11-12: that aggregate took 4.7-4.9 s against
  6.7-14.4 s for today's plain aggregate in the same hour.
- It is cheap today only because evidence is stored inline and uncompressed:
  a 10-minute sample averaged 1,146 bytes and peaked at 1,834. Documents with
  dual-stack legs cross the ~2 KB TOAST threshold, are pglz-compressed, and
  then every read decompresses them.
- The column costs nothing at read time. It is the design.

### 4.2 The 8-hour aggregate

`providerEgressHealthCountsSql` (model/provider_egress_health_model.go:730-736)
gains integer filters over the window rows, overall and for the general half:

```sql
COUNT(*) FILTER (WHERE ok_count = 0 AND failed_later_legs <> 0 AND failed_later_legs & 1 = 0) AS forgiven_ipv4,
COUNT(*) FILTER (WHERE ok_count = 0 AND failed_later_legs <> 0 AND failed_later_legs & 2 = 0) AS forgiven_tcp,
COUNT(*) FILTER (WHERE ok_count = 0 AND failed_later_legs <> 0)                            AS forgiven_both,
COUNT(*) FILTER (WHERE failed_later_legs & 2 <> 0)                                         AS negative_ip,
COUNT(*) FILTER (WHERE failed_later_legs & 5 <> 0)                                         AS negative_udp
-- and the same five with "AND url_source = 0" for the general half
```

- `ProviderEgressHealthCounts` (:643-659) gains these counts. Its `General`
  gains the same five.
- The query stays jsonb-free.
- The rollup's own window query (model/network_client_reliability_model.go:3157)
  is unchanged. It feeds the `egress_quality` diagnostic, which stays strict.

### 4.3 `ClientScore`

- **New field:** `CapabilityDowngrades uint8`, with bit 1 for IPv6 dropped and
  bit 2 for UDP dropped.
  - The score cache is gob-encoded. An entry from an older exporter decodes
    with zero, which means no downgrade: today's behaviour (the same reason as
    `NetworkOnly` and `IpFamilies`, model/network_client_location_model.go:5200,
    5213).
- **Labels served:**
  - `ip` is `dualstack` when `IpFamily()` is `dualstack` and bit 1 is clear,
    else `ipv4`;
  - `protocol` is `tcp` when bit 2 is set, else `dualproto`.
- **Two records per downgraded provider.** The export sets
  `CapabilityDowngrades` on the provider's shared score, which keeps the
  strict fields for today's facets. Each downgraded provider also gets a
  classified copy, carrying:
  - `PassesMinimums`, `Online`, `UrlProbeSuccessWeight` and `ScaledWeights`
    from the classified decision;
  - `Tiers`, which are performance-only, unchanged.

  The copy is written only to the capability facets (§4.4).

### 4.4 Export facets and keys

Today's facets `d`, `4` and `6` keep exactly today's strict membership and
bytes. Three new facets hold the classified copies of downgraded providers:

| Facet | Class | Who |
|---|---|---|
| `i` | B (`ipv4+dualproto`) | dual-stack providers whose IPv6 was dropped |
| `t` | C (`dualstack+tcp`) | dual-stack providers whose UDP was dropped |
| `x` | D (`ipv4+tcp`) | v4-only providers whose UDP was dropped, and dual-stack providers that lost both |

**Keys.** Same hash tags as today (model/network_client_location_model.go:5355-5368),
for facets `i`, `t` and `x`, in the non-forced pool of each rank mode:
- **union counts:** `{cs_0_<rm>_<caller>_<target>}c_l_<facet>`;
- **union samples:** `{cs_0_<rm>_<caller>_<target>}s_l_<facet>_<i>`;
- **groups:** `_g` in place of `_l`.

Every new facet is written, an empty one included. A reader can then tell an
exporter that predates them from an empty class.

**Native pages.** A separate snapshot holds them.
- Pointer `{cs_0_<rm>_<caller>_<target>}c_l:native_cap_v1`, slots
  `...:native_cap_v1:0` and `:1`, page fields `<facet>:<index>`.
- It uses the publication scripts of model/network_client_score_native.go.
  Its manifest lists exactly `i`, `t` and `x`.
- It cannot share today's `:native_v1` snapshot. A reader rejects any
  manifest whose facet count is not 3 (model/network_client_score_native.go:422-434),
  so adding facets there would make every api, alt and proxy still on the old
  build treat the native source as unavailable.

**Modern partition.** The `{...}m_l` document gains entries for `i`, `t` and
`x`. Readers look up only the facets they read
(model/network_client_score_modern.go:154-173), so old readers are
unaffected.

**Forced pools.** Nothing is added. `cs_1` pools already hold every provider
that passes the common gates. In selector mode a forced read classes each
record by its `CapabilityDowngrades`.

**Named-provider labels.** Two Redis sets are replaced each export, with the
export's TTL and a ready member, like `{provider_hard_exclusions}`
(model/egress_index.go:1041-1103):
- `{provider_capability_dualstack}`: providers labelled `dualstack`, about
  300;
- `{provider_capability_tcp}`: providers labelled `tcp`, about 20,700.

FP2 labels a client-id spec from them (§6.4).

**Cost.**
- About 20,700 downgraded providers are written into the union pools of both
  rank modes. That is about 75% more union pages, by count of members (Online
  holds 26.7k).
- The native capability snapshot holds the class-D Speed natives and the few
  class-D Quality natives.
- The decision work doubles: one more in-memory pass over the facts.
- Measure Redis memory for `cs_` keys before and after the first export (§10
  step 2).

## 5. The FP2 API

### 5.1 Request

JSON Schema (draft 2020-12). Every existing field is unchanged.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "FindProviders2Args",
  "type": "object",
  "properties": {
    "specs": { "type": "array", "items": { "$ref": "#/$defs/ProviderSpec" } },
    "count": { "type": "integer" },
    "force_count": { "type": "boolean", "default": false },
    "exclude_client_ids": { "type": ["array", "null"], "items": { "$ref": "#/$defs/Id" } },
    "exclude_destinations": { "type": ["array", "null"], "items": { "type": "array", "items": { "$ref": "#/$defs/Id" } } },
    "rank_mode": { "type": "string", "description": "unchanged: \"\" (quality), \"quality\", \"speed\", \"online\"" },
    "force_minimum": { "type": "boolean", "default": false },
    "ip_family": { "enum": ["", "v4-capable", "v6-capable", "dualstack", "v4-only", "v6-only"], "default": "" },
    "post_quantum_encryption": { "type": "boolean", "default": false },
    "require_modern_provider": { "type": "boolean", "default": false },
    "strict": { "type": "boolean", "default": false },
    "select": { "$ref": "#/$defs/Select" }
  },
  "$defs": {
    "Id": { "type": "string", "format": "uuid" },
    "ProviderSpec": {
      "type": "object",
      "properties": {
        "location_id": { "$ref": "#/$defs/Id" },
        "location_group_id": { "$ref": "#/$defs/Id" },
        "client_id": { "$ref": "#/$defs/Id" },
        "best_available": { "type": "boolean", "default": false }
      }
    },
    "Select": {
      "description": "Absent or null: legacy mode, today's answer exactly. Present: selector mode.",
      "type": ["object", "null"],
      "properties": {
        "ip": { "enum": ["dualstack", "ipv4"], "default": "dualstack" },
        "protocol": { "enum": ["dualproto", "tcp"], "default": "dualproto" },
        "target_count": { "type": "integer", "minimum": 1, "description": "the window's target count, for the protocol axis; default: count, or 1 when count < 1" },
        "held_count": { "type": "integer", "minimum": 0, "default": 0, "description": "window clients the caller already holds whose protocol label is the requested one (D11.2)" },
        "dualstack_target_count": { "type": "integer", "minimum": 0, "description": "the window's IPv6 minimum (D11.1); default 1 when ip is dualstack; treated as 0 when ip is ipv4" },
        "held_dualstack_count": { "type": "integer", "minimum": 0, "default": 0, "description": "window clients the caller already holds that are labelled dualstack" }
      }
    }
  }
}
```

Unknown members are ignored, as today: the body is decoded with
`json.Unmarshal`.

### 5.2 Response

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "FindProviders2Result",
  "type": "object",
  "properties": {
    "providers": { "type": "array", "items": { "$ref": "#/$defs/Provider" } },
    "window_strict_handover": { "type": "boolean", "description": "unchanged; omitted when unset" },
    "select": { "$ref": "#/$defs/Select", "description": "selector mode only: the selectors the server applied, defaults filled in; absent in legacy mode" }
  },
  "$defs": {
    "Provider": {
      "type": "object",
      "required": ["client_id", "estimated_bytes_per_second", "has_estimated_bytes_per_second", "tier"],
      "properties": {
        "client_id": { "type": "string", "format": "uuid" },
        "estimated_bytes_per_second": { "type": "integer" },
        "has_estimated_bytes_per_second": { "type": "boolean" },
        "tier": { "type": "integer", "description": "selector mode adds 3 x backfill_tier_offset to an overflow provider" },
        "intermediary_ids": { "type": "array", "description": "unchanged; omitted" },
        "network_only": { "type": "boolean", "description": "unchanged; omitted when false" },
        "reputation_failed_names": { "type": "string", "description": "unchanged; omitted when empty" },
        "location": { "type": "object", "description": "unchanged; omitted when unknown" },
        "ip_family": { "enum": ["dualstack", "v4-only", "v6-only"], "description": "unchanged: the proven category; omitted for client-id specs" },
        "match": { "enum": ["region", "country"], "description": "unchanged; omitted for the requested target" },
        "capabilities": {
          "description": "selector mode only, on every provider",
          "type": "object",
          "required": ["ip", "protocol"],
          "properties": {
            "ip": { "enum": ["dualstack", "ipv4"] },
            "protocol": { "enum": ["dualproto", "tcp"] }
          }
        },
        "overflow_step": { "type": "integer", "minimum": 1, "maximum": 1, "description": "selector mode only: 1 for an overflow provider (a tcp provider given to a dualproto request, §6.3); omitted otherwise" }
      }
    },
    "Select": {
      "type": "object",
      "required": ["ip", "protocol", "target_count", "held_count", "dualstack_target_count", "held_dualstack_count"],
      "properties": {
        "ip": { "enum": ["dualstack", "ipv4"] },
        "protocol": { "enum": ["dualproto", "tcp"] },
        "target_count": { "type": "integer", "minimum": 1 },
        "held_count": { "type": "integer", "minimum": 0 },
        "dualstack_target_count": { "type": "integer", "minimum": 0 },
        "held_dualstack_count": { "type": "integer", "minimum": 0 }
      }
    }
  }
}
```

### 5.3 Errors

Errors use the existing "<code> message" convention
(router/handler_utils.go, `RaiseHttpError`).

| Input | Answer |
|---|---|
| `select.ip` not `dualstack` or `ipv4` | 400 `unknown select.ip "<value>"` |
| `select.protocol` not `dualproto` or `tcp` | 400 `unknown select.protocol "<value>"` |
| `select.target_count` present and < 1 | 400 `select.target_count must be at least 1` |
| `select.held_count` < 0 | 400 `select.held_count must not be negative` |
| `select.dualstack_target_count` < 0 | 400 `select.dualstack_target_count must not be negative` |
| `select.held_dualstack_count` < 0 | 400 `select.held_dualstack_count must not be negative` |
| `select.dualstack_target_count` > 0 with `ip: ipv4` | 200; treated as 0 |
| A member of the wrong JSON type | 400 InvalidRequestBodyMessage (router/handler_utils.go:287-294) |
| `select` with `ip_family: v6-only` | 200; discovery returns none (§3.6); named providers are still answered |
| `select` with `force_minimum` | 200; the phases of §6.2 order the forced pool |

The checks run before any spec is read, like `ip_family`'s
(model/network_client_location_model.go:6351).

### 5.4 Compatibility matrix

| | Old server | New server |
|---|---|---|
| **Old client** (no `select`) | today | Legacy mode: byte-identical answers for the same cache state. No new fields. The hard gate, no overflow (D7.1). |
| **New client** (sends `select`) | See below | Selector mode, as designed. The limited window's requests carry the profile's rank mode: `quality` in auto (D10). |

**New client, old server.**
- The server ignores `select` and answers the hard gate, with no
  `capabilities`, no `overflow_step` and no `select` echo.
- The client sees no echo and treats the server as legacy:
  - each entry's labels come from `ip_family` (`dualstack` gives
    `dualstack`; anything else gives `ipv4`);
  - the protocol is always `dualproto`, since every entry passed the hard
    UDP gate;
  - the limited window does not fill;
  - the normal windows keep sending today's `v6-capable` shortfall request
    (IPV6.md B1-B2);
  - placement still checks compatibility with these labels.

**New client, new server, before the first new export.** This is the window
of a rolling deploy.
- The server answers selector mode from today's facets only: classes A and B,
  labelled from records whose downgrade bits are zero.
- Classes C and D are empty, so the limited window finds none and stays
  empty.

**Hosted devices.** The proxy service runs one build of the server and the
sdk, so its devices are always in a same-version cell.

### 5.5 Provider status (D11.6)

The provider status already reports URL checks as the ranking item
`url_checks` (model/provider_status_model.go:966-981), decided by the strict
rule (:777). The item keeps its value, count, total and `passes` as today.

For a downgraded provider, the item gains a `reason` field (string,
omitempty), and its `explanation` appends the same text. There is one
sentence per dropped capability:

| Label | Reason |
|---|---|
| `tcp` | "%d of these checks failed only over HTTP/3 (UDP), so this device is offered for TCP traffic only. Some app builds and some networks do not route UDP." |
| `ipv4` | "%d of these checks failed only over IPv6, so this device is offered for IPv4 traffic only." |

- **Counts:** the provider's `negative_udp` and `negative_ip` over the window
  (§4.2).
- **Labels:** the same classification as the export (§3.3), computed for the
  one provider.
- **Apps:**
  - Android already shows the explanation
    (android/app/app/src/main/java/com/bringyour/network/ui/stats/ProviderStatusPresentation.kt:121),
    so old Android builds show the reason too.
  - The Apple app shows its own help text for this item
    (apple/app/network/Shared/ViewModels/ProviderStatusPresentation.swift:251-262).
    It gains one line for `reason`.
  - The sdk's `ProviderRankingNumber` (sdk/api.go:3360-3371) carries the new
    field.

## 6. Server selection algorithm

### 6.1 Legacy mode

`select` absent or null: `FindProviders2` runs exactly today's code
(model/network_client_location_model.go:6335-7411). That path is not
modified. A golden test pins its answers (§9).

### 6.2 Selector mode

```
FindProviders2(args, session):
    if args.select == nil:
        return legacyFindProviders2(args, session)              // §6.1
    sel := validate(args.select)                                // §5.3; defaults filled
    familyFacets := ipFamilyFacetsForFilter(args.ip_family)     // unchanged, 400 on unknown
    // args.rank_mode is the requesting window's: quality or speed for a normal
    // window; for the limited window, the performance profile's (auto: quality), D10
    count := args.force_count ? args.count : max(args.count, 20)        // unchanged (:6427)
    loadCount := findProviders2LoadCount(count, len(exclusions))         // unchanged (:6325)
    answer := namedProviders(args.specs)                        // unchanged gates; labels §6.4
    ipNeed := 0
    if sel.ip == dualstack:
        ipNeed = max(0, sel.dualstack_target_count - sel.held_dualstack_count)   // D11.1
    for level in [request targets, region, country]:            // location fallback, unless strict (unchanged)
        discovered := []
        // 1. the IPv6 need: dualstack classes within the protocol ceiling (§6.3)
        if ipNeed > 0:
            need := fill(pool(needClasses(sel) ∩ keepFamilies(args.ip_family)),   // §6.3 table
                         args.rank_mode, args.force_minimum, ipNeed)              // A, then C
            for p in need where sel.protocol == dualproto and labels(p).protocol == tcp:
                p.tier += 3 * BackfillTierOffset                // a C taken for the need is overflow
                p.overflow_step = 1
            discovered += need
        // 2. the requested protocol's classes, as one pool (D11.1)
        main := pool(protocolClasses(sel.protocol) ∩ keepFamilies(args.ip_family))
        discovered += fill(main, args.rank_mode, args.force_minimum,
                           count - len(discovered))             // never redraws a provider
        // 3. overflow: a dualproto request that is not enough (D7.3, D11.2)
        if sel.protocol == dualproto and sel.held_count + eligible(main) < sel.target_count:
            over := fill(pool({C, D} ∩ keepFamilies(args.ip_family)),
                         args.rank_mode, args.force_minimum, count - len(discovered))
            for p in over:
                p.tier += 3 * BackfillTierOffset                // 33 with the default offset
                p.overflow_step = 1
            discovered += over
        for p in discovered:
            p.capabilities = labels(p)                          // §4.3
        if discovered is not empty or args.strict or no level left:
            break
    return answer + discovered, echo sel
```

### 6.3 Classes per phase, family filter, "enough"

| Request (`ip`, `protocol`) | IPv6 need from | Main pool | Overflow pool |
|---|---|---|---|
| `dualstack`, `dualproto` (normal windows) | A, then C if no A is eligible | A ∪ B | C ∪ D, tier +33 |
| `ipv4`, `dualproto` | none | A ∪ B | C ∪ D, tier +33 |
| `dualstack`, `tcp` | C | C ∪ D | none |
| `ipv4`, `tcp` (the limited window) | none | C ∪ D | none |

**How D7.2 and D11.1 combine.** The decided order A → B → C → D still holds:
- `dualstack` comes before `ipv4` for the IPv6 need;
- `dualproto` comes before `tcp` for the window's target.

Once the IPv6 need is met, the IP axis no longer orders the draw (D11.1). So A
and B form one pool, and C and D another. A pool of several classes reads
their facets in one shuffled order, as online requests read theirs
(model/network_client_location_model.go:6124).

**The ceiling.** A `tcp` request never receives a `dualproto` provider. A C
provider given to a `dualproto` request for its IPv6 need is overflow: tier
+33, `overflow_step` 1.

**`keepFamilies`** applies `ip_family` as a filter on IP labels:

| `ip_family` | Classes kept |
|---|---|
| `""` or `v4-capable` | every class |
| `v6-capable` or `dualstack` | the `dualstack` classes (A, C) |
| `v4-only` | the `ipv4` classes (B, D) |
| `v6-only` | none (§3.6) |

**"Enough" (D7.3, D11.2).**
- A `dualproto` request has enough when `held_count` plus the eligible
  candidates of the main pool reaches `target_count`. Only then does it skip
  overflow.
- Eligible candidates are those left after the request's exclusions, hard
  exclusions, offline and incapable filters, counted up to the load budget.
- Held clients count toward the target, and excluded providers do not. So a
  full window never overflows to fill a slot it does not have, and a pool
  depleted by exclusions overflows rather than leaving the window short.

**The IPv6 need (D11.1).** It is `dualstack_target_count` (the window's IPv6
minimum) less `held_dualstack_count`. A quality window wants 1 and a speed
window 0 (connect/ip_remote_multi_client.go:232), as today's soft minimum
does.
- So the IP axis reserves at most one slot per quality window, and none in a
  speed window.
- Beyond that slot, dual-stack providers are drawn only in proportion to their
  share of the main pool. On Main, us has 108 dual-stack hard-gate providers
  among 20,991 (§8.4).

### 6.4 Pools and labels

- **`pool(classes)`** reads, for each class, non-forced:

  | Class | Today's facet, records with no downgrade | Capability facet |
  |---|---|---|
  | A | `d` | none |
  | B | `4` | `i` |
  | C | none | `t` |
  | D | none | `x` |

  - A record found in today's facets with a non-zero `CapabilityDowngrades`
    is skipped. Its classified copy in a capability facet is the one used.
  - **Forced:** today's forced pool, with each record put in the class its
    `CapabilityDowngrades` and `IpFamily()` give.
- **Named providers** (client-id specs) pass today's gates and come first.
  Their labels come from the two sets of §4.4: membership gives `dualstack`
  or `tcp`; absence gives `ipv4` or `dualproto`.

### 6.5 The fill per rank mode, unchanged inside each pool

- **Quality and speed:**
  1. the requested mode's natives (`PassesMinimums[mode]`; Quality also needs
     the live quality read);
  2. then the other mode's natives at tier + `BackfillTierOffset` (11),
     within Quality's cutoffs;
  3. then the Online union at tier 2·11 (:7103-7281).
- **Online:** a uniform draw of `Online` members weighted by
  `ScaledWeights[online]` (:7083-7101).
- **force_minimum:** the forced pool, common gates only; no borrowing, as
  today.

The rest is today's, within each pool:
- the draw: `selectClientScoresByWeight` with modern-first partitioning
  (:6583-6587; model/network_client_selection_evidence.go:214);
- the weights: `ScaledWeights[mode]` (model/network_client_score_rank.go:233),
  from the classified URL weight for classified copies;
- the hard exclusion and live liveness reads;
- `require_modern_provider`, `post_quantum_encryption`, network-only and
  reputation.

### 6.6 Overflow tier offset

Overflow providers get 3 × `BackfillTierOffset` added to their tier: 33 with
today's default (model/egress_index.go:208).
- The highest tier a main-pool provider can carry before client demerits is
  the Online band, 22.
- Connect adds at most `MaxClientTierDemerit` (7, :189), so 29.
- 33 therefore keeps every main-pool entry ahead of every overflow entry after
  demerits, as `BackfillTierOffset` does for borrowing.
- Connect does not bound tiers.

## 7. Connect

### 7.1 Capability of a window client

- **`DestinationStats`** gains `Capabilities` (connect/ip_remote_multi_client.go:72-96).
- **`addProviderDestinations`** (connect/ip_remote_multi_client_api.go:1000-1023)
  fills it:
  - from `capabilities` when the answer echoes `select`;
  - otherwise from `ip_family` with `dualproto` (§5.4).
- **`multiClientChannel`** gains `supportsFlow(ipVersion, ipProtocol)`. It
  reads only these labels (D6.2, D7.5). A fixed destination supports every
  flow: it is the user's choice, as `supportsIpVersion` already says
  (connect/ip_remote_multi_client_family.go:53-66).

### 7.2 Compatibility

| Flow | IP label needed | Protocol label needed | Compatible classes |
|---|---|---|---|
| TCP over IPv4 (TCP 443 included) | any | any | A, B, C, D |
| TCP over IPv6 | `dualstack` | any | A, C |
| UDP over IPv4 (QUIC included) | any | `dualproto` | A, B |
| UDP over IPv6 | `dualstack` | `dualproto` | A |
| ICMP over IPv4 | any | any | A, B, C, D |
| ICMP over IPv6 | `dualstack` | any | A, C |

ICMP follows TCP: no probe measures ICMP, so no evidence exists against it.

### 7.3 Placement sites

Every site that places, adopts, rebinds or donates a flow today checks
`supportsIpVersion`. Each moves to `supportsFlow` on the flow's path. Sites
that gather by IP version alone take the protocol as well.

| Site (connect/) | Function | What it chooses |
|---|---|---|
| ip_remote_multi_client.go:4094-4100 | `raceCandidates` | the race field of a window: ordered, cross-tier, last resort |
| ip_remote_multi_client.go:8577-8691 (8603, 8605) | `coalesceOrderedClients` in `sendParsedPacketGroupAttempt` | the windows a flow tries; merges the limited window (§7.5) |
| ip_remote_multi_client.go:16428 (16455, 16478) | `orderedClients` | a window's ordered offer |
| ip_remote_multi_client.go:16401-16420 | `OrderedClientsForIpVersion`, `orderedClientsCrossTierForIpVersion` | per-version offers |
| ip_remote_multi_client.go:16295-16311 | `lastResortClients`, `lastResortClientsForIpVersion` | the benched field when nothing is offered |
| ip_remote_multi_client.go:4698 | `inheritAffinityClient4WithLock` | affinity donor for an IPv4 flow |
| ip_remote_multi_client.go:4790 | `inheritAffinityClient6WithLock` | affinity donor for an IPv6 flow |
| ip_remote_multi_client.go:5487, 5595 | `sendUpdate` | the scored affinity donor, IPv4 and IPv6 |
| ip_remote_multi_client.go:6222-6260 | `rebindCandidates` | QUIC rebind candidates across all windows, limited included |
| ip_remote_multi_client.go:6474-6481 | `rebindFlowsWithLock` (`carriesUpdates`) | a replacement must carry every rebound flow |
| ip_remote_multi_client.go:10141 | `clientReceivePacketResolve` | adoption of a flow by the client that answered |
| ip_remote_multi_client_affinity_syn.go:72 | `reuseAffinitySyn` | SYN affinity reuse |
| ip_remote_multi_client_affinity_time.go:168 | `adoptAffinityClientWithLock` | time affinity adoption |
| ip_remote_multi_client_dns_affinity.go:172 | `inheritDnsExitHintWithLock` | the exit that carried a flow's DoH lookup |
| ip_remote_multi_client_service_refresh.go:65, 87 | `requestServiceRefresh` | the exits a destination-service refresh moves to |
| ip_remote_multi_client_syn_retry_publication.go:249 | `enqueuePublishedSynOwner` | the SYN retry owner (TCP: the IP label decides) |
| ip_remote_multi_client_discovery_demand.go:439 | `requestDiscoveryDemand` | demand counts per family; adds the protocol |
| ip_remote_multi_client_prober.go:392 | `probeProviderPass` | the probe family (TCP probes: the IP label decides) |
| ip_remote_multi_client_family.go:330-345 | `ipv6Available` | window has an IPv6-capable client (from the IP label) |
| ip_remote_multi_client_family.go:413-445 | `selectFamilySwapVictim` | the v6 shortfall swap (IP label) |
| ip_remote_multi_client_family.go:695-702 | `candidatesForIpVersion` | becomes `candidatesForFlow` |

### 7.4 The limited window

| Property | Value |
|---|---|
| Window type | `WindowTypeLimited` (3), beside quality (1) and speed (2) (connect/ip_remote_multi_client.go:99-120) |
| Selectors | `{"ip": "ipv4", "protocol": "tcp"}`: the `tcp` classes C and D, never a `dualproto` provider (§6.3) |
| Rank mode | the performance profile's (D10): auto uses `quality`, a fixed Quality profile `quality`, a fixed Speed profile `speed`. Read from the window's profile when each request is built; normal windows read their own type (connect/ip_remote_multi_client_family.go:448). |
| `ip_family` | `v4-capable` |
| Size setting | `MultiClientSettings.LimitedWindowSizeFraction`, default 0.5 |
| Target | `max(1, floor(f × S))` for f > 0. S is the largest current target among the active normal windows, the `targetWindowSize` each window's resize pass computes (:14399-14425). In a fixed profile the inactive window's target is 0. f ≤ 0: no limited window. |
| Hard max | `max(1, floor(f × the largest normal hard max))` |
| Desktop auto | normal targets 6 (quality) and 1 (speed; max 2): S = 6, limited 3 |
| Mobile auto | normal 4 and 1 (sdk/mobile_memory_policy.go:237-238): S = 4, limited 2. If the memory gate fails, 1 (D11.5). |
| Mobile memory gate | The steady-memory test of §9 decides. If the device stays within its 24 MiB target with the limited window at 2, mobile uses 2; otherwise the mobile policy sizes it at 1 (D11.5). Desktop is unaffected. |
| Fixed profile of size N | `max(1, floor(N/2))` |
| Fixed destination | none: the fixed exit carries every flow |
| Fill and refill | its own `resize` loop (:13895) and `expand` (:14845), like a normal window. Each request carries `target_count` (its target), `held_count` (its `tcp` clients) and `dualstack_target_count` 0. There is no IPv6 need. |
| No `tcp` supply | the window stays empty and costs nothing |
| Old server (no echo) | the window does not fill |
| Profile change | Handled exactly like the normal windows. An equal profile is a no-op (:3403-3408). Otherwise `SetPerformanceProfile` (:3394-3430) installs the new profile on every window in `self.windows`, the limited window included (:13381-13400: it also forgets sticky and recent exits). `shuffle` then cancels every client (:16231-16238). Those clients' flows go through `removeClient` (:5718): QUIC flows are rebound to compatible candidates, and TCP flows are torn down. The resize loop refills under the new rank mode, with the target recomputed from the new active normal windows. |
| Count | one per multi client |

**Normal windows.**
- Each request carries `select: {"ip": "dualstack", "protocol": "dualproto"}`
  (D6.1) with:
  - `target_count`: the window's target;
  - `held_count`: its clients labelled `dualproto`;
  - `dualstack_target_count`: its IPv6 minimum (`WindowSizeMinIpv6Capable`:
    quality 1, speed 0; connect/ip_remote_multi_client.go:232);
  - `held_dualstack_count`: its clients labelled `dualstack` (D11.1).
- In selector mode the main request carries the IPv6 need. The separate
  `v6-capable` shortfall request (IPV6.md B1-B2) is therefore sent only to a
  server that did not echo `select`.
- The generator interface gains
  `NextDestinationsWithCapabilities(count, exclude, rankMode, ipFamily, select)`.
  `ApiMultiClientGenerator` implements it; a generator without it is called
  as today (connect/ip_remote_multi_client.go:171-173).
- The single limited window is sized from the largest normal window. A
  limited window per normal window would add the speed window's share again
  (1 more client) for the same supply.

### 7.5 Merged races

- In `coalesceOrderedClients`, each selected normal window's compatible
  candidates are followed by the limited window's compatible candidates.
- Today's filters then run on the merged list: service failures, reputation,
  performance, same-network and scored placement.
- The race dials every candidate in parallel and binds the lowest measured RTT
  (`MultiRaceClientCount` 0, :377-402). Limited clients therefore compete on
  equal terms for the TCP flows they can carry.
- Under a degraded backend the race is bounded
  (`MultiRaceDegradedClientCount`), and the normal windows' candidates come
  first.
- Limited clients are `tcp` (classes C and D), so they never join a UDP race.
  A class-C client also joins TCP/IPv6 races (§7.2).
- When the selected windows offer nothing compatible, a TCP flow races the
  limited window's compatible clients alone.

### 7.6 Auto, fixed profiles, rebind and donors

- **Auto** (`selectWindowTypes`, :3552-3576):

  | Flow | Windows, in order |
  |---|---|
  | UDP, any port | quality, then speed's compatible clients |
  | TCP 443, or streaming | quality, then speed (unchanged) |
  | Other TCP | speed, then quality (unchanged) |

  - Today, non-443 UDP goes speed-first.
  - The limited window is merged into every list (§7.5).
  - D11.4 settles UDP: quality first, then speed's compatible (`dualproto`)
    clients. It supersedes D4's "kept on quality not speed". Compatibility
    filtering removes the risk D4 named.
- **Fixed profiles:**
  - the active window carries every flow, compatible clients only;
  - the limited window is merged, and requests the profile's rank mode
    (D10);
  - the inactive window holds none.
- **Auto:** the limited window requests `quality` (D10).
- **Rebind** (`QuicRebindOnExitLoss`, :1531-1545): candidates come from every
  window. `carriesUpdates` (:6474-6481) admits only clients compatible with
  every rebound flow. A QUIC flow never moves to a `tcp` client, and TCP is
  not rebound.
- **Affinity donors:** a donor must be compatible with the new flow. A QUIC
  flow cannot inherit the `tcp` exit of its site's TCP flows. It then egresses
  from a different address, as already happens when an exit dies.

### 7.7 No compatible client

- **While no window has added a client**, a flow waits for offers
  (`FormationPollTimeout`), as today.
- **Once the windows have formed:**

  | Flow with no compatible client | Answer |
  |---|---|
  | UDP (IPv4 or IPv6) | ICMP port unreachable (`ipOosUnreachable`, connect/ip_packet.go:157). QUIC stacks fall back to TCP at once. Counted (`udpNoRouteCount`). |
  | IPv6 TCP or ICMP | today's B6 no-route (:8721-8733) and the interceptor's empty AAAA (`SetIpv6Unroutable`, sdk/device_local.go:5611), now decided from IP labels |
  | IPv4 TCP or ICMP | every client is compatible, so this happens only with no client at all: today's wait and retry |

- `UdpUnroutable()` mirrors `Ipv6Unroutable()`
  (connect/ip_remote_multi_client_family.go:605-658): the windows have formed
  and no window holds a `dualproto` client.

### 7.8 sdk and apps

- **Window status** (sdk/device.go:425-429; sdk/device_local.go:5776) gains:
  - `UdpAvailable`, beside `Ipv6Available`;
  - `LimitedWindowSize`.
- **Mobile memory policy** (sdk/mobile_memory_policy.go:544-558) keeps the
  normal sizes. The limited window follows the fraction, giving 2 on mobile.
  If the steady-memory test fails, the policy sizes it at 1 (D11.5).
- **Developer menu:** `LimitedWindowSizeFraction` is exposed beside the
  reliability controls (sdk/reliability_controls.go) for testing.
- **Provider status (D11.6):**
  - the sdk's `ProviderRankingNumber` (sdk/api.go:3360-3371) gains `Reason`;
  - the Apple app shows it on its network-checks row (§5.5);
  - Android already shows the server's explanation.

  This is the only app change.
- **No other app API or UI change.** Auto stays the default
  (sdk/device_local.go:9077-9078). Connected stays count-based on the normal
  windows (sdk/connect_view_controller.go:1215).

### 7.9 The shipped local IPv6 downgrade (B5): open question Q8

This design adds no local capability state (D7.5). Connect already ships one
piece, the IPv6 local downgrade of IPV6.md B5: an exit whose IPv6 dials starve
while IPv4 stays healthy is latched to v4-only for the life of the window. The
owner has not said whether D7.5 retires it. Both options:

**Option A: retire B5.** Placement reads only the server's labels.
- Removed:
  - `maybeDowngradeIpv6` (connect/ip_remote_multi_client_family.go:68-85);
  - its trigger on the dial-failure path (connect/ip_remote_multi_client.go:10406-10407);
  - `noteIpFamilyDowngrade` (family.go:661-676);
  - the `ipFamilyDowngraded` latch (connect/ip_remote_multi_client.go:17253-17259);
  - `ipv6DialStarvedWithLock` (family.go:97-138).
- `IpFamily()` (family.go:40-51) returns the server's IP label. The
  version-split dial-strike record stays, because ordinary starvation
  warnings read it.
- IPV6.md B5 is marked superseded.
- **Effect:** a dual-stack exit whose IPv6 breaks keeps getting IPv6 flows
  until its label turns (§3.5). Applications fall back to IPv4 meanwhile.

**Option B: keep B5 as shipped.** Nothing in B5 changes.
- The IP label a placement reads is the server's label, lowered by B5's latch
  where it fired. The latch only ever lowers `dualstack` to `ipv4`.
- D7.5 then applies to new local state only. This design adds none, and in
  particular no UDP downgrade.
- **Effect:** IPv6 flows leave a starved exit within a session, as today.

Every other part of this design works unchanged under either option.

### 7.10 Costs per device

| | Desktop | Mobile |
|---|---|---|
| Window clients today | quality 6, speed 1 (max 2): 7-8 | quality 4, speed 1: 5 |
| Limited window | 3 | 2, or 1 if the memory gate fails (D11.5) |
| Window clients after | 10-11 (+38-43%) | 7 (+40%), or 6 (+20%) |
| Contracts | +3 per direction while the limited clients carry data. The wide race sends each cold-start TCP flow's first packet to them, so they open contracts early. | +2 (or +1) |
| Control plane | +3 client setups (auth and control registration) and continuous pings (`CPingRestTimeout` 10 s, `CPingMaxByteCountPerSecond` 32 KiB/s; connect/ip_remote_multi_client.go:255, 264) | +2 (or +1) |
| Memory | each client holds its own transport graph and buffers | the steady target of 24 MiB (sdk/mobile_memory_policy.go:14) was calibrated for 4 + 1 clients. Per-client buffers include a resend queue of 64-512 KiB and an unreliable flight of 128 KiB (:224-236). The limited window adds two, or one if the gate fails (D11.5). |

Race duplication: each cold-start TCP flow's first packet goes to up to 3 more
exits on desktop and 2 (or 1) on mobile.

## 8. Metrics, dashboards, alerts, measures

### 8.1 Server

| Metric | Labels | Source | Dashboard |
|---|---|---|---|
| `urnetwork_egress_url_failed_later_legs_total` | `legs` = `v4udp`, `v6tcp`, `v6udp`, `v4udp_v6tcp`, `v4udp_v6udp` | ingest | urnetwork / provider quality probes |
| `urnetwork_stats_provider_egress_capability` | `bucket` (quality, speed, online), `ip`, `protocol` | stats collector, classified decision | urnetwork / providers |
| `urnetwork_findproviders2_select_requests_total` | `rank_mode`, `ip`, `protocol` | FP2 | urnetwork / providers |
| `urnetwork_findproviders2_select_answered_total` | `rank_mode`, `overflow_step` (0 or 1), `ip`, `protocol` | FP2 | urnetwork / providers |
| `urnetwork_findproviders2_select_overflow_total` | `rank_mode`: requests given `tcp` overflow | FP2 | urnetwork / providers |
| `urnetwork_findproviders2_select_ipv6_need_total` | `rank_mode`, `outcome` (`met`, `short`): requests with an IPv6 need, and whether the answer met it (D11.1) | FP2 | urnetwork / providers |

`urnetwork_stats_provider_egress_index` and `urnetwork_stats_provider_excluded`
keep the strict decision, unchanged (controller/stats_collector.go:80-86).

### 8.2 Hosted devices

These are connect process counters that the proxy exports, like
proxy/window_client_metrics.go:21-56.

| Metric | Labels |
|---|---|
| `urnetwork_proxy_window_clients` | `window` (quality, speed, limited), `ip`, `protocol` |
| `urnetwork_proxy_window_flows_total` | `window` of the winning client, `ip_version`, `protocol` |
| `urnetwork_proxy_window_no_compatible_total` | `ip_version`, `protocol` |

The existing `window` label of the proxy window metrics gains `limited`
(connect/ip_remote_multi_client_window_client_stats.go:22-45). Dashboard:
urnetwork / proxy. Apps report the same counts in their window status and
diagnostics; they have no Prometheus.

### 8.3 Alerts

No new alert in v1. The decisions are additive, and each metric has a
dashboard. Every new metric must still pass the ./grafana tests (for example
grafana/grafana_test.go:268), and no alert class changes
(`TestEveryAlertClassIsClassified`, monitor/alert_classes_test.go:468).

### 8.4 Measured on Main (read-only, 2026-11-12)

**Classification**, over the 8-hour window ending about 16:05Z:

| | Providers | Of which dual-stack |
|---|---|---|
| A, strict (`dualstack+dualproto`) | 280 | 280 |
| B, strict (v4-only, `ipv4+dualproto`) | 26,242 | 0 |
| B from IPv6 (`ipv4+dualproto`) | 2 | 2 |
| C (`dualstack+tcp`) | 14 | 14 |
| D from UDP (v4-only, `ipv4+tcp`) | 20,657 | 0 |
| D from both | 0 | 0 |
| No class passes | 63,132 | 294 |
| Fewer than 2 runs | 3,347 | 2 |

**UDP**, over the windows ending 15:15-16:05Z:
- 221,852 of 1,960,099 runs (11.3%) failed only on an HTTP/3 leg.
- 964,173 runs passed (49.2%).
- 52,396 providers had at least one such failure.
- Of the providers UDP alone kept out, 89% proved no UDP at all in the window.

**Dual-stack supply among hard-gate providers**, by country (dual-stack of
all):

| us | hk | se | sg | gb | de | jp | tw | fr |
|---|---|---|---|---|---|---|---|---|
| 108 of 20,991 | 36 of 122 | 30 of 225 | 22 of 97 | 21 of 388 | 16 of 196 | 15 of 30 | 14 of 32 | 5 of 235 |

**IPv6 flow share.** No fleet source exists today.
- The proxy metrics carry the protocol but not the family.
- Connect counts IPv6 no-route replies only per process.
- After the proxy deploy, `urnetwork_proxy_window_flows_total` gives hosted
  devices' shares by `ip_version` and `protocol` within a day.
- The protocol side is already known: QUIC is 56% of traffic
  (connect/ip_remote_multi_client.go:1539, commit 4948a163, 2026-08-02). That
  supports D7.2's order: UDP before IPv6.

### 8.5 Expected gains and success measures

- **Protocol axis.**
  - About 20,700 providers (C + D) return in selector mode.
  - Speed eligibility at the ratio level rises from 26.5k to 47.2k, about
    45k after the gates (the gauge reads 96% of the ratio count).
  - Online rises from 30.3k to 50.5k at the ratio level, about 44.5k after
    the gates (88%).
  - They reach users through:
    - the limited windows: 3 per desktop device, 2 per mobile device (1 if
      the memory gate fails);
    - the normal windows, when a location has fewer `dualproto` providers
      than a window's target.
- **IP axis.** 2 providers return. Its value is correct IPv6 placement, not
  supply.
- **Success:**
  - after the server deploy, `urnetwork_stats_provider_egress_capability{protocol="tcp"}`
    settles near 20k for Speed within 8 hours;
  - after the clients ship, limited windows are filled
    (`urnetwork_proxy_window_clients{window="limited"}`);
  - class-D providers with contracts in the trailing 24 hours reach the
    thousands;
  - TCP failure rates on `tcp` clients match those on `dualproto` clients;
  - `udpNoRouteCount` stays near zero where `dualproto` supply exists.

## 9. Test plan

**qualityprobe/egresshealth (unit):**
- `FailedLaterLegs` returns the right mask for every combination:
  - the IPv4 HTTP/3 leg: passed, answered, silent+provider, silent+place,
    not_measured;
  - the IPv6 HTTPS leg: passed, failed+provider, failed+place, not_measured,
    no_aaaa;
  - the IPv6 HTTP/3 leg: passed, silent+provider, silent+place;
  - a stand-in;
  - `udp_probes_disabled`;
  - a URL without h3;
  - a passing run, which is 0.
- 2|4 never occurs.

**Store (DB):**
- The mask is written per case.
- Rechecks, probation runs and replays store 0.
- An older-build insert stores 0.

**Aggregate (DB):**
- The five counts, overall and general.
- Rows from before the migration are never forgiven.
- The general half excludes country outcomes.

**Classification (unit):**
- The boundaries 4·T = 5·OK' and 4·T = 5·OK' − 1, for every class.
- v4-only against dual-stack.
- Fewer than 2 runs gives the advertised labels.
- No class passes: only the axes with evidence drop.
- A forgiven run, mask 4, goes to B before C.
- `udp_probes: false` stays `dualproto`.
- The country exemption is unchanged.
- Today's egress tests pass unmodified: the strict decision is untouched.

**Export (DB and Redis):**
- The `i`, `t` and `x` keys are written, empty ones included.
- Today's `d`, `4` and `6` keys and the `:native_v1` snapshots are
  byte-identical with and without the change (golden).
- `:native_cap_v1` has its own manifest; today's reader accepts the old
  snapshot unchanged.
- The modern document's new entries are ignored by today's reader.
- The named-provider sets are replaced each export.

**FP2 (DB and Redis):**
- **Legacy mode:** byte-identical answers (golden, every rank mode,
  `force_minimum`, `ip_family`, `strict` with fallback).
- **Selector mode:**
  - **the IPv6 need (D11.1):**
    - `dualstack_target_count` 1 with `held_dualstack_count` 0 and 1;
    - A eligible; A absent with C eligible, which is given as overflow;
      neither;
  - **the main pool:** A and B draw together, with no A preference beyond the
    need;
  - **overflow only when `held_count` plus the eligible main candidates is
    below `target_count` (D11.2):** at `target_count − 1`, `target_count` and
    `target_count + 1`, and with exclusions depleting the pool;
  - **the ceiling:** a `tcp` request never gets a `dualproto` provider; the
    limited window's request gets C and D;
  - labels on every entry; `overflow_step` 1 and tier +33 on overflow entries
    only;
  - each 400, including the two IPv6-need fields;
  - `force_minimum`;
  - named providers labelled from the sets;
  - `ip_family` v6-capable gives A and C;
  - `rank_mode: online`;
  - skipped downgraded records in today's facets;
  - the location fallback climbs only when every class is empty;
  - a cache from before the export yields A and B only.

**Provider status (D11.6):**
- `reason` and the explanation sentence for `tcp`, for `ipv4`, for both and
  for neither, with the counts;
- `url_checks` value, count, total and `passes` are unchanged;
- Android shows the explanation; the Apple row shows `reason`.

**Compatibility matrix, one test per cell (§5.4):**
- old request on the new server is byte-identical;
- a new request on an old server's stub gives no echo, so labels come from
  `ip_family` and the limited window stays empty;
- the new-new cell;
- the rolling-deploy cell.

**connect (unit):**
- the compatibility table (§7.2) for every class;
- for each site of §7.3, a test that a UDP, IPv6 or TCP flow is never bound,
  adopted, rebound or donated to an incompatible client. Extend the
  source-inspection tests in ip_remote_multi_client_flow_cap_test.go;
- limited window sizing:
  - the fraction, the floor, at least 1;
  - the largest normal target;
  - a fixed profile;
  - a fixed destination gives none;
  - f ≤ 0 gives none;
  - the hard max;
- the limited window's request: selectors, target and held counts, and
  `dualstack_target_count` 0;
- normal windows' requests: `dualstack_target_count` 1 (quality) and 0
  (speed), with the held counts; the `v6-capable` shortfall request goes only
  to a server without the echo;
- the limited window's rank mode, one case per profile (D10):
  - auto requests `quality`;
  - fixed Quality requests `quality`;
  - fixed Speed requests `speed`;
- a profile switch while connected, for auto → fixed Speed, fixed Speed →
  auto, and fixed Quality → fixed Speed:
  - the limited window's clients are cancelled with the normal windows';
  - QUIC flows are rebound and TCP flows torn down, as in `removeClient`;
  - the next requests carry the new rank mode;
  - the target is recomputed;
  - an equal profile changes nothing;
- the merged race: compatible limited clients join TCP races only, IPv6 ones
  only when labelled `dualstack`; and the degraded-backend order;
- auto: UDP tries quality, then speed's compatible clients;
- no compatible client: UDP gets port unreachable after formation and waits
  during formation; B6 reads labels;
- old-server detection;
- B5, per the answer to Q8:
  - under option A, no capability change after dial strikes;
  - under option B, B5's latch lowers a `dualstack` label, and the exit then
    gets no IPv6 flow.

**sdk:**
- the window status fields;
- the mobile policy's limited window: 2, or 1 when the memory gate fails
  (D11.5);
- a steady-memory measurement on iOS and Android with all windows filled,
  against the 24 MiB target:
  - it runs with the limited window at 2;
  - if that exceeds the target, it runs again at 1, which then ships
    (D11.5);
  - this gates the client release.

**Integration:**
- A local provider with UDP blocked is labelled `tcp`. It lands in the
  limited window, wins TCP/IPv4 races, and never receives QUIC.
- A dual-stack local provider with IPv6 blocked is labelled `ipv4` and gets
  no IPv6 flow. Use the local provider harness of `TestH3LegThroughALocalProvider`
  and `TestIpv6LegsThroughALocalDualStackProvider`.

**Merge set:** the server's model and controller DB tests, the ./grafana
tests and `TestEveryAlertClassIsClassified`.

## 10. Rollout and rollback

Every switch is on by default (owner preference: code on by default, timed by
when it merges). Old clients never enter selector mode, so each server step is
inert for them.

| Step | What | Must precede | Rollback |
|---|---|---|---|
| 1 | `db migrate` (§4.1) | any server build with this change (readiness) | none needed: the column is unused by older builds and stays |
| 2 | taskworker: ingest writes the mask; the export classifies and writes the capability facets, the native capability snapshot and the sets; stats gauges | step 3 for full value (not required for safety) | previous build. Capability keys expire with the export TTL (300 min); legacy keys are unchanged throughout. |
| 3 | api, alt and the proxy service's server side: selector mode and the provider status reason (D11.6) | the client release | previous build: new clients see an old server (§5.4) |
| 4 | proxy service with the new connect and sdk (hosted devices) | the app release, as the first fleet measurement of §8.2 | previous proxy build |
| 5 | apps through the nightly: the memory gate decides the mobile limited size (D11.5); the Apple status row shows the reason (D11.6); a /mainnet note goes out with it (D11.7) | none | the previous release |

- **Measure between steps 2 and 3.** Watch Redis memory for `cs_` keys,
  export duration, and the new gauges filling over 8 hours.
- **The client settings** `LimitedWindowSizeFraction` (0.5; 0 turns the
  window off) and the selectors are compiled defaults. They are exposed in the
  developer menu for testing; a production change is a release.
- **IPV6.md B5:** marked superseded under option A of Q8; unchanged under
  option B.
- **The /mainnet note (D11.7)** goes on the /mainnet timeline when step 5
  ships. It says that providers whose builds do not route UDP, or whose IPv6
  fails, are offered for the traffic they can carry, and that SN25 scoring is
  unchanged.

## 11. Risks and open questions

### Risks

| Risk | Mitigation |
|---|---|
| **Dual-stack concentration.** Measuring "enough" on the IP axis against the window's target would draw every window from class A wherever A fills it: us has 108 dual-stack providers of 20,991 on the hard gate. | Resolved by D11.1. The IPv6 need takes one dual-stack client per quality window, as today's soft minimum does, and A and B then draw together (§6.3). |
| **Mobile memory.** Two more window clients against a 24 MiB target calibrated for 5. | The §9 memory gate. If it fails, mobile uses 1 (D11.5). |
| **Platform load.** 3-4 more clients per device: setups, pings, contracts and race duplicates. | measure on the proxy fleet first (step 4) |
| **Label lag without local downgrade (D7.5).** A provider that loses UDP keeps `dualproto` for a few hours, and its UDP flows stall until then. | The wide race hedges cold starts, and apps fall back from QUIC to TCP. |
| **IPv6 without B5** (option A of Q8 only). A dual-stack exit whose IPv6 breaks keeps `dualstack` until the evidence turns. | Dual-stack is about 1% of supply; applications fall back to IPv4. |
| **Export cost.** About 75% more union pages. | measured at step 2; capability keys can be dropped by rollback |

### Open questions

Q1-Q7 are decided (D10 and D11). One question remains.

- **Q8. Does D7.5 retire the shipped IPv6 local downgrade (B5)?**
  - *Options:* §7.9 sets out both.
    - **A, retire it:** placement reads only the server's labels. IPv6 flows
      to a dual-stack exit whose IPv6 broke continue until its label turns,
      typically within a few hours (§3.5).
    - **B, keep it as shipped:** B5's latch may lower a `dualstack` label for
      the life of a window. D7.5 covers new local state only.
  - *What changes:* option A removes the five B5 pieces listed in §7.9, its
    tests and the IPV6.md B5 text. Option B changes nothing in B5, and adds
    one test that the latch composes with the labels.
  - *Recommendation:* A. The owner's reason for D7.5 ("impossible to tell
    per-flow failures from provider failures") applies to B5's dial-strike
    inference too. A gives placement one source of truth, and the supply it
    touches is about 1%.
