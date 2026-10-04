# Protected current-owner capture

This is an operator-only shadow comparison. It does not replace the active
MMDB, activate policy, refresh serving scores, change a location row, or cycle a
client. The aggregate always retains `actual_main_coverage=false`; an
independent complete fleet inventory, artifact custody, and operator review
supply that external authority.

## Release and resource prerequisites

Build the reviewed `arinshadowctl` from the same recorded Server source revision as
Connect and Taskworker. Deploy those two services with the capture adapter.
The current connection registry tracks announcements in every new Connect
process; a predecessor binary has no registry. Inventory every running and
draining generation, including predecessors. A latest-generation metrics
sample alone is insufficient. Missing owners stay in the SQL cohort and are
unknown, never omitted or called policy losses.

Keep today's active `arindb/arin.mmdb` and its manifest unchanged. Stage the
independently verified candidate at the separate config resource
`arindb-shadow/policy-two/arin.mmdb`, with its manifest beside it. The candidate
must have passed the full `arindbctl` structural verification/readback and
reproducible-input gate. Runtime capture hashes the same open file descriptor
and memory maps it; it checks schema metadata and treats lookup errors as
unknown. It does not repeat the expensive full-database `Verify` per process.
Immutable, read-only mounted files are required for the mapping's lifetime.

A release config staging tree therefore adds only:

```
all/arindb-shadow/policy-two/arin.mmdb
all/arindb-shadow/policy-two/manifest.json
```

Use the existing config-updater packaging from the isolated release home:

```sh
warpctl build main "$WARP_HOME/warp/config-updater"
warpctl deploy main config-updater "$CAPTURE_CONFIG_VERSION" "$REVIEWED_CONFIG_SELECTOR"
```

The exact service selectors and version come from the reviewed release plan.
Do not rely on `--config_restart=no`: deployed runners may not implement that
newer hold flag. Plan an ordinary compatible Config rollover, which may restart
existing services, or first independently qualify the selected runners' hold
behavior. Preserve every existing config and the active MMDB/flags during this
rollover. Staging is not proof that a running container sees the resource.
RunWorker's ordinary config-version mount is read-only at `/srv/warp/config`.
Do not assume a whole-Vault mount: the capture secret must be explicitly in
Connect and Taskworker's existing sorted `secret_files` list. RunWorker mounts
that file read-only at `/srv/warp/secrets/arin-shadow-capture.json`. Inspect the
actual selected mounts and resource hashes.
Do not run the all-build refresh path merely to stage this shadow: that would
also replace the active database. Normal final resource builds must still
honor the documented GeoLite2-before-ARIN build and matched places export.

## Temporary credential and endpoint

Prepare a fresh private operator directory after the candidate and active
resource identities have been qualified. This local command writes a random
32-byte HMAC key, runtime config, and an incomplete operator template; no
credential reaches stdout or argv:

```sh
umask 077
arinshadowctl capture-prepare \
  --output "$PRIVATE_CAPTURE_DIR" \
  --active-mmdb "$ACTIVE_MMDB" --active-sha256 "$ACTIVE_SHA256" \
  --candidate-mmdb "$CANDIDATE_MMDB" --candidate-sha256 "$CANDIDATE_SHA256" \
  --expires-at "$CAPTURE_EXPIRY_UTC" --capacity 250000
```

Choose expiry within four hours. The directory is created exclusively with
mode0700; all three files are0600. Never add it to Git or an image. Deliver
`arin-shadow-capture.json` to each approved host's *existing private Vault*
`main/arin-shadow-capture.json`, using the approved SSH identity and private
stdin, mode0600, owned by the service UID or root. The host's exact inventory
hostname must match before privileged installation. Do not copy the key in a
command argument or print the JSON. The scoped secret is bound from the host, not baked into the service/config
image. Add its basename to each owning service's sorted `secret_files`
selection before creating the new containers; preserve all existing entries.
The loader checks `/srv/warp/secrets/arin-shadow-capture.json` first, then the
legacy Vault resolver when that scoped file is absent. An isolated operator mount may instead set
`ARIN_SHADOW_CAPTURE_CONFIG` to an absolute0600 file.

