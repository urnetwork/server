# Reviewed ARIN classification and cutover

## 2026-10-04 classifier review, research and discriminator additions

This section records the deep review of [ARINDB.md](ARINDB.md) and the
classifiers, the research into additional evidence, and the builder changes
that followed. It supersedes the "Deferred" status of the origin-ASN and RPKI
rows in the signal matrix below; the other rows keep their status.

### Review findings

| Finding | Consequence | Disposition |
| --- | --- | --- |
| The origin stage discarded the RIS peer count, so a prefix/origin pair seen by one of 453 peers carried the same weight as a globally visible aggregate. Single-peer pairs include `3.0.0.0/8` from an unrelated origin, `198.18.0.0/15` benchmarking space and leaked ISP aggregates. | A leak, hijack or local-only announcement by a reviewed ISP ASN could manufacture an inference. | Fixed: visibility floor with aggregate inheritance. |
| No origin authorization check existed. | An unauthorized origin, including a leased or hijacked block announced from a reviewed access ASN, inferred clean. | Fixed: RPKI withholding. |
| Catalog `countries` were review context only. On a 127-ASN eyeball sample, routes geolocated outside the operator's country were almost entirely on-net CDN caches, leased blocks and anycast, and two deliberately misidentified entries geolocated 100% outside their stated country. | The cheapest available discriminator was unused, and a misidentified identity was invisible until shadowed. | Fixed: reviewed-country withholding and the catalog audit. |
| Proxy, VPN and Tor evidence existed only at organization and ASN scope, so an exit inside a reviewed access network inherited the inference. The official Tor export names 1,398 exit addresses. | Known contrary use at exact addresses was approved. | Fixed: address-level risk lists. |
| Records outside ARIN space carried no `associated_country`, so neither geography nor the country discriminator could apply there. | The global stage was blind to geography. | Fixed where an inference is made: the country policy records the observed country at withheld cells; GeoLite is now an augmentation input. |
| The catalog had 4,786 operators but no tooling to measure a catalog against observed routing before a 500 MB build. | Identity errors surfaced only in full readbacks. | Fixed: `audit-subscriber-catalog`. |
| Registration-country geographic risk applies only to ARIN direct registrations. Measured against the NRO delegated file, the delegated country disagrees with GeoLite for 6.4% of ARIN, 6.4% of RIPE, 6.1% of AFRINIC, 1.5% of APNIC and 0.7% of LACNIC assigned IPv4 space. | Risk semantics differ by registry: the same cross-border registration is risky in ARIN space and clean elsewhere. | Not changed: applying it globally would exclude several percent of non-ARIN supply from every bucket, which is a policy decision, not a classifier correction. The measurement and the delegated-file format are recorded for that decision. |
| Visibility and validity inheritance initially compared ASN sets, so sibling ASNs of one operator (an Orange ES aggregate with Jazztel more-specifics) looked like different identities. | 7,164 of 7,165 Jazztel routes were withheld. | Fixed: inheritance also recognizes identical reviewed-operator sets; the catalog must list sibling ASNs under one operator ID. |

The registration stage, allocation scopes, country evidence and runtime decoder
were reviewed without a correctness finding. The augmentation loop's iteration
errors surface through `Decode`, so no unchecked iteration remains.

### Research summary

Sources were fetched and their formats verified on 2026-10-04. Adopted now, as
pinned reviewed snapshots:

- RIS peer counts, already in the pinned dumps (`<origin> <prefix> <seen by #rispeers>`).
- RPKI payloads: `https://rpki.cloudflare.com/rpki.json` and
  `https://console.rpki-client.org/vrps.json` (integer or `AS`-prefixed ASNs,
  AS0 payloads present), routinator CSV, and the publisher-hashed RIPE NCC daily
  archive `https://ftp.ripe.net/rpki/<ta>.tal/YYYY/MM/DD/roas.csv.xz`.
- Tor: `https://check.torproject.org/exit-addresses` (TorDNSEL `ExitAddress`
  lines, the measured egress) and the bulk list; CollecTor archives are CC0.
- Operator-published egress geofeeds: Apple iCloud Private Relay
  `https://mask-api.icloud.com/egress-ip-ranges.csv` (about 285,000 rows) and
  Cloudflare WARP `https://api.cloudflare.com/local-ip-ranges.csv`, both RFC 8805
  CSV, usable as `vpn` lists after review.

Assessed and deferred, with the reason:

- Cloud prefix publications (AWS `ip-ranges.json`, Google `cloud.json`, Azure
  service tags, Oracle, DigitalOcean/Linode/Vultr geofeeds, Cloudflare and
  Fastly edge lists) are authoritative hosting evidence but only change a
  Quality outcome where a cloud prefix is originated by a reviewed subscriber
  ASN; unknown space is already excluded. Worth ingesting as `hosting` prefix
  evidence once the catalog's positive coverage makes that overlap measurable.
- Spamhaus DROP/ASN-DROP (free, credit required) is a small high-precision
  negative list; add when a `hosting`/`proxy` address-list category policy is
  reviewed for it.
- APNIC Labs per-ASN user estimates (`https://stats.labs.apnic.net/cgi-bin/aspop?cc=XX&ff=2`,
  attribution required), bgp.tools `asns.csv` class and `dsl`/`mobile`/`vpn`/`vpsh`
  tags, PeeringDB `info_types` (operational use only, no bulk redistribution),
  Stanford ASdb (bulk download now login-gated; per-ASN API open), Cloudflare
  Radar top ASes, and Steam per-country network rankings are discovery inputs
  for the review queue. APNIC counts spuriously credit VPN egress ASNs and are
  weak in RU, BR, JP, PL, KR and CN; none of them is reviewed subscriber proof.
- CAIDA AS classification is retired with download access removed. Rapid7 and
  OpenINTEL reverse-DNS datasets are access-gated; reverse-DNS naming
  heuristics would require our own PTR resolution and misclassify static
  business subscribers.
- Regulator joins beyond Brazil: LACNIC RDAP registrant handles are national
  legal identifiers (CNPJ confirmed for BR; NIT/RFC/CUIT/RUT to verify), which
  can bridge Colombia's Postdata `ID_EMPRESA` and similar files; JPNIC publishes
  an ASN list; FCC BDC, CRTC, Ofcom, ARCEP, BNetzA, AGCOM, CNMC, CRT and TRAI
  publications carry no ASN and need name bridges.
- MaxMind Enterprise/Anonymous IP/Residential Proxy, IPinfo and ipapi remain
  licensed inputs with the handling described in the matrix below.

### Measurements

All figures are from the 2026-10-04 10:03 UTC RIS IPv4 dump, the deployed
GeoLite2 release and today's Cloudflare RPKI export.

| Measurement | Value |
| --- | --- |
| Prefix/origin pairs; seen by one peer; by two | 1,270,431; 68,342; 28,329 |
| Address space in standalone routes under 10 peers, with no covering route | 0.65% |
| Address space in routes under 10 peers whose covering route has a different origin set | 0.34% |
| Routes under 10 peers that inherit an identically originated visible aggregate | 82,426 |
| Routes RPKI-invalid; of which inside a valid identically originated aggregate | 42,355; 24,200 |
| Sample of 127 eyeball ASNs: routed IPv4 outside the stated country, excluding the two misidentified entries | under 1% each; Orange FR 3.2% (overseas departments), Sky GB 9.3% (Ireland) |
| Misidentified sample entries flagged by the audit | 2 of 2 |

The two misidentified entries were AS36874 and AS37282, stated as NG and KE and
routed 100% in ZA and 89% in NG. The sample catalog is a measurement fixture,
not a reviewed catalog.

### Builder changes

`augment-subscribers` now reads peer counts, resolves visibility and RPKI
validity with aggregate and sibling-ASN inheritance, withholds the positive
inference below `minimum_origin_peers` (default 10) or for an invalid origin,
withholds it at GeoLite cells outside the identified operators' reviewed
countries under `origin_country_policy` with `--geolite2`, and applies
`address_risk_sources` at exact addresses with independent risk. Withheld
decisions preserve the base state and record the identity, peers, validity and
reason. The manifest binds every new input and counts withheld partitions by
reason, RPKI validity and applied address entries. `audit-subscriber-catalog`
produces the per-operator consistency report and per-country review queue. The
record schema remains `classifier_version: 1`, `quality_policy_version: 2`;
the runtime decoder ignores the additive fields and needs no change. Existing
catalogs build unchanged except for the default visibility floor, which the
manifest records.

The follow-up the same day made the catalog revision mechanical.
`refresh-subscriber-evidence` pins all five public snapshots, and the two
opt-in relay geofeeds, into a validated catalog in 16 seconds. The audit
gained NRO registry-holder sibling detection and nested-route merge
candidates: on the measurement fixture it surfaced 55 unreviewed Comcast
regional ASNs under one ARIN holder, Airtel's four unlisted APNIC siblings,
and 55 merge candidates led by Charter (AS22394 under AS6167, 3,818 routes),
AT&T (AS20057 under AS7018), KT/LG (AS3786 under AS4766) and the Orange
ES/Jazztel nesting (325 routes, 311 below the floor, no shared holder because
they are distinct RIPE holders). These are catalog suggestions; the build
reads neither registry data nor merge candidates.

