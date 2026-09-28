# FindProviders2 supply and quality-probe repair plan

Status: design and implementation plan, 2026-09-27. This document resets the
quality-probe/indexing contract; it does not claim the changes below are live.
Implement and test it in stages, keeping Main's existing supply available until
the replacement index has been shadow-checked. IPv6 provider admission remains
outside this rollout until the provider binaries support it.

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
- `non_quality`: known hosting, data center, transit, VPS, cloud, or other
  non-consumer/non-business access networks. Start with conservative, reviewed
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

Current work checkpoints (2026-09-28 04:05 UTC; these are not production-completion claims):

| Work item | Current boundary |
| --- | --- |
| Common gates, ranking, URL evidence and TLS recovery | Targeted model normal/race, owning API/Connect/Taskworker integration, and all nine quality-probe packages in normal/race modes pass |
| Fixed-slot scheduler, rolling quota and query work | Implemented; deterministic mixed-outcome and 100k-provider/one-million-history controls pass |
| Request-filter-aware fallback refill | Production-reader regression fails before the repair; bounded refill and adjacent request-filter/Redis-error controls pass after it |
| Grant-selection CPU repair | Committed as `760bc1b8`; isolated 25-test race gate and current grant, settlement, lifecycle, controller, API, Connect and Taskworker owning partitions pass. Earlier whole-model baseline failure remains recorded; no whole-model pass is claimed |
| GeoLite2/ARIN refresh and Vault inputs | Tooling `cea4a9a3`, paired resource publication Config `5313e94`; independent runtime readback passes. ConfigA deployed; all six reachable host samples mount the new resource bytes. ARIN database epoch is `1790556325`; per-process use requires connection provenance, not just file presence |
| Connection classification provenance | Connect converged: 20/20 intended slots, no old running generation at 03:44 UTC. The 03:48 shadow found 126,160 current connections: 123,057 located, 123,055 with the exact expected epoch, two located provenance gaps, and 3,103 missing locations. These residuals still need attribution; coverage percentage alone is not acceptance |
| Country-risk review | The 03:48 shadow would exclude 465/509 classified reliable Australian providers and 79/85 Malaysian providers by risk. This is a projected membership loss, not proof of incorrect classification or an empty API response. Actual address/registration evidence and rollout-order review remain prerequisites to publication |
| URL workflow monitoring | Committed `850b3bb0`; full monitor normal (434.120s), race (485.007s), and vet pass. The attached observer was restored at 03:44 after a recorded observation gap; all ten log tails have fresh advancing post-start windows. URL workflow activation is still pending |
| Commits, migrations and four-service rollout | Core workflow committed `d479eccd`. Main migrated 721→730 at 01:07 UTC; the exact artifact probe passes. ConfigA and Connect are deployed and verified. API, Taskworker, and final ConfigB images are built, not yet deployed; 20/20 API and 8/8 Taskworker slots still run prior versions |
| PostgreSQL CPU | Recent short samples range from about 29% to 58% of 96 cores; 04:05 was about 35%. One near-30% interval does not establish sustained recovery, and the grant-selection fix has not yet reached the owning API fleet |
| Main four-hour quota and app availability | Not established; requires live accepted-history and request-local verification |
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