The loader resolves active/candidate resource names through the actual
container Config resolver. Absent or expired config disables capture. A valid
config opens only an authenticated Unix listener and, in Taskworker, an
observer of the existing score publisher. No candidate reader is opened at
startup. Connect opens both mapped resources lazily for inventory/capture.
The socket is `/tmp/arin-shadow/<role>-<unique>/capture.sock` in the container:
parent/process directories0700, socket0600, identity file0600, all owned by the
process UID. RunWorker uses the image UID unless a configured `--user` says
otherwise; verify the actual UID instead of assuming root.

The identity contains Go build revision/dirty state, image config digest,
release version, environment, host/block, role, endpoint-start time, and a
fresh process nonce. `started_at` is **endpoint start**, not an OS process
start claim. Every RPC is HMAC-bound to that exact identity and process nonce,
its run/nonce/deadline, and reply hash. The credential expires with the endpoint.
Release closes candidate mappings; service close/expiry joins sockets, callback
work, and cleanup. A later capture of the same pinned resources may reopen
readers without reconnecting providers. Native lease release similarly drops
only its retained generation; the installed observer continues until expiry.

## Complete host inventory and transport

Run the following reviewed host-local command through the fixed enabled-IPv4
inventory, never edge5. Its hostname check precedes Docker/proc inspection:

```sh
sudo -n /absolute/reviewed/arinshadowctl capture-host-inventory \
  --hostname "$EXACT_INVENTORY_HOSTNAME" --env main \
  --docker /usr/bin/docker --output "$PRIVATE_NEW_HOST_DIR"
```

The command has a30-second owner, bounded Docker calls, at most64 selected
running Connect/Taskworker containers, narrow metadata fields only, and
before/after container identity comparison. It includes draining containers.
It reads each actual executable hash/build metadata and private endpoint
identity through the container's `/proc/<pid>/root`, checking source, image,
version, host/block and start binding. It reads no container environment,
Vault, logs, addresses or provider IDs. `host-inventory.json` and
`bridge-inventory.json` are private0600 artifacts; stdout has aggregate counts.
An old binary, missing endpoint, replacement, unreadable executable or bad
identity remains unqualified. Inventory is incomplete until those boundaries
are resolved. Do not kill/reconnect clients to satisfy the observer.

Create a private inventory plan containing `expected_slots` (exact `host`,
`service`, `block` for every enabled Connect/Taskworker slot), `host_inventories`
(each `host`, local private `path`, exact file `sha256`, named `bridge`), and
`bridges` (each `name`, explicit `argv`). The independently reviewed enabled
fleet inventory supplies the expected matrix; never derive that denominator
from only the hosts that answered. Copy and pin each complete private host
inventory without printing it. Assemble the operator config locally:

```sh
arinshadowctl capture-assemble \
  --template "$PRIVATE_CAPTURE_DIR/operator-template.json" \
  --inventory-plan "$PRIVATE_CAPTURE_DIR/inventory-plan.json" \
  --output "$PRIVATE_CAPTURE_DIR/operator.json"
```

Assembly rejects empty/missing hosts or slots, unsupported predecessors,
unqualified processes, unknown extra slots, reused identities and stale host
inventories (five minutes). It derives every Connect/Taskworker endpoint from
the pinned files, then binds the matrix, files, commands, run and resource
hashes into the inventory digest. `capture-current` repeats these checks before
transport. The private source matrix remains external operator authority;
assembly does not discover or redefine Main's enabled fleet. Run and resource
epochs are additionally bound by authenticated RPC. Exactly one current native
generation must answer across all inventoried Taskworkers.

Each bridge is an explicit argv array for the reviewed SSH invocation ending
in this host-local command (credentials remain in protected stdin frames):

```sh
sudo -n /absolute/reviewed/arinshadowctl capture-bridge \
  --inventory "$PRIVATE_NEW_HOST_DIR/bridge-inventory.json"
```

