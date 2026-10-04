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
  --geolite2 /path/to/GeoLite2-City.mmdb \
  --output /path/to/new-subscriber-resource
```

`--geolite2` is required when, and only when, the catalog declares
`origin_country_policy`. Use the GeoLite release the registration base was
built with.

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
minimum_origin_peers: 10
origin_country_policy: withhold-outside-reviewed-countries
origin_sources:
  - id: ris-ipv4-20261004
    url: https://www.ris.ripe.net/dumps/riswhoisdump.IPv4.gz
    file: sources/riswhoisdump.IPv4.gz
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T02:03:14Z
    expires_at: 2026-10-06T02:03:14Z
rpki_sources:
  - id: rpki-client-20261004
    url: https://rpki.cloudflare.com/rpki.json
    file: sources/rpki.json.gz
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T15:31:47Z
    expires_at: 2026-10-06T15:31:47Z
    format: rpki-client-json
registry_sources:
  - id: nro-delegated-stats-20261004
    url: https://ftp.ripe.net/pub/stats/ripencc/nro-stats/latest/nro-delegated-stats
    file: sources/nro-delegated-stats.gz
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T16:00:00Z
    expires_at: 2026-10-06T16:00:00Z
    format: nro-delegated-stats
hosting_prefix_sources:
  - id: aws-ec2-20261004
    url: https://ip-ranges.amazonaws.com/ip-ranges.json
    file: sources/aws-ip-ranges.json.gz
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T16:00:00Z
    expires_at: 2026-10-06T16:00:00Z
    format: aws-ip-ranges-json
    services: [EC2]
    reason: AWS-published EC2 ranges, including Wavelength zones inside carrier networks
label_sources:
  - id: bgp-tools-asns-20261004
    url: https://bgp.tools/asns.csv
    file: sources/bgp-tools-asns.csv.gz
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T16:00:00Z
    expires_at: 2026-10-06T16:00:00Z
    format: bgp-tools-asns-csv
address_risk_sources:
  - id: tor-exit-addresses-20261004
    url: https://check.torproject.org/exit-addresses
    file: sources/exit-addresses
    sha256: REPLACE_WITH_EXACT_64_CHARACTER_SHA256
    observed_at: 2026-10-04T15:00:00Z
    expires_at: 2026-10-06T15:00:00Z
    format: tor-exit-addresses
    category: tor
    reason: measured Tor exit egress addresses from the official TorDNSEL export
operators:
  - id: stable-reviewed-operator-id
    name: Reviewed Subscriber Operator
    asns: [64500]
    usage: subscriber
    source: https://operator.example/subscriber-service https://registry.example/asn-identity
    countries: [US]
```

`minimum_origin_peers` is the number of RIS peers a subscriber-only route must
be seen by before it is inferred; it defaults to 10 and must be at least 1.
`origin_country_policy` is optional; its only value withholds an inference
where the associated GeoLite country is outside every identified operator's
`countries`. `rpki_sources` accept `rpki-client-json` (rpki-client and
Cloudflare `rpki.json`, integer or `AS`-prefixed ASNs) and `routinator-csv`
(routinator `csv`/`csvext` and the RIPE NCC daily archive, matched by the
`ASN`, `IP Prefix` and `Max Length` header names). `address_risk_sources`
accept `tor-exit-addresses` (TorDNSEL `ExitAddress` lines),
`rfc8805-geofeed` (first CSV column) and `address-list` (one address or prefix
per line), each with one reviewed category among `tor`, `proxy`, `vpn` and
`virtual_isp` and a reason. `registry_sources` accept the NRO extended
delegated statistics (`nro-delegated-stats`); only the audit reads them.
`hosting_prefix_sources` accept `aws-ip-ranges-json` and
`azure-service-tags-json`, each with a required `services` selection,
`gcp-cloud-json`, `oracle-public-ip-ranges-json` and `rfc8805-geofeed`, each
with a reason; non-global entries are skipped and counted.
`registry_assignment_sources` accept `rpsl` dumps (RIPE split files, APNIC
split files, the AFRINIC database), gzip or plain, bounded to 16 GiB
decompressed. `registry_sources` also accept CAIDA's `caida-as2org-jsonl`.
`address_risk_sources` also accept the VPN operators' own lists:
`mullvad-relays-json`, `nordvpn-servers-json`, `pia-servers-json` and
`windscribe-serverlist-json`. `label_sources` accept `bgp-tools-asns-csv`,
`bgp-tools-tag-csv` with its `tag`, `apnic-aspop-csv`,
`asdb-categorized-csv`, `ipverse-as-json`, `linnaeus-predictions-csv`,
`linnaeus-labels-csv` (ground truth) and `linnaeus-splits-csv`; only the audit
reads them. Pinned subscriber evidence may be up to 1 GiB per stored file. Every
snapshot may be gzip-compressed; the pinned file is hashed as stored, and its
freshness window is at most 48 hours. List every sibling ASN of one operator
under the same operator ID; the audit's merge candidates show where that was
missed.

## Updating and refreshing evidence

The release runs `arindbctl update --subscriber-catalog catalog.yml`, which
pins every evidence family below best effort, audits, augments and validates
in one bundle; see [ARINDB.md](ARINDB.md). Keep the reviewed catalog at
`config/<env>/arindb-subscribers/catalog.yml` so the runner finds it. The
strict command below pins evidence for a manual revision and fails on any
source.

```sh
arindbctl refresh-subscriber-evidence \
  --rules /path/to/previous/catalog.yml \
  --output /path/to/new-catalog-directory
```

