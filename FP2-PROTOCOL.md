# Capability-aware FindProviders2 and connect placement

Design and implementation record. What building it changed is folded into
§1-§12.

| | |
|---|---|
| Status | Implemented and landed (§13), except the capability-probe retirement and the /mainnet note, which wait for their release steps (§10). All owner decisions recorded. |
| Date | 2026-11-12 |
| Server | design base cbb7d7306 for §1-§12; §13 cites the implementation at its merges (S1: 86553321d; S2: 4fdf026c0, which contains 3dcc7b138) |
| Connect | design base fc949b78 for §1-§12; §13 cites connect main 13a6c4f2 |
| SDK | 2f945bea, the limited-window branch, landed on sdk main as 1777be51; every sdk reference is at 2f945bea |
| Apps and localizations | android 130ed4526, apple e1c3cfac, windows 93af2d51, linux ff3a813, localizations 19b89a4, all on their main (§5.5) |

References are `path:line` at those commits. §1-§12 describe the design
against the code as it stood; §13 records how each workstream built it. Paths
without a repo prefix are in the server repo; `connect/`, `sdk/`, `android/`,
`apple/`, `windows/`, `linux/` and `localizations/` name the other repos.

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
| strict PQ | a request with `post_quantum_encryption` or `require_modern_provider` set (§12.4) |
| PQ-positive | a provider whose newest post-quantum outcome in the 8-hour window is positive (§12.4) |
| S1, S2, C, D | the implementation workstreams: server evidence and probe, server selection, connect, and sdk, apps and localizations (§13) |

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
  - Size: half the largest normal window's target, at least 1 (D8). On a
    low-memory device (iOS, Android, a hosted proxy) it is bounded at 1 if the
    memory gate fails (D11.5).
  - Rank mode: the performance profile's, `quality` in auto (D10).
- The provider status tells a downgraded provider why (D11.6).
- Connect's shipped local IPv6 downgrade (B5) is retired. Placement uses
  only the server's labels on both axes (D11.8).
- Strict post-quantum (PQ) requests match only providers the quality probe has
  seen seal a PQ session within the last 8 hours. Neither a provider's
  declaration nor the capability probe qualifies any more (D12, §12). While
  the ready marker is down, the probe's modern evidence of the last 7 days
  answers instead, never a declaration (D12.6, §12.8).

**Goals.**
1. Bring the providers that pass everything but UDP (about 20,700 on Main on
   2026-11-12) back into use for the traffic they can carry, without ever
   routing UDP to them.
2. Make IPv6 placement read the same probe-backed labels.
3. Keep every answer to an existing client as it is today: the same strict
   membership and the same JSON (§6.1).

**Non-goals.**
- **No local downgrade on either axis** (D7.5, D11.8). No capability state is
  derived from a client's own flows, and the shipped local IPv6 downgrade
  (IPV6.md B5) is retired (§7.9).
- **The hard gate stays the default.** Old clients, and new clients talking
  to old servers, get the hard gate.
- **No change to SN25.** Scoring, weights and payouts read no URL outcome and
  do not change (qualityprobe/H3.md, "SN25"). Providers that regain traffic
  earn more through traffic alone.
- **No change to the prober.** Its legs, verdicts, place rules or catalog do
  not change; the record line the monitor parses (monitor/tailer.go:1222-1250)
  is unchanged.
- **No new provider.yml knobs, and no off-by-default settings.**
- **No new window for post-quantum** (D12.3). Every window uses the current PQ
  setting.
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
| D7.1 | no selectors means today's hard gate exactly: strict, no overflow, byte-identical | accepted | §5.4, §6. Built as identical content and the exact legacy JSON: gob rules out identical bytes (§6.1). |
| D7.2 | overflow order: full → `ipv4+dualproto` → `dualstack+tcp` → `ipv4+tcp` | accepted | §6 |
| D7.3 | "not enough" means fewer matching providers in the location than the window's target count | "correct" | §6.3, refined by D11.1 and D11.2 |
| D7.4 | labels come only from negative probe evidence | "if no negative measurement assume dual" (for both axes; unmeasured `udp_probes: false` countries count as `dualproto`) | §3.2-3.3 |
| D7.5 | no local downgrade state on either axis | "it's impossible to tell per-flow failures from provider failures. also it's very hard to tell a failed udp flow" | §1 non-goals; applied to the shipped B5 by D11.8 (§7.9) |
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
| D11.8 (Q8) | D7.5 also retires connect's shipped IPv6 local downgrade (B5): placement uses only the server's labels on both axes | "a retire it accepted" | §1, §7.1-7.3, §7.9, §9, §10, §11 |
| D12.1 | the quality probe, not the capability probe, confirms post-quantum: every URL probe tunnel runs the PQ end-to-end session, and each run records a positive or negative PQ outcome | "I think the main change is using the quality probe to confirm that post quantum works, instead of the capability probe." | §12.2-12.3 |
| D12.2 | strict PQ requires a positive signal. "No negative measurement means capable" (D7.4) does not apply to PQ, and a declaration alone no longer qualifies. | "We should require positive signal for any provider that gets matched to a strict post quantum criteria." | §12.4 |
| D12.3 | no new window: every window, the limited one included, uses the current PQ setting, and a toggle regenerates them all. Overflow never relaxes PQ. | "we already added postquantum as a selection criteria to fp2. when postquantum is enabled/disabled, the entire multi windows are regenerated. We should make sure to the use current post quantum setting on all window selection. We do not need a new window." | §12.5 |
| D12.4 | 7918 (connect#220) is solved by strict selection with the positive signal; its fail-fast, cache and field-measurement options are not needed | "With the strong fp2 post quantum filter and quality probe positive signal, i think 7918 is solved by the multi client requiring a strong multi client selection when post quantum is enabled." | §12.7 |
| D12.5 | PQ outcomes come from a scheduled third of URL turns, not every turn: a turn is a PQ turn when its claim ordinal is a multiple of 3 and its provider has no PQ outcome in the last 4 hours | accepted 2026-11-12, in place of every turn (S1 step 2, merge 86553321d) | §12.2, §13.2 |
| D12.6 (2026-11-13) | no provider counts on its own claim, on either side of the ready marker: a network-only provider is modern or PQ only with the same probe evidence as a public one, and while the marker is down strict PQ reads the probe's modern evidence of the last 7 days instead of declarations. That evidence counts PQ only from the quality probe's positive outcome. | "1,2 sounds good, go ahead"; "They must have a positive quality probe handshake to count" | §12.4, §12.8 |

D11.8 applies D7.5 to the shipped IPv6 local downgrade (B5) too: it is
retired (§7.9). No owner question is open.

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
    if p.total < ProviderOnlineMinMeasuredUrlRuns (2) or no current evidence:
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
- **No current evidence** is `ComputeEgressIndex`'s: the window's newest run
  is older than the evidence age. Such a provider has no verdict either.
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
  - the two-axis model assumes IPv4, so they are never downgraded and have no
    classified copy. Their records stay in today's facet `6`, which selector
    mode does not read, so it never draws one;
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
    ADD COLUMN IF NOT EXISTS failed_later_legs smallint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pq_outcome smallint NOT NULL DEFAULT 0;
```

- **`pq_outcome`** is the run's post-quantum outcome: 0 unmeasured, 1
  positive, 2 negative (§12.3). It is added by the same statement and costs the
  same.
- **The bits** of `failed_later_legs`:
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
- **Built as migration 966** (§13.2), applied on Main 2026-11-13 00:14:53Z
  (D42 migrated 964 to 967).
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
COUNT(*) FILTER (WHERE failed_later_legs & 5 <> 0)                                         AS negative_udp,
COUNT(*) FILTER (WHERE failed_later_legs & 6 <> 0)                                         AS negative_ipv6
-- the first five again with "AND url_source = 0" for the general half;
-- negative_ipv6 and the post-quantum times (§12.3) cover the whole window only
```

- `ProviderEgressHealthCounts` (:643-659) gains these counts. Its `General`
  gains the first five.
- `negative_ipv6` (mask & 6) is the count the provider status tells for a
  dropped IPv6 (§5.5).
- The struct also gains the newest positive and negative post-quantum
  outcomes (§12.3).
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
  - The export also adds `PostQuantumPositive` (§12.4).
- **Labels served:**
  - `ip` is `dualstack` when `IpFamily()` is `dualstack` and bit 1 is clear,
    else `ipv4`, a v6-only provider included (§3.6);
  - `protocol` is `tcp` when bit 2 is set, else `dualproto`.
- **Two records per downgraded provider.** The export sets
  `CapabilityDowngrades`, and `PostQuantumPositive` (§12.4), on the
  provider's shared score, which keeps the strict fields for today's facets.
  Each downgraded provider also gets a classified copy. It is held in an
  unexported field of the shared score, so it is never encoded with today's
  records. It carries:
  - `PassesMinimums`, `Online`, `UrlProbeSuccessWeight` and `ScaledWeights`
    from the classified decision;
  - `Scores` and `Tiers` ranked again from its class's index. The Quality
    tier's base is the egress index, so the copy's Quality tier comes from
    its class's index. The Speed base is zero, so its Speed tier is
    unchanged. A copy whose counts give no current evidence keeps the strict
    tiers.

  The copy is written only to the capability facets (§4.4).

### 4.4 Export facets and keys

Today's facets `d`, `4` and `6` keep exactly today's strict membership and
content; their bytes cannot be pinned under gob (§6.1). Three new facets hold
the classified copies of downgraded providers:

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

FP2 labels a client-id spec from them (§6.4). As built:
- The export publishes them with the two post-quantum sets of §12.4. Each of
  the four sets is its own cluster slot, so each is replaced in its own
  transaction.
- A reader that finds a set without its ready member reads every provider as
  absent.

**Cost.**
- About 20,700 downgraded providers are written into the union pools of both
  rank modes. That is about 75% more union pages, by count of members (Online
  holds 26.7k).
- The native capability snapshot holds the class-D Speed natives and the few
  class-D Quality natives. It is a second native snapshot per non-forced
  target and rank mode, which lengthens the export.
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
| **Old client** (no `select`) | today | Legacy mode: today's answer, drawn from today's strict membership, with no new fields (§6.1). The hard gate, no overflow (D7.1). |
| **New client** (sends `select`) | See below | Selector mode, as designed. The limited window's requests carry the profile's rank mode: `quality` in auto (D10). |

**New client, old server.**
- The server ignores `select` and answers the hard gate, with no
  `capabilities`, no `overflow_step` and no `select` echo.
- The client sees no echo and treats the server as legacy:
  - each entry's labels come from `ip_family`: `dualstack` gives
    `dualstack`, a `v6-only` provider stays v6-only, and anything else gives
    `ipv4`;
  - the protocol is always `dualproto`, since every entry passed the hard
    UDP gate;
  - the limited window does not fill: it is disabled while the server is
    known not to echo, and the next echoed answer enables it;
  - the normal windows keep sending today's `v6-capable` shortfall request
    (IPV6.md B1-B2);
  - placement still checks compatibility with these labels.

**New client, new server, before the first new export.** This is the window
of a rolling deploy.
- The server answers selector mode from today's facets only: classes A and B,
  labelled from records whose downgrade bits are zero.
- Classes C and D are empty, so the limited window finds none and stays
  empty.
- A target whose capability facets exist but whose `:native_cap_v1` is
  missing gives C and D without natives (§13.4).

**Hosted devices.** The proxy service runs one build of the server and the
sdk, so its devices are always in a same-version cell.

**Whatever the server's version,** a new client has B5 retired, sends UDP
quality-first in auto, and answers UDP with no compatible exit with a
port unreachable. With legacy labels every exit is `dualproto`, so that reply
effectively never fires (§13.3).

**Post-quantum (D12).** `post_quantum_encryption` and `require_modern_provider`
already exist on every server, so no field is new. Only their meaning changes,
from declared or capability-probed to measured by the quality probe (§12.4):

| | Old server | New server, before `client_score_post_quantum_v1_ready` | New server, after it |
|---|---|---|---|
| Any client sending either field | today's meaning | the probe's modern evidence of the last 7 days, never a declaration (D12.6) | PQ-positive providers only |
| Any client sending neither | unchanged | unchanged | unchanged |

No client change is needed. A server from before D12.6 (2026-11-13) answered
the middle column with today's meaning: declarations for
`post_quantum_encryption`, and for `require_modern_provider` a network-only
provider's declaration.

### 5.5 Provider status (D11.6)

The provider status already reports URL checks as the ranking item
`url_checks` (model/provider_status_model.go:966-981), decided by the strict
rule (:777). The item keeps its value, count, total and `passes` as today.

**The fields.** A downgraded provider's item gains three fields. All are
omitempty, and all are also carried in the compact form.

| Field | Type | Value |
|---|---|---|
| `reason` | string | The server's English sentences, one per dropped capability, the `tcp` one first. `explanation` ends with the same text. |
| `reason_tcp_count` | int | The runs in the window with `failed_later_legs & 5`: an HTTP/3 leg judged the provider's. Set exactly when the `tcp` sentence is in `reason`. |
| `reason_ipv4_count` | int | The runs with `failed_later_legs & 6`: an IPv6 leg judged the provider's. Set exactly when the `ipv4` sentence is in `reason`. |

**The sentences:**

| Label | Sentence |
|---|---|
| `tcp` | "%d of these checks failed only over HTTP/3 (UDP), so this device is offered for TCP traffic only. Some app builds and some networks do not route UDP." |
| `ipv4` | "%d of these checks failed only over IPv6, so this device is offered for IPv4 traffic only." |

- **Counts.** The IPv4 count reads mask & 6, not §4.2's `negative_ip` (mask
  & 2). A dual-stack provider classed B by mask-4 runs alone would otherwise
  read "0 of these checks".
