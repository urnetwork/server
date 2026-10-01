# IP database release refresh

The release runner builds and runs this tool natively on macOS or Linux.
Windows is currently unsupported by its Server dependency: the process log
scrubber uses Unix file descriptors and descriptor duplication.

`arindbctl refresh` downloads GeoLite2-City first, then ARIN organization and
network records, and builds one new bundle containing `mmdb/` and `arindb/`.
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
/tmp/arindbctl refresh \
  --geoip-config "$WARP_HOME/vault/mm-geoip.yml" \
  --credentials "$WARP_HOME/vault/arin.yml" \
  --rules "$WARP_HOME/config/main/arindb.yml" \
  --output /path/to/new-ip-database-bundle
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

The complete subscriber/risk proposal is staged in
`config/main/arindb-quality-v2.candidate.yml`. Active `main/arindb.yml` is
unchanged and remains the all-release default. Candidate builds must supply the
candidate path explicitly. Runtime enforcement additionally requires
`subscriber_quality_policy_version: 2` in `provider.yml`; it defaults off and
must remain off until coordinated promotion passes the coverage and performance
review described below.

`quality_policy_version: 2` requires affirmative reviewed subscriber access.
Unreviewed, uncovered, conflicting, hosting and ambiguous leased use are excluded
from Quality, with distinct `quality_state` metadata. An allow is not inherited
by an unreviewed child. Rules must explicitly supply `non_quality`; omission is
an error. The optional `risk_category` (`virtual_isp`, `proxy`, `vpn`, `tor`)
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
checkout and refreshes before constructing the config-updater image. Its input
overrides are `GEOIP_CONF_FILE`, `ARIN_CREDENTIALS_FILE`, and `ARIN_RULES_FILE`,
with the defaults shown above (`main` is the release's `BUILD_ENV`). Both
resources are installed under the same `WARP_VERSION`, then committed and
pushed together as scoped config changes. Missing inputs stop that release
refresh. Ordinary local service builds can still use their existing caches.

Offline verification:

```sh
go test ./arindbctl
go test -race ./arindbctl
```

The independent `geolite2 refresh`, `arin refresh`, and `build` subcommands
support operator-managed source acquisition and offline builds. They require
explicit paths and never silently fall back to older or unverified inputs.
