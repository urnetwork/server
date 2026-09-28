# FindProviders2 supply and quality-probe repair plan

Status: implementation and Main validation in progress, 2026-09-28. This
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
Speed requests take native speed first, then native quality, then online. Keep
the existing bounded, exclusion-aware sampling, deduplicate providers across
buckets, preserve each borrowed provider's lower client-visible tier, and
apply request filters to *every* borrowed candidate. If the preferred pool is
empty, a large reliable online pool must still answer. Online is a fallback
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
The ten-success scheduling target is distinct from the ratio gate: ten
successes within the trailing four hours satisfy the success quota, but
ten successes and ten errors in the eight-hour history give `r = 0.5`, so that
provider remains online-only until its history meets the success threshold.
Ten successes are a collection target, not an additional quality/speed gate;
for example, three successes and two errors already pass the URL ratio gate.
Probing pauses only when the rolling quota is met **and** all TLS exceptions
have cleared; a quota-complete but quarantined provider still needs work.

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
still permits one success out of one observation; the ten-success/four-hour
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
ten successes remain within the rolling four-hour window **and no TLS
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
URL probes until it has 10 accepted successful URL outcomes in the **rolling
trailing four hours** and no unresolved TLS exceptions, as selected by the user.
At exactly four hours an
outcome stops contributing to this target; a delayed report uses measurement
time, not arrival time. Persist accepted history and pacing across worker
restarts. Neither a fixed period nor a long-running unfinished cycle may carry
old successes forward and claim four-hour completion. Keep failures in the
eight-hour ranking history and retry fairly while the rolling target is
incomplete. Replenish successes as they expire; reaching ten must not cause
an unconditional four-hour pause after the tenth success. The minimum fleet success throughput is
`10*N/4` URL successes per hour, with headroom for errors and retries.
Publish the denominator, eligible-due backlog, unique completions/hour,
oldest eligible evidence age, p50/p95 attempt duration by stage, attempted
versus accepted URL reports, completed ten-success quotas, security-clear
completions, unresolved/unknown-target security recovery, and estimated cycle
time. Fleet census states and their observation timestamp must be one coherent
atomic snapshot; a canceled/partial/stale census cannot advance freshness.
Replace the two old schedules with one URL-evidence workflow, preserving
historical telemetry during migration. For historical context, an earlier
pre-repair Main sweep found cheap checks covered 57,907/120,037 providers at roughly
7,199/hour, whereas full-quality probes were only 6,428/120,037 and roughly
411 attempts/hour. Those old measurements describe different populations and
must not be relabeled as unified URL-probe throughput. The new eligible `N`
must be measured before setting concurrency; simply raising worker count has
already produced millions of goroutines and timeouts without a faster cycle.

Make due admission one cheap, indexed, bounded operation per batch, excluding
providers that cannot enter the target index *before* opening a tunnel. Use
leases/claims with expiry and fair oldest-due scheduling so shards cannot
duplicate work or starve never-probed providers. Schedule paced URL work
with stable jitter until its ten-success quota is met; retry local failures in a
separate bounded lane without repeatedly taking the head of the queue.
Provider IDs map to **1,024 fixed logical slots**, independently of the current
worker/host allocation. Each shard owns a set of slots. Index eligible due
work by `(slot, next_attempt_at, client_id)` and select only owned slots, with
a lazy oldest-due merge rather than materializing every slot's entire batch.
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
has no cleanup owner. At 100,000 providers and ten successes per four hours,
it will grow by at least six million rows/day, before failed attempts. A small
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
failed-TLS rechecks while exceptions exist; stop only at rolling ten successes
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