The bridge routes only to the protected nonce-to-socket inventory. It neither
reads the key nor derives a new socket from untrusted input. SSH must use the
user-authorized explicit identity/host-key policy and exact enabled IPv4 host
from the sealed plan. The pool permits at most **two simultaneous bridge
processes total**, joins idle transports before replacement, and never retries
a failed host. It caps total starts at512. All RPCs have a5-second cap,64KiB
request/256KiB response caps and replay limits. Count **all** concurrent
diagnostic sessions against the currently attested watcher policy; the bridge
pool is not a separate allowance. For Main's approved global-four/per-host-two
capture lane, an archive session and database tunnel occupy edge2's two slots,
while at most two bridges use distinct enabled service hosts. The service
matrix is edge0/edge1/edge3/edge4, with Connect beta/g1/g2/g3/g4 and Taskworker
g1/g2 on each host. Edge2 is not a capture service host; edge5 stays excluded.
Root must reserve the quiet lane and count any other watcher, transfer or
diagnostic session before admission. Existing actual watcher fingerprints,
inventory authority and four-fence requirements still apply. This accounting
does not establish a direct Main PostgreSQL route or authorize a new contact.

Run the coordinator under the reviewed Main database/Vault authority and the
sealed not-before/not-after window:

```sh
arinshadowctl capture-current --config "$PRIVATE_CAPTURE_DIR/operator.json" \
  > "$PRIVATE_CAPTURE_DIR/aggregate.json"
```

This command uses one read-only maintenance-pool repeatable-read cohort with
indexed count/cursor discovery. Each point read is at most256 exact keys on the
owning process's normal pool, using a fresh statement. A bounded4096-connection
lookahead groups requests by host. No raw address crosses IPC. Candidate
classification is computed only at the live owner after exact address,
durable connection/client/handler and active lookup epoch/flags are bound.
Masked hashes cannot substitute for an address. Immutable lookup times may be
old when those bindings still hold; fresh capture and durable-read times are
reported separately.

The operator has a180-second total deadline, including a separately bounded
90-second resource/inventory setup. The unchanged cohort capture/native lease
starts after setup and is bounded at90seconds and2million source
connections. One immutable score generation must be current at lease start.
Normal publication rollover does not mutate that leased membership; end
publication identity/change is reported explicitly. This is a comparison to
the pinned starting generation, not a claim that membership stayed current at
capture end. One lease per observer retains at most one older generation;
expiry releases it even without another request. Competing capture is refused.

Completion requires exact source totals and an explicit end marker. Every
source provider is counted once, including multiple connections, absent
owners and unknown membership. Every observed connection must qualify before
its provider can have an affirmative candidate result. All677 finite country
buckets (including `all` and empty buckets) are provided by the prepared
config. Missing owners/facts, source churn and transport failures remain
indeterminate and cannot become confirmed policy loss. The aggregate includes
cohort/native clocks, membership unknowns, Quality/Speed old/candidate/loss
counts, subscriber/risk/proxy classifications, bridge counts and unreleased
owners. It never contains addresses, connection/client IDs or credentials.

For evidence-based catalog expansion, `registration_connection_counts` groups
qualified connections by public ARIN organization/network handles,
classification-owner handle and SHA-256 of the rule name. It is capped at
4,096 tuples, with explicit unattributed and overflow counts. These are
**connection counts**, not distinct providers; incomparable multiple-owner
records and unavailable/invalid handles are unattributed rather than assigned
an arbitrary owner. Missing network handles remain empty. Rule hashes can be
joined locally to the pinned catalog; registration handles identify primary
ARIN records for targeted official-source research. Do not infer subscriber
use from an ASN, brand, parent organization or traffic volume. Any proposed
prefix rule still needs affirmative residential/business subscriber evidence,
proxy/virtual exclusion checks, rebuild/readback and another complete shadow.
Attribution truncation never changes the provider/native denominators.

