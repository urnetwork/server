# Reviewed ARIN classification and cutover

The initial `config/main/arindb.yml` rules were reviewed on 2026-09-27. They
classify named cloud/VPS/hosting owners as `non_quality`, not as geographic
`risk`. These rules are a reviewed initial scope, not a comprehensive provider
directory or proof that every address of a diversified corporation hosts VMs.
The candidate must pass the shadow review below before activation.

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
the independent reliability, risk, TLS, or URL-success gates. Unknown direct
children inherit a reviewed hosting parent as specified by the classifier.

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

Multinational registration country is not proof of a user's physical location.
`risk` is computed separately from authoritative ARIN registration and actual
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
remain unknown; differing hosting classifications cannot create `non_quality`.
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
  go test ./arindbctl -run '^TestReviewedArinRules$' -count=1
```

An independently reviewed owner-expectation document also detects a rule that
was omitted entirely; enumerating only existing rules cannot detect that error.
Keep actual owner identities in the explicit offline review input, not synthetic
test fixtures. Its version-one JSON contains `expectations` with `org_handle`,
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

For a provider-level shadow before enabling new serving policy, use two
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