A third pass validated the data with independent methods. Cross-checking
bgp.tools against APNIC showed why neither is evidence alone: APNIC credits
users to 807 of 2,256 VPS-host ASNs and 306 of 770 VPN ASNs. As audit-only
label sources they gave the 127-operator sample 106 agreeing and 20 mixed
subscriber verdicts and one unlabeled (Jazztel, whose users APNIC attributes
to the Orange ES aggregate), and an eyeball queue ranked by users instead of
address weight. Measuring the deferred cloud publications against routing
overturned their deferral: 52 AWS EC2 Wavelength prefixes are originated by
Verizon Wireless AS6167 and an Oracle block by Cox AS22773, so they are now
prefix-scope hosting evidence. The same data showed Vultr's geofeed listing
6to4, Teredo and documentation space, so geofeed readers skip non-global
entries. A full augmentation with every evidence family applied 16,265 cloud
prefixes and 436,929 address entries in 188 seconds, and readback showed both
Verizon Wavelength blocks excluded with their origin identity retained.

### Evidence expansion, ground-truth validation and the update command

A third pass, the same day, measured every candidate source against real data
before adopting it, added a prefix-level discriminator, validated the finished
database against a held-out reference, and made the whole refresh one release
command.

| Measurement (2026-10-04) | Result |
| --- | --- |
| Label sources against 1,978 Linnaeus hand labels (300 pure eyeball, 160 pure hosting): eyeball precision and recall | bgp.tools 0.961 / 0.49; ipverse 0.942 / 0.70; Linnaeus held out 0.927 / 0.41; ASdb 0.823 / 0.85; APNIC 0.802 / 0.76 |
| Two agreeing sources with none contrary; three | 0.969 / 0.73; 0.988 / 0.56 |
| RIR assignment objects scanned (RIPE, APNIC, AFRINIC) | 6,739,442 |
| Share under hosting origins: DATACENTER, DEDICATED, VPS, HOSTING, CLOUD | 0.955, 0.929, 0.907, 0.876, 0.861 |
| Share under hosting origins: ADSL, PPPOE, GPON, DOCSIS, RESIDENTIAL, BNG | at most 0.003 |
| Hosting-named objects inside eyeball origins judged hosting on review | 42 of 45 |
| Hosting-named objects kept, and nested non-hosting objects overriding them | 109,833; 32,778 |
| VPN operator server addresses (Mullvad, NordVPN, PIA, Windscribe); inside inferred subscriber space | 10,861; 26 |
| Consolidated operator geofeeds in identified space; decisions they would change | 117,903; 54 |
| Atlas validation of the 129-operator fixture build: clean precision | 99.05% (98.61-99.35) of 2,734 networks |
| Datacentre clean rate; residential recall | 0.85% of 3,045; 39.5% of 6,865 |
| Residential Atlas networks lost to registry withholding; to visibility; to country; to hosting prefixes | 0; 9; 25; 5 |

The 26 datacentre false approvals are mostly RIPE Atlas anchors that access
ISPs host inside their own networks, which identity inference cannot see. The
residential recall reflects the fixture's 129 operators: 4,116 of the missing
networks have no identified operator at all. An independent research pass
found that RIPE registry keywords agree with Atlas tags at Cohen's kappa 0.90
where decisive, a PTR lexicon reaches 95-99% residential precision at a third
of the recall, and leased space, about 6% of routed IPv4 prefixes, is the main
remaining path for hosting inside eyeball-registered blocks; lease inference is
the next deferred technique.

Two real-data defects surfaced only against live sources: ASdb lists a reserved
AS0 row, and the Linnaeus splits file has a third split named "test". Both made
strict pinning fail until fixed, which is why `arindbctl update` pins
everything except GeoLite2, ARIN and the RIS routing snapshots best effort and
names what it left out. Update now runs from `build/all/run.sh`; without a
reviewed catalog at `config/<env>/arindb-subscribers/catalog.yml` it publishes
the registration database alone, as `refresh` did.

Package, race and vet checks pass. The real-data audit over today's table ran
in 20 seconds for 129 operators. A full augmentation of a local policy-two base
built from the 2026-02-18 ARIN snapshot (6,843,456 base leaves) with the
129-operator measurement fixture, today's RIS, Cloudflare RPKI and Tor exit
snapshots and the reviewed-country policy completed in 178 seconds (a later
measurement put the augmentation's peak resident set near 20 GB, of which a
routing-only run already uses 19.5 GB; an earlier 5.4 GB figure was a mid-run
sample): 7,269,483 emitted partitions, 3,867,522 inferred, 2,317
withheld for visibility, 60 for an invalid origin, 4,216 cells withheld outside
reviewed countries, and 1,398 Tor exit addresses excluded with risk. Spot
readback confirmed a Tor exit excluded at its /32 with the adjacent address
unchanged, a Cox /24 geolocated in GB and an Akamai on-net block inside Telkom
Indonesia withheld with their identity recorded, the single-peer `3.0.0.0/8`
announcement never inferred, and ordinary Comcast and Free addresses inferred
with their peer counts and validity. This is a local builder qualification with
a measurement fixture; no production catalog, resource or Main selection was
changed.

## 2026-10-04 subscriber coverage correction

The Root-owned Main receipts supersede the historical inactive-candidate
statements below: policy two was selected at 03:57 UTC, and the 04:28 UTC native
readback found zero Quality supply on all eight Taskworkers. The selected
catalog's affirmative scope was only Google Fiber and Webpass. Successful
policy enforcement does not establish adequate subscriber coverage.

The later 2026-10-04 user policy supersedes the original direct-allocation-only
requirement: an **identified subscriber ISP defaults clean when there is no
additional discriminator**. Missing child-registration or service-purpose detail
alone does not exclude an identified ISP. Explicit hosting, transit, proxy,
virtual-ISP, conflicting-use and independent risk findings remain effective.
An unidentified origin is a review backlog, not an identified subscriber ISP.

Quality must support subscriber access globally, including the long tail of
operators. Research up to 30 actual subscriber operators per state/province
within every country. Deduplicate entities and record actual service footprints,
ranking dates, metrics, ties and incomplete coverage; a national ranking does
not establish a regional rank or footprint. Country/provider coverage and
official operator rankings prioritize research; neither is an eligibility
cutoff. Record fewer than 30 where fewer operators are evidenced, and mark
unverified ranks and footprints explicitly.

The `augment-subscribers` command joins reviewed operator identity/use evidence
to a fresh RIPE RIS origin-prefix snapshot across all RIRs. RIS establishes
observed routing origin, not subscriber use or market rank. The resulting
approvals carry `subscriber_evidence_kind: isp_inferred`, preserve registration
metadata and cannot clear existing exclusions, conflicts or independent risk.
All origin routes participate: a narrower unidentified origin blocks a broader
ISP inference; mixed known/unknown origins are ambiguous; an explicit negative
origin wins. See [the catalog contract](SUBSCRIBER-ORIGINS.md) for reproducible
inputs, audit fields, the clean-default distinction and remaining limits. ARIN
referrals still cannot manufacture foreign customer registration authority;
global inference uses the separate reviewed identity and routing evidence.

Policy-two rules may now use `allocation_scopes`, each containing an exact
`net_handle`, `org_handle` and canonical full allocation `prefix`. Such a rule
must explicitly approve subscriber access and cannot also contain unscoped
organization, name or prefix selectors. The builder requires every tuple in
the authoritative input and rejects missing, transferred, resized or referral
records. The scope applies only to that allocation: separately registered
children, even children with the same organization, need their own review for
this exact-allocation approval. The separate identified-ISP inference can
qualify an unreviewed child under the later user policy above.
Incomparable-owner disagreement and direct-use contradictions remain excluded;
geographic and proxy/virtual-ISP risk remain independent vetoes. Exact-key
indexes avoid multiplying bulk classification work by catalog length.

