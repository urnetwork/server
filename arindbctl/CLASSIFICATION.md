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

The initial rules deliberately do not blanket-match `AMAZON-4`: Amazon also
operates [Leo consumer internet access](https://www.aboutamazon.com/what-we-do/devices-services/amazon-leo).
Consequently AWS coverage is incomplete until additional specific owners or
prefixes have evidence-backed review. Consumer cable/mobile/satellite owners,
unrelated near-match names, and whole external registries are negative controls.

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

## Verification and shadow review

Run the release-catalog controls against the exact config artifact:

```sh
ARIN_REVIEWED_RULES_PATH=/absolute/path/to/config/main/arindb.yml \
  go test ./arindbctl -run '^TestReviewedArinRules$' -count=1
```

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