- **Labels:** the same classification as the export (§3.3), computed for the
  one provider.
- **sdk** (on main at dce4ab69):
  - `ProviderRankingNumber` gains `Reason`, `ReasonTcpCount`,
    `ReasonIpv4Count` and `ReasonLines` (sdk/api.go:3379-3390).
  - `ProviderStatusReasonLine{Label, Count, Text}`
    (sdk/provider_status_reason.go:28) has the labels `tcp`, `ipv4` and
    `text`.
  - Its `UnmarshalJSON` (:56) derives the lines: `tcp`, then `ipv4`, each
    when its count is at least 1. Otherwise it gives one `text` line with the
    English reason.
  - The C ABI exposes `urnet::ProviderRankingNumber::reason_lines`.
- **Localizations** (19b89a4): plural keys `provider_status_reason_line_tcp`
  and `provider_status_reason_line_ipv4` in all 28 locales
  (localizations/keys/provider_status_reason_line_tcp.yaml and
  `_ipv4.yaml`).
- **Apps.** Every app shows its own localized help line for this item, so all
  four needed code, and no old build shows the reason. `explanation` is only
  a debug or accessibility fallback.
  - **android** (130ed4526):
    android/app/app/src/main/java/com/bringyour/network/ui/stats/ProviderStatusPresentation.kt:43, 401, 414, 536
    and
    android/app/app/src/main/java/com/bringyour/network/ui/stats/ProviderDemandChart.kt:357.
  - **apple** (e1c3cfac):
    apple/app/network/Shared/ViewModels/ProviderStatusPresentation.swift:203, 232, 238,
    apple/app/network/Shared/ViewModels/ProviderStatusStore.swift:270, and
    apple/app/network/Main/Account/Earnings/ProviderDemandSection.swift:175.
  - **windows** (93af2d51):
    windows/app/src/App/ProviderStatusPresentation.h:260, 315, 409 and
    windows/app/src/App/WalletPage.cpp:510, 6105.
  - **linux** (ff3a813): linux/app/src/ProviderStatusPresentation.hpp:261,
    411, 527 and linux/app/src/EarningsPage.cpp:513, 570, 8229.
- **Servers.** Older servers omit the fields, so the reason shows only after
  the server deploy that adds them (§10 step 3).

## 6. Server selection algorithm

### 6.1 Legacy mode

`select` absent or null: `FindProviders2` runs today's code
(model/network_client_location_model.go:6335-7411).

**As built (§13.4),** that path changes in three places only:
- the branch to selector mode at entry;
- the post-quantum request metric;
- strict PQ once the ready marker is set, which D12 requires in legacy mode
  too (§5.4, §12.4).

Selector mode is a separate function that repeats the level body for each
pool. A fix to either must be checked against both (§11).

**What the tests pin.** Byte-identical output cannot be pinned:
- the score cache is gob-encoded, so any new exported `ClientScore` field
  changes the encoded type descriptor;
- the export writes its pages in Go's random map order.

The tests pin instead:
- the content of today's facets
  (`TestExportClientScoresTodaysFacetsIgnoreCapabilityCopies`);
- the exact JSON of a legacy answer
  (`TestFindProviders2LegacyAnswerEncodesWithoutNewFields`).

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
                         args.rank_mode, args.force_minimum, ipNeed)              // A, then C for the rest
            for p in need where sel.protocol == dualproto and labels(p).protocol == tcp:
                p.tier += 3 * BackfillTierOffset                // a C taken for the need is overflow
                p.overflow_step = 1
            discovered += need
        // 2. the requested protocol's classes, as one pool (D11.1)
        main := pool(protocolClasses(sel.protocol) ∩ keepFamilies(args.ip_family))
        discovered += fill(main, args.rank_mode, args.force_minimum,
                           count - len(discovered))             // never redraws a provider
        // 3. overflow: a dualproto request that is not enough (D7.3, D11.2)
        if sel.protocol == dualproto and sel.held_count + len(non-overflow answers) < sel.target_count:
            over := fill(pool({C, D} ∩ keepFamilies(args.ip_family)),
                         args.rank_mode, args.force_minimum, count - len(discovered))
            for p in over:
                p.tier += 3 * BackfillTierOffset                // 33 with the default offset
                p.overflow_step = 1
            discovered += over
        for p in discovered:
            p.capabilities = labels(p)                          // §4.3
        if args.strict or no level left or a pool read had an admitted provider:
            break                                               // today's rule, over every pool read
    return answer + discovered, echo sel
