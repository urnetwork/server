# Global identified subscriber ISP evidence

The 2026-10-04 user policy permits an identified residential or business
subscriber ISP to default clean when no additional discriminator is available.
This includes ordinary access resellers and MVNOs; lack of last-mile ownership
does not establish proxy use. Unknown child-registration details alone do not
veto that inference. Explicit hosting, transit, proxy, virtual-ISP and conflicting
evidence still excludes Quality, and geographic/network risk remains independent.

`augment-subscribers` applies this policy to an existing complete policy-two
registration database. It joins reviewed operator identity/use evidence to
[RIPE RIS's observed prefix-to-origin data](https://ris.ripe.net/docs/ris-whois/).
The origin snapshot supplies routing evidence across RIR regions. It does not
attest subscriber use, customer identity, geographic service footprint, or rank.
The output preserves registration facts and adds auditable origin evidence.

```sh
arindbctl augment-subscribers \
  --source /path/to/unaugmented-registration/arin.mmdb \
  --rules /path/to/reviewed-operators/catalog.yml \
  --output /path/to/new-subscriber-resource
```

The source must be a complete policy-two database without prior origin
augmentation. Always rebuild from that registration base when refreshing an
origin catalog: feeding a previously augmented resource back into the command
could preserve stale inferred approvals, so it is rejected. The output directory
must not exist. The command validates and publishes a new local directory;
resource upload, configuration selection and Main activation are separate steps.
The ordinary `refresh` command does not implicitly select an operator catalog.

## Reviewed input contract

The version-one YAML catalog has this shape. Placeholder URLs, hashes and the
example operator below are illustrative and must never be deployed.

```yaml
version: 1
policy: identified-subscriber-default
origin_sources:
  - id: ris-ipv4-20261004
    url: https://www.ris.ripe.net/dumps/riswhoisdump.IPv4.gz
    file: sources/riswhoisdump.IPv4.gz
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T02:03:14Z
    expires_at: 2026-10-06T02:03:14Z
operators:
  - id: stable-reviewed-operator-id
    name: Reviewed Subscriber Operator
    asns: [64500]
    usage: subscriber
    source: https://operator.example/subscriber-service https://registry.example/asn-identity
    countries: [US]
```

Each operator needs a unique stable ID, reviewed name, distinct public ASNs,
nonempty evidence source, known country codes, and one of `subscriber`,
`hosting`, `transit`, `virtual_isp`, `proxy`, `vpn` or `tor`. A diversified parent
company is not an automatic review of every subsidiary or ASN. Review identity,
subscriber service and each ASN-to-entity mapping. Country codes record operator
coverage context; they do not assert every route is located in those countries
or change geographic risk. Entries may share an ASN where evidence establishes
different uses; an explicit negative then wins. A corporate name search,
unreviewed APNIC traffic-estimate row, or PeeringDB category alone is not a
reviewed subscriber identity.

Keep a machine-readable evidence receipt alongside the catalog: source URL,
retrieval date, local snapshot hash, observation/ranking date where supplied,
exact identity/ASN claim, reviewer decision, caveats, and country/admin1
footprint/ranking status. The `source` field links that evidence; the build
manifest binds the exact catalog hash. Operator evidence needs periodic review
for acquisitions, use changes and reassignment. The builder checks input
structure and snapshot consistency; it cannot replace source review.

Origin inputs must be local regular gzip snapshots beneath the catalog directory,
with unique IDs, HTTPS provenance, exact SHA-256, and a freshness interval no
longer than 48 hours. Generation headers must also be within 48 hours of the
build. IPv4 and IPv6 snapshots can be included together. Missing, stale, future,
truncated, malformed or changed inputs fail the build. All input hashes are
checked again before publication. The manifest records catalog/base/source
hashes, generation, reviewed operator count, route count and emitted partitions.

## Decision and retained evidence

The longest observed origin prefix is evaluated, and equal-prefix origins are
merged independent of input order. Unknown origin routes are retained so a
narrower unrelated network does not inherit a larger ISP's identity. Default
routes never identify the entire address space.
IPv4-compatible/mapped, Teredo and 6to4 announcements are also ineligible for
origin inference: MMDB aliases those addresses to IPv4, and those observations
must not replace the separate native IPv4 table. The manifest distinguishes
observed rows from usable unique origin prefixes. The pinned 2026-10-04 IPv6
source includes 28 mapped announcements as well as other alias ranges; ignoring
them is explicit input normalization, not a missing service-purpose discriminator
for an identified ISP. Native IPv6 outside those aliases remains supported.

| Origin/use evidence | Result for an otherwise unknown base record |
| --- | --- |
| All observed origins identify reviewed subscriber ISPs | Subscriber, inferred |
| No observed origin has a reviewed identity | Unknown; enqueue for review |
| Subscriber identity plus an unidentified competing origin | Ambiguous |
| Any explicit hosting, transit, proxy or other negative use | Excluded |
| Missing child/service-purpose detail inside identified ISP | Subscriber, inferred |

An existing registration exclusion or actual conflict is preserved. A known
negative origin can veto an existing reviewed subscriber approval. Proxy,
virtual-ISP, VPN and Tor origin evidence also adds independent network risk;
hosting/transit evidence alone does not invent geographic or security risk.
No inferred approval clears any existing geographic or network risk.

Observed origins add `origin_use_state` and `origin_asns`. This includes unknown
origins, so a current-owner capture can identify the public routing networks
whose operator/use evidence needs review without exporting provider addresses.
Unknown origin metadata never changes a direct approval, exclusion, conflict or
risk finding. Known decisions also add `origin_operator_ids` and
`origin_evidence_source`. New positive inferences add
`subscriber_evidence_kind: isp_inferred` and the classification rule
`identified-subscriber-isp-default`. Direct allocation approvals retain their
existing evidence. Runtime policy version two remains compatible: `subscriber`
means the subscriber condition passed under the selected policy, including
the explicit inference. It is not proof that an individual device is free of
undetected proxy use; the independent risk and provider-health gates still apply.

## Country and state/province research coverage

Research up to 30 real subscriber operators for every state/province in every
country. Keep the country/admin1 inventory and review queue complete even when
no operator has yet been reviewed. Deduplicate legal entities and brands while
preserving source-backed service footprints. National operator rankings can
prioritize work but cannot be copied into every region as a verified local rank.
Record metric, period, source, ties, lower-bound or estimated counts and rank
confidence. Where fewer than 30 are evidenced, record the real count. A gap in
the administrative-region index must be explicit, not silently dropped.

APNIC estimated-user ASN observations can produce a country research queue;
they do not themselves identify reviewed subscriber operators or establish
state/province market share. Unknown ranks and footprints stay `unverified`.
Regulator subscriber/coverage data, official filings and operator service-area
records can qualify those claims. Country rankings are a research priority,
never a permanent allowlist cutoff: a reviewed smaller ISP uses the same
classification path.

## Qualification and limits

The focused controls cover missing child purpose, unknown origins, a narrower
unidentified origin, mixed-origin ambiguity, explicit hosting/transit/proxy
vetoes, same-ASN contrary use, retained geographic/network risk, global IPv4/IPv6
coverage, immutable base metadata, source corruption, freshness and stale
augmentation rejection. Independent full package, race and vet checks qualify
the implementation separately from each real catalog and full resource build.

Before selecting a new resource, compare the complete candidate to the pinned
base, inspect inferred approvals by reviewed operator and all retained/additional
exclusions, and use authenticated current-provider shadow evidence. Prefix and
address totals are not provider totals. Verify the loaded resource epoch/token,
fresh provider classification, rollup and complete native Quality generation.
Absence of additional discrimination is an intentional inference; undiscovered
proxy/hosting use and imperfect BGP visibility remain limits. Catalog omissions
are measurable review backlog and should drive subsequent batches. Offline
build success does not establish recovered Main Quality supply.
