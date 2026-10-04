# IP database release refresh

Current status (2026-10-04): policy two is selected on Main; earlier inactive
candidate statements in this document describe the original rollout sequence.
The active positive catalog is incomplete. See the dated coverage correction
in [CLASSIFICATION.md](CLASSIFICATION.md) before preparing a successor resource.
Identified subscriber ISPs now default clean in the absence of additional
discrimination; explicit negative use and independent risk remain exclusions.
The global [subscriber-origin command and catalog](SUBSCRIBER-ORIGINS.md)
implement that policy without treating country rankings as an eligibility list.
Since the 2026-10-04 classifier review the inference is withheld for routes
without global visibility or origin authorization and for geography outside an
operator's reviewed countries, reviewed address-level Tor/VPN/proxy lists apply
at exact addresses, `refresh-subscriber-evidence` pins the public snapshots
into a new catalog, and `audit-subscriber-catalog` checks that catalog,
including sibling-ASN merge candidates, before it is built. The release now
runs `arindbctl update`, which refreshes GeoLite2, ARIN and every subscriber
evidence source best effort, augments with the reviewed catalog, and validates
the result against RIPE Atlas; see [ARINDB.md](ARINDB.md).

The release runner builds and runs this tool natively on macOS or Linux.
Windows is currently unsupported by its Server dependency: the process log
scrubber uses Unix file descriptors and descriptor duplication.

`arindbctl update` is the release command: it downloads GeoLite2-City, then ARIN
organization and network records, builds the registration database and, with
`--subscriber-catalog`, pins fresh subscriber evidence, augments, audits and
validates, publishing one bundle whose `mmdb/` and `arindb/` become the config
resources. `arindbctl refresh` is the registration-only form: it builds one
new bundle containing `mmdb/` and a registration `arindb/`.
The bundle is published only after both databases validate. Existing output
directories are never replaced; failed work leaves the previous versions intact.