```

### 6.3 Classes per phase, family filter, "enough"

| Request (`ip`, `protocol`) | IPv6 need from | Main pool | Overflow pool |
|---|---|---|---|
| `dualstack`, `dualproto` (normal windows) | A, then C for what A leaves | A ∪ B | C ∪ D, tier +33 |
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
- **As built,** the eligible candidates are counted as the answers the need
  and main phases gave that are not overflow. Up to the target, that is the
  same number whenever the request's count is at least its target. Connect
  never sets `force_count`, so its requests' count is at least the server's
  minimum of 20.

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
  or `tcp`; absence gives `ipv4` or `dualproto`, as does a set no export has
  published.

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
  - from `capabilities` when the answer echoes `select`. An echoed entry
    without `capabilities` is labelled from `ip_family` with `dualproto`;
  - otherwise from `ip_family` with `dualproto` (§5.4).
  - **Unknown values fail closed:** an unknown protocol reads `tcp`, and an
    unknown or empty IP reads `ipv4`. An empty protocol reads `dualproto`.
- **`multiClientChannel`** gains `supportsFlow(ipVersion, ipProtocol)`. It
  reads only these labels (D6.2, D7.5, D11.8). A fixed destination supports every
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

Every site reads only the server's labels, on both axes. The B5 latch that
could lower an exit's family is retired (D11.8, §7.9), so no client-side
state changes a label between FP2 answers.

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
| Size setting | `MultiClientSettings.LimitedWindowSizeFraction`, default 0.5. A runtime setter changes it; zero, a negative value, NaN or Inf turns the window off. It lives in `MultiClientSettings`, not `ReliabilitySettings`, because the sdk mirror's round trip zeroes unknown fields. |
| Target | `max(1, floor(f × S))` for f > 0. S is the largest current target among the active normal windows, the `targetWindowSize` each window's resize pass computes (:14399-14425). In a fixed profile the inactive window's target is 0. f ≤ 0: no limited window. |
| Hard max | `max(1, floor(f × the largest normal hard max))`, or unbounded when an active normal window is unbounded. `LimitedWindowSizeMax` caps both target and hard max. |
| Desktop auto | normal targets 6 (quality) and 1 (speed; max 2): S = 6, limited 3 |
| iOS and hosted proxies (32 MiB memory target) | normal windows scale with the target (sdk/mobile_memory_policy.go:237-238, 292-299): quality 5, speed 1, so S = 5 and the limited window is 2 |
| Android (64 MiB) | quality 10, speed 2, so S = 10 and the limited window is 5 |
| Low-memory bound (D11.5) | `MultiClientSettings.LimitedWindowSizeMax` caps the limited window; 0 means no bound. The sdk feeds it from its unexported `DeviceLocalSettings.lowMemoryLimitedWindowSizeMax` (sdk/device_local.go:735, default 0 at :411), through `applyMobileLimitedWindowSettingsForPlatform` (sdk/mobile_memory_policy.go:590, called at sdk/device_local.go:5536). It applies on low-memory devices only: iOS, Android and hosted proxies. A fraction alone cannot give D11.5's 1 on Android. |
| Memory gate | Steady memory is measured on the iOS and Android nightlies with every window full, before the stable release. If either is over its target, the bound is set to 1 at sdk/device_local.go:411. Until then the gate counts as not failed: no bound. Desktop is unaffected. |
| Fixed profile of size N | `max(1, floor(N/2))` |
| Fixed destination | none: the fixed exit carries every flow |
| Fill and refill | its own `resize` loop (:13895) and `expand` (:14845), like a normal window. Each request carries `target_count` (its target), `held_count` (its `tcp` clients) and `dualstack_target_count` 0. There is no IPv6 need. |
| No `tcp` supply | the window stays empty and costs nothing |
| Readiness | No standing reserve and no watchdog. The window waits until every normal window has published its sizing, and its listing is held while its target is 0. |
| Old server (no echo) | the window is disabled while the server is known not to echo, and the next echoed answer enables it. Without a capability-aware generator it stays disabled. |
| Profile change | Handled exactly like the normal windows. An equal profile is a no-op (:3403-3408). Otherwise `SetPerformanceProfile` (:3394-3430) installs the new profile on every window in `self.windows`, the limited window included (:13381-13400: it also forgets sticky and recent exits). `shuffle` then cancels every client (:16231-16238). Those clients' flows go through `removeClient` (:5718): QUIC flows are rebound to compatible candidates, and TCP flows are torn down. The resize loop refills under the new rank mode, with the target recomputed from the new active normal windows. |
| Count | one per multi client |

**Normal windows.**
- Each request carries `select: {"ip": "dualstack", "protocol": "dualproto"}`
  (D6.1) with:
  - `target_count`: the window's pre-reserve demand target, seeded from
    config before the first pass;
  - `held_count`: its live, unwarned clients labelled `dualproto`;
  - `dualstack_target_count`: its IPv6 minimum (`WindowSizeMinIpv6Capable`:
    quality 1, speed 0; connect/ip_remote_multi_client.go:232).
    - It is floored at 1 for the auto quality window and for a fixed
      profile's active window: mobile omits the minimum, and fixed profiles
      carry 0, so they would otherwise lose today's dual-stack exit.
  - `held_dualstack_count`: its live, unwarned clients labelled `dualstack`
    (D11.1).
- In selector mode the main request carries the IPv6 need, so the separate
  `v6-capable` shortfall request (IPV6.md B1-B2) goes only to a server that
  did not echo `select`. An echoed answer with no `dualstack` provider while
  the shortfall is open marks the family starved.
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
  | IPv6 TCP or ICMP | today's B6 no-route (:8721-8733) and the interceptor's empty AAAA (`SetIpv6Unroutable`, sdk/device_local.go:5651), now decided from IP labels |
  | IPv4 TCP or ICMP | every client is compatible, so this happens only with no client at all: today's wait and retry |

- `UdpUnroutable()` mirrors `Ipv6Unroutable()`
  (connect/ip_remote_multi_client_family.go:605-658).
- The reply judges only the flow's own windows. It is sent when the flow's
  selected normal windows have formed and neither they nor the limited window
  holds a client that can carry the flow. A fixed profile's inactive window
  never keeps UDP routable.

### 7.8 sdk and apps

The sdk's limited-window work, branch `feat/fp2-limited-window-1112`
(2f945bea), landed on sdk main as 1777be51, right after connect's change
(§13).

- **Window status** (sdk/device.go:435-441):
  - `UdpAvailable` comes from the merged window event;
  - `LimitedWindowSize` is set at sdk/device_local.go:5670 and 5812;
  - `GetWindowStatus` no longer calls the client while holding `stateLock`.
- **Sizes:**
  - The fraction stays connect's 0.5; the sdk never copies it.
  - On low-memory devices the sdk sets `LimitedWindowSizeMax` (§7.4, D11.5).
- **Developer controls:**
  - `GetLimitedWindowSizeFraction`, `SetLimitedWindowSizeFraction` and
    `ResetLimitedWindowSizeFraction` (sdk/reliability_controls.go:610-645);
  - the override is built into each new window (sdk/device_local.go:5545);
  - DeviceLocal only: DeviceRemote (rpc) does not have them yet (§13).
- **Exits:** `CapabilityIp`, `CapabilityProtocol` and `OverflowStep`, also in
  `ExitRpc`. A limited exit's `WindowType` is `limited`
  (`ExitWindowTypeLimited`).
- **Diagnostics:** memory usage reports `limited_clients`, and transfer
  diagnostics report `limited_client_count`.
- **Provider status (D11.6):** all four apps changed (§5.5).
- **No other app API or UI change.** Auto stays the default
  (sdk/device_local.go:9129-9130). Connected stays count-based on the normal
  windows (sdk/connect_view_controller.go:1215).

### 7.9 Retiring the local IPv6 downgrade (B5) (D11.8)

Connect ships one piece of local capability state: the IPv6 local downgrade
of IPV6.md B5. An exit whose IPv6 dials starve while its IPv4 stays healthy is
latched to v4-only for the life of its window. D11.8 retires it, so placement
uses only the server's labels on both axes.

**Removed:**
- `maybeDowngradeIpv6` (connect/ip_remote_multi_client_family.go:68-85);
- its trigger on the dial-failure path
  (connect/ip_remote_multi_client.go:10406-10407);
- `noteIpFamilyDowngrade` (family.go:661-676);
- the `ipFamilyDowngraded` latch
  (connect/ip_remote_multi_client.go:17253-17259);
- `ipv6DialStarvedWithLock` (family.go:97-138);
- `dialStrikeVersion` and `noteClientIpFamily`.

**Kept:**
- the version-split dial-strike record, which ordinary starvation warnings
  read;
- the monitor's `SetProviderIpFamily`, which now has no caller.

**Changed:**
- `IpFamily()` (family.go:40-51) returns the server's IP label.
- IPV6.md B5 is marked superseded by this document.

**Effect:**
- A dual-stack exit whose IPv6 breaks keeps receiving IPv6 flows until its
  label turns (§3.5), typically within a few hours. Applications fall back to
  IPv4 meanwhile.
- Only dual-stack providers are concerned: about 1% of supply.

### 7.10 Costs per device

| | Desktop | iOS and hosted proxies (32 MiB) | Android (64 MiB) |
|---|---|---|---|
| Window clients today | quality 6, speed 1 (max 2): 7-8 | quality 5, speed 1: 6 | quality 10, speed 2: 12 |
| Limited window | 3 | 2, or 1 with the bound (D11.5) | 5, or 1 with the bound |
| Window clients after | 10-11 (+38-43%) | 8 (+33%), or 7 | 17 (+42%), or 13 |
| Contracts | +3 per direction while the limited clients carry data. The wide race sends each cold-start TCP flow's first packet to them, so they open contracts early. | +2 (or +1) | +5 (or +1) |
| Control plane | +3 client setups (auth and control registration) and continuous pings (`CPingRestTimeout` 10 s, `CPingMaxByteCountPerSecond` 32 KiB/s; connect/ip_remote_multi_client.go:255, 264) | +2 (or +1) | +5 (or +1) |
| Memory | each client holds its own transport graph and buffers | the window sizes scale from the 24 MiB calibration (sdk/mobile_memory_policy.go:14, 292-299). Per-client buffers include a resend queue of 64-512 KiB and an unreliable flight of 128 KiB (:224-236). | the same, at 64 MiB |

Race duplication: each cold-start TCP flow's first packet goes to up to 3 more
exits on desktop, 2 on iOS and hosted proxies and 5 on Android, or 1 with the
bound.

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

**Post-quantum (D12):**

| Metric | Labels | Source | Dashboard |
|---|---|---|---|
| `urnetwork_url_probe_e2e_outcomes_total` | `pq` (`positive`, `negative`, `unmeasured`) | taskworker prober | urnetwork / provider quality probes |
| `urnetwork_egress_url_pq_outcomes_total` | `pq` | ingest | urnetwork / provider quality probes |
| `urnetwork_stats_provider_post_quantum` | `bucket`, `signal` (`positive`, `negative`, `none`) | stats collector | urnetwork / providers |
| `urnetwork_findproviders2_post_quantum_requests_total` | `field` (`post_quantum_encryption`, `require_modern_provider`) | FP2 | urnetwork / providers |

The capability probe's metrics (capprobe/metrics.go) retire with it. Its
dashboard, "capability-probes", is replaced by the URL-probe PQ panels, and
its ./grafana tests move to them.

`urnetwork_stats_provider_egress_index` and `urnetwork_stats_provider_excluded`
keep the strict decision, unchanged (controller/stats_collector.go:80-86).

**As built (§13.4):**
- `urnetwork_stats_provider_egress_capability` and
  `urnetwork_stats_provider_post_quantum` are read from the egress counts
  snapshot, which moves to schema 8 with the wire keys `k` (capability) and
  `q` (post-quantum).
- Both gauges bucket by the classified decision. A provider that is not downgraded
  counts by its strict decision, which equals it.
- Every label set of the FP2 counters is registered at startup, so every
  series exists at zero.
- The providers dashboard's row "capability-aware selection (FP2-PROTOCOL.md)"
  (panel 935) holds panels 936-944:
  - supply by capability for each bucket;
  - the post-quantum signal;
  - strict PQ requests;
  - selector-mode requests and answers;
  - `tcp` overflow;
  - the IPv6 need.

  A ./grafana test covers the row.

### 8.2 Hosted devices

These are connect process counters that the proxy exports, like
proxy/window_client_metrics.go:21-56. Connect counts them (§13.3), and the
proxy's exporter publishes them (server merge 4fdf026c0, §13.4).

| Metric | Labels |
|---|---|
| `urnetwork_proxy_window_clients` | `window` (quality, speed, limited; auto for any other window type), `ip`, `protocol` |
| `urnetwork_proxy_window_flows_total` | `window` of the winning client, `ip_version`, `protocol` |
| `urnetwork_proxy_window_no_compatible_total` | `ip_version`, `protocol` |

The existing `window` label of the proxy window metrics gains `limited`
(connect/ip_remote_multi_client_window_client_stats.go:22-45). Dashboard:
urnetwork / proxy, panels 80-82, with a ./grafana test. Apps report the same
counts in their window status and diagnostics; they have no Prometheus.

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
    - the limited windows: 3 per desktop device, 2 per iOS device or hosted
      proxy, and 5 per Android device (1 on a low-memory device if the memory
      gate fails);
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
- The five counts, overall and general; `negative_ipv6` and the
  post-quantum times over the whole window.
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
- Today's `d`, `4` and `6` keys and the `:native_v1` snapshots hold the same
  content with and without the change. Bytes cannot be compared under gob
  (§6.1).
- `:native_cap_v1` has its own manifest; today's reader accepts the old
  snapshot unchanged.
- The modern document's new entries are ignored by today's reader.
- The named-provider sets are replaced each export.

**FP2 (DB and Redis):**
- **Legacy mode:** today's answers (every rank mode, `force_minimum`,
  `ip_family`, `strict` with fallback), and the exact legacy JSON, with no new
  field.
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
  - the location fallback climbs only when every pool read was published
    without an admitted provider;
  - a cache from before the export yields A and B only.

**Provider status (D11.6):**
- `reason` and the explanation sentence for `tcp`, for `ipv4`, for both and
  for neither, with the counts;
- `url_checks` value, count, total and `passes` are unchanged;
- the sdk derives the reason lines from the counts (§5.5).

**Compatibility matrix, one test per cell (§5.4):**
- an old request on the new server gets today's answer, with no new field;
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
- **B5 retirement (D11.8):**
  - remove B5's tests: `TestMaybeDowngradeIpv6` and
    `TestClientDialFailureDowngradesAndRepublishes`
    (connect/ip_remote_multi_client_family_test.go:648, 770);
    `TestDialFailureDowngradesStarvedIpv6AndFillsShortfall` and
    `TestDialFailureNeverDowngradesIpv6OnlyExit`
    (connect/ip_remote_multi_client_rerace_test.go:2179, 2261);
  - rewrite the two tests that set the latch directly to set the server's IP
    label instead: `TestUnboundIpv6FlowIsNotAdoptedByAnExitDowngradedToIpv4`
    (connect/ip_remote_multi_client_unbound_flow_test.go:1028-1034) and the
    late-owner case (connect/ip_remote_multi_client_syn_retry_publication_test.go:586);
  - add tests proving placement no longer downgrades:
    - IPv6 dial strikes across distinct destinations while IPv4 stays healthy
      (B5's old trigger) leave the exit's labels, `IpFamily()` and its IPv6
      placement unchanged, publish no `family_downgrade` event and open no
      IPv6 shortfall;
    - UDP flows that get no reply leave the protocol label unchanged;
    - a label changes only when a fresh FP2 answer re-labels the entry.

**sdk:**
- the window status fields;
- the limited window's size: 2 on iOS and hosted proxies, 5 on Android, and 1
  with the low-memory bound set (D11.5);
- the window status fields, the exit fields and the diagnostics' limited
  counts (§7.8);
- the developer controls set, read and reset the fraction for new windows;
- a steady-memory measurement on the iOS and Android nightlies with every
  window full, against each device's memory target (32 MiB on iOS, 64 MiB on
  Android), before the stable release:
  - if either is over its target, the bound is set to 1 (D11.5);
  - this gates the stable release.

**Integration:**
- A local provider with UDP blocked is labelled `tcp`. It lands in the
  limited window, wins TCP/IPv4 races, and never receives QUIC.
- A dual-stack local provider with IPv6 blocked is labelled `ipv4` and gets
  no IPv6 flow. Use the local provider harness of `TestH3LegThroughALocalProvider`
  and `TestIpv6LegsThroughALocalDualStackProvider`.

**Post-quantum (D12):**
- **Prober:**
  - a local PQ provider gives `positive`;
  - a provider that negotiates a classical group gives `negative`;
  - a provider without the responder gives `unmeasured` within a short turn,
    and `negative` after a whole establishment window;
  - each one's URL outcome is identical with PQ on and off, so PQ never fails
    a URL run;
  - the seal wait is bounded and counted in the run budget.
- **Ingest:**
  - a prober cannot claim `positive` without a post-quantum key exchange and a
    passed IPv4 HTTPS leg;
  - `pq_outcome` is stored, and rechecks and probation runs store 0;
  - modern evidence is recorded.
- **Aggregate:** latest wins between positive and negative; no outcome means
  not PQ-positive.
- **FP2:**
  - both fields keep only PQ-positive providers in every draw, named
    providers included;
  - a declared but unmeasured provider is left out, on both sides of the
    ready marker (D12.6);
  - a network-only provider is left out unless it has the same evidence as a
    public one;
  - selector mode's phases and overflow never add a non-positive provider;
  - before the ready marker the probe's modern evidence of the last 7 days
    answers, and after it the measured signal;
  - a negotiated PQ session that never sealed is not PQ evidence (D12.6).
- **connect:**
  - a PQ toggle regenerates every window, the limited one included;
  - every request, the limited window's included, carries
    `require_modern_provider`;
  - every window client, limited ones included, runs
    `EncryptionModeRequired`.

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
| 4 | proxy service with the new connect and sdk (hosted devices), B5's retirement included (D11.8) | the app release, as the first fleet measurement of §8.2 | previous proxy build |
| 5 | apps through the nightly, B5's retirement included (D11.8): the memory gate decides the mobile limited size (D11.5); the Apple status row shows the reason (D11.6); a /mainnet note goes out with it (D11.7) | none | the previous release |

- **As built (§13.2, §13.4), in deploy order.** D42, rolling now, is server
  a151cee8d. It carries S1 step 1 only. S1 step 2 (86553321d) and S2
  (3dcc7b138, 4fdf026c0) ship together in the next server release. The step
  numbers are the table's.
  1. `db migrate` to 966 or later: done on Main on 2026-11-13.
  2. **taskworker.**
     - S1: PQ turns, and the ingest of the mask and the PQ outcome.
     - S2: the classification, the `i`, `t` and `x` facets,
       `:native_cap_v1`, the four sets, PQ stamping and the marker call.
     - Watch the `st_client_key_history` insert rate (about 27 a second for
       the first hour, then about 7) and PQ turn time (up to 3 s more).
     - Watch Redis memory for `cs_` keys and export duration (a second native
       snapshot per non-forced target and mode).
     - Watch the new metrics, and the gauges filling over 8 hours.
  3. **api, alt and the proxy service's server side:** selector mode, strict
     PQ once the marker is set, and the status reason. Remote probers can
     update in any order.
  4. **The proxy build with connect 13a6c4f2:** the hosted limited window and
     placement, and the proxy metrics.
  5. **Apps** through the nightly, as in the table.

  Beside the steps:
  - **Grafana**, after the server steps: the providers and proxy dashboards.
  - **Owner config release**, one day after step 3: `capability_probe.yml`
    `enabled: false`. It is the only config change, and no vault change is
    needed.
  - **Next release:** merge the retirement branch
    (`feat/fp2-s1-capprobe-retire-1112`). It must follow the config release,
    or monitor §2.19g pages.

  **Rollback:** the previous taskworker. PQ turns stop, the marker expires,
  and strict PQ returns to the probe's modern evidence of the last 7 days
  (D12.6, §12.8), which ages out without new PQ turns. The capability keys
  and sets expire with the export TTL.
- **Post-quantum (D12).** It rides the same steps:
  - **Step 1:** the migration adds `pq_outcome`.
  - **Step 2:** the taskworker's URL prober opens the PQ session on PQ turns
    (D12.5), and the ingest stores `pq_outcome` and the modern evidence. The
    export stamps `PostQuantumPositive` and publishes the sets. 8 hours after
    the PQ-measuring prober started, a full evidence window, the export marks
    `client_score_post_quantum_v1_ready`.
  - **Step 3:** api, alt and the proxy use the measured signal once the marker
    is set, and the probe's modern evidence of the last 7 days until then,
    never a declaration (D12.6; before 2026-11-13, today's sources).
  - **The capability probe** is switched off (`capability_probe.yml`
    `enabled: false`) one day after step 3, and its code is deleted in the
    next release.
  - **Connect and sdk** need no change for PQ (§12.5).
- **Connect (§13.3):** connect has no service deploy of its own.
  - It ships with step 4, in the proxy build with S2's exporter, and with
    step 5, in the apps through the nightly (sdk 1777be51 with connect).
  - It is safe before any server step: an old server never echoes, so labels
    come from `ip_family`, every exit is `dualproto`, and the limited window
    stays empty.
- **Apps, sdk and localizations (§13):**
  1. The provider status reason shows only after the server deploy that adds
     its fields (step 3); older servers omit them.
  2. The sdk's limited window lands right after connect's change. Hosted
     proxies take it first (step 4), then the apps (step 5).
  3. Before the stable release, steady memory is measured on the iOS and
     Android nightlies with every window full. If either is over its target,
     the low-memory bound is set to 1.
  4. The /mainnet note (sn `docs/mainnet-fp2-capabilities-1112` 1c94ca1c, web
     42b091f6) stays unmerged by design. Its date is a placeholder, and it
     publishes with the stable app release.
- **Measure between steps 2 and 3.** Watch Redis memory for `cs_` keys,
  export duration, and the new gauges filling over 8 hours.
- **The client settings** `LimitedWindowSizeFraction` (0.5; 0 turns the
  window off) and the selectors are compiled defaults. They are exposed in the
  developer menu for testing; a production change is a release.
- **IPV6.md B5** is marked superseded in the connect change that retires it
  (D11.8).
- **The /mainnet note (D11.7)** goes on the /mainnet timeline when step 5
  ships. It says that providers whose builds do not route UDP, or whose IPv6
  fails, are offered for the traffic they can carry, and that SN25 scoring is
  unchanged.

## 11. Risks and open questions

### Risks

| Risk | Mitigation |
|---|---|
| **Dual-stack concentration.** Measuring "enough" on the IP axis against the window's target would draw every window from class A wherever A fills it: us has 108 dual-stack providers of 20,991 on the hard gate. | Resolved by D11.1. The IPv6 need takes one dual-stack client per quality window, as today's soft minimum does, and A and B then draw together (§6.3). |
| **Mobile memory.** Two more window clients on iOS and hosted proxies (32 MiB target), and five more on Android (64 MiB). | The memory gate on the nightlies before the stable release. If it fails, the low-memory bound is 1 (D11.5). |
| **Platform load.** 3-4 more clients per device: setups, pings, contracts and race duplicates. | measure on the proxy fleet first (step 4) |
| **Label lag without local downgrade (D7.5).** A provider that loses UDP keeps `dualproto` for a few hours, and its UDP flows stall until then. | The wide race hedges cold starts, and apps fall back from QUIC to TCP. |
| **IPv6 without B5 (D11.8).** A dual-stack exit whose IPv6 breaks keeps `dualstack` until the evidence turns, typically a few hours (§3.5). | Only dual-stack providers are concerned, about 1% of supply. Applications fall back to IPv4, and B6's no-route still answers when no `dualstack` client exists. |
| **Export cost.** About 75% more union pages, and a second native snapshot per non-forced target and mode. | Measured at step 2: Redis memory for `cs_` keys and export duration. Capability keys can be dropped by rollback. |
| **Two selection bodies.** Selector mode repeats the legacy level body for each pool in a separate function (§6.1). A fix to one can miss the other. | Check every selection fix against both. The legacy JSON and today's-facet tests pin the legacy side. |
| **Network-only providers under strict PQ** (D12.2, D12.6). The URL probe cannot reach a provider that holds only a Network key, so it never has a positive signal, and a strict-PQ request no longer matches it, on either side of the ready marker. Only evidence left from a former Public provide, while fresh, still counts. | This follows D12.2 ("any provider") and D12.6. Such providers stay available to their network's requests without PQ. |
| **Strict supply while the marker is down** (D12.6). Strict requests then read only the probe's modern evidence of the last 7 days. A probe outage longer than that empties them. | The 7-day window outlasts any probe gap the marker covers. Strict supply empties only when no provider has fresh positive evidence, which is the rule (§12.8). |
| **A provider that loses PQ** stays PQ-positive until its next run reads negative, typically within a turn (about 24 minutes). | Window clients stay fail-closed and verify every session (§12.5), so nothing is sent in the clear. |
| **Prober cost** (§12.2). One key registration, one PQ handshake and up to a 3 s seal wait per PQ turn; about 3 GB a day of `st_client_key_history` at about 7 turns a second. | The schedule (D12.5) keeps it to a third of the turns of a provider without a recent outcome. The capability probe's own tunnels go away. Watch the insert rate and turn time at taskworker deploy. |
| **A provider that seals but cannot open the probe client's frames** fails that PQ turn's URL legs. | At most one turn in three, and only without an outcome in 4 hours (§12.2). |

### Open questions

None. Q1-Q8 are decided (D10, D11.1-D11.8), and post-quantum selection is
decided by D12.1-D12.5 (§12).

## 12. Post-quantum selection (D12)

### 12.1 Today

**Two request fields ask for post-quantum (PQ) providers:**

| Field | What it means today | Who sends it |
|---|---|---|
| `post_quantum_encryption` | The provider's latest provide declared PQ sessions. It is read per request (`readProvidePostQuantumEncryption`, model/provide_capability_model.go:38) and applied in every draw (model/network_client_location_model.go:5100-5105; candidates at :6653, named providers at :7364). | connect builds from before FP2-MODERN |
| `require_modern_provider` | `ClientScore.Modern`: the capability probe measured end-to-end PQ (X25519MLKEM768) and resizable windows within 7 days (model/egress_modern_provider.go:1-31, :348). A Network-key-only provider counts by its declaration (model/network_client_location_model.go:5790). Applied by the request filter (model/network_client_selection_prefilter.go:20, 36) and for named providers (:6732-6734, :7351-7352). | connect today, whenever the profile asks for PQ (connect/ip_remote_multi_client_api.go:885, 926) |

The table is as of 2026-11-12. D12.6 ended both declaration paths on
2026-11-13 (§12.8).

- **The declaration** comes from the provider's provide frame
  (connect/transfer_contract_manager.go:1512-1567). It sets
  `PostQuantumEncryption` when the provider's session manager serves PQ
  sessions.
- **The capability probe** (capprobe/README.md) measures each Online provider
  in a short tunnel of its own. That tunnel uses
  `EncryptionModeOpportunistic`, a `PeerCapabilityRecorder` and
  `ClientKeyRegistrationRequired`
  (qualityprobe/providertunnel/capability_observation.go:23, 73). The URL
  probe's tunnels keep end-to-end encryption off
  (model/egress_modern_provider.go:11-13).
- **Not strict filters:** the provider.yml modern gate is off, and
  modern-first priority only orders bands (FP2-MODERN.md).

**Connect.**
- A PQ profile runs every window client fail-closed, with
  `EncryptionModeRequired` (connect/ip_remote_multi_client.go:17306-17318).
- `syncGeneratorPostQuantumEncryption` (:3440-3447) pushes the PQ setting
  into the shared API generator before any window discovers: at construction
  (:3008) and on each profile change (:3425). It lands in
  `ApiMultiClientGenerator.SetPostQuantumEncryption`
  (connect/ip_remote_multi_client_api.go:748-749).
- Every FP2 request the generator makes reads that flag:
  - discovery through `nextDestinationsContext` (:807, arguments at
    :878-886);
  - named redials through `NextDestinationsForClientId` (:908, :926).
- A profile whose PQ setting differs is not equal (`performanceProfilesEqual`,
  connect/ip_remote_multi_client.go:3365-3369). So `SetPerformanceProfile`
  retires and regenerates every window (:3394-3430).
- The limited window (§7.4) is built through the same generator and the same
  client-settings path, so it inherits both.

**7918 (connect#220).** PQ mobile connects stalled for 60 s and more.
Discovery offered providers that could not seal a PQ session, and fail-closed
evaluation waited out each dead candidate before trying the next.

### 12.2 The probe: PQ on URL turns (D12.1, D12.5)

**Which turns.** A URL turn is a PQ turn when its claim ordinal is a multiple
of 3 and its provider has no PQ outcome in the last 4 hours (D12.5).
- PQ on every turn would mean about 68 key registrations a second and about
  29 GB a day of `st_client_key_history`.
- The schedule gives about 7 a second and about 3 GB a day, and still about
  two outcomes per provider per 8 hours.
- The claim marks the turn, and the mark travels to the prober with the due
  provider (§13.2).

**The tunnel of a PQ turn.**
- It applies the capability observation the capability probe used:
  `EncryptionModeOpportunistic`, the run's own `PeerCapabilityRecorder`, and
  `ClientKeyRegistrationRequired`. It does not include the capability probe's
  redial pacing, which URL tunnels never had.
- The turn holds its first sends up to 3 s for the session to seal. That is
  connect's `OpportunisticEstablishHold`; URL tunnels carry data only.
- Opportunistic means a provider without the end-to-end responder keeps
  carrying the turn in plaintext. So the URL outcome, every bucket and the
  classification of §3 do not depend on the session, with one residual case:
  a provider that seals but cannot open the probe client's frames fails that
  turn's URL legs. That happens on at most one turn in three, and only for a
  provider with no outcome in 4 hours.

**Each PQ turn records one outcome:**

| Outcome | When |
|---|---|
| positive | the session negotiated a post-quantum group (X25519MLKEM768), was sealed when the IPv4 HTTPS leg wrote its first request (`sealed_before_request`), and that leg passed |
| negative | the session negotiated a classical group; or a post-quantum session was still unsealed 10 s into the run (a grace period); or a ClientHello went unanswered for a whole establishment window (60 s) while the provider acknowledged data |
| unmeasured | anything else: a handshake still pending, a replaced window client or a re-created tunnel, a run that failed or was not measured. Short turns mostly read unmeasured. |

The same session also shows whether the provider's acks advertise a receive
window, as the capability probe read it (capprobe/reading.go:51-60).

The URL observer binds a positive to the live ordinary initiator session's
client id, epoch id and key-exchange group before the first request writes
(`GotConn`), after those writes (`WroteRequest`), and at completion. Later
retry or redirect callbacks cannot replace that first observation. A rekey,
demotion or closed session cannot borrow the recorder's sticky seal or the
group from another epoch; a classical request remains classical evidence
even if a later epoch negotiates PQ.

**Cost per PQ turn:**
- one key-registration round trip before admission;
- one TLS 1.3 handshake with X25519MLKEM768 through the provider, carrying
  ML-KEM-768 flights of about 5 to 9 KB;
- up to 3 s of waiting for a seal that a non-PQ provider never gives. The run
  budget counts it once per generation.

The capability probe's own tunnels, prober shard and passes go away (§12.4).

### 12.3 Evidence and storage

- **Evidence:** `url_probe_evidence` gains `e2e`, omitted when the run
  observed nothing:

  ```json
  {"pq": "positive|negative|unmeasured", "key_exchange": "...", "sealed_before_request": true, "window": "resizable|fixed|not_tested"}
  ```

  The ingest validates it as it does the other legs. A prober cannot claim
  `positive` without a post-quantum key exchange and a passed IPv4 HTTPS leg.
- **Column:** the §4.1 migration adds `pq_outcome` beside `failed_later_legs`,
  so there is no second migration. The store sets it from the validated
  evidence: 1 positive, 2 negative, 0 otherwise. Rechecks and probation runs
  store 0.
- **Aggregate** over the 8-hour window (§4.2):

  ```sql
  MAX(measured_at) FILTER (WHERE pq_outcome = 1) AS pq_positive_at,
  MAX(measured_at) FILTER (WHERE pq_outcome = 2) AS pq_negative_at
  ```

- **Modern evidence:** the URL-probe ingest records each conclusive
  observation in `provider_modern_evidence` (`RecordProviderModernEvidence`,
  model/egress_modern_provider.go:222), as the capability probe did. The
  window reading keeps its table and its census gauges.
  - `post_quantum` means a post-quantum group was negotiated.
  - `max_receive_window_byte_count` is stored as 0: the URL probe reads
    whether the window is advertised, not its size.
- **The stream:** each stored outcome of a PQ turn also notes the PQ stream
  (about 7 a second, two Redis operations each). The ready marker reads it
  (§12.4).

### 12.4 Strict match (D12.2)

- **PQ-positive:** a provider whose newest PQ outcome in the 8-hour window is
  positive. `pq_positive_at` is set and is later than any `pq_negative_at`.
- **Positive proof is required.** D7.4's "no negative measurement means dual"
  does not apply to PQ. A provider without a positive outcome in the window is
  not PQ-positive, and its declaration no longer qualifies.
- **`post_quantum_encryption: true`** keeps only PQ-positive providers in
  every draw, named providers included.
- **`require_modern_provider: true`** keeps only modern providers: PQ-positive,
  with the newest modern evidence of the last 7 days advertising resizable
  windows. This is FP2-MODERN.md's "modern", now sourced from the URL probe.
  Connect keeps sending this field (§12.1).
  - The newest modern evidence is the newest `provider_modern_evidence` row,
    whichever probe wrote it. Until the capability probe is switched off, its
    rows count too.
  - The request filter also requires `PostQuantumPositive` for this field.
    That guards the one export cycle whose `Modern` was computed before the
    marker was set.
- **How FP2 reads it:**
  - the export stamps `ClientScore.PostQuantumPositive`. It computes
    `ClientScore.Modern` from it once the marker is set when the export
    starts, and today's way before that
    (model/network_client_score_rank.go:218);
  - the request filter (model/network_client_selection_prefilter.go:20, 36)
    reads those fields;
  - for named providers, the export publishes the sets
    `{provider_post_quantum_positive}` and `{provider_modern}`, replaced each
    export like `{provider_hard_exclusions}`. They replace
    `readProvidePostQuantumEncryption` (:6653, :7364) and
    `readProviderModernClientIds` (:6732-6734, :7351-7352);
  - every export writes the flag and the sets, and readers use them only
    after the marker.

  The exported positive also carries its own deadline: the positive's
  measurement plus eight hours, capped by thirty minutes past the health
  snapshot the exporter read (`PostQuantumMaxStaleness`). Modern proof is
  additionally capped by the latest window evidence's seven-day lifetime.
  Every strict draw compares those deadlines at one request instant; the
  exact deadline is expired. Fresh ordinary URL results do not renew a PQ
  proof. Named providers use batched deadline hashes at each existing set's
  same-slot `:proof_v1` companion key, published atomically with that set.
  The set types remain compatible with older writers and readers; after the
  ready marker, a missing deadline from an older snapshot fails closed until
  a current exporter publishes proof. No per-provider database read is added.
- **The ready marker:** `client_score_post_quantum_v1_ready` holds the PQ
  stream's start.
  - After each complete export, the export sets it while the stream has run
    for 8 hours or more and its last outcome is under 30 minutes old, and
    deletes it otherwise.
  - The export reads it before it writes any score. FindProviders2 reads it
    through a per-process cache no older than the request settings' max age.
  - Until it is set, and whenever a probe gap clears it, strict PQ answers
    from the probe's modern evidence of the last 7 days, in legacy and
    selector mode alike, never from a declaration (D12.6, §12.8). Before
    2026-11-13 it answered from today's sources (§10).
- **Network-only providers:** a provider that holds only a Network key cannot
  be reached by the URL probe, so it never has a positive signal. Under D12.2
  ("any provider") a strict-PQ request does not match it. Its
  declaration-based modern status ended on 2026-11-13 on both sides of the
  marker (D12.6): it is modern or PQ only with the same evidence as a public
  provider.
- **The capability probe is retired.** Both of its checks, PQ and resizable
  windows, move to the URL probe.
  - `capability_probe.yml` is set to `enabled: false` once the URL probe has
    fed `provider_modern_evidence` for a day.
  - The `capprobe` package and `work.CapabilityProbe`
    (taskworker/work/capability_probe_work.go:178) are deleted in the next
    release.
  - The modern gate and modern-first priority keep their mechanisms, fed by
    URL-probe evidence. The provider.yml gate stays off.

### 12.5 Windows (D12.3)

- **No new window.** Every window, the limited one included, asks with the
  current PQ setting through the shared generator (§12.1). A PQ toggle
  regenerates every window.
- The new capability request (`NextDestinationsWithCapabilities`, §7.4)
  carries `require_modern_provider` from the same flag.
- **Under strict PQ, overflow never relaxes PQ.** The IPv6 need, the main pool
  and the `tcp` overflow (§6.3) all draw from PQ-positive providers only. So
  the limited window draws PQ-positive `tcp` providers.
- **Placement is unchanged:** window clients still run
  `EncryptionModeRequired` and verify every session before sealing traffic.

### 12.6 Impact

**Today's proxy** is the modern-gate shadow count,
`urnetwork_stats_provider_modern_gate_shadow`:

| Bucket | Modern | Not post-quantum |
|---|---|---|
| Quality | 84 | 778 |
| Speed | 26,319 | 1,651 |
| Online | 26,504 | 1,733 |

**Measured on Main** (read-only, 2026-11-12): the classes of §3 over the
8-hour window ending about 16:40Z, against capability-probe evidence from the
last 7 days.

| Class | Providers | Post-quantum | Not post-quantum | No evidence |
|---|---|---|---|---|
| A | 261 | 258 | 2 | 1 |
| B | 27,865 | 25,907 | 1,785 | 173 |
| C | 15 | 15 | 0 | 0 |
| D | 20,487 | 10,928 | 3,845 | 5,714 |

**What a strict-PQ user can draw under positive-signal matching:**
- **Legacy mode:** about 26,200 providers at the ratio level (A and B
  post-quantum). That is today's 26,319 modern Speed providers.
- **Selector mode:** about 37,100 (adding C and D post-quantum), or roughly
  35,600 after the gates.
  - It rises further as the URL probe measures the 5,714 class-D providers the
    capability probe never reached: that probe measures Online providers only,
    and the hard gate keeps most of class D out of Online.
- **The July build** (2026.7.2+N, about 12,200 providers) predates PQ:
  providers answer X25519MLKEM768 since v2026.7.21
  (model/egress_modern_provider.go:4-6). As the 11-11 updater fix and the next
  nightly move them, they turn PQ-positive. They also carry UDP, so they move
  from class D to class B.

### 12.7 7918 (D12.4)

- 7918 is solved by strict selection with the positive signal. A PQ client is
  offered only providers that sealed a PQ session through the quality probe
  within 8 hours. Fail-closed evaluation therefore no longer waits out
  providers that never could seal one.
- The fail-fast evaluation, discovery cache and field-measurement options
  recorded on 7918 are not needed.
- The residual case is a provider that lost PQ after its last positive run.
  Its client fails closed within the existing ping budget, as today, and its
  next run reads negative.

### 12.8 No provider counts on its claim (D12.6, 2026-11-13)

**The hole.** Two matches still read a provider's own claim, both only while
the ready marker was down (the first 8 hours of a stream, or after a prober
went silent for 30 minutes or more):
- `require_modern_provider`: a network-only provider was modern by its
  declaration, in the export's `ClientScore.Modern` and in the named read
  (`readProviderModernClientIds`).
- `post_quantum_encryption`: every candidate and named provider was matched
  on its declaration (`readProvidePostQuantumEncryption`).

So providers that claimed PQ but could not complete the handshake were
offered to PQ clients, which then failed closed on each one, as in 7918.

**The rule.** "They must have a positive quality probe handshake to count."
No provider counts on its claim, whatever keys it holds, on either side of the
marker.
- **Marker set:** unchanged (§12.4). The URL probe's positive outcome in the
  8-hour window counts, with fresh window evidence for
  `require_modern_provider`.
- **Marker down:** the probe's modern evidence of the last 7 days counts
  (`provider_modern_evidence`, `ProviderModernEvidenceMaxAge`).
  - `require_modern_provider`: the row is modern, PQ with resizable windows.
    Candidates are judged by `ClientScore.Modern`, which the export now takes
    from the evidence for every provider. Named providers are judged by
    `readProviderModernClientIds`.
  - `post_quantum_encryption`: the row is fresh and PQ, whatever its window.
    Candidates and named providers are both judged by
    `readProviderPostQuantumEvidenceClientIds`.
  - Either way the provider must hold a Public or a Network key. With
    neither, it never counts.
- **Network-only providers** need the same evidence. No probe can reach them,
  so they count only while evidence from a former Public provide is fresh.
- **The declaration** is still stored in `provide_capability`, as the
  provider's record of what it announced. No selection reads it.
  `readProvidePostQuantumEncryption` is deleted.

**Why the marker gate stays.** The gate keeps strict supply from emptying
while the 8-hour signal is incomplete (a stream's first 8 hours) or decaying
(a silent prober). The fallback behind it is now evidence as well:
- since the capability probe's retirement, only the URL probe's conclusive PQ
  turns write `provider_modern_evidence`;
- that evidence outlasts a probe gap by 7 days.

So while the marker is down, strict requests get the providers with positive
evidence from the last 7 days. They answer empty only when no provider has
such evidence, which is what the rule requires.

**Evidence counts PQ only from a positive outcome.** The URL ingest used to
set `post_quantum` from the negotiated group, as the capability probe did. A
PQ session that never sealed, which the quality probe reads as negative
(§12.2), therefore still counted as PQ evidence. Since D12.6 the ingest sets
`post_quantum` only for a positive outcome (`providerModernEvidenceOfE2e`): a
PQ group sealed before the IPv4 HTTPS leg's request, and that leg passed. A
negative never counts. The negotiated key exchange is still recorded.
- Rows written before the change keep their reading until the provider's
  next conclusive PQ turn, about two per 8 hours, or until they are 7 days
  old. Rows the retired capability probe wrote age out the same way.
- While the marker is down, the modern census and modern-first priority move
  with it: a provider whose PQ session never sealed reads `not_post_quantum`.

**Rollout.** It ships with the next api, alt, proxy and taskworker release
(D46). No migration or config change is needed.
- The readers (api, alt and the proxy's in-process FindProviders2) apply the
  named reads and the `post_quantum_encryption` fallback from their deploy.
- The ingest runs in api and alt (remote probers) and in the taskworker's
  in-process operator. Each records the stricter reading from its deploy.
- The taskworker export takes `Modern` from evidence from its first export
  after deploy. Pages an older export wrote expire with the export TTL.

## 13. Implementation status

### 13.1 Workstream D: sdk, apps, localizations

**Landed on main:**
- localizations 19b89a4;
- sdk 1d00b3a3, f00867d6 and dce4ab69;
- android 130ed4526, apple e1c3cfac, windows 93af2d51, linux ff3a813.

**Then landed:** sdk `feat/fp2-limited-window-1112` (2f945bea), as 1777be51,
right after connect's change. Until it landed, the sdk's
`TestExitFromConnectCarriesEveryExitInfoField` failed against connect main's
new ExitInfo fields.

**Unmerged by design:** the /mainnet note, sn
`docs/mainnet-fp2-capabilities-1112` (1c94ca1c) and web 42b091f6. It
publishes with the stable app release (§10).

**Deviations from the design, now folded in above:**
- **§5.5:** every app shows its own localized help line, so all four needed
  code. The server adds `reason_tcp_count` (mask & 5) and
  `reason_ipv4_count` (mask & 6) beside `reason`.
- **§7.4 and §7.10:** mobile window sizes scale with each device's memory
  target. D11.5's 1 is the low-memory bound `LimitedWindowSizeMax`, not a
  fraction.

**Follow-ups:**
- the DeviceRemote (rpc) developer control for the fraction;
- the memory gate on the iOS and Android nightlies (§10).

### 13.2 Workstream S1: server evidence and probe

All server references in this section are at origin/main 86553321d.

**Landed:**
- step 1, merge 2f3ebac8b (875789fcc, 8eb3b8eb4);
- step 2, merge 86553321d (c4eda7229, 925c80009, 64a5e5d3f). c4eda7229's
  message still says "deviation" and "1 h Redis key"; both are superseded,
  and the merge message is correct.

**Not merged:** the capability-probe retirement, branch
`feat/fp2-s1-capprobe-retire-1112` (a253a22c9). It waits for the owner's
config flip (§10).
- It deletes `capprobe` (the README stays, marked "Retired."),
  `taskworker/work/capability_probe_work.go` and their tests.
- It removes panels 127-137 of egress-probes and panels 18-24 of
  capability-probes.
- It adds `work.CapabilityProbe` to `removedTaskTargets`, and
  `ensureProviderBandwidthProbeChain` replaces the chain loop.

**Migration 966:**
- db_provider_egress_fp2_evidence.go:41 is one DO block with `lock_timeout`
  5 s. It adds `failed_later_legs` and `pq_outcome` to
  `provider_egress_health_history` (§4.1), in the catalog only, unattached
  day tables included. A rerun takes no lock.
- Accessor at :78; position db_migrations.go:11513.
- Monitor contract: monitor/migration_provider_egress_fp2_evidence.go:14,
  registered at monitor/migration_current_suffix.go:1003. It adds two schema
  faults.
- Applied on Main on 2026-11-13 at 00:14:53Z.

**Mask and outcome** (the single source; S2 aliases them):
- `UrlProbeFailedLaterLegIpv4Udp`, `Ipv6Tcp` and `Ipv6Udp` are 1, 2 and 4
  (qualityprobe/egresshealth/url_evidence.go:87).
- `FailedLaterLegs(ok, total)` (:105) is 0 unless the run is a failed single
  run.
- `UrlProbeE2eEvidence{pq, key_exchange, sealed_before_request, window}`
  (qualityprobe/egresshealth/url_e2e.go:108):
  - the pq values are at :44 and the window values at :60;
  - `pq_outcome` 0, 1, 2 at :69;
  - the post-quantum groups are at :83 and
    `UrlProbeKeyExchangePostQuantum` at :94.
- Validation (:123): `positive` needs a post-quantum group and
  `sealed_before_request`; `negative` is refused for a sealed post-quantum
  session; the key name is at most 64 bytes of UTF-8.
- `PqOutcome` is at :161. qualityprobe/egresshealth/url_evidence.go:79 adds
  the field. `ValidateOutcome` (:556) refuses a positive unless the IPv4
  HTTPS leg passed.

**Ingest:**
- model/provider_egress_health_model.go:408-420 writes both columns.
  Rechecks, probation runs and outcomes from older releases store 0 and 0; a
  replay stores nothing.
- `ProviderEgressHealthStored` gains both
  (model/provider_egress_url_source.go:132-134).
- `recordProviderEgressHealthE2e`
  (model/provider_egress_post_quantum_ready.go:83) runs after the store
  transaction (model/provider_egress_health_model.go:286, 297). It records
  modern evidence and notes the stream.
- Metrics: `urnetwork_egress_url_failed_later_legs_total{legs}`
  (controller/provider_egress_url_source.go:249) and
  `urnetwork_egress_url_pq_outcomes_total{pq}` (:258).

**Ready marker:**
- key at model/provider_egress_post_quantum_ready.go:46;
- `ClientScorePostQuantumReadySettings{ReadyAfter 8h, SeenMaxAge 30m}` (:56);
- `MarkClientScorePostQuantumReady` (:149), called by the export after each
  complete export (model/network_client_location_model.go:6231).

**The PQ turn schedule** (D12.5):
- `ProviderUrlProbePostQuantumSettings{RefreshAge 4h, TurnStride 3}`
  (model/provider_url_probe_post_quantum.go:38).
- `markProviderUrlProbePostQuantumTurns` (:65) runs after the claim
  transaction (model/provider_url_probe_due.go:311) and never fails a claim.
- The flag travels:
  1. `ProviderUrlProbeDue.PostQuantum` (model/provider_url_probe_due.go:48);
  2. `ingest.DueProvider` (qualityprobe/ingest/schedule.go:166);
  3. localoperator/operator.go:306;
  4. `prober.Provider.PostQuantum` (qualityprobe/prober/prober.go:38);
  5. qualityprobe/fleetprobe/pool.go:77.

**Prober:**
- Only PQ turns get a recorder and a capability observation
  (qualityprobe/fleetprobe/full.go:251-263).
- `UrlProbePostQuantumSettings{SealWait 3s, UnsealedGrace 10s,
  NoResponderWindow 60s}` (qualityprobe/fleetprobe/url_e2e.go:32). The
  observation is at :60, with `EstablishHold` equal to `SealWait` and
  `KeepTunnelOnReplacement`. `readUrlProbeE2e` is at :129.
- egresshealth:
  - `Options.E2e` (qualityprobe/egresshealth/egresshealth.go:694);
  - the run budget adds the seal wait once per generation (:943);
  - qualityprobe/egresshealth/url_family.go:281 hooks the IPv4 HTTPS leg's
    first request write, which defines `sealed_before_request`;
  - qualityprobe/egresshealth/url_probe.go:127 sets the evidence.
- providertunnel:
  - `EstablishHold` and `KeepTunnelOnReplacement`
    (qualityprobe/providertunnel/capability_observation.go:50, 56);
  - `Tunnel.ClientReplaced` (qualityprobe/providertunnel/tunnel.go:794);
  - `watchTunnelPath` (:857);
  - qualityprobe/fleetprobe/path.go:173.
- taskworker:
  - the peer key fetcher (taskworker/work/provider_url_probe_post_quantum.go:29)
    uses `model.GetClientPublicKey`, wired at
    taskworker/work/provider_egress_probe_work.go:2163;
  - `urnetwork_url_probe_e2e_outcomes_total{pq}` (:43), observed at
    taskworker/work/provider_egress_probe_metrics.go:654.

**Dashboard:** egress-probes.json row 170, panel 171 (failed later legs) and
panel 172 (PQ outcomes). Test: grafana/url_probe_fp2_evidence_dashboard_test.go.

**Deviations from §12, folded in above:**
1. The seal wait is connect's `OpportunisticEstablishHold`.
2. `sealed_before_request` is read at the IPv4 HTTPS leg's first request
   write.
3. `negative` is stricter: a 10 s unsealed grace, and a full 60 s no-responder
   window while the provider acknowledges data.
4. A replaced window client or a re-created tunnel reads `unmeasured`.
5. `key_exchange` is omitted when empty, and the modern evidence's window size
   is 0.
6. The stream is noted on every stored outcome.
7. The capability probe is switched off by a config flip, because a code
   switch-off would trip monitor §2.19g.
8. The residual case of §12.2.

**Tests:**
- 24 of 24 FP2 DB tests on the tested tree ddb722dda.
- On 66bfa959a: 266 model and controller DB tests, taskworker 222 of 222, and
  providertunnel 198 of 198.
- One failure, `TestUrlProbeDueHundredThousandProviders`, is a load timeout
  at load 100-285. It fails the same way without step 2.

**Release status:**
- D42, rolling now, is server a151cee8d. It contains step 1 only.
- Step 2 (86553321d) ships in the next server release.

**Follow-up after the retirement:** remove monitor §2.19g and the model's
capability-probe page query.

### 13.3 Workstream C: connect

All connect references in this section are at connect main 13a6c4f2.

**Landed:**
- connect 885ab442..8b52ea81, `Merge feat/fp2-connect-placement`:
  - d1db68a3, the feature;
  - 91585162, an origin merge;
  - af6decb8, the UDP no-route verdict judges the flow's own windows;
  - f8e4edce and 1b21a06d, CODESTYLE;
  - 1d168653, tests.
- 8b52ea81..13a6c4f2, `Merge feat/fp2-connect-spec` (38d3072e,
  api/bringyour.yml), after S2's server model fields (server 3dcc7b138).
- In all: 52 files, +4522/-649. No migrations or config.
- D's sdk limited window landed right after it (sdk 1777be51).
- S2's proxy exporter, server `feat/fp2-s2-proxy-window-metrics-1112`
  (dd73d29a1), landed as server merge 4fdf026c0 (§13.4).

**Wire and labels:**
- `FindProviders2Args.Select` (connect/api.go:406), the echo (:419), and each
  provider's `Capabilities` (:447) and `OverflowStep` (:451), all omitempty.
- Without `select`, the request bytes are unchanged; a test pins them.
- `ProviderCapabilities{Ip, Protocol}` (connect/ip_capability.go:42).
- `Normalize` (:51) fails closed (§7.1).
- `providerCapabilitiesFromIpFamily` (:84).
- `FindProvidersSelect` (:103) sends all six members without omitempty,
  because the server defaults `dualstack_target_count` to 1.
- `flowNeed` (:122) and `carriedBy` (:141) implement §7.2.
- `addProviderDestinations` (connect/ip_remote_multi_client_api.go:1108)
  labels each entry. `DestinationStats` gains `Capabilities` and
  `OverflowStep` (connect/ip_remote_multi_client.go:99-107).

**Generator:**
- `MultiClientGeneratorWithCapabilities` (connect/ip_remote_multi_client.go:237)
  adds `NextDestinationsWithCapabilities` and
  `NextDestinationsForClientIdWithCapabilities`, each returning the answer
  and whether it was in selector mode, plus `SelectorMode()`.
- The API generator implements it:
  - connect/ip_remote_multi_client_api.go:749 and :760;
  - `nextDestinationsContext` at :833;
  - `observeSelectorMode` at :982;
  - a tri-state at :1004.
- Selector mode means the request carried `select` and the answer echoed it.
  Each answer replaces the last, so a server rollback mid-session is followed.
- `require_modern_provider` rides every request, the limited window's
  included. A wrapper that embeds `*ApiMultiClientGenerator`, such as the
  server qualityprobe's, inherits all of this.

**Window requests** (connect/ip_remote_multi_client_limited.go):
- `providerSelect` (:288) builds each window's `select` (§7.4).
  - `target_count` comes through `setRequestSizing`
    (connect/ip_remote_multi_client.go:14697), seeded by `initRequestSizing`
    (connect/ip_remote_multi_client_limited.go:239).
- `discoverDestinationsWithCapabilities` (:358):
  - the limited window asks once, `v4-capable`;
  - a normal window asks once, and the server serves its IPv6 need;
  - `probeOnly` sends the IPv6 request alone.
- Named lookups (`lookupClientId` :463, `relabelledStats` :491) overlay fresh
  labels only in selector mode. The limited window drops a provider
  relabelled `dualproto`.
- The rank mode comes from `requestRankMode` (:276) and
  `limitedWindowRankMode` (:266), per D10. Discovery demand uses
  `admitsCapabilities` (:334).

**The limited window:**
- `WindowTypeLimited = 3` (connect/ip_remote_multi_client.go:139).
  `RankMode()` returns "" for it, and `PerformanceProfile.Validate` refuses
  it.
- It is created at :3202, except under a fixed destination. Its hooks (:3228)
  are stored as an atomic pointer once every window exists, followed by one
  wake.
- Settings:
  - `LimitedWindowSizeFraction` (:698, default 0.5 at :314);
  - `LimitedWindowSizeMax` (:702);
  - at runtime, `SetLimitedWindowSizeFraction`
    (connect/ip_remote_multi_client_limited.go:629) and
    `LimitedWindowSizeFraction()` (:620).
- Sizing:
  - `limitedWindowSize` (:93): min, max and target are equal;
  - `limitedWindowSizing` (:136) returns 0 until every normal window has
    published (`publishSizing` :178);
  - the window's `SetPerformanceProfile` resets the published sizing
    (connect/ip_remote_multi_client.go:13629).
- No standing reserve (:14728). The listing is held while the target is 0
  (`limitedListingHeld`, connect/ip_remote_multi_client_limited.go:320;
  enumerator at connect/ip_remote_multi_client.go:13834 and 13937).
- Regeneration: `SetPerformanceProfile` (:3558) installs the profile on and
  shuffles quality, speed, then limited. A PQ toggle is a profile change, so
  it regenerates all three.
- Accessors:
  - `LimitedWindowSize()` and `LimitedWindowTargetSize()`
    (connect/ip_remote_multi_client_limited.go:591, 607);
  - `MultiClientMemorySnapshot.LimitedClientCount`
    (connect/ip_remote_multi_client.go:2283);
  - `ExitInfo.CapabilityIp`, `CapabilityProtocol` and `OverflowStep`
    (:10791).

**Placement:**
- `supportsFlow` and `supportsIpPath`
  (connect/ip_remote_multi_client_family.go:92, 104) replace
  `supportsIpVersion` everywhere. They read only the server's labels, kept in
  the channel's immutable args (`capabilities`, :55).
- The sites of §7.3 are pinned by `TestEveryPlacementSiteReadsTheFlowLabels`
  (connect/ip_remote_multi_client_capability_placement_test.go:423). Among
  them:
  - `raceCandidates` (connect/ip_remote_multi_client.go:4269) and
    `raceField` (connect/ip_remote_multi_client_limited.go:519);
  - `orderedClients` (connect/ip_remote_multi_client.go:16707) and
    `lastResortClientsForFlow` (:16587);
  - the IPv4 and IPv6 affinity donors (:4879, :4971) and the app-pin donors
    (:5668);
  - QUIC rebind's `carriesUpdates` (:6659), and adoption (:10340);
  - `flowAvailable` (connect/ip_remote_multi_client_family.go:311) and
    `candidatesForFlow` (:733).
- The race: each window's field is followed by the limited window's. A
  bounded race uses `limitedClientsLast(contractRateLimitHeldLast(...))`
  (connect/ip_remote_multi_client.go:8552).
- Auto sends UDP on any port to quality, then speed (`selectWindowTypes`,
  :3722, :3745).
- No route: `replyUdpNoRoute` (connect/ip_remote_multi_client_family.go:708,
  sent at connect/ip_remote_multi_client.go:8917) answers when
  `flowUnroutable` holds (connect/ip_remote_multi_client_family.go:653).
  The accessors are `UdpAvailable` (:608), `UdpUnroutable` (:642) and
  `UdpNoRouteCount` (:727). `Ipv6Available` now includes the limited window.
- **B5 is retired** (§7.9), and IPV6.md B5 is marked superseded.

**Status and stats:**
- `WindowExpandEvent.UdpAvailable`
  (connect/ip_remote_multi_client_monitor.go:46).
  `AddWindowExpandEventWithAvailability` (:470) is new;
  `AddWindowExpandEvent` is unchanged.
- The merged monitor (`newMergedMultiClientMonitorWithAvailability`, :689)
  takes only `Ipv6Available` and `UdpAvailable` from the limited window, and
  nothing about size, readiness, failure, reason, location or provider
  events.
- `WindowClientStats` counts three things; the window label includes
  `limited`:
  - window clients by window, IP label and protocol label;
  - flows, once at first bind, by window, IP version and protocol (`addFlow`,
    connect/ip_remote_multi_client_window_client_stats.go:568);
  - no-compatible replies (`addNoCompatible`, :579).

  Visitors are at :591, :607 and :623.
- The limited window is in the memory snapshot, the owner census, transfer
  stats and pacing diagnostics. SYN readiness capacity adds its share.

**Deviations from §7, folded in above:**
1. A legacy v6-only provider stays v6-only (§5.4).
2. No limited window under a fixed-IP destination: a second exit would change
   the egress address.
3. `dualstack_target_count` is floored at 1 for the auto quality window and a
   fixed profile's active window (§7.4).
4. `target_count` is the pre-reserve demand target, seeded from config.
5. The limited window has no watchdog and no standing reserve. Its hard max is
   unbounded when a normal window's is. It waits for every normal window to
   publish, and it is disabled without an echoing server.
6. The fraction lives in `MultiClientSettings`, with a runtime setter.
   `LimitedWindowSizeMax` was added for D11.5, since Android's quality window
   is 10 and no fraction yields 1.
7. Unknown labels fail closed (§7.1).
8. In selector mode, IPv6 starvation is read from the main answer.
9. A bounded race puts normal candidates before limited ones.
10. The UDP verdict judges only the flow's own windows (§7.7).
11. Limited sizing counts only active normal windows.

**Tests:**
- **Pre-change probe at fc949b78:**
  - auto UDP to ports 53 and 3478 went speed, then quality: FAIL;
  - three IPv6 dial strikes latched v4-only: FAIL;
  - window requests carried no `select`: FAIL;
  - legacy request bytes: PASS, and pinned identical after the change.
- **42 new tests:** connect/ip_capability_test.go (12),
  connect/ip_remote_multi_client_limited_test.go (16),
  connect/ip_remote_multi_client_capability_placement_test.go (11) and
  connect/ip_remote_multi_client_capability_api_test.go (3). B5's tests were
  removed or rewritten.
- **The merged tree 8b52ea81:**
  - the full `go test -race` root package ran in 2 h 57 min under load
    40-300, with no race or panic;
  - three timing-bound failures outside the change each pass 5 of 5 on both
    the base and branch trees: `TestAltMemoryGoawayRetiresPoolAndJoinsShutdown`,
    `TestChromeClientHelloResumesSessionsPerPath` and
    `TestWindowRetainedCandidateUsesMeasuredRoundTrip`;
  - the targeted `-race` run of 126 tests passed; build and vet are clean.
- **Spec:**
  - api/test_spec.py passes 7 of 7, and check-routes.sh shows no drift;
  - server `TestSpecConformance` passes at 3dcc7b138;
  - a control run on the old spec logs the 4 new fields as drift.

**Memory:** about floor(0.5 × S) more clients: 3 on desktop in auto, 2 on
iOS and 5 on Android (§7.10).

### 13.4 Workstream S2: server selection

All server references in this section are at server 4fdf026c0, S2's later
merge, which contains 3dcc7b138.

**Landed:**
- 3dcc7b138, `Merge feat/fp2-s2-selection-1112`, onto dfacbfd0b:
  - 4aac4a980: the classification, the aggregate counts and the
    `ClientScore` fields;
  - 43dd4ec30: selector mode, the export facets, `:native_cap_v1`, the named
    and PQ sets, strict PQ and the metrics;
  - 58efed495: copies rank by their class's index, and DB tests;
  - 4adbb2ab0: comments and a partition test;
  - 7d6e84602, merging 86b26ee6e: the status reason, the stats gauges and the
    providers dashboard;
  - 9ad7a3097: one shared v6-only classifier;
  - e927d6760 and 574bd9633: test fixes;
  - e31aeb30d: a main merge with S1 step 1, where S1's ready marker replaced
    S2's stand-in;
  - b80a4291c: the mask and PQ constants alias S1's egresshealth constants.
- 4fdf026c0, `Merge feat/fp2-s2-proxy-window-metrics-1112` (dd73d29a1), onto
  f5a19c1db: the proxy exporter of §8.2, built against connect 13a6c4f2.
- No migrations: S2 reads S1's 966.

**Labels and classes** (model/provider_capability.go):
- The labels are `ProviderCapabilityIpDualstack` and `...Ipv4`, and
  `ProviderCapabilityProtocolDualproto` and `...Tcp`.
- `ClientScoreCapabilityDowngradeIpv6` is 1 and `...Udp` 2 (:45).
- `ProviderEgressFailedLaterLeg{Ipv4Udp,Ipv6Tcp,Ipv6Udp}` (:55) alias
  `egresshealth.UrlProbeFailedLaterLeg*`.
- Classes A-D are `providerCapabilityClass`.
- `providerCapabilityForgiven` (:214) is §3.2's table:
  - a dual-stack provider's B forgives `ForgivenIpv4`, its C `ForgivenTcp`
    and its D `ForgivenBoth`;
  - a v4-only provider's D forgives `ForgivenTcp`;
  - an advertised class forgives nothing.
- `classifyProviderCapability` (:272) is §3.3, with `ComputeEgressIndex`'s
  exact 4/5 comparison. With fewer than 2 runs, or no current evidence, the
  provider keeps its advertised labels.
- `classifiedCounts` (:317) adds the forgiven runs overall, and to `General`
  from `General`'s own counts.
- `ClientScore.CapabilityLabels()` (:200) gives the labels of §4.3.
- `classifyProviderCapabilityOfFamilies`
  (model/provider_capability_families.go:19) never classifies a v6-only
  provider. The export, the status and the counts all use it.

**Aggregate** (model/provider_egress_health_model.go):
- The window SELECT reads `failed_later_legs` and `pq_outcome`. The store
  side (:415) is S1's.
- `ProviderEgressHealthCounts` (:661) gains the counts of §4.2 (:682-695).
- `postQuantumPositive()` (:701): the newest positive is strictly later than
  any negative.
- `providerEgressHealthCountsSql` (:787) uses integer FILTERs only, no jsonb.
  Its general filters keep `url_source = 0` last.

**Classified decision** (model/egress_index.go):
- `classifiedEgressFacts` (:844) runs over `egressFactsWithCounts` (:861):
  today's facts, with the class's forgiven runs as passes. That covers the
  Quality and Speed ratio, the index, and the Online rule's 0.8 general half.
- The strict country guard applies to both decisions. A provider that is not
  downgraded gets exactly its strict facts.
- `rankClientScoreWithCounts` (model/network_client_score_rank.go:190) takes
  the URL weight from OK'.

**Export** (`UpdateClientScores`, model/network_client_location_model.go):
- The loop at :5903 classifies every decided provider through
  `newClientScoreCapability` (model/network_client_score_capability.go:91).
  That computes:
  - the classification;
  - for a downgraded provider, the classified decision and counts;
  - `postQuantumPositive`;
  - `postQuantumModern` (`providerPostQuantumModern`, :153).
- The PQ marker is read at model/network_client_location_model.go:5922,
  before any score is written.
- `migrateClientScore` (:5939) sets `CapabilityDowngrades` and
  `PostQuantumPositive` on the shared score, whose buckets stay strict.
  - A downgraded provider also gets the unexported `capabilityCopy`, ranked
    with the classified decision and counts.
  - The copy's Quality tier comes from its class's index
    (`rankClientScoreCapabilityTiers`,
    model/network_client_score_capability.go:120, called at
    model/network_client_location_model.go:5982).
- `Modern`: once the marker is set when the export starts, `Modern` is
  `postQuantumModern`. Before that, it is the evidence decision for every
  provider, network-only ones included. Since D12.6 (§12.8) no declaration
  counts on either side.
- `exportClientScores` (model/network_client_score_targets.go:300) puts the
  copies (:311-320) into the union facets `i`, `t` and `x` (Online or native)
  and into the native capability facets, in non-forced modes only.
  - Every capability facet is written, empty ones included.
  - The `ClientFilter` does not count copies.
- The emit (model/network_client_location_model.go:322-331, 355-391) writes
  `c_l_<i|t|x>` and `s_l_<facet>_<n>`, and puts `i`, `t` and `x` in the
  `m_l` partition.
- `:native_cap_v1`:
  - its key is `clientScoreNativeCapabilityKey`
    (model/network_client_score_capability.go:68);
  - `writeClientScoreNativeSnapshotOf`
    (model/network_client_score_native.go:356) writes it, with the manifest
    facets exactly `i`, `t` and `x`;
  - `readClientScoreNativeManifestOf` (:435) reads it;
  - the fanout is `publishCapability` and `aliasCapability` (:40).
- After a complete export:
  1. `publishProviderCapabilitySets`
     (model/network_client_score_capability.go:176, called at
     model/network_client_location_model.go:6224) replaces the four sets,
     each with the export's TTL and a `ready:v1` member, one transaction per
     slot;
  2. then S1's `MarkClientScorePostQuantumReady` runs (:6231).

**Readers:**
- `clientScoreNativeTargetPages` (model/network_client_score_native.go:575)
  collects the pages per snapshot. The legacy read is unchanged.
- `clientScoreReadOptions` (model/network_client_score_read.go:187) adds
  per-facet record filters, and switches the un-faceted fallback off.
- `clientScorePage.filter` (model/network_client_score_cursor.go:36) runs
  after the count and checksum proofs.
- The pool facet `"*"` (model/network_client_selection_evidence.go:155) draws
  a pool as one group.
- `clientScoreRequestFilter` (model/network_client_selection_prefilter.go):
  - `capabilityClasses` (:26) replaces the family facets in selector mode;
  - `requirePostQuantumPositive` (:53) is the strict PQ check.

**API** (model/network_client_location_model.go):
- `FindProviders2Args.Select` (:5166) is a `*FindProviders2Select` (:5172).
  Its pointer members tell absent from null. The same type is the echo, with
  all six members filled.
- `FindProvidersCapabilities{Ip, Protocol}` (:5207).
- `FindProvidersProvider.Capabilities` (:5238) and `OverflowStep` (:5243),
  both omitempty.
- The JSON tags match connect's. None of these fields appears in legacy mode.
- `newFindProviders2Selection` (model/network_client_location_select.go:67)
  validates before any read, with §5.3's 400s and §5.1's defaults.

**Selection** (`findProviders2WithSelect`,
model/network_client_location_select.go:529):
- It is entered at model/network_client_location_model.go:6573, only when
  `select` is set.
- `ipv6Need` is at model/network_client_location_select.go:137.
- Each level runs three phases:
  1. the IPv6 need (:1361): A, then C topping up. A C provider here is
     overflow for a `dualproto` request;
  2. the main pool: A ∪ B for `dualproto`, C ∪ D for `tcp`;
  3. overflow (:1382): C ∪ D, only when `held_count` plus the non-overflow
     answers is below `target_count`.
- Each phase is a full legacy fill in its pool: natives, then the other mode
  at +offset, then the Online union at +2×offset. An Online request is a
  uniform draw.
- `force_minimum` reads the forced pool, classed by each record's downgrade
  bits.
- Overflow gets tier + `findProviders2OverflowBand` (3, :53) ×
  `BackfillTierOffset`, and `overflow_step` 1. No phase redraws a provider
  that an earlier phase or a name answered.
- `providerCapabilityClassesForIpFamily` (:179) is §6.3's `keepFamilies`.
- `newFindProviders2PoolFacets` (:207):
  - non-forced: `d` and `4`, skipping downgraded records, plus the `i`, `t`
    and `x` copies of the pool's classes;
  - forced: `d` and `4`, filtered by class.
- `loadFindProviders2PoolNative` (:292) reads both native snapshots. A
  missing `:native_cap_v1`, as in a rolling deploy, leaves C and D without
  natives. `loadFindProviders2PoolPreferred` is at :373.
- Named providers come first, labelled from the sets
  (`readNamedProviderCapabilities`,
  model/network_client_score_capability.go:255).
- The location fallback keeps today's rule, `targetsWithoutSupply`, over
  every pool the level read.

**Strict PQ:**
- `findProviders2PostQuantumReady`
  (model/network_client_location_select.go:475) reads S1's ready key through
  `clientScorePostQuantumReadyCache`
  (model/network_client_score_capability.go:285), whose max age is
  `RequestSettingsMaxAge`. It is called at
  model/network_client_location_model.go:6593.
- After the marker:
  - candidates need `PostQuantumPositive`, and `Modern` too for
    `require_modern_provider` (model/network_client_selection_prefilter.go:53);
  - named providers are answered from the sets
    (`readFindProviders2PostQuantumEncryption`,
    model/network_client_location_select.go:486, and
    `readFindProviders2NamedModern`, :512).
- Before the marker, the evidence reads of §12.8 run (D12.6):
  `readProviderPostQuantumEvidenceClientIds` for `post_quantum_encryption`,
  and `readProviderModernClientIds` for named providers under
  `require_modern_provider`. This holds in legacy and selector mode alike, and
  overflow never relaxes it.

**Status** (model/provider_status_model.go):
- The `url_checks` item gains `reason`, `reason_tcp_count` (mask & 5) and
  `reason_ipv4_count` (mask & 6) (:339-341).
- The explanation appends a space and the reason (:1015-1020).
- The value, count, total and `passes` stay strict. The compact form carries
  both counts.

**Metrics and dashboards:**
- FP2 request metrics (model/network_client_location_select.go:414-438), with
  every label set registered at startup:
  - `urnetwork_findproviders2_select_requests_total{rank_mode,ip,protocol}`;
  - `urnetwork_findproviders2_select_answered_total{rank_mode,overflow_step,ip,protocol}`;
  - `urnetwork_findproviders2_select_overflow_total{rank_mode}`;
  - `urnetwork_findproviders2_select_ipv6_need_total{rank_mode,outcome}`;
  - `urnetwork_findproviders2_post_quantum_requests_total{field}`, in both
    modes.
- Stats gauges:
  - `urnetwork_stats_provider_egress_capability{bucket,ip,protocol}` and
    `urnetwork_stats_provider_post_quantum{bucket,signal}`
    (controller/stats_collector.go:455, 469);
  - they read `CountProviderEgress` (model/egress_index.go:1451;
    `CapabilityCounts` :1351);
  - the counts snapshot goes to schema 8, with the wire keys `k` and `q`
    (controller/stats_provider_egress_cache.go:137-138).
- The proxy exporter (proxy/window_client_metrics.go:61-77) publishes §8.2's
  three metrics. Its `window` values are `quality`, `speed`, `limited` and
  `auto`.
- Dashboards: providers panels 935-944 and proxy panels 80-82. Their tests
  are grafana/providers_capability_dashboard_test.go and
  grafana/proxy_window_capability_dashboard_test.go.

**Deviations from §3-§12, folded in above:**
1. Today's export cannot stay byte-identical under gob. Content identity and
   the exact legacy answer JSON are pinned instead (§6.1).
2. The legacy path changes only at the select branch, the PQ request metric
   and the marker-gated strict PQ. Selector mode repeats the level body per
   pool (§6.1, §11).
3. Copies take their Quality tier from their class's index (§4.3).
4. The status IPv4 count is mask & 6 (§5.5).
5. The IPv6 need reads "A, then C" as C topping up what A leaves (§6.3). That
   is the same for the windows' minimum of 1.
6. Modern after the marker reads the newest `provider_modern_evidence` row,
   whichever probe wrote it, and `require_modern_provider` also requires
   `PostQuantumPositive` (§12.4).
7. The PQ flag and sets are written by every export, and read only after the
   marker (§12.4).
8. A v6-only provider is never downgraded, so selector mode never draws one
   (§3.6).
9. `urnetwork_stats_provider_post_quantum` buckets by the classified decision
   (§8.1).
10. Overflow eligibility is counted from the non-overflow answers (§6.3).
11. S1's migration is 966, not 965.

**Tests:**
- **Unit:**
  - class boundaries (off by one), mask 4 to B before C, too few runs, no
    class passing, no UDP legs, counts and labels, reasons, and the strict
    guard under the Online 0.8 rule;
  - selection defaults, the echo, every 400, `ip_family` classes, the phases
    and the pool facets;
  - export facets, emit keys, the legacy JSON, today's-facet content and
    partition compatibility.
- **Redis reads:** the skip filter, the rolling deploy, and `:native_cap_v1`
  against today's reader.
- **DB:**
  - the export's facets, snapshot and sets;
  - selector mode: legacy unchanged, the need, the main pool, overflow at
    held 6, 7 and 10, exclusions, the limited window's request, `dualstack`
    with `tcp`, v6-capable, v6-only with names, named labels, online,
    `force_minimum` and the 400s;
  - strict PQ before and after the marker;
  - the status reason, and the capability and PQ counts.
- **Proxy:** collector tests.
- Every new test failed to compile before the change.
- **The merged-tree gate:**
  - model DB 100 of 100, 261 related model unit tests, and controller stats
    21 of 21;
  - `TestEveryAlertClassIsClassified`, the proxy window tests,
    `TestSpecConformance` and `TestSpecRoutesImplemented` pass;
  - build and vet are clean, and ./grafana fails only on its 21 known
    failures;
  - `TestFindProviders2PrefilterBoundsSubscriberReadsUnderContention` flaked
    once under load 150-400, and passed in the gate.

**Release status:**
- D42, rolling now, is server a151cee8d. It carries S1 step 1 only.
- S2 (3dcc7b138 and 4fdf026c0) and S1 step 2 ship in the next server release,
  in §10's deploy order.
- No config or vault change.

## 14. Review findings and fixes (2026-11-12)

This review used server `a6735f73e` and connect `13a6c4f2` as the original
baselines. The findings, fixes and regression evidence are recorded below.
The implementation follows connect's
[CODESTYLE.md](../connect/CODESTYLE.md).

### 14.1 P1: preserve the requested IPv6 minimum through admission and refill

**Failure.** FP2 requested at least one dualstack provider for an active
window, but mixed that candidate with IPv4 providers. Faster IPv4 evaluations
could consume every admission slot. Initial formation, fixed profiles and
mobile windows with no explicit IPv6 minimum then computed zero shortfall,
so a full IPv4 window never recovered the requested capability.

**Fix.** The same requested dualstack target now drives discovery ordering,
admission reservations and refill shortfall. Scarce IPv6 candidates enter the
bounded evaluation set first. Admission reserves the remaining needed slots
until that expansion pass completes; completion admits healthy IPv4 fallback
when IPv6 supply is absent. A full strict-cap window uses the existing safe
victim/replacement path only after supply is ready. Fixed destinations remain
exempt, and the window caps and client retirement ownership are preserved.
See [request sizing and discovery](../connect/ip_remote_multi_client_limited.go),
[shortfall](../connect/ip_remote_multi_client_family.go) and
[resize/admission](../connect/ip_remote_multi_client.go).

**Regression evidence.** Against the original production code,
`TestExpandReservesRequestedIpv6Admission` admitted the fast IPv4 provider into
the only slot. `TestResizeFormationReservesRequestedIpv6` reproduced that loss
through real resize for both auto and fixed profiles.
`TestResizeRecoversRequestedIpv6AtStrictCapacity` observed a requested target
of one but zero refill shortfall in a full five-slot mobile window. All pass
after the fix; recovery also covers the one-slot fixed profile.
`TestExpandIpv6ReservationFallsBackWhenSupplyEnds` verifies bounded IPv4
fallback for unknown and known-empty IPv6 supply. The fixtures use distinct
providers, actual enumeration/admission, explicit callback barriers and
complete client joins, without timing-based candidate races. See
[admission tests](../connect/ip_remote_multi_client_expand_lifecycle_test.go)
and [formation/recovery tests](../connect/ip_remote_multi_client_ipv6_admission_test.go).

### 14.2 P2: expire PQ proof independently of ordinary URL health

**Failure.** Score pages and named-provider sets retained only positive PQ
booleans. A nearly eight-hour-old positive could remain eligible for the
300-minute export TTL, even while newer ordinary URL probes kept ordinary
health fresh. Both strict PQ and modern-provider selection could consume it.

**Fix.** Exported scores retain a PQ-specific deadline: the earlier of the
positive's eight-hour limit and the snapshot's freshness limit (30 minutes by
default). Modern proof additionally respects its seven-day evidence limit.
Scored and named reads share one request clock and reject the exact deadline
and later times. The existing named sets retain their Redis keys and types;
same-slot `:proof_v1` hashes carry deadlines and are published atomically with
membership. Readers batch membership and proof reads, require both, and do
not query the database per provider. Missing old-snapshot deadlines fail
closed after the ready marker; the intended pre-marker legacy behavior
remains. See [export and named proof](model/network_client_score_capability.go),
[score fields and legacy selection](model/network_client_location_model.go),
[selector reads](model/network_client_location_select.go) and
[prefilter](model/network_client_selection_prefilter.go).

**Regression evidence.** `TestPostQuantumExportCannotOutlivePositive` failed
on the baseline: an actual exported/gob-decoded positive survived its own
eight-hour window despite a fresh ordinary URL result. It passes after the
fix. `TestPostQuantumExportProofBoundaries` and
`TestNamedPostQuantumExportProofBoundaries` pass exact before/at/after
boundaries for positive age, snapshot age and modern age. The named tests
publish and read actual Redis sets/hashes and cover missing/malformed proof,
missing readiness and membership removed by an older writer. The public API
matrix, `TestFindProviders2ExpiredPostQuantumInEveryDraw`, exercises legacy
and selector modes, PQ and modern requirements, discovered and named draws,
and force-count fallback. These tests live in
[network_client_post_quantum_expiry_test.go](model/network_client_post_quantum_expiry_test.go).

### 14.3 P2: bind URL proof to one live sending epoch

**Failure.** `RequestWritten` saved a sticky sealed boolean, while completion
read the recorder's latest key-exchange group. A classical request followed
by a PQ seal could therefore report positive. A seal retained after live
session demotion could also be mistaken for usable encryption. Reading only
at `WroteRequest` additionally left a gap because that callback follows the
request's writes.

**Fix.** The opt-in recorder exposes the live ordinary initiator's client,
epoch and group separately from sticky capability evidence. It uses the
actual cipher gate and a weak session reference, so retired clients are not
retained and normal unobserved traffic gains no packet-path recording. The
URL observer requires the same live epoch at `GotConn`, `WroteRequest` and
completion; later request callbacks cannot overwrite the first bound epoch.
Rekey, demotion or missing session proof leaves PQ unmeasured. Classical
negative evidence is retained in either transition direction. Opportunistic
URL requests still proceed without a responder. See
[live recorder](../connect/transfer_peer_capability_observation.go),
[cipher-gate observation](../connect/transfer_encrypt.go),
[request trace](qualityprobe/egresshealth/url_family.go) and
[URL evidence](qualityprobe/fleetprobe/url_e2e.go).

**Regression evidence.** On the baseline,
`TestUrlProbeE2eObservationDoesNotCombineRequestAndRekey` reported a positive
PQ result for a classical request followed by a PQ rekey. The corrected test
passes. `TestPeerCapabilityRecorderBindsLiveSendingEpoch` drives actual epoch
promotion and cipher withholding and passes, including role/companion/closed
session exclusions. Additional observer tests cover a different PQ epoch,
classical rekey, demotion before/after the request, an unchanged positive,
and rekey between `GotConn` and `WroteRequest`. See
[recorder tests](../connect/transfer_peer_capability_observation_test.go),
[observer tests](qualityprobe/fleetprobe/url_e2e_test.go),
[trace tests](qualityprobe/egresshealth/url_e2e_test.go) and
[local URL tunnel tests](qualityprobe/providertunnel/post_quantum_url_turn_integration_test.go).

### 14.4 P2: read original dualstack facets for forced B/D pools

**Failure.** Forced `v4-only` selection read the IPv4 export facet for B/D
classes. Export retains a downgraded provider in its original address-family
facet, so dualstack providers downgraded for IPv6 (B), or IPv6 and UDP (D),
were compatible but invisible to that read.

**Fix.** Forced pool loading includes the original dualstack facet for all
requested classes, then applies the existing class filter. The original IPv4
facet is still needed for B/D; IPv6-only providers remain excluded. See
[pool facets](model/network_client_location_select.go).

**Regression evidence.** `TestLoadFindProviders2ExportedForcedDowngrades`
failed against baseline for B, D and combined B/D pools because downgraded
dualstack records were missing. It passes after the fix through actual score
export, Redis payload reads and pool union. Its matrix includes original
IPv4/dualstack/IPv6-only families and class combinations. See
[export/read regression](model/network_client_location_select_read_test.go).

### 14.5 Validation and scope

The failing-before runs used immutable copies of the two original heads via
Go overlays, so later edits could not change a build's baseline. The Connect
overlay added only an inert end-of-resize test barrier; its original
decisions remained unchanged. Server regression adapters used the old APIs
for the original expiry and epoch-mixing assertions. The final deadline and
live-session API tests add coverage beyond those baseline adapters; their
compilation was not presented as a baseline behavioral failure.

Completed checks:

- Connect composed regressions: `TestExpandReservesRequestedIpv6Admission`,
  `TestResizeFormationReservesRequestedIpv6`,
  `TestResizeRecoversRequestedIpv6AtStrictCapacity`,
  `TestExpandIpv6ReservationFallsBackWhenSupplyEnds`,
  `TestPeerCapabilityRecorderBindsLiveSendingEpoch` and
  `TestIpv6CapableShortfall` passed.
- Connect nearby family/discovery/window/recorder tests passed, followed by
  the adjacent `TestEnumerateDestinations*`,
  `TestCandidatePathsRespectIpVersion`,
  `TestFixedDestinationExitCarriesEveryVersion`, `TestMultiClientExpand*`,
  `TestEvaluationBudgetDeadlineOwnership*`,
  `TestEndToEndHandshakeRecordsItsKeyExchange`,
  `TestPeerInitiatedHandshakeRecordsNoEvidence`, `TestClientHello*`,
  `TestEncryptionOffRecordsOnlyAcknowledgementEvidence` and
  `TestAcknowledgementsRecordReceiveWindowEvidence`.
- Connect race check passed with selector
  `Test(ExpandReservesRequestedIpv6Admission|Resize.*RequestedIpv6|ExpandIpv6ReservationFallsBackWhenSupplyEnds|PeerCapabilityRecorderBindsLiveSendingEpoch|PeerCapabilityRecorderConcurrent)`.
- Focused server expiry/boundary, named-proof, forced-facet and initial URL
  epoch regressions passed against disposable PostgreSQL/Redis fixtures.
- Final exact server regressions passed:
  - `TestFindProviders2ExpiredPostQuantumInEveryDraw` passed after its fixture
    was corrected to publish the full modern-partition export (19.533 s).
  - The final fleet observer and egress trace regressions passed, including
    `TestUrlProbeE2eObservationRejectsRekeyDuringRequestWrite` and
    `TestUrlProbePostQuantumTurnReadsItsSessionAtTheIpv4HttpsRequest`.
  - `TestPostQuantumUrlTurnThroughALocalProvider` passed (24.335 s), proving
    that the real seal path supplies the same live epoch before the request,
    after its write and at completion.
  - `TestPostQuantumUrlTurnWithoutAResponderCarriesTheSameRequest` passed
    (34.695 s), preserving the opportunistic plaintext fallback.
- `git diff --check` passed in both repositories.

Broader server validation is delegated to the already-running server test
runner at the user's request. This work's overlapping broader run was
stopped deliberately, not treated as a test failure or a blocked check. No
production services, deployment or shared launcher repairs were part of
these fixes.
