# FindProviders2 supply and quality-probe repair plan

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

Status: implementation and Main validation in progress, 2026-09-29. This
document defines the quality-probe/indexing contract. The checkpoint below
distinguishes deployed changes from remaining production acceptance; code or
deployment completion alone does not establish the four-hour quota or CPU
recovery. IPv6 provider admission remains outside this rollout until the
provider binaries support it.

## Product contract

The index, the probe due queue, and the monitor must use the **same base
eligibility facts** for an active, connected, valid, publicly reachable provider.
Bucket admission and probe scheduling then apply the rules below. A
reliability failure or an ARINdb risk finding disqualifies a provider from all
three public buckets and from URL probing. The ARINdb
`non_quality` finding disqualifies it from **quality only**; it may still qualify
for speed or online. Missing ARINdb coverage means *no exception*, not a risk
or hosting finding. A corrupt or unavailable entire database is a publication
error: retain the last verified database/index and alert, rather than silently
reclassifying the fleet.

| Gate | Quality | Speed | Online |
| --- | --- | --- | --- |
| Pass reliability | Required | Required | Required |
| No ARIN risk exception | Required | Required | Required |
| No probe security exception | Required | Required | Required |
| URL success ratio ≥ 0.6 | Required | Required | Not required |
| No ARIN quality exception | Required | Not required | Not required |

This matrix is the bucket admission contract. Probe admission has a different
purpose: every provider passing reliability and the ARIN **risk** gate is
eligible for paced URL probes. ARIN quality exceptions do not stop probes;
security-quarantined providers also need probes to establish recovery.

The probe records URL successes, errors, and security exceptions. There is no
separate blackhole definition or cheap-blackhole admission gate. The indexer
aggregates accepted measured URL outcomes in the preceding eight hours and
requires `success + error > 0` and `success / (success + error) >= 0.6` for
quality and speed. The threshold belongs to indexer configuration, alongside
reliability. Use the exact inclusive 3/5 boundary; 2/3 also qualifies. A URL's
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
| URL success ratio `r` | `S / N` when `N > 0`; otherwise unknown | Quality/speed gate at `r >= 0.6`, and ranking factor |
| URL ranking weight | Initially `0.1 + 0.9*r` for measured history; `1` when unknown | Multiplies existing reliability/performance selection weight; stays positive even for online providers with errors |
| Quality failure index | `ceil(MaxFailureIndex * (1-r))` for measured history; undefined when `N = 0` | Sample-count-normalized quality-tier penalty; more observations at the same ratio cannot worsen the tier |
| Relative latency | Existing measured relative latency; separately record DNS, connect, TLS, and TTFB timings for diagnosis | Performance ordering within eligible buckets |
| Throughput | Successful transferred bytes divided by transfer duration after first response byte | Speed ordering; DNS/connection wait is not transfer time |
| Evidence age | Measurement timestamps and their eight-hour expiry, not ingest/retry timestamps | Removes expired outcomes from the ratio; missing/stale measurements do not imply failure |
| Four-hour progress | Unique accepted URL successes measured strictly within the trailing four hours, capped at ten for coverage reporting | Rolling scheduling/coverage metric, not an additional admission gate; secure completion additionally requires no unresolved TLS exceptions |
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
reliability, performance, and bucket are equal. A provider at exactly `3/5`
passes the URL gate for quality/speed and gets a URL weight of `0.64`; it must
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
for example, three successes and two errors already pass the URL ratio gate.
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

The classifier should make the absence of an ARIN record default to quality,
then emit explicit, auditable exceptions:

- `risk`: the registration country's credible country set and GeoLite2's
  associated country disagree. Unknown/ambiguous countries are *unknown*, not
  automatically risky; document multi-country organizations and overrides.
- `non_quality`: verified cloud, CDN, hosting, data center, transit, VPN/proxy
  infrastructure, VPS, or other machine-hosted egress rather than individual
  subscriber or business end-user access. Verified cloud/CDN-operator ranges
  with ambiguous office-versus-hosted use are also excluded, as clarified by
  the user; a reviewed more-specific clean business/access exception may clear
  them. This is quality-only, not a risk finding. Start with reviewed
  organization/prefix rules and test against measured providers; do not use a
  broad organization-name substring as an unreviewed mass exclusion.
- An unclassified child inherits a known hosting parent's `non_quality`
  exception, as approved by the user. A reviewed clean consumer/business ISP
  child or most-specific prefix override can clear it; publish the matching
  rule, source, and ancestor provenance. An absent child classification must
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
complete. The current ARINdb only carries organization country codes and
`arinForeignScore` is a separate location score, not this new classifier.

Existing connection rows do not retain the raw observed client IP, so a
database-only backfill cannot safely reconstruct their classifications. The
approved Connect deployment will cycle connections; each actual database
lookup records `arin_lookup_at` and `arin_database_build_epoch`, including a
valid no-record result. An explicit IP override must not claim such a lookup.
After rollout, measure aggregate current-public-connection coverage against
the deployed ARIN database epoch and cutover time. Do not call the new risk
rules effective merely because the database file or Connect image changed.

The 2026-09-28 exact-owner review found omitted Google Cloud customer
`GOOGL-2`, Alibaba Cloud `AL-3` and IBM Cloud/SoftLayer `IBMC-24`/`SOFTL`
registrations. Independent permanent omission tests fail the old catalog and
pass the corrected candidate with existing cloud/access and quality-only
serving controls. The three added rules do not activate a new resource or
measure current provider impact. Native Quality's eight-hour success ratio
still permits one success out of one observation; the ten-measured-run/four-hour
collection quota is not an additional admission gate. High native counts are
not by themselves proof of a classifier error or sufficient evidence coverage.

Complete non-ARIN/global-cloud prefix coverage and independently registered
network-child inheritance remain explicit review gaps; an organization-parent
inheritance test does not prove allocation-parent inheritance. See
`arindbctl/CLASSIFICATION.md` for exact evidence, tests and the resource-build,
provider-shadow and lookup-epoch activation sequence. Official feed imports and
third-party proxy evidence remain separate candidates until their scope,
freshness, overlap and clean-access controls are reviewed. Never infer that a
customer origin is CDN infrastructure just because a CDN fronts its hostname.
Missing ARIN coverage remains no exception, not positive proof of access use.

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
gate; the 0.6 ratio, 2-second TTFB, and 100-kbps meaningful-throughput thresholds
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

Native URL-history admission currently requires selected-policy history with
`N > 0` and success ratio at least `0.6` in the eight-hour evidence window,
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