The ARIN request uses the official selective
[`orgs+nets.zip` bulk download](https://www.arin.net/reference/research/bulkwhois/),
not the archive containing points of contact. Approved ARIN Bulk Whois access
is required. The API key is read from a protected YAML file and is never a
command-line value or an error message. Redirects are refused. Use a replacement
for any previously exposed key; this tool does not rotate credentials.

```sh
go build -o /tmp/arindbctl ./arindbctl
/tmp/arindbctl update \
  --geoip-config "$WARP_HOME/vault/mm-geoip.yml" \
  --credentials "$WARP_HOME/vault/arin.yml" \
  --rules "$WARP_HOME/config/main/arindb.yml" \
  --subscriber-catalog "$WARP_HOME/config/main/arindb-subscribers/catalog.yml" \
  --output /path/to/new-ip-database-bundle \
  --timeout 2h
```

`geoipupdate` must be installed. The protected `vault/mm-geoip.yml` document
contains `account_id`, `license_key`, and an `edition_ids` list that includes
`GeoLite2-City`. The downloader converts it into an owner-only temporary native
`GeoIP.conf`, passes only its filename to `geoipupdate`, and removes it on
success, failure, or cancellation before publishing the database bundle. It
never stores the secret in the bundle or prints parser/updater diagnostics.
The ARIN credential document has one field, `api_key`; restrict
both credential files' permissions to their owner. Credentials are read with a 64 KiB limit,
compressed ARIN archives with a 4 GiB limit, and extracted XML with a 16 GiB
limit. The complete command has a one-hour default deadline.

Each database has a manifest with its output hashes and builder version. The
ARIN manifest additionally records the exact XML, GeoLite2, and classification
rule hashes, classifier version, and emitted partition counts. Inputs changing
during a build cause publication to fail. Registration countries retain the
existing parent-first/direct-owner-last representation. Missing country facts
do not imply a mismatch.

Classification rules must be explicitly supplied and reviewed. They require
`version: 1` and nonempty `rules`, each with a unique `name`, `reason`, `source`,
and organization or prefix criteria. More-specific prefix rules override
organization rules, including narrowly verified access-network exceptions.
Do not substitute a blanket organization-name heuristic for evidence-backed
hosting classifications. No production classifier or credentials are bundled
with the tool.

Reviewed policy-two subscriber pools can instead use `allocation_scopes` with
exact `net_handle`, `org_handle` and full source-allocation `prefix` tuples.
These positive rules cannot contain unscoped selectors. They fail the build
when their exact authoritative registration changes and do not approve child
delegations or the owner's unrelated allocations. Use this form when service
evidence covers only part of a mixed-use operator's holdings.

The selected `main/arindb.yml` is the all-release default. Successor builds must
supply their reviewed candidate paths explicitly; they do not change that
selection. Runtime enforcement also requires
`subscriber_quality_policy_version: 2` in `provider.yml`, selected on Main at
the dated checkpoint above. New resources still require the coverage and
performance review described below.

The registration builder requires affirmative reviewed subscriber access and
does not inherit an allow to an unreviewed child. The subsequent global origin
augmentation accepts identified subscriber-ISP inference under the latest user
policy. Missing child use alone is not an exclusion; unidentified, conflicting,
hosting and explicit other-use evidence remains excluded with distinct
`quality_state` metadata. Registration rules must explicitly supply `non_quality`;
omission is an error. The optional `risk_category` (`virtual_isp`, `proxy`, `vpn`, `tor`)
adds an independent hard exclusion whose positive evidence cannot be cleared by
an access or country override. See [the research and rollout requirements](CLASSIFICATION.md).

Organization rules apply parent-first: an unknown child inherits a reviewed
hosting parent's `non_quality` flag until a more-specific reviewed owner or
prefix rule overrides it. To allow a verified access ISP within a hosting
organization, name that child organization or a narrow prefix in a rule with
`non_quality: false`. A reviewed child overrides its ancestors, and the longest
matching prefix overrides organization rules. Under policy two, contradictory
rules for the same organization or equally specific prefix produce ambiguous
exclusion; legacy policy retains last-rule precedence for reproducible comparisons.
Each rule must carry its evidence source and reason. The manifest binds the exact
reviewed rule file; each record retains the direct owner plus the matched rule,
evidence source, reason, and inherited classification owner when applicable.
An independently registered network child can also retain negative Quality
evidence through a containing authoritative ARIN `parentNetHandle` chain.
Missing/referral/noncontaining links stop that fallback. Reviewed access stops
the chain without approving its unreviewed child; country and independent
network risk keep their original authority. Records always retain the direct
`net_handle` and add `classification_network_handle` for this fallback. These
additive fields require a new resource build and shadow review before cutover.

ARIN bulk data is not authoritative for other RIRs' customer registrations.
The builder retains `netBlock.type` and distinguishes direct ARIN records from
external-registry referrals, registry allocations, reserved space, and unknown
types. Administrative/referral countries cannot produce a geographic `risk`
flag: those records have unknown customer country, while authoritative child
allocations still override them. Legacy `AV` remains an ARIN registration;
unrecognized types are not guessed. Record metadata and manifest counts expose
this scope distinction. See ARIN's [block-type definitions](https://www.arin.net/reference/research/bulkwhois/#net-xml-elements)
and [authority limitations](https://www.arin.net/vault/blog/2021/06/09/arins-whois-what-data-is-public-information-and-how-can-it-be-accessed/).
AFRINIC-wide customer classification needs authoritative additional inputs;
registry membership or continent alone is not a negative classification.

The all-release runner builds this command from its selected versioned Server
checkout and runs `update` before constructing the config-updater image. Its
input overrides are `GEOIP_CONF_FILE`, `ARIN_CREDENTIALS_FILE`,
`ARIN_RULES_FILE` and `ARIN_SUBSCRIBER_CATALOG_FILE`, with the defaults shown
above (`main` is the release's `BUILD_ENV`); a missing default catalog yields a
registration-only resource, while an explicitly configured but unreadable one
stops the release. `ARIN_RELAY_GEOFEEDS=1` adds the relay geofeeds. Both
resources are installed under the same `WARP_VERSION`, then committed and
pushed together as scoped config changes. Missing inputs stop that release
refresh. Ordinary local service builds can still use their existing caches.

Offline verification:

```sh
go test ./arindbctl
go test -race ./arindbctl
```

The independent `geolite2 refresh`, `arin refresh`, `build`,
`augment-subscribers`, `audit-subscriber-catalog` and
`refresh-subscriber-evidence` subcommands support
operator-managed source acquisition, offline builds and catalog review. They require
explicit paths and never silently fall back to older or unverified inputs.