The first candidate using this mechanism intersects Comcast's official
[dynamic-range publication](https://spa.xfinity.com/md/faqs/en/postmaster/comcast-dynamic-ip-ranges.md)
and [residential-use statement](https://spa.xfinity.com/md/faqs/en/postmaster/comcast-mail-errors.md)
with current Comcast registration blocks. It omits explicit VoIP registrations.
It does not approve the whole Comcast organization or apply a global prefix
override to unrelated reassignments. Source snapshots, retrieval clocks and
hashes must accompany the review. Rebuild against the complete pinned registry,
compare every changed output leaf and retained exclusion, then measure fresh
actual-provider overlap before Root publishes an increment. Registry address
weights and operator market shares are not provider supply. This increment
does not establish worldwide coverage or resolve Quality zero by itself.

The synthetic allocation-scope controls cover all addresses across a subscriber
pool, unrelated corporate use, unknown and same-owner children, inherited proxy
risk, geographic mismatch and incomparable registrations. Changed owner,
network, block size or registry authority invalidates build approval. Candidate
activation still requires the source-bound resource, fresh loaded-epoch
lookups, current-provider shadow, rollup and a complete native-index generation.

The complete pinned-source candidate finished at 06:02 UTC. Its full MMDB
readback compared both directions across 6,948,793 leaves: exactly 1,628 leaves
changed from unknown to subscriber, all under the reviewed Comcast allocation
rule. Total subscriber leaves increased from 493 to 2,121; every other record
field, including risk, stayed equal. The resource SHA-256 is
`8426869bd98e06711858bc6dd0eba3685e04eea032bcadbdcb56f471c057a2a0`.
This is an offline evidence check, not a provider count or a Main activation
receipt. Global inference and catalog coverage are separate successor work.

## 2026-10-04 07:08 UTC global candidate readback

The first reviewed global seed contains 44 subscriber operators, 48 subscriber
ASNs and 22 countries, plus four explicit hosting-origin operators. Every
emitted identity joins current official RIR ASN evidence to an official service
source; three pending seed identities are not emitted. This is an initial
catalog, not completion of the country/state/province top-30 research. Its
frozen catalog SHA-256 is
`46cee901d365d5a50c99cec0831c27268b6f4d121ba9b6b109de1d568c592a07`.
The APNIC estimated-user ASN queue remains research prioritization, not reviewed
subscriber identity or regional market rank. All regional rankings remain
unverified in this first frozen catalog.

The complete global augmentation finished at 06:54 UTC in 485.7 seconds. The
serialized candidate has 7,027,342 leaves: 2,920,041 subscriber, 3,894,465 unknown,
211,987 excluded and 849 ambiguous. Of these, 2,917,920 are new identified-ISP
inferences and 124,610 are new origin-use vetoes. The resource SHA-256 is
`c27a3e4904e00d8dd747f8c872126ece2a99b31c784b83c908ebdc1de04f6a49`,
with build epoch `1791096376`. The builder's manifest counts emitted partitions
before adjacent equal records are compressed, so those counts are larger than
the serialized leaf counts.

A complete two-direction output comparison passed in 264.6 seconds. Every new
approval had exactly the reviewed origin ASN/operator set and matching frozen
evidence source; existing registration metadata and independent risk values
remained equal. Only the candidate-to-base pass counts the actual candidate
state/address population: the reverse pass samples candidate state at each
base leaf's first address and checks preservation. Neither prefix counts nor
address-space weights measure live provider coverage.

The independent native decoder control on the qualified 6800 production source
passed 119 public registry/RIS samples, with 44 verified and 75 excluded outcomes
matching the candidate's epoch, state and risk. This was a narrow diagnostic
control using production files: the unrelated root-package generator preflight
prevented a full root suite. Focused/full/race builder suites and vet passed
separately. No Main contact or resource activation occurred in these checks.
Fresh actual-provider overlap, loaded-resource proof, rollup and native-index
Quality recovery remain Root-owned acceptance work.

## 2026-10-04 07:55 UTC expanded global candidate

The second frozen catalog contains 78 subscriber operators, 85 subscriber ASNs
and 45 countries, with the same four hosting-origin vetoes. Four unresolved
identity/service joins remain pending and are not emitted. Frontier AS5650 is
deduplicated into the stable Verizon identity using the current ARIN record and
Verizon's dated acquisition filing. Registry legacy names have explicit source
bridges; the LG Powercomm/LG Uplus continuity inference records that a separate
legal merger instrument was not captured. The catalog SHA-256 is
`9751341eb8c4eb8ac2a47451c6a45a9f37d0d33de382cb4455973cddb8f0e8f7`.
Independent review verified source hashes, joins, pending exclusions and entity
deduplication; this is not an independent semantic review of every identity.

The new full build completed at 07:47 UTC in 483.2 seconds. Resource SHA-256
`d8f26b416aa5bb72d7845850ad1e09c9cefa615d7d33a5fcd4bbc6b4f7191584`,
epoch `1791099583`, contains 7,054,846 serialized leaves: 2,939,274 subscriber,
3,902,447 unknown, 212,004 excluded and 1,121 ambiguous. The complete
bidirectional comparison passed in 276.2 seconds, including exact ASN/operator
attribution and preserved registration and risk values. New inferred approvals
number 2,937,153; origin-use vetoes number 124,610. Independent native decoding
on the qualified 6800 graph passed 187 public samples: 78 verified and 109
excluded. These are resource and compatibility checks, not provider counts.

Regional evidence now records 77 reviewed advertised-service operator/region
pairs in 46 US states, with all ranks unverified. The global administrative
inventory has 3,865 regions; no state/province top-30 research is complete.
The September 28 TRAI release adds dated Indian national broadband rankings
and observations for 22 telecom service areas. Cross-state service areas are
kept distinct from administrative regions. The handoff receipt under
`temp/arin-subscriber-coverage-20261004/global-candidate-handoff-v4.json`
binds the catalog, build, complete readback and native decoder evidence.
Current-provider shadow, publication, loaded-resource proof and native Quality
recovery remain Root-owned work. No Main contact occurred in this candidate
build or its controls; the earlier candidate remains frozen separately.

## 2026-10-04 09:40 UTC India expansion and complete v5 readback

Catalog v3 adds twelve reviewed Indian access operators and fifteen ASNs. Its
total is 90 subscriber operators, 100 subscriber ASNs and 45 countries, with
the four hosting-origin vetoes retained. The catalog SHA-256 is
`4894e396b142804cffa61c1090748d6e85b0d909e91a87bd037ca6f05da53b4e`.
Official TRAI August 2026 subscriber tables now join the reviewed operator
identities for metric-specific national top-five rankings. They do not establish
state/province ranks. Three additional advertised service footprints in India
bring reviewed presence to 80 operator/region pairs in 49 of 3,865 regions;
every regional rank remains unverified. Four seed identities remain pending
in this frozen catalog. Later research is a separate catalog revision.

The unchanged gated builder completed v5 in 463.7 seconds. Resource SHA-256
`0abe2281bf2f257a06735d6875e7e073c1422187637060fe54a615a8ba110be0`,
epoch `1791105668`, contains 7,058,446 serialized leaves: 2,941,220 subscriber,
3,903,856 unknown, 212,004 excluded and 1,366 ambiguous. The complete
bidirectional comparison passed in 280.0 seconds. All 2,939,099 new inferences
carry the exact reviewed ASN/operator/source attribution; 124,610 origin-use
vetoes remain, and registration metadata and independent risk match at every
compared address. Only candidate-to-base counts describe the serialized
candidate population; neither directional prefix count measures live providers.

The independent qualified-6800 native decoder diagnostic passed 206 public
origin samples, with 90 verified and 116 excluded outcomes. It compiled all
70 production files with the exact external test; an unrelated root-package
fixture preflight prevented using that package's TestMain. The handoff
`temp/arin-subscriber-coverage-20261004/global-candidate-handoff-v5.json`,
SHA-256 `13cb54ecee2d902b100b017e169053f373e6c219a9e141db16b6a91d5faffe53`,
binds the catalog, sources, output, complete readback and decoder control.
This is local resource qualification. Current-provider overlap, publication,
loaded-resource proof, rollup and native Quality recovery remain distinct
Root-owned acceptance work. No Main contact occurred in these checks.

## Original policy-two review history

The following records the original stricter review and rollout. The current
policy and Main selection status are the dated correction above.

The initial `config/main/arindb.yml` rules were reviewed on 2026-09-27. They
classify named cloud/VPS/hosting owners as `non_quality`, not as geographic
`risk`. These rules are a reviewed initial scope, not a comprehensive provider
directory or proof that every address of a diversified corporation hosts VMs.
The candidate must pass the shadow review below before activation.

The policy-two rule catalog is staged only in
`config/main/arindb-quality-v2.candidate.yml`. Active `main/arindb.yml` and the
all-release default refresh input are unchanged. Candidate builds and catalog
tests must name the candidate path explicitly. No resource was published.

Quality means residential-subscriber or business-subscriber access. The
2026-10-01 review makes that an affirmative requirement: unknown use, conflicting
ownership, hosting, CDN, transit, leased-address ambiguity and proxy infrastructure
are excluded from Quality. An excluded unknown is not asserted to be hosting.
The new `quality_policy_version: 2` is independent of the compatible
`classifier_version: 1` record format. Read the research and rollout requirements
below before building or activating this stricter policy.

Verified ISP-branded proxy infrastructure (the operational meaning of
`virtual_isp` here), proxy services, VPN services and Tor exits can additionally
carry an explicit `risk_category`. Their network-use exclusion is independent of
geographic risk and applies to Speed, Online and URL-probe admission too. A
legitimate wholesale-access reseller or MVNO is not automatically such a network;
its name, lack of owned last-mile plant, or ARIN ISP status is insufficient.

## Subscriber and proxy research, 2026-10-01

| Signal | Evidence and interpretation | Policy |
| --- | --- | --- |
| ARIN organization, assignment and ISP/LIR status | ARIN's [request guide](https://www.arin.net/resources/guide/request/) includes hosting, colocation, VPS and VPN within ISP services. Registry status identifies administrative relationships, not subscriber use. | Bind reviewed operator evidence to exact organizations or prefixes. Never allow all ISP allocations or names containing `residential`, `broadband`, or `business`. |
| ISP-branded/static residential proxies | [IPRoyal](https://iproyal.com/isp-proxies/) sells ISP proxies. [Oxylabs](https://oxylabs.io/pricing/isp-proxies) explicitly describes ISP registration on datacenter servers. | An ISP owner or consumer ASN cannot waive positive proxy evidence. Exclude verified virtual-ISP/proxy egress through independent network risk. |
| Residential proxy participation | [PacketStream](https://packetstream.io/) describes proxy traffic through participating household connections. Residential access and proxy participation can therefore coexist. | Positive subscriber evidence is necessary but cannot override a proxy finding. An operator rule covers its registrations, not every peer under unrelated consumer ISPs. |
| Anonymizer flags | [MaxMind's binary schema](https://dev.maxmind.com/geoip/docs/databases/anonymous-ip/binary/) distinguishes VPN, public proxy, residential proxy, Tor and hosting. Its residential-proxy flag does not cover peer-to-peer proxy addresses. | Use reviewed positives at the observed address/prefix. Hosting is Quality-only unless additional proxy/virtual-ISP evidence exists. Missing flags are not proof of subscriber access or absence of proxies. |
| Fresh residential-proxy sightings | [MaxMind's Residential Proxy feed](https://dev.maxmind.com/geoip/docs/databases/residential-proxy/) supplies provider attribution, confidence and last-seen dates, commonly at IPv4 /32 or IPv6 /64 scope; provider coverage is partial. | Highest-value next input for proxies hiding under real access ISPs. Preserve observed prefix, observation time, confidence, source generation and expiry. Stale, missing or low-confidence data must never promote unknown use to Quality. No paid feed is installed by this change. |
| Leased-address owner | [IPXO's operator instructions](https://www.ipxo.com/kb/technical-guides/adding-subnets-to-ipxo-from-arin/) explicitly bind its leasing marketplace to ARIN `IL-845`. | Exclude unresolved lessee/subscriber use from Quality. Leasing itself does not establish proxy risk; reviewed direct-child access can qualify independently. |
| Origin ASN, RPKI and IRR | [ARIN's RPKI documentation](https://www.arin.net/resources/manage/rpki/) describes authorization of origin ASNs for prefixes. That is routing authorization, not endpoint-use attestation. | Corroborate prefix authority and detect changes requiring review. RPKI-valid, an access ASN, or a small/large ASN is never enough to allow Quality or set proxy risk. |
| PeeringDB type and facilities | [PeeringDB's FAQ](https://docs.peeringdb.com/faq/) describes a database maintained by its participating networks. Presence or self-description does not identify the use of one subscriber address. | Review aid only; no admission from `Cable/DSL/ISP`, and no exclusion merely from an IXP/facility presence. |
| Geofeed and country agreement | [RFC 8805](https://datatracker.ietf.org/doc/html/rfc8805#section-3) calls for authority, accuracy and refresh review of self-published location data. | Country evidence refines geographic risk only. Matching country does not clear proxy risk or establish residential/business use. |
| DNS, latency, bandwidth and URL success | These measurements establish reachability and performance of the observed connection, not who supplies subscriber access. | Retain their existing independent gates; successful probes never turn an unknown or proxy network into Quality. |

These are policy inferences from the sources' defined scope, not claims that
registration or a commercial label proves each address's physical use. We do
not add broad consumer-ISP allows: a mixed ISP may lease prefixes or carry proxy
participants. The current affirmative catalog remains the already reviewed
Google Fiber identities; expanding it requires direct access-service evidence
for the exact owner/prefix, reassignment review and proxy evidence review.
Therefore this conservative policy can sharply reduce Quality coverage. It is
acceptable to return fewer Quality providers; Speed remains independently usable.

The initial additional catalog contains:

- `IL-909` — [ARIN IPRoyal identity](https://whois.arin.net/rest/org/IL-909.html),
  paired with its ISP-proxy product above: `non_quality: true`,
  `risk_category: virtual_isp`.
- `PL-1198` — [ARIN PacketStream identity](https://rdap.arin.net/registry/entity/PL-1198),
  paired with its proxy-network description: `non_quality: true`,
  `risk_category: proxy`.
- `IL-845` — [ARIN IPXO identity](https://whois.arin.net/rest/org/IL-845.html),
  paired with its exact-handle reallocation instructions: `non_quality: true`,
  no network-risk category.

Applying operator evidence to its exact registered infrastructure is the
conservative reviewed inference. These three entries are not a comprehensive
proxy directory. No claim is made that every IPXO lessee is a proxy or that all
PacketStream household exits are registered to PacketStream. Coverage outside
ARIN and proxies embedded in otherwise allowed access networks remain gaps
until authoritative and current address-level inputs are integrated.

## Additional-signal deployment matrix

These are the next inputs to evaluate beyond exact reviewed organizations. The
status column describes this source change, not a live deployment. “Virtual
ISP” is an ambiguous industry label: `virtual_isp` here specifically identifies
ISP-branded proxy egress, including static residential/ISP proxies hosted on
server infrastructure. It does not assert that every reseller, leased prefix,
business connection, MVNO or hosting ASN is a proxy.

| Signal | Authoritative availability | False-positive or coverage limit | Use and implementation status |
| --- | --- | --- | --- |
| Direct delegated organization | ARIN bulk Whois and RDAP provide the allocation/reassignment hierarchy; [ARIN's guide](https://www.arin.net/resources/registry/reassignments/) explains direct allocation versus reallocation/reassignment. | A registry relationship is administrative. Smaller delegations may not be reported; residential records have special reporting rules. A child can change use independently of its parent. | **Implemented:** direct-owner precedence, no inherited subscriber allow for unreviewed children, and conflict exclusion. Positive service evidence still requires review. Foreign-RIR/customer ingestion is deferred; ARIN referrals never manufacture that authority. |
| Reassignment and leased address space | The same registry hierarchy, corroborated by the lessor's own exact-handle instructions, is available now. IPXO explicitly names `IL-845` as its reallocation destination. | Leasing can serve legitimate access, business networks, hosting or proxies. The lessor does not establish every lessee's use. Unreported subleases can remain invisible. | **Implemented:** the IPXO identity is Quality-only excluded pending specific reviewed access. **Not implemented:** treating every lessor or its ASN as hard risk. IPXO evidence does not meet the proxy-risk criterion by itself. |
| Origin ASN | A current routing collector establishes observed origin; [GeoLite ASN](https://dev.maxmind.com/geoip/docs/databases/asn/) supplies downloadable IP-to-AS-number/name data. These have different authority and freshness. | A single ASN can carry consumer access, business access, leased blocks and proxies. The registered organization, origin operator and endpoint user can differ. | **Implemented** by `augment-subscribers` with pinned RIS snapshots, reviewed operator identities, a peer-visibility floor and reviewed-country withholding (2026-10-04). Neither ASN branding nor routing through an access ISP can allow Quality or clear risk. |
| RPKI/ROA | [ARIN RPKI](https://www.arin.net/resources/manage/rpki/) supports cryptographic prefix-origin authorization; a validated, time-bound VRP snapshot is deployable. | Route authorization proves neither subscriber use nor the absence of proxies. Invalid/unknown validation can also be an operational routing issue, not anonymizer evidence. | **Implemented** as a withholding discriminator over pinned rpki-client/routinator exports (2026-10-04). No proxy risk or subscriber allow follows from RPKI state; an invalid more-specific under the same operator's valid aggregate is not withheld. |
| Operator prefix publications | [AWS publishes JSON](https://docs.aws.amazon.com/vpc/latest/userguide/aws-ip-ranges.html); [Google distinguishes cloud customer ranges from broader Google service ranges](https://docs.cloud.google.com/vpc/docs/configure-private-google-access#ip-addr-defaults). Public TLS downloads are available. | AWS documents incomplete service coverage and missing BYOIP ranges. A broad corporate/service list is not equivalent to hosted customer egress, and cloud is not automatically proxy risk. | **Deferred ingestion; reviewed prefix rules already supported.** Prefer service-scoped cloud customer prefixes as Quality exclusions. Add input hashes, publication times, refresh/expiry, overlap checks and release diffs before automating; never infer clean access from absence. |
| Commercial user type | [GeoIP Enterprise](https://dev.maxmind.com/geoip/docs/databases/enterprise/) provides `user_type`, including `business`, `residential`, `hosting`, `consumer_privacy_network` and other classes. | A vendor's user label is not a guarantee of individual endpoint use. Residential and business access can also carry proxy traffic; omitted/unknown classes are ambiguous. GeoLite City does not supply this field. | **Recommended deferred licensed input** for broader affirmative coverage. Ingest exact prefix labels with database generation/hash and freshness limits; shadow false inclusions against direct-owner/use evidence and current proxy data before treating reviewed residential/business labels as supporting subscriber evidence. Never let a positive type override independent risk. |
| Commercial connection type | [GeoIP Connection Type](https://dev.maxmind.com/geoip/docs/databases/connection-type/) supplies `Cable/DSL`, `Cellular`, `Corporate` and `Satellite`; Enterprise also includes connection type. | Transport/access category does not establish that a particular endpoint is a residential or business subscriber, or exclude a proxy on the same access network. `Corporate` is not an automatic business-subscriber allow. | **Recommended deferred licensed corroboration.** Require licensed snapshots, freshness and missing-value handling; combine with reviewed user type/ownership and anonymizer evidence. Do not infer these fields from GeoLite location data, reverse DNS or ISP branding. |
| Anonymizer databases | [MaxMind Anonymous IP](https://dev.maxmind.com/geoip/docs/databases/anonymous-ip/binary/) documents distinct hosting, anonymous VPN, public proxy, residential proxy and Tor fields. | Its residential-proxy category does not cover peer-to-peer proxy networks. Hosting alone is not proof of a proxy, while missing flags mean unknown coverage. | **Deferred licensed input.** Specific positive VPN/proxy/Tor findings can populate independent risk; hosting remains Quality-only. The new risk categories/provenance can represent reviewed findings, but no commercial feed was purchased or installed. |
| Fresh residential-proxy sightings | [MaxMind Residential Proxy](https://dev.maxmind.com/geoip/docs/databases/residential-proxy/) provides observed prefixes, confidence, last-seen and provider attribution. | Coverage is partial; IP reassignment and shared address use make temporal and prefix scope essential. It must not be expanded to all upstream ISP customers. | **Highest-priority deferred input** for proxies using real access ISPs. Require a licensed snapshot, reviewable confidence threshold, exact prefix scope, generation, observation and expiry before activation. Missing/stale data cannot create an allow. |
| PeeringDB | [PeeringDB](https://docs.peeringdb.com/faq/) makes participant-maintained network, interconnection and facility metadata available. | Self-description and datacenter/IXP presence do not attest how a particular egress address is used. Legitimate access networks also colocate equipment. | **Deferred review aid only.** No automatic subscriber approval from network type, and no automatic proxy/hosting exclusion from facilities or peering. |

[MaxMind's product guidance](https://support.maxmind.com/knowledge-base/articles/anonymizer-and-proxy-data-maxmind)
distinguishes Anonymous/Anonymous Plus residential proxy flags from the newer
residential-proxy sightings product; the former does not cover peer-to-peer
proxy IPs. Pair user/connection-type evidence with the appropriate current
anonymizer/sightings input rather than treating a false flag as proof of absence.
No Enterprise, Connection Type, Anonymous Plus or sightings license/feed is
installed here. The current GeoLite-only static catalog cannot prove broad
subscriber quality.

The two initial proxy-operator rules do not provide comprehensive virtual-ISP
coverage. Independent address-level sightings are needed for proxy resellers,
leased exits and household peers registered under unrelated access ISPs. A
reviewed access rule establishes the necessary subscriber condition, not a
guarantee that no device on that network participates in a proxy service.

## Policy implementation

Every rule now requires explicit `non_quality`; omission cannot become an
accidental allow. Policy two emits `quality_state` as `subscriber`, `excluded`,
`unknown` or `ambiguous`, and only `subscriber` has `non_quality: false`.
Uncovered IPv4 and IPv6 receive explicit unknown default records. Unknown
organization children keep reviewed negative parent evidence but do not inherit
subscriber approval. A reviewed direct child or more-specific prefix can supply
positive access evidence. Contradictory rules for the same owner or equally
specific prefix are ambiguous regardless of file order. Incomparable owner
disagreement excludes Quality and
retains each owner's evidence; it does not invent hosting or geographic risk.

`risk_category` accepts `virtual_isp`, `proxy`, `vpn` or `tor` only on an explicit
non-quality rule. All matching positive risk rules accumulate independently of
Quality precedence. Their categories, reasons, exact matched organization
identities and sources survive in `network_risk_evidence`. The record stores
`geographic_risk` separately; final `risk` is geographic OR network-use risk.
Country waivers, unknown geography and subscriber overrides cannot clear a
positive network-use exclusion. This does not infer risk for ordinary clouds
or all leased addresses.

The runtime reader validates policy/state consistency. A real, versioned
subscriber lookup with no risk is persisted as `arin_quality_verified`; old
records, missing records and manual location overrides cannot manufacture it.
Migration 750 defaults that connection fact to false; migration 751 binds it to
each location write and revokes all pre-751 positives. The controller
keeps the database's raw `non_quality` flag separate from this new fact. With
explicit policy activation, rollup treats any active unverified connection as
non-quality. Activated Quality requests additionally
read bounded current-connection facts from the primary database for every
candidate, including explicit IDs, force-minimum, Speed borrowing and Online
fallback. Every live connection must qualify; a missing location also excludes.
This prevents stale cache/rollup evidence from admitting a new unknown address.
Disconnected or stale-handler history does not prevent current qualification.

Runtime activation is a separate `subscriber_quality_policy_version: 2` entry
in `provider.yml`. Absent/zero preserves legacy rollup and Quality fallback
semantics, so deploying compatible binaries does not silently empty Quality.
Malformed or unsupported policy values fail the rollup/request instead of
quietly downgrading an attempted activation. Both strict and default-off behavior
are regression-tested through scoped config fixtures, including a warm legacy
cache when activation switches on. The active Config tree does not enable it.

Promote only after exact candidate input/output attestations, a provider shadow
diff, sufficient affirmative coverage, the schema append, the new API fleet and
an FP2/database-load canary have passed. Then make the reviewed candidate the
explicit release input, acquire new Connect facts, refresh rollup/indexes, and
switch the provider policy gate in a coordinated release. Until those checks
pass, keep both candidate rules and activation setting out of the active inputs.

Candidate catalog validation uses explicit paths, for example:

```sh
ARIN_REVIEWED_RULES_PATH=/absolute/path/config/main/arindb-quality-v2.candidate.yml \
ARIN_SUBSCRIBER_RULES_PATH=/absolute/path/config/main/arindb-quality-v2.candidate.yml \
go test -race ./arindbctl
```

Migration 750 (index 749) alone is insufficient for mixed writers. Its false
default covers inserts, but an older `ON CONFLICT DO UPDATE` omits
`arin_quality_verified` and retains a prior new-writer `true`. A deterministic
schema-750 control reproduces that stale positive even when the old writer
changes the location, and the strict eligibility expression still accepts it.
The server `720e7c61` release retains this limitation; it does not include the
successor correction below and cannot establish the v2 activation guarantee.

Migration 751 (index 750) appends without changing any earlier identity. It adds
nullable `arin_quality_write_token` with no default, clears every existing
positive, and installs a row trigger in the same transaction/table lock. The
new writer supplies a fresh token with the complete location/classification
statement on every insert and conflict update, including repeated identical
facts. The trigger forces the boolean false on inserts without a token and on
updates with a missing or unchanged token. An invalidating update retains the
last token, so immediately replaying it cannot re-attest after invalidation.
The token identifies a write; it is not an authorization credential or a
historical replay ledger. Only a new lookup and a fresh write can re-attest.

This covers old server `0b8e758d` upserts, schema-750 writers that explicitly set
the boolean but know no token, and direct updates that do not participate in the
protocol. Even an identical or ancillary legacy update revokes the fact; value
equality cannot distinguish a new lookup from an old writer. Older statements
remain SQL-compatible. PostgreSQL runs the row update trigger on the conflict
path as well as the insert trigger on the proposed insert
([trigger semantics](https://www.postgresql.org/docs/18/trigger-definition.html)).
The live request guard and rollup continue reading the same boolean, so already
deployed readers also observe revocation. The append does no affirmative SQL
backfill. Its one-time reset scans the location table and updates positive rows
under the migration's table lock; qualify that lock duration and retained-row
volume on the intended database before production scheduling.

Use a successor release containing both migration 751 and the token writer.
Keep `provider.yml` absent/zero, apply 751 with that release's migration tool,
then deploy its serving/Connect/writer fleet. Successor startup refuses schema
750. Readiness checks only a lower bound: old binaries requiring 749 or 750 can
still report ready against 751, which does not establish policy readiness.
Do not use an old migration tool against the newer catalog or remove the
trigger for a binary rollback. With policy still off, old writers can overlap
the deployment safely but cannot establish positive subscriber coverage. Before
v2 activation, complete the guarded API fleet, capable writer fleet, actual
lookup re-attestation, coverage/shadow/load gates, and rollup/index refresh.

The October 1 successor qualification passes 27 selected roots in normal and
race modes, without skips, across server/model/controller/router; all four
packages pass vet. The tests cover 749/750 upgrades, the contaminated schema-750
control and reset, migration restart without repeated revocation, missing/null/
unchanged tokens, schema-750 inserts and upserts, identical and changed legacy
upserts, repeated new-writer re-attestation, an old transaction committing after
a new attestation, warm native/fallback/named guards, rollup, default-off policy,
and startup readiness. Removing only the trigger makes both new model controls
fail with a retained positive/live-join eligibility. The synthetic guard-plan
fixture also passes with 20,000 providers, 40,000 current and 200,000 historical
connections; this is not a production capacity or migration-lock approval.
Evidence is retained at
`/mnt/data/sn-testnet/astra-arin-mixed-writer-20261001/receipt.json` and
`SHA256SUMS`. Initial broad runs lacked a synthetic documentation-range override
and reached an absent GeoLite resource before coverage assertions; those logs
remain beside the final successful runs. No production database, release image,
active classifier resource or provider policy was changed.

A mixed pool containing old API binaries cannot enforce the new Quality
contract consistently, because old fallback code may still borrow non-quality
providers. Do not declare the policy active until every API serving Quality
uses the new guard and each intended subscriber connection has a real new-policy
lookup. A rollback to old APIs rolls back that policy guarantee as well. Once
activated, new APIs with legacy ARIN resources fail closed, so resource/provenance coverage
must be measured before traffic switches. The current narrow positive catalog
is a release hold: keep this candidate unpublished until a provider shadow diff
shows adequate reviewed access supply, or expand exact reviewed access scopes
before cutover. A near-empty Quality pool is not an activation success.

This change requires the new migration before the new binaries. Build and
shadow the exact policy-two resource first; compare subscriber/unknown/ambiguous
and proxy-risk cohorts separately. Deploy Connect to acquire real lookup facts,
then complete rollup and native-index refresh. After activation, legacy
connections will be excluded from Quality until positively reclassified. No SQL reconstruction of
raw IPs, automatic subscriber backfill, resource publication or Main activation
is part of the source change. Measure the additional bounded primary read in
Quality requests during the canary.

## Request-guard performance review

An opt-in disposable PostgreSQL test exercises the exact query against the
actual migrated indexes with 20,000 synthetic providers, 40,000 live and 200,000
historical connections, 239,000 locations and 128 current handlers. It includes
unverified and missing location rows. On the review host, warm-cache request
timings with activation enabled, including Redis membership, policy parsing
and database calls, were (other regression tests shared the host):

| Candidates | Bounded SQL calls | Median | p95 |
| ---: | ---: | ---: | ---: |
| 20 | 1 | 0.61 ms | 1.02 ms |
| 256 | 1 | 3.26 ms | 4.63 ms |
| 1,000 | 4 | 12.55 ms | 15.13 ms |
| 4,000 | 16 | 48.39 ms | 59.28 ms |

The original correlated aggregate repeated handler work per candidate and took
19.71 ms median for 1,000 candidates. The final query materializes each bounded
candidate/connection batch and joins handlers once per batch. `EXPLAIN
(ANALYZE, BUFFERS)` uses `network_client_connection_connected_client_id` and
`network_client_location_pkey`, without full connection/location scans. It
reads the small handler relation once per batch. Duplicate candidates and
already hard-excluded IDs do not incur another subscriber read. No new index
or fleet-wide request query is introduced.

These are synthetic warm-cache measurements, not production capacity evidence;
Main's load, connection multiplicity, cache misses and concurrent request rate
can change the cost. The added primary work remains a canary requirement. Check
FP2 latency, database CPU/buffers, candidates examined, and result shortfalls
before enabling the stricter policy broadly. Reproduce on a configured disposable
test environment with `ARIN_SUBSCRIBER_BENCHMARK=1 go test ./model -run
'^TestSubscriberGuardQueryPlan$' -count=1 -v`. The test prints aggregate timings
and plan shape only, and skips by default.

## Evidence and scope

Each YAML rule embeds the exact primary registration and operator sources.
The registration establishes identity; the operator documentation establishes
the hosting service. Applying that owner classification to its allocations is
the reviewed policy inference, rather than an assertion made by ARIN itself.

| Owner | Narrow matching scope | Primary operator evidence |
| --- | --- | --- |
| AWS | Six verified data-services handles and the anchored exact `Amazon Data Services, Inc.` name | [AWS network publication](https://docs.aws.amazon.com/vpc/latest/userguide/aws-ip-ranges.html) |
| AWS EC2 | Exact `AMAZO-4` registration, whose ARIN record explicitly identifies AWS/EC2 | [ARIN service attribution](https://whois.arin.net/rest/org/AMAZO-4), [EC2 virtual servers](https://aws.amazon.com/ec2/) |
| Oracle Public Cloud | Exact `OC-195` registration, not the Oracle corporate owners | [ARIN cloud identity](https://whois.arin.net/rest/org/OC-195.html), [OCI compute](https://www.oracle.com/cloud/compute/) |
| Microsoft | `MSFT` | [Azure virtual machines](https://azure.microsoft.com/en-us/products/virtual-machines/) |
| Google | `GOGL`, with a more-specific Fiber exception | [Google Cloud compute](https://cloud.google.com/products/compute) |
| Google Cloud customers | Exact `GOOGL-2`, whose registration explicitly identifies customer-cloud address use | [ARIN customer-cloud attribution](https://whois.arin.net/rest/org/GOOGL-2.html) |
| Alibaba Cloud | Exact `AL-3`, not an Alibaba brand substring or APNIC referral | [ARIN owner](https://whois.arin.net/rest/org/AL-3.html), [ECS virtual servers](https://www.alibabacloud.com/help/en/ecs/user-guide/what-is-ecs) |
| IBM Cloud / SoftLayer | Exact `IBMC-24` and `SOFTL`, not every IBM corporate allocation | [IBM Cloud registration](https://whois.arin.net/rest/org/IBMC-24.html), [SoftLayer registration](https://whois.arin.net/rest/org/softl.html), [IBM's SoftLayer infrastructure API](https://cloud.ibm.com/docs/virtual-servers?topic=virtual-servers-api-reference) |
| DigitalOcean | `DO-13` | [Droplets virtual machines](https://www.digitalocean.com/products/droplets) |
| Linode | `LINOD` | [Akamai compute instances](https://techdocs.akamai.com/cloud-computing/docs/compute-instance) |
| Vultr | `CHOOP-1` / The Constant Company | [Constant cloud compute](https://www.constant.com/products/cloud-compute/) |
| OVH | `HO-2` | [OVHcloud VPS](https://www.ovhcloud.com/en/vps/) |
| Hetzner | Anchored exact `Hetzner Online GmbH`, no guessed handle | [Legal identity](https://www.hetzner.com/legal/legal-notice/), [cloud servers](https://docs.hetzner.com/cloud/servers/overview/) |

The matching RDAP entities are linked from each rule. The six AWS identities
were checked against [ARIN's data-services search](https://rdap.arin.net/registry/entities?fn=Amazon%20Data%20Services*).
An example of why names/handles must be verified: `HOS-1` is
[Higher Order Solutions](https://rdap.arin.net/registry/entity/HOS-1), not Hetzner.
No generic `cloud`, `hosting`, `vpn`, or country substring is a rule.

Google Fiber's verified `GF`, `GF-231`, and `GF-238` registrations and anchored
exact name have `non_quality: false`: [ARIN](https://rdap.arin.net/registry/entity/GF)
and [the operator's home internet offering](https://fiber.google.com/internet/).
This explicit owner exception overrides a Google ancestor, but does not bypass
the independent reliability, risk, TLS, or URL-success gates. Unknown
organization children inherit a reviewed hosting organization ancestor.
The 2026-09-28 catalog additions did not cover independently registered network
children whose organization lacks `parentOrgHandle`. A separate 2026-10-03 UTC
builder regression demonstrates that gap and implements the originally
requested negative Quality fallback through `parentNetHandle`.

The fallback uses the nearest reviewed network owner's organization evidence,
and requires a fully containing authoritative ARIN allocation at every link.
It preserves the direct owner and exact network handle, publishing the inherited
rule, source, organization and `classification_network_handle`. Missing,
noncontaining, external-RIR referral, registry and unknown links stop the
search. A reviewed access ancestor stops an older hosting rule from being
resurrected but does not approve an unreviewed child. Direct reviewed children,
organization ancestry and most-specific prefix overrides keep precedence.
Incomparable-owner conflicts remain explicit.

This is the user's conservative Quality inheritance policy applied to ARIN's
documented [network relationship](https://www.arin.net/reference/research/bulkwhois/),
not a claim that the registry proves actual service use.
[Reallocations and reassignments](https://www.arin.net/resources/registry/reassignments/)
can represent independently operated downstream networks. Consequently this
fallback never transfers country evidence, independent proxy risk or subscriber
approval. Full MMDB tests cover those boundaries, including parent and child
country differences, proxy evidence, clean overrides, referrals and reversed
source order. The old near-name negative fixture is now truly unrelated: it
has no network-parent link; separate tests cover unreviewed actual children.

Every new record also retains `net_handle` independently of optional country
evidence mode. Earlier policy-two resources could omit it, so their owner
aggregates cannot claim exact current prefix coverage when that handle is
empty. These changes do not replace the staged candidate or active database;
resource rebuild, aggregate provider shadow and lookup-epoch cutover remain
required.

The initial rules omitted `AMAZO-4` (the earlier text misspelled it
`AMAZON-4`) out of concern about blanket Amazon classification. The 2026-09-28
review found that this exact registration explicitly identifies AWS and links
EC2; it and the explicitly named `OC-195` Oracle Public Cloud registration now
have quality-only rules. This is not a generic Amazon/Oracle brand match:
Amazon also operates [Leo consumer internet access](https://www.aboutamazon.com/what-we-do/devices-services/amazon-leo),
and the corporate `AT-88-Z`, `ORACLE-4`, and `ORACLE-4-Z` registrations are not
included in these additions. Their individual cloud prefixes need separately
reviewed evidence. Consumer cable/mobile/satellite owners, unrelated near-match
names, and whole external registries remain negative controls.

These additions correct catalog omissions; they do not change geographic risk
or automatically replace a deployed database. A subsequent resource build must
bind the new rules, review its classification diff, and observe a new Config
and connection-classification epoch. A quality-only change cannot alter the
eligibility of a row already excluded by risk from all buckets and URL admission.

The next 2026-09-28 review independently required `GOOGL-2`, `AL-3`, `IBMC-24`
and `SOFTL`. All four were absent despite existing Google/other cloud rules.
The permanent `TestReviewedArinMajorCloudOwners` fails the old rules on all
four exact-owner and four inherited-child cases, while existing cloud and
reviewed access controls remain healthy. The corrected candidate passes the
complete `arindbctl` race suite and the pure FP2 common/quality-only gate
control. This establishes a catalog omission, not its current fleet impact.

This is not complete worldwide cloud coverage. The current catalog does not
yet supply reviewed complete Akamai/CDN, Cloudflare, Tencent or other regional
cloud allocations; a service API listing only an account's addresses is not a
global feed. Official provider prefix publications are being reviewed as
separately versioned evidence, with source scope, hashes, freshness, overlap and
clean-access controls. Their absence, an external-RIR referral, or a high native
Quality gauge cannot by itself prove a particular live provider is misclassified.
No dynamic feed or third-party proxy list is activated by this change. An
unrelated customer's origin is not Cloudflare/Akamai infrastructure merely
because a CDN serves its hostname.

Multinational registration country is not proof of a user's physical location.
Geographic risk is computed separately from authoritative ARIN registration and actual
GeoLite2 prefix intersections. External-RIR referrals, registry administrative
allocations, reserved blocks, and unknown block types cannot supply customer
country. In particular no AFRINIC-wide exception is justified by ARIN referral
data. This initial input set cannot establish all non-ARIN customer ownership;
future authoritative data requires its own reviewed integration.

Equal-prefix allocations are not necessarily duplicate errors. A documented
`parentNetHandle` descendant supersedes its parent even when their ranges are
equal. Incomparable direct registrations remain together in `owner_evidence`;
the builder never uses source order, handle spelling, or update timestamps to
invent a unique owner. Only unanimous known authoritative country supplies
`registered_country` and geographic risk. Conflicting or missing country facts
remain unknown. Under quality policy two, differing network-use classifications
produce an ambiguous Quality exclusion; legacy policy retains its original
boolean consensus for reproducible comparisons.
The output records and manifest expose multi-owner and ambiguous-fact counts.
Malformed cycles or contradictory facts within the same network record still
stop publication.

## Reviewed prefix country evidence

The builder supports a separate, optional `country_policy_version: 2` input.
No production country rules or source snapshots are enabled by this support.
Unmatched prefixes retain the existing registration-country mismatch policy;
the hosting catalog and its inheritance remain independent.

This refinement is needed because ARIN's organization country describes
[registration](https://www.arin.net/reference/research/bulkwhois/#org-xml-elements),
and ARIN [does not maintain IP geolocation](https://www.arin.net/about/relations/law_enforcement/faq/).
MaxMind documents [legitimate cross-border registrations](https://support.maxmind.com/knowledge-base/articles/country-level-and-city-level-geolocation-maxmind).
A mismatch remains the selected baseline policy, but must not be described as
proof of malicious operation or physical geography.

`country_sources` contains reviewed local snapshots, each with `id`, HTTPS
`url`, relative `file`, lowercase `sha256`, `observed_at` and `expires_at`.
The rules file's directory is the configured read root: absolute paths, parent
escapes and symlink escapes are rejected. Sources must be nonempty regular
files no larger than 16 MiB. The build instant must be at or after observation
and strictly before expiry. Hashes are checked before and after generation and
included in `inputs_sha256`; malformed, missing, changed or stale evidence
fails publication. There is no implicit fetch or fallback when evidence fails.
Expiry does not silently reclassify an already published database; normal
freshness monitoring and replacement review still apply.

Each `country_rules` entry requires a unique `name`, canonical `prefix`,
`owners: [{net_handle, org_handle}]`, `source_id`, `reason`, and exactly one of
`country_codes` or nonempty `uncertainty`. Countries must be recognized country
codes; unknown and regional codes cannot be a complete country set. The owner
list must match all current direct owners of a containing authoritative ARIN
allocation. A broader prefix, changed owner, missing network, referral record,
or partial incomparable-owner set stops publication. An independently registered
child does not inherit its parent's country rule.

The builder intersects ARIN allocations, GeoLite country cells and all rule
boundaries before deciding risk. The most-specific matching country rule wins.
Equally specific assertions that disagree become `ambiguous`, with their
original evidence retained; they never become a union of countries. A complete
set excludes only a known associated country outside that set. Explicit
uncertainty, conflicting evidence and unknown GeoLite country do not invent a
geographic discrepancy. None of these decisions changes `non_quality`.

The normalized country assertion is reviewed policy input, not automatically
extracted from the snapshot. Review must establish that the snapshot supports
the precise prefix and complete country set or uncertainty. The builder checks
its bytes, freshness and registration binding; it cannot establish geographic
truth from a digest. For example, [AWS's prefix geofeed](https://docs.aws.amazon.com/vpc/latest/userguide/aws-ip-ranges.html)
accounts for Local Zones; parent-region names alone do not. AWS's documented
[`GLOBAL` ranges](https://docs.aws.amazon.com/vpc/latest/userguide/aws-ip-syntax.html)
can originate in multiple locations. Azure's
[whole-cloud service tags](https://learn.microsoft.com/en-us/azure/virtual-network/service-tags-overview)
do not identify one country. [RFC 8805](https://datatracker.ietf.org/doc/html/rfc8805#section-3)
requires authority and accuracy review and recommends periodic refresh.
No organization-wide or country-wide waiver follows from these documents.

`classifier_version: 1` continues to identify the explicit-boolean record
format understood by existing readers. Only refined records additionally carry
`country_policy_version: 2`, `country_evidence_state`, `credible_country_codes`
and `country_evidence`. They retain raw `registration_mismatch` and ownership
provenance. Existing readers consume the final `risk`/`non_quality` flags and
skip additive evidence; actual future record format version 2 remains rejected.
The manifest versions country policy and binds all source snapshots separately.
This compatibility does not authorize publication: actual provider binding,
source review and shadow membership review are still required.

## Verification and shadow review

Run the release-catalog controls against the exact config artifact:

```sh
ARIN_REVIEWED_RULES_PATH=/absolute/path/to/config/main/arindb.yml \
  go test ./arindbctl -run '^TestReviewedArin(Rules|MajorCloudOwners)$' -count=1
```

An independently reviewed owner-expectation document also detects a rule that
was omitted entirely; enumerating only existing rules cannot detect that error.
The permanent major-cloud control contains public ARIN policy handles and
synthetic addresses/organizations, never private provider IDs, real provider
IPs, hostnames or secrets. It requires the selected Config artifact through
`ARIN_REVIEWED_RULES_PATH` or an adjacent Config checkout; missing input fails
rather than silently skipping CI coverage. An optional broader offline
review input's version-one JSON contains `expectations` with `org_handle`,
`rule_name`, and `non_quality`. Run it against both the old and candidate rules
to retain the omission failure and corrected result:

```sh
ARIN_REVIEWED_RULES_PATH=/absolute/path/to/reviewed/arindb.yml \
ARIN_REVIEWED_QUALITY_EXPECTATIONS_PATH=/absolute/path/to/review/expectations.json \
  go test ./arindbctl -run '^TestReviewedArinQualityExpectations$' -count=1
```

The always-on synthetic `TestArinQualityCatalogAdditionKeepsRiskAndAccessOverrides`
checks every address across same-country and country-mismatch partitions before
and after an exact hosting addition, including unknown-child inheritance,
reviewed direct-access and narrower-prefix overrides, and an unrelated near-name
negative control. It does not fetch operator evidence or activate a release.

The build manifest binds the rule, ARIN source, GeoLite2, and output hashes.
Before replacing the runtime resources, validate both databases and record
aggregate classified-prefix counts, per-rule owner/child matches, registration
scope counts, and country transitions. Review newly risky and non-quality
provider cohorts and unexplained country losses, especially multinational
operators and inherited children. Do not print raw provider addresses or treat
the absence of a rule as proof of consumer access. Preserve the previous bundle
for rollback. Classification expansion needs this review again.

For these exact-owner additions, activation is a separate resource operation:
commit the tested rules and test together; build a new immutable ARIN MMDB
against the attested ARIN/GeoLite source hashes; inspect the old/new per-rule,
country and prefix/address diff; then obtain an actual-provider shadow at the
lookup boundary with current raw addresses kept in memory only. Catalog address
weights are not provider counts. Root/operator approval is required before
publishing that Config resource and cycling the owning Connect lookup
generation. Attest loaded resource bytes/build epoch, post-boundary connection
lookup coverage, the subsequent rollup and one complete native-index generation.
A YAML-only commit does not alter the currently deployed MMDB or any provider.

## Connection classification readiness

The database no longer stores raw connection IPs: only keyed connection hashes
remain. There is therefore no safe SQL backfill that can reclassify existing
connections. Classification runs in Connect with the current connection IP;
its successful facts are persisted by `SetConnectionLocation` and then folded
into the active-address rollup. Old default-false booleans are not evidence
that this code has run.

The migration adds nullable `arin_lookup_at` and zero-default
`arin_database_build_epoch` to each connection's location. A real MMDB lookup
stamps the actual loaded database's build epoch, including successful no-record
lookups. An explicit IP override, missing location, failed lookup, or legacy row
does not acquire this provenance. The timestamp and generation are operational
attestation only, not an additional serving gate. No raw address is persisted.

After the new resource is attested, deploy Connect to cycle connections as
approved, then run the location/reliability rollup. Call
`model.GetProviderArinClassificationCoverage(ctx, expectedBuildEpoch, cutoverAt)`
with the manifest-attested epoch and the chosen UTC rollout boundary. The
read-only result contains only aggregate counts of live public top-level
provider connections and providers. A provider is fully classified only when
**all** its live connections have a real post-boundary lookup against that exact
epoch. Unclassified and outdated connections must be understood before the
canary is declared populated; two false exception bits alone never pass this
check. Disconnects and stale handlers do not create artificial deficits.

The following two-release recipe documents the original 2026-09 classifier/URL
cutover. **Do not reuse it as a nonactivating subscriber-policy-two shadow on
the current fleet.** Current API/index code already consumes `arin_risk` and
`arin_non_quality`; replacing the active MMDB can change exclusions even when
the subscriber request-guard switch is absent. Policy-two shadow work must keep
the active reader/resource unchanged and mount an independently attested
candidate only for the separate default-off observer and aggregate reducer.
See `arinshadowctl/README.md`. A missing, stale or differently versioned lookup
is an unresolved comparison, not a known policy loss. Connection identities
must join exact current census lookup times, epochs and flags; arrival time is
not a fresh lookup. No actual Main candidate shadow or adequate subscriber
supply has been established by the local tooling controls.

For the historical provider-level shadow before enabling that policy, use two
immutable config releases after the required tests and commits. Migrate first.
Build stage A from the currently effective Main config, preserving
`provider.yml`, `provider_egress_probe.yml`, and `egress-sites.yml` unchanged;
overlay only the attested GeoLite2/ARIN databases, their places resource, and
additive classifier rules and `qualityprobe.yml` catalog. Old services may
restart broadly onto this compatible release, but retain their existing image
versions; the old API does not consume the new catalog. Deploy new Connect
explicitly and cycle connections, then compare aggregate new connection flags
against the old base/reliability gates. The old Taskworker does not populate the
new rollup flags, so this shadow must aggregate the per-connection location flags
across live addresses rather than trust default-false rollup columns. Require
the exact database epoch and post-boundary lookup coverage before interpreting
those counts.

Only after that review deploy the new API, then the new Taskworker, while stage
A still preserves the legacy probe settings. New Taskworker overlays those
settings onto its URL defaults: URL policy activates at this binary deployment,
not at the later config release. The initial pool is eight URL workers per
configured shard; it never resumes the old full/blackhole execution path. The
new API's catalog/evidence contract is therefore required first. Let old
Taskworker processes and leases retire through the normal deployment lifecycle,
without assuming authority to hard-kill them. Legacy submitters cannot earn
version-one ratio or quota credit, but overlapping old workers can temporarily
consume due claims without their new tokens. Verify old workers have exited,
the new rollup has populated ARIN flags and buckets, and URL progress is steady.
Then publish stage B with the final URL-only probe and index settings. This
keeps old workers away from the new YAML while making the activation boundary
explicit.

The existing ARIN reader at `9ac534ea` reads `org_country_codes` and skips unknown
fields; additive exception/provenance metadata does not activate the new gate.
The old publisher still uses its own admission code. This does not promise
identical location/ranking inputs after a refreshed GeoLite2 database.
`config-updater` only copies immutable version directories, but its run worker
normally restarts services for newer config. The deployed runners inspected for
this rollout do not implement the newer `config_restart=no` / `restart: false`
hold; do not rely on that flag to preserve their effective config. Attest both
image and config versions after each stage. In particular the old publisher's
legacy score overrides and its full/blackhole task config must not be replaced
under an old binary during the shadow interval. Prefix-only candidate counts
cannot substitute for the post-reconnect provider shadow.

Verify the subsequent rollup and bucket counts after reconnection. A known
generation is not by itself proof of classifier correctness: the input hashes,
reader validation, shadow review, and publication identity are separate release
prerequisites. Legacy URL-policy history also remains audit-only during the
independent eight-hour quality-evidence warmup.


The full current-owner shadow transport and exact operator activation sequence
are documented in [arinshadowctl/CAPTURE.md](../arinshadowctl/CAPTURE.md). The
protected runtime adapter keeps candidate resources separate, inventories
current and draining owners, and pins one start-current native generation
through healthy publication rollover. Its local full-population controls do
not establish Main subscriber coverage or authorize policy activation. Those
remain mandatory before the final active-resource and policy cutover.

## 2026-10-04 10:47 UTC global v7 qualification checkpoint

This checkpoint supersedes the earlier offline v4/v5 coverage counts, without
changing the historical Main observations above. The frozen v7 resource is
`temp/arin-subscriber-coverage-20261004/global-candidate-v7/arin.mmdb`, SHA-256
`2bca7f6f76bd0a099cfa8704c326efea4d3dfb9cd734a0350f10ff2a842fb150`,
505,506,195 bytes, build epoch `1791109251`. Its manifest hash is
`6b3518a452c1fed8a43b6262f1b9645904ff40191c0fa46e9370e3b46dd9be49`.
It uses the independently checked global catalog v5, SHA-256
`02e7ccf38fc57626f429918b8b58a375df468cdc05e4a7f55dd761cbdfa361a4`:
4,786 positive operators, 4,966 positive ASNs and 46 countries, with four
unchanged explicit hosting operators. Identified subscriber use remains an
inference; explicit contrary use, conflicting origins and independent risk
retain their vetoes. Legitimate access resellers/MVNOs and a diversified product
menu do not by themselves identify ISP-branded proxy egress.

Brazil supplies a scalable primary-source identity join: positive Anatel
fixed `INTERNET` subscriptions for a legal entity, exact CNPJ legal-root join
to NIC.br's ASN/OrgID publication, and current Brazilian ASN delegation.
This reviewed rule application qualifies 4,694 operators and 4,865 ASNs;
it is not a claim that every operator website was individually reviewed.
Eight foreign/global ASN associations await separate identity/scope bridges,
and 5,061 reported operators remain unmatched. Names, ranks and market size
are not identity join keys or admission cutoffs.

Independent Sol review streamed all 5,631,845 regulator rows, matched all eight
monthly totals, independently rebuilt the national and all 27 state top-30
arrays, and rechecked the legal-root/NIR/delegation join. The August 2026
ranking metric is 56,986,853 fixed `INTERNET` subscriptions, including both
natural and legal persons; dedicated lines, M2M and other product categories
are separate. These are metric-specific rankings, not combined fixed/mobile
market rankings. The gate is `sol-brazil-v5-gate.json`, SHA-256
`7ab1e15f9c8c45fd61d5a456d313421c4b1926b58c8f643fd643553cbe0f3464`,
in the same research directory. There are 7,299 reviewed operator/region
footprints across 78 of 3,865 indexed regions. Worldwide country/state/province
top-30 coverage remains incomplete.

Complete bidirectional v7 readback passed in 262.97 seconds. The authoritative
candidate-to-base pass counted 7,075,149 serialized leaves, 2,957,726 subscriber
leaves, 2,955,605 new inferred subscriber leaves and 124,610 explicit origin
negative veto leaves. Every compared address retained independent risk and
approval attribution. Prefix leaves and IPv4 address weights are not Main
provider counts; the opposite pass's first-address weights are not exact
candidate population counts. Summary SHA-256 is
`a480a1e155d472d7ccce3fa2e3fba53c99f8832dbed80d10f8caa163121baac3`;
full readback log SHA-256 is
`31ef1ae52c08c919f6cabc67b085cacbca2a85dd0aa057ce1979d67b6c1af9ce`.

The independent qualified-6800 native decoder control used all 70 production
Go files plus the exact external test, and passed 5,023 public origin samples:
4,696 positive and 327 excluded. It is a decoder compatibility control, not a
full root-package suite or a provider sample. Its gate
`sol-global-v7-native-gate.json` has SHA-256
`ce48eea69afbbb9bec9e02607ebad125f1b49de66c1e0e7be2f13b824db05964`;
its log hash is
`750d3827b0f719eb82aeba631322856a320a1851ff6c3e7374f6108ca9b1c87d`.
The actual catalog package control also passed (`global-catalog-full-v5.log`,
SHA-256 `2d02c2ee3404ad033dd26e39b77c9dc3f02fa0ea9fcc7cf4a95cf791def30c23`).
The pinned routing evidence expires no earlier than 2026-10-06 02:03:14 UTC.

This checkpoint records no Main contact, resource publication or activation.
Root owns the current Config identity check, separate shadow resource staging,
authenticated provider comparison, active resource selection and cached reader
rollover. Fresh lookup epoch/flags, rollup and a complete native Quality
generation remain necessary to establish actual recovery. Subsequent country
research remains separate from this immutable v7 release candidate.