The refresh downloads the RIS IPv4 and IPv6 dumps, the Cloudflare rpki-client
export, the Tor exit-address export and the NRO delegated statistics over
HTTPS without following redirects, bounds each body, validates it with the
same reader the build uses, stores the JSON and statistics gzipped under
`sources/`, and writes `catalog.yml` with the previous catalog's operators,
visibility floor and country policy and the freshly pinned stanzas. RIS
observation times are the dumps' own generation headers; the others are the
fetch time. Without `--rules` it writes an `evidence.yml` fragment instead.
`--registry-assignments` pins the RIPE inetnum and inet6num split files, the
APNIC inetnum and inet6num files and the AFRINIC database, stored as
published. `--vpn-servers` pins the Mullvad, NordVPN, PIA and Windscribe
server lists. CAIDA AS2Org is always pinned beside the NRO statistics.
`--hosting-prefixes` pins the AWS EC2, Google Cloud, AzureCloud, Oracle,
DigitalOcean, Linode and Vultr publications; the Azure file is found through
its stable download page, and only a `download.microsoft.com` link is
accepted. `--label-sources` pins the bgp.tools ASN list and seven tag lists
(with a descriptive User-Agent, as bgp.tools requires), the worldwide APNIC
user estimates, the current ASdb release, ipverse's metadata, and Linnaeus's
predictions, hand labels and splits from the commit that published release
202506. `--relay-geofeeds` additionally pins the Apple Private Relay and
Cloudflare egress geofeeds as `vpn` lists; include them only after reviewing
that category. The written file is reloaded and rehashed before publication, and
the manifest records every snapshot hash and the fetch time.

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
| Subscriber identity seen by fewer peers than the floor, without an identically originated visible aggregate | Unknown, withheld `insufficient-origin-visibility` |
| Subscriber identity whose origin is RPKI-invalid, without a valid identically originated aggregate | Unknown, withheld `rpki-invalid-origin` |
| Inferred subscriber whose GeoLite country is outside the operators' reviewed countries | Unknown at that cell, withheld `outside-reviewed-countries` |
| Inferred approval inside a most-specific RIR assignment named for hosting | Unknown at that assignment, withheld `registry-hosting-assignment` |
| Prefix in an operator-published cloud list | Excluded, or ambiguous over a direct reviewed approval; no risk |
| Exact address on a reviewed Tor, proxy, VPN or virtual-ISP list | Excluded with independent network risk |

A withheld decision records the identity for review and never changes the base
state, so a reviewed direct registration approval survives it. Vetoes are
never withheld. Each withheld reason is counted in the manifest.

An existing registration exclusion or actual conflict is preserved. A known
negative origin can veto an existing reviewed subscriber approval. Proxy,
virtual-ISP, VPN and Tor origin evidence also adds independent network risk;
hosting/transit evidence alone does not invent geographic or security risk.
No inferred approval clears any existing geographic or network risk.

Known origin decisions add `origin_use_state`, `origin_asns`,
`origin_operator_ids`, `origin_evidence_source`, `origin_peers` and, with RPKI
payloads, `origin_rpki_validity`; withheld decisions add
`origin_withheld_reason`. Address-level findings add
`address_risk_source_ids`. New positive inferences add
`subscriber_evidence_kind: isp_inferred` and the classification rule
`identified-subscriber-isp-default`. Direct allocation approvals retain their
existing evidence. Runtime policy version two remains compatible: `subscriber`
means the subscriber condition passed under the selected policy, including
the explicit inference. It is not proof that an individual device is free of
undetected proxy use; the independent risk and provider-health gates still apply.

## Catalog audit

```sh
arindbctl audit-subscriber-catalog \
  --rules /path/to/reviewed-operators/catalog.yml \
  --geolite2 /path/to/GeoLite2-City.mmdb \
  --output /path/to/catalog-audit
```

The audit reads the same pinned catalog, routes, payloads and geography as a
build and writes `catalog-audit.json` with a manifest binding every input. For
each operator it reports observed routes, routed IPv4 addresses and IPv6 /48
networks, the IPv4 share per associated country, the share outside the
operator's reviewed countries, routes below the visibility floor, RPKI-invalid
routes, routes shared with other reviewed operators, and ASNs that originate
nothing. An operator whose routed space is at least half outside its reviewed
countries is flagged `identity-review-suggested`; on the 2026-10-04 sample this
flagged exactly the two deliberately misidentified entries. With a registry
source it adds each operator's registry holder ids, sibling ASNs that are
unreviewed or listed under another operator, and `operator_merge_candidates`:
pairs sharing a holder, and pairs where one operator's routes are
more-specifics under the other's aggregates, with how many of those sit below
the visibility floor, and each operator's share of routed IPv4 space under
hosting-named registry assignments. With label sources it adds each operator's
independent sources, verdict and `corroborated` flag, a verdict tally by use,
`label_source_quality` and `catalog_ground_truth` when the Linnaeus hand labels
are present, and `unreviewed_eyeball_queue_by_country`, which ranks unreviewed
ASNs with an independent eyeball signal by APNIC users and flags contrary
signals. The report also lists, per associated country, the
thirty unreviewed origin ASNs with the most routed IPv4 space. That queue is prioritization by address weight, which
over-represents hosting and transit; it is not a subscriber ranking and never
an approval.

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
coverage, immutable base metadata, source corruption, freshness, stale
augmentation rejection, visibility and validity withholding with aggregate and
sibling-ASN inheritance, reviewed-country withholding at GeoLite cell
boundaries, exact-address risk in every list format, both RPKI formats
including AS0 payloads, and the audit's flags and review queue. Independent full package, race and vet checks qualify
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