`origin_connection_counts` complements registration attribution with the
candidate record's public origin ASN set and its reviewed `use_state`. It retains
unknown origin identities across RIR regions. Each qualified connection belongs
to at most one set; a multiple-origin route is not attributed to an arbitrary
single operator. Counters show the final subscriber/excluded/unknown/ambiguous
classification and independent risk alongside that routing identity.
Sets have at most eight strictly increasing, distinct ASNs. Missing, malformed
or larger sets increment `origin_unattributed_connections`; the aggregate keeps
at most 4,096 distinct sets and counts additional connections separately in
`origin_overflow_connections`. Existing sets still accumulate exact counts after
that bound. The sorted output is the retained groups ordered by observed
connections, not proof of a complete global top-ASN list when overflow exists.
All source, provider, native-membership and eligibility denominators remain
unchanged. Old resources or owner binaries may lack this optional attribution.
Use the current coordinator with owners exposing the added reply field; an old
strict coordinator rejects new owner replies.

Join unknown ASN sets to the pinned catalog and official RIR/operator sources
to prioritize missing identity and subscriber-service reviews. Preserve mixed
origins and contrary-use evidence during that research. Record verified regional
service footprints and ranking evidence separately; connection counts do not
prove market share, customer location or subscriber use. Then rebuild from the
unaugmented base and compare a fresh complete provider capture. The origin
snapshot manifest provides provenance for unknown routing identities; this
aggregate adds neither implicit subscriber approvals nor network-risk flags.

The supported Main capacity control covers100001 real PG providers/105001
connections,20 real Connect Unix endpoints behind4 real child multiplexers,
and the selected native publisher, with a strict2-bridge pool. It injects150ms
process setup,20ms RPC and10ms PG-read delay and exercises healthy native
publication rollover. On frozen25897, all end markers passed in60.934s under
the race detector, with90 starts and peak2 bridges. The exact owning test is
`TestArinRemoteFullPopulationTwoHostPipesRolloverAndLatency`; its helper uses
four hosts (the name's two refers to concurrent pipes). Resource opening and
inventory of all8 Taskworkers occur before the separately measured cohort
interval and remain subject to their own setup deadline.

The earlier8-host author run completed in66.160s, but the independent8-host
rerun took87.610s and **failed** its85-second headroom assertion. That result
supersedes any general8-host capacity claim. The90-second cohort lease is
unchanged. An expanded host/owner population needs another supported capacity
gate; neither complete local rows nor the finite512-start cap proves adequate
headroom. The current4-host result is a local control, not Main latency or
fleet coverage. The operator reports
setup and capture durations separately. Measured hashing/opening of two
334,604,722-byte resources took1.609s and339,792 Go heap allocation bytes;
setup is not allowed to consume the cohort's90-second lease. Mappings may
retain up to the full active plus candidate file size in resident pages; file
cache sharing across processes is not assumed for the host budget. There is
no always-on candidate mapping while capture is disabled. The earlier expired
publication failure is retained. If Main exceeds the bound, return incomplete;
never turn truncation or expiry into coverage.

## Final policy activation and rollback

A successful capture is a prerequisite, not activation. Review current verified
subscriber coverage, every required native bucket, unknowns, measured losses,
proxy/virtual exclusions and public evidence. An inadequate affirmative
catalog requires rules/evidence work and another pinned candidate comparison.
Do not make the denominator smaller or allow an ambiguous owner to pass.

Only after independent coverage/loss/operator GO, build and publish the final
active ARIN MMDB/manifest with the matched GeoLite2/places inputs through the
standard release path. Deploy the actual readers and verify old reader
retirement, fresh real lookup epoch/flags, location rollups and a completed
native refresh. The serving reader is cached per process. A shadow capture
never writes active facts; historical stored-IP hashes cannot backfill them.
Use the explicitly reviewed cutover method for fresh real lookups. Then deploy
`subscriber_quality_policy_version: 2` in the owning provider-egress config
with source-qualified serving controls. Replacing the active MMDB already
changes risk/non-quality behavior even before that guard switches.

For rollback, restore the previous paired resources and policy config using
the standard release process, verify reader retirement and real lookup/native
refresh again. First remove the capture entry from Connect/Taskworker's future scoped-secret
selection and verify the replacement units; only then remove the host secret.
A still-selected missing secret would prevent a future container start. An
expired valid config is safely disabled. Existing endpoints/mappings expire
automatically or close with their own service lifecycle. Keep the private receipt and aggregate evidence, then
remove the ephemeral key/operator files according to the approved retention
plan. No automatic cleanup deletes provider evidence or connection rows.
