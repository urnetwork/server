# FindProviders2 supply and quality-probe repair plan

Expanded offline subscriber candidate, 2026-10-04 07:55 UTC: the second reviewed
catalog covers 78 subscriber operators, 85 ASNs and 45 countries, preserving
the same four hosting vetoes and excluding four pending identities. Full build
and complete bidirectional attribution/risk readback passed; the independent
qualified-6800 native decoder passed 187 public samples (78 verified, 109
excluded). Candidate resource `d8f26b416aa5bb72d7845850ad1e09c9cefa615d7d33a5fcd4bbc6b4f7191584`
has epoch `1791099583` and 2,939,274 subscriber leaves, including 2,937,153 new
inferences. These are database populations, not live provider supply. The
frozen handoff is `temp/arin-subscriber-coverage-20261004/global-candidate-handoff-v4.json`.
Regional research records 77 service-presence pairs across 46 states; no
state/province top-30 ranking is complete. No Main activation or provider
overlap is claimed by these offline controls. Prior checkpoints remain below.

Current ARIN status, 2026-10-04: Root's Main receipts record policy two selected
at 03:57 UTC and zero native Quality supply across eight Taskworkers at 04:28
UTC. Earlier inactive-candidate statements below are historical. The subscriber
contract remains in force; expanding identified global subscriber coverage is
required to repair this supply loss. A source patch or catalog prefix count is
not evidence that actual provider supply has recovered.

Latest subscriber policy, 2026-10-04: an identified residential or business
subscriber ISP defaults clean when no additional discriminator is available.
Missing customer-registration or service-purpose information alone is not a
negative. Explicit hosting, transit, proxy, virtual-ISP and conflicting-use
evidence still excludes Quality; independent risk still excludes all public
buckets. Unidentified networks remain a review backlog. Research up to 30 real
subscriber operators per state/province in every country, with source-backed
service footprints and ranking provenance; unknown ranks or fewer evidenced
operators must remain explicit. This supersedes the earlier direct-positive-only
requirement, not the health, risk or provider freshness gates.

The isolated `augment-subscribers` builder implements this policy by joining
reviewed operator identities with fresh global RIPE RIS origin-prefix evidence.
Its new approvals are explicitly tagged `isp_inferred`; existing exclusion and
risk discriminators survive. A missing child use alone no longer rejects an
identified ISP. Focused and full builder tests, race tests, vet and an independent
qualified-6800 gate passed. The complete preceding Comcast allocation candidate
also built and passed full two-direction MMDB comparison: 1,628 additional
subscriber leaves with all other fields unchanged. Neither result establishes
Main provider overlap, adequate worldwide coverage or recovered native Quality.
Source and catalog details are in
[the classification correction](arindbctl/CLASSIFICATION.md) and
[the origin catalog contract](arindbctl/SUBSCRIBER-ORIGINS.md).

Offline global candidate checkpoint, 2026-10-04 07:08 UTC: the full reviewed
44-operator/48-ASN/22-country seed built successfully. Complete bidirectional
readback checked 7,027,342 serialized candidate leaves, including 2,917,920 new
identified-ISP subscriber inferences and 124,610 new explicit origin-use vetoes;
all prior registration metadata and independent risk values were retained.
An independent native decoder control matched epoch, state, risk and verified
status for 119 public registry/RIS samples. It is a narrow production-decoder
control, not a full root suite or a live-provider observation. The candidate
SHA-256 is `c27a3e4904e00d8dd747f8c872126ece2a99b31c784b83c908ebdc1de04f6a49`.
Main activation, provider overlap and native Quality recovery are unproved;
worldwide state/province operator research and regional ranks remain incomplete.

Latest user override, 2026-09-29: each eligible provider needs **ten total
accepted measured URL runs (success plus failure) in the rolling four hours**.
A completed turn with no accepted measured URL result does **not** count.
Setup failures, claims, abandoned work, security-only zero-total reports and
publication retries cannot manufacture quota credit. The previous success-only
target and its completion/rate interpretations are superseded. Dated checkpoints
below retain their original measurements and labels as history; a reported zero
ten-success census is not evidence of zero completion under this new target.
Bucket admission, eight-hour success-ratio ranking, common eligibility gates,
and independent same-URL TLS recovery are unchanged.

Status: implementation and Main validation in progress, 2026-10-02. This
document defines the quality-probe/indexing contract. The checkpoint below
distinguishes deployed changes from remaining production acceptance; code or
deployment completion alone does not establish the four-hour quota or CPU
recovery. IPv6 provider admission remains outside this rollout until the
provider binaries support it.

## Product contract

The subscriber-policy requirement, updated 2026-10-04, accepts reviewed direct
subscriber evidence or identified subscriber-ISP inference with no additional
negative discriminator. Unidentified, conflicting, hosted and proxy/virtual-ISP
use cannot qualify. Verified ISP-branded proxy infrastructure also receives an independent
risk exclusion; a legitimate access reseller or MVNO is not a proxy merely
because it lacks last-mile ownership. These requirements supersede the legacy
default-allow Quality behavior. Policy two is selected on Main; its expanded
catalogs require reviewed subscriber coverage, fresh provider shadow losses and
operator supply checks. No source edit or resource build establishes activation.

The index, the probe due queue, and the monitor must use the **same base
eligibility facts** for an active, connected, valid, publicly reachable provider.
Bucket admission and probe scheduling then apply the rules below. A
reliability failure or an ARINdb risk finding disqualifies a provider from all
three public buckets and from URL probing. The ARINdb
`non_quality` finding disqualifies it from **quality only**; it may still qualify
for speed or online. Under the target subscriber policy, missing ARINdb coverage
cannot qualify Quality; it does not manufacture a risk or hosting finding and
does not by itself exclude Speed or Online. The deployed legacy policy's
default-allow behavior remains a rollout fact, not the target contract.
A corrupt or unavailable entire database is a publication
error: retain the last verified database/index and alert, rather than silently
reclassifying the fleet.

| Gate | Quality | Speed | Online |
| --- | --- | --- | --- |
| Pass reliability | Required | Required | Required |
| No ARIN risk exception | Required | Required | Required |
| No probe security exception | Required | Required | Required |
| URL success ratio ≥ 0.8 | Required | Required | Not required |
| Direct or identified-ISP subscriber evidence and no quality exception | Required under policy two | Not required | Not required |

This matrix is the bucket admission contract. Probe admission has a different
purpose: every provider passing reliability and the ARIN **risk** gate is
eligible for paced URL probes. ARIN quality exceptions do not stop probes;
security-quarantined providers also need probes to establish recovery.

The probe records URL successes, errors, and security exceptions. There is no
separate blackhole definition or cheap-blackhole admission gate. The indexer
aggregates accepted measured URL outcomes in the preceding eight hours and
requires `success + error > 0` and `success / (success + error) >= 0.8` for
quality and speed. The threshold belongs to indexer configuration, alongside
reliability. Use the exact inclusive 4/5 boundary; 2/3 does not qualify. A URL's
final measured outcome is counted once after its retries, with idempotent
ingest so retries of a report do not bias the ratio. Zero denominators mean
online-only; at exactly eight hours an outcome is stale. A transport/setup failure,
missing result, rejected submission, or provider not contacted is **not** a
negative provider verdict. Such an attempt gets a bounded retry and a distinct
operator alert; it must not move that provider to a dark or failing bucket.
Attach an immutable evidence-policy version to every URL result. The indexer
and rolling-quota scheduler use the same explicit version selector, initially
version 1 for this contract. Preserve legacy/version-0 and future/unsupported
results for diagnosis, but do not count them as selected-policy success or
failure. Do not accept a missing or inconsistent success payload merely because
it claims the current version. URL-catalog generation and evidence-policy
version are separate: changing a URL list is not automatically a new definition
of success. During cutover, providers with only incompatible history are
online-only under the common gates until new measurements arrive. Existing
TLS exceptions remain independently unresolved; changing the selector must
never silently remove security quarantine.
Both quality and speed require qualifying recent URL history. All three
buckets require reliability, no ARIN risk, and no quality-probe security
finding. Online does not require qualifying URL history: ordinary reachability
failure must not independently remove a reliable,
risk-free, secure provider from online. A current TLS-authentication failure
is a hard quarantine until a valid authenticated TLS response from that same
failed URL, not something that ages into online. That response may clear the
URL's TLS exception even if its content or performance fails the quality test.
An unrelated URL's success cannot clear it. Apply the same security exclusions
and risk rules to explicit-client and
`force_minimum` paths so callers cannot bypass them.

Quality requests take native quality first, then native speed, then online.
Speed requests take native speed first, then native quality, then online.
Subscriber evidence applies to native Quality membership, including Quality
borrowed by a Speed request. A Quality request cannot carry that Quality-only
refusal into its lower Speed or Online tiers. Those borrowed rows keep their
own bucket's common security gates and lower client-visible priority. Explicit
provider IDs and force_minimum retain the explicitly requested Quality policy.
Read enough pages to fill the requested native limit after request filters,
or establish actual exhaustion of that native source, before borrowing from
the next bucket. Online rows cannot count toward a native quota. An initial
sample or exclusion-row allowance ending is not proof of exhaustion. Keep
individual reads, retained samples and publication storage bounded, using
native-only pages rather than repeatedly walking a large online union.
Deduplicate providers across buckets, preserve each borrowed provider's lower
client-visible tier, and apply request filters to *every* borrowed candidate.
Missing, incomplete, changed-generation or corrupt cache data is unavailable,
not an empty native source. A failed refresh retains the last complete usable
generation. If a native source is unavailable, retain independently validated
native rows, try the next native bucket, then use bounded same-target online
fallback so missing probe/cache evidence does not create a connectivity outage.
That is explicitly degraded native priority, not a claim of native exhaustion;
never manufacture zero supply or widen the requested target or security rules.
Healthy readable native sources still fill the requested limit or prove their
exhaustion before advancing. If complete native sources are exhausted, a large
reliable online pool must still answer. Online is a fallback
source containing all providers that pass the common gates, including members
of quality/speed; deduplication keeps them from appearing twice. Performance
metrics order eligible candidates rather than adding hidden bucket admission
gates. The URL-success definition below includes the explicitly requested
TTFB/throughput thresholds. Failing those thresholds is an ordinary URL error
in the ratio, not an additional exclusion from online.

The legacy publisher also applies a speed-score minimum to candidates logged
as `online` before publication. That hidden admission cutoff is removed by
this contract, but it is not sufficient to explain the observed app outage:
a bounded 2026-09-27 read still found over 100,000 candidates after those
legacy floors, and sampled IPv4 cache pages contained online backfill. Keep
database eligibility, actual cache publication, API-reader compatibility, and
request-specific filtering as separate acceptance boundaries. A pre-publisher
log count or a caller-country label does not prove the requested market has
usable answers.

The deployed baseline predating this repair does not implement this contract: `model/egress_index.go`
defaults evidence to the older provider-evidence lifetime and its legacy
`egressIndex == nil` path can admit unprobed rows to quality/speed. The due
queue in `model/provider_egress_location_model.go` starts with connected,
valid, Public-key providers but does not apply the proposed reliability and
ARIN gates. `model/network_client_location_model.go` already has the intended
quality/speed/online backfill order; preserve it while fixing membership.

## Metrics derived for ranking

The indexer computes admission and ranking from the same eight-hour URL
history and the existing connection-reliability lookbacks. Security exceptions
and ARIN flags are gates; they must not be averaged away into a good score.

| Metric | Derivation | Role |
| --- | --- | --- |
| Connection reliability | Existing connection reliability and independent-reliability weights per lookback | Common admission floor and ranking weight |
| URL successes `S` | Count of unique, accepted, measured URL successes newer than eight hours | Numerator and evidence volume |
| URL errors `E` | Count of unique, accepted, measured URL errors in the same window | Denominator and evidence volume |
| Measured URL count `N` | `S + E`; excludes unmeasured/local setup failures and duplicate reports | Distinguishes zero observations from observed failures |
| URL success ratio `r` | `S / N` when `N > 0`; otherwise unknown | Quality/speed gate at `r >= 0.8`, and ranking factor |
| URL ranking weight | Initially `0.1 + 0.9*r` for measured history; `1` when unknown | Multiplies existing reliability/performance selection weight; stays positive even for online providers with errors |
| Quality failure index | `ceil(MaxFailureIndex * (1-r))` for measured history; undefined when `N = 0` | Sample-count-normalized quality-tier penalty; more observations at the same ratio cannot worsen the tier |
| Relative latency | Existing measured relative latency; separately record DNS, connect, TLS, and TTFB timings for diagnosis | Performance ordering within eligible buckets |
| Throughput | Successful transferred bytes divided by transfer duration after first response byte | Speed ordering; DNS/connection wait is not transfer time |
| Evidence age | Measurement timestamps and their eight-hour expiry, not ingest/retry timestamps | Removes expired outcomes from the ratio; missing/stale measurements do not imply failure |
| Four-hour progress | Unique accepted measured URL results (success plus failure) strictly within the trailing four hours, capped at ten for coverage reporting; setup/no-result turns earn no credit | Rolling scheduling/coverage metric, not an additional admission gate; secure completion additionally requires no unresolved TLS exceptions |
| Four-hour scheduling count | Unique completed URL-probe attempts in the trailing four hours, including failures and zero-attempt providers | Lowest count first among currently eligible, paced-due providers; distinct from measured-run quota and eight-hour admission evidence |

For quality/speed, the initial selection weight is
`reliabilityScale * reliabilityWeight * performanceScale * urlRankingWeight`.
Retain the existing positive reliability/performance scaling floors and clamp
normalized factors to their valid range. For online, use
`reliabilityWeight * urlRankingWeight`, then the existing performance ordering.
Higher selection weights are better; a lower quality failure index is better.
Latency and throughput affect ordering and the explicitly configured URL-success
test below, not extra bucket cutoffs beyond the gating matrix. Unknown URL
history stays online-eligible with a neutral URL factor of `1`, as approved by
the user; this is not a fabricated success or failure ratio. Its URL factor is
intentionally the same as a measured ratio of `1`, although unknown history
cannot admit a provider to quality or speed.

Better measured URL success must never worsen ranking when
reliability, performance, and bucket are equal. A provider at exactly `4/5`
passes the URL gate for quality/speed and gets a URL weight of `0.82`; it must
still pass the other gates for that bucket. A `0/0` provider has no defined URL
ratio and can enter only online, subject to the common gates. Security or ARIN
risk failures cannot be offset by excellent reliability, throughput, or URL
success. ARIN quality exceptions exclude only quality, never speed or online.

Bucket priority precedes these within-bucket metrics: quality → speed → online
or speed → quality → online, with deduplication and preserved request filters.
The ten-run scheduling target is distinct from the ratio gate: ten accepted
measured successes or failures within the trailing four hours satisfy quota.
Ten errors can complete collection while leaving the provider online-only;
ten successes and ten errors in the eight-hour history give `r = 0.5`, so that
provider also remains online-only until its history meets the success threshold.
Ten measured runs are a collection target, not an additional quality/speed gate;
for example, four successes and one error already pass the URL ratio gate.
Three successes and two errors give `3/5` and fail the inclusive `4/5` gate.
Probing pauses only when the rolling quota is met **and** all TLS exceptions
have cleared; a quota-complete but quarantined provider still needs work.

### Reliability history mapping and publication freshness

The real writer's `ClientLookbacks` emits indices 0, 1 and 2, not 1, 2 and 3:

| Stored index | Actual source window | Current role |
| --- | --- | --- |
| 0 | 5 minutes | Lowest emitted index supplies independent/shared-IP normalized ranking weights; not the 0.95 hard gate. |
| 1 | 1 hour | Common admission requires observed independent weight at least 0.95 under the current normal-network branch. |
| 2 | 12 hours | Common admission requires observed independent weight at least 0.70 under the current normal-network branch. |
| 3 | Not emitted by the current writer | Existing 0.60 compatibility floor can still inspect a legacy row; no current six-day window is established by a commented-out source entry. |

A valid raw block means an established public-provide connection, at most one
unexcused new connection, no unexcused provide change, and at least one
received message. Connect counts zero-byte liveness pings as received messages.
This measures platform-connection availability, not Internet/DNS/URL success.
High connection-reliability pass rates can therefore coexist with poor URL
egress without establishing false positives in that connection metric. Native
Quality and Speed must still independently pass the selected-policy URL ratio,
and Quality additionally applies the reviewed non-quality classification gate.

With a pinned closed minute and no uncovered/degraded blocks, the writer's
inclusive endpoint convention yields 6, 61 and 721 blocks. Effective
denominators exclude uncovered and immutable platform-degraded blocks. A
denominator with no usable blocks intentionally retains prior scores. Missing
rows are currently neutral in the gate; absence must not be described as
measured perfect history. The writer-to-SQL/Go/request controls exercise real
raw-history and score writers rather than inventing index-3 score fixtures.

A score of 1 describes its stored interval, not necessarily the current
interval. The bounded 2026-09-28 due-head/tail sample at 18:35:55Z found sixteen
physical 0/1/2 score triplets, all weights 1, with stored denominators 6/61/721
and an endpoint about 71 minutes old. The repeat found the same global running
endpoint while the drain advanced; it did not preserve the same provider
identities across queue selections. A later bounded task read established a
49m51.842s successful writer run followed by its configured thirty-minute wait,
then a 7m05.762s successful successor. The 18:57:34 score read confirmed actual
client-score publication, with two sampled twelve-hour weights below 1. This
was not proof of current perfect reliability, a dead task, a representative
fleet pass rate, or a particular SQL stage owning all elapsed time.

The reviewed publication correction commits client running checkpoints and
their atomic three-window score publication before seven-day/network-window
work. Each score writer advances only the windows it consumes. Deterministic
real-transaction failures prove later network failure cannot withhold client
scores, while failed client maintenance or score writes cannot publish a
mixed/incorrect new generation. The completion-plus-thirty-minute schedule,
two-hour task ceiling, hard gate floors and evidence policy are unchanged.
Early publication alone does not guarantee a fresh five-minute window while
that singleton remains occupied by long later work.

Monitor §2.15a is implemented in source, with shared gates and watcher
activation still pending at this checkpoint. Its source detects old physical
sampled score endpoints despite
a fresh advancing drain: warning beyond forty minutes, page at sixty minutes,
both sustained twice. Active claims qualify task activity, not freshness;
missing/partial sources and zero-usable-denominator retention remain explicit
diagnostic limits. Gate admission must not be changed merely to make the
freshness signal green.

## ARINdb and GeoLite2 build

Move the standalone `arindb` tool into `server/arindbctl/main.go`, with
reusable classification/build code in the Server module. Make the release's
Server checkout own its build inputs and output schema. `arindbctl` needs
separate commands for:

1. `geolite2 refresh`: use the official GeoLite2-City updater and its protected
   MaxMind YAML in `vault/mm-geoip.yml`, stage a new MMDB, validate type/build date/sample lookups,
   generate the matching places export, and atomically publish that pair.
2. `arin refresh`: fetch the ARIN bulk source with a dedicated protected API
   key file, validate archive/XML completeness and provenance, and stage it.
3. `build`: correlate each ARIN network/organization with GeoLite2 using the
   same lookup semantics exposed by `server/ip.go`; build a versioned ARINdb
   with organization, prefix, registration country, observed country,
   classification, confidence, reason, and source versions. Validate the
   resulting MMDB and atomically publish it with a manifest/checksum.

`build/all/run.sh` must run **GeoLite2 refresh before ARIN refresh/build** on
every release build, from the versioned Server build checkout and before
config/service images are produced. Both versioned MMDBs and the places
export must be included in the release inputs together; reject a mixed or
missing pair. A failed download, validation, or build fails the release
before deployment, without overwriting the last good files. Provide an
explicit offline/reproducible mode using pinned, checksum-verified staged
inputs for rebuilds; it must never quietly use an unversioned old file.
Measure fetch/build duration so the release gate remains practical.

Credential separation matters. The earlier `root/GeoIP.conf` contains
**MaxMind** credentials, not an ARIN key. The user requested copying them into
`vault/mm-geoip.yml`; `arindbctl` and the all-build runner use that protected
YAML, with a short-lived native updater config kept outside published output.
Git history confirms the
old `xops/arin/update.sh` downloaded `arin_db.xml` from ARIN bulk Whois, but
`xops/MAIN-SETUP.md` records that its old API key was committed and identifies
it as exposed pending rotation/revocation. That is a recorded credential-risk
finding, not proof here of public disclosure, misuse, or provider-side
revocation; Git tracking or a key's age alone does not establish those facts.
The user explicitly authorized recovering and testing the historical value on
2026-09-27. It is now stored owner-only in `vault/arin.yml`; a bounded request
received HTTP 200 and a ZIP header while an invalid-key control received 401.
This establishes current bulk-download access, not absence of historical
exposure. Rotation remains recommended if exposure is confirmed, since moving
an old value does not revoke existing copies. Read it through a file/secret input without printing it or placing it in a
process argument, image, config repo, test fixture, or build log. Do not
confuse this with the GeoLite2 credential. Keep the historical updater as a
compatibility wrapper only until callers move to `arindbctl`.

The deployed legacy classifier treats the absence of an ARIN record as no
exception. The subscriber-policy-two candidate instead represents absent or
unreviewed use as unknown and excludes it from Quality without asserting risk
or hosting. Preserve explicit, auditable reasons for both policies:

- `risk`: the registration country's credible country set and GeoLite2's
  associated country disagree. Unknown/ambiguous countries are *unknown*, not
  automatically risky; document multi-country organizations and overrides.
  Under the later subscriber policy, reviewed proxy, ISP-branded virtual-ISP
  proxy, VPN or Tor evidence also sets an independent network-use risk. A
  matching country or subscriber allow cannot clear that risk. An ordinary
  access reseller, MVNO or leased prefix does not acquire it from its label.
- `non_quality`: verified cloud, CDN, hosting, data center, transit, VPN/proxy
  infrastructure, VPS, or other machine-hosted egress rather than individual
  subscriber or business end-user access. Verified cloud/CDN-operator ranges
  with ambiguous office-versus-hosted use are also excluded, as clarified by
  the user; a reviewed more-specific clean business/access exception may clear
  them. A `non_quality` flag alone is Quality-only; the independently reviewed
  network-use evidence above additionally excludes all public buckets. Under
  policy two, unknown or conflicting subscriber use also fails Quality without
  asserting that it is hosting or proxy infrastructure. Start with reviewed
  organization/prefix rules and test against measured providers; do not use a
  broad organization-name substring as an unreviewed mass exclusion.
- An unclassified child inherits a known hosting parent's `non_quality`
  exception, as approved by the user. A reviewed clean consumer/business ISP
  child or most-specific prefix override can clear the Quality exclusion;
  independent risk remains. Publish the matching rule, source, and ancestor
  provenance. An absent child classification must
  not silently turn a known hosting allocation into a clean access ISP.

ARIN bulk Whois is not authoritative ownership data for every RIR. Preserve
the network block type and registration authority: an external-RIR referral,
registry-level administrative entry, reserved block, or unknown type cannot
produce a country-risk exception merely by comparing the registry's mailing
country with GeoLite2. Direct authoritative child registrations remain usable.
This distinction has a deterministic builder regression; it is not evidence
that Main previously classified those ranges incorrectly. See the
[ARIN bulk schema](https://www.arin.net/reference/research/bulkwhois/) and
[ARIN's scope explanation](https://www.arin.net/vault/blog/2021/06/09/arins-whois-what-data-is-public-information-and-how-can-it-be-accessed/).

Use most-specific-prefix precedence and explicit override ordering, preserve
the raw evidence/reason, version the classifier, and report counts and sample
diffs before enabling new exclusions. Expand `server/ip.go`'s ARIN decoder
without breaking the current `OrgCountryCodes` reader until migration is
complete. The original pre-classifier ARINdb carried only organization country
codes, and `arinForeignScore` was a separate location score. That describes the
historical starting point, not a current artifact attestation.

Existing connection rows do not retain the raw observed client IP, so a
database-only backfill cannot safely reconstruct their classifications. The
original approved Connect rollout cycled connections; each actual database
lookup records `arin_lookup_at` and `arin_database_build_epoch`, including a
valid no-record result. An explicit IP override must not claim such a lookup.
After rollout, measure aggregate current-public-connection coverage against
the deployed ARIN database epoch and cutover time. Do not call the new risk
rules effective merely because the database file or Connect image changed.
For the still-unactivated subscriber-policy-two candidate, retain the active
resource and use the separate observer first. The historical cycle recipe is
not authorization to replace today's active MMDB for a shadow comparison.

The 2026-09-28 exact-owner review found omitted Google Cloud customer
`GOOGL-2`, Alibaba Cloud `AL-3` and IBM Cloud/SoftLayer `IBMC-24`/`SOFTL`
registrations. Independent permanent omission tests fail the old catalog and
pass the corrected candidate with existing cloud/access and quality-only
serving controls. The three added rules do not activate a new resource or
measure current provider impact. Native Quality's eight-hour success ratio
still permits one success out of one observation; the ten-measured-run/four-hour
collection quota is not an additional admission gate. High native counts are
not by themselves proof of a classifier error or sufficient evidence coverage.

Complete non-ARIN/global-cloud prefix coverage remains an explicit review gap.
The 2026-10-03 UTC builder control closes the separate independently registered
network-child inheritance gap: a containing authoritative `parentNetHandle`
chain can retain reviewed negative Quality evidence, with direct-child and
most-specific-prefix precedence. Missing, referral or noncontaining links do
not establish that authority; subscriber approval, country evidence and proxy
risk are not inherited through this new path. Organization-parent tests alone
did not prove this case. The builder correction and its additive provenance
still require a rebuilt resource and current Main shadow before activation. See
`arindbctl/CLASSIFICATION.md` for exact evidence, tests and the resource-build,
provider-shadow and lookup-epoch activation sequence. Official feed imports and
third-party proxy evidence remain separate candidates until their scope,
freshness, overlap and clean-access controls are reviewed. Never infer that a
customer origin is CDN infrastructure just because a CDN fronts its hostname.
Missing ARIN coverage is not positive proof of access use. The candidate's
affirmative catalog and additional evidence limits are documented in
`arindbctl/CLASSIFICATION.md`; prefix/rule counts are not provider coverage.
Use the separate default-off shadow reader for preactivation comparison.
Never replace today's active MMDB merely to obtain a shadow: its risk and
non-quality flags already affect serving independently of the policy-two
request-guard switch.

## Probe URL configuration and execution

Make `config/<env>/qualityprobe.yml` the authoritative source for the global
and country-specific URL catalogs, request contract, sampling quotas, and
freshness metadata. Keep the previously researched country lists and a
similarly sized global list, but ensure a country list has no overlap with the
global list. Draw equal general/country samples when both are available;
document the fallback for countries without enough verified URLs. The only
requested URLs must be randomized entries from this config. Remove the
hardcoded built-in URL table, URL replacements, and silent built-in fallback
in `qualityprobe/egresshealth`, `qualityprobe/fleetprobe`, and
`controller/provider_egress_destination_controller.go`. Retain the useful
dynamic site retirement/probation machinery, but make its candidate source
and active-pool version traceable to this config. Invalid, missing, or stale
catalogs are visibility failures, not negative provider evidence.

For each eligible provider, use an isolated DeviceLocal/tunnel and the SDK's
regional DNS settings. Resolve the sampled target separately with bounded
retries; distinguish DNS time from connection time, TLS/authentication, TTFB,
and throughput after first byte. No predictable `/ip` warm-up or special
unlisted URL. Reuse no global admission/DoH budget across probes. A provider
failure needs enough independent URL/retry evidence to distinguish a bad exit
from a single site's outage or a taskworker/control-plane failure. Track
`provider_contacted`, attempt stage, URLs tried, error class, completion,
accepted report, and index publication as separate counters.

The URL-only phase profile is DNS **5 seconds**, TCP connect **3 seconds**,
TLS handshake **3 seconds**, and read idle **5 seconds**. DNS retries share
one resolution allowance; the TCP and TLS allowances start at their respective
boundaries. Each successful read refreshes only the read-idle allowance. Every
phase remains clipped by the original total attempt deadline, including the
redirect chain. These limits do not turn local setup or contract failures into
measured provider outcomes and do not change TLS authentication or quota credit.

### URL success and final-response performance

A successful URL probe must retrieve actual destination content, not just an
HTTP success code, an empty response, a redirect, a CAPTCHA/challenge screen,
or a captive/other interception portal. Follow the sampled URL's redirects up
to a configured finite depth and retain bounded redirect evidence. Legitimate
locale or canonical-host redirects are not failures by themselves. These
redirect requests are part of the sampled load, not a new fixed warm-up URL.
Validate every hop's destination and TLS; do not follow into disallowed local
addresses or bypass certificate validation to obtain a passing response.

Only the **final response** supplies performance measurements. Intermediate
redirects must not contribute bytes, TTFB, or transfer duration to that sample.
DNS, TCP connection, TLS, redirect-chain elapsed time, and total attempt time
remain separate diagnostics; the entire chain has one bounded attempt budget.
Start transfer timing at the final response's first body byte. The TTFB
clock is final request written through first response byte, keeping DNS/connect/
TLS setup separately observable.

The initial configurable URL-success thresholds requested by the user are
TTFB **at most 2 seconds** and throughput **at least 100 kbps** (100,000 bits
per second, or 12,500 bytes per second). Both thresholds must pass along with
content validation, except for the approved small, complete-body throughput
exemption below. Performance, CAPTCHA, portal,
redirect-limit, and ordinary HTTP failures are recorded with distinct reasons;
they are not automatically TLS-authentication/security exceptions. Such URL
errors affect quality/speed ratio admission and ranking, but do not introduce
an independent online exclusion. A status/body-only contract from the legacy
catalog must not bypass these requirements in the new URL workflow.

Content checks must use positive, bounded evidence and destination-specific
contracts where needed. An ordinary page mentioning CAPTCHA, a login form on
its expected site, or a legitimate cross-host redirect is not sufficient proof
of a challenge/interception page. Preserve a distinction between a confirmed
content failure and a measurement that cannot support a verdict. The existing
1 KiB body cap was designed for lightweight reachability and is replaced by
the approved **1 MiB (`1024 * 1024` bytes)** maximum final-body read. Follow at
most **five redirects**. A genuine completed page too small to establish
throughput passes that gate, while recording `insufficient_sample`, not an
invented infinite or excellent speed. The initial configurable meaningful
sample is 16 KiB; the exemption requires confirmed complete EOF. A timeout,
partial-body read error, or capped/truncated response cannot claim it. Remove
or replace catalog entries whose declared healthy result is
bodyless before enabling this contract; a known incompatible catalog contract
is a configuration problem, not evidence against every provider.

The maximum read is a cap, not a mandatory one-MiB download. The implementation
may finish once it has a useful bounded content prefix (initially 64 KiB) and
the minimum meaningful wire sample. Requiring the entire cap within a short
deadline would wrongly reject a large page meeting the approved 100-kbps
floor. Count encoded wire bytes rather than decompressed HTML bytes; bound
both compressed and decoded reads. Record first-body-byte wait independently,
and compute throughput from subsequent wire bytes and their elapsed time so
the first byte does not acquire zero-cost transfer credit. A capped sample
still needs the minimum wire bytes; only genuine small-body EOF gets the
throughput exemption. Tests cover exact threshold timing, slow first byte,
large threshold-speed pages, gzip expansion, and partial-body failure.

Challenge detection uses explicit response headers first, then bounded,
versioned HTML structural rules. Cloudflare's documented
[`cf-mitigated: challenge`](https://developers.cloudflare.com/cloudflare-challenges/challenge-types/challenge-pages/detect-response/)
and AWS WAF's
[`x-amzn-waf-action: captcha` or `challenge`](https://docs.aws.amazon.com/waf/latest/developerguide/waf-captcha-and-challenge-actions.html)
identify blocking responses. Google-style challenge pages need corroborating
blocking-page/form/redirect evidence rather than a universal assumed header.
A widget or script alone is insufficient: ordinary usable pages can contain
[reCAPTCHA](https://developers.google.com/recaptcha/docs/display),
[hCaptcha](https://docs.hcaptcha.com/), or invisible
[Cloudflare detection scripts](https://developers.cloudflare.com/cloudflare-challenges/challenge-types/javascript-detections/).
Keep confirmed challenge, ambiguous classification, and TLS-authentication
failure separate. Test positive interstitials and ordinary widget/article/
script-only controls. Static HTML inspection does not prove the absence of
every JavaScript-only challenge and must not claim that coverage.

### URL-scoped TLS recovery

Keep independent, ordered security evidence for each failed URL, including
the actual failing redirect hop when it differs from the initial sampled URL.
An accepted valid authenticated TLS response to that URL clears its exception
even if the page is slow, challenged, or otherwise fails quality. Elapsed time,
an unrelated successful URL, a missing report, and a local setup failure do not
clear security state. Duplicate reports are idempotent and equal-time
conflicting evidence resolves conservatively to failure.

While unresolved URL-scoped TLS failures exist, draw with **50% probability
from the failed-TLS URL pool and 50% from the normal pool**. Within the normal
half, retain equal general/country sampling when both are available. Thus the
typical mixture is 50% security rechecks, 25% general, and 25% country URLs;
without failures it is 50% general and 50% country. Continue paced work until
ten accepted measured runs remain within the rolling four-hour window **and no TLS
exceptions remain**. A security recheck's quality result can count normally,
but authenticated TLS clearing does not require quality success.

Retain the failed destination snapshot for revalidation, with the same bounded
public-address, redirect, tunnel, and TLS policies. Legacy aggregate TLS flags
without trustworthy URL identity remain explicitly unresolved; recover an
identity only from authoritative historical evidence, never from a guessed
current catalog position. Removed targets or unknown historical identity must
remain visible as recovery work, not silently clear or fabricate provider
errors.

## Four-hour scheduler and database budget

Each shard execution owns one distinct network, credential parent and balance.
Fund that balance at **at least 10 times** the anticipated full-pass contract
exposure without relying on unused credit being reclaimed during the pass.
Budget the bounded 5,000 selected turns plus concurrent headroom, both origin
and companion directions, every initial-to-standard renewal stage, current,
announced-ahead and prefetched contracts, and every allowed tunnel generation.
Use the standard contract ceiling for each budgeted slot, rather than only the
URL body size; reject arithmetic overflow instead of clipping the allocation.
Do not multiply an individual allocation by the number of unrelated shards,
refill it from a shared account, or delete unsettled obligations during cleanup.
This forecast is a funding margin, not proof that contract acquisition is
healthy or permission to classify a local acquisition failure as provider loss.

The supported companion-on-companion encrypted reply also retains this private
payer. A reverse companion anchor must prove the same private payer and both
endpoint networks before its source-side reply can inherit that payer. Re-read
the eligible anchor after changing the process-local payer turn; never hold
the former turn or a database transaction while waiting for the new one.
The normal active-owner/deadline, current-client and credit fences remain in
force, and ordinary accounts keep destination-payer billing. The private ramp
can use only eligible reservations of that payer and exact pair. Actual-PG
controls reproduce the prior shard-payer rejection and check retirement,
replacement, cancellation, cross-shard refusal, zero/linger accounting and
the derived client's signed response. This is a supported-path correctness
fix: pinned probe encryption defaults to Off, so it is not evidence for the
current Main canceled-request cause or for live coverage recovery.

There is one URL-probe workflow, with no full/fast or independent blackhole
probe classes. Count the *currently eligible* public providers after the shared
reliability and ARIN risk gates; call this `N`. Each provider receives paced
URL probes until it has ten unique accepted measured URL runs, **success plus
failure**, in the rolling trailing four hours and no unresolved TLS exceptions.
Use immutable accepted `run_id` history with `url_probe=true`, the exact selected
policy version, `total_count=1` and `ok_count` either zero or one. The failed
count is `total_count-ok_count`. A completed setup/no-result turn, a zero-total
security-only report, a claim, an abandoned turn, grouped/old-policy history
and a publication replay cannot satisfy quota.

At exactly four hours a run stops contributing; exclude future measurements
and use measurement time rather than report arrival. Persist history and pacing
across restarts. A fixed period or long-running cycle cannot carry aged runs
forward. While incomplete, both success and failed measurements count toward
ten, with existing success/failure pacing and fairness. At quota, schedule the
oldest retained run's expiry; do not pause for four hours after run ten. TLS
revalidation remains independently due, even for a quota-complete provider.

The minimum fleet throughput is `10*N/4` accepted measured runs per hour,
or `10*N/14400` per second. Failures consume this quota normally; headroom is
needed for unmeasured setup work, security rechecks, publication loss, uneven
providers and worker occupancy. Track unique durable accepted success plus
failure separately from acknowledged worker outcomes and completed turns.
Publish the denominator, eligible-due backlog, oldest due age, p50/p95 stage
duration, measured-run deficit, quota completion, security-clear completion,
and unresolved/unknown-target security recovery. One coherent atomic census
must include its observation timestamp; canceled/partial/stale censuses cannot
advance freshness. An aggregate rate alone never proves per-provider coverage.

Replace the two old schedules with one URL-evidence workflow, preserving
historical telemetry. Earlier cheap/full probe measurements and success-only
quotas describe different populations or contracts and cannot be relabeled as
current measured-run coverage. Measure current eligible `N` and stage/resource
limits before changing concurrency; raising workers has previously increased
goroutines and timeouts without a faster cycle.

Make due admission one cheap, indexed, bounded operation per batch, excluding
providers that cannot enter the target index *before* opening a tunnel. Use
leases/claims with expiry so shards cannot duplicate work. The user-confirmed
2026-09-28 priority is **lowest completed URL-probe attempt count in the rolling
four hours first, including zero**, among eligible providers whose next paced
attempt is due. Break equal counts by oldest due time and then provider ID.
Count a completed failed run as well as a successful run, once across report
retries; a claim or an abandoned in-flight run is not a completed attempt.
Do not substitute the lifetime receipt ordinal, successful-outcome count, or
eight-hour admission denominator. Preserve stable jitter, claim exclusivity,
the rolling ten-measured-run quota, and outstanding URL-specific TLS recovery.
Local setup failures still do not manufacture measured URL evidence. Maintain
the scheduling window with bounded indexed state and exact aging at four
hours; do not rescan every provider's history in the claim hot path. This
scheduling change introduces no minimum-N quality/speed admission gate.
Provider IDs map to **1,024 fixed logical slots**, independently of the current
worker/host allocation. Each shard owns a set of slots. Index eligible due
work currently by `(slot, next_attempt_at, client_id)` and select only owned
slots. The new maintained-count ordering must preserve bounded owned-slot
access and a lazy priority merge rather than materializing every slot's entire
batch; its migration and writer-first rollout remain a separate verification
gate from the native-page publisher/reader.
Changing shard assignment redistributes slots, not receipt history or rolling
quota; unexpired durable claims remain excluded during handoff. Tests must
cover empty, sparse, dense, and hot-slot populations, non-power-of-two shard
counts, redistribution, fairness, and actual index-row/buffer work. An empty
shard must not scan the rest of the provider population.
Bound per-instance resource budgets for DNS, tunnel setup, HTTP work,
goroutines, and submission; add backpressure driven by measured stage
latency/host capacity, not an arbitrary fleet-wide connection ceiling. A
security-quarantined provider still needs scheduled clean URL revalidation;
ordinary failed URLs remain eligible for the normal refresh/retry cycle.

Profile the release-like workload before increasing parallelism: pprof of
Taskworker setup/DoH/transport, per-stage queue time, Redis/HTTP round trips,
and `EXPLAIN (ANALYZE, BUFFERS)` for due selection, report ingest, and score
publication. The current high PostgreSQL CPU also has a distinct hot path in
prober credit/grant selection; optimize that with bounded, indexed candidate
lookups and fresh reservation/accounting checks, not by suppressing financial
validation. An isolated candidate exists but is **not deployed**; review it
against this design before merging. Batch or cache immutable inputs, avoid
full-table polling/scans and duplicate score exports, and prove improvements
with a targeted synthetic 100k-provider benchmark plus a bounded Main canary.
Success is four-hour **unique accepted eligible** coverage while PostgreSQL
returns toward its usual ~30% CPU, query buffers/call and latency fall, and
app-facing provider counts do not regress.

Pre-fleet-ramp retention boundary: the new immutable URL history currently
has no cleanup owner. At 100,000 providers and ten measured runs per four hours,
it will grow by at least six million rows/day, before extra security rechecks. A small
measurement canary can retain every row; sustained full-fleet operation needs
an explicit bounded history policy and indexed incremental cleanup. Enforce
the accepted past-report/replay horizon before deleting deduplication rows.
Never prune unresolved TLS findings or clean security tombstones in a way
that lets an older failure be replayed or silently clears quarantine. Preserve
the eight-hour ranking and rolling four-hour quota boundaries, aggregate
diagnostic history, and deterministic delayed-report/replay tests. No cleanup
or production evidence deletion has been performed as part of this plan.

## Resolved decisions and remaining research

Resolved: coverage uses rolling four hours; unknown URL history has neutral
ranking factor `1`; final-load real content, at most five redirects, 1 MiB
maximum read, 2-second TTFB, and 100-kbps throughput with a genuine small-body
exemption; same-URL authenticated TLS clears independently of quality; 50%
failed-TLS rechecks while exceptions exist; stop only at rolling ten accepted measured runs
and no security exceptions; unknown children inherit known hosting parents
until a reviewed clean-ISP override. The common bucket gates and quality-only
ARIN exception rule remain unchanged.
Also resolved: immutable result-policy versions with an explicit shared read
selector, and provider affinity through 1,024 fixed slots owned by shards.

The user also approved the measured-history ranking curve
`0.1 + 0.9 * success_ratio` and the **16-KiB minimum meaningful throughput
sample**. They preserve the requested monotonic ranking and small-complete-body
exemption. Neither creates an additional online or quality/speed admission
gate; the 0.8 ratio, 2-second TTFB, and 100-kbps meaningful-throughput thresholds
remain the explicit product requirements.

The remaining research concerns evidence-backed virtual/hosting ISP rules and
legitimate multinational exceptions, particularly AFRINIC allocations. Do not
treat registry membership, continent, a registry's administrative address, or
one ambiguous geolocation lookup as proof of a bad provider. Review actual
registration authority, allocation hierarchy, corroborated geography, and
verified access/hosting business role. Publish classifier coverage and shadow
membership losses before enabling additional exclusions. Research may reveal
further discriminator signals; speculative rules are not approved production
exceptions.

The ARIN credential is an operational prerequisite, not another FP2 gate. Its
recovery and bounded access test are complete under the user's explicit
authorization. The recorded exposure warning is retained independently; a
successful download does not prove that historical copies are safe.

## Implementation, tests, and rollout

Current checkpoint (2026-09-28 10:31 UTC; production acceptance is incomplete):

- The current observer's actual query/parser diagnostic found eight retired
  processes represented only by historical counter ranges, alongside eight
  valid current processes. Treating the former as unknown live owners erased
  all current owners. The narrow correction and actual-parser regressions are
  adopted with focused normal/race/vet and full monitor/CLI normal passes;
  full race and watcher promotion remain pending. Desired placement still
  includes ten slots, so restoring eight observed workers must not turn the
  independent hourly completeness gap into a healthy verdict.
- Deterministic actual-API/producer/expansion tests prove that a fixed-provider
  request could pre-mint a second unused identity before its first setup
  returned. Fixed discovery now offers inert destinations and the requesting
  expansion owns minting. An adjacent canceled generator could also return an
  identity with an error without retiring it. Both corrections and thirteen
  controls are committed in Connect `8447b3f0`; owning and independent expanded
  normal/race/vet pass.
  Full release-relevant gates and a new Taskworker rollout are still pending.
  The unused identity never constructed a transport or contract manager, so
  this bug adds credential work but does not itself explain the origin storm.
- The standing observer recorded approximately 637,283 companion-origin
  fallback/deadline lookups per minute at 10:03, about 4.27 lookups per
  completed request. Missing-origin errors predominantly target probe-owned
  derived clients via stream fallback. Actual request ownership/age must be
  established before attributing them to current probes or historical clients;
  aggregate creation-minus-retirement counts are not an orphan census.
- At 10:27, the exact settlement read's nonexecuting custom and generic plans
  both estimated about 31,000 rows and selected two parallel workers. The
  10:29 catalog read shows approximately 3.73 billion escrow rows but only
  119,846 estimated distinct contract IDs at the default statistics target
  100; autoanalyze completed at 10:25, so stale statistics alone do not explain
  the estimate. The matched-reset 10:30:58–10:36:33 interval contains 66,104
  calls returning 67,418 rows (1.02/call, about 197 calls/second), with 27.37 ms
  mean execution wall time. Independent five-second PostgreSQL cgroup CPU is
  59.04% at 10:36. This does not attribute that CPU to the statement. A local
  control reproduces the row overestimate but not the parallel-worker plan;
  no settlement rewrite is accepted yet. No live
  ANALYZE, planner-setting change or accounting rewrite has been performed.
- The 10:36 authoritative census still has zero rolling ten-success quotas
  among 107,797 eligible providers, 86,109 due and 938,531 successes needed.
  Cached rolling counts agree; 152 providers remain uninitialized. These are
  not completion or throughput-recovery results.

Previous checkpoint (2026-09-28 09:50 UTC; retained outcome evidence):

- All eight source/boot-qualified Taskworkers in the 09:33–09:43 window
  completed 15,261 probe lifecycles; the 09:43 point has 501 of 512 configured
  slots occupied: 8 queued, 451 running, and 42 finished-waiting. These are
  lifecycle observations, not accepted success counts or continuous utilization.
  There were 35,611 successful internal retirements and no retirement
  error/timeout/canceled increments. Mint-minus-retire is not an orphan census.
- The corresponding fixed measurement-time receipt window contains 15,043
  accepted URL outcomes and 10,103 successes (16.84/second), including 3,231
  DNS-stage errors. Positive DNS p50/p95 are 3.15/45.13 seconds. At 09:47,
  a separate ten-minute same-generation DNS-wave window has 17,481 answers,
  5,742 timeouts with an active path, 2,733 while forming, and 1,542 with a
  provider-unresponsive snapshot. These waves are not unique outcomes, and
  active-at-completion is neither active-throughout nor delivery proof.
- The 09:50 authoritative rolling census has 107,616 eligible providers,
  79,570 due, 985,340 successes still needed, and zero ten-success completions.
  Cached rolling counts agree. All eight logical shard tasks have live leases,
  explicit 64-worker URL arguments and no current reschedule error. The
  watcher nevertheless reports URL coverage unobservable: its qualifying
  metric-owner count is not a census of running tasks. Investigate the missing
  qualification instead of suppressing the warning or claiming an idle fleet.
- PostgreSQL CPU is still unresolved: the 09:45 five-second sample is 62.89%
  of 96 logical cores. At 09:50, 590 sessions are idle in transaction, but
  460 have been idle less than one second and only one more than a minute
  (116 seconds, last statement BEGIN, no backend xmin). This does not establish
  widespread abandoned transactions. The oldest observed transaction is a
  21.6-minute autovacuum on another relation, which also holds the oldest
  reported xmin; this is not a proved vacuum blocker.
  The complete-grant prefix shortcut failed accounting-error parity and was
  rejected; the covering-index experiment also regressed on dirty heap pages.
- Four focused scheduler ownership/failure-scope controls passed owning and
  independent normal/race/vet gates and were adopted unchanged. They preserve
  existing behavior; they are not evidence that scheduler idling caused Main's
  current shortfall. No additional rollout follows merely from these tests.

Previous checkpoint (2026-09-28 09:33 UTC; retained evidence):

- All eight actual Taskworkers have the reviewed new config mount, activating
  eight shards with 64 workers each (512 configured slots). The census ended
  08:44:58; task arguments corroborated the geometry at 08:45 and 08:59.
- Unique accepted URL successes were 11,737 in 08:38:17–08:48:17
  (19.56/second), then 5,469 in 08:45:17–08:55:17 (9.12/second). The latter
  has 8,570 outcomes, 2,263 DNS failures, positive DNS median 7.00 seconds
  and p95 45.13 seconds. These overlapping measurement-time windows involve
  different cohorts and possible late ingestion; neither establishes rolling
  quota recovery or the approximately 74.88/second maintenance floor.
  The 09:18 rolling census has 107,644 eligible providers, 92,020 due,
  1,036,335 successful outcomes still needed and zero quota-complete providers.
  Cached counts agree with the authoritative selected-policy history. The
  eligible cohort changed; its current maintenance floor is 74.75/second.
- The five 08:53 public US/Best Available quality/speed/refill profiles each
  returned 20 distinct public IPv4 providers, all native requested-bucket
  tiers under the reviewed offset 11. This is request-local availability,
  not all-market, authenticated, backend-bound or data-plane acceptance.
- At 08:58 the complete exclusion cache naturally renewed beyond the earlier
  publication's expiry: `ready:v2`, 35,441 members and 1,798,948 ms remaining
  of the 30-minute TTL. Sequential reads do not establish membership parity.
- PostgreSQL short CPU samples were 17.30% at 08:52 and 38.56% at 09:00,
  versus 61.02% at 08:24. A matched 348-second post-config statement window
  fell to about 70,980 statements/second and 1.86 million shared-buffer
  accesses/second, versus 200,628 and 2.91 million earlier. Transaction
  controls are included; these are not request counts or CPU attribution.
  Workload/admission changed and sustained recovery remains unproven.
- Three active shard tasks had reschedule errors at 08:59; an indexed
  current-error discriminator at 09:11 found none. Their natural recovery
  does not establish the historical causes. Ordinary measured URL failures
  do not stop a shard; a turn's control-plane/publication error does stop
  fresh admission while current owners drain. Multi-cycle utilization is
  under test. Separately, an actual-Client regression proves the old shared
  drain/removal deadline can expire before derived-client retirement.
  Connect `18aa45f1` and Server `4949f5f9` correct the fresh retirement budget
  and adjacent Tunnel final-join/worker-release boundary. Both have actual-owner
  deterministic failure controls and owning/independent normal/race/vet passes.
  Taskworker `2026.9.28-outerwerld-1057873150` was built and deployed at 100%;
  the 09:28:19–09:28:41 census verifies all eight actual new images, the same
  config, and zero old, overlapping or missing containers. Their effect on
  Main's throughput and CPU is still under measurement, not established.
- Monitor migration731 catalog correction `c57c6ace` passed full monitor/CLI
  normal/race gates. A new primary-owned watcher started at 09:32:46 with the
  15-minute first-active-probe floor. Its predecessor and children were gone
  before launch; retain the approximately 20-second collection gap, process-local
  Sustain reset and predecessor alerts. Initial log streams do not prove
  delayed active-probe coverage or resolve previous mature alerts.

Previous checkpoint (2026-09-28 08:45 UTC; retained evidence, superseded by
the observations above where they differ):

| Boundary | Verified progress and remaining work |
| --- | --- |
| Schema, resources and Connect | Main reached migration 731 at 07:56:50. The independent catalog confirms its concurrent partial index is ready and valid; no data rows or database sessions were removed. Config Updater `2026.9.28-outerwerld-1057841730` was published at 08:35, retaining the same paired IP database bytes and epoch. Connect convergence and classification provenance are recorded in the earlier checkpoints below; this config rollout can cycle config-mounted services, and their earlier image censuses do not prove new-generation activation. |
| API deployment and exclusion-cache cost | The request-query correction `253977f2` was independently verified in all 20 expected API slots at 06:54 with Config A. Before complete cache publication, the fallback still cost about 5,124 buffer accesses per call. In the matched 08:28:20–08:33:42 interval, the exact hard-exclusion fallback fingerprint had zero calls and zero buffer accesses. This proves removal of that read-through workload in that interval, not global CPU recovery or permanent cache readiness. The 08:23 public-route acceptance below predates the 08:35 config rollout; refresh its request and artifact evidence after convergence. |
| Taskworker deployment | Repair image `2026.9.28-outerwerld-1057818710`, built from Server `6c6d4058` and local Connect `c2c515c6`, is independently verified on all eight actual slots at 08:11:22, with no old, overlapping or missing containers and Config A unchanged. It includes sparse publication, stale-head retirement, task timeout ownership, and pending-contract/setup-failure attribution. Warpctl's 40 status samples are HTTP responses, not 40 containers. |
| Capacity ramp | Config `36235fe0` and Server synthetic controls `de40a227` passed owning and independent normal/race/vet gates. Config Updater `2026.9.28-outerwerld-1057841730` was built and published at 100%; the 08:44:36–08:44:58 census verifies the exact read-only config mount and resource digest on all eight actual Taskworkers, with no old, overlapping or missing containers. At 08:45:16 all eight shard tasks have active leases, explicit URL concurrency/limit 64, result version 1, and zero reschedule errors. This activates 512 configured slots, replacing the 32-slot canary; it is not proof of 512 simultaneously occupied workers, increased throughput, or rolling quota completion. The three-scalar comparison is against checked-in Config B, not the former live Config A. |
| Queue initialization and quota | At 08:19 the current cohort was 107,821: 107,475 warming, 346 uninitialized/overdue, and zero rolling quota-complete. Selected-policy successes needed total 1,069,488. The authoritative partition is consistent; cached rolling counts match. Eight unresolved security exceptions have unknown legacy target identities. Changing denominators are retained; this is initialization progress, not a complete four-hour cycle. |
| Accepted URL evidence | The closed post-convergence 08:11:22–08:21:22 window recorded 2,203 selected-version outcomes from 2,203 providers: 1,735 successes and 468 errors, including 239 DNS failures. Median positive DNS time was about 1.12 seconds, p95 45.11 seconds. The earlier 07:11:05–07:21:05 window had 940 outcomes/695 successes. These are different cohorts/windows, not isolated causal attribution. Current successful throughput is about 2.89/second, still far below the approximately 74.88/second maintenance floor. No provider reached ten successes within that ten-minute interval; the separate rolling census, not this short-window count, measures quota. |
| DNS versus tunnel establishment | At 07:21 the same-source/boot counters paired all eight Taskworkers with all 45 result/path cells observed. Endpoint deltas show 973 answered waves, one authoritative empty answer, and 484 timeouts: 205 forming, 215 provider-unresponsive and 64 active. Counter scrape endpoints are near-aligned, not identical to the SQL receipt window; waves are not URL outcomes. The path label is the same tunnel's state at wave completion, not proof of its entire contact history. The first collector invocation incorrectly expected the Config A version instead of the Taskworker service version and is retained as unknown, not negative DNS evidence. |
| Location-index publication | The earlier source-bound plan scanned about 155 million historical location rows. Sparse complete-exception SQL `6c6d4058` plus index 731 passed causal/parity and independent normal/race/vet gates. Main's 07:57:57 non-executing plan now uses the sparse exception index and historical-location primary-key lookups, without the large history scan. At 08:17 the direct Redis read finds `ready:v2`, 34,604 set members including the marker, and 1,792,610 ms remaining of the 30-minute TTL; the pre-rollout key was absent. These sequential non-atomic observations prove fresh cache publication, not complete membership parity or completion of the later task stages. The bounded task journal did not establish a post-convergence finish and had one host timeout. Sustained CPU savings remain unproven. |
| Timeout attribution | Deployed `7f3d7b8b` preserves the timeout cause through panic recovery and joins timer ownership in both task execution paths. Deterministic pre-fix failures, independent focused normal/race, existing drain/lease/claim normal/race and vet pass. This fixes attribution/lifecycle safety rather than the bulk query cost. `claim_time` remains a moving heartbeat, not a start clock. |
| Additional scheduler correctness | Deployed `53e34cc2` retires only locked, rejected stale eligibility hints so they cannot pin a bounded due head ahead of healthy providers. Both pre-fix causal failures reproduce; full URL-model normal/race, eligibility/hard-exclusion controls, full API normal/race and vet pass after the repair. This mechanism has not been established as the cause of the sampled Main DNS failures. |
| Probe-credit allocation | A read-only PG/Redis capture at 08:13:53–08:14:02 finds all first 64 active grants have known reservations at least as large as their durable remaining credit. This explains rejection by the two bounded candidate windows at that observation. It does not establish corrupt reservations, historical CPU causality or permission to release them. Fifty older keys are missing and remain diagnostic unknowns; the application treats a missing reservation as zero. Trace normal reservation/closure and complete-reader costs before changing accounting. |
| Additional quality catalog | Config `c376c63` and Server `8d300943` add two exact, reviewed cloud-quality rules and independent omission tests. They do not change risk policy, current resource bytes or the deployed epoch; a future resource refresh needs its own classification diff/readback. |
| User-facing availability and CPU | At 08:23 the five ordinary public app-route profiles each returned 20 unique public IPv4 providers with correct country and exclusion-aware refill. All 100 responses used native requested-bucket quality/speed tiers under verified Config A tier offset 11; the 07:07 sample had only one native and 99 online fallback. This is current request-local improvement, not all-market, authenticated, backend-bound or data-plane proof. PostgreSQL was still at 61.02% of 96 logical CPUs at 08:24 after cache publication. Recovery to the expected approximately 30% remains unproven; matched residual query deltas are under investigation. Completed query time is not CPU attribution. |
| Completion | The four-hour rolling quota, sustained native quality/speed supply, and normal PostgreSQL CPU are not established. Continue source-bound diagnosis, deterministic correction, authorized deployment and matched post-deployment measurements. Do not substitute attempt counts, a green test gate or rollout completion for these outcomes. |

Historical work checkpoints (2026-09-28 06:02 UTC; superseded deployment states
are retained as evidence history, not current holds):

| Work item | Current boundary |
| --- | --- |
| Common gates, ranking, URL evidence and TLS recovery | Targeted model normal/race, owning API/Connect/Taskworker integration, and all nine quality-probe packages in normal/race modes pass |
| Fixed-slot scheduler, rolling quota and query work | Implemented; deterministic mixed-outcome and 100k-provider/one-million-history controls pass |
| Request-filter-aware fallback refill | Production-reader regression fails before the repair; bounded refill and adjacent request-filter/Redis-error controls pass after it |
| Grant-selection CPU repair | Committed as `760bc1b8` and deployed on API; isolated 25-test race gate and owning partitions pass. However, the 05:25–05:40 window pairs all 20 enabled API processes and records 436,304 full-fallback allocations with zero first-window fast-path successes. Extended-window counters are absent, not an observed zero. Investigate denomination/reservation causes before claiming the repair effective; no balance mutation has been performed. Earlier whole-model baseline failure remains recorded; no whole-model pass is claimed |
| GeoLite2/ARIN refresh and Vault inputs | Tooling `cea4a9a3`, paired resource publication Config `5313e94`; independent runtime readback passes. ConfigA deployed; all six reachable host samples mount the new resource bytes. ARIN database epoch is `1790556325`; per-process use requires connection provenance, not just file presence |
| Connection classification provenance | Connect converged: 20/20 intended slots, no old running generation at 03:44 UTC. The 03:48 shadow found 126,160 current connections: 123,057 located, 123,055 with the exact expected epoch, two located provenance gaps, and 3,103 missing locations. These residuals still need attribution; coverage percentage alone is not acceptance |
| Country-risk review | The bounded 04:33 query completed in five seconds: 465/507 classified reliable Australian providers and 79/85 Malaysian providers have risk on their preferred connection, not solely on secondary, unproven-family or extender connections. Australian non-quality overlap is 426; Malaysian overlap is zero. A separate bounded read at 05:04 isolated 544 affected providers into 526 private address-bucket groups for ownership validation; it changed no Main data. Registration-owner attribution is still required; these counts do not prove incorrect classification or actual cached/API supply |
| Prefix-country evidence support | Committed `a6df5158`; independent normal/race/vet gates and native macOS/Linux builds pass. No real exception, new database or country waiver has been activated |
| URL workflow monitoring | Committed `850b3bb0`. The opt-in 15-minute active-probe cadence correction `13bd4975` passed full monitor/CLI normal, race and vet gates; the successor started at 05:39:03 with no observer overlap, an approximately 19-second collection gap and a Sustain reset. All ten standing collectors have fresh advancing receipts. The first active URL census at 05:54 reports no new-workflow shard owners, consistent with activation still pending; missing coverage is not healthy coverage |
| Commits, migrations and four-service rollout | Core workflow committed `d479eccd`. Main migrated 721→730 at 01:07 UTC; the exact artifact probe passes. ConfigA and Connect are deployed and verified. API-only rollout of `2026.9.27-outerwerld-1057571520` started at 04:39; independent image/mount census confirms 20/20 new slots, matching the built registry digest, with no running old API or slot overlap. Taskworker and final ConfigB remain built but undeployed; no bootstrap rollup refresh has run |
| PostgreSQL CPU | Recovery is not established: the last two old-watcher samples were 32.90% and 55.07% of 96 cores. A stable 946-second post-API statistics delta positively binds 99,953 calls to `readProviderHardExclusions`, averaging 5,020 buffer accesses and 27.63 ms each. At realistic local cardinality, the original 256-candidate custom plan scans all 125,000 health rows; candidate-filtered intermediates preserve results and reduce measured work. The request-only repair passed causal failure reproduction, policy parity, prepared custom/generic plan bounds, and missing/legacy/expired-cache controls. Owning model/API normal/race and vet pass; independent focused model normal/race passed at 05:57. API build/deployment and live savings remain pending. Completed SQL wall time is not CPU attribution; historical escrow rows are unchanged |
| App provider-search availability | At 04:55 UTC, normal public US quality/speed and Best Available quality/speed requests each returned 20 unique IPv4 providers; excluding the first US result set returned 20 replacements and no excluded IDs. All five cases were online fallback, not native quality/speed. This is request-local evidence, not all-market, authenticated-client, backend-bound or data-plane recovery proof |
| Main four-hour quota | Not established; requires the new Taskworker and measured accepted selected-version successes in rolling four-hour history, not attempt counts or the successful online fallback sample |
| Long-running evidence retention/replay safety | Required before sustained fleet ramp; no production cleanup authorized or performed |

Rollout observation: the installed Warpctl serializes replacement/start/readiness
and old-container drain across each host's Connect groups. A ConfigA rollout can
capture the old image before waiting for that lock, then start it after the new
Connect version has been published. Read-only journals confirmed successful pulls,
ready replacement containers, and natural drains taking roughly 5–14 minutes.
A recent container start or a successful deploy-command exit is therefore not
proof of the requested executable version. Continue exact image/config and
remaining-generation checks; do not suppress pending version drift or force-stop
containers solely because these orderly replacements take time.

The country diagnostic's first Main EXPLAIN exposed severe cardinality
underestimation: materialized current-provider sets were estimated at one row,
selecting repeated nested loops between large sets. That statement was not
executed. Validate a bounded diagnostic plan at realistic cardinality before
running it; an EXPLAIN cost or a small-fixture pass is not a performance proof.
Any diagnostic planner setting must remain local to its read-only transaction,
never a global production tuning change.

The post-API query finding is a separate, source-bound production hot path, not
that earlier diagnostic. A bounded candidate list and bounded returned rows do
not prove bounded intermediate work: the common-gate predicate can plan a
fleet-wide hashed subquery. The request-only correction must preserve the shared
reliability, ARIN and security rules, cover missing/legacy/expired cache states,
and prove indexed work at realistic cardinality. A later complete cache can hide
the fallback's cost; successful cache publication is not a substitute for fixing
the fallback. The observed full-grant fallback is another owning path and needs
its own allocation-counter and grant-state evidence.

The narrower country query now carries risk details in the provider aggregate
and uses indexed current-location lookups with the normal planner. It does not
join the small risk cohort back to the full materialized connection set. Its
125k-provider/two-million-location synthetic control and bounded Main run both
pass. The earlier broader read returned no usable result and remains unknown;
do not retroactively classify that failure or promote a query-plan estimate to
an observed runtime.

API can be staged before country-index publication to activate its owning
grant-selection CPU repair. Source review found no API startup or handler path
publishing scores or refreshing location/reliability rollups. Keep the bootstrap
refresh and new Taskworker held during this stage: either can activate raw
connection ARIN flags in the rollups. The new reader rejects the old cache's
`ready:v1` completeness marker and performs bounded candidate SQL read-through,
so database load and request latency must be measured. Existing rollup risk,
reliability and TLS exclusions are enforced immediately; old native rows without
URL-expiry evidence can still supply online fallback. This is compatibility
staging, not proof of the new index gates, fresh URL coverage or CPU recovery.

Request verification must follow the actual public app route. A public-alias
`/status` response of 403 is intentionally generated by Warp and does not prove
an API outage. Attest executable/config generations separately. Likewise,
failure of an assumed public-IP-on-host-interface check is not proof of an
empty provider list; record the desired/applied interface discrepancy without
changing router or host configuration as part of this rollout.

1. Record a read-only baseline of eligibility counts by rejection reason,
   quality/speed/online membership, request-specific US and Best Available
   result counts, probe stage throughput, and PostgreSQL query plans. Add
   monitor alerts to `monitor/SIGNALS.md` for empty/undersized user-facing
   lists, stale eligible evidence, wrong bucket membership, catalog/database
   age, due/accepted throughput, and DB CPU/buffer pressure.
2. Build and validate the GeoLite2/ARINdb pipeline and classifier. Shadow
   compare current and proposed classifications by prefix/provider; review
   large losses of any country or organization before applying gates.
3. Implement a single eligibility function consumed by due selection,
   index/score export, counts, and diagnostics. Remove legacy quality/speed
   admission of unprobed rows. Add eight-hour exact-boundary tests and
   quality→speed→online and speed→quality→online backfill tests, including
   large exclusions and request-specific filtering.
4. Move URLs to config and harden probe stage accounting. Use only synthetic
   domains/IPs/keys in tests. Cover regional DNS, first available A/AAAA,
   retryable resolution failures, TLS rejection, local setup failure versus
   contacted-provider failure, stale/missing catalog, URL sampling, and
   accepted-report versus mere attempt.
5. Benchmark and optimize the scheduler and DB hot paths under an equivalent
   eligible population. Include deterministic tests for lease expiry,
   duplicate workers, retries, fairness, stop/cancel, bounded SQL work, and
   app-visible index generation. Follow `connect/CODESTYLE.md` (including no
   unnecessary `t.Run`).
6. The six reachable managed runners do not support deferred config
   restarts. Prepare two config releases rather than relying on
   `restart: false`: first preserve the effective legacy policy files and
   overlay the validated IP databases plus the additive `qualityprobe.yml`
   catalog; expect existing services to restart.
   Deploy Connect after the migrations. Require the intended artifacts on
   every expected enabled Connect slot and completion of old-generation
   drains before accepting the fleet shadow. Require exact-database-epoch,
   post-cutover, non-future lookup provenance on every live, located Public
   provider connection other than a separately verified, reviewed operator
   override. Keep the all-current-connection
   denominator and separately reconcile missing-location ages, entirely
   unlocated providers, mixed located/unlocated providers, and independently
   verified operator overrides. Connection insertion precedes location lookup;
   location failures retry without guaranteeing eventual success. Thus a
   missing location is neither classified nor automatically nonserving when
   another connection keeps that provider eligible. Epoch zero alone never
   proves an override; effective config/site evidence is required. Unexplained
   located provenance gaps and persistent missing-location gaps affecting
   candidates must be resolved or explicitly reviewed, not hidden by a small
   fresh cohort or an arbitrary coverage percentage. These are deployment
   evidence checks, not additional serving gates. Compare raw connection flags
   and conservative residual bounds against the current eligibility and country
   distribution. Old Taskworker rollups do not attest the new flags. GeoLite2
   refresh can itself change country and ranking inputs, so the resource phase
   is not behavior-neutral.
   After reviewing that shadow, deploy the compatible API, then Taskworker,
   then the final URL-only config. New Taskworker defaults already activate
   the eight-worker-per-shard URL workflow under the legacy YAML; the final
   config is not its activation switch. Retire old Taskworker generations
   before measuring steady-state throughput: an overlapping old worker can
   take a new claim without acknowledging its token, delaying the retry until
   the bounded lease expires. Its old reports remain audit-only evidence.
   Shadow-publish the new index and diff membership and ordering. Check US and
   other country queries with real
   exclusions and the app's location mode, not just aggregate fleet counts.
   Roll forward only when eligible coverage approaches four hours without
   DB/Taskworker overload and online backfill keeps results available through
   probe failures. Preserve a reversible last-good index and release inputs.

The first URL-only configuration uses four shards with eight active workers
each for a bounded measurement canary. This is not sufficient capacity for
the fleet and is not a four-hour completion claim. Measure actual eligible
population, successful-outcome share, per-turn occupancy, and queue tails,
then raise per-shard workers while observing DB/host and product health.
The synthetic 512-provider mixed-outcome control needed 16 workers to finish
every quota within four hours (eight left seven providers one success short).
Its roughly 3,125-worker linear projection for 100,000 providers is a test
planning estimate, not measured Main capacity or a global limit.

Implementation follows the user's clarified gate matrix above. Existing
authorization now explicitly includes applying needed schema changes and
deploying local Config Updater, API, Taskworker, and Connect after changes are
committed and tested. The operator acknowledges that Connect deployment
cycles connections. Record exact release versions and measure classification,
accepted quotas, actual cached lists, request-local results, and DB load after
each rollout; writing this plan itself has performed no production mutation.

## Source-bound checkpoint: 2026-09-28 14:15 UTC

This checkpoint is historical, not a statement of current fleet recovery.
The equal sampled quality/speed rank-document lengths (74,666 each) count
online-union cache rows: the publisher includes a row when it is native to
that mode **or** online. They are not native quality/speed counts. The then
visible native gauge values were approximately 87,000 quality, 89,000 speed,
and 112,000 online, but the source recomputation age was unknown; recent
metric delivery alone does not make those current eligibility counts. These
gauges and a requested market's cached rank-document lengths also have
different population/snapshot boundaries and must not be equated.

At that measurement, native URL-history admission required selected-policy
history with `N > 0` and success ratio at least `0.6` in the eight-hour evidence window,
plus the shared eligibility gates. A `1/1` history can qualify. Ten successes
in four hours is the scheduling/coverage objective, not a minimum-history
admission rule. The reported zero ten-success completions among 111,566
eligible providers therefore does not contradict a much larger native gauge.
The reported accepted-success rate of about `2.24/s` remains far below the
approximately `77.48/s` needed for that population's rolling quota. Do not add
a new minimum sample-count gate or infer successful coverage from native
membership without an explicit policy decision and supporting evidence.

Source review found an independent selection bug: both primary and alternate
refill loops counted filtered online-union rows before checking native
membership. Enough online rows could terminate refill while a later requested
native page remained unread; the old exclusion cap was also not proof of
native exhaustion. The private correction publishes native-only pages while
preserving legacy union keys for old readers and eventual online fallback.
It fills requested natives, then alternate natives, then online, applying
the same hard, network, family and explicit exclusions to every candidate.
It changes neither the URL-success ratio nor other bucket admission rules.
Adjacent request-layer controls found and rejected an overly strict private
failure path that suppressed online fallback when native metadata was unknown.
The corrected path keeps last-good/independently verified pages, attempts
bounded legacy compatibility, and continues through available lower-priority
buckets. It never equates missing/corrupt/unread native data with exhaustion.
The additive native-source diagnostic advertises
`urnetwork_findproviders2_native_source_schema_version=1`, while preserving
selection schema 2 for the existing monitor. It exposes each visited tier through
`urnetwork_findproviders2_native_source_outcomes_total` with fixed `rank_mode`,
`source` (`primary`/`alternate`) and `outcome`
(`quota`/`exhausted`/`unavailable`) labels. A full 20-provider online fallback
can therefore still be diagnosed as degraded native priority; returned count
alone is not evidence that native publication was healthy. Request cancellation
and hard-exclusion backend failures remain errors, not availability overrides.

Native publication uses two bounded Redis hash slots per mode/caller/target,
not an unbounded five-hour history of generation-key copies. Guarded staging
writes cannot alter the active generation. An atomic pointer switch follows
complete page-count/checksum metadata, with empty facets represented
explicitly. Interrupted staging retains last-good data; stale competing
writers and reused-generation readers fail explicitly. A target's manifest
counts its own pages, not fleet-distinct providers: summing city, region,
country and group manifests double-counts overlapping identities. Fleet
publication counts use a separate deduplicated snapshot committed only after
the complete export. Its generation ID, selected policy version, source-start,
source-completed and last-successful publication timestamps remain explicit.
The existing eight-hour selected-policy evidence map supplies denominator
bands `0`, `1`, `2`, `3–4`, `5–9`, and `>=10`, with no additional database census. These
are admission-evidence bands, not four-hour attempt or successful-quota bands;
the latter remain unavailable until their owning scheduler/receipt source is
wired. A missing or malformed census is unknown, and a complete empty census
is zero. Scrape time never refreshes source age. Any displayed count must
retain its population, generation and freshness qualification.

This is a phased cache-schema rollout: deploy the tested Taskworker native
publisher while old API readers continue using unchanged union keys; attest
complete manifests for the required targets and facets before switching API
readers. Verify interrupted refresh/last-good, native refill beyond the old
allowance, exact exclusions, and complete-zero-native online availability
before activation. Also verify bootstrap/corrupt-source online availability,
validated native priority and explicit unknown telemetry. Bounded legacy
compatibility must not mistake unread or missing pages for exhausted native
supply. Record final source/artifact and
cache-generation evidence separately from the first URL-only rollout above.

The accepted-policy-1 measured interval 2026-09-28 13:41:09–13:51:09 UTC had
2,733 unique completed URL outcomes: 1,344 successes (`2.24/s`), 1,177
`dial_dns` errors and 212 other errors. Positive DNS-stage samples among final
successes (`n=1,338`) had p50 `25.050s` and p95 `42.115s`; `dial_dns` failures
(`n=1,177`) had p50 `45.108s` and p95 `45.153s`. That stage includes cold
tunnel/contract/route formation as well as resolver residence. First-route
timing telemetry was pending, so these durations are not pure resolver latency
or proof of DNS-server fault. Keep the overlap diagnosis and its measured
stage boundaries separate from native membership and selection.
## Agent transition checkpoint: 2026-09-28 20:25 UTC

This section supersedes the *status* in the 14:15 checkpoint above, not its
product contract. At this historical checkpoint the success-only goal (superseded
by the 2026-09-29 measured-run override above) remained open: complete ten successful
URL probes per otherwise eligible provider in each rolling four hours; make
FindProviders2 consume native Quality/Speed supply before fallback without
starving users; and bring Main PostgreSQL CPU toward its usual ~30% level.
No FP2 service build, migration, or deployment from this checkpoint has reached
Main. Do not describe the local fixes or a changing rolling denominator as
production recovery.

### Main facts to carry forward

- The standing 19:39:40 UTC URL census had 113,539 eligible providers, **zero**
  with ten successes in four hours, 113,439 overdue, and 972,214 successes
  needed. A complete accepted-receipt read for 19:30–19:40 had 13,881
  successes, or 23.135/s, versus approximately 78.85/s required. It improved
  from the prior ten-minute 15.393/s but is one window, not sustained recovery.
  The next agent must take a fresh matched window and census; do not reuse
  these values as current.
- Main PostgreSQL used 44.202 of 96 logical cores (~46%) in the 19:31 watcher
  frame, above the operator's usual ~30%. The old settlement statement had
  ~266 calls/s and ~46 shared-buffer hits/call in a matched 18:22–18:32
  `pg_stat_statements` window. Execution time includes waits and is **not CPU
  attribution**. The contract-local escrow-read correction is already
  committed in Server `e2356969` but **not deployed**. A numeric-only,
  reset-aware bounded statement-delta helper is frozen but untested and has
  made no new Main read; see the contract-lane handoff below.
- The open-contract probe remained capped at `>=250001`, all sampled older
  than 30 minutes. A closer batch verified 25,000 terminal closes but retained
  117 underfunded-accounting failures and retried; do not claim the capped
  population is falling, release disputed escrow, or weaken the financial
  guard. The recovered `ForceCloseOpenContractIds` log class is not proof of
  Taskworker process crashes. This backlog needs its own matched progress and
  financial-error investigation.
- A bounded sample of 16 due-head/tail providers had real stored reliability
  scores for indices 0/1/2, not missing-neutral fallback; legacy index 3 was
  absent. The 5m/1h/12h scores had become ~81 minutes old while the raw rollup
  stayed fresh. One `UpdateReliabilities` run took ~50 minutes, then waited its
  configured 30 minutes after completion; the successor completed in ~7
  minutes and published fresh scores. This proves intermittent publication
  lag, not permanent corruption or a fleet-wide false-admission rate. The
  producer's validity signal is platform connection/message liveness (including
  zero-byte messages), **not** successful Internet URL egress; the gates must
  remain distinct.
- Successful in-tunnel DNS answer waves fell while tunnel-forming and
  provider-unresponsive timeout waves rose in matched 17:50–18:10 windows.
  The `dial_dns` timer includes tunnel setup and retries. Do not label this a
  remote DoH-server fault, or raise probe concurrency blindly; a prior 5,000
  worker/shard attempt produced millions of goroutines and poor coverage.

### Local code and independent gates

Server HEAD at this checkpoint is `d63d8582`, with uncommitted scoped changes.
Preserve the shared worktree and inspect `git status --short` before editing.
The native-only score publisher/reader, bounded cursor, complete-generation
manifest and census, availability fallback, tests, and this plan are now in the
shared `model/` and `FP2FIX.md`. The first private reader incorrectly blocked
online fallback for an unavailable native source; the corrected code retains
validated natives, tries the next tier, then bounded same-target online rows,
with an explicit unavailable-source metric. Shared merged native/adjacent
normal and race tests passed. Private causal tests proved the old native
dilution, false exhaustion, outage, and canceled-publication failures; the
synthetic 100k publisher/cursor cost normal and race gates passed. These are
local test results, not a full release gate or Main throughput measurement.

The shared reliability fix publishes 5m/1h/12h client scores before unrelated
seven-day/network work without changing the task's 30-minute completion-based
schedule. Four real-database controls passed normal/race with exact old-code
causal failures; adjacent reliability controls passed. `monitor` §2.15a now
checks scored-window freshness against a fresh rollup with bounded indexed
reads. Full `./monitor` normal/race and `go vet ./monitor` passed; a Main
non-executing plan cost ~160 with no fleet scan. The new signal is in the
successor watcher below, but its first active probe was still pending at this
checkpoint. Four formerly failing FP2 model tests were corrected as test
fixtures only, and their combined focused normal/race controls passed.

The rolling-four-hour, lowest-*completed*-run scheduler is **private and
unmerged** at
`/Users/brien/urnetwork/temp/fp2-completed-priority.Nm69B1mI/server`.
Functional normal/race, two old-behavior causal REDs, and corrected 100k
custom-plan normal/race passed. The initial all-future oracle falsely rejected
an indexed zero-row/three-buffer seek; its strengthened replacement also
rejects an actual 100,000-row zero-output scan. **Forced generic prepared
plans still fail**: dense claim scans the cycle population and no-work expiry
touches ~1,543 buffers. This is a real blocker, not a waived test. The
key-driven generic-safe SQL repair is diagnosed but not yet written or tested;
do not merge or migrate the scheduler until generic claim, expiry and
promotion plans, quiet-queue paths, and 100k normal/race controls pass.
The separate scheduler migration catalog guard passed normal/race and causal
trigger/index mutants, but is also private. The scheduler's count means
acknowledged completed URL-probe turns (including failed setup), never issued
claims; replay is idempotent, but a lost receipt is still unobserved.

The **final merged** full `./model` and `./taskworker/work` normal/race release
gates have not run after native integration. Earlier full model tests exposed
four now-corrected fixture expectations; do not quote that old RED as a
product regression or call the final tree green from focused tests alone.
The corrected writer-to-FP2 all-invalid-history causal test is queued but
unrun. Source review suggests zero-positive observed history can disappear
into missing-neutral admission, but neither the real-flow RED nor Main
prevalence has been established. Preserve truly missing-history neutrality;
do not apply a global fail-closed gate or query-time raw-history scan on this
untested hypothesis. The system `/usr/local/go/bin/go` is currently an
incompatible Linux binary on this Mac; the checksum-verified private Darwin
Go 1.26.7 at `/Users/brien/urnetwork/temp/fp2-go.bxmaIv/go/bin/go` was used
for these tests. Revalidate it before relying on the test gate.

### Authoritative monitor handoff

The old watcher PID 64125/session 85284 was gracefully stopped. A new,
root-owned watcher started at approximately 20:23:22 UTC in session **86987**,
PID **9421**, from
`/Users/brien/urnetwork/monitor/server-monitor.watch.CZOoRpby/monitor`
(SHA-256 `caf2f883ed1c510438b983ddddd4d9ce8a0ecc5eea151d1122eb961c78a4ca47`).
Its pinned Warpctl image SHA-256 is
`df39c1ace9cd6c9ee8f65759c1481c1f93f0e1de13b58cc0f82b9873aa9dd329`;
the resolved Warpctl revision contains the required Loki live-tail cursor
guard. `-list-signals` includes `2.15a reliability-freshness`. PID 9421 had
ten expected `warpctl logs ... -f` children for web, app, connect, alt, api,
grafana, proxy, taskworker, gossip and mcp; their loaded executable paths
matched the pinned Warpctl image. A short no-overlap stream gap occurred
between watchers; record it as a coverage gap, not continuous proof. The new
watcher uses `-min-probe-cadence=15m`, so its first active probe is due no
earlier than ~20:38 UTC. This transition did **not** yet prove two fresh
same-generation log reconciliations or the first new freshness result.

The next agent must first try to poll **session 86987** and verify PID 9421,
its exact stdout/stderr artifacts in the run directory, all ten standing
tails, loaded binary identities, first active probe, and two fresh log
reconciliations. If that session handle cannot transfer across agents, the
agent must take ownership with the controlled watcher handoff in
`monitor/RUN-MAIN.md`; a PID-only narrative is not an authoritative monitor
session. Keep at least 15 minutes between active monitor probe runs and do
not start a duplicate one-shot merely to validate the handoff. The primary
ledger is append-only at
`/Users/brien/urnetwork/monitor/runs/server-monitor-watch-20260907T092035Z-pid63535/ledger.jsonl`;
the watcher switch is recorded as
`main-fp2-watcher-handoff-20260928T2030Z`, following
`main-fp2-native-availability-gates-and-1939-census-20260928T1941Z`.
Append subsequent verified evidence; never rewrite prior records.

### Next-agent order of work

1. Adopt and validate the new watcher as above; get a fresh URL-quota frame,
   direct accepted measured-at receipt window, native Quality/Speed/Online
   counts, and PostgreSQL CPU sample. Do not infer hourly throughput from a
   coverage-unobservable watcher frame or the reporter ACK counter.
2. Repair the scheduler's generic prepared-plan population scans privately;
   rerun functional, causal, 100k custom and generic, expiry/maintenance,
   catalog, and migration-shape gates. Do not touch Main schema yet. Its
   private handoff is
   `/Users/brien/urnetwork/temp/fp2-completed-priority.Nm69B1mI/TRANSITION-20260928.md`
   (SHA-256 `2b6eb6b0a211db751b31a2da40744e6fc29c490561b7bffbf93bb9dcd3fc6863`).
3. Reconcile the exact scheduler patch with the now-shared native and
   reliability files; run the corrected writer-to-FP2 causal control, full
   final model/work/monitor normal and race gates, vet, and migration catalog
   audit. Inspect source/worktree overlap before any commit. The independent
   test evidence lives under
   `/Users/brien/urnetwork/monitor/fp2-gate-recovery.uGQ629ah`.
4. Commit reviewed changes, then stage rollout carefully. The scheduler's
   API receipt writer must be available before completion-capable Taskworkers;
   the native Taskworker publisher must produce complete manifests before an
   API native reader is trusted. These dependencies require an explicit
   staged build/feature boundary; do **not** deploy one combined API/Taskworker
   version and assume both ordering contracts hold. Apply and audit needed
   migrations before activating receipt writes. Attest receipt-writer
   convergence and a full four-hour delivered window before enabling the new
   priority; an epoch age alone is not delivery proof. Keep legacy ordering
   until that condition holds. Use the already-authorized local build/deploy
   workflow only after gates, recording exact versions, artifact/source and
   config generations. A Connect deploy cycles live connections; do not add
   one without a demonstrated dependency.
5. After each rollout, compare matched request-local native Quality/Speed
   supply and online fallback, accepted URL successes/s versus
   `eligible*10/14400`, per-stage failure/time distributions, worker
   goroutines/memory, PostgreSQL statement deltas and CPU, and contract
   backlog/financial failures. The target is a **measured**, sustained full
   eligible cycle within four hours, not merely higher worker count or a
   successful deployment. Keep repairing and redeploying only proven causes.

## Cross-host transfer checkpoint: 2026-09-28 21:01 UTC

The operator requested a stop for transfer to another host. This section
supersedes the live-session instructions above: the authoritative Main monitor
watcher PID 9421/session 86987 was gracefully stopped; its child tails exited.
No successor watcher was launched. The first active run completed and the
20:54:22 UTC URL frame paged: 113,080 eligible, zero quota-complete, 113,009
overdue, and 915,037 accepted successes still needed. Its hourly success-rate
source was incomplete, so this frame cannot establish throughput. The sole
ledger records the stop as `main-fp2-cross-host-pause-20260928T2101Z`.
Monitoring is **not** currently continuous. On the new host, establish a
single pollable watcher, attest its source/binary and ten tails, and preserve
the 15-minute active-probe cadence; do not infer a healthy interval from the
gap. The staged replacement binary on this host was never activated.

The shared full `./model ./taskworker/work` normal gate was attempted with the
attested Darwin Go 1.26.7 and local test environment. `./model` hit its
20-minute package timeout during numerous TestEnv database setups; the
remaining combined run was stopped. This is **not** a clean release gate and
not evidence of a particular FP2 product regression. The new agent should
first run the focused owning tests in isolation, then diagnose the full-suite
timeout before calling the merged tree green. Full race remains pending.

The private completed-run scheduler at
`/Users/brien/urnetwork/temp/fp2-completed-priority.Nm69B1mI/server`
is frozen after the operator's stop. Its generic v2 tests passed claims and
quiet maintenance at 100,000 providers across 1/3/4/256 shards, with populated
reliability, security and URL-history tables. Populated expiry/promotion passed
at 1/3/4 shards, but **expiry at 256 shards failed the bounded-work oracle**:
8,515 cycle reads and 34,422 buffers from repeated bounded-key scans. A
parameterized lateral, locked lookup correction was then written; the v3 run
compiled and passed its pure oracle but was interrupted before the 100,000-row
generic cases completed. It is **unverified**, not a green fix. Re-run generic
normal/race, custom plans, causal pre-fix, functional, catalog and migration
tests. Never treat the interrupted v3 as PASS. Exact private evidence and
pre-fix SQL are in
`/Users/brien/urnetwork/temp/fp2-completed-priority.Nm69B1mI/generic-evidence/`
on this host; those logs may include
environment diagnostics and must not be added to a product commit. The
portable source checkpoint/commit, once created, is identified below.

For transfer, obtain the committed Server branch and this document on the new
host, then independently configure local Vault/test credentials and reread
`monitor/RUN-MAIN.md`. A chat opened on another host does not transfer this
host's process/session handles, local uncommitted files, Vault, or private
test artifacts. Do not restart probes, tests, migrations or deployment merely
because a process handle is absent on the new host; first inspect the actual
source, deployed versions, ledger, and current Main state.

### Committed cross-host source and unfinished gates

At the operator's explicit direction, unfinished source was committed and
merged into local Server `main` for transfer. `591d52c0` checkpoints the native
FP2 selection/reliability signal work; `49e698d8` is the isolated scheduler
source commit and `c10f0cf8` merges it into `main`. **Merged does not mean
validated, migrated, activated, or deployed.** The completed-run priority
remains disabled without an explicit `url_completed_run_priority_since`; its
new receipt/maintenance schema must be applied through the reviewed migration
sequence before completion-capable services can be rolled out. Do not set the
priority epoch or deploy from this handoff solely because the source is on
`main`.

The exact scheduler test debt is: forced generic 100,000-provider claim and
quiet paths passed in the private v2 candidate; populated 256-shard expiry
**failed** with 8,515 cycle reads/34,422 buffers; the final lateral locked
lookup was written, but v3 100,000-provider generic testing was interrupted
before that case. Functional, custom/generic normal and race, causal pre-fix,
catalog, migration-shape, and merged full Model/Taskworker gates remain
unproven. A compile-only gate for `./model ./qualityprobe/... ./taskworker/work
./api/handlers` passed after merging. The earlier shared full Model gate hit
its 20-minute timeout during many local TestEnv setups; investigate this
separately and do not count it as a passing FP2 gate. The local pre-commit
hook's `/usr/local/go/bin/gofmt` is wrong-architecture on this host; the
checksum-verified Darwin Go 1.26.7 `gofmt` was run explicitly over staged Go
files and reported no changes. Repair the hook/toolchain on the new host
before relying on it.

The three top-level `server-*` Git worktrees were moved into
`/Users/brien/urnetwork/temp/` using `git worktree move`:
`server-grant-index.ia3wI8fq`, `server-maintenance-backup.SMjfesYc`, and
`server-prober-grant.ph6eNObP`. The grant-index branch was committed as
`5fef6daa` and merged in `9744bcb6`, resolving the append-only index to
zero-based migration 738 (operator migration 739). Its focused replay and
retained-snapshot page-work tests passed locally; no Main DDL was applied.
The maintenance-backup worktree's distinct SIGNALS.md notes merged in
`8460fdcc`. The older prober-grant implementation `b7423d18` conflicts
semantically and at the Go type level with the newer bounded selector already
on `main` (`760bc1b8`); its ancestry was recorded in `main` with an **ours**
merge, not by adding duplicate code. Its historical source remains available
in Git, while `760bc1b8` remains the active implementation. This is a
deliberate supersession, not a passing cross-implementation equivalence test.

The new host should fetch `origin/main`, check the final pushed commit recorded
by the operator, and start with `git status --short` and `git log -8 --oneline`.
The non-versioned monitor ledger and private test transcripts remain local to
the old host unless copied separately; the source, known RED, exact handoff
steps, and no-deploy boundary above are the portable minimum.

Additional bounded investigations are sealed separately in
`/Users/brien/urnetwork/temp/fp2-contract-lane-handoff.ls5u2PWK/HANDOFF.md`
(SHA-256 `87f7af3b530b502331192b7ac8f6cdb2ae451be6f88c296052fdefcff6b2a643`):
the pending all-invalid reliability causal test, numeric-only PostgreSQL
statement pair, young idle-transaction attribution, contract-generation
controls, and DNS/quality-dashboard follow-ups. None is a proved Main fix or
authority to bypass the release gate. Do not use private temporary research
artifacts as a substitute for checked-in tests and source once a fix lands.

## Linux continuation checkpoint: 2026-09-28 22:20 UTC

This checkpoint supersedes the transfer's **status**, not the product contract.
The current Linux checkout pulled `origin/main`, then committed and pushed the
monitor's missing migration-artifact contracts for versions 732–739 as Server
`0b18d6fc`. Focused migrated-catalog faults, full `./monitor` normal and race,
and `go vet ./monitor` passed against isolated PostgreSQL 18.6 and Redis 8.0.5.
The completed-run scheduler's merged forced-generic 100,000-provider claim,
quiet maintenance, and populated expiry/promotion plans passed, including 256
shards. Its focused receipt/maintenance functional controls also passed. These
local gates do not establish a completed four-hour Main cycle.

The new authoritative Main watcher started at 21:53:33 UTC with a pinned
Server `0b18d6fc` binary, a validated Warpctl binary, ten matched live log
tails, and `-min-probe-cadence=15m`. Two fresh log reconciliations and later
one-minute frames covered all ten collectors. Its first active pass began at
22:08:33 UTC. The FP2 provider-picker, provider-selection, egress,
control-route and URL-coverage signals reported unavailable bounded Mimir
evidence. A separate exact-request discriminator found this observer's SSH
transport to the configured edge-0 gateway timed out before any Mimir query.
Therefore the frame does **not** establish zero native supply, zero URL
throughput, or quota recovery. The watcher remains live while strict-key,
inventory-bound observer transport is repaired. Log-derived proxy/taskworker
DoH and Taskworker ForceClose/panic identities remain under causal triage.

A direct, bounded read through the enabled Main PostgreSQL host found migration
head **731** and its sparse ARIN-exception index valid/ready; versions 732–740
have not been applied. No index build was active. A long-running client read
held a snapshot, requiring a fresh horizon/backup check before migration 739's
large concurrent grant index. One 2.09-second direct process CPU sample used
5.19 of 96 host core equivalents for PostgreSQL; this point is not a sustained
CPU recovery result. The observer's quota, native membership, and hourly
success-rate sources remain unknown at this checkpoint.

The corrected real raw-writer → score → Go/SQL → FP2 → URL admission test first
reproduced a distinct merged-source defect: 721 observed invalid minutes
produced no 0/1/2 score rows, allowing missing-neutral Quality/Speed and URL
admission. The uncommitted candidate migration 740 and writer repair now
publish explicit zero scores while preserving truly missing neutrality; the
focused normal/race, rolling-expiry, mixed-writer and exact-zero fractional
controls pass. A million-row local custom/generic plan control measured 238
shared buffers for the observed-history aggregate versus 148 for valid-only
history and about 4,460 for an unbounded-invalid counterfactual. Full merged
Model/Taskworker and monitor release gates, commit/push, Main migration and
service rollout remain pending. The operator authorized running the migration
locally after commit, pull and push, using a verified direct maintenance path.

At 22:32 UTC a strict-tunnel, read-only Main PostgreSQL query using the exact
`GetProviderUrlProbeFleet` eligibility and completion expression found 113,008
eligible providers, **zero quota-complete**, 111,599 overdue, 97,260 due, and
900,384 accepted successes still needed for the rolling four-hour target.
The separately bounded native-membership query timed out at five seconds; its
result is unknown. This is a single direct database observation, not a measured
URL success rate or proof of API-visible native supply. The watcher continues
on its 15-minute minimum active-probe cadence.

The final local monitor package normal/race tests and `go vet` passed with
the migration-740 catalog guard and picker gateway fallback. At 22:47 UTC the
tested candidate watcher, using a private strict-SSH identity and pinned
Warpctl, replaced the earlier watcher after two fresh ten-of-ten log windows.
The old parent and all of its log-tail children exited cleanly. The successor
keeps `-min-probe-cadence=15m`; its first active signals cannot start before
22:57:35 UTC. Earlier active findings remain open pending new observations.

A direct read-only migration preflight at 22:39 UTC reconfirmed Main head 731,
absent 732–739 indexes, and no live index build. Two transactions held old
snapshots for roughly 53 minutes, including an active client read whose owner
still needs attribution. The PostgreSQL host's SSH path then timed out; fresh
CPU, storage, and snapshot-horizon evidence is unavailable. No migration has
started. The private local staged runner remains held behind the final broad
Model/Taskworker gate, code commit/pull/push, and a fresh reachable direct
maintenance preflight.

The promoted watcher's first active pass at 22:57 UTC confirmed migration lag
731→740 and found a separate §8.10 physical-index drift: the legacy
`client_reliability` index remains by name, while the desired covering parent is
absent and none of its 34 expected children are attached. The legacy parent's
actual definition and any standalone covering children still need a bounded
catalog read. The covering-index upgrade is outside ordinary
schema migrations, so applying 732–740 would not repair this condition. The
local million-row aggregate plan used the desired covering shape and must not
be used as a Main cost forecast until the live index path is checked. Picker,
URL, and sustained CPU results from the new watcher were still pending at this
checkpoint.

The first merged `./model ./taskworker/work` normal run exhausted its 35-minute
Model package limit; the test active at timeout had run only three seconds.
Thirteen named failures used a missing GeoLite binary in the portable fixture.
Two independent assertions came from its PostgreSQL Los Angeles timezone;
private fixture repair plus `PGTZ=UTC` made representative GeoLite, payout,
and retry controls pass. A third independent FP2 cached-evidence test remained
red under that environment and reproduced a real request-time demotion bug:
expired or missing-clock legacy native records lost both native and online
membership. A narrow source repair now retains their prior online eligibility
while clearing native evidence; expanded normal and race controls pass. Full
Model tests are being partitioned into disjoint bounded groups before the
release gate can be called green. No production code commit or Main migration
has followed from the failed broad run.

A strict direct PostgreSQL catalog read at 23:02:58 UTC resolved the §8.10
qualifier: the legacy parent is valid/ready with `(valid, block_number,
client_address_hash)` and **no** `INCLUDE` payload. The desired covering parent
is absent, and none of the 34 partitions has a covering-shape child, attached
or standalone. This requires the supported full covering-index upgrade rather
than only final metadata cleanup; no upgrade was started. The earlier long
client SELECT had ended, while an autovacuum on `transfer_contract` was still
active. Current direct CPU and free storage were still unknown.

The candidate's first active picker and provider-selection results paired
20/25 expected API processes; the picker saw four initial errors and one read
filter in its observed five-minute subset. URL coverage warned that the hourly
success range or expected process coverage was incomplete, so it supplied no
authoritative current throughput rate. A separate 23:03 UTC strict host read
found 1.042 TB available on the 7.556 TB PGDATA mount and a 2.095-second
PostgreSQL process sample of 6.04 core equivalents on 96 logical CPUs (6.29%).
That point sample is not a sustained CPU recovery measurement. The full
covering-index build is distinct from migrations 732–740; no such upgrade has
been authorized or started in this continuation.

A 23:07 UTC bounded catalog sizing read put `client_reliability` at 34
partitions, about 3.869 billion estimated rows, 737.47 GB heap, 434.55 GB
existing indexes, and 100.17 GB in the legacy secondary family. The largest
partition has about 173.99 million estimated rows, 33.22 GB heap and 4.20 GB
legacy secondary index. The recent 1.042 TB free-space sample is not a peak
budget for the new covering family because its `INCLUDE` payload and build
sort space have not been measured. Keep this operation separate from migration
739 and the writer's mandatory re-anchor; there has been no Main index build.

The 23:12 UTC watcher wave reported PostgreSQL CPU **51.55 core equivalents
of 96 (53.70%)** over 5.02 seconds at 23:16:09, so the earlier 6.04-core
point was not representative of this later load. The URL-coverage signal at
23:16:47 remained unobservable because it lacked a fresh coherent global
census; per-shard owner counts cannot be added into a fleet throughput rate.
This is evidence of ongoing high CPU, not a sustained post-fix measurement.
A separate strict 5.08-second host reduction at 23:24 UTC measured the PG unit
using 50.18 core equivalents; 145 of 769 child PIDs turned over between its
endpoints. Its local peer `pg_stat_activity` snapshots failed, so the high CPU
is independently corroborated but no SQL or Taskworker owner is yet proven.
The 23:27 active monitor wave separately paged on seven Connect processes on
edge-3/edge-4: individual RSS was about 78.8–166.6 GB, goroutines about
618,936–949,462, and five-minute CPU about 4.56–5.88 cores per process.
These are exact affected-process observations, not yet a causal leak or PG CPU
attribution. Bounded source/task diagnosis is underway; no process was
restarted or globally capped.
A corrected paired direct-PostgreSQL/host reduction at 23:33:58–23:34:06 UTC
measured 36.84 PG-unit core equivalents over 5.04 seconds, with 174 child
PIDs gone and 176 new between endpoints. No statement kept the same query
hash and start time across both database snapshots. About 20.08 core
equivalents came from churn or unmatched children; this proves high load and
short-lived work but still cannot assign it to a particular SQL/task owner.
The third active wave also observed zero providers on 92/92 sampled ordinary
IPv6/Quality requests with US callers at 23:31:44 UTC. This is a real
request-local empty cohort, not evidence that global provider supply is zero.
The counter does not include target IDs, exclusions or requested count, so the
owning selection stage remains unknown. Picker pairing
remained 20/25 API slots; the URL signal lacked a trustworthy same-URL TLS
quarantine target, and third-wave PG CPU/migration results were still pending.

With the corrected local MMDB/UTC fixture, the first of four disjoint full
Model test buckets finished in 920.497 seconds with one failure:
`TestUrlProbeDueEmptyShardPlan` logged 893 shared buffers for its empty-shard
scenario on a 100,000-client synthetic population, returning zero slot rows;
that scenario passed its bound. A subsequent scenario then failed because its
plan used the global due index on PostgreSQL 18.6. A scenario-labelled rerun
identified the **dense shard-0/4, limit-100** case: a keyed lateral recheck
used that index for 100 point lookups, one row each, about 620 buffers. The
blanket index-name assertion needs review against the intended population-scan
boundary before changing production SQL; the remaining buckets and race gates
are not yet complete. No commit/push or Main DDL followed this red gate.

That review found a false-positive test oracle: the global index was used only
for 100 client-keyed lateral rechecks, with one returned row, zero filtered
rows and about 6.2 buffers per lookup. The owned-slot scan stayed bounded.
The narrowed oracle permits only client-keyed point rechecks under row/buffer
bounds while retaining synthetic negative controls against global due-head
scans. It passed 16 normal/generic 100,000-client scenarios and the focused
race run. Production claim SQL was not changed; the four full Model buckets
must be rerun against the corrected test source.

A bounded request-local Mimir discriminator at 23:39:49 UTC returned 99
fixed-label selection-outcome rows. In the observed five-minute IPv6/Quality
default-minimum slice, zero outcomes increased by about 149.79 and all carried
`cache_empty`; target-kind labels split about 113.22 country and 36.56
best-available. No nonempty outcome appeared in that slice of the returned
vector. These are Prometheus increases, not exact request counts. The native
source-outcome query returned no series, which is missing diagnostic evidence,
not zero native traffic. The labels cannot link this aggregate to the 92 US
caller requests, and picker process pairing remains 20/25. The cache-empty
stage is now the next bounded source/code discriminator; global provider
supply and the exact request cause remain unproven.

The observed API schema boundary confirms `selection_schema_version=2` on
20 process series, matching the 20/25 picker pairing, while
`native_source_schema_version=1` returned no series at 23:43:29 UTC. Thus
native-source counters are unavailable at this deployment/metric boundary;
their absence cannot establish zero source events.

A matched direct-primary `pg_stat_statements` pair at 23:42:01–09 UTC
retained 2,720 statement identities with zero resets. Over about 7.62
seconds, transfer-balance-feature statements accumulated 60.67 seconds of
execution time across 23,790 calls, transfer-contract statements 46.84
seconds across 300,951 calls, and other statements 58.78 seconds across
824,721 calls. Smaller feature classes included escrow 3.34 seconds, client
score 1.39 seconds, URL cycle 0.30 seconds, URL history 0.20 seconds,
payout reliability 0.11 seconds, and no raw/running reliability calls.
Session counters recorded 209 new and 143 abandoned connections and
553,438 commits. There were no new temporary bytes or deadlocks. These
figures establish large short-lived database work, including transfer tables;
summed SQL elapsed time is not CPU attribution, and the owning application
and background load still need a bounded discriminator.

The paired activity snapshots also contained 271 before and 360 after
idle-in-transaction backends whose last statement was the exact pgx default
`BEGIN` emitted by `db.go` (`repeatable read read write not deferrable`).
Combined with about 90% of client backends younger than 30 seconds, this
locates a transaction-start/backend-churn boundary. It does not prove that
`BEGIN` itself consumed the observed CPU or identify the calling service;
live pool and owner counters are the next discriminator.

An offline normalization of two captured statement hashes matched exact
source queries in `CreateCompanionTransferEscrow` (`subscription_model.go`):
the origin lookup made 135,972 calls, returned 4,997 rows and accumulated
24.53 seconds of query execution over the 7.62-second sample; its fallback
made 130,976 calls, returned two rows and accumulated 6.58 seconds. This
proves repeated, mostly empty companion-origin lookups at the SQL boundary.
The service caller, retry cause and fraction of PostgreSQL CPU remain to be
established. A separate 51.3-second escrow/grant fingerprint is under review.

That 51.316-second fingerprint (1,867 calls, 1,921 rows in the same sample)
matches the legacy settlement escrow-to-balance join from the source before
`e2356969`. Current `main` already contains that commit's contract-local
LATERAL replacement. Thus an old statement is still executing on Main;
the emitting process and its version remain unknown, and query elapsed time
still does not prove CPU consumption. This requires deployment provenance
before considering another source change.

The fourth watcher pass began at 23:42:35 UTC, preserving the 15-minute
minimum probe spacing and ten fresh/consecutive log collectors. At 23:47:36,
its request-local IPv6/Quality/US-caller cohort again paged on empty providers
(104 sampled ordinary requests); picker pairing remained incomplete. This
repeats the user-visible symptom without yet connecting individual requests
to the aggregate `cache_empty` counter or proving global native supply.

The corrected first full Model partition then passed in 1,046.764 seconds
using the final disjoint test manifest. Three Model partitions, Work package
normal, and Model/Work race gates remain; this is a partial release gate.

The fourth watcher URL signal at 23:47:49 UTC still warned that coverage and
throughput lack a coherent global census; a separate legacy TLS quarantine
also lacked a trustworthy same-URL target. Neither is evidence of zero URL
work. Redis cache publication/read diagnosis is being limited to exact
count documents and authorized caller aliases. The enabled Redis host on
edge-6 currently lacks enrolled strict host trust from this observer, so
the direct reader boundary remains unknown until a verified route is found.

A bounded fixed-label counter read at 23:49:49 UTC observed five-minute
API companion-origin increases of about 1.102 million initial, 3.155
million fallback and 0.501 million deadline lookups, with only about one
event lookup. The measured sum was about 4.759 million lookups over 1.101
million requests (about 4.32 per request) across 20 API process series,
with zero counter resets. Measured Connect (20 series) and Taskworker
(eight series) lookup/wake increases were zero. This points strongly to the
current API fallback path as the repeated-work owner at the metric boundary;
it does not alone connect a specific SQL fingerprint or source revision to
the observed PostgreSQL CPU.

The fourth URL watcher did obtain a fresh database cohort snapshot at
23:47:49 UTC: 110,485 eligible providers, zero quota-complete and zero
secure-complete, 98,805 due, 110,297 overdue, and 916,854 accepted
successes still needed. Warming was 188, uninitialized 26, security-pending
10 and unknown-target seven; oldest due age was about 61,447.6 seconds.
These are one-time cohort facts and supersede the older 113,008-provider
snapshot; incomplete hourly success and process evidence still prevents a
trustworthy throughput rate or a completed-coverage claim.

Corrected parsing of the concatenated watcher reports recovered completed
third- and fourth-wave migration/index/CPU findings previously described
as pending. At 23:27:46/23:42:47 UTC, §8.10 still found the old
non-covering reliability parent, no desired covering parent and zero
attached desired children across 34 partitions. At 23:27:52/23:42:56,
§8.9 still found Main migration head 731 against code-required 740.
PostgreSQL CPU was 42.865 of 96 logical-core equivalents (44.65%) over
5.02 seconds at 23:31:30, and 47.377 of 96 (49.35%) over 5.03 seconds
at 23:46:37. These are repeated high-load snapshots, not an exact
statement-CPU attribution. No monitor probe was omitted; only the local
report-heading parser had missed these entries.

A bounded local Connect mechanism control built 32 Resident-style internal
clients with the current constructor (buffer 4,096, no transports, DB or
Redis). It added 129 goroutines, about four per client, and 1.06 MiB total
live heap; CloseAndWait returned to the single global pool-stats worker.
That bare constructor alone cannot explain the Main affected processes'
roughly 19–26 goroutines or 2.0–2.35 MiB heap per resident. The local
control did not reproduce full Resident transport, queued work or the
deployed revision. In sealed Main edge-4/g2 and edge-3/g2 observations,
about 87%/78% of goroutines remained outside the three existing owned
gauges, so a targeted active-owner discriminator is still required.

A bounded notification counter read at 23:52:24 UTC found about 105,682
API publishes, 105,762 enqueues and 2,113,773 received broadcast copies
over five minutes. The received count is copies, not unique origins. All
fixed notification-loss classes (queue full, publish/subscription failure,
registration decline, invalid message and unowned) increased by zero on
the 20 instrumented API processes, with no counter reset. The measured
3.155 million fallback lookups therefore are not explained by measured
notification loss in those processes. Five API slots are missing and there
is no per-request join, so the full-fleet cause is still unknown.

The implementation and this checkpoint were committed directly on Server
`main` as `d4eada4f`, pulled with no incoming changes and pushed; local
`main` and `origin/main` matched with a clean worktree. A locally rebuilt
and pinned migration adapter from that commit reports migration count 740
and identities for all nine pending versions 732–740. At 23:59:26 UTC its
read-only, strict inventory-bound direct-primary preflight verified primary
port 5432, current head 731, zero active index builds, zero other adapter
sessions, five-second statement and one-second lock deadlines, and no DDL.
The temporary tunnel was then closed. This catalog check does not establish
backup, disk, old-snapshot or CPU capacity for the apply stages; Model/Work
gates and fresh resource preflight remain pending.

The fifth watcher wave began at 23:57:35 UTC on cadence. Migration lag and
reliability-index drift repeated. At 23:59:31, eight Connect processes
on edge-3/edge-4 paged; edge-3/g1 joined the prior seven with 127.65 GB
RSS, 742,289 goroutines and a five-minute 5.478-core CPU rate. Edge-4/g2
remained the peak at 163.00 GB, 950,545 goroutines and 5.978 cores. All
reported the lazy-forward capability enabled and one same-block generation;
neither fact identifies the active goroutine owner or proves a leak.

The exact-key cache reader attempted only its initial cluster PING through
the enabled edge-6 overlay Redis endpoint at 23:59:51–00:00:02 UTC and
timed out. It read no count or alias keys, wrote nothing and did not retry.
This is an observation-path failure from this host, not proof that Redis,
its publication pipeline or IPv6 provider supply is down. The bounded
reader and private exact US target are retained for an attested reachable
route; the cache publication/read boundary remains unknown.

A separate, attested edge-1 forward to the inventory edge-6 LAN address
reached the Redis entry and boundary node ports. One bounded 33-forward
session read only exact count/schema/alias keys for the known **US provider
target** at 00:03:42–53 UTC, then closed. All three schema markers were
ready. Normal and forced Quality/Speed count arrays were encoded empty for
dual-stack and IPv6-only; the IPv4 control held 73,259 providers (forced
Speed 73,008, with an older publication TTL). Caller aliases selected the
baseline correctly. This establishes a published US-target IPv6
`cache_empty` boundary rather than a Redis read failure or a request-time
family filter. It does not prove fleet-wide IPv6 absence or explain why the
publisher found no family members. The Main request metrics label **US
callers**, not US provider targets, so those requests still cannot be joined
to this cache target without their target IDs.

The fifth watcher product frame at 00:03:17–41 UTC independently found
zero providers on 58/58 best-available and 126/126 location IPv6/Quality
ordinary requests with US callers. Picker paired only 20/25 API processes:
initial outcomes were 177 nonempty/five error, search 18 nonempty/one
empty, with three read-initial and one read-filters diagnostic. A later
URL cohort contained 110,721 eligible, zero quota-complete and zero
secure-complete, 94,997 due, 110,440 overdue and 923,953 accepted
successes still needed; throughput remained incompletely observable.
These are sampled/cohort facts, not a global provider-supply rate.

The eight fifth-wave incomplete `_ccnew` reindex artifacts belong only to
`user_auth_reset`, `device_add_history` and `competition_job_event`
(including their TOAST tables), total about 8.53 MB. They do not overlap
the pending FP2 migration 733–739 table or index names, though this does
not resolve Main's high load or large-index capacity.

An exact-US publisher-source family census was stopped at a non-executing
`EXPLAIN`: Main would scan the global valid/connected client index and
apply the US country ID as a residual filter, with estimated cost 431,540
and further global provide-key probes. Under the current PostgreSQL load,
that is not a bounded-country read. No census query ran and writer-family
membership remains unknown; a narrower index-ordered discriminator is needed.

The corrected second full Model partition passed in 1,180.705 seconds with
no failures. Partitions zero and one of four are green; partition two is
running. Work package normal and Model/Work race gates remain.

A later explicitly biased 4,096-ID prefix sample at 00:15:20 UTC contained
3,136 US rows and 3,048 top-level selectable candidates, but zero stored
or candidate IPv6 proof. Among 3,144 connected rows, 16 had IPv6 sockets,
yet none had IPv6 intent, proven family or fresh located proof; all 3,144
carried legacy intent and the same update block 29,843,997. This aligns
with the published US IPv6 cache-empty result and points to family marking
in the sampled source cohort. Because IDs were sampled as a prefix, it
cannot establish full-US or fleet counts. No full-table census ran.

The sampled update block 29,843,997 corresponds to 23:57:00 UTC and is the
reliability job's captured `maxTime`, not its commit time. Its age at the
sample does not alone prove a stalled writer: the current job reschedules
30 minutes after completion. Source review found that
`ConnectionProvenIpFamily` deliberately maps any intent-zero row, including
one with an IPv6 socket, to legacy IPv4; IPv6 eligibility requires both
observed and declared IPv6. The sampled source therefore fits missing
declared-family intent upstream, without proving a cache fanout omission.
Current H1 `X-UR-IpFamily` and H3/Auth intent paths exist; deployed Connect
artifact and client-intent ownership remain to be distinguished. IPv6
eligibility must not be widened on socket shape alone.

The sixth watcher wave continued to page on infrastructure load: at
00:14:40 UTC one edge host had load-1 174.77 against 72 logical CPUs
(normalized 2.427), execution ratio 0.9446 and available-memory fraction
0.4889. At 00:14:50 eight edge-3/edge-4 Connect processes still paged;
the largest observed values in that frame were 150.06 GB RSS, 929,342
goroutines and a 6.449-core five-minute CPU rate. At 00:16:59,
PostgreSQL used 49.930 of 96 logical-core equivalents (52.01%) over its
short sample. Migration head 731 and missing covering reliability index
also persisted. These are active Main performance problems; process
ownership and causal relationships remain under investigation.

The saturated host was enabled `by-us-fmt-5-edge-3`. Its five-minute
execution ratio means about 94.46% non-idle, non-iowait CPU time across
node modes, not a per-process share; paired load 2.427 per logical CPU
and zero iowait support host CPU saturation. The separate missing-metric
VPN/planetoid host findings remain unknown rather than cleared.

The sixth picker frame at 00:18:51 UTC remained incomplete at 20/25 API
processes. In its sampled five-minute window, initial outcomes were 199
nonempty/three error with no empty, search 25 nonempty and direct two
nonempty; one read-initial and one read-filters error remained. This does
not clear the earlier request-local IPv6 empty cohorts or establish fleet
health. The URL signal at 00:19:11 warned that shards 0, 1, 3, 5 and 6
each had two reported owners and lacked a fresh coherent global census;
the sixth-wave quota cohort was still pending. Owner duplication needs
generation/source reconciliation before deriving a fleet throughput rate.

The completed sixth URL signal did not produce a coherent quota cohort;
its verdict remained unobservable because of the duplicate owner and
missing census evidence. Therefore sixth-wave quota status is unknown,
not a new zero-quota observation. The fifth-wave direct DB cohort above
remains the latest trustworthy one-instant count.

The corrected third full Model partition passed in 1,062.372 seconds.
Partitions zero, one and two of four are green; the final normal Model
partition, Work normal and race gates still remain.

The strict edge-1 peer image check stopped before Docker or process reads:
`sudo -n` required a password. It did not retry or mutate anything.
Therefore independent running-container/binary identity remains unknown;
the fresh 48-process exported provenance above is still the strongest
available witness. A local decoder control matched `go version -m` on
the pinned watcher after correcting module-info framing, but this is not
Main binary evidence.

The seventh watcher wave began at 00:27:35 UTC on the 15-minute cadence.
Its early §8.10/§8.9 checks again found the old non-covering reliability
parent across 34 partitions, no desired attached children, and migration
head 731 against required 740. Other seventh-wave results were pending
at this checkpoint; no schema mutation was observed.

The seventh edge/runtime frame at 00:29:52–00:30:02 UTC paged on both
edge-3 and edge-4 CPU saturation: edge-3 load-1 194.62/72 logical CPUs
(2.703 normalized), five-minute execution ratio 0.9577; edge-4 load-1
232.28/72 (3.226 normalized), execution ratio 0.9127. Both reported
zero iowait. Eight enabled Connect processes on those hosts still paged;
edge-4/g2 reached 153.88 GB RSS, 1,014,398 goroutines and a 6.589-core
five-minute rate. These are active runtime problems, not proof of a
particular callsite, leak slope or safe hard cap. The missing vpn0 and
planetoid node series are separate unknowns.

The seventh PostgreSQL CPU signal at 00:32:05 UTC remained high at
48.718 core-equivalents of 96 (50.75%) over 5.03 seconds. It is another
short load point, not statement or service CPU attribution.

A fixed-label API failure cohort at 00:32:37 UTC narrowed the repeated
fallback work. Across 20 processes with no counter resets, five-minute
missing-origin increases were about 916,024 for originally noncompanion
requests and only 2.09 for requested companions. Of the noncompanion
failures, about 854,275 (93.26%) were `stream_fallback` on a public
relationship from active-top/other source to active-derived
`egress_prober` destination; about 852,381 carried absent sender role
and 1,893 client sender role. The dominant measured work therefore targets
durable prober-owned derived clients, rather than ordinary requested-
companion cold starts. This is outcome/ownership attribution, not CPU
share or proof of a particular deployed Taskworker artifact. Prober
teardown and origin ownership are the next source boundary.

The seventh picker/URL frame at 00:34:02–23 UTC still paired only 20/25
API slots. Its five-minute sampled picker outcomes were 209 initial
nonempty/ten error, 29 search nonempty, no direct result and no sampled
empty, with nine read-initial and one read-filters error; this partial
sample cannot clear the earlier request-local IPv6 empties. The URL
source did return a fresh DB cohort: 111,319 eligible, zero quota-complete
and zero secure-complete, 91,659 due, 110,109 overdue, 1,210 warming,
none uninitialized and 933,919 accepted successes still needed. Security
pending was 11 and unknown target seven; oldest due age was about 6,574.8
seconds. The hourly success range/process coverage was still incomplete,
and legacy TLS recovery lacked a trustworthy target. The changed oldest-
due value alone cannot establish quota or throughput recovery; the cohort
still shows a severe completion deficit.

Source comparison adds a guard against an easy but unsupported fix: the
serving clean API revision already has active-destination checks,
`ContractError_Reliability` for missing origins and a 500 ms event-assisted
fallback. Merely redeploying API cannot be assumed to repair the measured
active-derived return failures. About 99.78% of the prober-target failure
cohort carried absent sender role; that is capability/producer evidence,
not a device or version identity. Provider-return source-gate adoption,
prober retirement and control outcomes need independent checks before
assigning an old-client or teardown cause.

A bounded Taskworker owner control at 00:37:58 UTC saw all eight processes
with initialized counters and no resets. Over five minutes, internal
credential mint completed about 11,225 times (9.41 canceled), and retire
completed about 11,650 times with zero measured error, timeout or
cancellation. This rejects aggregate retirement-call failure as the
measured current explanation for the API fallback cohort; it is not a
per-derived-client lifecycle join. Separate owned DNS waves had about
5,774 answers and one authoritative empty result, but active-path
timeouts remained about 1,874, forming timeouts 413 and provider-
unresponsive 952, with canceled/unanswered zero. These are completed
logical DNS waves, not raw DoH dial lines or final provider verdicts.

The source lifecycle trace qualifies that result: internal retire=ok follows
the transaction that marks the model client inactive, but the retirement
metric begins only once removal is reached. Earlier transport, client and
out-of-band joins can retain an active derived child without incrementing
retire failures. A read-only, plan-gated comparison of the oldest/newest
128 active prober-network clients is prepared with at most nine connection
rows and keyed handler reads per sampled client under a three-second
deadline. It will emit aggregates only. No cleanup or wait change is
justified before this ownership boundary is measured.

The plan-approved direct-primary read at 00:48:52 UTC sampled the oldest
and newest 128 active clients in the singleton durable prober network.
Among the oldest, 127 were derived and older than a day (median create
age about 24.90 days, maximum 31.29 days); none of those 127 had a
connected row or fresh handler. The newest 128 were all derived, median
age 2.34 seconds and oldest 2.81 seconds, also not yet connected in a
normal setup-in-progress control. This proves stale active derived objects
coexist with fresh creation, but the biased prefixes are not a population
census, leak rate or join to failing API requests. Some oldest residue
may predate the current cleanup artifact. Idle reaper and deployed-base
cleanup timing remain to be checked; no manual retirement was performed.

Source and the running Taskworker base clarify retention: disconnected
derived clients remain active until `auth_time` is 30 days old; the
top-level idle marker is also 30 days, while connection history is eight
hours. The sampled derived clients' median auth age of about 24.90 days
is within that intended window, so the prefix is **not** proof of reaper
failure. The oldest sampled maximum of 31.29 days includes a non-derived
parent and cannot be assigned to a derived child. All eight observed
Taskworker processes started on September 28 around 09:25–09:27 UTC,
after the old residue began. Old retained rows and recent successful
retire calls can therefore coexist correctly. A small negative age on
the newest sample reflects transaction-start versus later visibility,
not proven clock skew. Exact request-to-child linkage and pre-removal
worker ownership remain unknown; no cleanup patch is justified.

The first full Work normal run ended red in 288.165 seconds on two tests
that both panicked because the local test fixture lacked
`arindb/arin.mmdb`: `TestDeriveLocationsRemovesTheRowOfANodeWithNoPings`
and `TestRemoveExpiredPingsFallsBackTheReadPath`. No product assertion
failure was identified in that run. The private fixture is being repaired
from the versioned Config source, then those tests and full Work normal
will be rerun before the race gates.

The two focused Work controls then passed with the private ARIN fixture,
and the full Work normal rerun passed in 270.518 seconds. All four full
Model normal partitions and Work normal are now green on the unchanged
source; Model/Work race gates remain.

The eighth PostgreSQL sample at 00:47:13 UTC remained high at 46.406 of
96 logical-core equivalents (48.34%) over 5.02 seconds. Its picker frame
at 00:49:17 still paired only 20/25 API processes and warned on read
errors: 186 sampled initial nonempty/five error, 18 search nonempty/four
empty, one direct nonempty, and five read-initial errors. This is partial
process/request evidence and does not clear the IPv6 empty cohorts. The
eighth URL result was still pending at that time.

The completed eighth URL frame at 00:49:37 UTC paged with a fresh DB
cohort of 110,206 eligible providers, zero quota-complete and zero
secure-complete, 91,153 due, 110,133 overdue, 73 warming, 19
uninitialized and 921,963 accepted successes still needed. Security
pending was 10, unknown target seven, and oldest due age about 65,149
seconds (18.1 hours). Throughput/process coverage stayed incomplete;
shards 2, 3, 5 and 7 each reported two owners, and the legacy TLS
recovery target remained untrustworthy. This cohort is a current
completion deficit, not a measured fleet throughput rate.

A bounded read-only Taskworker Loki discriminator at 00:52:40 UTC hit its
128-record cap in about 236 ms of one hashed container stream. Of the
matching lines, 64 parsed structured records contained
`ForceCloseOpenContractIds` and exact escrow-insufficiency `errorString`
fields; 64 others were unparsed. This confirms a real accounting
rejection class in that stream, not a PostgreSQL SQLSTATE or process
crash. The cap reached before a completed-batch control, so its absence
cannot prove no progress. The 64 lines are not 64 unique contracts, and
the exact process/image join remains unknown. No raw logs were retained.

A separate bounded completed-batch control at 00:53:54 UTC found one
record from the same hashed container stream, about 0.252 seconds after
the sampled error burst. It reported 25,000 terminal-verified contracts,
129 unresolved accounting items, zero quarantined accounting items and
2,936 ms retry delay. Verified progress and unresolved accounting thus
coexist in that batch; the stream is not totally stalled. This does not
count unique failed contracts, identify the exact PID/image or assign
CPU share. Current source intentionally uses a two-to-four-second retry
when verified count is at least 6,250; these 129 rejections did not
trigger an empty-batch fast retry. No financial mutation or retry-policy
change is justified by this bounded control.

The fourth and final disjoint full Model normal partition passed in
954.992 seconds. All four normal Model partitions now pass on the final
source digest. Work package normal and Model/Work race gates remain.

The eighth watcher at 00:44:54–00:45:05 UTC still paged on edge-3 CPU
saturation (load-1 198.82/72 CPUs, five-minute execution ratio 0.9631,
zero iowait) and eight enabled edge-3/edge-4 Connect processes. The
peak affected edge-4/g2 process had 144.37 GB RSS, 934,615 goroutines
and a 6.410-core five-minute rate. This frame did not establish
resolution on any unpaged host; vpn0 and planetoid fresh node metrics
were separately absent. Migration/index drift remained unchanged.

A bounded fresh executable-exported provenance read at 00:21:10 UTC
identified all 48 observed service processes by start/build/source/RSS
metrics: 20 API processes report revision `253977f2a713` with
`modified=false`; 20 Connect report `d479eccdfdc0` with `modified=true`;
eight Taskworker report `4949f5f93566` with `modified=true`. Each service
had one reported source/image identity. These are running-process exported
labels, stronger than an assumed rollout version, but modified builds
still need independent artifact/digest review before claiming exact code.
The five API processes missing from picker pairing require separate
coverage analysis; this provenance observation alone does not clear them.

The three reported base revisions exist locally. Clean API `253977f2`
predates both the `e2356969` settlement-query replacement and the
native-source telemetry addition, explaining why its observed processes
export no native-source schema. Connect `d479eccd` and Taskworker
`4949f5f9` bases also predate the settlement fix, but their
`modified=true` effective deltas are unknown. All three base sources
already contain H1/H3 family-intent plumbing and `ipv6_proven`
aggregation, so version ancestry alone does not prove an absent intent
feature or identify which process emitted the old SQL. Independent running
artifact and caller evidence remain required.

The next documentation checkpoint was committed on Main as `d616f38b`,
pulled with no incoming changes and pushed; the worktree matched
`origin/main`. The local migration adapter was rebuilt from that exact
commit. Its executable SHA-256 is
`ab1fd25b5ffa42595dd9f75718f70723e853ab14272db7e46a93064716345152`;
its nine versioned identities for 732–740 retain manifest SHA-256
`2448e0b0242f52665dc3cc9be2d5cd19ad211e2b1539c40696c6a7823942833b`.
This is a source identity check, not a Main migration; the numbered schema
head remains 731 at the ninth watcher start.

The full Work race run then passed in 328.183 seconds on the same private
PostgreSQL, Redis, GeoLite2 and ARIN fixtures. Work normal and race plus
all four disjoint Model normal partitions are green. Model race partition
zero is running; the remaining Model race partitions and a fresh Main
resource/horizon preflight are still required before staged migration.

The ninth watcher began no earlier than 00:57:35 UTC, preserving the
15-minute active-probe floor. At 00:58:11–15 the reliability parent still
had the old non-covering definition with no desired covering parent or
attached children across 34 partitions; numbered migration head was 731
against code-required 740. At 01:00:11–31 edge-3 still paged for CPU
saturation (load-1 188.69/72 logical CPUs, five-minute execution ratio
0.9580, zero iowait), and eight edge-3/edge-4 Connect processes still
paged. Edge-4/g2 reached 137.88 GB RSS, 842,102 goroutines and a
5.922-core five-minute rate. This is continuing load, not callsite or
leak attribution. Ninth PostgreSQL CPU, picker and URL frames were still
pending at this checkpoint.

The watcher also supplies a current-cohort lifecycle control distinct from
the biased oldest-client prefix: at 00:50:33 UTC its six-hour prober cohort
had 877,732 mature clients, 877,715 inactive and 17 active disconnected,
all never connected. The `probe-unused-args-retirement` WARN identifies
those 17 as a real residual, with oldest age about 20,162 seconds, while
the overwhelmingly inactive mature cohort contradicts a blanket current
retirement failure. These recent rows do not measure retention of the
24.9-day-old prefix or join to missing-origin API requests. Their exact
setup/cancellation owner is under source review; no cleanup mutation has
been made.

The fresh read-only direct-primary migration preflight at 01:03–04 UTC
confirmed PostgreSQL 18.4 on port 5432, successful migration head 731,
zero new migration starts in two hours, zero active index builds and no
733–739 candidate indexes. The old sparse ARIN exception index remained
valid/ready. Estimated target sizes were 9.41 million `transfer_balance`
rows (1.375 GB heap), 132,000 probe-cycle rows (29.85 MB heap) and
988,000 reliability-running rows (221.8 MB heap). No `pg_dump` COPY,
prepared transaction or replication slot was observed. The oldest
snapshot belonged to an active autovacuum worker about 3,160 seconds old
with `VacuumDelay` and xmin age about 10.17 million; the next client
transaction was about 10.2 seconds old in the bounded top 16. Host
backup unit was inactive and PostgreSQL active. The PGDATA mount had
about 1.37 TB free; WAL held about 10.65 GB in 635 files, archive mode
was off, and there were no active senders. The cgroup CPU quota could
not be observed, so host resource evidence remains incomplete. The
strict inventory-bound tunnel was closed after the read. This fresh
preflight removes the earlier unclassified long client transaction from
the observed top horizon, but the old autovacuum and high PostgreSQL CPU
still require a staged migration admission decision after race gates.

The remaining ninth watcher frames did not establish recovery. PostgreSQL
used 37.531 of 96 logical-core equivalents (39.09%) over five seconds at
01:02:19 UTC. Picker at 01:04:30 still paired only 20/25 API slots:
246 sampled initial nonempty and nine error, 62 search nonempty, with
four read-initial and three read-filters errors. This partial request
sample cannot clear the earlier IPv6 empty cohorts. The URL frame at
01:04:49 still paged: 110,206 eligible, zero quota-complete and zero
secure-complete, 92,752 due, 110,133 overdue, 919,545 accepted
successes needed and oldest due age about 18.3 hours. Hourly
throughput/process coverage remained incomplete and the legacy TLS
recovery target untrustworthy. These counts do not measure a fleet
throughput rate or prove which worker is responsible.

A private three-control regression isolated one concrete prober residual
mechanism without touching Main. In unchanged source, cancellation
delivered after `AuthNetworkClient` commits but before its optional Redis
identity-cache fill left one committed active child while the caller
received an error and no child result. The healthy and pre-transaction
cancellation controls did not fail. An overlay containing only error
containment around that optional cache fill passed all three controls;
the post-commit child identity remained available to its caller. This
proves the source ordering defect and the proposed local repair in the
fixture, not that the 17 Main residuals all share this cause. A tracked
fix and final-source test gates are underway; no Main service artifact
has changed.

The tracked correction now contains only the optional identity-cache
fill's Redis wrapper error, preserving the already-committed child result.
The new regression additionally uses ordinary network-fenced removal and
checks the returned child becomes durably inactive. It and the adjacent
cache-miss/refill control passed together in 12.414 seconds normal and
18.535 seconds under race. The prober unused-args alert and `SIGNALS.md`
now name both the committed-mint/cache boundary and the older
direct-removal lifecycle, while requiring artifact comparison before
assigning the 17 Main residuals to either. Its severity, cohort SQL and
thresholds remain unchanged. Full final-source Model, Work and monitor
gates are being regenerated and run; no Main rollout has occurred.

The tenth active watcher began at the 01:12:35 UTC cadence. Its
01:13:17–21 catalog frame still found the old non-covering reliability
index and migration head 731 against required 740. At 01:15:24 UTC,
edge-3 and edge-4 again paged for CPU saturation: one-minute loads
210.20/72 and 195.63/72 logical CPUs, with five-minute execution
ratios 0.9575 and 0.9137 and zero iowait. All eight enabled Connect
processes on those two hosts paged at 01:15:42; edge-4/g2 reached
140.63 GB RSS, 865,263 goroutines and a 5.965-core five-minute rate.
The separate vpn0 node series was still absent. Tenth PostgreSQL CPU,
picker and URL frames were pending at this checkpoint; the runtime
pages do not identify a leak callsite or ownership of SQL CPU.

The final-source Model manifest now enumerates 1,191 unique disjoint
top-level tests in four buckets of 296, 305, 311 and 279. The one added
test is the committed-child/cache cancellation regression; no prior test
was removed. The manifest SHA-256 is
`c49361eee0cf13be01b839ff4fcf07b73dd26348bef5f1f9bdb5038749e92ca1`
and the 453-file root/Model source digest is
`9d116fdd2a0016195f26953e371095491a41d13465c7f50b823db6fda8a124ba`.
`go vet ./model ./taskworker/work` passed. The focused prober alert
synthetic passed normal and race in 3.174 and 4.693 seconds; full
monitor normal/race, Model partitions and Work normal/race remain final
release gates for this source.

The tenth PostgreSQL CPU frame at 01:17:26 UTC was 40.522 of 96
logical-core equivalents (42.21%) over 5.02 seconds. Picker at
01:19:43 still paired only 20/25 API slots: five-minute sampled
initial outcomes 185 nonempty, zero empty and one error; search 24
nonempty and one empty; direct one nonempty, with one read-initial
error. This partial process/request observation does not clear native
IPv6 supply or request failures. Tenth URL and prober cleanup frames
were still pending.

The tenth URL frame at 01:20:01 UTC still paged: 110,174 eligible,
zero quota-complete and zero secure-complete, 93,485 due, 110,094
overdue, 915,417 accepted successes needed and oldest due age about
16.8 hours. Hourly throughput/process coverage remained incomplete;
the legacy TLS recovery target still warned. The 01:20:48 prober
cleanup frame found 857,331 mature children, 857,315 inactive and 16
active disconnected, all never connected; 135 newer active children
were outside the mature cohort and no deactivate timestamp was missing.
Its unused-args WARN remains a current residual, without a per-child
join to the cache-cancellation mechanism. The changed cohort and oldest
due age do not establish sustained improvement.

The full final-source monitor normal package passed in 98.931 seconds.
Full monitor race and vet are running, followed by the disjoint Model and
Work broad gates.

The repair and diagnosis were committed directly to Server `main` as
`679d136c`, pulled with no incoming changes and pushed; the worktree
matched `origin/main`. The post-commit local migration adapter rebuild
retained executable SHA-256
`ab1fd25b5ffa42595dd9f75718f70723e853ab14272db7e46a93064716345152`
and the same 732–740 migration manifest SHA-256
`2448e0b0242f52665dc3cc9be2d5cd19ad211e2b1539c40696c6a7823942833b`.
Final-source `go vet ./model ./taskworker/work` and `go vet ./monitor`
both passed. Main migration head remains 731; no schema or service
mutation followed this commit.

The full final-source monitor race package passed in 188.160 seconds;
monitor normal, race and vet are now green. Model bucket zero normal
(296 tests) and full Work normal started under distinct local PostgreSQL
and Redis test leases, with one heavy Model bucket at a time. The
committed root/Model source digest still matches the final manifest.

A separate bounded fixed-label Taskworker URL control at 01:24:21 UTC
observed all eight processes, 504 series rows, fresh scrapes within
12.1 seconds, complete five-minute and one-hour ranges and no counter
resets. The observed hourly successful-report ACK count was about
53,870 versus at least 275,435 per hour needed for ten successes per
110,174 eligible providers over four hours. At most 19.6% of that
required rate is represented by successful ACKs even if every one became
new accepted history; a commit followed by a lost ACK could add durable
rows outside this counter. The five-minute sample had about 5,929
successful and 1,486 error ACKs, 7,431 completed turns and mean turn
occupancy 16.34 seconds;
local failures were about 5.22, mostly classified as
`general_country_unavailable`. ACKs are not a count of newly accepted
unique provider results, and these aggregates do not establish worker
capacity or per-provider fairness. They do establish a throughput
shortfall in acknowledged work independently of the monitor's previously
incomplete hourly coverage. The exact accepted-history rate, source
ownership and per-provider fairness still need a durable receipt window.

The full Work normal package on the committed final source then passed
in 339.217 seconds while Model bucket zero normal ran under a separate
local test lease. Work race and all full Model partitions remain.

The eleventh active watcher began at the 01:27:35 UTC floor. Its
01:28:21–24 catalog frame again found migration head 731 against 740
and the old non-covering reliability parent, with no desired parent
or attached child indexes across 34 partitions. Edge-3 and edge-4
still paged for CPU saturation at 01:30:36 UTC: one-minute loads
183.60/72 and 216.53/72 logical CPUs, five-minute execution ratios
0.9238 and 0.9358, and zero iowait. Eight enabled Connect processes
on those hosts again paged; edge-4/g2 peaked at 147.30 GB RSS,
941,010 goroutines and a 6.473-core five-minute rate. vpn0 node
metrics were absent and planetoid warned separately. Eleventh
PostgreSQL CPU, picker and URL results were pending; these host
signals do not assign the SQL or goroutine cause.

The local desired Main URL configuration declares eight shards with
64 workers each, a 60-second turn deadline, two tunnel recreations,
and 134 general destination entries but no country-specific catalogs.
That explains why `general_country_unavailable` can appear as a local
classification without establishing a network-latency cause; general
probes continue. At the measured 16.34-second completed-turn mean,
512 continuously busy workers would support only about 112,800 turns
per hour before failures, below the current cohort's roughly 275,435
needed successes per hour. This is a conditional sizing calculation,
not proof of effective running worker arguments, occupancy, or safe
capacity to raise limits. The running configuration and a bounded
durable receipt-timing sample are the next evidence boundaries.

A plan-gated direct-primary URL receipt read at 01:33:58 UTC used a
non-executing index plan, then selected only the newest 1,024 accepted
history rows by measured time. That capped prefix spans 50.916 seconds,
contains 1,024 distinct providers and all current policy/evidence
fields, and has 750 successes (73.24%). Among failures, 130 were
other/unavailable, 127 content, nine TLS authentication, six
connect/DNS/TLS, and two performance. Encoded mean DNS timing was about
11.48 seconds among successes and 42.18 seconds among
other/unavailable. The direct-primary tunnel was closed after the read.
This is a biased completed-receipt prefix, not an exact fleet insert
rate or a phase attribution; the timing fields require source-qualified
stage interpretation before changing retries or worker counts.

The eleventh picker at 01:34:55 UTC still paired only 20/25 API slots:
five-minute sampled initial outcomes 251 nonempty, zero empty and two
error; search 37 nonempty and zero empty; one read-initial error.
This partial sample cannot clear the earlier request-local IPv6 empty
cohorts. The 01:35:13 URL frame still paged with 110,167 eligible,
zero quota-complete and zero secure-complete, 91,158 due, 110,089
overdue, 913,161 accepted successes needed and oldest due age about
17.1 hours. Hourly process/throughput coverage was incomplete, and
legacy TLS recovery still warned. The eleventh PostgreSQL CPU frame
and prober cleanup frame were not yet sealed; their absence here is
unknown, not a healthy measurement.

Source review qualifies the receipt `dns_ms` field: it spans the
resolver's first DNS start through a positive IPv4 answer and can
include up to three private in-tunnel DoH waves plus inter-wave
jitter. It is not a direct upstream nameserver RTT, and the resolver's
lookup context deliberately does not carry target HTTP trace phase
values. Zero or near-zero TLS timing may involve duplicate target
TLS callbacks; that requires a deterministic control before assigning
a production phase-cost cause. No timeout or worker limit was changed.

A second plan-gated direct-primary read at 01:37:10 UTC found all eight
expected `RunOnce` URL shards with matching function/argument/result
versions. Each current task declared shard count eight, URL limit and
concurrency 64, per-probe timeout 60 seconds, task maximum 900 seconds,
idle delay five seconds and two tunnel recreations; no reschedule error
was recorded. Claim ages were 0.93–7.50 seconds and release times
292.5–299.1 seconds ahead. Eight distinct sessions each held exactly
one sampled advisory ownership lock, below the bounded 4,097-lock cap.
The tunnel closed after the read. This verifies current task arguments
and one-instant ownership, not process/image provenance, actual inflight
occupancy or a sustained throughput rate. Earlier duplicate-owner
watcher frames may represent different instants and remain separate.

The eleventh prober cleanup frame at 01:35:54 UTC still warned: 861,642
mature children, 861,626 inactive and 16 active disconnected, all
never connected. Another 876 active children were newer than the
ten-minute grace. No deactivate timestamp was missing; the oldest
residual was about 18,935 seconds old. The eleventh PostgreSQL CPU
signal had no sealed Alert, so its numerical value remains unknown
here rather than zero or healthy.

The full Work race package on the committed final source passed in
437.135 seconds while Model bucket zero normal continued in a separate
fixture lease. Work normal and race are now green on the repaired source;
the four Model normal and race partitions remain.

A separate local TLS timing regression then found that the custom
provider tunnel's completed handshake emits two start/done callback
pairs. The request progress clock overwrote the first start, recording
zero TLS duration for a synthetic seven-second handshake. Unchanged
source failed only the completed custom-handshake control; standard
transport and failed-custom controls passed. A private patch retaining
the first TLS start/end passed normal and race controls. Source review
shows this duration feeds the `tls_ms` diagnostic field; provider verdict
performance gates use written-request TTFB/body measurements instead.
The Model bucket zero passed in 1,176.298 seconds as a pre-TLS baseline
only. The tracked correction now retains the first TLS start and end
under the existing mutex. Its durable real-transport regression passed
normal and race. Both owning packages,
`qualityprobe/egresshealth` and `qualityprobe/providertunnel`, passed
focused normal and race suites; vet for them, Model and Work passed.
`tls_ms` is diagnostic evidence and is checked for finite/nonnegative
encoding, so this repair does not itself change provider verdicts or
quota capacity. Model and Work import the changed package and require
renewed broad gates on a dependency-complete final source digest.

The twelfth watcher began at the 01:42:35 UTC cadence. Its early
catalog frame at 01:43:23–28 still found migration head 731 against
740 and the old non-covering reliability parent, with no desired
covering parent or child indexes across 34 partitions. At 01:45:48,
edge-3 and edge-4 again paged for CPU saturation: one-minute loads
214.34/72 and 211.85/72 logical CPUs, five-minute execution ratios
0.9324 and 0.9392, zero iowait, and available-memory ratios 0.4805
and 0.4533. Taskworker DoH dial timeouts also paged at about 11/min.
These are source-fresh host/transport symptoms, not a process or
provider verdict; the twelfth FP2 and PostgreSQL frames were pending.

The twelfth picker at 01:50:07 UTC still paired only 20/25 API slots.
Its sampled initial outcomes were 191 nonempty, zero empty and one
error; search 32 nonempty and one empty, with no direct result and one
read-initial error. Separate request-local US IPv6 Quality probes
remained effectively empty, and RU Quality small-list findings persisted;
the subset metrics do not supply a full-fleet explanation. The 01:50:24
URL frame again paged: 109,806 eligible, zero quota-complete and zero
secure-complete, 88,149 due, 109,554 overdue, 908,880 accepted
successes needed and oldest due about 19.1 hours. Hourly throughput
and process coverage were incomplete. The 01:51:03 prober cleanup
frame had 847,298 mature children, 847,282 inactive and 16 active
disconnected, all never connected; none lacked a deactivate timestamp.
The PostgreSQL idle-in-transaction frame counted 574 such sessions,
with oldest transaction age about ten seconds and 59 active sessions.
The twelfth PG CPU value was not yet sealed. These observations keep
product, lifecycle and DB load findings open without assigning one
common cause.

A bounded all-eight Taskworker progress read at 01:52:51 UTC found
fresh, finite counters/gauges and complete five-minute ranges with
no resets. Summed per-process sampled means were about 473 active
batches of the configured 512 lanes, 426 running, three queued and
44 finished waiting; a separate current scrape totaled 283 active,
including 262 running. Provider-finished and batch-finished counters
rose by about 8,534 and 8,563 over five minutes, respectively. This
rejects a mostly idle claim scheduler during that sample. `running`
includes setup, network work and teardown; `finished_waiting` spans
post-return guard/publication/release. The gauges do not isolate DNS,
HTTP, database or API time, and per-process sampled extrema are not
simultaneous fleet bounds. With Main edge and PostgreSQL load still
high, raising concurrency is not a justified first correction.

The TLS timing repair was committed directly on Server `main` as
`99cecad8`, pulled with no incoming changes and pushed. The local
migration adapter rebuilt from that exact checkout still has executable
SHA-256 `ab1fd25b5ffa42595dd9f75718f70723e853ab14272db7e46a93064716345152`
and 732–740 manifest SHA-256
`2448e0b0242f52665dc3cc9be2d5cd19ad211e2b1539c40696c6a7823942833b`.
The new final test manifest includes all first-party transitive Model
and Work dependencies: 1,191 disjoint Model tests in four buckets,
manifest SHA-256
`aefed8367dfed60d42181aa9a0ed42cba9ba549ecaf75372580f75d3d35b9d09`,
and 1,069 Go files across 26 package directories under digest
`afa4b53c02a38e4d97efc71578412f02b3b3936c416468ef8c1b795d868ff1cb`.
Owning egress-health/provider-tunnel normal/race suites and relevant vet
passed. Full Work normal on this committed source then passed in
338.943 seconds alongside Model bucket zero normal; Work race and the
Model broad gates remain. No Main schema or service mutation occurred.

A bounded paired first-byte log reduction at 01:58:04 UTC narrowed URL
lane occupancy. The newest 512 matching summaries hit the cap across
six container streams and spanned 55.58 seconds. Among 340 successful
one-check summaries, mean encoded DNS phase was 18.284 seconds versus
1.115 seconds from DNS completion through actual first response byte;
DNS accounted for 94.25% of their summed first-byte time. Median DNS
was 18.467 seconds and p95 37.907 seconds. Another 133 failures with
no response byte averaged 44.098 seconds of DNS phase; 39 failures
with a byte also had 94.71% DNS share. The remainder derives from
actual first-byte timing, independent of the corrected `tls_ms` field.
This capped six-stream prefix is not a fleet census or an accepted-
history join; setup before the DNS clock, teardown and body time sit
outside this split. It identifies target-resolution phase as the
dominant observed first-byte cost, while the specific tunnel/DoH
substage and safe remedy remain under investigation.

The thirteenth watcher early log frame at 01:57:36 UTC still paged on
Taskworker panic-shaped lines, about 130/min, with recognized owner
`server/model.ForceCloseOpenContractIds` and error type
`*errors.errorString`; PostgreSQL SQLSTATE and process-crash status
were not established. The aggregate and owner buckets overlap and
must not be added. A separate 01:58:36 escrow-reconcile WARN recorded
a failed 125-second run with `postgres-statement-timeout` on edge-1/g1
at 01:36:54. Its phase and version impact require their own check.
Thirteenth product, host and PostgreSQL frames were still pending.

The thirteenth 02:01 UTC host frame kept edge-3 at load-1 134.73/72
logical CPUs with five-minute execution ratio 0.9042, and edge-4 at
222.43/72 with ratio 0.9322. Eight Connect process alerts persisted;
edge-4/g2 reached 145.61 GB RSS, 949,059 goroutines, a 6.113-core
five-minute CPU rate and about 212.75 MB/s allocation. PostgreSQL
idle-in-transaction count was 599 with oldest transaction about nine
seconds and 28 active sessions. These observations do not apportion
host CPU to Connect, identify goroutine owners or establish a leak
slope. Thirteenth URL, picker and PostgreSQL CPU results were pending.

Full Work race on the committed TLS source passed in 440.278 seconds.
Together with Work normal (338.943 seconds) and final-source vet, the
Work release gate is green; the four full Model normal/race partitions
remain.

The thirteenth PostgreSQL CPU frame at 02:02:45 UTC measured 46.631
of 96 logical-core equivalents (48.57%) over a paired 5.02-second
service-cgroup sample, with one active PostgreSQL service and matching
host/unit identity. It is continuing elevated database consumption,
not a query or service attribution or a quota-normalized peak.

The subsequent local causal controls exposed two separate admission
boundaries. A finite 25 ms Connect forwarding caller waited 275.727 ms
behind an unrelated producer, and a canceled sibling remained queued;
the single-caller 25 ms control passed. The private Connect correction
passed all three controls. With that correction alone, Server
`AddForward` still waited for resident-wide admission after close;
the combined Connect and Server correction passed its cancellation
control. Raw synthetic logs remain private; aggregate receipts are
sealed in the append-only monitor ledger. These controls do not measure
Main incidence or establish that the corrected artifacts are deployed.

A separate private DNS-route timing overlay had the expected
diagnostic-unavailable baseline RED, then passed warm, cold, unready,
lost and ambiguous route controls, full provider-tunnel normal/race,
and Work fixed-cardinality normal/race. Its added timing observation
preserves the measured DNS outcome, attempt count and socket behavior;
the baseline RED is not a DNS correctness failure. Both scoped source
changes are applied locally. Focused Connect admission and idle-close,
Server resident, and Work DNS-cardinality normal/race tests passed.
The private DNS overlay passed full provider-tunnel normal/race tests;
the combined tracked source passed egress-health and provider-tunnel
full normal tests, with their race gate pending. Affected-package vet
passed. A new transitive manifest
covers both the Server and Connect modules: 1,191 Model tests in four
disjoint buckets, manifest SHA-256
`5861e4e6cc0712a91b656b4741ad73876a0def2cea3551900083ef44c34d3a31`,
and 2,218 Go files across 32 package directories under digest
`1eef07a4f53b56399da7ff4d79cfca6462abb840a22dd5b38bbd297cec776c06`.
Broad Model and Work gates on this combined source remain pending.

The initial unpartitioned full Connect-module and Server `./connect`
normal gates each exhausted a 15-minute package timeout while later
tests were active; neither timeout is a product assertion. Four earlier
Connect package-discovery tests also failed because this Linux PATH
lacked `zsh`. A genuine local zsh 5.9 binary made that focused group
pass. Explicit disjoint Connect-module (4,420 tests, eight buckets)
and Server `./connect` (413 tests, four buckets) manifests now allow
bounded sequential normal/race gates with the genuine shell and
adequate per-bucket timeouts. The first Connect bucket is running;
its result and all subsequent broad gates remain open.

The promoted Main watcher continued at its 15-minute floor. At 03:21:36 UTC,
the eighteenth active frame could not produce a fresh coherent URL global
census: shards 0, 1, 2, 5, 6 and 7 each had two fresh capable heartbeat
owners. Its current eligible and quota counts were **unknown**, rather than
zero. The preceding seventeenth frame measured `quota_complete=0` among
109,667 eligible providers, and the nineteenth frame again measured zero
among 110,763 eligible providers through its same-cohort TLS quarantine
observation; the twentieth measured zero among 109,823. The observed duplicate
heartbeat is an ownership visibility gap. Those frames alone do not identify
the individual processes or prove that an old completed pass caused it.

A later bounded, privacy-reviewed discriminator resolved that observed wave.
For each of the six duplicate shards, the predecessor's pass-completion
counter advanced at its terminal heartbeat scrape and its heartbeat then
stopped changing; a successor began advancing 7.37–11.03 seconds later. A
03:28:13 UTC read of the eight exact RunOnce advisory keys found one session
per shard. This explains the sampled dual-heartbeat episode as retained
completed predecessors plus active successors, without evidence of two live
RunOnce holders in those samples. The advisory read was later than the watcher
frame and not a process/image join; 15-second Mimir samples cannot exclude a
short transient overlap or unexpected task keys outside the exact-eight read.
The sealed receipt is SHA-256
`b413417a73e8dada2617435326ef4604758d35e66e61b628dd4a7ce3b739d64e`.

The Work source did expose a sufficient stale-owner mechanism. The former URL
wrapper refreshed a shard heartbeat during the pass but left its last nonzero
timestamp published on normal completion; the shared refresher's cancel/join
also sat after `run()` and was skipped when a task panic unwound through it.
A completed pass could therefore remain a fresh owner for up to the monitor's
180-second acceptance window, even while another pass owned that shard. A
private deterministic baseline reproduced retained and republished completed
heartbeats on success, error, cancellation and panic, plus final-owner handoff.
The corrected Work wrapper joins refresh on every exit and publishes zero
only after the last local invocation for that shard retires. The same controls
passed on the private fixed overlay and the tracked source in normal and race
modes; full Work normal/race, Taskworker normal/race, Taskworker CLI build and
vet passed after the tracked change.

Zero publication is observable only if the process exits normally enough to
emit it and Mimir scrapes that sample. A crash, missing scrape, or old process
series can still leave ownership ambiguous, so the monitor's unique-fresh-owner
and coherent-census guards remain unchanged. The local correction has not
been observed in a deployed Taskworker artifact; Main URL quota and accepted
history must be remeasured after rollout, separately from heartbeat ownership.

At the latest local release-gate checkpoint, full egress-health and
provider-tunnel normal/race suites, their vet checks, and the tracked DNS
fixed-cardinality controls are green. After the Work-only heartbeat change,
full Work normal/race, Taskworker normal/race, the Taskworker CLI build and
vet are green. Model normal buckets zero and one are green; bucket two is
running, with bucket three and all four race buckets still required. The
Connect-module eight-way normal partition is green across all 4,420 tests.
Its final bucket initially failed an existing global message-pool root check;
a controlled unrelated owner holding ten roots reproduced that exact
assertion with zero forwarding admissions on both old and new production
Connect code, while an isolated child passed the unchanged warm/cold protocol
checks. A seven-line test-only fresh-process isolation passed normal/race
controls and the final normal bucket. Production Connect transfer code did not
change during this correction. Connect race buckets are now running.

The earlier four-way Server `./connect` partition had a cumulative 15-minute
timeout without product assertions. It was superseded by a verified disjoint
eight-way manifest for all 413 top-level tests, SHA-256
`5a3819852dd53a3f67d4ecbfc49d2a08e5a2ef276d7c1b4634fd810a07078ad5`.
Its first two normal buckets are green, the third is running, and the remaining
normal/race buckets are still required. This checkpoint does not declare a
complete Connect or Model release gate. Main's latest measured migration head
remains 731 versus required 740; no Main migration, service rollout or
non-versioned covering-index maintenance has been performed by these tests.

At the 05:20 UTC release-gate checkpoint, all four disjoint Model normal
partitions are green on the combined source. Model race buckets zero and one
are green; bucket two is running and bucket three remains. The full Work and
Taskworker normal/race suites, Taskworker CLI build, and owning vet gates are
green after the URL-heartbeat correction.

All eight Connect-module normal partitions are green. Race bucket zero is
green. Bucket one had a 35-minute cumulative timeout while the SDK profile
test was actively running, plus a strict 1 ms scale assertion at 1.28036 ms
of process CPU under the broad run. The unchanged 100,000-peer scale test
passed focused normal and race controls at 0.197415 ms and 0.65924 ms,
respectively. A private disjoint split preserves all 540 bucket-one tests:
the scale control is green, the SDK profile is running alone, and the other
538 tests remain. No Connect production-code change follows from that gate.

Server `./connect` normal buckets zero through three are green; bucket four
is running, with buckets five through seven and all race buckets still
required. Bucket three originally failed a missing local `nginx` executable
and then exhausted its 25-minute package timeout with a performance test
active. A privately built, checksum-verified NGINX 1.31.4 with the required
stream module made the focused proxy test pass; the exact same bucket-three
selection passed with that binary on `PATH` and a 45-minute bound. No system
NGINX service or product source was changed.

Main measurement and release remain separate from these local gates. The
authoritative watcher continues at the 15-minute floor, and the twenty-fifth
active frame still measured migration head 731 versus required 740, the old
non-covering reliability index, and `quota_complete=0` among 99,948 currently
eligible URL-probe providers; hourly throughput and API process coverage were
incomplete. The operator confirmed that deployed host network values are the
current truth, edge5 is offline with `RunWorker` stopped, and
`xops/main/ansible/run-edges.sh` must not run because it includes staged router
changes. Edge5 needs independent artifact, migration-compatibility, worker,
and signal verification when it returns. Main migration, rollout, and
non-versioned covering-index maintenance are pending.

The twenty-sixth watcher frame kept those Main release limits in view:
`quota_complete=0` among 99,938 currently eligible URL-probe providers,
hourly throughput still incomplete, and migration head still 731. Its
provider-selection cache and eligibility marker became source-unobservable;
that is an unknown measurement, not evidence that the cache emptied. The
watcher measured PostgreSQL at 42.782 of 96 host logical CPU cores over its
five-second sample, without a cgroup-quota or query-owner attribution.

The local 05:33 UTC host out-of-memory event interrupted the former watcher
and retained local tests and portable PostgreSQL/Redis. The last accepted
active Main frame was at 05:24:37 UTC. A single recovery watcher started at
08:54:57 UTC with ten live service log tails and a 15-minute minimum active
cadence; its first active pass is due no earlier than 09:09:57 UTC. The interval
after the last accepted frame is a coverage gap, and any sustained recovery
window resets. Watcher startup alone does not revalidate migration head,
quota, process coverage or PostgreSQL CPU. Private portable PostgreSQL and
Redis restarted at 08:56 UTC on loopback ports 25432 and 26379. Broad Model
and Server Connect release gates remain pending the native-reader activation
change and sequential retesting on final source.

The local native-reader activation gate is tracked with default-off
`provider.yml` configuration, one request settings snapshot shared by primary
and alternate loads, and one unlabeled effective-state gauge. Its unchanged
source causal control failed as expected for absent, false, malformed and
mid-request changed settings; fixed focused normal and race controls passed
in 31.904 and 45.705 seconds, and owning Model/Work/Server Connect vet passed.
The source and test receipt is sealed as SHA-256
`b22b989b1399cebe68971dba973e4e748e2c2ed823bcfe25f00a58c018a0c18b`.
The refreshed Model inventory contains 1,194 tests, including three new
activation controls. Broad Model, Work, Server Connect and Monitor gates must
be rerun on this source. No Main deployment or configuration publication has
occurred.

The staged activation order is migration head 740 and verified catalogue;
receipt-capable APIs with native reading off; converged completion-capable
Taskworkers and native publisher with complete target/facet census; then a
separate reviewed API configuration/version rollout that turns native reading
on and verifies every participating process. Completion-priority activation
remains separate until a full four-hour delivered-receipt window is proven.
The private review plan is `fp2-native-reader-gate/ROLLOUT.md`; it preserves
the operator prohibition on `xops/main/ansible/run-edges.sh` and treats deployed
host values as authoritative.

The Connect 100,000-peer scale test had also counted unrelated threads in its
process CPU budget. A test-only exact-root child now measures the same peer
work under the unchanged 1 ms assertion. The old test reproduced a failure
with a joined foreign CPU owner; fixed foreign-owner normal/race and plain
normal/race controls passed, and the latter measured 113.595 and 571.82
microseconds. The sealed receipt is SHA-256
`4c8a567d5dfc3e82ac3c72b6462f98f4c3042001095b771df20483d28cd30b2d`.
This attributes a test accounting error; it does not identify the owner of the
earlier broad-run 1.28036 ms measurement. Connect library race gates on the
final tracked test source remain pending.

The recovery watcher's first active wave began at 09:10:04 UTC and had settled
by the 09:22:53 UTC scoped check, with ten fresh standing log collectors. The
coverage gap from the last accepted 05:24:37 UTC frame ends at that conservative
first-wave bound; sustained-health accounting starts anew. Main still measured
migration head 731 versus 740. Its coherent URL cohort contained 102,419
eligible providers with `quota_complete=0`; hourly throughput and expected
process coverage remained incomplete. Picker counters covered 20 of 25
expected API slots, with 426 initial nonempty outcomes, six initial errors and
four initial read errors. A fresh PostgreSQL CPU value was not visible from
this first wave after the sustained-alert reset, so CPU recovery remains
unknown. These first-wave observations do not establish rollout readiness.

The second recovery wave began at 09:25:08 UTC. Its scoped URL census still
showed `quota_complete=0` among 103,012 eligible providers and paged; hourly
coverage remained separate. PostgreSQL CPU warned at 51.426 of 96 logical
cores over 5.02 seconds, establishing sustained high service consumption
without identifying the query owner or normalizing for a cgroup quota. Picker
observation remained incomplete at 20 of 25 expected slots, with five initial
errors and three read errors. Migration head was still 731. Ten standing log
collectors remained fresh; their independent one-minute reconciliation and
bounded log reads continue after the active product signals, so a transient
extra `warpctl` child is not itself a duplicate watcher or probe.

An offline API heap review kept ownership unresolved. Historical exact-process
FP2 stage counters put 97.41% of completed decision residence in the selector,
but this is neither CPU nor heap ownership. Variable candidate `Count` and
pool work are source hypotheses; no retained Main `Count` distribution or
exact stats-instance binding was found. A bounded numeric-only comparison of
already existing samples is prepared, with actual sample enablement unknown;
the default 1% rate would yield only about 13 conditional samples over the
historical 15-minute target window. Its encoded count has int32 and
post-filter primary-pool limits. The sealed offline review is SHA-256
`976f6c248b0b22072c9008a7c90fc4b9955569fbbe93f784664bb1008dddb754`.
No new Main contact, profile, collector run or product fix follows from it.

The final tracked Model dependency source now has a 1,194-test inventory across
16 disjoint, resource-bounded normal buckets. All 16 exited successfully on
the same 2,223-file/32-package source digest
`322db4a3793eaf058337b063a0ff4972c0fab95cf03c5caac629832dde9a71e5`:
1,379 reported test runs passed, zero failed, and six top-level tests skipped.
Five skips require the optional `pro.yml` absent from the private fixture; one
requires a sibling operator-proxy checkout. The 1,024-client proxy test passed
in 128.05 seconds but reached about 5 GiB RSS before GC, so its race gate is
isolated in its own capped process. The previous combined Model race bucket
that ended in a host OOM is not counted as a product assertion or a completed
race gate. Final-source Model race, broad Work, Server Connect and Monitor
gates remain open.

The third through fifth recovery waves independently kept the Main blockers
open. Migration head stayed 731 against required 740. The three coherent URL
cohorts each had `quota_complete=0` among 103,013, 103,032 and 103,032 eligible
providers; picker coverage stayed incomplete at 20 of 25 expected API slots.
PostgreSQL warned at 53.656, 59.092 and 58.429 of 96 logical cores over
paired five-second service samples, without a query-owner or cgroup-quota
attribution. The fifth wave's one separately authorized read-only matched
CPU-owner diagnostic failed closed with incomplete host/query output and no
aggregate attribution; it was not retried. These scoped records are in the
append-only monitor ledger and do not establish sustained recovery or a
deployed release. The single watcher and its ten standing tails remained live
on the 15-minute floor.

An isolated race run of `TestCreateProxyClient` hit its 9 GiB memory cgroup
and 1 GiB swap cap while its fixture queued 10,000,000 proxy IPv4 addresses,
before client assertions began. The host, watcher and portable test services
survived. This is an incomplete resource gate, not a product assertion failure;
the remaining Model race buckets are running serially while a bounded private
fixture receives focused normal and race controls. The sixth scheduled wave's
one authorized matched PostgreSQL owner read also failed closed: its host
sampler exited successfully, but the SQL helper returned SQLSTATE 22P02 in the
bounded-read phase. It supplied no query-owner attribution and was not retried.
The private receipt is recorded in the append-only monitor ledger.

The sixth scheduled recovery wave still found migration head 731 against 740
and PostgreSQL service CPU at 36.937 of 96 logical cores over five seconds.
Its URL probe coverage lacked a fresh coherent global census, so it supplied
no eligible-cohort or quota-complete count. Picker observation was incomplete
at 20 of 25 expected API slots, with 569 initial nonempty outcomes, five
initial errors and five initial read errors in the observed subset. The scoped
sixth-wave record in the ledger preserves those unknowns; it does not prove
recovery, deployment or PostgreSQL query ownership.

The proxy-client test fixture now seeds only the 1,024 addresses needed for
its 1,024-client lifecycle check. The tracked test-only change matched its
private overlay byte for byte; focused normal and race runs passed, including
all client iterations. Production IPv4 reset behavior is unchanged. This edit
was committed as Server `daf30bc7` after the 16-bucket Model normal gate and
the first race bucket, so those earlier broad receipts are historical source
evidence. A refreshed 1,194-test inventory has identical test names, and new
final-source normal and race gates remain pending. The source manifest and
focused receipt are recorded in the ledger.

The seventh wave's single authorized matched PostgreSQL diagnostic completed
over 6.243 seconds. Its service unit used 269.415 CPU-seconds (43.154 cores).
Stable interior process samples covered 151.77 CPU-seconds; 117.645 unit
CPU-seconds lay outside that coverage. Only 3.23 CPU-seconds had a qualified
unique join to the raw-reliability statement family. Changing or inactive
queries, missing activity ownership and timing gaps leave the larger share
unattributed. This sample supports a narrow observed subset, not an upper
bound or a total query-owner conclusion; statement elapsed-time deltas are
not CPU time. The read-only aggregate receipt is sealed in the monitor ledger.

The settled seventh watcher frame still had migration head 731 versus 740.
Its independent five-second PostgreSQL service sample measured 37.522 of 96
logical cores. A coherent URL census had 103,531 eligible providers and zero
quota-complete providers, while hourly success and expected-process coverage
remained incomplete. Picker observations paired 20 of 25 expected API slots,
with 455 initial nonempty outcomes and four initial read errors in that
subset. These are scoped observations, not a sustained or deployed result.

Offline analysis of the seventh-wave matched sample linked a legacy
settlement query fingerprint to pre-fix repository source: 6,533 completed
calls had 226.64 seconds of summed SQL elapsed time in a 7.12-second counter
window. Of 104 observed parallel-worker rows, 68 joined a matching leader in
the same snapshot, 16 carried their own matching query identity, and 20
remained unjoined. The reviewed settlement query fix is already an ancestor of
the current source. Worker membership and SQL elapsed time do not establish a
CPU fraction or the emitting service; the deployed modified binaries are
unresolved. This analysis used retained private evidence without another
Main read, and the eighth wave has no extra diagnostic scheduled.

The settled eighth watcher frame still had migration head 731 versus 740,
PostgreSQL service CPU at 39.501 of 96 logical cores over five seconds, and
zero quota-complete providers among 103,533 in a coherent URL census. Hourly
URL throughput/process coverage remained incomplete. Picker observations
paired 20 of 25 API slots, with 488 initial nonempty outcomes and nine
initial read errors in that subset. The watcher kept the same PID and ten
standing tails after a same-name user-systemd restart drop-in was attached;
its persistent boot unit and lingering user are staged, but a live restart
and boot recovery were not exercised. No extra Main probe was run.

An offline review found no new source defect in the staged 732–740 migration
path. It does not clear the production apply: the last detailed admission
preflight is stale, and every concurrent index build in 733–739 needs fresh
old-snapshot, backup, resource and catalog checks. A caller deadline can
precede the detached commit's terminal state by up to 30 seconds, so a failed
call cannot be blindly retried. Migration 740 forces client 5-minute, 1-hour
and 12-hour observation reanchors; it does not by itself force the seven-day
network reanchor. The numbered migration review receipt is in the monitor
ledger; Main remains at 731 and no migration was applied.

Offline URL review separates a durable quota deficit from a visibility gap.
At the coherent 10:46 census, 103,531 eligible providers had 366,522 current
qualifying successes in total, an average of 3.54 each; none had the required
ten. Holding that cohort at quota needs at least about 71.90 accepted unique
successes per second across a four-hour window. Local desired Taskworker
placement has ten slots, while monitor inventory disables one two-slot host;
the two desired gaps keep hourly/process coverage incomplete until placement
intent or authorized reachability changes. Current heartbeat retirement and
range-only counter fixes address visibility, not durable quota credit. Exact
remote effective placement, DNS/route owner and throughput remain unproved;
the offline receipt adds no Main contact or code change.

The ninth scheduled watcher frame kept migration head at 731 versus 740.
Its independent five-second PostgreSQL sample measured 44.566 of 96 logical
cores. A coherent URL census had 103,534 eligible providers and zero
quota-complete providers, while hourly success/process coverage remained
incomplete. Picker observation paired 20 of 25 API slots, with 537 initial
nonempty outcomes, 13 initial errors, and seven initial plus two filter read
errors in that subset. The single watcher stayed on its original PID and
none of these scoped observations establishes sustained recovery.

An offline API heap review still found no allocation owner or production
patch. Ordinary location reads share the map; sampled occupancy and completed
request residence have different denominators, so completed means cannot
bound unfinished or canceled request work. Exact-process artifact and reader
state after rollout should be checked first. A separately reviewed bounded
identity-free live-work and SearchLocal structure diagnostic is an option if
that still leaves the tail unexplained; it has not been implemented, deployed
or used against Main.

Post-rollout runtime artifact proof has two separate parts for the reviewed
48 enabled API, Connect and Taskworker slots: two fresh matching exported
source-tuple observations per slot, followed by independent bounded reads of
the running container image and executable bytes on all four enabled hosts.
No authenticated host-local Docker/process read path has been verified for
the full host set. Edge0 overlay trust, edge1 noninteractive sudo, edge3
direct reachability and edge4 authenticated key remain distinct gaps; no
relay is verified. The offline procedure is sealed in the monitor ledger,
but the all-slot collector has not been implemented or run. Exported version
labels alone cannot establish deployed binary identity.

The tenth scheduled watcher frame again found migration head 731 versus 740.
Its five-second PostgreSQL service sample measured 44.994 of 96 logical
cores. A coherent current URL census had 103,067 eligible providers and zero
quota-complete providers; hourly success/process coverage remained
incomplete. Picker observation paired 20 of 25 API slots, with 432 initial
nonempty outcomes, nine initial errors, and eight initial plus one filter
read error in that subset. The eligible cohort changed from the prior frame,
but the cause is not established. The single watcher kept its PID and ten
standing tails; no probe or deployment was added.

The eleventh scheduled frame still found migration head 731 versus 740.
PostgreSQL service CPU measured 51.047 of 96 logical cores over five seconds.
A coherent URL census had 103,065 eligible providers and zero quota-complete
providers, with hourly throughput/process coverage incomplete. Picker
observation paired 20 of 25 API slots, with 465 initial nonempty outcomes,
three initial errors, and two initial plus one filter read error in that
subset. Bounded watcher children settled before the next cadence floor; the
single watcher retained its PID and ten standing tails. These observations
do not show sustained recovery or a deployed release.

An offline settlement-hotfix review found that the current combined API,
Connect and Taskworker images all require migration head 740 at startup;
leaving native reads off does not bypass readiness or their real schema
dependencies. The reviewed settlement-query change could be backported into
an isolated older source at head 731, but no release-ready hotfix exists.
Historical dirty service inputs and replacement modules remain unknown, and
such a candidate would need its own exact-source tests, builds, artifact
proof and all-caller rollout. Current-main gates cannot certify it. The
emitting service and CPU share remain unproved, and no Main change was made.

The refreshed final-source Model race gate passed all 16 serial buckets on
the committed bounded proxy-client fixture. It reported 1,379 passing runs,
zero failures or race warnings, and six optional top-level skips: five need
the absent `pro.yml`, while one needs a sibling operator-proxy checkout. The
1,024-client proxy test passed inside its full race bucket; the earlier
10-million-address cgroup OOM remains historical resource evidence. The
source digest was reverified after the gate, and every bucket exited zero
under its 9 GiB memory cap. Final-source Model normal and the other owning
package gates remain separate.

The twelfth scheduled frame still found migration head 731 versus required
740. PostgreSQL service CPU measured 47.842 of 96 logical cores over 5.02
seconds; this sample does not identify a query owner. A coherent URL census
had 103,539 eligible providers and zero meeting the ten-result quota, while
hourly process and throughput coverage remained incomplete. Picker
observation paired 20 of 25 API slots, with 602 initial nonempty outcomes,
23 initial errors, and 21 initial plus one filter read error in that subset.
Bounded watcher children settled by 12:09:42Z; the single watcher retained
its PID and ten standing tails. This is scoped observation, not sustained
recovery or a deployed release.

A private no-DB Connect accepted-handler control passed all four current-source
closure, sibling and final-zero gauge cases in both normal and race modes.
Omitting the Server caller context reproduced caller and sibling gauge
retention after stream close while healthy controls stayed green. The older
Connect mutex counterfactual timed out in its private test harness, so it
has no clean causal verdict; the combined variant was stopped. This control
made no tracked change or Main contact and does not identify production CPU
ownership.

The thirteenth scheduled frame again found migration head 731 versus 740.
PostgreSQL service CPU measured 53.716 of 96 logical cores over 5.03
seconds, without query attribution. URL coverage could not report a coherent
current cohort: shards 0, 1, 3, 4 and 7 each had two eligible owner
candidates, so current eligible and quota counts are unknown, not zero.
The aggregate cannot distinguish cross-slot handoff from a freshness or
scrape artifact; a private offline triage manifest records the source
filters and limits. Picker observation paired 20 of 25 API slots, with 433
initial nonempty outcomes, nine initial errors and eight initial read
errors. The watcher retained its PID and ten standing tails, and the next
scheduled wave began at 12:25:09Z. No Main mutation or extra probe occurred.

An offline follow-up of that URL alert confirmed the two candidates are
distinct configured-slot metric candidates after the monitor's freshness
and per-slot generation checks. Ordinary shard passes can move between
unchanged processes; an earlier frozen-predecessor mechanism is compatible
but not proved in this frame. The current retirement source and tests still
match their prior causal receipt. A bounded retrospective discriminator was
designed but not run. No new source defect or current quota value was proved.

The fourteenth scheduled frame kept migration at 731 versus required 740.
PostgreSQL service CPU measured 48.394 of 96 logical cores over 5.02
seconds, without query attribution. URL coverage regained a coherent
current census: 102,330 eligible providers, zero quota-complete, and
683,587 accepted successes needed; hourly process and throughput coverage
remained incomplete. Picker observation paired 20 of 25 API slots, with
454 initial nonempty outcomes, 13 initial errors, and ten initial plus one
filter read error. Bounded watcher children settled by 12:37:29Z, with
the same watcher PID and ten standing tails. This frame does not repair the
prior unknown census, prove sustained recovery, or show a deployed release.

The fifteenth scheduled frame still found migration head 731 versus 740.
PostgreSQL service CPU measured 50.886 of 96 logical cores over 5.02
seconds, without query attribution. A coherent URL census had 107,767
eligible providers and zero quota-complete providers, while hourly process
and throughput coverage remained incomplete. Picker observation paired 20
of 25 API slots, with 486 initial nonempty outcomes, seven initial errors,
and six initial plus one filter read error. Bounded watcher children settled
by 12:53:04Z; the watcher retained its PID and ten standing tails. This
scoped frame does not establish sustained recovery or a deployed release.

The sixteenth scheduled frame kept migration at 731 versus required 740.
PostgreSQL service CPU measured 48.129 of 96 logical cores over 5.02
seconds, without query attribution. A coherent URL census had 107,769
eligible providers and zero quota-complete providers; hourly process and
throughput coverage remained incomplete. Picker observation paired 20 of
25 API slots, with 457 initial nonempty outcomes, eight initial errors,
and six initial read errors. Bounded watcher children settled by 13:08:36Z;
the watcher retained its PID and ten standing tails. This scoped frame does
not establish sustained recovery or a deployed release.

The seventeenth scheduled frame still found migration head 731 versus 740.
PostgreSQL service CPU measured 47.633 of 96 logical cores over 5.03
seconds, without query attribution. A coherent URL census had 110,695
eligible providers and zero quota-complete providers; hourly process and
throughput coverage remained incomplete. Picker observation paired 20 of
25 API slots, with 484 initial nonempty outcomes, seven initial errors and
seven initial read errors. Bounded watcher children settled by 13:24:34Z;
the watcher retained its PID and ten standing tails. This scoped frame does
not establish sustained recovery or a deployed release.

The refreshed final-source Model normal gate also passed all 16 serial
buckets on the committed bounded proxy-client fixture. It reported 1,379
passing runs, zero failures, and the same six optional top-level skips as
the race gate: five require absent `pro.yml`, and one requires a sibling
operator-proxy checkout. All buckets exited zero under the 9 GiB cap, and
all 1,643 participating source inputs still matched the manifest after
the run. Normal and race now cover the same tracked Model source; Work,
Taskworker CLI, Server Connect, Monitor, Connect module and Main rollout
remain separate gates.

The eighteenth scheduled frame still found migration head 731 versus 740.
PostgreSQL service CPU measured 58.311 of 96 logical cores over 5.03
seconds, without query attribution; one higher sample does not establish a
trend. A coherent URL census had 110,694 eligible providers and zero
quota-complete providers, while hourly process and throughput coverage
remained incomplete. Picker observation paired 20 of 25 API slots, with
511 initial nonempty outcomes, 11 initial errors, and nine initial plus two
filter read errors. Bounded watcher children settled by 13:39:44Z; the
watcher retained its PID and ten standing tails. This frame does not show
sustained recovery or a deployed release.

Fresh final-source Work normal and race gates both passed under the serial
9 GiB cap: each reported 451 passing runs, zero failures, and one optional
`TestDeriveLocationsAtAFractionOfTheTarget` skip; the race run reported no
race warning. All 757 compiled first-party input files still matched the
manifest after both modes. Work imports the changed native-reader Model
production files, so an earlier broad Work receipt could not be reused;
the later proxy-client Model test fixture is outside Work's compiled inputs.
Taskworker CLI and the other owning package gates remain separate.

The final-source Taskworker package passed all 19 tests in both normal and
race modes with no skip, failure or race warning. Its CLI built into a
private local binary, and owning vet passed with empty output. All 698
compiled first-party input files still matched the manifest after these
serial gates. The binary has not been packaged into an image or deployed;
Server Connect, Monitor, Connect module and Main rollout remain separate.

The nineteenth recovery watcher frame began at 13:40:09Z and settled before
the next scheduled frame at 13:55:09Z. Main migration remained 731 versus
required 740. A five-second PostgreSQL sample used 39.511 of 96 cores,
without query or service ownership attribution. The coherent URL census at
13:50:17Z found 112,039 eligible providers and zero quota-complete providers;
hourly process and throughput coverage remained incomplete. Picker observation
paired 20 of 25 API slots, with 496 initial nonempty outcomes, nine initial
errors, eight initial read errors and one filter read error. The same watcher
PID and ten standing tails persisted. This scoped frame is neither a release
verification nor evidence of sustained recovery.

The twentieth scheduled recovery frame began at 13:55:09Z. Migration stayed
at 731 versus required 740, and PostgreSQL used 37.266 of 96 logical cores
over an independent five-second sample without query ownership attribution.
The URL signal found two fresh owner candidates on shards 0, 4, 6 and 7,
so its current eligible and quota counts are unknown; hourly coverage is
also incomplete. Picker observation paired 20 of 25 API slots, with 538
initial nonempty outcomes, 14 initial errors and 12 initial plus two filter
read errors. Scheduled release-builder provenance was unobservable on five
remote targets, and backup-archives observation failed; neither proves
release identity or a fresh backup. The same watcher began its next scheduled
frame at 14:10:09Z. A bounded offline comparison to the thirteenth frame
found recurring ambiguous shard candidates but no proof of persistent
processes or simultaneous task execution. No Main rollout or extra monitor
probe followed from this frame. The bounded offline follow-up is sealed as
SHA256 `c65de955ca91e42fa782b02259daae4c286594f12980d9d370b3c02e785f442f`;
it found no new source fix or reason for another active read before staged
retirement-correction rollout.

The twenty-first scheduled frame began at 14:10:09Z with migration still
731 versus required 740. PostgreSQL consumed 38.798 of 96 logical cores in
an independent five-second sample; the query owner remains unknown. A
coherent URL census had 112,909 eligible providers, zero quota-complete,
111,540 overdue and 1,369 warming, while hourly process and throughput
coverage remained incomplete. Picker observation paired 20 of 25 API slots,
with 451 initial nonempty outcomes, 12 initial errors and nine initial plus
two filter read errors. Compared with the nineteenth coherent census,
warming rose by 1,341 while eligible grew by 870 and overdue fell by 471.
Source defines warming as incomplete cycles started within four hours;
aggregate counts do not identify provider transitions or the cause. The
twenty-second scheduled frame began at 14:25:09Z with the same watcher PID.
An offline source review (sealed SHA256
`18cfdf5efe7f661d7ffeae221afdf5be4b63de9d1e3eaf8076e535560e0fad5c`)
found that cycle tokens are seeded once, while due claims can lower the
oldest-due age before any successful measurement. The implied capped
rolling-success stock fell by 5,057 across the two coherent frames; no
new source defect or release-changing reason for another active read was
established. No extra monitor probe or Main rollout was performed.

A separate strict, read-only primary admission check completed at 14:24:05Z
and kept migration on **HOLD** (sealed receipt SHA256
`7311391cc0caa529e5908ac17af12d99e47e57602f1629a0d5ca713b105dc0f7`).
The head and catalog were 731, with the 731 ARIN index valid and ready,
candidate indexes for 733–739 absent, and no active index build, prepared
transaction, replication slot, COPY or vacuum in the
bounded snapshot. Backup service was inactive; the latest source archive
was about 41.6 hours old, and metadata alone did not prove restore or
writer authority. The source backup schedule is Sunday/Thursday, so an
inactive Tuesday unit and that file age alone do not establish a missed
scheduled run. PGDATA had about 1.00 TB available, but CPU quota and
cpuset were unobserved, and the capped transaction sample could not
exclude older NULL-xmin sessions. Every migration stage needs fresh
admission before DDL; this check performed no DDL or deployment.

A separate one-shot edge1 access check passed its hostname gate and
authenticated read-only root privilege, with Docker executable and socket
present (receipt SHA256
`48a5d31210b00ca60c16ef65c3b162f8febb01d26bb282d225828b742f9a4cd2`).
It ran no Docker command or process census, so live image and executable
identity across the enabled fleet remain unverified.

The twenty-second scheduled frame began at 14:25:09Z and again showed
migration 731 versus required 740. PostgreSQL used 36.646 of 96 logical
cores in its separate five-second sample, without a query-owner join. A
coherent URL census had 111,496 eligible providers, zero quota-complete,
111,474 overdue and 22 warming; hourly process and throughput coverage
remained incomplete. Picker observation paired 20 of 25 API slots with
527 initial nonempty outcomes, 13 initial errors, nine initial read errors
and one filter read error. Warming thus returned near its nineteenth-frame
level after the twenty-first-frame spike, but the separate aggregate
censuses do not identify the changing providers or producer process. An
offline source review sealed as SHA256
`6192af09321e3fc88db5f1d33c9cd3fa116f4025cafa8973e3eb3a8513c6f07d`
found no proved new source defect or release-changing reason for another
active read. The same watcher began frame 23 at 14:40:09Z; no Main rollout
or extra monitor probe occurred.

One separately authorized, bounded read of four exact archive-integrity
metric names in the post-core window returned a valid empty vector (sealed
receipt SHA256
`a2bc9c52416ae89d226d2213b4de809a4cb81c1bbbe6afa527c3a71de135459f`).
It did not observe integrity, check time or producer boot for the latest
September 27 source archive; zero series does not show that archive is
absent or corrupt, or that backup stopped. The source backup runs on
Sunday/Thursday; a separate archive pull is daily. A verified September 24
archive cannot certify the September 27 artifact. No archive bytes or
scripts were read, and no Main mutation followed.

The twenty-third scheduled watcher frame began at 14:40:09Z. Migration
remained 731 versus 740. PostgreSQL consumed 47.564 of 96 logical cores
in its independent five-second sample, with no query-owner attribution;
one higher sample does not establish a trend. A coherent URL census again
had 111,496 eligible and zero quota-complete providers, with 111,474
overdue and 22 warming. Those state counts matched frame 22, while due
and rolling-success deficit changed; hourly process/throughput coverage
remained incomplete. Picker observation paired 20 of 25 API slots with
586 initial nonempty outcomes, nine initial errors and seven initial plus
two filter read errors. The same watcher began frame 24 at 14:55:09Z.
These separate samples do not identify URL producer identity, provider
transitions or the PostgreSQL query owner.

The twenty-fourth scheduled frame began at 14:55:09Z. Migration remained
731 versus required 740; PostgreSQL used 37.509 of 96 logical cores in its
independent five-second sample, without query attribution. A coherent URL
census had 111,329 eligible providers, zero quota-complete, 111,305 overdue
and 24 warming; hourly process/throughput coverage remained incomplete.
Picker observation paired 20 of 25 API slots, with 502 initial nonempty
outcomes, 13 initial errors and 11 initial plus two filter read errors.
The scheduled backup-archives signal also could not observe its target.
The same watcher began frame 25 at 15:10:09Z; none of these scoped samples
proves a deployed fix or sustained recovery.

A separate bounded source-backup completion read in that post-core window
failed closed at the privilege boundary (sealed receipt SHA256
`89a90360767bf9bf5a7d308fca36df597eebd246a3fd1a7ad18b6c7e211f74fa`).
The strict edge2 SSH and hostname gate passed, but remote sudo required a
password, so the diagnostic did not start and no sidecar, journal, script
or archive bytes were read. Latest backup completion remains unknown; this
failure is not evidence of a corrupt or missed archive. There was no retry
in that wave and no Main mutation.

The twenty-fifth scheduled frame began at 15:10:09Z. Migration remained
731 versus required 740, and PostgreSQL used 42.651 of 96 logical cores
in a separate 5.02-second service sample without query-owner or quota
attribution. The URL signal found two fresh owner candidates on each of
shards 0 and 2 at 15:21:33Z, so the current global eligible and quota
counts are unknown, not zero. The amended bounded offline owner triage
(SHA256 `e053c2873b057336e6a78ad073482890b8f57221fd236aea6d9640427f11ec94`)
compares earlier coherent and ambiguous frames but has no current producer
identity or proof of concurrent work. Picker observation paired 20 of 25
API slots, with 470 initial nonempty outcomes, nine initial errors and
seven initial plus two filter read errors. The same watcher began frame
26 at 15:25:09Z. The credentialed backup read was deferred because no
safe post-core window remained; no extra Main contact or probe occurred.
An offline review (sealed SHA256
`c8abf8cccee788b45394406907ce94b26d1b44d5e91b70b1b3a82c43a74fd027`)
of that owner alert found no new source defect or release-changing evidence.
The coherent numeric frames 21–24 identify a unique shard-zero census at
their own instants, not unique ownership on every shard or continuity
between frames. Shard 2 was absent from the wave 13 and 20 duplicate lists
but appeared in an earlier retained handoff. Aggregate candidates do not
establish simultaneous work or its cause; a temporal owner read remains
deferred until a verified corrected-artifact rollout or a decision that
requires it. The review made no new Main contact.

The twenty-sixth scheduled frame began at 15:25:09Z. Migration remained
731 versus required 740; PostgreSQL used 40.486 of 96 logical cores over
an independent 5.01-second service sample, without query-owner or quota
attribution. A coherent URL census at 15:36:45Z had 112,006 eligible
providers, zero quota-complete, 111,975 overdue and 31 warming; this is
one current instant after frame 25's ambiguity, with hourly process and
throughput coverage still incomplete. Picker observation paired 20 of 25
API slots, with 563 initial nonempty outcomes, 18 initial errors and 14
initial plus two filter read errors. The same watcher began frame 27 at
15:40:09Z; these samples do not prove sustained recovery.

A separately authorized source-local backup read in the post-core window
completed once and sealed as SHA256
`9ccecf013cb5b182e8fb794fb9460d7d22f85f37ac4088d6b376cb8635ab9206`.
The September 27 unit shows a 00:00 start and successful 21:50:01 exit;
the deployed writer hash matched reviewed source, and the source archive
and strict sidecars had stable publication metadata. This supports a
recent source-backup completion, while the current ciphertext checksum,
destination copy, decrypt and restore remain unverified. The bounded historical
journal query returned no rows, which leaves retention and visibility
unknown. No archive bytes, mutation, DDL or extra monitor probe occurred.

The twenty-seventh scheduled frame began at 15:40:09Z. Migration remained
731 versus required 740. PostgreSQL used 35.790 of 96 logical cores over
an independent 5.02-second sample, without query-owner or quota
attribution. A coherent URL census at 15:51:56Z had 112,009 eligible,
zero quota-complete, 111,978 overdue and 31 warming; hourly process and
throughput coverage remained incomplete. Picker paired 20 of 25 API slots,
with 618 initial nonempty outcomes, ten initial errors and five initial
plus two filter read errors. The same watcher began frame 28 at 15:55:09Z.
The separately prepared host and backup reads were deferred because bounded
watcher SSH children remained after picker and the safe cadence margin
expired. No extra Main contact or probe occurred.

The twenty-eighth scheduled frame began at 15:55:09Z. Migration remained
731 versus required 740. PostgreSQL used 31.783 of 96 logical cores in
its independent 5.02-second sample, without query-owner or quota
attribution. A coherent URL census at 16:07:07Z had 108,003 eligible,
zero quota-complete, 106,910 overdue and 1,093 warming; the oldest-due
value was 4,400.6 seconds. Those state values changed sharply from frame
27, but the retained alert has no producer or process join that explains
why. Hourly process and throughput coverage remained incomplete. Picker
paired 20 of 25 API slots, with 549 initial nonempty outcomes, 16 initial
errors, three search errors and 16 initial read errors. The watcher began
frame 29 at 16:10:09Z; no separate read fit the frame-28 post-core
cadence margin.

A separately authorized, one-shot Planetoid two-file read was attempted
after frame 29 began. Its unprivileged file open hit a permission boundary,
so neither archive status file was read and backup destination integrity
remains unknown. This attempt used the user-authorized SSH host-key bypass;
the remote host's cryptographic identity was therefore unverified. The
attempt was not another active monitor probe and made no archive mutation.

A later bounded noninteractive-sudo read of the same two small Planetoid
status files succeeded once (receipt SHA256
`2c4cae479239cfe185e735fdb8385de04832743fe810ae000ccacd7a693d242b`;
sanitized result SHA256
`dfdee8418842f81f1aa09504d8f161fe73a72792093b17036b35faea7a111fb6`).
The producer reports a matching September 27 PostgreSQL generation in its
latest and integrity rows, `pg-gpg-sha256` verified about 5 hours 22 minutes
before the read, and `in_progress=0`. Stable file metadata supports a
coherent pair. This is a producer-reported recent destination check; no
independent ciphertext rehash, decrypt or restore was done, and the
user-authorized host-key bypass leaves cryptographic host identity
unverified. No archive bytes or production data were changed.

The twenty-ninth through thirty-first scheduled watcher frames began at
16:10:09Z, 16:25:09Z and 16:40:09Z, with the same watcher process and no
restart. The migration alert still saw head 731 before the separate 732
application, then independently saw 732 at 16:44:42Z. PostgreSQL used
36.729, 32.654 and 36.812 of 96 logical cores in the frames' separate
five-second samples; none identifies a query owner or quota-normalized
load. Each frame had a coherent current URL census with zero providers
meeting the ten-success quota, and hourly process/throughput coverage
remained incomplete. In frame 31, eligible providers fell from 107,998 to
107,183, warming fell from 1,085 to 24, and oldest due rose from 4,665 to
122,865 seconds. Bounded offline triage (SHA256
`7786759107b8d55adfc6d07de69e2ddc5d05abe83eca2ce9ebd034b830dbe7fb`)
found that warming can age across the refresh boundary and oldest due is a
maximum over the changing eligible cohort. The retained alerts omit the
producer identity, exact census timestamp and per-provider due rows, so
they cannot attribute that jump to a process restart, a specific provider
or a scheduler failure. Picker coverage remained 20 of 25 API slots. The
watcher began frame 32 at 16:55:09Z.

User-authorized Main migrations then reached required head **740** from
committed Server source `672063de` using the pinned migration adapter.
Independent direct-primary postflights confirmed metadata stage 732,
online index stages 733–738 with six new indexes valid and ready, stage
739 with the seventh valid and ready, and metadata stage 740 at 17:04:56Z.
The final catalog showed all seven new indexes valid and ready, the prior
731 ARIN index present, and no active index build. Stage and postflight
receipts are recorded in the primary ledger under
`main-fp2-linux-migration-732-applied-and-postflight-20260929T163033Z`,
`main-fp2-linux-migration-738-applied-20260929T170313Z`,
`main-fp2-linux-migration-739-applied-and-postflight-20260929T170425Z`
and `main-fp2-linux-migration-740-applied-and-postflight-20260929T170453Z`.
This verifies the schema stage, not service rollout or FP2 recovery; the
release build and owning gates remain separate.

The final-source Server Connect normal gates are running serially under
the 9 GiB cap with pinned local NGINX 1.31.4. Buckets 0 and 1 passed all
55 and 50 runs. Bucket 2 finished native zero before its 30-minute cap:
62 runs, 61 passes, no failure and one intentional skip,
`TestStreamRoutePerformanceComparison`, which requires the separate
`CONNECT_STREAM_ROUTE_PERFORMANCE_MEASURE=1` opt-in. Its log SHA256 is
`c5fb0fb1403168ab190362716963a1c70c2c887534bb71d207730091ef3acb89`;
all 731 first-party compiled inputs matched the frozen source manifest.
The pinned full bucket 3 remains red because
`TestConnectMultiClientTcpPerformance` completed zero of five samples,
although its NGINX-backed TCP fixture passed. A same-source isolated run
completed one of four samples and does not clear the full bucket. Private
ACK-lineage controls passed normally and under race; one 100 MiB
diagnostic completed without an ACK-lifetime exit (sealed receipt SHA256
`70513720e7b0f142ff4610ff0ff7a4b5833d312a656aeddaebbcd82423aa8859`).
That diagnostic is not a performance gate or a causal repair. Remaining
normal/race Connect and other owning release gates are pending.

At the user's direction, the long-running release tests were stopped before
the service build. Connect bucket 4 had 11 passes, one intentionally
interrupted run and 37 unstarted runs; that interruption is not a product
failure. The bucket 3 TCP performance failure remains open. The frozen
release source was Server `6ba71b7e` with clean linked repositories.

The local release build compiled and scanned the Linux binaries, then
published eight Main images: `config-updater`, `api`, `taskworker`,
`connect`, `proxy`, `mcp`, `gossip` and `alt`. Each published manifest has
both `linux/amd64` and `linux/arm64`; exact tags and index digests are in
`/home/by/urnetwork/temp/fp2-release-tools.adwfff8e/main-published-images.json`.
The config-updater image contains `restart: false`. Initial pushes that
had captured the old Docker Hub token failed with HTTP 401. The replacement
token had `pull,push` scope for all eight repositories, and the failed
pushes were retried from the already compiled and scanned build outputs.

The deployment control step is still blocked. Warpctl advanced only the
`main-config-updater:main-latest` registry alias to the new image, then
failed before updating the `deployment-blocks` DynamoDB row. Without an
AWS credential, its PutItem could not authenticate; the local Main IAM
credential received `AccessDeniedException`, and the available security
credential received `UnrecognizedClientException`. A subsequent read of
the `main-config-updater-main` row still returned
`2026.9.28-outerwerld+1057841730`. RunWorker selects the exact image
version from DynamoDB, so the alias alone is not proof of any running
service change. No service deployment or FP2 recovery is claimed. A
write-capable deployment credential is needed to resume the rollout.
The `main-latest` alias was subsequently restored to the DynamoDB-selected
old image (index digest
`sha256:1501bc8e6e69e995b77f4276d10840a6e0897220dfbbb4eae962e3ee2c104515`),
and the unchanged DynamoDB row was read back again.

On September 29 at 18:43Z, the user supplied a new local AWS credential.
A conditional PutItem preflight proved write authorization without changing
the table. Warpctl then deployed config-updater's `main` block and API's
`beta` block, followed by the remaining planned API, Taskworker, Connect,
MCP, Proxy, Gossip and Alt blocks. All 30 planned registry aliases were
created successfully. Independent Docker Hub inspection found **30/30**
alias index digests equal to the published source index digests; independent
DynamoDB readback found **30/30** deployment rows at their intended new
versions. The frozen release source is Server `6ba71b7e` and the exact
versions and index digests are recorded in the local rollout plan at
`/home/by/urnetwork/temp/fp2-release-tools.adwfff8e/main-rollout-plan.json`.
The two independent postflight outputs are
`main-dynamo-after-deploy.txt` and `main-alias-digests-after-deploy.json`
in that same directory. These results establish deployment control-plane
selection and registry publication, not complete running-host convergence.

Warpctl status polling after the updates showed mixed old/new running
versions: API beta reached 9/20 new responses by 18:46:24Z, while the
other four API blocks reached 40/80 new responses by 18:48:34Z. Those are
HTTP response samples, not counts of unique hosts or containers. A later
per-block API sample still showed beta and g1 entirely on the old API and
config versions, g2 and g4 entirely on the new API and config versions,
and g3 on the old API with new config. Direct independent container-image
reads on enabled edges 0, 3 and 4 timed out at SSH connect; edge1 reached
the expected host but denied noninteractive sudo. Thus actual image
versions on those hosts remain unverified, and a mixed status sample is
not sufficient to claim full rollout. Edge5 remains disabled/offline by
the user's instruction and was not contacted.
At 18:56:07–10Z, a bounded public-LB sample returned the new API version
with status `ok` for beta and g1–g4, but all 20 responses self-identified
as edge0. This proves a new, ready API path on that observed host only;
it does not enumerate the other enabled hosts or certify their images.
Pinned public-interface status reads at 18:58:40–44Z then preserved the
main-LB Host/SNI and received matching host identities from edges 1, 3
and 4. API beta/g1/g2/g4 and Taskworker g1 returned their new versions
with status `ok` on all four enabled edges, including the earlier edge0
read. Connect g1 still returned the old version with status `ok` on all
four; its config version was new on edges 0/1 and old on edges 3/4.
These are sampled ready paths, not a complete container census or proof
that old generations have drained. Connect's runtime rollout remains
under investigation despite its updated registry alias and DynamoDB row.
Further pinned reads at 19:00:21–24Z found Connect g2 new and ready on
edge0 while the other 15 sampled Connect paths remained old and ready.
At 19:04:21–25Z, Connect was new and ready on 3 of 20 sampled block/host
paths (edge0 g1/g2, edge1 g2); the other 17 were old and ready. Taskworker
g2 was new and ready on all four enabled hosts, complementing the earlier
g1 result. A bounded 20-minute Loki search at 19:05:23–26Z found no
matching Connect startup-not-ready, ingress-init, panic or fatal records;
that search cannot certify full log coverage or host RunWorker state.
Observed Connect convergence is partial and progressing, with no sampled
unready response. The service-scoped host drain lock can serialize block
replacement for up to the configured drain timeout, but the exact drain
owner on each host was not observed.

At 19:06:23–25Z, MCP beta and g1 also returned the new MCP and config
versions with HTTP 200 and ready status on all four enabled edges, with
matching pinned host identities. That read did not enumerate all MCP
blocks or overlapping generations. At 19:15:01–09Z, a later pinned
read returned new and ready MCP g2/g3/g4 on all four enabled edges, and
new and ready API g3 on all four; taken with earlier reads, every
configured API and MCP block has at least one new, ready sampled path
on each enabled edge at its respective observation time. These samples
still do not enumerate overlapping generations. Alt and Proxy are assigned to
Fireside/Crisp transparent hosts; Gossip has no status route. Supported
pinned-LB status reads therefore cannot prove their running images, and
the prior direct host reads did not succeed. Their alias and DynamoDB
selection is proven, but their runtime versions remain unverified.

At 19:10:19–23Z, another bounded pinned Connect read found seven of 20
block/host paths new and ready, up from three of 20; the other 13 were
old and ready, and all 20 returned HTTP 200 with matching host identity.
This remains a sampled rolling state, not complete convergence.
At 19:15:01–09Z, Connect advanced to ten of 20 new, ready paths, with
the other ten old and ready; all 20 returned HTTP 200. The new-path
counts by edge0/1/3/4 were 4/4/1/1.
At 19:20:15–19Z, Connect reached 12 new and eight old ready paths, and
at 19:25:39–44Z it reached 13 new and seven old ready paths. All 20
sampled paths returned HTTP 200 in both reads; edge0/1 had all five
blocks new by 19:20, while the remaining old paths were on edges 3/4.
The 19:29:44–47Z bounded repeat was unchanged at 13 new and seven old,
all 20 ready; the old paths were edge3 beta/g1/g3 and edge4
beta/g1/g2/g4. A service-scoped drain may still be in progress, but its
remote lock and old-generation ownership were not directly observed.
At 19:37:25–29Z the sampled Connect rollout reached 14 new and six old
ready paths; at 19:42:47–51Z it reached 16 new and four old ready
paths. Every sampled path returned HTTP 200 and matched its pinned host.
The four old paths were edge3 g1/g3 and edge4 g1/g2; old-generation
drain remains incomplete.

The user directed deployment to proceed without waiting for the long
Connect test queue. Buckets 0, 1, 2, 4 and 5 have since passed their
normal runs (bucket 2 has its intentional opt-in skip). Bucket 3's
`TestConnectMultiClientTcpPerformance` failure remains open. Bucket 6
was intentionally interrupted after 12 passes with no test failure; its
remaining cases and bucket 7 are pending. This deployment does not clear
the failed performance gate or establish sustained FP2 recovery.

A test-only correction in Server `e3d4fb42` snapshots gVisor TCP counter
values before computing per-run deltas in the failing multi-client TCP
performance diagnostic. The old code retained live counter pointers and
could print false zero deltas after the counters advanced. An exact-root
scalar snapshot control passed normally and under race in an isolated
worktree (receipts SHA256
`1aba12310f7d266b7630d02071e24ccd5d818d06d016bb7cd54880883cfe830e`
and `2ef39b3024248e4e5019f30654d443c39711a27513d399a4bbf47f43272852da`).
This repairs diagnostic visibility, not the TCP performance failure.
An isolated ten-case virtual-time ACK control then passed its pure-ACK,
future-ACK, payload-order, receipt-prefix and cleanup cases normally and
under race. Its candidate piggyback-ACK cases failed exactly four expected
IPv4/IPv6 window-progress assertions, in normal and race runs: when the
payload queue was full, a valid ACK carried with payload did not advance
the modeled return window, while a pure ACK did. The sealed receipt is
SHA256 `eba9b68d9f3b9247da21bd5afe764f34d1543cd34827360e35df512e9d767f70`.
This localizes a modeled ACK-half admission dependency; it does not yet
prove the cause of the historical 30-second TCP stall, because provider
return replay may rescue progress. No production ACK behavior changed.

The sole scheduled Main watcher continued at its 15-minute active-probe
cadence throughout rollout. Its thirty-ninth scoped record began at
18:40:09Z and was appended to the primary ledger. PostgreSQL used 64.173
of 96 logical cores in a separate 5.03-second sample at 18:51:14Z,
without query-owner or quota attribution. At 18:54:37Z, no shard had a
fresh URL-census owner, so current eligible, quota-complete and throughput
values were **unknown**, not zero. The picker observed 16 of 25 expected
API slots; five remote release-builder reads failed SSH. These timestamps
overlap the service rollout but do not establish its effect on FP2.

The fortieth scheduled record began at 18:55:09Z. Its independent
PostgreSQL sample at 19:06:22Z used 31.233 of 96 logical cores, still
above the 25% warning threshold without query-owner attribution. The URL
census was coherent again at 19:09:54Z: 108,779 eligible, zero
quota-complete, 108,757 overdue and 22 warming. The picker paired 20 of
25 API slots. This one post-deploy census restores numeric visibility but
does not establish sustained coverage, throughput or FP2 recovery.
The next scheduled coherent URL census at 19:25:15Z still found zero
quota-complete among 108,780 eligible providers, with 108,758 overdue
and 22 warming. A separate 5.02-second PostgreSQL sample at 19:21:28Z
used 35.088 of 96 logical cores (36.55%), without query-owner or
quota-normalized attribution.
The forty-second scoped monitor record was appended after its scheduled
19:36:35Z PostgreSQL sample used 36.017 of 96 logical cores (37.52%).
Its coherent 19:40:35Z URL census had 109,838 eligible, zero quota-complete,
108,991 overdue and 847 warming; oldest due was 13,807.1 seconds. The
large warming and due-state shift from the prior frame has no retained
per-provider/process join, and the audit's timestamp overlap does not
identify its cause. Picker coverage remained 20/25.
The forty-first scoped record containing that scheduled sample and census
was appended to the primary ledger. A separate scheduled reliability-index
check at 19:28:27Z still reported its desired covering index absent with
34 partitions and zero attached children. This access-path gap is
unresolved; it is not yet a causal attribution for the measured CPU load.

At the user's request, current-source `bringyourctl db audit` ran against
the verified Main primary through a direct maintenance tunnel. The dry
run completed at recorded/local migration head 740 with zero pending
migrations and **no missing additive schema objects**; it reported six
extra indexes only. The subsequent `bringyourctl db audit --fix` completed
successfully and printed `Nothing to apply` and `0 migration(s) need to be
applied`. It did not drop the six extras because `--force-drop-indexes`
was not requested. Both comparison runs cleaned up their temporary
databases and closed the tunnel. This does not install the separate
partitioned reliability covering index, which is owned by the explicit
model maintenance upgrade rather than numbered migrations or schema audit.

A bounded direct-primary read at 19:47:03Z confirmed that the desired
`client_reliability_valid_bnch_net_client` parent is absent and no
partition has a standalone covering child. The valid legacy parent still
has 34 attached children. The 34 reliability partitions have estimated
3.91 billion rows, 695.5 GiB of heap and 409.2 GiB of existing indexes;
the three future partitions dated September 30 through October 2 are
currently empty. This is a model-maintained physical index gap, not a
missing numbered migration. All four sampled window markers carried
observation and degraded-classification tokens. There were no active
index builds or lock waiters; one autovacuum worker held a roughly
54-minute snapshot without a target-relation lock. These are point-in-time
observations, not admission for a large concurrent build.

The fresh 19:50:33Z resource window measured PostgreSQL at 34.20 of 96
logical cores, PGDATA device busy 97.19%, device writes 57.8 MB/s, and
cluster WAL 6.80 MB/s. An exact privileged filesystem metadata read at
19:51:07Z found PGDATA and `pg_wal` on the same filesystem with 976.16 GB
available. The latest source backup remained the September 27 completed
generation under its Thursday/Sunday schedule, with the next due October
1; destination integrity and restore remain unverified. These reads did
not change production. The private phased covering-index operator awaits
an immediate per-phase resource admission. No covering-index DDL has run.

At 19:48:13–16Z, the Connect rollout had 17 new and three old ready
paths, all 20 pinned paths HTTP 200. The remaining old paths were edge3
g1/g3 and edge4 g1. Runtime image identity for transparent blocks and
full Connect convergence remain unproved.

The final pinned Connect path advanced at 20:06:50–53Z: all 20 enabled
host/block paths returned HTTP 200, the new startup version and ready.
This completes sampled version convergence, not an immutable running-image
digest or proof that overlapping old containers have exited. Repeated
Connect status polling stopped after that result.

The 19:55:47Z scheduled Main URL census had 109,157 eligible providers,
zero four-hour quota-complete, 109,125 overdue, 881,834 accepted successes
still needed and oldest due 119,010.2 seconds. At that denominator the
steady ten-success/four-hour target needs at least 75.80 accepted fleet
successes per second. The 20:00:51Z complete eight-process Taskworker
five-minute counter window measured 6,233.6 successful outcome ACKs,
8,855.9 attempted turns and a 16.85-second mean completed turn. Its ACK
rate was 20.78/second; ACKs are not an exact count of new durable,
unique-provider accepted history. A complete post-sampled-Connect window
at 20:15:19Z had 4,486.5 successful ACKs, 6,372.0 attempts and a
20.33-second mean turn, or 14.96 ACK successes/second. All eight process
identities were unchanged; each had a lower rate and longer mean turn,
while aggregate ACK yield stayed near 70.4%. This cross-window association
does not attribute the slowdown to the Connect rollout.

A direct 20:18:55Z fixed-cardinality Taskworker gauge read observed eight
current processes, 487 active full batches, 430 running providers, 12
queued and 45 finished-waiting. These full-batch gauges and the separate
completed-turn worker-time equivalents support a nearly occupied configured
512-lane pool, rather than a broad idle scheduler, but running includes
setup, URL/DNS and joined tunnel cleanup. The same point read summed
Connect process CPU at 77.02 core equivalents, RSS at 696.05 GB and
goroutines at 6.824 million across 20 processes; it does not assign that
load to URL probes or establish host spare capacity.

The eight-shard URL configuration caps each shard at 64 simultaneous turns.
The user asked to evaluate 256 per shard, or 2,048 total. Current source
accepts that geometry, and private exact-256 ownership, old-task retirement,
refill and credit tests passed normally and under race without Main contact.
At the 20:00 mean/yield, an ideal fully occupied 2,048 lanes would yield
about 85.6 successful ACKs/second; at the later 20:15 mean/yield, only
about 71/second, below the 75.8/second accepted-success target even before
durability, fairness or overhead. The internal prober transfer-credit floor
would rise from 2 TiB to 4.5 TiB. No capacity configuration was changed.
Fresh direct in-flight phase and host/credit/PG headroom remain admission
checks before even a measured 64-to-72-per-shard canary; a fourfold jump
is not supported by these observations.

A 20:08:28Z strict, read-only Redis-cluster GET retrieved the most recent
complete native score publication (source completed 19:58:51Z, published
20:05:26Z). Deduplicated public target membership was 91,658 quality,
94,119 speed and 109,114 online; these bucket memberships may overlap.
Eight-hour selected-policy outcome denominator bands were quality
0/40/57/1,445/90,116/0 and speed 0/41/60/1,508/92,510/0 for bands
zero/one/two/three-to-four/five-to-nine/ten-plus respectively. They are
neither four-hour successful-probe quotas nor a live recomputation.

The private phased covering-index operator was source-tested on current
Server head in a local PostgreSQL 18 fixture. Its initial wrapper paired
equal transaction and statement timeouts, which disables the intended
statement timer; a local causal control reproduced this. The corrected
private candidate sets transaction timeout to zero while retaining a
bounded statement timeout and outer context, and rejects wrong-kind index
names before touching a child. Its focused normal/race, source-bound
timeout, vet and build gates all passed. The prepared empty-future-partition
pilot remains unapplied: it would not improve current URL turns, and an
index build would confound the ongoing capacity/resource comparison.

The first post-convergence phase diagnostic at 20:21:50Z had 9,856.4
DNS route waves in a complete five-minute, eight-process window. About
3,549.6 waves (36.0%) timed out with unready route endpoints, accounting
for 53,085.8 of 105,274.3 measured DNS route seconds (50.4%). The earlier
53,930.7-second timeout sum also includes 844.8 seconds classified as a
changed or ambiguous route. Another bounded window at 20:23:20Z
classified timeout waves mainly as forming
(2,227.9) or provider-unresponsive (1,546.1), with 194.4 on active paths;
all 5,991.3 answer waves were active. Several DNS waves may belong to one
URL turn, and the windows are not an identity join. These signals place
substantial time before an observed active private route. "Forming" covers
control registration and evaluation, so these counters do not establish
a specific fix or an exact lost-success count.

A fresh four-host node-exporter read at 20:24:20Z found CPU execution
34.5% on edge0, 42.7% on edge1, 91.3% on edge3 and 94.6% on edge4;
edge3/4 one-minute loads were 102.6/223.9 against 72 logical CPUs each.
Memory available was 930.3/669.1/664.7/536.4 GB respectively. These
point measurements do not attribute the host load to URL probes, but
they withhold a safe uniform concurrency increase. The proposed first
64-to-72-per-shard canary and the 2,048-slot target are held while the
running-stage latency and host placement are diagnosed.

A same-process five-minute 20:25:39Z timing read measured a 17.97-second
mean inner full pass, including joined tunnel close, against a 20.28-second
mean URL turn. The roughly 2.31-second difference is an approximation from
independent Prometheus windows, not exact per-turn subtraction. Most
observed residence is inside the inner pass, rather than an outer
publication-only wait. A direct 20:28:04–29Z placement read found two
Taskworkers per enabled edge: mean URL turns of 15.19/16.96 seconds on
edge0/1 and 18.20/18.02 seconds on edge3/4. Edge3/4 CPU remained
92.3%/93.1% with larger Connect RSS and goroutine totals than edge0/1.
This host association does not prove that Connect load caused each
Taskworker turn or that relocating shards would improve the private path.

The fixed-label Connect owner read at 20:30:21Z found connected transport
clients of 22,035/18,021/45,836/56,232 on edge0/1/3/4, respectively.
Resident-device counts were 29,552/26,788/112,512/180,237. The same
hosts had 0.846/0.716/2.324/3.475 million Connect goroutines and
48.4/53.5/284.9/401.8 GB Connect RSS. Thus edge3/4 carry much more
current client/resident work, though the metric populations differ and
their source does not identify the ingress-placement cause. Existing
Taskworker claim admission allows one live URL shard per instance and
has no host affinity; stopping edge3/4 workers would halve the available
URL shard owners rather than rebalance them safely.

The scheduled 20:55Z monitor frame found a coherent 20:57:18Z cohort of
108,692 eligible providers, zero complete ten-success/four-hour quotas,
108,654 overdue and 833,416 selected-policy successes still needed.
Hourly process coverage remained incomplete, so the change in this stock
is not a measured accepted-success rate. Its 21:09:50Z host sample again
found edge3/4 CPU execution at 93.60%/90.66% of 72 logical CPUs and
one-minute load normalized to 2.207/2.382. PostgreSQL's separate
21:07:17Z sample used 34.598 of 96 cores without query attribution.
These observations continue to hold a uniform URL-worker ramp. At the
latest 20.33-second mean URL turn and 70.4% outcome-ACK yield, ideal
2,048-lane occupancy projects about 70.93 ACK successes/second versus
the current eligible-cohort maintenance floor of 75.48 accepted unique
successes/second; ACKs do not prove that durable per-provider rate.
The Main URL configuration is still eight shards at 64 turns each (512
configured slots), and no 1,800-slot URL setting was found. A private
same-turn registration/admission timing diagnostic and bounded live
placement read are in preparation; neither has changed Main or proved
the cause of the long turns.

A separate read-only Route53 exact-name and alias-leaf check at
21:16:50–21:22:28Z mapped all ten live, health-checked `main-lb` A
records to enabled Main hosts. Every mapped IPv4 health check was wholly
healthy. Edge0/1 had 120/120 configured and healthy weight, while
edge3/4 had 200/200: 62.5% of weighted IPv4 ingress favored the two
already CPU-saturated hosts. This is a live DNS placement fact, not a
measured share of new connections or an attribution of existing resident
load. Existing Connect residents remain sticky across DNS changes. The
corresponding AAAA read was incomplete as a host census: edge3's mapped
200 weight was unhealthy, edge4's mapped 200 healthy, and four healthy
records totaling 220 weight could not be bound to current inventory
addresses. A live host-local LB read was also incomplete (three SSH
transport failures and one reader inventory-binding failure). No DNS,
router, LB or resident placement was changed.


## 2026-09-29 measured-run quota correction and current diagnostic evidence

The current user contract is ten accepted measured runs, success plus failure,
per eligible provider in the strict rolling four-hour window. Earlier dated
success-only completion counts, throughput deficits and 2,048-lane acceptance
calculations above are superseded as interpretations of the user target; their
raw success/error measurements remain historical evidence. Bucket admission
and ranking policy do not change.

The private correction reads accepted history through a new bounded latest-ten
measured-run index (migration 741), retains `success_count` and its existing
index as success-only diagnostics, and retains `completed_run_count` as the
separate all-turn fairness ledger. Ingest, claims, attempt pacing and the fleet
census use the measured quota; setup receipts cannot reopen a full measured
quota. `runs_needed` is the new response/metric field; `successes_needed` remains
a deprecated compatibility alias for the same deficit under coverage capability 2.
`quota_complete`, `secure_complete` and `complete` keep their names. New monitors
require capability 2 and both explicit success/error counter families; mixed or
old success-only telemetry is unobservable rather than new-contract proof.

Install and attest the additive concurrent index before new quota readers:
`provider_egress_health_history_url_run(client_id, measured_at DESC)` with
predicate `url_probe AND url_probe_policy_version=1 AND total_count=1 AND
(ok_count=0 OR ok_count=1)`. Retain the existing success index and receipt schema.
There is no history rewrite or new provider-admission gate. Rollout requires
reviewed migration 741, converged API quota writers/readers, Taskworker capability 2
census/metrics, then the matching monitor. Until convergence, an old success-only
writer can re-pace a provider under its old quota; no mixed interval proves the
new target. No quota correction or migration is yet applied to Main by this patch.

The preceding timing-only Taskworker release is Server `d85261d9`, image
`2026.9.29-planetoid+1059178590`, pushed manifest
`sha256:54762c6995cb5a129eb61e3203047533f88e107f19068922c623af09463fb725`.
Warpctl reported all 20 status paths new at 21:46:08Z; that status convergence is
not immutable container identity proof. Eight exact processes in the fixed
21:46:27–21:51:27Z diagnostic window exposed timing capability 1 and 43 series,
41.70 completed turns/s, mean 11.126s worker occupancy, and 9.585s check-plus-buffer
stage. Those completed turns do not prove accepted measured quota, and host CPU
has not been causally attributed to this release.

A separate bounded read of durable selected-policy `url_probe,total_count=1`
history over 21:46:27.283940–21:51:27.283940Z found 12,766 unique run IDs from 12,766
providers: 10,304 successes plus 2,462 failures, **42.5533 measured runs/s**.
Its receipt SHA256 is
`0a4d726ad507778fc1ff7d7bdfc481ad8713d27956b26ba9ed1ec1f58225c3e1`;
stdout SHA256 is
`0a3b715cd8a8b7bae062738838757677f09156f089b83747c1ea991cca153ffe`.
This is a fixed short interval, not rolling per-provider ten-run completion.
Its measurement clock differs from completed-turn publication, so dividing these
two rates does not establish an acceptance yield.

Reevaluate 2,048 lanes against total accepted measurements. For the dated 19:55
cohort of 109,157 providers, the numerical floor was 75.80/s, now measured runs
rather than successes. At 11.126s mean occupancy, 2,048 fully busy lanes have an
ideal raw-turn ceiling of about184.1/s; it is not a measured acceptance forecast.
The observed 42.5533 durable runs/s cannot be extrapolated linearly across a
concurrency ramp. Re-measure the current cohort and total measured acceptance,
prove lane occupancy and stage bottlenecks, and retain per-host CPU/memory,
latency and TLS/availability gates. The documented hot-host placement and sticky
residents remain independent limits. No uniform 2,048-lane ramp is validated here.

## 2026-09-29 Main measured-run rollout checkpoint

The measured-run correction is committed on Main as `9d890f6d`; the independent
TLS deadline test correction is `2fbe91bf`. The merged focused gate passed all
49 selected roots in normal and race modes, with no race finding, and vet passed.
Migration 741 ran once against the inventory-bound Main primary from 22:18:45.797
to 22:19:03.905Z. Its terminal receipt is
`5b95a237527313416829cc8ffeb792406c62cbdf15c6e16fef9ca0e367d21f8b`.
A separate read-only postflight at 22:19:21Z found successful head 741 and the
new 128.6 MB measured-run index valid, ready and live; the older success-only
index remained valid. No index builder or blocker was present.

The local API image `2026.9.29-planetoid-1059198330` was published with
manifest `sha256:16a26d580a80b308a294c9d0245b69fe8067f02127ffe1a1eafbcdd992994ce5`;
the Taskworker image `2026.9.29-planetoid-1059198720` with manifest
`sha256:dae38cfd9c2a5aca5babb998678c3de5cdaa0c31be4f87b560f58dac6ffac80f`.
Warpctl deploy exited zero for API by 22:23:05.601Z and Taskworker by
22:25:39.512Z, each ending at 20/20 new-version service-status paths. This is
version-path convergence, not immutable running-container proof. This patch
changed API quota admission/ingest and Taskworker fleet telemetry; no Connect,
proxy or config-updater source was changed in this rollout.

The bounded current-process read at 22:32:01–03Z found all eight enabled
Taskworker shard owners uniquely current on edge0/1/3/4, all capability 2 with
configured shard count 8. One coherent shard-zero census observed at 22:31:52Z
reported `eligible=108936`, `quota_complete=0`, `secure_complete=0`,
`due=68495`, `overdue=108921`, `warming=15`, `runs_needed=658358`,
`security_pending=14`, `security_unknown_targets=5`, `uninitialized=2`.
The receipt is
`159a2c707da051c3efc33bba7d2988c0f8b949d5d413733dffe758c5c15b4ae5`.
This is the new total-measured-run contract, not the earlier success-only
projection. It establishes a large quota deficit, not an accepted rate or
individual provider recovery guarantee.

The old capability-1 watcher stopped at 22:32:56Z with its parent and all 13
captured descendants reaped. One new capability-2, enabled-host watcher started
at 22:33:03Z as the durable user service
`fp2-main-monitor-cap2-20260929.service`, PID 781613, with ten standing tails,
zero restarts and an explicit 15-minute first-probe floor. The last old active
start was 22:25:13Z, so the new first active probe cannot start before 22:48:03Z.
The brief log-collection gap and process-local Sustain reset are explicit;
first new active probe, hourly range completeness and recovery remain pending.

At this census denominator the numerical floor is 75.65 unique accepted measured
runs/s. The prior fixed five-minute durable rate of 42.5533/s predates this
quota rollout and cannot be used as its outcome. Fresh post-rollout durable rate,
per-provider rolling recovery, CPU attribution, 2,048-lane admission, native
quality/speed buckets, and the separate reliability covering index remain open.

The first fixed post-rollout durable-history window, 22:26:00–22:31:00Z,
contained 15,747 unique accepted selected-policy measured runs from 15,747
providers: 12,332 successes and 3,415 failures, or **52.49 runs/s**. The bounded
read receipt is `7d5965c699a058beccdb2c7036607601064c28fe80ca3c0b0d8043b0b1350ef9`.
This window is wholly after the Taskworker version-path deploy, but does not
prove an individual provider's rolling quota or immutable running images. Its
rate is about 69.4% of the current 75.65/s numerical floor; the change from
the earlier 42.5533/s window is not causally attributed to the quota release.
The eight configured URL shard pools have 64 concurrent workers each (512
configured slots); edge3 and edge4 were near 93–95% CPU in recent watcher
samples. Same-window occupancy, CPU attribution, and a safe ramp decision
remain open.

An independent fixed-window Taskworker capability-2 timing read for the same
22:26–22:31Z interval found 52.2253 timed completed turns/s across all eight
current shard processes. Mean turn occupancy was 9.28947 seconds, of which
9.12374 seconds was check-and-buffer; publication, close-join, and readiness
means were 0.08493, 0.04811, and 0.02895 seconds respectively. Rate times
mean occupancy implies about 485 worker slots occupied on average of 512
configured (roughly 94.8%). The Prometheus timing cohort and durable-history
measurement cohort have different clocks and cannot be divided into an
accepted-result yield. Read receipt
`d083e389b7e5ea6a9db33981742fecbb349651a7434290f26371bfefed4729ca`;
at that mean turn time the 75.65/s target would need at least about 703 busy
slots even with perfect measured acceptance. This is a lower bound, not a safe
configured concurrency or linear scaling prediction. Same-window host CPU and
admission witnesses remain pending.

The proposed uniform 2,048-slot setting is held. Source accepts 256 workers
per shard, but the one shared shard setting would raise concurrency on hot and
cool hosts alike; the prober credit floor would rise from 2 TiB at 64/shard to
4.5 TiB at 256/shard. A source-bound fixed-window per-host Taskworker/node CPU
read failed closed twice on unexpected metric label shapes (first a missing
node `instance`, then replacement process series outside the allowed early
witness), so no process CPU fractions were admitted. These failures are retained
without a third broad retry. The existing enabled-host placement has no
host-specific URL concurrency knob, and a larger fleet-wide setting or host
move awaits resource attribution and a staged rollout plan. The existing
512-slot setting remains in place.

An indexed, capped, read-only aggregate over the same 22:26–22:31Z accepted
cohort retained exactly 15,747 results. Positive final-hop DNS clocks appeared
on 12,233 successes (mean 2.365 seconds, p50 1.310, p95 8.238) and 3,399
failures (mean 22.315 seconds, p50 15.634, p95 45.153). Of the 3,415 failures,
1,509 were classified `dial_dns`; their positive DNS clocks totaled 68,020
seconds, about 45.1 seconds per such failure. Observed wire-body span totaled
2,056.6 seconds across 12,332 successes and 186.7 seconds across 1,333
failures with body bytes. These clocks make DNS a stronger bottleneck candidate
than body transfer, but they retain only the final attempted hop and are from
accepted results, not every worker turn; they do not prove full fetch elapsed
or CPU use. Query execution was 0.799 seconds under a two-second bound;
receipt `10e66f05dffb78d25b7c8a681744ab3128029001f2116b92906872f2b9dfe820`.
The owning DNS/DoH path and a correctness-preserving repair are under review.
Two independently gated historical DNS-counter reads failed closed: the first
exceeded its reviewed metric-row cap, and a narrower exact-process read fit
3,606 rows/1.08 MB but encountered an unexpected source label. No route-state
or path-state aggregate was admitted from either read; the accepted-result
DNS clocks above remain the durable evidence. The next step is an offline
exporter-label audit or a separately scoped shape discriminator, not a blind
retry or an inferred zero.

A separate deterministic provider-path correctness bug was reproduced: the
monitor may coalesce an admitted client's `Added` then `Removed` events into
one delivered `Removed` diff while the probe callback is delayed. Inspecting
only the current active set loses proof that the tunnel had a route, so the
turn may time out as a measured provider failure instead of following the
existing lost-tunnel no-result path. The small source correction consumes
delivered admission/removal proof before checking current replacements; it
does not infer admission from an empty or overflow-reset snapshot. The
independent Sol gate observed the expected sole failing baseline root, then
all eight selected candidate roots passed normal and race modes with vet
clean. This is a classification correction, not a claimed throughput gain;
post-deploy verification remains pending.

The correction was committed as `5e9a4144` and built/pushed as Main Taskworker
`2026.9.29-planetoid-1059244520`, manifest
`sha256:385dd0924823a741883bc554f6680ca8755f01c1e1a5c9ceee0348376068fe27`.
Both Linux release-binary vulnerability scans passed. The exact-version
Warpctl deploy exited zero by 23:38:57.979Z with 20/20 target service-status
paths after updating both `g1` and `g2` tags. This is control-plane version
convergence; current eight-process generation, immutable running containers,
and the correction's Main outcome effect still need independent verification.
The existing capability-2 watcher remains active through the rollout, so its
during-rollout owner gaps are scoped rather than silently counted as recovery.

One bounded current-process read at 23:41:49–52Z found exactly eight newest
fresh enabled-host Taskworker processes, all capability 2/configured eight,
with eight unique fresh shard owners. Every process start followed its `g1`
or `g2` tag update; eight predecessors were excluded by the reader's now-only
rules. A coherent shard-zero census observed at 23:41:11Z reported
`eligible=109535`, `runs_needed=598286`, `quota_complete=0`, and
`secure_complete=0`. The receipt is
`012ea8c4dc29d3dec5642d5fe44bd5208e6b7c3ce10e77b33b34098401821a4b`.
This proves fresh runtime generation, capability and shard ownership, not the
running containers' immutable digest or the correction's Main effect.

The first fixed post-convergence history window, 23:39–23:44Z, then found
9,393 unique accepted measured runs: 6,992 successes and 2,401 failures, or
**31.31/s**. The indexed read took 0.232 seconds under a two-second bound;
receipt `4be3a2a8e4f4acef099f1e19825dfd1274c0464539fdf784a892f543263ad01c`.
At the contemporaneous 109,535-provider denominator the numerical floor is
76.07/s, so this short interval supplied about 41.2% of that floor. The rate
is lower than earlier nonoverlapping 52.49 and 45.8867/s windows, but source
cohorts, process ages and measurement clocks differ; a causal effect of the
route-loss correction is **not** established. A second settled window and
same-window completed-turn/local-failure evidence are required before deciding
whether this reflects reclassification, transient rollout pressure or another
capacity change. Correctly unmeasured lost-tunnel turns must not be counted
as accepted failures merely to raise throughput.

A second nonoverlapping fixed history window, 23:44–23:49Z, found 8,482
unique accepted measured runs: 6,055 successes and 2,427 failures, or
**28.2733/s**. The indexed read took 0.266 seconds; receipt
`d400eb07f3b35833a97d0ee3e844f3f66fec61038493b67d8df8798f829e3511`.
These two settled post-correction windows remain below the roughly 76.07/s
maintenance floor at 109,535 eligible providers. They do not by themselves
establish whether the correction, local failures, or changing work mix caused
the decline. A same-window worker-counter read was rejected by its strict
process-series guard (`nonreference-current-or-range-series`, 2,342 rows),
so no attempted/accepted/local-failure or stage totals are admitted from it.

A corrected reader then accepted only complete witness pairs for strictly older
processes, while rejecting foreign counters and retaining the same eight pinned
new processes. Its one bounded 23:39–23:44Z process-counter read passed all
49-cell guards (receipt
`4907e54c6b3431dea71d1ecd799bc28a9fc5912f9df58a594ad17f3a6064e23a`).
The window contained about 9,198 attempted turns, 9,197 accepted turns and
one local failure; 9,201 timed completed turns give 30.67 turns/s and mean
14.96 seconds per turn. Mean check-and-buffer was 12.83 seconds, publication
1.07 seconds and close-join 0.96 seconds. These are counter increases from
the pinned new processes, not the durable 9,393 accepted history rows; their
clocks and denominators differ, so they cannot be divided or subtracted into
an acceptance yield. The older process witnesses retained by the metrics
backend do not prove that predecessors were absent. Relative to the earlier
nonoverlapping 9.29-second mean, the longer turn time is a diagnostic lead,
not a causal attribution to the path-loss change.

A separate direct control exposed an existing classification error: a fully
read negative URL response could be made `NotMeasured` if the tunnel-loss
signal arrived during body close. The narrow correction preserves complete
content/performance failures only after observed peer EOF; partial bodies,
unknown clocks, setup failures, completed successes and TLS failures retain
their respective classifications. The isolated baseline failed exactly its
two completed-negative cases; the candidate passed four focused roots under
normal and race runs plus vet. Frequency in Main and throughput impact remain
unknown pending the tracked change's release and new measurement.

The completed-negative correction and ten-case control were committed as
`8666931b` and pushed to Main. The tracked four-root focused gate passed;
the isolated candidate had also passed normal, race and vet gates. Taskworker
was the only affected service. Local Warpctl built and pushed image
`2026.9.29-planetoid-1059274730`, manifest
`sha256:d305ab076a66d46d704fb8de9cdf90e0291a81b2ccdbf80ac458ca3dda2aed12`,
with both Linux binary vulnerability gates reporting no called vulnerable
symbols. Build log SHA is
`3b11701f3e1490b5ff878c5bc6c7271cdf6937a8334fb3e62e23f809bcf6bb29`.
Warpctl deploy exited zero and service status converged 20/20 enabled paths to
that version by approximately 00:23Z on September 30 (deploy log SHA
`8390c596fa6524931cdde4a469d8630901d8fd3468fc388546717d18fcf60bc2`).
Current-process runtime identity, immutable container digest and post-release
accepted-run effect require separate proof; scheduled frames overlapping this
rollout cannot establish a gap-free history.

One bounded now-only read at 00:32:10–13Z then found eight current enabled
Taskworker processes, capability 2/configured eight, with eight unique fresh
shard owners. Process starts ranged 00:21:03.720–00:22:31.870Z, each after
its block's `g1`/`g2` tag update. The coherent shard-zero census observed at
00:31:35Z reported `eligible=109873`, `runs_needed=576537` and
`quota_complete=0` (receipt
`00298b0802437a41b67e9b2de161ef8d9489c9aea859d2972c261ebb9e53e5d8`).
This establishes current generation and shard ownership, not immutable
container digests or uninterrupted coverage. The seventh scheduled census at
00:23:49Z missed shard owner 3 after the deploy command had ended; that
post-terminal source-convergence gap remains recorded separately. A settled
post-release accepted-run rate is evaluated separately below.

The first settled post-release fixed history window, 00:25–00:30Z, found
11,522 unique accepted measured URL runs: 9,003 successes and 2,519 failures,
or **38.4067/s**. The indexed read completed in 0.253 seconds (receipt
`0626dc16822fe0efe37949c3f091e44869c34d7a683e6556e3114852244529eb`).
This excludes setup-only turns and remains well below the current maintenance
floor. The eighth census at 00:39:01Z had a changed 111,220-provider eligible
cohort, `runs_needed=594629` and zero quota-complete providers, implying a
77.24/s numerical maintenance floor. The earlier 28.2733/s window belongs
to a different image/process and provider cohort; the rise does not establish
a causal throughput benefit from the completed-negative correction.

One bounded 74-cell Taskworker DNS read for the same 00:25–00:30Z selected
eight-process window passed source, service-label, reset and freshness guards
(receipt `eab40b699fe40294f28d280ea8ea74a9ef847fa37928401b6176b239397616b6`).
Among completed timeout waves, the `unready_endpoints/unattributed` cell
accumulated **50,940.81 wave-seconds**; `changed_or_ambiguous/unattributed`
accumulated 5,984.49, and the current-route-admitted before/after cells
accumulated 838.50/1,655.87 seconds. The unready class means both known
endpoint snapshots had zero active endpoints, after lost/closed precedence;
it does not prove continuous absence of a route between snapshots or an
admission wait of that duration. The read covers only answer/timeout DNS waves,
which can multiply per turn and include unaccepted work. Counter cells are
independent and cannot be divided by the 11,522 durable accepted URL rows or
used as a CPU attribution. The large unready timeout burden is a lead for
source-level route-lifecycle diagnosis, not yet a proven throughput cause.

The route-readiness source review found that placing a wait inside the
existing DNS wave relocates the same never-ready allowance and has no direct
occupied-slot saving. Healthy late admission retains its third-wave recovery
opportunity; earlier real-DoH controls preserved 5/16-second success and
roughly 45-second never-ready timing while suppressing pre-admission dial
allocations. A gain from reduced packet or socket contention remains
unmeasured, so the route-wait change is held for a finite real-gVisor control.

A separate provider-tunnel pump ownership control proved that `Tun.Read`
returns caller-owned pooled packet storage and a rejected `SendPacket` leaves
it with the caller. The old pump ignored rejection. A narrow correction returns
only rejected packets to `MessagePoolReturn`; accepted packets still transfer
ownership. The private baseline failed exactly two rejected-owner cases, while
the candidate passed eight focused server/Connect roots under normal and race
runs, plus vet. This restores pool reuse; it does not shorten the 15-second
send allowance or establish a Main throughput or CPU gain.

The packet-pool correction was committed and pushed as `fd87d5fa`. The local
Taskworker build passed both Linux binary vulnerability gates and pushed
`2026.9.29-planetoid-1059303750`, manifest
`sha256:aa560dd47abaa6e24558ae2fcc4e785e17c3d72cd0ad29649f9e983cf1b50ced`
(build log SHA `e9330bad9cd6653bc1e731d705cc9495c6b72ef79c934696b314bda1a5601007`).
Warpctl updated `g1` at 01:09:06Z and `g2` at 01:09:09Z on September 30;
deploy exited zero with service status 20/20 enabled Taskworker paths on the
new version by 01:11:18Z (deploy log SHA
`62f5e099cf994fd6d94cc1627c0ab234867e1a9638993175b8f03c310267cd34`).
Current-process identity and a settled post-release rate remain separate
verification tasks. The tenth scheduled monitor frame overlaps the rollout,
so temporary owner gaps there cannot be silently counted as recovery.

A bounded now-only read at 01:16:42–44Z found eight current enabled Taskworker
processes, capability 2/configured eight, with eight unique fresh shard owners.
Every process started after its block tag update; starts ranged
01:09:14.210–01:10:50.890Z. The coherent shard-zero census observed at
01:14:55Z reported `eligible=112089`, `quota_complete=0` and
`runs_needed=609152` (receipt
`acdef78b6b4e96c36eebcae0bd1a931cf8d8d513da527bf8635a1e60b165c2e9`).
Three older metadata groups were excluded. This proves current generation and
shard ownership, not immutable container digest or a gap-free rollout history.
The eligible cohort has changed again; no post-packet-pool accepted-run rate or
CPU saving has been established.

Two settled post-packet-pool fixed history windows remain below target. The
01:13–01:18Z window had 5,847 unique accepted measured runs (3,336 successes,
2,511 failures), **19.49/s**; indexed receipt
`2985a35da6fdffd258e36e9c3b4823dbd1a8fd0911df63401357e5eb845a222d`.
The nonoverlapping 01:18–01:23Z window had 8,762 (6,850 successes, 1,912
failures), **29.2067/s**; indexed receipt
`1e271d8151d278e541f2e9de7d33d1f161e5a44071dfc25a8a23646721af0b27`.
Both exclude setup-only turns. The second window's partial rebound and changed
providers/destinations/process ages prevent attributing the swing to the
packet-return correction. The eleventh scheduled census at 01:24:36Z found
`eligible=111865`, `quota_complete=0` and `runs_needed=608086`; this cohort's
numerical maintenance floor is 77.68 accepted measured runs/s. Same-window
worker stage and operational counters are required to distinguish slower turns
from fewer attempts or accepted results.

The exact eight-process capability-2 counter read for the low 01:13–01:18Z
window subsequently passed all 49-cell guards (receipt
`b412024554965c35cef2e5ca80b8f1d7f53326b56f624466b6a0e0edcb6d40dd`).
Its own completed-turn counter was about 5,669.5 turns, **18.8983/s**, with
mean total 25.95 seconds: check-and-buffer 22.84, publication 1.63 and
close-join 1.35 seconds. Attempted and accepted turn counter increases were
both about 5,654.9, with zero local-failure increases. The separate ACK
counters and durable 5,847 history rows have different clocks and cannot be
combined into an acceptance yield. Relative to the earlier different-cohort
14.96-second mean, this establishes longer measured residence, not why it
grew or whether the packet-pool edit caused it. Completed-turn rate times
mean duration is about 490.5 occupied slot-seconds/s, near the 512 configured
slots. At unchanged duration, an idealized zero-loss maintenance calculation
already needs roughly 2,021 slots for the 112,089-provider floor, so 2,048
offers almost no margin and is not an admitted ramp on hot edge hosts.

The first successor capability-2 active frame began at 22:48:13Z, after the
explicit 15-minute floor. Its coherent 22:50:58Z census found `eligible=109392`,
`quota_complete=0`, `secure_complete=0`, `due=63290`, `overdue=109383`, and
`runs_needed=635274`; the current numerical floor is about 75.97 accepted
measured runs/s. Enabled-host picker pairing was 20/20 at 22:53:07Z, with one
initial read error still surfaced. The new process-hourly counter range is
expectedly incomplete until a full hour of coverage; neither zero quota
completions nor a short-window aggregate can be relabeled as recovery.

A second nonoverlapping indexed history window, 22:40–22:45Z, found 13,766
unique accepted measured runs: 10,398 successes and 3,368 failures, or
**45.8867/s**. Its read completed in 0.233 seconds under a two-second bound;
receipt `5b2785543731a4104ef095ac35b650e18675fe31bfbf885dda36ed98fb9f71c7`.
The earlier 52.49/s was not a persistent flat rate; neither five-minute
window reaches the later cohort's roughly 75.97/s numerical floor. The
different outcome mixes and changing eligible cohort are retained without
causal attribution. Aggregate throughput still cannot establish per-provider
rolling completion.

A bounded read of the complete published native-score census at 23:15:12Z
found 92,152 public native-quality providers, 94,643 public native-speed
providers, and 109,378 online providers. The source snapshot completed at
23:00:22.948Z, about 14 minutes 50 seconds before the read; publication was
23:07:45.183Z. A provider can appear in more than one bucket, so these counts
are not additive. The six denominator bands describe accepted selected-policy
outcomes in the trailing eight hours, **not** the new ten-run/four-hour quota:
quality `0/64/43/82/91963/0`, speed `0/67/46/85/94445/0`, and online
`1017/133/110/219/107808/91` for zero/one/two/three-to-four/five-to-nine/
ten-plus respectively. The first bounded GET reached the key but its older
Python parser rejected a valid Go fractional timestamp; a separately reviewed
read with a source-proven parser and local controls succeeded. Both receipts
are retained. The successful private result SHA256 is
`f61e17249c5ef7111ffd3eeab8d65daf400c7d544df8ed5af4608bf85d445496`.

A bounded historical final-clock read aligned to the earlier 21:46:27–21:51:27Z
accepted-result window found 102 of 12,766 outcomes with final TTFB over two
seconds, all measured failures with wire body observed. This is only 0.8% of
accepted outcomes and does not justify deploying the private TTFB shortcut as
the throughput repair. The accepted-only sample cannot rule out time spent on
turns that produced no accepted measurement; current worker timing is being
measured separately.

The continuing 15-minute Main monitor remains active without a restart. Its
01:54:59Z URL frame could not identify fresh shard-0 or shard-6 owners and
therefore did **not** establish a current global quota count for that frame.
The later 04:12:17Z frame again had a coherent shard-zero census and reported
`eligible=111912`, `quota_complete=0`, `runs_needed=700329`, and
`overdue=111837`. That later observation resolves the particular missing-owner
snapshot, but does not prove uninterrupted ownership between frames. Its
current-process hourly measured-run ranges or expected-process coverage remain
incomplete, so it cannot support a whole-fleet hourly throughput projection.
The deficit increased relative to the 01:39:48Z census, with a changing cohort;
the latest numerical maintenance floor is about 77.72 accepted measured
success-or-failure URL results per second. Setup/no-result turns do not count.
The independent 04:08:50Z database sample measured 39.181 PostgreSQL CPU
cores of 96 logical cores (40.81%) over 5.02 seconds, without query-owner or
CPU-quota attribution. No concurrency ramp or reliability-index build is
admitted by these observations.

Two separately prepared, bounded, read-only discriminators subsequently
completed for the same fixed 01:13–01:18Z eight-process Taskworker window.
The CPU reader covered all 28 selected Taskworker and Connect process slots,
with 284 source rows, complete reference generations and no raw metrics
retained (receipt stdout SHA256
`460419ab044b12e2ae6ef544301b7f77c75c31fd3459ee32c1f9134598d3070d`).
The eight Taskworkers used about **8.75 CPU cores** in aggregate; the 20
Connect processes used about **70.35 cores**. Edge-3 and edge-4 Connect used
22.85 and 23.87 cores, versus 3.70 and 1.37 for their Taskworkers. One
edge-3/g2 Taskworker used 3.09 cores against a reported `GOMAXPROCS=4`;
the other seven were lower. These are process CPU rates, not URL-path CPU or
a whole-host census, and cannot assign the host load or slower turns to
Connect. All 28 selected process generations were complete in this historical
window; their later continuity and immutable image digests remain unproved.

The paired DNS reader selected the exact eight Taskworker process identities
and all 74 cells, with 3,600 source rows and complete guarded reduction
(receipt stdout SHA256
`0d7bebcb8b4dca5f7a6105cb681a27c0eaa3c58cbe017a7fe461b5e326288b09`).
About 5,685.76 completed timeout waves had `unready_endpoints` at both route
snapshots, accumulating 85,040.86 seconds of unattributed wave time across
parallel turns. The `current_route_admitted` answer-wave class accumulated
16,288.21 seconds before the observed admission and 10,603.49 seconds after
it; timeout waves accumulated 1,328.90 and 2,106.02 seconds respectively.
The independent path-end timeout labels were about 486.02 active, 3,758.48
forming and 1,925.68 provider-unresponsive waves. Endpoint snapshots do not
prove continuous absence, path and route cells cannot be joined wave by wave,
and a turn can have multiple DNS waves. These DNS clocks cannot be divided by
the accepted-result count or treated as an exact fraction of the 25.95-second
completed-turn mean. They do identify route readiness as a large measured
resource/timing discriminator for local source experiments; no readiness-gate
change or capacity ramp has been released.

## 2026-09-30 Connect CPU and route-readiness checkpoint

The continuing 15-minute Main monitor's coherent 06:13:54Z census reported
`eligible=111992`, `quota_complete=0`, `runs_needed=687651`, and
`overdue=111984`. The quota remains ten accepted measured URL outcomes,
success plus failure, per eligible provider in a strict rolling four hours;
setup-only turns do not count. Current-process hourly coverage is still
incomplete. The 06:03 frame overlapped the Connect g2 metrics rollout, so its
later callbacks cannot be used as a clean before/after performance comparison.

Historical 01:13–01:18Z counters for two exact Connect generations support a
retained-forward workload investigation. Edge0/g2 averaged 4,739.95 forward
workers, 4,594.80 outbound endpoints, 2.8814 CPU cores and 14.618 MB/s
allocated. Edge4/g2 averaged 15,098.47 workers, 14,285.84 endpoints, 5.8049
cores and 116.563 MB/s allocated. Their sent ping rates were 17,053.36/s and
55,659.95/s. These counters cover all exchange traffic; neither their CPU nor
their worker counts identify FP2-only work or exact disconnected owners.
The read receipt SHA256 is
`4ea0f7729275e3f9ea03f38d65c07c6fa4cafba9b9c106c657cf0afaa941876a`.

Two source defects were fixed in Server commit `fb1d1eb7` and pushed to main.
Disconnected `ResidentForward` owners now wait for a queued payload before
resident lookup or redial, while retaining queued recovery, FIFO order and
pooled-buffer ownership. The existing 15-minute payload-idle policy now waits
for the remaining deadline instead of checking only every 15 minutes. A local
network/Redis fixture previously kept a forward alive for 1,799 seconds after
its last payload; the candidate closes it at 900 seconds. Five paired local
32-connection, 1,800-second virtual-horizon runs reduced median CPU by 47.86%,
allocated bytes by 42.23% and sent ping operations by 49.98%. These are local
mechanism measurements, not Main savings. Sol independently passed 27 normal
and 27 race test roots plus vet on the integrated source, attestation SHA256
`1d1a3ec0c3697891888949612f4847eac4da963f74a2092e3489cce8ed1f3235`.

The lookup instrumentation image
`bringyour/main-connect:2026.9.29-planetoid-1059458480` was published with
manifest SHA256 `1df461698f99812a7331cd9051dd311c36ccc06562d3a5e553377c15d8d327b5`
and selected for the shared g2 block at 06:11Z. Its LB block status reached
20/20 repeated successful **g2 samples**; this is neither a 20-container count
nor proof of the other blocks. At that checkpoint, exact image proof on each
enabled g2 host and a source-bound lookup/CPU baseline were pending. The running prior
edge1/g2 image was bound to its previous build record. The new image's own Go
build info identifies Server commit `2affcd32` as unmodified; its local
Connect dependency has only a qualified time-based source link. The rollback
version is `2026.9.29-planetoid-1059025240`. The first behavior-fix image,
`2026.9.29-planetoid-1059497230`, was superseded before selection because a
concurrent documentation edit made its binary report `vcs.modified=true`.
The clean rebuild `bringyour/main-connect:2026.9.29-planetoid-1059505090`
passed build and vulnerability gates and was published with manifest SHA256
`62f148be0d68242e90086ce7b421d3815d9444032b241be439c401607db2984d`;
its build log SHA256 is
`bfbb19110d6240502fa70f6bf1094df08736ec21fbb00094c26a575035226299`.
The clean image was selected for the shared g2 block at 07:02:37Z. Warpctl returned 20/20
repeated successful g2 LB samples on the new version. A strictly authenticated
read at 07:08:56–59Z proved that edge1/g2's actual running image matched
manifest SHA256
`62f148be0d68242e90086ce7b421d3815d9444032b241be439c401607db2984d`
and its binary reported Server `5971e48b`, `vcs.modified=false`. The other
three enabled hosts could not be read over their configured management TCP
paths; three one-shot connection timeouts are preserved without retries.
The direct rollback target for g2 is the prior metrics image
`2026.9.29-planetoid-1059458480`. Block samples and one host proof do not
establish four-host runtime image convergence.

Real local `providertunnel.Open` controls with delayed provider route
admission preserved measured success or failure and existing deadlines. At
35-second admission, an experimental route-readiness gate reduced generated
SYNs from 105 to 5, provider TCP dials from 25 to 5, and SYNs accepted from a
terminal local source endpoint from 8 to 0. Its paired total times were 35.590
and 35.291 seconds, a single jittered comparison with no latency-gain claim.
The 5- and 16-second admission pairs likewise found work reduction but no
latency gain. The never-ready turn produced a measured failure at its existing
deadline; cancellation produced no measured result or credit. The local gate
is **not deployed**. Its private virtual TLS/H2 fixture issue was resolved,
and Sol independently passed 20 normal and 20 race roots plus vet on the
source-pinned candidate (attestation SHA256
`db8ba6dfcc13d6039f788df9becdc1b437df566398a2c5e73855a33eed4a6155`).
No Main capacity or quota improvement has been demonstrated. No 2,048-slot
ramp is admitted by this evidence.

### 2026-09-30 07:50Z Main behavior canary measurement

The fixed 06:20–06:25Z lookup baseline admitted three current g2 process
generations, but withheld a four-host aggregate: edge4 had 18 samples against
the unchanged 19-sample threshold, and older edge3/4 generations appeared
within that window. Across the three qualified current processes,
`reconnect_empty` lookup attempts were 15.0922/s versus 6,705.1581/s for
`reconnect_pending`, only 0.2246% empty. The demand-driven empty-lookup fix
therefore targets a small observed branch; this metric does not identify the
CPU share or outcome of pending lookups. The bounded lookup result SHA256 is
`8865e228e376ba68a53f450b94312c38f530976567c57841f5974ac564a141c2`.

The separately guarded pre-rollout 06:20–06:25Z CPU/ping read qualified edge0,
edge1 and edge3, with CPU rates 3.08766, 3.12688 and 8.86012 cores and sent
ping rates 8,666.61, 8,765.54 and 35,468.59/s. Edge4 was again withheld.
The early post-rollout 07:10–07:15Z read qualified all four **new** g2 metric
generations. Their CPU total was 15.83894 cores; the same three new processes
used 2.64094, 2.52036 and 5.69677 cores and sent 5,862.21, 5,746.38 and
16,812.01 pings/s. Two older edge3/4 metric generations were still active
in the same response, together using 9.85490 cores and sending 82,234.22
pings/s. The complete six-generation queried population used 25.69384 cores.
These are whole-process and all-exchange measurements across changing process
ages and traffic, not a causal CPU attribution to the idle fix.

The steady 07:25–07:30Z read qualified the exact four early-post generation
hashes. Their total CPU rose to 21.89085 cores; edge0/1/3/4 used
2.80627/3.17111/8.14468/7.76880 cores and sent
10,448.21/10,356.64/53,604.64/48,004.43 pings/s. No older metric groups
were returned in that response, which does **not** prove their containers
exited. Edge3's steady ping rate exceeded its qualified pre-rollout rate;
there is no demonstrated sustained Main CPU or capacity saving. The steady
result SHA256 is
`ae9373cbb4d45c6211410e5c477d78c15df7d8aea56cd201cc447da48f436fa4`.

The 07:45:21Z coherent census still reported `eligible=112133`,
`quota_complete=0`, and `runs_needed=669823`. Earlier 07:30Z visibility
sampled no shard-2 owners, but later frames had no missing-owner detail;
neither observation proves a durable lease change. Current-process hourly
coverage remains incomplete. The standing monitor continues at 15-minute
active floors. Capacity remains at 512 slots while persistent pending
reconnect/ping work and route-readiness residence are investigated. A
concurrent `provide_key` reindex and PostgreSQL connection-capacity log lines
are monitored separately; neither is assigned as the probe bottleneck from
these samples.

A separate source-pinned owner-grid read of 07:40–07:45Z found one sampled
zero-owner point in each of shards 0, 2, 4, 5 and 7; all eight shard metadata
streams qualified, and those five owner counts returned to one. This is
sampled metric visibility, not durable lease duty or a reason to count fewer
accepted URL results. Result SHA256 is
`4ca5c91a1784acf5d47d9da15e3ee1c742357145783d3ab56fc1a3f329d51a89`.

Current source tracing identifies a possible persistent-work mechanism: after
a probe's carrier, client and local-control owners join and its SQL client is
retired, the adapter does not compare-and-remove that generation's Redis
resident. Inbound forward pings and polling can keep the old resident active,
and pending reconnect lookups can refresh its TTL. A generation-safe
capture-and-CAS retirement repair is being tested locally; it is **not** yet
committed or deployed, and Main CPU causation has not been established. Older
g2 metric generations also continued consuming CPU and pings during the
07:10–07:15Z rollout window; later absence from metrics does not prove the
containers exited. Drain-phase evidence is still needed.

The route-readiness gate was subsequently committed and pushed as Server
`ac985578`; shared-source Sol gates passed 19 focused normal roots, 19 race
roots and vet, with its separate private timer control already attested.
It remains **unselected on Main**. A clean Taskworker image containing that
gate and the local OOB control path was built and published as
`bringyour/main-taskworker:2026.9.30-planetoid-1059558190`, manifest SHA256
`74547d0bbc7e31794eab8eb9dfe5b8b304641566714deac658a08a300f1a6f10`,
binary Server revision `ac985578`, `vcs.modified=false`. The build log SHA256
is `f9d4a421e525d567aa24890a0c0e57d06d36b89daf4f43b5984fef7ffaa056d5`.
Main Taskworkers have not been changed to that image while the resident
cleanup and scheduler-state change are investigated.

The 08:15:48Z coherent census still had `eligible=112465`,
`quota_complete=0`, and `runs_needed=663450`. Its warming count rose from 6
to 548 and its oldest current due deadline age fell sharply. Source review
found that `oldest_due` reads the maximum lateness of **current** eligible
deadlines: a claim or pacing update can advance it before a measured result.
Ordinary reliability rollup seeds new rows without resetting existing cycle
starts. The changed eligible cohort and recent-cycle entrants are compatible
with the observed shift, but exact provider-row cause is unjoined; neither a
quota recovery nor a wholesale scheduler reset follows from this census.
The scheduled initial picker remained nonempty in all 20 enabled-block
samples at its next callback. Persistent empty US IPv6 quality location and
best-available lists are a separate API-model finding: FP2 URL due claims
exact fixed provider client IDs and does not use those ordinary discovery
lists.

The later 09:16:47Z coherent census remained at `quota_complete=0` with
`eligible=111670` and `runs_needed=634720`. Shard-2 and shard-6 owner sources
were zero in that sample, so current-process hourly coverage was incomplete;
this does not establish a durable lease gap. The 09:21:01Z host callback
sampled edge3 and edge4 CPU at 96.83% and 93.43%. The 09:11:39Z PostgreSQL
sample was 28.954/96 logical cores (30.16%); neither host nor database CPU
sample attributes cost to URL probes. The standing Main monitor remains on
its 15-minute probe cadence.

A local actual-Open comparison reproduced retained probe residency after
teardown. In both modes, 96 locally acknowledged URL results completed (48
successes, 48 completed failures). After 64 retired turns and 90 seconds,
retired residents were 64 without cleanup and zero with generation-safe
cleanup. Final ten-second Connect/control CPU fell from 0.673222 to 0.071803
CPU-seconds, with sent pings falling from 2,599 to 38. Across the full
90-second idle horizon, CPU instead rose from 4.95497 to 5.28094 seconds;
the experiment does not demonstrate a whole-workload CPU, Main throughput,
or quota gain. Residual pending forwards and queued messages remain. The
comparison summary SHA256 is
`24929bd78eee932bdb8c8a1eb9eb09c21b45cb58e65f9a2ca7bf2ccca9a4546d`.

The generation-safe retirement change captures the exact existing Redis
resident before teardown and deletes it by original-value compare-and-swap
only after successful SQL retirement and existing tunnel joins. A dedicated
context-aware Redis pool bounds optional capture and commit operations
separately to one second; process-owned pool maintenance can outlive them.
A native stalled-read control exposed a 15-second close path in the first
candidate, which the bounded pool corrected. The final sealed candidate
passed 35 independent normal roots, 35 race roots, four vet packages, and
a four-turn actual-Open smoke with two measured successes and two completed
failures. The exact tracked-tree integration also passed 35 focused normal
roots. Source/smoke attestation SHA256
`a7134ae70f04514ebea838a2576f9d9ac7a22a95ad447fa09573a48264792b77`,
independent gate attestation SHA256
`31f5a0c16a28b37ec77eabe942b68f6c37fb1d7cfbda8770f78e9d6569f86d7f`,
and tracked-tree attestation SHA256
`567e8a788429fa3a880d16d2ff6f8ad083d18a5b217302d0cb4ebfcd37c64a26`
record the evidence. This is local resource reclamation; a guarded Main
canary and unchanged-workload post-release measurement are still required.

The change was committed and pushed as Connect `fee85be2` and Server
`3e852af2`. The clean Main Taskworker image
`bringyour/main-taskworker:2026.9.30-planetoid-1059603990` was built and
published as manifest SHA256
`4020f5901a51b3e2158482ad14f3349bbabbd7b3c70dae7d9ede2395d238784a`.
Both Linux release-binary vulnerability scans found no reachable
vulnerabilities; the amd64 binary reports Server revision `3e852af2` and
`vcs.modified=false`. Warpctl selected the image for g1 at 09:33:20Z and
g2 at 09:48:43Z. Each block's sampled status paths converged 20/20 to the
target version, and both registry block tags resolve to the published
manifest. The g2 deploy log SHA256 is
`8b6621ae526f78420812ddf0111b2c52b2fa651a35e968621d532353dd7902e8`.

A single bounded post-rollout metrics read at 09:57:32–35Z selected exactly
eight current capability-2/configured-eight Taskworkers with eight unique
shard owners. All four g1 process starts followed its tag update; all four
g2 starts followed its tag update. The read's atomically published census
reported `eligible=112305`, `quota_complete=0`, and
`runs_needed=621657`. Its receipt SHA256 is
`141b520ca9b8e5b2729dabffeec69bd2bf1c2d74652ab895723cf003b6d8fde0`.
Process metrics do not prove the immutable digest of each running container.
The 09:33 scheduled monitor frame still had quota zero and sampled edge3
host CPU at 97.42%; it overlapped g2 cutover and did not provide a numeric
edge4 sample. Post-rollout accepted-run rate and host impact still need a
fixed complete window; no capacity ramp is justified by this deployment.

The first fully post-Taskworker scheduled census at 10:02:24Z still had
`quota_complete=0`, `eligible=112064`, and `runs_needed=622594`.
The 10:22:07Z host callback sampled edge3 CPU at 97.03%; that frame had no
numeric edge4 sample. These observations do not show a fleet quota or host
CPU recovery from resident cleanup. A separately scoped indexed read of
durable accepted results in a fixed post-rollout window is prepared but has
not contacted Main; scheduled SSH work prevented the guarded attempt.

A separate Connect forward-allocation issue was reproduced locally. The
forward receive ring is closed immediately and never enqueues a payload,
yet the old constructor allocated a 98,304-byte backing for it and a
64 KiB reader before the echoed header. A narrow candidate removes the
unused ring and limits the forward reader to 4 KiB without changing the
16 KiB frame ceiling, send capacity, deadlines or wire protocol. Five
paired 512-operation local runs measured successful forward allocations
at 365,274→205,388 bytes/operation and constructor/framing CPU at
245.77→181.56 microseconds/operation. Refused handshakes also fell from
148,421→88,493 bytes/operation. The source and profile review are sealed
under manifest SHA256
`0e0ee789897728c486ac9d9f4fe8fd443828667d46f1a1d25cca10ab4cd7de4d`.
Sol independently passed 25 focused normal roots, 25 race roots and vet,
then 25 focused roots from the exact tracked tree; attestations SHA256
`0844a9377880be9365c32caf334058f3134b612df0dfe52d64f031c4a7b643db`
and `d2d7f5f0c534c6cf966d5c8388b44bbefbdff58c38ce8240cefb71fecabb1c62`.
The change has no measured Main RSS, CPU or quota effect yet. A fixed
post-Taskworker Connect baseline is needed before a service canary.

The Connect allocation fix was committed and pushed as Server `2f361252`.
The clean Main Connect image
`bringyour/main-connect:2026.9.30-planetoid-1059642290` was published at
manifest SHA256
`ddeeeac0f6584410aa700e90a973d73d43fcc95fba521e923aa2590e99100a3e`,
with `vcs.modified=false` and both Linux binary vulnerability scans clean.
Only Connect g2 was selected at 10:59:22Z; its sampled status paths reached
20/20 on the target version. The g2 deploy log SHA256 is
`08f5c56731c66dd6d6befcc0653dbc3fd811f640180c3b5305fa2d782426f2ac`.
Other Connect blocks remain on their prior tags.

One bounded, complete 20-slot Connect metrics read of the 10:20–10:25Z
pre-canary window measured 81.1897 process CPU cores fleet-wide, including
20.2586 for g2, and 1.58023 GB/s fleet process allocations. Its receipt
SHA256 is `305bb0d4fc9f3a9a13af48fe0bb377582899c4846c66370e9ffcfc21acb630ab`.
The matched-definition 11:05–11:10Z post-canary read qualified four fresh
g2 process identities and the 16 unchanged other-block references. New g2
processes used 13.8060 CPU cores and allocated 568.311 MB/s versus 20.2586
cores and 648.744 MB/s in the pre window. Two older g2 generations on
edge3/4 also appeared in the post window: all observed g2 generations used
22.0584 cores, while all observed Connect generations used 81.870 cores,
versus 81.1897 before. The post-read receipt SHA256 is
`477ecc8c5f1ab06ccc4898d8fe311f3c9e69e5762543f9edce9cae28e21ad44a`.
Traffic, generation age, and overlap differ; the read does not prove a
causal Main CPU, URL-rate or quota improvement. The other blocks are held
until the old drains and a complete steady comparison are understood.

A separately reviewed, bounded read-only indexed query of the fixed
09:52–09:57Z post-Taskworker/pre-Connect-storage history window completed
once at 11:34:44–46Z. It counted 9,851 unique accepted measured URL runs:
7,342 successes and 2,509 completed failures, or **32.8367/s**. Setup and
no-result turns are excluded. The primary-side query used the leading
`measured_at` index with parallel execution disabled and took 0.2225 seconds
under a two-second statement cap. Receipt SHA256
`61c47e34259582ceb4cd2f2d98baec90375a78a97f597c6577fcd6ec46a56183`.
This historical rate is well below the roughly 76–78 accepted runs/s
maintenance floor at the observed 110–112k eligible cohort. The window
does not establish arrival-time completeness, a rolling quota recovery, or
causal benefit from the Taskworker release; different fixed windows have
different traffic and cohort conditions. No extra active URL probe was run.

The complete 20-slot Connect comparison for the later 11:25–11:30Z window
measured 82.4446 process CPU cores fleet-wide versus 81.1897 in the fixed
10:20–10:25Z pre-canary window (+1.55%). G2 used 20.8616 versus 20.2586
cores (+2.98%) and allocated 849.800 versus 648.744 MB/s (+30.99%); its
forward-worker and outbound-endpoint populations also rose 8.90% and 6.67%.
The read returned the 20 expected current process metric groups and no older
g2 metric group. Metric absence does not prove OS/container exit. Different
traffic, process ages and populations prevent a causal CPU or allocation
conclusion; in particular, local constructor savings have not established a
Main fleet CPU benefit. Other Connect blocks remain on their previous image.
The bounded read receipt SHA256 is
`4ca4278fa3e6ba09692cbf3ffcfc2206e6fc1b09ef7607a3696e779f25accb23`.

Initial-ping dependency observation was committed and pushed as Connect
`8fc48acc` and Server `b9cd123e`. It records 49 fixed aggregate scalar
series per Taskworker process, with no provider, client, destination or URL
labels and no timeout, admission or retry changes. The exact tracked source
passed 30 focused normal and 30 race test roots plus vet; integration
attestation SHA256 is
`762157c479981edb67adf5035080c823e15ef3b2a6f2fe038a03cde9feb8e501`.
The clean multi-architecture Taskworker image
`bringyour/main-taskworker:2026.9.30-planetoid-1059705550` was published
as manifest SHA256
`febae1e23c128fbbac1a09bd62e2835537ffdd1cf5a062dab6591f973b5a2e45`;
its embedded Server revision is `b9cd123e`, `vcs.modified=false`, and both
Linux binary vulnerability scans found no reachable vulnerabilities. Only
Taskworker g1 was selected at 12:21:23Z. Its sampled status paths converged
20/20 on the new image, and the g1 registry tag resolves to the same
manifest. Deploy log SHA256 is
`988b5dbbd62424f11a239619aa48f19f94eb296dc1b5e4566c66fe3bb996fd9c`.
The counters need a settled fixed Main window before drawing a phase or
throughput conclusion. The backend-degradation retry hypothesis remains
unsupported: a real local Open control dispatched its first prewarmed
contract in 0.304 ms and completed a measured URL result in 1.205 seconds.

One separately bounded Connect composition read compared edge4 g2 and its
unchanged g1 context in the same fixed pre-canary and steady windows. All
four window/block groups and 52 derived comparisons qualified. G2 process
RSS summed 155.98→102.53 GB and heap-in-use 124.09→78.89 GB, while its
allocation rate rose 297.87→486.81 MB/s. Reported free message-pool backing
fell 13.16→0.79 GB; heap allocation excluding that reported pool actually
rose by about 4.71 GB. G2 received data bytes rose 47.4%, handshake frames
52%, and its process age differed substantially (about 202 minutes before
versus 30 minutes after). This read explains why constructor savings cannot
be inferred from process allocation rate, but it does not attribute memory,
GC, CPU or quota changes to the patch. Other Connect blocks remain held.
The single-read receipt SHA256 is
`2f3d7c54db11fecd6c3138b41a168f0e152eb39ba672713b99bc832824cd8e8f`.

The g1 initial-ping observer was read once for the fixed 12:25–12:30Z
post-deployment window, after a separate current-process proof found four
g1 starts at 12:21:31–35Z. All 49 fixed cells per current g1 process,
source freshness and reset checks qualified; four older generations were
excluded from the window. The PromQL counter increases estimate 4,342.60
terminal evaluations and 23,793.82 elapsed seconds. About 3,487.96 were
acknowledged (mean 1.454 seconds), 416.51 expired at the 30-second deadline
(mean 30.001 seconds), and 438.13 were canceled or ended (mean 14.213
seconds). Every estimated expiry ended with a terminal snapshot classified
`carrier_present_write_attempted`; the two carrier-present contract-wait or
contract-failure-without-write cells were zero. The expiries consumed about
52.5% of observed terminal evaluation seconds. The write-attempt witness is
set before the route writer runs, so it does not prove any carrier channel
accepted bytes. This identifies route disposition, delivery and
acknowledgment as the next diagnostic boundaries. These counters do not
prove remote receipt, attribute each second to its terminal dependency, or
equate an initial-ping evaluation with an accepted URL result. The
fixed-window receipt SHA256 is
`a95fd87dce0257f6ab0ca595cc307e6e630cc2dccf6a9e0fab20998c160b5b0e`.

An independent, indexed read of durable accepted measured URL outcomes for
the same fixed 12:25–12:30Z window counted 7,765 unique runs: 6,078
successes and 1,687 completed failures, or **25.8833/s** fleet-wide. Setup
and no-result turns were excluded. The primary query took about 0.234
seconds with read-only, two-second and no-parallel guards; it ran once and
finished before the next scheduled monitor probe. Receipt SHA256 is
`91c283faecdc2062d834fa5f9957a7dfd49bfdeb82aa4418dd245a17769bc71c`.
The earlier 09:52–09:57Z fixed window measured 32.8367/s, but different
traffic, cohorts, ingestion timing and one-block observer selection prevent
a causal comparison. This fleet-wide accepted-run denominator cannot be
divided by the four-process g1 initial-ping population. Both fixed rates
remain well below the roughly 76–78/s quota maintenance floor; the latest
scheduled census still has zero quota-complete providers.

The 13:20Z scheduled census reported zero visible owners for shard 7. One
bounded 13:18–13:23Z historical owner-grid read later found a unique owner
for shards 0–6 at 21/21 sampled points and for shard 7 at 20/21. Shard 7's
sole zero was at 13:20:00 on the same process: a completed pass retired,
then a new heartbeat value from 13:20:03.207 was visible at 13:20:15.
This supports a brief between-pass heartbeat gap, not sustained worker
loss; it does not measure durable lease ownership or exact idle duration.
The read has a recorded execution exception: its second concurrency audit
rejected an overlapping scheduled SSH read, but the private shell wrapper
continued anyway. The queries used separate gateways and finished before
the next scheduled probe, but the intended safe gate did not pass. The
result was not rerun; a replacement private wrapper now tests that a
rejected second audit issues zero queries. Result receipt SHA256 is
`2067d6c2e6120ca7b30159fd4dca3d45bcd68a75e77b3756ed558d3ab7dde4db`.

A local mixed-batch control ruled out one proposed URL idle cause. Production
URL rows declare a 900-second maximum, and the existing task picker isolates
tasks whose declared maximum exceeds two minutes. With a real production-
shaped URL row, both `EvalTasks(2)` and `EvalTasks(1)` finalized the URL and
admitted its successor while an ordinary sibling remained blocked. Only a
deliberately counterfactual 120-second URL row co-batched and waited for the
sibling. Three normal roots, three race roots and vet passed with production
source unchanged. This is a local exclusion of the mixed-batch hypothesis,
not a Main throughput measurement. Evidence manifest SHA256 is
`bd88048222767521aa58a45c52063a7486f5d98e77098b1837f41a71060853e6`.

Local real-path controls showed that the earlier `write_attempted` initial-
ping label can occur before the initial ping has a successful route write.
An unread carrier, accepted outbound loss, lost reverse ACK, and a held
successful ACK callback all produced deadline expiries under distinct
conditions; 25-second recovery controls still admitted. The production
route/ACK observer therefore adds 48 fixed aggregate series per Taskworker
process (97 total), separating successful local route-writer acceptance of
the exact initial ping from its ACK callback entry. These are terminal
snapshots, not proof of remote receipt or elapsed phase durations. No
deadline, admission, retry, failure-authority or provider-contact policy
changed. The private candidate passed 16 normal roots, 16 race roots and
vet, then independent Sol gates; its manifest SHA256 is
`fabf081d710f8b7af18beaf549fc008bc5b2c5e09e137831a442aa0b1d202410`.

The exact patch was committed and pushed as Connect `ab6f0bcd` and Server
`2a0d15f3`, with tracked 16 normal/16 race roots and vet green. A clean
multi-architecture Main Taskworker image
`bringyour/main-taskworker:2026.9.30-planetoid-1059780540` was published
as manifest SHA256
`f3ae5c4a71083c9df8a58a541fc02f66f7112a195825c12de6ceb0fe54b13e47`.
The embedded Server revision is `2a0d15f3`, `vcs.modified=false`, and both
Linux binary vulnerability scans found no reachable vulnerabilities. Only
Taskworker g1 was selected at 14:28:00Z; sampled status paths converged
20/20, and the g1 registry tag resolves to the published manifest. Deploy
log SHA256 is
`802730525a7909dfd90f4e6caa0a1e408b0ab81ddb81c549e40e3450113cf888`.
The release itself did not establish route, ACK, CPU, accepted-run or quota
improvement; a fixed post-start 97-cell Main read follows below.

The fixed 14:35–14:40Z g1 path read completed once at 15:33:44–48Z after
two passing overlap audits. All four new g1 process references and their 97
fixed scalar series qualified, with fresh prior/end witnesses and no observed
counter resets. PromQL counter increases estimate 6,842.49 path terminal
evaluations. The only expiry cell was `route_write=accepted` and
`ack_callback=pending`: about 578.00 evaluations, mean 30.001 seconds and
17,340.83 evaluation-seconds. No expiry lacked a successful local route
write of the exact initial ping, and none had entered a successful or error
ACK callback at its terminal snapshot. About 5,622.90 evaluations were
acknowledged with local route acceptance and callback success (mean 0.915
seconds). The other nonzero cells were canceled/ended or about one error.
Receipt SHA256 is
`7bb6fce1abe2845dc693bf768ded0dc6853c63efe7bad21bbe2753cdc3d4b7b4`.
The old 49-cell and new 48-cell families are independent estimates of the
same evaluations, not additive populations. Local route acceptance can be
queue acceptance; it does not prove physical delivery or provider receipt.
This cohort rules out failure to accept the initial ping into a local route
as the primary source of its 30-second expiries. Carrier delivery,
Connect/resident forwarding, provider validation, Transfer ACK return and
local callback processing remain separate possible boundaries. These
initial-ping cells cannot be divided by fleet-wide durable URL outcomes or
attributed to controller/model/API cost without a joined witness.

The URL probe now has an explicit fixed-one-hop, URL-only mode: after
registration it admits the provider channel through the existing owner and
hard-cap gate, then the real in-tunnel DNS and HTTPS traffic supplies the
first data/Transfer ACK. It omits the one-time admission `IpPing`, continuous
provider pings, and the optional active stall ping for this mode only. Normal
SDK users retain their prior behavior; contract, auth, encryption, terminal
and no-result handling remain in force. A real local Open control across two
provider networks produced the same six outcomes in each mode (two URL
successes, two completed HTTP failures, two TLS-pin failures), with 12
provider pings before and zero in URL-only mode. Mean local turn time was
107.2 versus 98.0 ms; that small control is not a Main rate estimate.

Connect `05822164` and Server `e306d36e` are committed and pushed. A
concurrent upstream Connect merge brought outage window-expansion gating,
a degraded race cap and dial backoff into the same image; the merged source
passed 47 focused normal roots, 47 race roots and four-package vet. The clean
multi-architecture Taskworker image
`bringyour/main-taskworker:2026.9.30-planetoid-1059870910` has manifest
SHA256 `83d5c06921044d8993f8e755b309cd5da33fdbc5946514c37130541f2862e5cd`,
embedded Server revision `e306d36e` with `vcs.modified=false`, and zero
reachable vulnerabilities in both Linux binary scans. Only g1 was selected
at 17:00:05Z on September 30; sampled blocks converged 20/20 by
17:01:10Z, and `g1-latest` resolves to that manifest. g2 and the 512-slot
configuration were unchanged. The prior g1 version
`2026.9.30-planetoid+1059780540` is the rollback point. Deploy log SHA256 is
`473ff77c5741eaed6b1d1b6d35c2d213ee780f9d09007086320fad8f4f61c6f6`.
The corrected current-process proof at 17:24Z qualified eight fresh slots,
capability 2, configuration 8 and unique current shard ownership. The four
g1 process starts were 17:00:12–17:00:18Z, all after selection; g2 retained
its earlier starts. That binds the post interval to new g1 processes but is
not immutable image-to-PID proof. The first preparatory `--check` invocation
of a copied reader incorrectly made one read-only metrics POST without the
required two-audit gate; it launched no active URL probe or mutation. Its
result remains unqualified and excluded. A corrected entrypoint passed
zero-contact controls and an independent review before the later guarded
proof. The matched pre-count reader's first run then opened a bounded SSH
tunnel but made no SQL query because its copied binary lacked execute mode;
its corrected preflight rejects that condition before contact. These two
exceptions are preserved, not retroactive proof of safe execution.

Guarded, fixed fleet-wide accepted-history reads found 9,047 unique measured
URL outcomes in 16:50–16:55Z (6,469 success, 2,578 failure; 30.1567/s) and
10,528 in 17:05–17:10Z (8,533 success, 1,995 failure; 35.0933/s). Thus
accepted outcome rate was descriptively 16.37% higher after g1 selection.
Both queries used the same indexed measured-at window and read-only two-second
bound; source and receipts are retained privately. These are all-fleet
different-time cohorts, not a g1 treatment/control join. A simultaneous
eligibility census changed from 108,797 to 101,520, and the upstream Connect
controls changed with this image. The 16.37% comparison therefore does not
isolate ping removal, nor does it prove the per-provider rolling quota is
recovered; the quota-complete count remains zero. Exact g1/g2 turn, residence,
DNS and no-result measurements remain pending before widening the canary.

A separate proposed 15-second total DNS cap is not part of this image. The
current resolver can use several DNS waves inside a 60-second HTTP deadline.
A valid answer at 20 seconds would be lost by a hard 15-second total cap but
preserved by 30 seconds. In a capped September 29 local-success log sample,
at least 170 of 340 checks had first-to-last DNS trace time over 15 seconds;
that timing can include redirects, has no exact provider/process or durable
accepted-history join, and is not a private DoH-wave distribution. No exact
accepted-success 15–30-second band is available from retained aggregates.
An authenticated local real-Open paired experiment kept the same provider and
URL. With a valid DNS answer at 20 seconds, a 15-second cap produced measured
`dial_dns` failure at 15.016 seconds while a 30-second cap loaded the URL at
20.047 seconds. With valid recovery at 35 seconds, the current allowance
loaded the URL at 35.037 seconds while a 30-second cap failed at 30.008
seconds. Four locally acknowledged measured outcomes, 42 virtual timing
cells, 14 normal/race roots and vet passed; that paired experiment's timeout
patch remained private, distinct from the subsequent production change below.
These controls prove both shorter hard caps can create false negatives under
healthy late recovery, without measuring how often Main experiences it.

On September 30 at 18:05:33Z, at the user's request to simplify the
all-fleet rate measurement, the same Taskworker image was selected for `g2`.
The deploy command exited successfully and sampled `g2` converged 20/20;
subsequent sampled version reads showed both `g1` and `g2` at
`2026.9.30-planetoid+1059870910` on all 20 sampled status paths each.
`g2-latest` resolves to the published multiarch manifest
`sha256:83d5c06921044d8993f8e755b309cd5da33fdbc5946514c37130541f2862e5cd`.
The previous `g2` version was `2026.9.30-planetoid+1059603990`.
The 18:05 selection is a control-plane and sampled convergence fact; a
fresh process-start fence and settled, exact-window accepted-outcome read are
required before attributing a subsequent Main rate to the all-block rollout.
The corrected guarded current-process read at 18:21:54–56Z found exactly
eight current shard owners, each with capability 2 and configuration 8.
All four new `g2` process starts were 18:05:39–18:05:46Z: after the 18:05:36
tag retag and before the fixed 18:07–18:12Z outcome window. The four `g1`
processes had remained active since 17:00:12–17:00:18Z. This is fresh
process-start and shard-ownership proof, not an immutable image-to-PID join.
The separately gated indexed read of that settled window counted 12,945
unique accepted measured URL outcomes: 11,168 successes and 1,777 failures,
or 43.15/s. This is descriptively 22.96% above the prior 17:05–17:10Z
g1-only fleet window (35.0933/s), and 43.09% above the 16:50–16:55Z
pre-canary window (30.1567/s). These are different-time all-fleet cohorts;
neither comparison isolates the initial-ping removal from traffic mix,
upstream Connect changes or process-drain timing. The count is an accepted
outcome rate, not evidence that the rolling per-provider quota has recovered.

The operator subsequently chose 15-second URL network-phase limits, accepting
the late-positive loss demonstrated by the paired DNS controls above. Server
commit `50637792` applies separate 15-second budgets to URL DNS resolution
(including route wait and retries), target TCP connect and TLS handshake,
plus a 15-second idle limit per target response socket read. A shorter caller
deadline still wins, and the existing overall 60-second cold request owner
remains. These changes are scoped to sampled URL probes; the shared Connect
transport and non-URL tunnel callers retain their previous limits. A timed-out
read closes its raw socket promptly, avoiding TLS close-notify delay.
Independent local validation passed 33 normal and 33 race cases and package
vet; two Linux binary vulnerability scans found no reachable vulnerabilities.
The clean multiarch Taskworker image
`bringyour/main-taskworker:2026.9.30-planetoid-1059946840` has manifest
`sha256:b50287b735fc93e77ecf029c6dd63ddc3de22c199ebd3d9e87177b19e9648b57`.
It was selected for `g1` at 19:01:14Z and for `g2` at 19:02:38Z on September
30. Both deploy commands exited successfully; subsequent sampled version
reads reported 20/20 new-version status paths for each group. Fresh
process-start and accepted-outcome/failure-stage reads after the rollout were
required; sampled convergence alone does not establish the running image or
the throughput effect.
The guarded 19:19Z current-process read subsequently found exactly eight
fresh shard owners, capability 2 and configuration 8. All `g1` starts were
19:01:24–19:01:32Z and all `g2` starts were 19:02:44–19:02:55Z, after their
respective retags and before the fixed 19:07–19:12Z window. A separately
guarded indexed history read counted 16,554 unique accepted measured URL
outcomes in that window: 11,917 successes and 4,637 failures, or 55.18/s.
The earlier full-block, pre-timeout 18:07–18:12Z window had 12,945 outcomes
(11,168 successes, 1,777 failures), or 43.15/s. The newer total rate is
descriptively 27.88% higher; its success count is 6.71% higher, while the
success share fell from 86.27% to 71.99%. These are different-time fleet
cohorts and do not isolate the timeout effect or prove rolling quota recovery.
At the latest 108,588 eligible-provider census, ten accepted outcomes per
provider per four hours requires about 75.41/s in steady state. A second
settled post-change rate window remains pending.
A subsequent one-statement, read-only indexed failure-stage count used the
same database snapshot for three fixed windows. The additional pre-timeout
18:27–18:32Z window held 10,250 accepted outcomes (8,516 successes, 1,734
failures), or 34.17/s. DNS-class failures were 171/12,945 in the earlier
18:07–18:12Z pre-timeout window, 402/10,250 in the later pre-timeout window,
and 2,898/16,554 in the post-timeout 19:07–19:12Z window. These finite DNS
buckets include `dial_dns` and `request_dns_timeout`; missing, null and
unrecognized stages were zero in all three windows. Of the 2,860 additional
failures in the post versus earlier pre window, 2,727 were DNS-class, while
the success count increased by 749. This is a descriptive DNS-stage shift
consistent with the 15-second cap, not a measured late-recovery fraction or
an isolated causal effect. A later post-change rate window was read separately.
The later fixed 19:20–19:25Z post-change window held 14,056 accepted outcomes
(10,736 successes, 3,320 failures), or 46.8533/s, in a separately guarded
read. This was 15.09% below the first post-change window's 55.18/s, so the
initial higher rate did not persist in this later five-minute cohort. It
remained above the 43.15/s earlier pre-timeout cohort, but the windows differ
in time and provider mix. Neither post-change window meets the approximately
75.41/s steady rate implied by the latest 108,588 eligible-provider census.
Current exact-shard stage and CPU/PG-acquisition measurements were needed to
locate the remaining capacity limit before another code or slot change.
A guarded paired exact-eight-shard read subsequently compared the fixed
19:07–19:12Z and 19:20–19:25Z windows. All core, progress, whole-Taskworker
CPU and default-PG-pool groups qualified independently in both windows.
Accepted-turn counter rates moved from 55.095 to 46.538/s. Completed timed
turn means rose from 8.658 to 9.314 seconds: `check_and_buffer` 7.535 to
7.811, publication 0.699 to 0.949 and close/join 0.332 to 0.435 seconds.
Shared-lane active-batch scrape means fell from 476.64 to 444.31 of 512
configured slots; running fell from 433.72 to 391.63 while finished-waiting
rose from 38.17 to 47.53. The running decline appeared on all eight shards,
not one edge. Whole-Taskworker CPU fell from 15.03 to 13.63 cores. Mean
default-PG acquire residence rose from 0.953 to 2.214 milliseconds, but this
does not measure statement time. These are fixed-window counter estimates
and unweighted scrape means, not a continuous occupancy integral or an
exact durable-history join. They point to longer turns and less-filled slots
as the immediate observed rate difference; claim/refill, terminal waiting,
and Connect-side work still need owner-specific discrimination.

The scheduler admission-boundary correction and bounded observability were
committed as `5ffa265626e1af398dc2ca590821b2a675fa00a0`. After a
synchronous due response, a claim that has crossed the pass deadline is now
completed as unstarted with its identity and publication receipt joined;
it does not enter the tunnel, create a measured URL outcome, or gain quota
credit. The original deadline and 310-second reserve remain in force. The
new collector exposes 39 fixed, identity-free series for scheduler phases,
stop reasons, due-call results and claim disposition. Independent normal and
race checks each passed 21 tests and vet was clean. The clean multi-platform
Taskworker image `2026.9.30-planetoid-1060010880` has manifest digest
`sha256:fa7cbd200650b384aca8228060927873a934d2bd95b9e9604ecf7bc994de9f4b`
and embeds the commit with `vcs.modified=false`. Both `g1` and `g2` deploy
commands exited zero and sampled 20/20 on the new version; both block tags
resolve to that digest. This is deployment evidence, not yet a full-eight
process-identity proof or a measured throughput improvement at the rollout
boundary.
The first fixed post-rollout 20:51–20:56Z window was subsequently qualified
by a guarded all-eight-process read: both `g1` and `g2` starts were after their
retags and before 20:51Z, and every process reported capability 2 and eight
configured shards. A separate, source-reviewed indexed history read counted
15,262 unique accepted measured outcomes, 10,635 successes and 4,627 failures,
or 50.8733/s (69.68% successful). This is descriptively 8.58% above the
later 46.8533/s pre-scheduler window and 7.80% below the earlier 55.18/s
window. Different-time fleet/provider cohorts do not isolate scheduler
causality, and the rate remains below the approximately 75/s steady demand
implied by the current eligible-provider count. Exact scheduler phase, due,
stop and claim-disposition measurements were still pending at that read. The
process read qualifies time and capability; it does not directly attest a binary digest
for every PID.
A separately guarded, exact-eight-process scheduler read for the same
20:51–20:56Z window qualified all groups and retained older metadata rows as
excluded evidence. Synchronous Due occupied 2,359.61 of 2,399.99 observed
scheduler-owned seconds (98.317%); scheduler wait occupied 40.28 seconds
(1.678%) and drain was zero. All eight processes were in Due at the window
boundaries and across scrape means. Extrapolated completed-Due counters
estimated 4,294.03 calls in 2,361.11 seconds, averaging 0.54986 seconds
per call; 4,232.41 were full, 58.52 partial and 3.10 empty, with zero
observed error, invalid or maintenance outcomes. Estimated claims admitted
were 15,311.24, with zero unstarted/ack-failed
claims; completed stop reasons were zero, which is expected to be possible
because five minutes need not span a 900-second pass. Independent turn
counters estimated about 51.044 accepted/s and zero local failures. The
shared-lane scrape means were 466.09 active, 414.95 running, 43.33
finished-waiting and 7.82 queued slots of 512. These counter deltas and
unweighted scrape means point to synchronous Due refill latency, rather
than a pass-level wait or drain, as the immediate scheduler-owned limit.
They do not split API time, model transaction/SQL, post-commit cleanup or
worker CPU, and cannot alone establish causal throughput gain. The next
diagnostic must isolate those Due subphases under loaded Main conditions.
A bounded local diagnostic then exercised the real loopback Due HTTP handler,
model and database with 100,000 providers, 100,000 accepted-history rows,
400,000 retained receipts and eight concurrent shard lanes. Across eight
calls per lane at limit four, HTTP Due mean was 44.403 ms with completed-run
priority off and 59.989 ms with it ready (p95 70.48 and 107.14 ms). All 512
claims were uniquely fenced and durable; no URL probes ran. Claim SQL plus
decode averaged about 16.5 ms in either mode, while empty retention
transactions averaged 0.5–0.6 ms. Fresh custom/generic plans for five
complex statements had 4.67–8.33 ms planning time and 0.57–0.99 ms
no-work execution; the claim statement executed in about 2.0–2.1 ms in
that control. This local fixture does not reproduce the roughly 550 ms
Main Due mean, so it does not justify a SQL rewrite or prove Main's owner.
A source-bound Main HTTP/statement/pool comparison is the next discriminator.
The scheduled 21:27:22Z census reported the first nonzero quota count in
the current sequence: 61 quota-complete and secure-complete providers of
107,834 eligible, with 459,929 accepted runs still needed. This is an
unjoined rolling-cohort snapshot, not a causal rollout comparison.
The later census in the complete 21:18 monitor frame
reported 332 quota-complete providers of 108,049 eligible. This is another
unjoined, changing-cohort snapshot; it shows some current providers meeting
the quota, while the overwhelming majority remain incomplete, and does not
attribute the increase to the scheduler release.
The next complete 21:33 monitor frame counted 649 quota-complete providers
of 108,049 eligible, again without a joined stable provider cohort. Its
edge3 host CPU sample was high, while edge4 had no positive current callback;
the frame explicitly leaves edge4 current host coverage unavailable.
Capacity also limits what Due refill alone can achieve. If the last measured
roughly nine-second turn residence remained representative, filling all 512
configured URL slots from the 466.09 active-slot scrape mean would raise the
roughly 51/s accepted rate only to about 56/s. About 675–700 occupied slots
would be required for 75/s before allowance for setup-only turns, publication
tails and changing provider mix. This is a conditional arithmetic bound, not
a post-rollout residence measurement or proof that 2,048 slots are safe.
Increasing geometry also raises reserved transfer credit and load on shared
Connect hosts. A staged, host-qualified ramp is still needed after the
loaded-Main Due attribution; the deployed setting remains 512 slots.
A first source-bound Main `pg_stat_statements` attempt stopped at its first
endpoint on a combined global reset-or-deallocation continuity guard. The
reader exited nonzero, admitted no Due-family delta, did not run its second
snapshot and was not retried. The retained result cannot tell whether a
global reset or unrelated statement eviction caused the guard; it does not
attribute the roughly 550 ms Due time to SQL. An isolated PostgreSQL 18.6
control then observed 29 global deallocations while a hot tracked entry kept
the same key and `stats_since` and rose from 50 to 400 calls. An evicted and
recreated cold entry changed `stats_since` despite a larger call count, and a
global reset also changed the guard. These controls justify a separately
reviewed survivor-entry measurement that warns on unrelated global churn but
still rejects lost/recreated tracked entries or a reset. Such a delta would
cover only continuously observed entries, not short-lived statements born
and evicted between snapshots. No replacement Main read has yet run.
The operator then requested a higher network service bar: sampled URL DNS,
TCP connect, TLS handshake and per-response-read idle limits are five seconds
each. Server `d881152a` changes only the URL-scoped phase constant and its
focused controls; the caller deadline, 60-second cold owner, non-URL paths,
TLS trust, no-result handling and evidence policy version 1 remain. Phase
budgets are outside the v1 policy payload, which versions redirects, body,
TTFB and throughput. A bare v2 switch would make old API binaries reject new
results and exclude existing v1 quality/quota history; the production
eight-hour ratio and four-hour quota will instead mix old and new deadlines
until earlier results expire. Own and independent validation each passed 33
normal and 33 race controls plus vet. The clean multiarch Taskworker image
`2026.9.30-planetoid-1060083850` has manifest digest
`sha256:c46e9c26417aae45559aef2a86224c256788976d49ffa2428f843d5bd5be3e13`
and embeds `d881152a` with `vcs.modified=false`. Main `g1` selected the image
at 22:49:16Z and retagged at 22:49:19Z; `g2` selected at 22:50:46Z and
retagged at 22:50:49Z. Both deploys exited zero, sampled 20/20 on the new
version, and both registry block tags resolve to the manifest digest. A
full-eight-process start proof and fixed post-change accepted/failure-stage
measurement remain required before attributing outcomes to the new limit.
The last complete pre-change 22:18 monitor frame already counted 7,002
quota-complete providers of 108,069 eligible; this unjoined census preceded
the five-second rollout and is not its effect.
The fixed 22:35–22:40Z pre-change accepted-history window held 15,322
selected-policy measured outcomes, 11,224 successes and 4,098 failures,
or 51.0733/s with a 73.253% success share. A complete cached native-score
publication with source interval 22:34:16–22:34:41Z and publish time
22:46:18Z, all before the first `g1` selection, counted 88,510 native
Quality, 91,125 native Speed and 108,088 online providers. The buckets
overlap; their counts are not additive. This is a pre-change source snapshot
with mixed prior eight-hour v1 history, not an exact ratio-only pass count.
The post-change measurement must use starts after both block retags and
before its fixed outcome window, then compare its own accepted successes,
failures and failure stages. A later complete native publication is needed
to observe bucket change; neither cohort alone proves a causal timeout
effect or the eventual eight-hour equilibrium.

A guarded current-process read found all eight enabled Taskworker processes
started after their respective `g1`/`g2` retags, with capability 2 and eight
configured shards, before the fixed 22:55–23:00Z post-change window. This
proves a current start fence, but does not attest immutable per-process image
identity or uninterrupted process residency throughout that window. The
post-change window held 21,019 accepted selected-v1 measured URL outcomes:
8,805 successes and 12,214 failures. Throughput was 70.0633 accepted/s,
37.18% above the 51.0733/s pre-change window; successful checks fell from
37.4133/s to 29.3500/s, and success share fell from 73.254% to 41.891%.
The largest failure-stage difference was DNS-class (`dial_dns` plus
`request_dns_timeout`), 2,530 before versus 10,920 after. TCP failures were
128 versus 152 and TLS failures 140 versus 101. Different times and provider
cohorts make these descriptive fleet observations, not isolated causal
effects or proof that a particular DNS outcome would have succeeded at 15s.

Every provider in each fixed five-minute accepted-history window had exactly
one measured outcome. Consequently, among the 21,019 providers observed
after the change, **8,805 (41.891%) pass an S/N ≥ 0.6 threshold** and
12,214 fail it for that one-observation window. Before the change, 11,224
of 15,322 observed providers (73.254%) pass by the same one-observation
method. These are fresh five-minute cohorts, not the production eight-hour
native Quality or Speed bucket counts. They exclude setup/no-result attempts,
do not join the same providers across windows, and cannot predict the final
rolling eight-hour pass count until the old evidence ages out. A fresh native
bucket publication is still required to measure the operational count.

The first qualified post-rollout native-score publication evaluated at
23:08:32–23:08:52Z and published at 23:19:18Z, after both Taskworker block
retags. It counted 86,350 native Quality and 88,960 native Speed providers
among 106,533 online. Relative to the pre-rollout source snapshot, these
counts are lower by 2,160 Quality, 2,165 Speed and 1,555 online. Native
buckets overlap and apply other score and tag gates as well as the rolling
eight-hour measured URL ratio. The publication still includes old timeout
evidence and a changing provider population, so its differences do not
isolate the five-second setting or give the eventual steady-state pass count.

The existing complete score export already holds each provider's selected-v1
accepted measured URL success/total pair for its exact trailing eight-hour
query window. Server `e620ef06` adds an optional, source-clocked ratio census
from that in-memory map without another Main history scan. It reports the
configured success threshold for distinct publicly usable online providers,
with passing, failing and no-evidence counts, plus separately labeled counts
for all provider IDs in the source map. Older cached publications remain
readable with the new field unknown. Author and independent focused normal,
race, vet and build gates passed. The multiarch Taskworker image
`2026.9.30-planetoid-1060119460` embeds clean `e620ef06`; its registry
manifest is `sha256:8095fe115f937805b77923c8b42522ba25e8afea72a6528dc100d58ea0fecc01`,
and both Linux binaries had no reachable vulnerabilities in the release scan.
Main `g1` retagged at 00:09:37Z and `g2` at 00:12:32Z on 2026-10-01;
both deploys exited zero and sampled 20/20 on the new version. Both block
tags resolve to the manifest digest. Current-process starts and a complete
post-retag score publication are still needed before a ratio count from this
new field can be attributed to the deployed publisher. Until 06:50:49Z,
even a qualified eight-hour publication mixes pre- and post-five-second
checks; its source population also changes over time.

The operator then raised the quality/speed URL success gate from inclusive
3/5 to inclusive 4/5. Server `9e62c301` changes the shared default; Main's
checked-out `provider.yml` has no ratio override, and no config-updater or
API release is required by the source callsite audit. Focused independent
normal and race runs passed 22 roots and 36 named subtests, including exact
4/5 admission, 79/100 rejection, both native buckets and Online retention.
The clean multiarch Taskworker image
`2026.9.30-planetoid-1060155930` has manifest digest
`sha256:0df16111de5c587d8d06ffe6eb9eedb3cd0f4d6c7b33b943b6fe18b5b71ccec2`
and no reachable vulnerability findings in either Linux binary. Main `g1`
retagged it at 01:12:30Z and `g2` at 01:14:04Z on 2026-10-01. Both deploys
exited zero, sampled 20/20 on the new version, and both registry block tags
resolve to that digest. A bounded current-process read found all eight enabled
starts after their respective retags with capability 2; the latest start was
01:14:16.510Z. That is the minimum source-start fence for a new 4/5 score
publication. The read does not prove old writer retirement, immutable image
identity per process, URL ownership or the effective mounted ratio override.
A separate bounded mounted-config read ended incomplete before any host row
qualified, so override presence remains unknown. A fresh complete census
publication with numerator 4 and denominator 5, sourced after the eight-start
fence, is still required to confirm the effective Main threshold and count
passing providers. Even then, its trailing eight-hour evidence remains mixed
between prior and five-second phase limits until 06:50:49Z.

The first qualified 4/5 score census evaluated from 01:15:26.418Z to
01:15:56.898Z and published at 01:26:27.652Z, all after the verified
01:14:16.510Z eight-process start floor. Among 106,199 distinct publicly
usable online providers, 21,578 passed the inclusive rolling eight-hour URL
success ratio (20.318%), 84,198 failed, and 423 had no measured URL evidence.
Among the 105,776 with evidence, the pass share was 20.400%. The same
publication reported 20,320 native Quality and 21,578 native Speed providers;
the buckets overlap and include other admission gates. Its ratio metadata
reports numerator 4 and denominator 5, establishing the effective threshold
for this complete publication despite the earlier incomplete mounted-file
attestation. The exact evidence window was
`(2026-09-30T17:15:26.797695Z, 2026-10-01T01:15:26.797695Z]`, so it still
contains pre-five-second checks. This is one publication from a changing
population, not proof that old writer processes retired or that every future
publication will stay at 4/5. A later stable publication and current writer
retirement check should close that gap.

A second distinct complete score publication evaluated 01:40:56.097–
01:41:35.720Z and published 01:46:22.829Z. It also reported the exact 4/5
ratio. Among 107,747 public online providers, 20,602 passed (19.121%),
86,955 failed and 190 had no measured URL evidence; native Quality was
19,406 and native Speed 20,602. Its exact accepted-history window was
`(2026-09-30T17:40:56.436549Z, 2026-10-01T01:40:56.436549Z]`.
The first-to-second aggregate changes include 1,548 more online providers
and 976 fewer ratio passes. These are unjoined changing populations; they
do not identify which providers changed state or isolate the higher threshold
from continuing five-second evidence turnover. Both publications establish
that the active score export used 4/5 at their source times, while complete
old-writer retirement remains unproved. A second bounded mounted-config
attestation stopped at its first host with SSH exit 255 and produced no
qualified host row; it neither proves nor disproves a mounted override.


## 2026-10-02 settlement-cache deployment and unresolved acceptance

Main server source `36f43eefefe1ef4153c526d3c3b656cceb19b368` includes the
revision-checked reservation read-through and exact snapshot preservation
through settlement and terminal metadata updates. Release suffix `4000` was
selected for API, Taskworker and Connect. At 12:43 UTC, the source-qualified
API pressure read found all 20 API processes current and ready. Eight
Taskworkers were observed on `4000` at 12:47. Nineteen of 20 Connect slots had
been witnessed on `4000` across different observations by 12:54; this is not a
simultaneous fleet sample, and edge3/g3 still reported `3900` at 12:54. These
observations do not establish predecessor retirement or every serving path.

The 12:43 five-minute API pressure read reported 608,791 completed
`/connect/control` requests, mean duration 0.927 seconds, cancellation share
2.10%, 1,967 requests in flight, and mean PG acquisition time 5.6 microseconds.
Across the 19 cells with comparable counter endpoints, reservation refreshes
reported 594 snapshot reuses and 40 reloads; settlement reported 579 reuses and
55 reloads. These count completed snapshot reads and attempted settlement
balance operations, respectively, rather than committed financial operations,
saved SQL calls, or per-query CPU. They establish that the guarded cache path
was exercised in this interval. The earlier `3900` refresh read at 11:40 had
zero reuses and 453 reloads in five minutes, with a different population and
clock; that comparison does not isolate the new release's effect.

Local financial validation covered settlement, rollback, replay, missing and
already-settled rows, and a concurrent legacy writer. Author and independent
34-root race runs passed in 135.810 and 125.225 seconds. In the bounded local
20-close fixture with 10,001 surviving escrows and no intervening admission,
the baseline performed 20 mirror censuses; the new warm snapshot performed
zero main and zero mirror censuses, and a cold snapshot performed one main
and zero mirror censuses. Revision mismatch, incomplete knowledge, and an
unexpected mutation retain the exact census fallback. This fixture does not
predict Main coverage when other transactions change the revision.

Standing Main PG CPU at 12:45:20 was 52.754 cores out of 96 (54.95%). The
separate 12:55 PG state sample reported 203 active sessions, 90 idle in a
transaction, and 695 client sessions. These clocks are not a joined sample
and do not identify which query consumes CPU. A fresh bounded catalog sample
remains outstanding; an unrelated SSH transfer blocked its quiet precontact
gate. The successful pre-cache 10:52 catalog sample identified the normalized
1,023-row reservation-page census as the largest sampled active/no-wait
family, but activity samples are not per-query CPU measurements.

The scheduled URL coverage observations at 12:36:02 and 12:51:18 remained
`owner_unavailable`. The former lacked qualifying owners for shards 0, 2 and
6; the latter lacked owners for shards 0 and 2. They reported no qualified
current numeric quota census. All eight current Taskworker versions alone do
not establish active shard ownership, accepted measured-run throughput, or
ten-run rolling coverage.

The next scheduled observation at 13:06:30 produced a qualified coherent
census: 113,561 eligible providers, zero quota-complete or secure-complete,
98,115 due, 113,558 overdue, three warming, 15 uninitialized, and 786,714
measured runs still needed. It reported 17 security-pending providers, one
unknown recovery target, and an oldest due age of 192,746 seconds. A separate
13:09:20 owner read found all eight expected Taskworker slots at capability 2
and configured geometry eight, with exactly one fresh owner for each shard
and heartbeat ages 15.87–65.25 seconds. This later evidence resolves ownership
for that observation; it does not identify the cause of the earlier gaps or
establish sustained throughput. The current-process hourly rate window remains
incomplete. The fresh native bucket/ratio publication, durable shard-cleanup
validation, and remaining scheduler and ARIN policy acceptance are still open.
No quota, DB CPU, or end-user connection recovery is claimed by this checkpoint.

Evidence: bounded runtime receipt
`Connect-Taskworker-settlement-snapshot-runtime-1247-v1/run-20261002T124742Z`
(SHA-256 `70891c754f99259d2d1cf0e95275c523e5421eca85373783bd5e1d0502ffa688`),
API receipt `API-current-settlement-snapshot-pressure-https-1243-v2/run-20261002T124317Z`
(SHA-256 `c5a0ea83b19f3e01696efee493be14b811c8a2b03d6992110f022176e73ee121`), and the retained scheduled URL alert scope
(SHA-256 `1dc907abbe6df9456b000b52406ffc57e5065f0abad75b1fea108660ad9fd56a`).
The source-local author financial receipt is
`settlement-snapshot-preservation-20261002/author-review.json`
(SHA-256 `a85c9eb856544b155f470c8736da3b34941181a348a959b5880a8bb28436595b`);
the independent receipt is `independent-monitor-financial-review.json`
(SHA-256 `d0081bd25d8f0a794ec8e41aded578d843c96c63e597f3d6aca47bc8891944c5`).

The 13:09 owner receipt is `URL-owner-qualification-4000-1309-v3/run-20261002T130920Z`
(SHA-256 `1440ddf2084ef1b147c1d98d95d2620ec383b2bbde02daf641bde0c743a6e3ad`);
it is an instantaneous metric observation, not a
durable task-lease, cleanup, or accepted-history join.
The 13:06:30 scheduled census is in retained scoped record 475
(SHA-256 `e781a2d0600959ebe09abf565fdb9caafaf21b9c8918bb6fa662a2efe62ae35f`);
the earlier `1dc907ab` scope supports only the 12:36 and 12:51 observations.


## 2026-10-02 contract lifecycle and probe release checkpoint

The timeout and funding changes are committed on Main. Taskworker release
`2026.10.2-probe-timeouts-headroom+1060864100` uses clean server source
`e072f9e87ebb4a354208132c1f1c771a69b6442f`. Both Linux image binaries were
verified against the local build and embedded VCS metadata. Both blocks were
selected at 15:56 UTC; the 16:14:29 UTC process-metric witness qualified all
eight enabled slots against the verified image configuration and source.
These self-reported metrics do not prove predecessor retirement, readiness,
or a funding or throughput improvement. The release implements DNS/read-idle
5s, TCP/TLS 3s and tenfold conservative private-shard funding without assuming
reclamation during a pass. Existing financial obligations remain protected.

The coherent 16:35:33 UTC scheduled census had 115,061 eligible providers,
2,449 quota-complete and 2,448 secure-complete, with 515,915 measured runs
still needed. Cohort changes and incomplete hourly process/measurement ranges
prevent attribution of that change to this release. This is about 2.13 percent
quota coverage, far from the required ten accepted measured runs for every
eligible provider in the rolling four hours. Local allocation failures cannot
be counted as provider failures or quota credit.

The private companion-chain correction passed independent actual-PG race
tests for 15 Model and three Controller roots and is merged. It preserves an
exact reverse anchor's private payer, re-resolves after releasing the first
gate/transaction, and preserves ordinary destination-payer semantics. This
repairs a supported encrypted-chain path; current default encryption is off,
so it does not establish the cause of Main's cancellation deficit.

Two distinct admitted-result cleanup races are reproduced and repaired in
Connect source `6443417d70dc5825f442c18461a3ec874fed884e`, directly descended
from the deployed `e1b5d77b5029` plus the known-result correction `a863a03b`.
A locally committed result arriving after cancellation gets one finite, joined
requester-only zero-use close through the original authority. A result whose
callback is still running when the manager closes is now joined before OOB
admission retires, and cannot refill a final-flushed queue. No provider close,
unknown-result guess, creation replay or early reservation release is added.
Independent SDK race/vet and six real-controller financial/lifecycle roots
passed. The initial one-connection financial test used a 1-GiB private grant
and thus exercised ordinary fallback. The follow-up `eed038cd` uses the actual
configured URL-probe grant and requires exactly one `selected_first` counter
increment. The old SDK reproduced a committed allocation with no requester
close; the combined correction passed both fallback and selected-first roots
under the race detector, independently in 10.380 seconds. Both preserve the
provider reservation until independent normal provider settlement. This adds
selected-path correctness evidence, not a loaded-Main latency or frequency
claim. API, Connect and Taskworker deployment of this integration was still
pending at the original checkpoint above; subsequent rollout/runtime receipts
must establish its serving state separately.

The UTC-only sweep insertion repair passed independent nine-root financial
race tests, including the original denormalization failure and timezone/replay
controls. Main already uses UTC; this repairs test/product correctness and is
not attributed to the outage. Full Model partition closure remains outstanding.

The continuous sampler is running in the single authoritative monitor, clean
source `7a5f1ae8a87c809908728b28f8b98fd3bf221c92`, PID 4144942. The prior
cgroup was retired before the transient unit was recreated with the same name,
launcher, append paths and cadence; the handoff includes an explicit collection
gap and possible canceled callbacks. Its first durable admitted attempt at
16:34:22–16:34:27 UTC failed `source-unavailable` with zero snapshots; the next
durable eligibility is 16:49:27.582939897 UTC. No successful recurring Main
database sample is established. An offline regression proves that shared-slot
expiry can precede sampler state admission and the generic sustain gate can
hide the first failure; this is not yet proof of the initial runtime cause.
Fair bounded admission and finite bootstrap diagnostics are being corrected.

Evidence: Taskworker runtime receipt SHA-256 `1732b96ba280860ed5b24f834e6cf06529bf6b67806d3a1c6dcc319f68945c2e`;
companion independent review `7167933b006ac7d5dcf87fbe9c72a53a27362e5ce7307e22bd5b73d4dec05704`;
combined lifecycle review `77babeccb5aef8e751aa592917fd2d8a1b513ddf224ee8c0845c338341a58b21`;
UTC sweep review `2979c5181770f2ed594dfae7ab419184fb3e95c9c16b9c49e8d196eb1220edc7`;
failed sampler receipt `a293c8f053d436cbaa876df74ef4e8614ae260919b56cb839410a4dea8197b9a`.
The fresh native-bucket/request-local acceptance, ARIN subscriber shadow,
loaded Main query cause and complete four-hour coverage remain unresolved.

### 17:24 UTC evidence addendum

The exact task read at 13:54 established eight URL shards with concurrency and
limit 64: 512 configured slots, outer timeout 60s, maximum pass time 900s and
idle delay 5s. All eight had one advisory owner and zero task errors in that
snapshot. It does not prove continuous occupancy, timely due admission or a
completed provider sweep. The older four-row/208-slot blackhole sizing example
in the operator guide is now explicitly historical.

The matched 13:40–13:45 window remains the durable acceptance baseline:
4,650 qualifying policy-1 URL measurements, with 1,445 successes and 3,205
failures, or 15.5 accepted measurements/s versus approximately 78.86/s needed
for that eligible cohort. Scheduler counters in the same nominal window
reported 95.801 attempts/s, 15.489 accepted/s and 80.312 local failures/s;
they are not a row-for-row join. g1 had nearly zero scheduler acceptance.
Its DNS cohort had 14,444 timeouts and zero answers, versus approximately
1,823 g2 answers. The corresponding local create-contract handler counters
showed g1 14,548 cancellations versus about two handler successes and mean
4.94s residence. Constructor/route admission is not a usable contract, handler
success is not a durable URL receipt, and unmeasured local failures remain
excluded from the provider denominator. The earlier completed-stage window
13:37:20–13:42:20 overlaps but cannot be divided into these later cohorts.

A fresh bounded holder snapshot at 16:59:23 observed ordinary, legacy singleton
and shard-registry grant waiters (35, 14 and 6 respectively). One retained
active/no-wait reservation-census family had query age 6.675s and transaction
age 23.604s. Selected holder depth and fanout were truncated; registry ownership
is historical, and no output joined an owner cohort to that particular holder
or to g1. These are positive contention witnesses, not per-query CPU shares
or proof that every probe cancellation had that cause.

The 17:24:17 release witness found API20/20 and Taskworker8/8 qualified against
`2026.10.2-contract-lifecycle+1060864200`, source `bf7d6205`. Connect had15/20
qualified, two fresh explicit4000 processes and three strict unknown slots.
Only API readiness was observed. Completed tag-selection commands, self-reported
metrics and stale/unjoined predecessor generations do not prove current DNAT,
predecessor retirement or continuous serving health. That partial Connect
boundary remains open.

The recurring database sampler also failed at16:49:27–16:49:32 and
17:04:32–17:04:36 with coarse `source-unavailable` receipts and no completed
sample. Its cadence is observed; query coverage is not. The isolated scheduler
and finite source-phase corrections are in independent verification and have
not yet supplied a successful Main sample. The prior receipts cannot identify
whether bootstrap, transport, authority or SQL failed. No CPU, useful throughput
or complete rolling-quota recovery is claimed by these checkpoints.

Evidence SHA-256: task geometry
`ffad4f4acd7ef26486fb593074868026c8a56c557756602a84a903880b268236`;
durable measured window
`d367708d5b4d89e433392a9a4f89b38d3f94215fd23e68eb4ae1096435bf88fb`;
fresh holder
`64a41fe34f44f575bb117b4fb6c81c9f518b4f6836abeb8c3e224e46d58deaca`;
4200 runtime
`d42b2be91f4ab510f7573ce11dcf519d715c27f28e41481c268502ba6a2a59a6`;
selected-grant independent financial control
`ea267bc6eb1256ab4312d445173178da5883adb0d2d8e73d2763d3ca41c62d59`.


## 2026-10-02 17:25 UTC lifecycle rollout and metadata contention checkpoint

Release `2026.10.2-contract-lifecycle+1060864200` was built locally from clean
`bf7d620517b082bfd6a485765ff929f5996a7f2c`, using Connect `6443417d70dc`.
All three multiarchitecture registry graphs were verified against the actual
local binaries, embedded revision, SDK module, and isolated version/help runs.
API, Taskworker and Connect all-block deployment commands completed at
17:03:53, 17:06:12 and 17:14:04 UTC respectively. Command success is selection
progress, not proof of fleet convergence.

A single source-qualified HTTPS metrics read at 17:24:17 UTC found 43 of 48
expected fresh source/start witnesses: API 20/20, Taskworker 8/8 and Connect
15/20. API readiness gauges were positive. Two Connect witnesses explicitly
reported the older 4000 source; three were strict-null and require generation
triage. A strict-null result can include incomplete predecessor coverage and
is not proof that a new process is absent. These self-reported metrics do not
prove executable/container identity, old-process retirement, DNAT, Connect
QUIC readiness, Taskworker readiness, or improved accepted throughput. The
Connect rollout boundary remains open.

Independent actual-PG tests now also cover the configured large private shard
grant: selected-first allocation is positively counted with one pool
connection, late-result cleanup closes only the requester, and reservations
remain until an independent provider close and normal financial settlement.
The smaller ordinary-fallback control is retained. These are correctness
controls, not measurements of Main incident frequency.

A separate settlement-metadata race was reproduced deterministically. Metadata
read a current reservation snapshot without locking the corresponding balance;
concurrent admission then advanced the revision before metadata updated its
settled flag. Guarded publication correctly rejected the stale prediction, but
the next reader repeated the exact reservation census. The merged correction
locks every existing balance for the terminal contract in sorted order before
escrow/cache work, including balances outside a partial payout map. Custom and
generic false-zero plan controls with 100,000 unrelated rows used three live
escrow rows and three balance primary-key probes. Independent eight-root
financial race tests passed, including interleave, partial-map, rollback,
ambiguous commit, legacy, missing/zero and numeric-extreme controls.

One loaded two-process comparison improved 64 admissions from 3 completing
inside their five-second limits to all 64 completing in 1.296 seconds while
another process settled contracts; candidate accounting remained exact.
A later baseline also completed all 64 within the limit, so deadline failures
are schedule/load dependent. Its overlap still performed 81 exact snapshot
reloads with ten sampled concurrent censuses. The deterministic cache race,
not a guaranteed timeout ratio, establishes the mechanism. This correction
needs new API, Connect and Taskworker binaries; its Main performance benefit
is not yet proven. No migration is required.

The scheduled 17:21:18 UTC census has 113,314 eligible providers, 9,226
quota-complete and 9,223 secure-complete (about 8.14 percent quota coverage),
with 395,364 measured runs still needed. The earlier 17:06 census was
9,111/114,885. Cohort changes and incomplete hourly ranges prevent a sustained
rate or fixed-provider recovery claim. Standing PostgreSQL CPU at 17:07:17
was 40.15 of 96 cores (41.82 percent); the separate 17:06:55 state sample had
164 active sessions and 54 idle transactions. These do not identify query CPU.

Four actual continuous query-sampler attempts have failed source-unavailable;
the latest finished 17:19:41.625 UTC and reserved its next eligible time at
17:34:41.625. There is still no successful recurring query sample. Fair bounded
scheduling and finite source-phase diagnostics are implemented, with independent
monitor gates in progress. A failing local test launcher was isolated to a
pyenv shim under restricted PATH and repaired; it is not the cause of the
unclassified production failures. The authoritative watcher and 15-minute
floors remain intact. Native bucket acceptance, ARIN shadow activation, full
Model partitions, loaded Main attribution and complete provider coverage remain
outstanding.

Evidence: all verified images `bbd65107dd75f94b7aa23cb36038d62e1f99166a73cc9c7f848e8d5f5ff9de9d`;
all deployment terminals `886db690583fe09dd3f49b4ff2135c7c896c9548fe075fbf3669daa5e424cc1d`;
fleet witness `d42b2be91f4ab510f7573ce11dcf519d715c27f28e41481c268502ba6a2a59a6`;
selected-grant independent review `ea267bc6eb1256ab4312d445173178da5883adb0d2d8e73d2763d3ca41c62d59`;
metadata-fence independent review `0ee9845667b087768e6a91757fb03602eae21473c5919f4ce1bc0f86648dd89e`.


## 2026-10-02 18:17 UTC metadata release and monitoring handoff checkpoint

API, Connect and Taskworker multiarchitecture images for
`2026.10.2-metadata-balance-fence+1060864300` were built from source
`3213739b6be57a71d502fb63cc97bf387a4851dd` with Connect SDK `6443417d70dc`.
Registry graphs and embedded local binaries were verified. All-block deployment
commands completed successfully at 17:51:13, 17:59:03 and 17:50:00 UTC,
respectively. Actual 4300 runtime convergence remains unverified; the earlier
43/48 witness applies only to 4200 and must not be reused as current proof.

The final matched local two-process financial comparison completed all 64
admissions and 64 settlements in both arms. Both preserved exact durable and
cached accounting. The old implementation performed 81 exact reservation
snapshot reloads with ten sampled concurrent censuses; the balance-lock
correction performed zero. This establishes the local mechanism and preserves
accounting, but does not establish Main CPU recovery or a guaranteed timeout
improvement. Independent focused financial and integrated cross-patch race
controls passed. The full Model suite remains incomplete: its owning partition
has a static clock-order guard failure under investigation. Initial source
triage found two mutually exclusive endpoint-lock branches where the older
static guard expects one textual marker; no runtime monetary failure has been
established by that failure.

The actual scheduled 18:06:53 UTC census reports 112,542 eligible providers,
5,529 quota-complete and 5,525 secure-complete, about 4.91 percent quota
coverage. It reports 398,306 measured runs still needed. The preceding 17:51:41
census was 7,994/112,629, about 7.10 percent. Cohort changes and incomplete
hourly history prevent fixed-provider or sustained-rate attribution. This is
not quota recovery and the 100-percent coverage requirement remains open.

Seven recurring database query samples have failed, with no successful sample.
The latest old-watcher attempt completed at 18:05:21.466533523 UTC and retained
its next eligible floor of 18:20:21.466533523. The reviewed fair-admission and
finite failure-phase monitoring correction was promoted through one same-unit
restart. Old PID 4144942 retired; new PID 193014 started at 18:15:32 UTC and
runs exact source `1ff1a4e2c520a5822c5c2b69d1d79bb11c610976`. Independent
posthandoff audit confirmed unchanged unit/launcher, the 15-minute interval,
all durable cadence clocks, and no duplicate watcher. The startup delay makes
the earliest new active callback 18:30:32 UTC; this handoff has an observation
gap and is not evidence of a successful query sample. Database load recovery
and query attribution remain unverified.

The nonactivating ARIN shadow correction passed independent focused race
controls. It retains the owning lookup clock, matches current census facts,
and classifies missing/stale observations as indeterminate rather than proven
provider loss. A separately staged policy-two resource, verified subscriber
coverage, bucket census, and activation evidence remain outstanding. No active
catalog replacement or classification cutover is claimed.

Evidence SHA-256: all 4300 deployment terminals
`6042af765c80cc1ed2bfbf81e5bfaa02746fce5657ba05f4fa86239b24725d10`;
matched local financial comparison
`a877f338f954c8149f245bb98ef3e8c577200ee691be42134e8377cea8eb0023`;
monitor promotion
`3a43bed6e4c91889047971abb495da93687f41970f030f2592c51863d107d386`;
independent posthandoff audit
`3624856d5e17548aa24a58adc5ef7ec54e34821167217a41249c7d8759a837c5`;
independent ARIN source review
`5f561e5ee7273d7c4b6880cd3bd0eebf5b8ecd9c118ab8c3110638fd5c9c3c9b`.


## 2026-10-02 18:27 UTC all-block source witness

One bounded source-qualified HTTPS read at 18:27 UTC found all 48 enabled
API, Connect and Taskworker slots with fresh source/start/config witnesses for
`2026.10.2-metadata-balance-fence+1060864300`, source `3213739b`. API was
20/20 with positive readiness gauges; Connect was 20/20 and Taskworker 8/8
for release identity. This supersedes the earlier partial 4200 version witness.
These self-reported instant metrics do not establish direct executable identity,
predecessor retirement, DNAT, Connect QUIC readiness, Taskworker readiness,
continuous health or accepted probe throughput. No additional rollout or restart
was performed to obtain this read.

The owning reader review caught null deployment floors in the actual reducer
pins before contact. The correction binds all twelve parsed ISO floors and
rejects loaded-pin/manifest disagreement before requesting Main; independent
32-control verification passed. A local interpreter import failure also occurred
before any contact. The actual read used the independently tested interpreter
and made exactly one production request. Receipt SHA-256:
`2859398f3caf23b350131b576d3b499d47f9027eacf1e556eb4c073e44f18d92`.

The latest retained automatic PostgreSQL CPU sample, at 18:07:52 UTC, was
50.318 of 96 logical cores (52.41 percent), over a 5.01-second interval.
A separate 18:07:24 state observation had 279 active sessions, 143 idle
transactions and 711 clients. Both precede the monitoring handoff and neither
attributes current load to a query or proves recovery. The first new scheduled
query sampler remains pending.


## 2026-10-02 18:30 UTC first classified recurring database failure

The first autonomous query-sampler attempt from the promoted watcher ran at
18:30:32.598286774–18:30:39.350513219 UTC. It failed at `history_start` with
`statement_timeout`; its finite diagnostic was not truncated. This establishes
that the initial query-history snapshot timed out, rather than identifying an
SSH/bootstrap or missing-source failure. It does not identify the responsible
application query, provide a valid historical share, or establish recovery.
There are still zero successful recurring samples. The failed attempt retained
its next eligible floor of 18:45:39.350513219 UTC; no manual retry was run.
Astra is investigating the owning snapshot query and its dependencies. Receipt
SHA-256: `da96a82accd316c8330ad0de8fd0a9136241f090417e60344827feedb2526881`.


## 2026-10-02 19:30 UTC throughput and sampler checkpoint

The automatic 19:22:11 UTC URL census had 113,823 eligible providers,
1,649 quota-complete (1.45 percent), and 1,643 secure-complete. Its cohort
changed and the hourly coverage remained incomplete; this is not a matched
provider recovery comparison. The 19:16:04 UTC resource sample measured
58.022 PostgreSQL cores out of 96 (60.44 percent), over 5.01 seconds.
Neither result establishes query attribution or recovery.

Three bounded current-worker metric reads used eight source/start-qualified
Taskworker references for the deployed 4300 release and the same fixed
18:55–19:00 UTC interval. Completed timed turns averaged 5.037 seconds;
setup phases totaled about 69 milliseconds, while check-and-buffer averaged
4.959 seconds. Acknowledged outcomes were approximately 24.56 per second;
these counter increases are not durable per-provider quota proof. Local failures
accounted for 73.95 percent of attempts. DNS timeout waves spent about 4.94
seconds after route admission. Route admission establishes registered-channel
selection, not contract availability, an actual provider write or provider fault.
Data-only probes intentionally omit the initial ping/evaluation exchange.

Qualified contract-frame counters averaged 1.903 seconds, with 34.41 percent
canceled and 475 inflight. Successful PostgreSQL pool acquisition averaged
110 microseconds; canceled acquisition duration and several sparse error,
settlement and refresh families remain unknown. Admission cache reuse was
99.95 percent; creation-cache reloads were zero in the qualified interval.
These are separate cohorts, without a request/provider/SQL causal join.
The next discriminator is contract phase timing and the first provider write.

Sampler source `78a7e518` materializes normalized history text once before
classification, preserving existing filters, aggregation, caps and timeouts.
A controlled PostgreSQL fixture reproduced the old three-second timeout;
the corrected full twelve-frame program completed. Independent full monitor
normal, race and vet gates passed. One same-unit restart at 19:26:50 UTC
replaced PID 193014 with PID 303929 and exact binary SHA-256
`42f074e9c9051c1e4600eae2bea57dcdd21b6d8831ea26c48abadd74534884f1`.
The restart succeeded, but its original verification script exited on a local
/proc executable permission error. A subsequent privileged read-only check and
independent audit verified the binary, retired old PID, unchanged unit/launcher
and all retained cadence clocks. No second restart occurred. The startup floor
makes the first new active callback no earlier than 19:41:50 UTC; successful
Main query sampling and recurring coverage are still unproven.

The UTC straggler-reap timestamp correction is committed on Main as
`9a7abf78`; seven financial roots passed independent normal/race controls,
including the previously failing backfill test and cross-zone retention checks.
It is source-only, not a deployed fix or current Main outage attribution.
The 328-root model normal partition completed with 325 passes, zero failures
and three skips. Whole-model normal/race verification remains incomplete.
Accepted health-history retention, current native bucket counts, complete ARIN
shadow coverage and sustained 100-percent probe coverage remain open.

Evidence SHA-256: fixed-window metric analysis
`1ca9c6fb20cd2c5a5b86009356cc93efc06cc31e6282891931405267791730cc`;
sampler full independent gates
`10680dc2ca2ff099104c69c6fa8bdaa1f04a2b8cdc45538b596749d65da17493`;
recovered promotion verification
`4af2f4fc960b98c679bcddb0d822a9ec0d5a6fe62ec912d5c3fce89c6464e437`;
independent handoff audit
`d42f73deb6aa7ee11309aa53de0b6251c3df5dd7838449342414b536db07100d`;
UTC financial gates
`bb8e2ce9c44ff0ab6d3e8cf35a9396e7e310574ca27ee343939df7b9a15247e0`.


### 19:42 UTC first successful Main query sample

The corrected autonomous sampler ran at 19:41:50.424922150–
19:42:17.640870401 UTC and completed all twelve snapshots. Its next
eligible floor is 19:57:17.640870401 UTC; a second eligible completion is
still required to prove recurring coverage. No manual retry occurred.
The retained receipt is
`bdbed2288686529f798e1b7ffd3e4a0b3d5a55c7d019ece8be2299b3fc1c1336`.

The grant-lock tuple-wait cohort totaled 1,423 backend-samples, with a peak
of 127 and maximum query age of 72.94 seconds. Escrow-access tuple waits
peaked at 47, and settlement balance-lock tuple waits at 44. Selected grant
and settlement waiters shared one opaque active, non-waiting blocker classified
as a reservation-census prefix. These are direct backend lock edges, without
logical payer, private-probe, service or full-statement attribution.

Load/completed output and blocker selection were capped; 217 group-samples
were omitted, and activity query text was limited to 1,024 bytes. The blocker
sample had 78 edges for 16 selected waiters out of 228 reported lock waiters.
PGSS 1.10 histories remain interval-unqualified; these findings do not quantify
CPU share or prove exclusive root cause. Independent initial-triage SHA-256:
`25e27a1b1d8162895d387218a37331aaab368cc8491ba130ba85dae29a931c63`.

### 20:47 UTC deployment and recurring query-sample checkpoint

The autonomous query sampler has now completed five twelve-snapshot Main
runs, most recently at 20:43:35.148096191–20:44:00.220028633 UTC.
Recurring visibility is established; database recovery is not. The latest
sample retained grant-lock tuple waits peaking at 121, escrow waits at 55,
and settlement balance-lock waits at 54. Selected reservation-census-prefix
backends were active without waits. Load/completed output and blocker selection
remain capped, and the samples do not identify payer, service, full statement,
or CPU share. The latest receipt is
`0484e84225465e28664c313c1ebab02b29771ddd93c901a39c92dbc065eb2053`.

The coherent 20:44:03 UTC URL census reports 112,526 eligible providers,
160 meeting the ten-run rolling quota (0.14219 percent), and 158 meeting the
security gate. Hourly process-counter coverage is incomplete. This is not
sustained quota recovery. The last numerical CPU sample is historical and
must not be presented as current database CPU.

Release `2026.10.2-contract-path-timing+1060864600`, source `ea2a7abd`,
was built locally, pushed, and selected by successful all-block deployment
commands for API, Connect and Taskworker. It includes the UTC reap correction,
contract stage/outcome instrumentation, and DNS no-provider-write diagnostics.
Instrumentation does not itself establish a performance improvement.
The release preserves the intervening Main blob-store changes and published
Connect SDK `e0d75562aa23`; focused financial, probe lifecycle and probe-package
race gates passed against that exact source/dependency combination. Whole-model
normal/race closure remains incomplete.

A fresh read-only runtime witness at 20:47:08 UTC qualifies the source, start
and image-config reports of all 20 API and all eight Taskworker instances,
but only 14 of 20 Connect instances. Three Connect instances still report the
previous 4300 build, and three lack a qualified current-process witness.
API readiness gauges are positive; Connect and Taskworker readiness, direct
container identity and predecessor retirement are not proved. The eight
Taskworker references qualify the next fixed-window timing discriminator.
Runtime receipt:
`c26377569bd66138fd57048bc065f366bba47e01fe2486c2b5c5499d8cfa997e`.

A two-connection local PostgreSQL control reproduces a cold settlement census
holding the same balance row while another connection times out; the row
becomes available after commit. This proves a possible lock amplification
mechanism, not its Main caller or exclusive CPU attribution. The financial
repair and full current/legacy census fingerprint discriminator remain in
progress. No speculative query rewrite or removal of financial guards has
been deployed.

Current native bucket counts remain unknown: the bounded Redis census attempt
failed before authentication or any GET because its direct connection timed
out. ARIN subscriber-policy activation, complete current-owner shadow coverage,
accepted health-history retention, full tests, and sustained 100-percent
eligible-provider quota coverage remain open.

### 21:40 UTC settlement rollout and admission consistency requirement

The cold settlement repair is committed and pushed as `075bd3b8`.
Seventeen financial race roots passed author and independent runs; the
independent current-source/published-SDK integration also passed. The repair
removes cold balance-wide census work from the financial lock scope, retains
warm revision-guarded deltas, and orders cold metadata before committed mirror
rebuilding. It does not remove admission's own balance/revision contention.
Independent financial gate:
`be978fb1baccd9c13a8bd60d0ed33c14d3f82baf48325982e9d6e33182b06579`.

Release `2026.10.2-settlement-lock-scope+1060864700` was built, pushed and
verified for both Linux architectures for API, Connect and Taskworker. All
three all-block deployment commands terminated successfully by 21:34:51 UTC.
Fresh running-instance convergence and subsequent performance remain unproved.
Deployment manifest:
`91f89647be64a9daea13e1cc7a899f1f0ce29a49e6de56b136b327dd05f83eb1`.

The qualified eight-Taskworker timing window at 20:40–20:45 UTC attributed
95.80 percent (g1) and 96.41 percent (g2) of completed synchronous contract
residence to the process-local payer gate. For DNS timeout waves, 60.10 and
74.61 percent respectively carried an affirmative same-tunnel local-contract
failure/pending witness with no provider write. These are not a cross-request
SQL join, accepted quota counts, or measurements of the 4700 repair.
Receipt:
`b0594f05e59ba62021dabd942f2108e58db4adfc6a5e022e4d29510b1b35a77a`.

Seven consecutive successful autonomous query samples establish recurring
visibility; the eighth also completed at 21:30:15 UTC with persistent grant
and settlement queues. The coherent 21:29:38 URL census reports 112,344
eligible providers and 62 meeting quota; hourly process-counter coverage is
incomplete. A retained 21:02:17 CPU sample measured 44.264 PostgreSQL cores
out of 96 (46.11 percent over 5.02 seconds), before the repair deployment.
This supersedes the previously retained 19:16 CPU observation, without
attributing an improvement to a fix.

The operator now explicitly requires contention-free contract creation for
large numbers of clients on the same network and accepts approximate
accounting at crash/reconciliation edges in exchange for eliminating admission
contention. This supersedes absolute cache/admission consistency as a release
criterion for the replacement admission design. The deterministic regression
must use many distinct clients sharing a funded network across independent
processes, demonstrate the current contention, and verify the repaired path
without hiding serialization behind a local gate. Healthy concurrent
reservation behavior, throughput, and the accepted failure/replay/reconciliation
windows must be tested and documented. This replacement is not implemented or
activated by the 4700 settlement repair.

Current source has no distributed Redis admission mutex: September commit
`35274673` made admission and settlement debit atomic using PostgreSQL locks
and durable reservation reads. Redis currently mirrors committed state.
The later local-controller path calls the same model and did not remove a
mutex. Creator revision-trigger and snapshot writes also need attention in
the replacement; removing only the local gate or balance lock is insufficient.

ARIN full current-provider shadow coverage, hosted proxy local-controller
integration, and dashboard corrections continue as separate work. New agent
threads were rejected by the agent service's thread limit, so an existing
Astra Max agent was reassigned exclusively to ARIN coverage in parallel.

### 22:07 UTC admission discriminator and ARIN completion priority

The completed 4700 timing read covers all eight qualified Taskworker processes
in the fixed 21:40–21:45 UTC window. Payer-gate waiting accounts for 97.02
percent (g1) and 97.13 percent (g2) of completed synchronous contract residence;
mean returned-call times are 1.715 and 1.594 seconds. For DNS timeout waves,
83.74 and 75.52 percent respectively have the affirmative same-tunnel
local-contract pending/failure witness with no provider write. This confirms
that gate dominance persists after the settlement repair; it does not join
individual requests to SQL waits or establish accepted quota recovery.
Receipt: `80d72d36cb6e4b961ac35ab8c7ca7a8abdf3e8415bc100afdc68feda92219129`.

The 21:50 UTC runtime read qualifies 44 of 48 source/start/image-config reports:
20 API, eight Taskworker and 16 Connect. Four Connect reports remain unavailable,
with no positively observed older build. These are runtime reports, not direct
container identity, predecessor retirement, or customer recovery proof.
Receipt: `ea71bdb55363ca5e184ae2773e1a6028016c91d32f4c4e2dcd495d66d2c6a18e`.

The coherent 22:00:02 UTC URL census reports 112,355 eligible providers and 365
meeting the rolling quota (0.3249 percent); hourly expected-process coverage
remains incomplete. The 21:47:44 UTC resource sample measures 46.254 PostgreSQL
cores out of 96 (48.18 percent over 5.02 seconds). The tenth complete autonomous
query cadence ends at 22:01:06 UTC with persistent grant queues. These separate
observation clocks do not establish an isolated deployment effect.

The Redis admission candidate has passed local synchronized, two-process tests
with 512 distinct clients sharing one network, including varied- and shared-
destination controls. All creations succeeded with no sampled balance/cache/
census lock waits; settlement, replay, crash, compatibility and release-gate
checks remain open. The candidate is not yet deployed or activated.

The operator explicitly requests completion of all ARINDB work through final
merge and deployment. Current-owner capture core `6b478365` is independently
reviewed, merged and pushed on Main as `4af854be`. All 22 race roots pass
against that exact Main source and published SDK. The full classifier/tooling
race run also passes 43 tests, with two optional external-input audits skipped.
Independent current-Main gate:
`61e851a1813cff8e6fc7cdc4d8c03c82af2a0ebcb25cfd07cebacbb558ca3d6d`.
The protected fleet
adapter, complete Main shadow, adequate verified subscriber coverage, resource
cutover and policy deployment remain required. The policy remains inactive;
local tests or an observer-only deployment cannot close that requirement.


### 2026-10-03 00:45 UTC schema 755 and service rollout

Migration 755 is applied to Main. The native CLI exited successfully; the
post-migration read verified successful schema head 755, all expected
compatibility artifacts, and exactly one admission-policy row with enabled
false. The controlled attempt completed at 00:38:21 UTC in 14.146 seconds,
using a 30-second native lock allowance and no statement timeout. The earlier
three-second lock failure remains recorded; its unmatched start was qualified
against a fresh read of head 754 and absent 755 objects before this attempt.
Migration receipt:
`7d097704ae23238ba2b0f658feb1b234766464e31dcb45eaad9ac8aa4c6f60e0`.
Independent post-migration closure:
`5a201ee3fad6a5bea036b7c29827dbf93b1f653b9096cf8a80e4313bb50e3b2e`.

All API, Connect and Taskworker block deployment commands completed with exit
zero for `2026.10.2-arin-final+1060864900`, compiled from `25897f4c`.
Taskworker completed at 00:40:01 UTC, API at 00:41:08 UTC, and Connect at
00:45:08 UTC. Both architecture manifests and image configurations were
verified before selection. This is completed deployment-command and selected
version evidence; direct process identity and incompatible predecessor
retirement are still being collected. Redis admission remains disabled until
that compatibility boundary is established. Edge5 remains excluded under the
operator's explicit offline guarantee.

The protected ARIN capture adapter is merged and pushed. Its independent
four-host, twenty-Connect-process transport test completed in 60.062 seconds
within the unchanged 90-second cohort lease. The separate eight-host
headroom failure is retained and does not qualify a larger topology.
Actual Main shadow coverage, affirmative subscriber catalog adequacy, and
classifier activation remain open. Default-off capture deployment does not
activate the new classifier or demonstrate Quality/Speed provider coverage.

The operator requests a follow-up that removes the admission-policy SELECT
and uses Redis admission unconditionally. Deploy the schema-755-compatible
unconditional code to all readers before dropping the enabled column in a
later migration; existing flag-reading binaries must not encounter the
removed column.


### 2026-10-03 02:01 UTC Redis admission and unconditional rollout

The four enabled hosts were directly checked before activation: all 48 active
API, Connect and Taskworker slots used compatible images; no incompatible live
or restartable financial predecessor was found. Independent four-host closure:
`639f2935e108aca638870bf6eacf7fa6bd4b441320dec962c9c6d36ca9753688`.
Redis admission was enabled by a guarded primary transaction at 01:16:08 UTC;
the fresh read verified schema 755, one policy row and enabled true. Activation
receipt: `e0eec26d4043ab24d799fb711cf62cf812f8c7aed1a73e128d2914cd1f5b9764`.

The unconditional admission bridge `75b40973` is merged and pushed. Focused
accounting, lifecycle and synchronized contention checks passed independently.
Both image architectures were verified, and all API, Connect and Taskworker
block deployment commands exited zero for
`2026.10.3-redis-always-on+1060875100`: Taskworker at 01:52:44 UTC, API at
01:52:50 UTC and Connect at 01:56:06 UTC. Fresh direct process proof and
retirement of every older flag-reading binary remain required before the
separate enabled-column removal migration. Schema head remains 755.

The fixed 01:17–01:22 UTC eight-Taskworker timing sample reports internal
controller returns averaging 20.68 milliseconds, with no measured payer-gate
or reservation-snapshot residence. This is internal control timing, not an
accepted URL-probe completion rate. Protocol rejections and five-second DNS
timeouts remain. Qualified rolling-quota coverage rose from 2,400 of 111,397
providers at 01:18:27 UTC to 10,291 of 111,400 at 01:48:51 UTC (9.24 percent).
The separate hourly forecast is still incomplete. PostgreSQL measured 31.47
percent CPU at 01:46:31 UTC; query sampling still found tuple-wait queues.
These observations have separate clocks and do not isolate a deployment effect
or close the 100-percent quota-coverage goal.

Hosted-proxy local controller wiring and API-request regression tests are in
progress in isolated worktrees. An initial real hosted-device test delivered
16 MiB with renewal, reconnect and payer reconciliation while making zero
requests to its rejecting API origin. Quality-probe traffic tests, discovery
and key-read coverage, negative controls, independent review, merge and rollout
remain open. The hosted-proxy fix is not yet deployed.


### 2026-10-03 02:52 UTC hosted local control and acceptance boundaries

Hosted-proxy local control and API-free regression tests are merged and pushed
as `9d7b930b`, with exact minimal published Connect `b8bd3c994855` and SDK
`95ccd57da971` dependencies. The independent normal-module race gates passed:
local authorization 12.146 seconds and real hosted proxy 25.422 seconds;
focused vet passed. The real quality-probe DNS/TLS path passed its race run in
5.805 seconds. Successful traffic, renewal, reconnect, durable payer settlement
and derived-client retirement completed with zero requests to the rejecting
control API. Actual missing-hook controls reached that API, establishing a
non-vacuous regression boundary. Independent source/control receipt:
`c068f583d34b46cb354e8d6b3e097fdfe5cbb2d5fcfa5c07fb893b23c1f48cb5`.

Both architectures of API, Connect, Taskworker and Proxy images were built,
pushed and verified from this Main commit as
`2026.10.3-hosted-local-control+1060875200`. All block deployment commands exited
zero: Proxy at 02:42:48 UTC, Taskworker at 02:44:38 UTC, API at 02:44:48 UTC and
Connect at 02:48:48 UTC. Raw eight-image-configuration closure:
`ebff1ac5d80e3691758b75013c7a9e05c738c6a42aa752a20a6e85bbee99e92b`.
This is completed selection/rollout-command evidence, not proof of every live
process or zero HTTP API requests in Main. The current four-service runtime
scope includes the 48 financial service slots on four enabled edge hosts plus
20 Proxy slots on Fireside and Crisp. Edge5 remains excluded. Fresh 68-slot
source/image/process proof and predecessor retirement remain open. Migration
755 is still the schema head; the enabled column has not been removed.
The running native monitor's schema catalog also expects that column and must
be promoted compatibly before its removal.

The source-qualified historical 01:17–01:22 UTC scheduler sample reports
110.158 accepted ProbeOne collection outcomes per second, 110.169 attempted
turns per second and 0.01064 local-failure turns per second across all eight
Taskworkers. These are collection outcomes, not verified unique durable quota
or internal controller calls. Receipt:
`326029f2a0161da315e14f5922049c4c86547048f3b47483327dd8e5b8eff075`.
The 02:50:01 UTC automatic census reports 46,700 of 112,988 eligible providers
meeting the configured rolling quota (41.33 percent); the separate hourly
forecast is incomplete. Provider-contact classification is now under causal
review because a writer attempt may be marked before successful local buffer
admission. A faster or larger reported count cannot close acceptance if it
includes unmeasured local failures. No classification fix is yet deployed.

PostgreSQL CPU measured 38.35 percent at 02:47:01 UTC. The independent recurring
query sample ending 02:47:23 UTC still contains settlement and escrow tuple
queues with maximum observed ages about 16 seconds. These capped samples do
not identify a payer, service or every root blocker; accounting and grant
contention remain unresolved.

All four enabled edge-host mounted-resource observations now match the
recovered selected ARIN, GeoLite and places pair and preserved configuration.
The baseline/candidate Config staging tree retains all 61 baseline files and
adds only the separate policy-two resource. Four-host selected-resource join:
`aaa9f3d38605d1eda16a48c9245259bf90e75f384f1aa432732a4bd45b7eba8c`.
This is file custody and mounted selection, not loaded-reader, full Main shadow,
verified subscriber coverage or classifier activation. ARIN policy two remains
inactive until those requirements are met.


### 2026-10-03 03:12 UTC local admission and live rollout verification

An actual local-route refusal regression reproduced a provider-contact
classification defect: registration and durable contract creation succeeded,
but the local route rejected the write; no control API or DNS endpoint was
contacted, yet the URL result counted as a measured DNS failure. The candidate
changes that result to an unmeasured local-transport-admission failure, while
the healthy real provider DNS/TLS/accounting control still succeeds without
API use. The paired race run passed in 14.196 seconds. Further owner and
precedence controls, independent review and production rollout remain open.
No ping or transfer acknowledgement was restored. Until this correction is
verified in Main, collection throughput and rolling coverage are provenance
of the deployed classification, not assured provider-contact measurements.

The automatic 03:06:03 UTC census recorded 64,362 of 112,989 eligible providers
at the rolling quota (56.96 percent), with 64,346 secure and quota-complete.
The accounting defect above prevents using these numbers to close coverage.
The latest PostgreSQL CPU sample is 28.97 percent. The recurring query sample
ending 03:02:52 still has escrow and settlement tuple queues; maximum observed
ages were 7.464 and 1.619 seconds respectively. Lower CPU does not establish
that lock contention or Main performance is resolved.

The fresh 68-slot runtime collection completed, but its strict reducer
qualified 47 slots: all 20 API slots, all eight Taskworker slots and 19 Connect
slots. One Connect slot has unavailable identity evidence. All 20 Proxy slots
have fresh source, version, process start and ready state; their producer
reports the OCI index digest while this reducer expects architecture-specific
configuration digests. That mismatch requires a reviewed identity-semantics
correction, not a claim that those Proxy processes are old or fully verified.
Runtime receipt: `d311fe455bc8612a1f5cc0bd930e287064271e96dfca033ac44feb72e15f3d19`.

Finite contract-rejection diagnostics are merged and pushed as `ce8e7aec`.
Independent six-root controller race tests and vet passed; the fixed cause
labels distinguish early mode/secret refusals from credit and lifecycle
failures without changing protocol or accounting. These diagnostics are not
yet deployed. The remaining 384-root Model race partition is running on a new
independent local fixture; it has no terminal result yet.

The policy-two capture Config image build exited zero at 03:06:13 UTC.
Selection, credential installation, full Main shadow comparison, subscriber
coverage validation and classifier activation remain incomplete. Policy two
remains inactive. Monitor ledger T0504 was appended with its prior-tail guard;
continuous URL and PostgreSQL monitoring retains the 15-minute cadence.


### 2026-10-03 03:54 UTC measurement correction and remaining contention work

The local-write classification fix is merged and pushed as `49450197`, using
published Connect `638e55103517`. Independent normal-module verification
passed the actual rejected-route and healthy traffic/accounting roots under
race (52.604 seconds), full egress-health race (8.850 seconds), focused vet,
and the 28 Connect producer/recovery roots. Review:
`c48c64b7ac999673f784d414d75845a4b2fc73b1e389d69fb71d15774b4b0478`.
The rejected local writer created its real contract but reached neither API
nor DNS, returned an unmeasured result and made zero health publications.
The healthy provider control still completed authenticated DNS/TLS and payer
accounting. Contract-attempt semantics and the removed ACK remain unchanged.

API, Connect, Taskworker and Proxy version
`2026.10.3-local-write-admission+1060875400` completed both-architecture builds
and artifact verification. All-block deployment commands are running; source
convergence and retirement are not yet proven. Do not treat existing four-hour
quota or eight-hour ratio history as validated by this rollout. Establish the
last incompatible probe-writer retirement time, validate fresh outcomes after
that boundary, and preserve security evidence while older history ages out.
The automatic 03:36:26 census reported 97,604 of 114,521 eligible providers at
quota (85.23 percent), but remains subject to that classification qualifier.

The query sample ending 03:33:45 still contains escrow and settlement tuple
queues, with observed maxima about 14.9 and 11.7 seconds. Retained one-hop
financial blocker edges primarily lead to other waiting transactions and do
not identify terminal holders. A bounded chain reader has passed a real
three-transaction PostgreSQL control and awaits independent gate and actual
Main use. The settlement balance-lock query did not reproduce a bad plan in
an actual 1.35-million-row local control: generic/custom plans used primary-key
nested loops and about nine buffers. No query rewrite is justified by that
planning hypothesis alone. Exact holder SQL, lock lifetime and source caller
mapping remain open, together with deterministic hot-area contention tests.

The remaining 384-root Model race partition is terminal with eight failures.
Seven financial/clock failures need current Redis-path accounting, TTL,
reconciliation, cancellation and ordering controls while retaining meaningful
legacy coverage. The eighth is the full-population ARIN transport deadline.
They remain failures until repaired and verified; no full Model closure is
claimed. Astra owns both repair tracks.

The Config Makefile mode-preservation correction is independently verified and
pushed in Warp `9a75631`. Rebuilt capture version
`2026.10.3-arin-capture+1060875301` preserves all 61 baseline files and modes,
adds only the two candidate resource files, and passed both-architecture image
custody and offline CLI verification. The original unselected 5300 build is
retained as failed mode-custody evidence. Version 5301 remains unselected;
full Main shadow, protected credential installation, subscriber coverage,
catalog expansion and policy-two activation are still open. A candidate local
four-host capture also failed the unchanged 90-second bound, so capacity must
be fixed rather than extending its lease or claiming completion.


### 2026-10-03 04:42 UTC async settlement regression and qualified rollout

API, Connect, Taskworker and Proxy version
`2026.10.3-local-write-admission+1060875400` completed all four all-block
selection commands with exit zero. The fresh runtime collection qualified
64 of 68 enabled slots: 20 API, eight Taskworker, 19 Connect and 17 Proxy.
One Connect slot positively still reported the preceding 5200 release; three
Proxy slots had unavailable identity evidence. Command success does not prove
all predecessor retirement or all-slot runtime convergence. Edge5 remains
operator-offline and excluded; IPv6 provider availability remains outside the
current acceptance target.

The automatic census at 04:22:01 reported 111,270 of 113,902 eligible providers
at quota (97.69 percent). Historical local-writer refusals previously counted
as measured provider failures, so this remains a reported counter rather than
validated coverage. Establish the last incompatible probe-writer retirement,
verify new measured outcomes, and let incompatible four-hour quota/eight-hour
ratio history age out. Hourly owner coverage was incomplete for shards two,
four and six. Do not infer complete rate or corrected measurement truth.

The complete recurring query sample ending 04:35:25 still reported 88 lock
waiters. Escrow tuple waits reached 28.157 seconds with peak occupancy 60;
settlement balance tuple waits reached 2.378 seconds with peak occupancy 48.
The latest sampled PostgreSQL CPU was 41.40 percent of 96 CPUs. Query-family
occupancy is not CPU attribution and the capped blocker output does not prove
all terminal holders. The earlier independently qualified chain read traced
four oldest selected seeds to an idle transaction whose last statement was
`contractParticipantsWithUsageOriginInTx`; that statement is not itself the
lock-acquiring statement and does not uniquely identify the caller.

History verifies a settlement regression in
`352746737bb1deb28198f4162985b434ebc98461`: payer balance debits moved from a
separate post-commit transaction into `settleEscrowInTx`, together with shared
balance row locks held through the settlement transaction. An older aggregate
Redis-to-database flush has not been established by the inspected history.
Redis admission alone does not remove these settlement writes. Astra is
implementing asynchronous writeback with deterministic current-public-path
held-row and concurrent-settlement controls, plus interrupted-post, replay,
reconciliation and shard-cleanup coverage. The proposed journal migration must
follow already present migrations 756 and 757. No async correction or new
migration is deployed yet; legacy compatibility contention remains explicit.

The seven financial/clock Model failures have independent focused race,
mandatory same-network large-N and vet evidence and are merged in `87e7e8a6`.
The ARIN transport failure has a host-pair scheduler correction merged as
`0a2fc4e`; independent full-population capture completed in 39.225 seconds
normally and 73.715 seconds under race, within the unchanged 90-second lease.
Neither focused repair establishes a current complete Model-suite pass.
Concurrent Main payout changes and source migrations 756/757 were preserved
in pushed Main `c5b02bbb`; those changes are not attested as deployed or migrated
by this checkpoint. Current 5400 services use source `49450197`.

Verified candidate-only Config version
`2026.10.3-arin-capture+1060875301` completed all-block selection at
04:38:25 UTC with exit zero. Its 63 files preserve all 61 baseline file bytes
and modes and add only two separately located candidate resource files.
Current and draining service namespaces still need actual candidate-resource
verification before protected capture credentials are installed. Policy two
remains inactive. The protected installer and transport scheduler have local
independent gates, but the gateway coordinator, full Main shadow comparison,
verified native subscriber coverage, catalog expansion and rebuilt allocation
classification remain unfinished.

Fresh Connect resource alerts still report very high memory/goroutine burdens,
including approximately 98.24 GB RSS and 703,507 goroutines in one observed
instance. The resource alert does not identify the allocation callsite or the
running build, so no leak mechanism or retirement is claimed. Safe private
profiling instrumentation and root-cause repair remain open. The authoritative
watcher is continuously active with its 15-minute cadence. Root appended
ledger T0506 under its 505-record prior-tail guard; the resulting canonical
tail is `fcd8c5a48ae24755a0be5de29bf8685075b04280f49fa2b6bcae3ad9f7666c7d`.
FP2FIX is not complete and Main performance is not resolved.


### 2026-10-03 05:12 UTC settlement test and migration compatibility boundary

The author's actual public settlement contention control now has a red/green
pair: 64 independent clients/contracts sharing a grant all timed out on the
baseline while three shared financial rows were independently held; all 64
completed on the candidate. The candidate package took 3.502 seconds including
fixture setup. This is an author result on mutable source, not an independent
release gate or a Main rollout. Partitioned asynchronous debit writeback,
interrupted-post recovery, replay, fairness and shard cleanup remain under test.

The standard migration sequence for proposed journal migration 758 also runs
source migrations 756 and 757. Migration 756 changes account_payment and adds
payment guards that may reject older unproven payout edits; migration 757 adds
an immutable earning boundary. Their deployment compatibility and the current
Connect08d module graph require explicit qualification; earlier Connect638
financial gates do not establish it. No new migration has been applied here.

The fresh 04:43 runtime witness qualified 39 of 68 slots, with no positively
identified incompatible current process. Most failures collapse missing,
stale, unjoined or ambiguous evidence into strict nulls; one Proxy process had
qualified release identity but was not ready. All 20 Connect slots qualified,
but no complete Taskworker reference set or predecessor-retirement proof was
available. The reduced qualification count alone is not a downgrade diagnosis.
Direct current and draining namespace inventory remains necessary.

The 04:52:24 automatic census reported 109,606 of 111,471 eligible providers at
quota (98.33 percent), still subject to historical measurement truth and hourly
owner coverage limits. The 05:03:17 PostgreSQL CPU sample was 25.60 percent;
preceding query samples still contain escrow and settlement tuple queues.
Root appended T0507 under the 506-record guard, producing canonical tail
`4463ed11146fd3a3226333515c0deb3f47132a69dc7e933eb891e84908b02c77`.
The standing watcher and both independent fixture endpoints remain active;
no restart, reset, classifier activation or completion is claimed.


### 2026-10-03 06:24 UTC asynchronous accounting migration and rollout

The asynchronous accounting correction is committed as `c56f9557`, merged and
pushed in Main `0015fbdc`. Current Redis-admitted settlement writes exact debt
journal entries without updating or locking the shared transfer balance in the
settlement transaction. Sixteen bounded background partitions apply the debit
and release Redis debt after commit; pending debt fences grant and shard cleanup.
Legacy unmarked contracts retain their compatibility path and remain a separate
contention boundary.

Independent focused race gates passed for all twelve asynchronous debit roots,
three local-controller authority roots and five migration/monitor roots. They
cover replay, rollback, interrupted Redis posting and acknowledgement, cleanup
fencing, fairness and distinct-payer capacity. The author's public 512-client,
two-process held-row settlement and sixteen-partition flush controls also passed.
These focused results do not establish a complete current Model-suite pass.

Main's native migration command completed once with exit zero at
06:17:52 UTC, advancing schema 755 to 758 through canonical migrations 756,
757 and 758. Read-only postflight verified exact migration identities, table,
index and guard definitions, the enabled Redis admission policy, and an empty
provider-payout boundary. The owned primary tunnel closed. The earlier claim
that the primary-local backup completed within 24 hours was incorrect: it
confused metadata observation time with producer completion. The retained dump
generation began on October 1 at 00:00:15 UTC; its producer completed at
19:28:43 UTC that day. The October 2 23:24:08 metadata observation refreshed
neither time. Stable local metadata does not attest ciphertext integrity,
destination-copy completion, decryption or restore. The Main migration receipt
is `/home/by/urnetwork/temp/main758-native-primary-20261003-root-activation-v1/run-20261003T061740.770997Z/receipt.json`,
SHA-256 `29370a6f4f03bbf1a8a72eff66685811206955d310b77ec1a5f51cda1f4bd824`.

Both architectures of API, Connect, Taskworker and Proxy were built, pushed and
verified from c56 with published Connect `08d48400`, SDK `95ccd57d` and SCTP
`6443417d`. All four all-block selection commands completed with exit zero for
`2026.10.3-async-transfer-debit+1060875500`: Proxy at 06:18:31, Taskworker at
06:20:21, API at 06:21:02 and Connect at 06:23:56 UTC. Selection success is not
proof that all current and draining processes converged. Fresh 68-slot runtime
identity and post-rollout accounting/probe performance observations remain due.

The pre-rollout automatic 06:07:32 UTC query sample still showed settlement
balance locks peaking at 66 sampled backends with a 21.643-second maximum query,
and escrow access peaking at 56 with a 13.794-second maximum. Occurrences are
not distinct clients or CPU attribution; observation coverage remains partial.
The older authoritative watcher continues at the 15-minute cadence while its
schema-758 successor is prepared for a controlled handoff.

A real H1/exchange/full-resident churn control and signal guidance are committed
as `f2420ef6`. All 24 residents released indexed sequences and transport routes
after joined shutdown, including under race. The specific active control path
retains at least 480 KiB of named channel slots per resident; this is a population
cost lower bound, not a Main heap attribution or proof that no leak exists.
Native PID/start/build/RSS and population attribution remains unfinished.

ARIN policy two remains inactive. Exact 5500 namespace-admission successors
have independent local gates; final Main resource custody, protected capture,
full shadow comparison, rebuilt allocation classification and subscriber
coverage still require production evidence. FP2FIX is not complete.

### 2026-10-03 07:43 UTC admission unwind and all-block selection

The admission-error fix `bf7cadcd` is merged into Main and pushed through
`7d93570f`, preserving concurrent Main changes. A real PostgreSQL regression
control reproduced cancellation during the shard-admission query being treated
as a policy refusal, followed by a misleading commit-on-closed-connection panic.
Operational query, scan and iteration errors now unwind the transaction; retired
shard policy refusals retain their ordinary return behavior. Independent focused
tests passed. Main's initiating cancellation and exact failing process generation
are not yet proved; the patch does not blindly replay ambiguous commits.

API, Connect, Taskworker and Proxy were built and verified for both architectures
from `bf7cadcd`, retaining the published libraries and deployed c56 accounting
implementation. All four all-block selection commands completed with exit zero
for `2026.10.3-client-admission-unwind+1060875600`: Proxy at 07:38:12,
Taskworker at 07:40:00, API at 07:40:07 and Connect at 07:42:28 UTC. This release
requires no migration beyond Main's schema 758. Its raw OCI configuration closure
has SHA-256 `6b52d5cc3c726bca959c7eb7751ada0ed85fa916a876deb17ba5a06e8c64fac8`.
Selection-command success does not prove current and draining process convergence
or an improved probe rate; fresh runtime and post-rollout measurements remain due.

The independent 07:44 UTC HTTPS runtime sample qualified 22 of 68 expected
current slots: API 10/20, Connect 6/20, Taskworker 4/8 and Proxy 2/20. Twenty
witnesses matched architecture configuration digests and two matched the verified
release index. Old, missing or ambiguous witnesses remain unqualified. These are
self-reported source/image/start metrics, not native executable proof or evidence
that predecessor processes retired. Fleet convergence is incomplete in this
sample. Receipt SHA-256:
`c65e2ebfe2dfc3e1fe3ba14f14fac34e7b74eb7297772e79789ba2e16bc9146b`.

The schema-758 monitor successor replaced the older watcher at 06:34 UTC without
overlapping watcher parents. The handoff had a brief collection gap and reset
sustain windows; it was not gap-free. The successor continues the 15-minute probe
cadence. Its 07:20 database sample had truncated coverage. A separate read-only
07:32 financial-holder snapshot found no candidate waiters during 38 milliseconds,
which is an instantaneous quiet control, not proof that earlier lock queues cleared.
Local 64-client controls reproduce contention in the legacy unmarked settlement
path while the marked/current control passes. The legacy correction remains open.

Private baseline ARIN capture configurations are installed on edge0, edge1,
edge3 and edge4, without a separate service restart or classifier activation.
The rebuilt parent-allocation candidate passed full native decoder readback;
expanded subscriber classification and Main shadow coverage remain unfinished.
A capped Main log sample found 64 Taskworker ForceCloseOpenContractIds panics
classified as insufficient escrow; generation, close outcome and unsampled
generator failures remain unknown. FP2FIX is not complete.

### 2026-10-03 08:14 UTC coverage, native reader and legacy contention

The delayed 07:50 UTC runtime sample qualified 46/68 serving slots for release
5600: API 20/20, Taskworker 8/8, Connect 10/20 and Proxy 8/20. Native edge0
inspection separately verified five bf7 Connect processes and one older c56
draining predecessor across five blocks. Those six processes used about 25.06
GiB RSS, including 13.55 GiB for the predecessor. Different time namespaces
prevent a qualified native-start/metrics join; neither this host nor self-reported
metrics prove full-fleet predecessor retirement.

The 07:42 URL census reported 112,020 eligible providers, 109,761 quota-complete
and 109,743 security-clear complete: approximately 98% rolling ten-measured-run
coverage, with 7,472 runs needed. Four shard-owner/hourly ranges were unobserved,
so aggregate throughput remains unqualified. The 08:08 exact-release Taskworker
metric read found eight source-qualified bucket publishers: per-process native
Quality 28,370–28,399, Speed 29,943–29,974 and Online 111,999. These overlapping
global buckets must not be summed across processes. Scrape freshness does not
prove the isolated underlying database refresh succeeded recently.

All twenty exact-release API effective native-reader flags were false. The
disabled flag selects compatibility Redis union loading, rather than bounded
native Redis pages; it does not mean discovery bypasses Redis altogether.
Selected candidates still undergo database-backed hard exclusions. Native page
publication and activation are being validated independently of ARIN policy two.

The 07:42 PostgreSQL CPU sample was 26.161 cores of 96 logical CPUs, or 27.25%,
above the monitor's 25% warning band. Financial contention persisted at the
07:51 and 08:07 cadences, with final selected waiter counts 89 and 90. The
07:57 bounded holder graph positively matched unmarked legacy settlement's
shared transfer_balance lock and metadata's NOT redis_reserved branch. It
selected four seeds from 89 candidates and does not attribute the entire graph
or CPU to that path. A private deferred-legacy-settlement correction passed its
first 64-client held-row control; durability, rollback, mixed-writer and larger
controls remain. Its migration must follow canonical Main migrations 759–761,
as 762, rather than collide with already-merged migrations.

The protected ARIN fleet attempt reached Main but failed in host inventory,
before retained native inventory or shadow output. Cleanup completed without
unreleased hosts. Its exact transport/native-stage cause remains unproved;
ARIN policy two remains inactive. Provider IPv6 findings are excluded from goal
acceptance under the user's current rollout instruction. FP2FIX is not complete.


### 2026-10-03 15:10 UTC canonical migration and backup evidence correction

Release source `114b708c` preserves Main's exact published prefix through Circle
migration 762 and appends legacy settlement intents at 763. Root's native
migration command completed once with exit zero, advancing Main from 758 to
763. Read-only preflight and postflight verified the primary, all migration
identities, exact table/index/function/trigger guards, preserved 756–758
artifacts, enabled Redis admission policy and empty new tables. The owned
primary tunnel closed. Native lock timeout remained three seconds, statement
timeout remained zero and the CLI retained its 60-second outer bound. Receipt:
`/home/by/urnetwork/temp/main763-native-primary-20261003-root-1506-48h-v2/run-20261003T150747.226470Z/receipt.json`,
SHA-256 `1549df300b712819766055423cd52938f4eba1867fc1471a745850c3fe6c10bb`.

The 24-hour producer-age prerequisite was an agent-created migration wrapper
policy, not a user or `monitor/RUN-MAIN.md` requirement. Root explicitly accepted
a separate, reviewed limit of 48 hours since producer completion solely for
exact additive canonical migrations 759–763; the strict predecessor was
preserved. The October 3 14:32:32 metadata observation still found the successful
October 1 producer: approximately 43 hours since completion but approximately
63 hours since dump-generation start when migration ran. This is not a 48-hour
recovery-point or data-loss bound. No current ciphertext rehash, decrypt,
restore or destination-copy proof was obtained. Native metadata receipt
SHA-256: `f7c44dd03933efc60e36906453d3e17982fbce3077cde63716b5bc540e2f9911`.

Root scheduled a new backup start after migration because a running `pg_dump`
retains shared table locks that can block migration 761's table changes. The
bounded start action then refused during its free-space preflight:
`start_command_issued=false`, `production_mutation=false`. No backup started
and no retry occurred. Its proposed free-space floor was twice the retained
599,562,125,770-byte ciphertext plus 1 GiB, allowing the existing writer's
compressed plaintext and ciphertext staging to coexist. The receipt establishes
only that available space was below 1,200,197,993,364 bytes; it does not retain
actual available bytes or prove the next dump's size. A bounded filesystem,
staging-file and retained-generation metadata read remains due before an owning
capacity correction. Refusal receipt SHA-256:
`a3781be79d606eae14c06ce0588cb550be938de4edb7c21a64d21ba69c764187`.

R57 API, Connect, Taskworker and Proxy all-block selection was in progress at
this checkpoint. Schema success and local controls do not prove fleet
convergence, legacy-worker progress, drained settlement debt or restored service
health. The 15-minute monitor remains authoritative; FP2FIX is not complete.


### 2026-10-03 18:02 UTC exchange retention and close-backlog correction

Root built and verified all four R58 service images for both architectures and
completed all-block API, Connect, Taskworker and Proxy selection with exit zero.
The exact source is `a58dd054` on R57's `114b708c`, retaining schema 763 and the
qualified Connect/SDK dependencies. This release clears completed exchange
batch backing references. Its local native writer control reproduces retention
and collection; it does not attribute the large Main heap to that defect.
Current Main financial admission/recovery changes are not implicitly part of
R58. The corrected source/build/image/start reader qualified 57 of 68 slots:
API 20, Taskworker 8, Connect 13 and Proxy 16. Four Connect slots still reported
R57, and three Connect plus four Proxy slots were ambiguous. This is
self-reported release evidence, not direct executable proof or predecessor
retirement. The first read used the deployment tag in place of the build
version; the corrected reader binds the exact build-plan version and keeps
true revision, image and version mismatches unqualified.

The 17:18 bounded primary snapshot found all 16 legacy partitions at their
65-row pending and due sentinels, at least 1,040 each. Accounting-failure counts
were exact at 159; operational failures were zero. The oldest 65 open contracts
were all owned by legacy intents. This establishes head ownership for that
sample, not the entire 25,000-row selector or the disputed population. The
fixed 17:16–17:20 worker log read selected 45 owning `flushLegacySettlement`
errors, all `insufficient_escrow`, below its 64-record cap. These logs do not join
every accounting row or prove exact native process generation. Reservations
and accounting rejections remain intact; an accounting invariant discriminator
is still required.

Two local controls reproduced additional correctness defects: the closer can
reselect an intent-owned head without reaching later eligible contracts, and
resident teardown can discard a queued control after the sender receives a
successful native transfer ACK. The ACK fix `c371033c` drains accepted controls
after fencing and joining their producers; seven focused race controls pass.
Root merged it to Main as `393c76bf`; it is not in R58. R59 source `610a5c75`
combines that fix with financial candidate `8d983696`. All four image builds,
both-architecture verification and all-block deployment commands completed
with exit zero; the last was Connect at 17:52:29 UTC. Runtime convergence and
backlog improvement remain unproven. A focused financial test initially
expected synchronous debit behavior; its correction is being checked separately
without changing the published R59 bytes.

Bounded closer continuation `8f92f41e` now has passing focused model and
scheduler race controls, including tied timestamps and a fixed pass boundary.
Each independent open/disputed scan limits raw candidates to 25,000, advances
past existing intents and persists its cursor in the ordinary task. Local
32,768-intent head controls use indexed pages. The existing 92 workers, row
proofs and financial rejections remain unchanged. Root merged the selector
as `2cee420c`; the thin R60 source `54f09828` is prepared on R59 and remains
unpublished at this checkpoint.

The 15-minute monitor and the FP2FIX goal remain active. High contract backlog,
accounting failures, large resident heaps, native-reader config delivery and
ARIN full-fleet shadow/coverage remain unresolved. ARIN policy two stays inactive;
the offline expanded artifact does not establish sufficient Main provider supply.


### 2026-10-03 19:14 UTC rolling coverage and deployment checkpoint

Root merged mature URL recovery pacing `675fa67b` to Main as `243e7439`, then
pulled and pushed successfully. The native regression reproduced a mature
provider still below ten accepted measurements being deferred another
20m26.52s after a successful measurement. Seven focused race roots pass with
the correction: this case uses the existing one-minute retry pace, while
warmup, measured failure, quota-full expiry, security and replay controls retain
their contracts. No dependencies or migrations changed. R61 source `675fa67b`
combines this correction with the bounded expiry selector already prepared in
R60. Registry publication remains held pending replacement-credential disposition;
local Makefile binaries do not establish published images or deployed fixes.
Neither selector nor pacing is deployed at this checkpoint.

R59's four all-block deployment commands completed, but the 18:04 source-bound
runtime read qualified only 54 of 68 slots: API20, Taskworker8, Connect11 and
Proxy15. Sequential native Connect observations later proved current executable
identities on sampled owners while also finding older/current overlap on hot
hosts. They do not prove simultaneous fleet convergence, drain phase or retired
predecessors. Large RSS in directly verified current R59 processes remains an
open ownership problem; the exchange-tail and accepted-control fixes cannot be
claimed to explain or resolve the whole retained heap.

The fixed 18:30–18:35 UTC selected-policy history window contained 23,682 unique
accepted measured runs (16,587 success, 7,095 failure; 22,848 providers), or
78.94/s. This is measurement-time history, not arrival rate or fixed-provider
recovery. The coherent 18:54 census had 92,253/107,139 quota-complete providers
(86.11%). At 19:09:21 it reported 96,316/107,142 (89.90%), 10,847 overdue,
4,203 due and 16,598 runs needed, with 142,355.3 seconds oldest due. Observation
age was 55.04 seconds and sample age 14.30 seconds. The improvement preceded
R61 deployment and receives no causal credit; provider-cohort turnover remains
unmeasured. At least 6,644 overdue providers were outside the due set in that
single later snapshot, a lower bound on future-paced work.

The 18:49 oldest-eligible-hint head contained 128 mature deficient providers,
all currently eligible under both reliability modes and all latest claims
completed without recorded setup failure. Their 1,119 retained measurements
included 332 accepted failures and left a 161-run deficit. This bounded head
cannot explain all overdue providers. The next oldest-all-cycle head consisted
entirely of inactive historical cycles, so it did not establish live false
hints. A native control independently reproduces restored reliability remaining
unscheduled until its hint refresh; Main prevalence is still unknown.

Native ACK-loss replay and additive checkpoint accounting are now reproducible
locally. An idempotent checkpoint repair and proposed migration764 remain under
qualification, with backup/schema prerequisites and mixed-client compatibility
still explicit. Existing accounting reservations and insufficient-escrow
rejections remain intact. The authoritative 15-minute watcher and the FP2FIX
goal continue; memory ownership, quota coverage, native-reader delivery and ARIN
full-fleet shadow/positive supply remain open. ARIN policy two stays inactive.

### 2026-10-03 20:32 UTC checkpoint identity and owner-observation checkpoint

Root merged and pushed the optional close-report identity backend to Main as
`afca7d83`; monitor764 contract support followed in `d486bdc6`. Combined release
source `107c79c7` includes the bounded expiry selector, mature URL pacing and
backend764. All sixteen local Makefile binary builds completed successfully
(receipt `a7d44032`), with no published OCI images or deployment. Registry
publication remains held pending replacement-credential disposition. Migration764
has not been applied and report-ID emission remains disabled. The streaming
backup producer was started at 20:08 UTC after the qualified writer install;
completion and a new pre-DDL backup have not yet been established. No migration
or deployment is inferred from source merges, local binaries or backup start.

The 19:54:56 standing URL census became unobservable with no qualified shard-zero
owner. A bounded Root HTTPS read later distinguished two explicit shard-zero
heartbeat zeros from six missing series in a new evaluation at that historical
time. At 20:24:59 all eight shards again had exactly one owner. All eight
self-reported process hashes and starts were unchanged between those frames;
edge0/g2's shard-zero returned-ok passes advanced from 16 to 19 and its error
count stayed one. This supports a local task-owner return gap, without proving
duration, return reason, continuous ownership, native process identity or probe
throughput. Separate SSH255 observations do not establish that gap's cause.
Receipt `39d3d950` and privacy summary `a86d07b3` preserve those limits.

The coherent 20:25 observation reported 69,249/85,087 quota-complete providers
(81.39%), 12,588 warming and 204,073 seconds oldest due. The preceding coherent
20:10 census had 84,323 eligible and 66,412 quota-complete (78.76%), after the
19:39 population of 106,655 eligible. These are changing eligibility cohorts,
not fixed-provider recovery or proof of a cycle reset. Neither the selector nor
the mature pacing correction is deployed, so neither receives causal credit.
The separate 20:20 database CPU observation was 49.47%; the 20:22 activity sample
omitted 179 groups and truncated 129 query texts, so it cannot clear database
contention or establish an owning query cause.

R59 all-block selection is complete but full simultaneous native convergence,
predecessor retirement and current resident memory ownership remain unresolved.
Old accounting failures retain their reservations and insufficient-escrow
rejections. Stable per-logical-close report-ID emission is follow-up source work
and must wait for migration764 and backend-generation coverage before enabling.
ARIN capture leases expired at 18:06:48; renewed files require ordinary service
startup to create endpoints, and older live owners remain part of full-fleet
coverage. Policy two stays inactive: the 493 offline verified Google Fiber and
Webpass leaves do not prove enough Main residential/business supply. Native
reader configuration delivery, full ARIN shadow, quota coverage, contract
backlog and memory ownership remain open under the active FP2FIX goal and the
authoritative standing monitor.

### 2026-10-03 21:28 UTC cohort-origin selection boundary

The private-prober native PostgreSQL control `83a4bf25` passes under the race
detector, including independent review. Normal private shard roots have no
Public key, and derived children remain excluded even when given one. A second
reliability refresh preserves an existing provider's first-cycle timestamp.
An explicitly Public top-level private root can enter, so network ownership
alone is not an exclusion. This control does not identify Main cohort members
or establish the cause of the earlier warming increase.

Root's bounded origin read completed at 21:28 UTC (`57088ccd`, privacy reduction
`b3eb0a52`). Both 64-client Public-key ID heads were full, but all 128 selected
rows were currently ineligible: 107 failed base admission and 21 failed normal
reliability. All were top-level identities with no retained private-shard owner.
The sample held 105 missing cycles, 23 existing deficient cycles and 80 accepted
measurements. Fourteen older clients had first-cycle timestamps in the selected
change window. None of these rows joined the current eligible warming
population, so the read does not explain its increase or prove a reset. The
next discriminator must select currently eligible providers with bounded raw
candidate work; no fleet prevalence follows from either sample.

Root published the thin close-report emitter `fd4388de` and Main Connect
counterpart `b83e4fe9` as source. Neither is selected by the Server dependency or
enabled at runtime. Hosted emission cannot deduplicate checkpoints from old
ID-less provider clients. Root also merged and published the Connect ownership
ledger source; its dependency selection and runtime enablement remain separate.
ARIN renewal and full-capture sources are qualified against the new monitor764
authority. No fresh operator generation, renewed Main capture lease, completed
full Main shadow or sufficient verified subscriber supply is established here.
Policy two remains inactive and the FP2FIX goal continues.

### 2026-10-03 21:54 UTC eligible warming and settlement error boundaries

Root's follow-up Main read (`cfaaa72b`, reviewed reduction `528fa0d3`) reached
64 currently eligible deficient warming providers after capped 1,024-cycle,
128-recent-candidate and 64-eligible stages. All were top-level clients without
a retained private-shard owner. Sixty-three had both stored client and
first-cycle creation during 19:30–20:15, and one had both afterward. Five cycle
delays were under five minutes and 59 were five minutes to four hours. The
sample contained 401 accepted measurements and a 239-run deficit. This moves
the sampled mechanism toward client creation and identity replacement, not an
old client row with only a reset cycle. It does not establish fleet prevalence,
historical eligibility or the cause of the global warming increase.

The separate coherent 21:47:54 standing census reported 73,246/85,922 quota
complete (85.25%), 12,110 warming, 588 overdue, 90 due and 45,389 runs needed.
These remain changing-cohort observations; selector/pacing/backend764 successor
images are not deployed and receive no causal credit. The 21:47 database
activity sample omitted 332 groups and truncated 246 query texts, so it gives
no holder or CPU clearance.

The 21:40 Taskworker normalized panic sample names `flushLegacySettlement`.
Its qualified source catches raised transaction errors with `HandleError` and
returns the error for durable failed/retry accounting. This is not evidence of
an uncaught worker death; financial refusals and retained intent ages remain
failures. A fresh fixed-window owning-cause reader is prepared separately, with
no native generation join or completeness claim for dropped Loki records.
Unclassified DNS health and window-stall observations remain separate.

ARIN renewal and full-capture sources are independently qualified against the
current monitor764 authority. Root-local native baseline operator preparation
is ready for the next ordinary all-block rollout; no new lease or forced
ARIN-only restart is claimed. Policy two remains inactive until full Main
capture closure, provider losses and sufficient verified residential/business
supply are established. The 493 offline verified leaves are not provider supply.
Registry publication still awaits credential disposition. FP2FIX remains active.


### 2026-10-03 23:19 UTC coverage, query provenance and rollout checkpoint

The coherent 23:03:55 URL census reported 49,521/72,810 quota complete (68.01%),
23,310 overdue and 38,977 runs needed. At 23:19:07 it reported 47,000/70,858
(66.33%), 23,879 overdue, 10,873 due, zero warming, two uninitialized and
45,291 runs needed. Eligibility changed between observations; neither equal
nor similar population counts prove a fixed-provider comparison. The sharp
warming/eligibility transition and continued rolling-quota deterioration remain
unexplained. The prepared one-minute pacing for mature deficient providers and
bounded close selector are not deployed and receive no causal credit.

The fixed 23:00–23:05 accepted-measurement window (`23c2f520`) contained 15,443
runs: 11,777 success, 3,666 failure and 15,315 providers, or 51.4767/s. The
22:20–22:25 window had 18,157 runs (60.5233/s). These are durable measurement-time
history windows, not arrivals, exact live process generations or fixed-cohort
recovery. Global throughput does not establish adequate distribution across
providers or enough capacity for arrivals and existing deficits.

A stable five-second native observation at 22:55:59 (`1377c638`) measured
PostgreSQL at 44.0615 of 96 effective cores (45.8973%); the earlier 22:13 point
was 52.99%. Neither point attributes CPU to a query or proves sustained recovery.
The retained 23:04 activity receipt (`5c5ddfa9`) identifies the long ClientWrite
candidate as an active local backend declaring `pg_dump`, with about 2,198
seconds statement age and 10,593 seconds transaction age. A backup table COPY
can match the `contract_close_access` substring family. The separate backup
start supports that candidate but supplies no socket/backend join, so this is
not proved contract-close worker ownership or a cancellation basis. Other
reservation-I/O and close-family samples remain separate. The diagnostic source
now preserves finite application/locality/backend fields; the running watcher
has not been promoted by this source change.

The authorized 22:50 bounded activity read found no current long reliability
family; it does not determine how the historical 111-minute statement ended.
The fixed 22:32–22:51 terminal-log read (`7edcfe23`) returned no matching records.
Successful-return logs require verbosity one, capture completeness is unproved,
and function return precedes durable task finalization. Completion, retry,
timeout and the relation to the cohort transition remain unresolved.

Connect source `59864c2b` and its thin release counterpart remove the unused
production compatibility ACK queue and worker while preserving direct ACK
ownership, bounded legacy compatibility and teardown. Local allocation controls
show no compatibility-channel storage for constructor-owned sequences; this is
source-only evidence and gives no production heap-recovery credit. Existing
source-qualified selector, pacing, backend764, emitter and memory ownership
work remains pending image publication/deployment. Registry publication still
awaits replacement-credential disposition. Report-ID emission requires native
schema764 and all backend-generation coverage; ID-less peers remain ambiguous.

The source backup was still active and generation-bound at 23:14; completion,
restore and destination custody are unproved, so migration764 remains unapplied.
ARIN leases remain expired and startup endpoints need the next ordinary rollout;
no forced ARIN-only restart, full Main shadow or sufficient positive subscriber
supply has been established. Policy two stays inactive. The 493 offline verified
leaves are not Main provider supply. FP2FIX, current resident memory ownership,
financial backlog recovery and the authoritative standing monitor remain active.


### 2026-10-04 00:37 UTC optional capture and ordinary rollout checkpoint

The last cited coherent URL census is historical: 79.15% quota complete at
23:34. It is not current coverage or a fixed-provider recovery claim. A fresh
fixed-window accepted-measurement read is separate; neither source changes nor
global throughput prove provider distribution, financial recovery or CPU cause.

API and Proxy 6200 deployment commands completed successfully. Their current
native runtime convergence remains unproved here. Connect and Taskworker 6200
selection is held while the successor restores genuine native Go VCS metadata
and isolates optional capture failure from primary startup. The release keeps
schema763, the qualified dependency graph, the bounded closer, mature-deficient
one-minute pacing, ownership ledger and guarded reliability no-op. It does not
emit Report IDs or claim migration764. Focused actual Run controls reproduce the
fresh enabled/no-VCS and unsafe-config startup failure, then pass with strict
capture refusal preserved. No Main improvement is attributed to those controls.

The user has lifted the registry credential prerequisite; this is authorization
to continue normal publication, not a claim of credential replacement. The next
four-service release builds from a genuine Git clone. No forced ARIN-only restart
is planned. A fresh three-hour baseline operator was prepared, but expired unused
publication activations establish no Main lease or endpoint change. Policy two
remains inactive pending full Main baseline/final capture and sufficient verified
residential/business provider supply; 493 offline address leaves are not supply.

The c39 provenance monitor is authoritative on its ordinary fifteen-minute
cadence. The source backup remains active; completion, restore and destination
custody are unproved, and migration764 remains unapplied. Retained legacy intent
refusals, resident memory, URL coverage and ARIN shadow work remain open. FP2FIX
continues through source merge, ordinary all-block rollout and fresh measurement.

### 2026-10-04 01:14 UTC selection and runtime observation checkpoint

All four 6300 service deployment commands completed with exit zero; Connect
finished at 01:01:30 after all five block selections. The release uses genuine
source `7cd8c437`, schema763 and the qualified dependency graph, including the
bounded closer, one-minute mature-deficit pacing, ACK allocation correction,
ownership ledger, guarded reliability no-op and optional-capture startup
isolation. Selection success does not prove every running generation changed.

The bounded 01:07 HTTPS read (`5d2d`) qualified fresh source/start witnesses for
46/68 slots: API 20/20, Taskworker 8/8, Connect 10/20 and Proxy 8/20. Connect had
six old-5900 and four slots without a unique current selection; Proxy had seven
old-6200 and five slots without a unique current selection. Missing/stale start
evidence and tied newest processes share that unknown result. These are
self-reported metric witnesses, not
native executable, routing, drain-phase or predecessor-retirement proof. The
CLI's 21/100 response tally is also not an owner count, and its normal timeout
return does not establish convergence. Current host proof remains separate.

The coherent 01:07:38 URL census reported 66,589/68,687 quota complete (96.95%),
2,119 overdue, 1,223 due, 4,545 runs needed, two warming and two uninitialized.
The eligible denominator is 3,933 smaller than at 00:52; this is not a fixed-ID
recovery comparison or attribution of the change to 6300. The 136,881.7-second
oldest-due value covers due timestamps of eligible cycles, including quota-full
providers; it does not by itself locate the oldest deficient overdue provider.
A bounded eligible tail and its measured history are still needed for the
remaining coverage gap. Setup-only completions do not satisfy the ten-run goal.

The fixed 00:31–00:34 log read (`b4a1c1ee`) selected 26 caught legacy settlement
errors, all finite `insufficient_escrow`, below its 64-record cap. It does not
classify the whole durable backlog or prove worker death, a current native
generation or the cause of DNS/window-stall observations. Accounting ownership
and refused debt remain preserved; no clamp, replay or deletion is authorized.

The authoritative c39 watcher continues at the ordinary fifteen-minute cadence.
ARIN lease publication is file-only and cannot revive a listener absent at
startup; full baseline/final Main shadow and verified subscriber supply remain
open. Policy two and the separately prepared Redis native-reader flag remain
inactive. Backup completion is unproved and migration764/Report-ID emission
remain pending. FP2FIX remains active through complete generation qualification,
fresh measurement and repair of the remaining coverage and financial boundaries.


### 2026-10-04 02:34 UTC due-index rollout and renewal checkpoint

All four 6400 deployment commands completed. The fresh source/start metric
reference qualified 56/68 slots at source `2cae806b`: API 20/20, Taskworker 8/8,
Connect 14/20 and Proxy 14/20. The other six Connect and six Proxy slots uniquely
reported the preceding 6300 source. These are metric witnesses, not native
executable or predecessor-retirement proof. The deployed due-index correction
removed 400,000 filtered future rows in each local owning-query control,
reducing 5,354 buffers to three; its effect on Main CPU/backlog remains unproved.

Current numeric quota coverage is unknown in this checkpoint. The last cited
coherent counts were historical 95.10% at 01:38; the 01:53 and 02:08 observations
were stale. The 02:23 visibility alert reports a coherent census but omitted its
counts when only hourly history was incomplete. The source correction preserves
qualified census counts on that warning while keeping stale/ambiguous census
values unreported. Absence of a deficit alert is not full-coverage proof.

The six-minute renewal source admits replacement before one of the latest ten
accepted measurements expires. Native controls with 25-second completion show
expiry-only admission's 25-second gap and its repair; eleven/twelve current rows,
clustered expiries, concurrent claims, setup/replay and policy/security controls
preserve bounded work and actual measurement credit. This source is prepared for
the next ordinary release and has not been deployed. Existing persisted deadlines
refresh when touched; neither the controls nor a future deployment imply instant
100% coverage. The separate payload-instrumentation source is not in that release.

The attempted bounded ready-tail read stopped at its plan guard without provider
results; no tail prevalence or cause follows. A plan-only discriminator is being
qualified rather than widening its limits. The c39 monitor remains authoritative
until separately qualified promotion, preserving the ordinary fifteen-minute
cadence. Backup completion, migration764/Report-ID emission, ARIN baseline/final
Main shadow and verified subscriber supply remain open; policy two and the
prepared Redis native-reader flag remain inactive. FP2FIX continues through
source merge, all-block deployment and fresh measurements.

### 2026-10-04 09:40 UTC subscriber coverage qualification checkpoint

The earlier inactive-policy statements above are historical: Root's Main
receipts selected policy two at 03:57 UTC, and the 04:28 native readback found
zero Quality supply on all eight Taskworkers. The selected positive catalog
then covered only Google Fiber and Webpass. Enforcement alone did not prove
adequate subscriber coverage. The later user policy defaults an identified
subscriber ISP clean when no additional contrary discriminator exists;
explicit hosting, proxy, virtual-ISP, transit, owner/origin conflicts and
independent risk keep their vetoes. Missing identity remains review work.

The independently checked global v5 candidate now joins 90 reviewed subscriber
operators and 100 ASNs across 45 countries to pinned RIPE RIS origins. Complete
bidirectional output validation passed across 7,058,446 serialized leaves,
including exact approval attribution and preserved risk. Independent native
decoding on the qualified 6800 source passed 206 public origin samples. Resource
SHA-256 is `0abe2281bf2f257a06735d6875e7e073c1422187637060fe54a615a8ba110be0`,
epoch `1791105668`; the frozen handoff is
`temp/arin-subscriber-coverage-20261004/global-candidate-handoff-v5.json`,
SHA-256 `13cb54ecee2d902b100b017e169053f373e6c219a9e141db16b6a91d5faffe53`.
These are local resource checks, not Main provider counts or activation proof.

Country/state/province research remains incomplete: 80 reviewed advertised
operator/region footprints cover 49 of 3,865 indexed regions, and no regional
top-30 ranking is complete. TRAI's dated national metric rankings remain
separate. Fresh current-provider overlap, loaded epoch, rollup and a complete
native Quality generation are still required to establish recovery. See
`arindbctl/CLASSIFICATION.md` for the evidence and the directional count limits.

### 2026-10-04 10:47 UTC global v7 subscriber release checkpoint

The immutable v7 candidate expands the reviewed global catalog to 4,786
subscriber operators, 4,966 positive ASNs and 46 countries. Brazil's bulk
primary-source rule joins regulator-reported fixed Internet subscribers by
exact legal CNPJ root to NIC.br ASN identity and Brazilian delegation. Sol
independently reprocessed every regulator row, state ranking and identity join;
this is a reviewed bulk rule, not thousands of individually reviewed websites.
All 27 Brazilian states now have August 2026 fixed-Internet metric top-30
rankings, while worldwide and combined-service rankings remain incomplete.
Legitimate MVNO/reseller status and mixed products alone are not proxy evidence;
explicit proxy/hosting/transit/conflicting-origin and independent risk vetoes
remain effective.

Complete bidirectional readback passed 262.97s and counted 7,075,149 serialized
candidate leaves, including 2,957,726 subscriber leaves. Independent native
decoding passed 5,023 public samples (4,696 positive, 327 excluded). These are
offline artifact controls, not Main provider supply. Resource SHA-256 is
`2bca7f6f76bd0a099cfa8704c326efea4d3dfb9cd734a0350f10ff2a842fb150`,
epoch `1791109251`; catalog SHA-256 is
`02e7ccf38fc57626f429918b8b58a375df468cdc05e4a7f55dd761cbdfa361a4`.
The exact independent gates under
`temp/arin-subscriber-coverage-20261004/` are `sol-brazil-v5-gate.json`
(`7ab1e15f9c8c45fd61d5a456d313421c4b1926b58c8f643fd643553cbe0f3464`)
and `sol-global-v7-native-gate.json`
(`ce48eea69afbbb9bec9e02607ebad125f1b49de66c1e0e7be2f13b824db05964`).
The full readback summary hash is
`a480a1e155d472d7ccce3fa2e3fba53c99f8832dbed80d10f8caa163121baac3`.

No Main publication, selection or recovered Quality supply is claimed here.
Root owns Config identity and publication, current-provider shadow, cached
reader rollover and fresh lookup/rollup/native-index proof. The previously
observed policy-two activation and sparse-catalog Quality gap remain the
production facts until Root's new receipts establish the changed boundary.

### 2026-10-04 11:48 UTC close-cursor rollout and remaining work

Root deployed the completed-accounting-error cursor correction from Main
`2d96d996` through the qualified Taskworker carrier `25988a51`, version
`2026.10.4-close-retry+1060890500`. All eight deployment commands completed by
11:40:43 UTC. The correctly activated eight-Taskworker metrics receipts at
11:45 and 11:48 independently qualified that release and compared the same
process generations. The earlier runtime receipt's inconsistent disabled flag
remains an evidence caveat; the later metrics manifest authorized its contact.
The carrier preserves the preceding Redis admission and asynchronous settlement
models; this rollout is not a financial-path revert.

The fresh accepted URL census reported 85,283 eligible providers and zero quota
or secure completions, with 349,724 measured runs still needed. During the
135–151s same-process interval, the shard-zero owner completed one refresh in
9.04s and one deadline at 10.00s. The census age was 148.593s at the second read.
All eight native Quality buckets were zero; Speed ranged 2,359–2,382 and Online
85,259–85,260. Missing close-task counters remain unknown. These measurements
restore a current denominator; they do not establish full coverage or backlog
recovery. Root's paired reduction is
`temp/pg-contention-20261004/root-tw-census-delta-1145-1148.json`, SHA-256
`9f696c11268cc9b7f6f4a00bdcecf1190e86f5ca18ac56c52d391d52035b9bab`.

The subsequent local close-selector control exposed report-table work beyond
the existing raw-page cap: 1.2 million synthetic report rows caused about 2.45
million row examinations for one 25,000-contract page. Keyed report joins
returned the same first and continued pages while examining about 97,000 rows.
Local elapsed time improved, but buffer accesses increased; Main performance
and its actual execution plan remain unproved. The independent experiment is
`temp/pg-contention-20261004/close-query-loaded-v2/sol-independent-result.json`,
SHA-256 `143e0be6a7af5e5cc762c0c74dc1a3bab793213e2d065407ad431f624ef6e1ea`.
The owning patch bounds those two nullable report lookups and retains the
existing quiet-period, intent, accounting, cursor and retry rules. Its source
qualification and deployment are separate from the already deployed cursor fix.

Native truncated prefixes also point to the close selector and disconnected
client reliability fill; they do not prove full statement identity, runtime
caller, cancellation failure or CPU ownership. Current close/legacy settlement
backlog, all-provider accepted-run throughput, full subscriber coverage, actual
prober funding/admission, and sustained performance recovery remain open. Keep
accepted measured outcomes separate from setup failures, and measure the
configured concurrency before considering a capacity change.

### 2026-10-04 13:43 UTC published close-page fix and pool pressure

The keyed close-page selector is deployed on all eight Taskworkers through the
qualified carrier `b7fb7f56`, version
`2026.10.4-close-page-keyed+1060890600`. Its financial and retry paths preserve
the prior carrier. The 12:48–12:49 same-process measurements reported 86,045
eligible providers, zero complete quotas, 257,813 runs needed and zero Quality
buckets. That earlier interval had zero measured successes. A later 13:04–13:07
eight-worker pair did observe 57 measured successes and 3,240 measured errors,
plus 512 `run_not_measured` producer events. These distinct counters and windows
are not an exact join or proof of durable quota acceptance. Full coverage remains
open. The dense/sparse reliability alternatives remain held because local wins
regressed other loaded cohorts.

The four-API 13:41–13:43 pool pair qualified the same processes but unequal
135–150s intervals. Two sampled pools stayed at 512 total/no idle at both ends,
with every successful acquire counted as empty-pool work. Their mean successful
acquisition was about 1.11s; the other two were about 6ms. The finite reduction
is `temp/pg-contention-20261004/sol-api-url-pool-paired-finite-1341-1343.json`,
SHA-256 `f5c76997700c6f38b8e3f58b9a55954ba86f280a1584d58e706129fbc9536740`.
These metrics cover every API database call and do not identify the retaining
route, native PostgreSQL backend, CPU owner or PgBouncer shard.

The negative subscriber cache previously consumed its one-second age while
waiting for a connection, before any fact was read. The dispatch-clock repair
starts that same bounded age immediately before the first fact query; later
chunks cannot refresh it. It preserves positive fresh reads, all-live
connection checks, bounded capacity, cancellation/panic cleanup, and the policy
epoch/new-flight deletion guard. Its deterministic queued-read control requires
one SQL-reader call for an owner and 63 already-coalesced followers after a
two-second acquisition wait. This is a local amplification mechanism, not a
claim of Main recovery. The separate retention candidate removes the extra
per-Due storage-cleanup checkout only after its bounded global worker lane is
deployed and observed. Neither change raises pool limits or weakens quota,
measurement, security, proxy/hosting or financial authority.

### 2026-10-04 13:14 UTC measured throughput and Due pool boundary

Root deployed the qualified keyed close-selector carrier `b7fb7f56`, Taskworker
version `2026.10.4-close-page-keyed+1060890600`, and the later runtime/metrics receipts
qualified all eight Taskworkers. The financial admission, asynchronous debit,
legacy settlement and close retry models remain the preceding release's bytes.
The 12:48–12:49 census reported 86,045 eligible providers, zero quota-complete
providers and 257,813 measured runs needed; all eight Quality buckets were zero.
The same-process 90–106-second throughput windows had zero measured successes,
717 measured errors and 1,080 local failures. Those counts describe distinct
process intervals, not one synchronized fleet rate. Root's delta is
`temp/pg-contention-20261004/root-tw-url-throughput-90600-delta-1248-1249.json`,
SHA-256 `4cbafd00c849dec7024cadd39deb710860359baf727787c177ffc17fc8e302c8`.

The later 13:04–13:07 pair observed 57 measured successes, 3,240 measured errors
and 512 `run_not_measured` attempts across eight stable processes. Health and
attempt submissions were acknowledged, but those counters are not exact
request joins or distinct durable quota history. Setup failures still earn no
quota; removing the initial provider probe does not remove actual application
writer evidence or permit an extra ping. The finite paired reduction is
`temp/pg-contention-20261004/sol-tw-url-failures-paired-finite-1304-1307.json`,
SHA-256 `f63b70ce6d5b3117dda9c36d22260ccfb2e0a7fc1627460007dd78bd51e8f54b`.

Four fixed API processes subsequently returned 56 Due calls with 3.175-second
mean model time. The old second storage-retention transaction consumed
1.345 seconds on average, mostly acquiring a connection and starting the
transaction; its SQL averaged 0.007 seconds. These are nested completed-call
wall times, not CPU or backend-owner proof. The finite reduction is
`temp/pg-contention-20261004/sol-api-url-due-paired-finite-1312-1314.json`,
SHA-256 `1585fb4ae16cf4ab105dff7d2dc3592e458c70ece0c9ccb32b4cf2f6a8bd417f`.
The owning candidate moves seven-day storage cleanup to one bounded durable
Taskworker lane before removing its second checkout from API Due. Quota,
security, current completion expiry and claim issuance remain synchronous.
Deployment must verify the lane first, then measure API and fleet behavior.

Reliability-fill alternatives remain held: the client-first query's sparse
speedup regressed a cohort with unrelated missing clients from about 137 to
680 milliseconds and increased dense buffer work. Census aggregate variants
likewise have no general loaded-control win. These experiments do not justify
an unconditional query replacement. Root's fresh backlog, exact pool/backend
owners, sustained accepted-run coverage, global subscriber overlap and Main
performance remain open; no full-coverage or CPU-recovery claim is made.

### 2026-10-04 14:34 UTC shared-pool ownership and merged source

Root merged the completed retention, subscriber dispatch-clock, local fixture,
test-boundary and monitor visibility work into Main `f888f495`. Worker-lane
deployment and runtime observation remain distinct from deploying the API Due
cleanup removal. The existing dispatch worktree was advanced to that Main
baseline for the next profile connection-lifetime correction.

`GetNetworkUser` held the profile-read client across four independent
authentication transactions. The owning fix releases that client before those
unchanged readers; the local control runs eight real profiles with one pool
slot and populated password, SSO, wallet and seedphrase result families. It
also checks cancellation followed by an independent read and a missing user.
These controls isolate a possible starvation mechanism; they do not establish
the rate of this endpoint or its contribution to Main's observed pool pressure.

Published reliability maintenance permits a two-hour checkpoint statement.
The client checkpoints publish before the separate seven-day network window,
but an individual anchor can retain its earlier DELETE locks and MVCC horizon
until commit. The new disabled witness preserves the existing three-second
read and native admission bounds while privately joining selected long/blocker
query IDs to bounded statement text. Truncated prefixes, absent statements and
multiple variants remain unknown. No rejected reliability performance variant
has been promoted, and provider coverage and slow-query ownership remain open.

### 2026-10-04 15:38 UTC picker latency and native queue ownership

The operator clarified that the blank picker waits too long for its API
response. Initial GET and searched POST latency must therefore be measured
separately from successful empty results. The legacy Quality-named location
filter includes public online fallback; native Quality zero alone does not
prove that this picker returned no locations. No subscriber, security, health,
timeout or fallback predicate changes accompany the fixed phase observations.

The adjacent typed-search metadata loader discarded failed pipeline commands.
It now shares the existing candidate/filter read-error helper, including an
earlier missing key masking a later failure and the expanded-parent pass.
Genuine missing metadata retains the existing partial-result behavior. This
error correction is independent of the unproved latency cause.

The qualified native PgBouncer snapshot at 15:15 observed 26,502 queued clients
across 32 instances, with no idle server connections. Three instances had zero
active server connections and one login in progress. This is current queue
pressure, without a request or backend-login cause join. A bounded successor
classifies only recent current-process log tails and compares declared backend
endpoint equality. It cannot replace source-qualified API route and pool
observations. The native five-second PostgreSQL CPU observation at 15:38 was
56.076 cores on 96 logical CPUs; that one interval is not sustained recovery.

The picker phase change measures exclusive model residence and inflight calls
while preserving the existing query, cache and policy behavior. Root continues
the qualified deployment/measurement loop for dispatch-clock, profile client
lifetime and the retention worker lane. Worker cleanup success is observed;
API cleanup removal still requires its own release proof. Current quota
coverage, successful provider traffic, picker latency and sustained database
recovery remain open.

### 2026-10-04 17:00 UTC expired reliability windows and queue recovery

The 16:38 native PgBouncer observation qualified all 32 instances against the
same native process and configuration generations as 15:15. The sequential
snapshot found 14 queued clients, 399 active and 197 idle servers; its maximum
queue age was 2.505 milliseconds. The earlier three stalled instances each had
idle servers and no queued clients. This is observed recovery from the earlier
26,502-client queue, without attribution to one rollout or proof of sustained
recovery. Four bounded current-process log tails hit their caps and contained
only the finite `other` class, so the older login cause remains unknown.

The reliability source has a separate reproducible work-bound defect: when
the complete previous window has expired, rolling add/subtract scans the gap
between windows twice. The normal thirty-minute task cadence exposes this in
the six-block client window. The correction uses the existing current-window
aggregate for disjoint windows; it does not change arithmetic, quality policy,
retained history, timeouts, task cadence or accounting. An 85,000-provider
managed control checks actual checkpoint ranges, shared-address weights,
observed zero, degraded blocks and cancellation rollback before qualification.
Overlapping windows keep the established rolling and maintenance-deferral
behavior. The separate numeric-division fast-path experiment was held because
its unique-address gains came with shared-address regressions.

Recurring `reliability_running` prefixes do not yet prove which checkpoint
or physical index owns the Main stall. Exact bounded statement and catalog
observations remain required. Picker phase deployment, current durable URL
quota coverage, successful provider traffic and sustained database recovery
remain open; neither local parity nor lower point-in-time queue counts closes
those requirements.

### 2026-10-04 17:12 UTC legacy settlement page-budget qualification

The local legacy-settlement worker control reproduced a separate retry stall:
with 64 intents and bounded 350ms per-contract residence, its fifteen-second
page budget expired after 41 committed contracts and 23 retained intents. The
old path attempted retry metadata under the canceled context and returned a
task error, losing normal cursor continuation despite those durable commits.
Candidate `ec83da08` yields only a completed prefix on its own page deadline;
the continuation cursor remains before the interrupted intent. Parent
cancellation, unrelated SQL errors and failed retry-state writes remain errors.
No debit, payout, reservation, outcome, accounting cooldown, statement timeout
or concurrency policy changed.

Independent Sol controls reproduced the baseline failure, passed six focused
model cases and the loaded/cancellation race controls, and passed vet. The
existing task continuation test passed after using the release's frozen SN
dependency instead of the advancing canonical checkout. The normal candidate
committed 41 before yielding, the race candidate 40; both resumed to exact
final accounting with no remaining reservation. The independent receipt is
`temp/close-backlog-deadline-20261004/sol-close-go.json`. No Main deployment,
throughput improvement or backlog recovery is established by these local gates.
The separate ordinary-close mixed accounting/deferred-page classification
remains under investigation.

### 2026-10-04 19:55 UTC integration and live acceptance boundaries

Canonical Main `1d834ace` combines the latest origin ARIN evidence rules with
the qualified origin-attribution capture work and uniform provider fallback.
The complete ARIN package passed 78 focused controls; three opt-in offline
controls remain explicitly skipped. The new provenance artifact has passed
independent whole-artifact readback, but its Main resource activation and
current-provider coverage are not yet proved. Its classification fields are
unchanged from the active v7 artifact; new subscriber catalog approvals require
a separate artifact build and validation.

The serial concurrent reliability covering-index repair is running. At
19:53 UTC the CLI had attached 18 of 34 partitions; final native catalog
validation and the subsequent performance observation are still required.
The API `65f0232b` and Taskworker `db96d42c` all-block rollout commands completed
successfully. API fallback carrier `c3bcac1e` passed focused, race, vet and
both-platform immutable image verification, and its all-block rollout is
running. Rollout completion alone does not prove current process convergence.

The last qualified quota census remains 18:54:35 UTC: 89,041 of 89,759 providers
completed ten accepted measured runs, with 718 incomplete and 4,298 runs needed.
Fresh post-Taskworker-rollout readers completed their HTTP transport, but none
of eight slots passed the expected runtime identity join. Their empty metric
maps are unknown, not zero counts or proof that coverage regressed. Preserve
the failed identity predicates before another measurement. The bounded durable
quota diagnostic reached its ten-second context deadline; collect a native
EXPLAIN without ANALYZE before retrying or changing its query budget.

The matched 19:44–19:49 API window qualified eleven unchanged processes.
Provider selection recorded 29,850 handlers, zero completed, 22,812 canceled
and 7,038 panic outcomes. Initial picker and search handlers completed quickly.
The five remaining processes are unknown, and the counters do not identify
the panic cause. Treat provider selection as an outage boundary until current
deployed-source measurements and actual successful selection establish recovery;
lower database or Redis utilization cannot substitute for that proof.

### 2026-10-04 uniform provider fallback source correction

The deployed API source `4ee64ef3`, current Main and the next `65f0232b` API
candidate all retained an original-request subscriber guard across Speed and
Online borrowing. Existing tests even required a Quality response to stay empty
when the same target had a full eligible Speed cohort. This conflicts with the
bucket matrix and uniform fallback contract above. The correction separates
common exclusions from native Quality membership and preserves lower-tier
fallback without claiming it as Quality supply. It also revalidates Quality
borrowed by Speed, retains strict explicit/forced Quality checks, and avoids
subscriber SQL for candidates considered only as Speed or Online.
Fresh explicit risk observed by that same SQL remains a common exclusion; it
cannot be converted to a lower-tier answer or stored as a Quality-only cached
refusal. Repeated and concurrent warm-cache controls cover this distinction.
If the later Quality read discovers risk for an already chosen Speed or named
provider, it revokes that earlier selection before computing remaining quota.
The same-target lower tiers refill the vacancy; answered and backfill counters
count the surviving selections only. The overlap control covers both cache
readers, named/discovery overlap, and native/borrowed/Online ordering.

The actual initial/search geographic picker already uses a public
native-or-Online count under its legacy Quality-named key. Neither native
Quality zero nor the completed FP2 country zero cohorts prove that this listing
returned empty or explain its latency. Root's retained 18:50 observations of
high completed Quality zero fractions are a separate selection symptom; this
source mechanism is not an exact attribution of those requests. Provider IPv6
empty cohorts are outside this investigation as requested. The frozen next API
candidate remains untouched; integration and deployment follow independent
baseline, focused and race gates. Main selection, picker latency, durable URL
quota, and sustained database recovery remain open.

### 2026-10-04 22:26 UTC Main database-path recovery

Main's outage included a failed backend-startup path across all 32 PgBouncer
shards: the qualified 21:43 UTC native snapshot had no established backend
connections, one pending login per shard, and 301,567 waiting clients.
PostgreSQL itself answered an authenticated same-role IPv4 loopback `SELECT 1`
with the pooler's configured TLS-disabled transport in approximately 53 ms.

Controlled restarts of the failed pooler generations restored backend connections
on every shard by 22:23 UTC. Pool configuration and PostgreSQL were unchanged.
The 22:24 UTC native verification qualified all 32 shards with 565 active and
47 idle backends, 4,808 waiting clients, and 4,940 active clients. This verifies
restoration of the database connection path, not complete product recovery.

The 22:26 UTC PostgreSQL activity census covered 600 rows and observed no
heavyweight Lock waits; it retained 43 lightweight-lock waits and 121 sessions
idle in transaction. Actual successful provider-selection responses, Connect
recovery, deployed generations, and the initial stalled-login cause still need
verification. Index repair remains held during acute outage recovery. Current
FP2 quota coverage cannot be inferred from historical pre-outage worker samples.

Root ledger T580 retains the immutable native recovery and verification receipts.

### 2026-10-04 22:37 UTC successful provider responses after pool recovery

Two fresh, same-process HTTP observations qualified all 16 API blocks on the
selected `2026.10.4-api-bucket-fallback-v2+1063426840` build with readiness true.
The latest matched interval observed 578 provider-selection requests and 578
HTTP-200 responses, with no cancellations or panic outcomes; mean handler time
was 24.6 ms. Initial picker requests were 30/30 HTTP-200 at 15.0 ms mean. Location
search was 2/2 HTTP-200 across 14 observed slots; two slots had no qualified lazy
search counters. Successful HTTP responses do not by themselves prove populated
results or delivery to an individual user's app.

A fresh probe in Connect edge1/g1's mounted configuration, network namespace,
and UID authenticated and completed its PostgreSQL ping in 2.25 ms. This proves
the current database path, not the application's cached pool or an end-to-end
provider session. Authenticated Connect traffic and semantic contract-creation
outcomes remain acceptance checks.

The native 22:35 sample identified PgBouncer 1.26.0 with libevent 2.1.12-stable.
All 32 shards had established backends and no pending backend login; PostgreSQL
had no heavyweight Lock wait and one lightweight-lock wait. One long active
statement was matched by its exact composed SQL hash to the seven-day network
reliability full recompute, rather than the held covering-index repair. Its
trigger and execution plan remain under investigation.

Root ledger T581 retains the API paired counters, Connect path control, native
version/owner census, and generated-cache reclamation evidence. The HTTP-200
pair is retained separately for the next acceptance ledger record. Overall
FP2 coverage and full Main recovery remain open until their own current evidence
passes.

### 2026-10-04 23:27 UTC transport recovered; financial admission remains open

The matched 22:50–22:52 interval qualified all 16 Connect processes on the
intended build with fresh authenticated carrier progress: 15,406 successful
HTTP upgrades and more than 1.26 million completed HTTP carrier writes.
These counters prove transport progress, not individual provider delivery.

Current API contract outcomes remain unacceptable. The 23:09–23:13 pair
observed 8,470 replies against 147,551 protocol rejections. Separate cause
counters attribute 73.4% of rejections to insufficient balance and 18.9% to
missing companion origins. Payer attribution and balance-release custody
are not yet established; these counters alone do not prove a customer-wide
allowance failure. Financial admission is the priority recovery lane.

All eight Taskworkers are ready on the deployed close-progress build.
Current quota-complete coverage remains only 0.56%, despite a large decrease
in aggregate remaining checks between successive censuses. Historical
pre-outage coverage must not be substituted for current coverage.

Fresh representative plans for the long seven-day network reliability
recompute use old noncovering scans across all eight recent partitions.
Root resumed the serial concurrent covering-index repair in a durable user
unit at 23:25 UTC. The valid detached child was attached, and the first
missing child rebuild started. This is operation progress, not a completed
catalog audit. The previously failed V3/V4 attempts made no Main contact.

The independently tested cooperative close-page budget is ready for an
urgent compatible Taskworker release. Main recovery and FP2 completion
remain open until financial and provider outcomes are verified.

### 2026-10-04 23:49 UTC populated picker and near-complete recovered quota

The public provider-locations endpoint returned a qualified HTTPS200 response
with66 country candidates in293ms at23:39:48. The SDK candidate set is nonempty.
This verifies the initial picker response, not a particular user's contract.

The newest pre-rollout census clock was23:36:21:17,321 of17,345 eligible providers
were quota-complete (99.8616%);24 were deficient with197 measured checks remaining.
Sparse Due responses and the low recent accepted rate are consistent with this
near-complete quota, rather than stalled URL scheduler owners.100% remains open.

Fresh paired debit clocks advanced for all16 logical shards.32 committed batches
and32 released batches progressed; every latest pending and release oldest-age
sample was zero. No aged asynchronous-debit backlog was observed. Financial
correctness still requires independent reservation and token custody evidence.
The API insufficient-balance label is substring-based and may include a joined
compensation failure; it does not classify payer ownership or prove spending.

The cooperative ordinary-close budget image was rebuilt from a source-identical
standalone clone after Go omitted revision metadata for a worktree. Both published
architectures now attest clean6e748b3, and the100% Taskworker rollout completed at
23:44:12. Actual all-eight process convergence remains a separate acceptance.
Root ledgerT583 retains these observations and the image/deployment receipts.

### 2026-10-05 00:28 UTC new-worker acceptance and stale expanded census

Two current observations qualify all eight Taskworkers on the new clean6e748b3
budget release, ready across75–91second advancing scrape intervals. The initial
picker and both public US Quality/Speed requests returned populated candidates
in under300ms. This proves public selection, not authenticated provider delivery.

The census source clocks did not advance on any worker and are20–24minutes old.
The retained eligible population has expanded to78,212, so the earlier99.86%
coverage of17,345 providers must not be treated as current. Current quota remains
unknown until fresh comparisons succeed. Source investigation separately owns
normal short-pass cancellation of census refresh and score-publication ordering
that may leave cycle eligibility hints behind newly eligible providers.

The V5 covering-index repair reported28 attached children before its CLI observed
conn closed. Fresh native reconciliation confirms28 healthy attached children,
one valid detached child and five missing children; no active build progress or
index-owner locks were observed. The next repair should attach the valid child
and concurrently build the remaining five serially. Four truncated activity rows
remain an explicit uncertainty. Root is obtaining fresh capacity and repairing
the independently supervised monitor-CONT fallback before the next quiet window.
Root ledgerT584 retains the acceptance and reconciliation evidence.

### 2026-10-05 00:56 UTC interrupted reliability-index repair complete

The durable V7 repair completed successfully at00:55:20UTC. It attached the
previously completed detached child and built the remaining five children with
CREATE INDEX CONCURRENTLY, serially. The old parent index was then removed.

Fresh native catalog verification at00:56:39UTC confirms34 of34 current
partitions have healthy, correctly shaped, attached covering indexes. The new
parent is valid; missing, invalid and wrong-shape children are all zero. No index
build progress or index-owner locks remain. Independent replay of the retained
native receipt confirms physical completion.

Both representative enter/leave custom EXPLAIN plans use covering Index Only
Scans. These are planned queries, not executed latency or CPU measurements, and
do not establish the application's selected plan. The interrupted index repair
is complete; broader database pressure and FP2 coverage remain separate work.

Native receipt: temp/reliability-index-repair-20261004/root-reliability-v7-final-v1/
run-20261005T005634.926225Z/receipt.json; SHA256
f2446b4aeff3cc9aac5c37e3e7a3a1d8abd43ad5232abf6a07b1183fbb95ccc1.
Independent closure gate SHA256
047c82226b78a249bf81650ae54daf082a64c001b4ed06512fff35762b66170d.

### 2026-10-05 02:10 UTC pooler outage recovery and current probe status

The blank picker and connection outage coincided with all 32 native PgBouncer
shards having zero established backends, one pending login each, and roughly
300,000 waiting clients. Direct PostgreSQL authentication still succeeded.
API phase evidence located the queue in authentication; sampled picker failures
expired during pool acquisition before subscriber SQL executed. Low PostgreSQL
load therefore did not establish application health.

Bounded recovery replaced 31 failed pooler generations; the remaining shard was
already healthy and was skipped. At 02:00 all 32 had established backends, 463
total, with no waiting clients. The same generations remained established in the
02:10 snapshot: 635 backends, 412 waiting clients, and a maximum wait of 57 ms.
These snapshots establish recovery of backend capacity, not continuous health or
resolution of the recurring login failure's root cause.

The bounded Quality-validation API fix is merged into main and its deployment
completed at 02:03. A public provider-location request returned 78 country
entries in 0.29 seconds at 02:09. Running-version verification and actual provider
connection progress remain separate acceptance gates. The Connect inclusive
authentication-deadline fix passed independent normal and race controls; its
verified image rollout is in progress.

All eight current Taskworkers are ready. The shard-zero census owner has ten
successful refreshes averaging 3.02 seconds, with no recorded cancellations.
Census publication is owned by shard zero; absence on other workers is not a
failure by itself. Fresh rolling quota coverage and accepted-probe throughput
remain unmeasured after recovery. FP2FIX is not complete.

Pooler baseline finite evidence SHA256
8709207fda27b0ef9dec124d6efd79506807cf237644918b76c524001e0b73be;
ten-minute comparison SHA256
59b08d41426f7bc9c3d4d5ac9fb715bd7b07fa8056c02ad75f6357fe3f8bff96.
Root ledger T588 retains the initial recovery and API deployment boundary.

### 2026-10-05 02:24 UTC provider selection and probe measurements resume

All 16 APIs were independently verified on the bounded Quality-validation
release. Its process-lifetime means were 0.406 s for the initial picker, 0.947 s
for search, and 0.465 s for selection; these are not interval rates or tail
latency. Public US requests returned three providers in Quality mode in 0.746 s
and Speed mode in 4.307 s. Public responses do not identify the serving build or
prove contract creation and traffic delivery.

All eight Taskworkers retained the same current processes across fresh samples
at 02:20 and 02:22. Accepted results increased by 430 successes and 7,263 errors:
approximately 3.70 successes/s and 62.78 errors/s over native intervals of
106–121 s. The fresh shard-zero census reported 3,990 of 36,858 eligible providers
quota-complete (10.83%), 3,987 secure-complete, and 178,487 runs still needed.
Census age was 126.5 s. Its dynamic provider cohort and short sampling interval
do not establish sustained per-provider capacity, completion time, or 100%
coverage. Current shard ownership was not queried by this narrow census reader.

Connect deployment is incomplete: only four of sixteen instances qualified on
the inclusive authentication-deadline fix at 02:21; twelve still reported the
preceding release. The deployment CLI timed out. The next API authentication
containment image was selected successfully at 02:23; its actual adoption is
still unverified. Root ledger T589 retains these distinct acceptance boundaries.

Paired probe reduction SHA256
3b7832974fe23e84bbbbcdc11837201cca0bc2774c1db20c748e9156b55856db.

### 2026-10-05 03:19 UTC API adoption and Connect deployment lock

All 16 APIs were independently verified ready on authentication-containment
source `6d52edbecbb456f6c0972069085106a5b2f8618b` at 02:33. Their native pool
snapshot contained seven constructing connections and 565 idle connections;
these point samples do not establish continuous health or user delivery.
The user still reports both a blank picker and no connected provider dot.

The 03:19 edge1 read verified new Connect containers on all four blocks. Its
g3 worker held the native host-wide Main/Connect lock while a joined descendant
ran `docker stop -t 3600` against the old container for that block. The lease
observation was complete and generation-stable. The overall reader remained
partial because an additional active unit needs its deployment domain resolved.
This proves the lock is retained during old-container drain; it does not prove
the current adoption state of the other three enabled hosts or kernel-observed
waiting Go routines.

The Warp fix releases the promotion lock before draining the old container,
preserving the existing drain grace. It is merged and pushed, with independent
normal and race controls and clean binaries for both architectures. Native
worker activation and fleet adoption verification are still pending.

The private database capture identified the provider-count SQL source, with an
active statement reaching about 13 seconds. Local dense fixtures reproduce a
full historical-rollup scan. Native planning-only EXPLAIN and bounded system
facts were captured successfully, but their public projection requires a
validator correction for PostgreSQL JIT metadata. The retained private capture
allows correction without repeating the read. These facts do not attribute
total database CPU or establish the requesting process.

Root ledger T590 records API adoption, local pooler compatibility controls,
and remaining outage boundaries. FP2FIX and end-to-end recovery remain open.

### 2026-10-05 04:35 UTC connection restored; rolling coverage remains open

The user reports that provider connection is working again. Alt investigation is
out of scope at the user's instruction; public discovery checks do not establish
which service change restored the connection. The fresh 03:24 public picker
returned 87 country candidates in 0.504 seconds. Quality and Speed selection each
returned three valid US IPv4 candidates in 0.697 and 1.787 seconds respectively;
the response bodies were parsed in memory and discarded.

The retained 04:32:51 census reports 39,651 of 75,285 eligible providers
quota-complete (52.67%), 39,632 secure-complete, and 61,760 measured runs needed.
There are 34,859 overdue and 794 warming providers, with no uninitialized cycles.
Its source age was 57.1 seconds. These dynamic-cohort counts show a lower aggregate
deficit than the 03:16 snapshot, not fixed-provider progress or sustained capacity.
The earlier 66.48 accepted outcomes/s remains historical; current hourly visibility
is incomplete and a fresh rate is required after the next Taskworker rollout.

Uniform release source `536c2f3db5c73a308e5d8b504ffcfbfcf2df42c8` retains the
Taskworker census lifetime and eligibility repairs from `0c90117a`. Root started
the API deployment after its image gate; Connect and Taskworker rollout/adoption
remain separate acceptance steps. Do not delay that rollout for an old-generation
rate pair. Root ledger T592 retains the recovery report and rollout boundary.
FP2FIX remains open until current eligible providers satisfy the rolling quota
and the independent quality, security and operational requirements.

### 2026-10-05 05:40 UTC rolling quota recovery and rate interpretation

The fresh 05:39:54.603 UTC global census reports 75,057 of 75,082 eligible
providers quota-complete (99.9667%), with 126 measured runs still needed,
22 warming providers and two uninitialized cycles. Four rows are due, and the
oldest due age is 7.36 seconds. Secure completion is 75,033: all 24 outstanding
security cases already have ten measured runs and remain a separate condition.
This is a current aggregate snapshot, not proof of sustained 100% coverage.
The earlier 04:49 census of 7,882/74,755 (10.54%) is historical; similar net
population counts do not establish fixed-provider membership or the cause of
recovery.

All eight Taskworkers qualify on source `536c2f3d` and retain the same process
identities as the earlier samples. Between the fresh 05:38 and 05:40 receipts,
acknowledged measured outcomes increased by seven successes and seven failures
across native intervals of 90.502–105.750 seconds, totaling 0.149797/s. With the
cohort almost full and few due rows, this short rate does not measure maximum
capacity. It is not directly comparable to the Oct 3 18:30–18:35 durable-history
window's 78.94 unique measured runs/s; ACK counters can include acknowledged
replays, and the history window counts immutable selected-policy run rows by
measurement time. A matching current five-minute history read is being prepared.

The deployed source parks a quota-full provider until the oldest of its latest
ten measurements reaches age 3h54m, preserving six minutes for replacement.
Clustered catch-up measurements can therefore produce quiet periods followed by
renewal waves, but Main's per-provider expiry distribution has not yet proved
that cause. No fairness, pacing, headroom or funding change follows from the
0.15/s observation. The bounded 05:27 native read found no rows in the prior
October 1 deadline's microsecond precision interval; it does not establish a
current ancient tail or rule out stale hints elsewhere. FP2FIX remains open.

Fresh paired reduction SHA256
431197ab324c25b2c1755d49581f35c3fbd562b51b709a9db71123bfd63213d2;
finite ancient-deadline reduction SHA256
899a3177c02f7a52832c7b5ac433c58b0256d2dcaa95a22a8076536bab766bcb.
