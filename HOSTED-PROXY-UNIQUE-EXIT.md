# HOSTED-PROXY-UNIQUE-EXIT: N proxies, N unique exit IPs (2026-11-05)

The owner, 11-05, on support item 8205: "how do we support their use case?" Then, on the built-in option: "let's map out this feature".

This document designs a hosted-proxy option that gives a network N simultaneous, genuinely unique exit IPs from at most N hosted proxies.

**Conventions:**
- A statement with a `file:line` or a measurement is a fact about origin/main or Main on 2026-11-05. The code citations are server `c65ed59fa`, connect `539e8d80`, sdk `5517f035` and sn `b2c491cf8`.
- Everything marked **Design** was a proposal when written. Phase 1 is now built (2026-11-05); §13 holds what was built, the final API names, and where the build differs from the design.
- Measurements are read-only Main queries through `support db query`, and Mimir.

## 1. Problem and goals

**The case.** engageub is a Pro network. Its reply (support item 8205, 2026-11-05 17:29Z):
- "I need approximately 1,000 unique IP addresses for web scraping"
- "a significant portion of the generated proxies are duplicates. I created 5,000 proxies to ensure I could obtain 1,000 unique IPs"
- "My requirement is simply to have access to 1,000 unique IP addresses simultaneously"

**Why a proxy today is not an IP.** A hosted proxy is a full client device:
- The proxy device applies its stored location and performance profile to an sdk `DeviceLocal` (`proxy/proxy_device.go:1328-1332`).
- That device runs the ordinary multi-client windows: a quality window of 6 exits (`connect/ip_remote_multi_client.go:226-228`) and a speed window of 1-2, with a hard maximum of 4 (`:236-239`).
- So one proxy spreads its flows over several providers at once.
- Every proxy of a network draws its exits independently from the same provider pool, so the exits of different proxies overlap.

**Measured, engageub (network `01971732-…`), Main 18:50-18:55Z:**

| Measure | Value |
|---|---|
| Open transfer contracts paid by the network (sample limit 60,000) | 41,213 |
| Distinct exit providers in them | 16,343 |
| Distinct window clients (source ids) | 17,324 |
| Of 14,912 sampled exit providers: no connected connection row | 2,480 |
| … distinct connection-address hashes among the rest (12,432) | 546 |
| … providers behind the single most common hash | 11,745 |

**The most common hash.** In a 20,000-contract sample it carried 6,308 of engageub's providers.
- That one address has 31,436 connected rows on the connect service, on edge-0 (19,609), edge-1 (11,294), edge-4 (372) and edge-3 (161).
- All of them connected between 08:27Z and 18:55Z today.
- None of them is in the current client_reliability block.
- It is one host or NAT carrying tens of thousands of provider slots. The provider CLI's SOCKS5 exits do exactly this: "Each exit gets its own provider slot" (`sn miner/cli_front.go:151`).

**The fleet, for comparison.** client_reliability block 29898408 has 117,460 provider rows on 22,810 connection-address hashes:
- 13,308 hashes carry one provider;
- 6,961 carry 2-9;
- 2,541 carry 10 or more, which is 73,740 rows, or 63% of providers;
- the largest carries 449.

**Goals (Design):**
1. **Unique exits.** A network asks for N proxies and gets N proxies whose exit IPs are pairwise distinct within the network, each held simultaneously.
2. **Sticky.** A proxy keeps its exit until it fails or the customer rotates it.
3. **Automatic replacement on failure,** with a new IP that is still unique.
4. **Plan caps apply unchanged.** Pro allows 1,000 active hosted proxies and 20 creates a minute (`model/network_client_proxy_limit.go:42-46`), with no override.

**Non-goals:**
- uniqueness across different networks;
- exposing provider IPs;
- per-request rotation.

## 2. The user-facing API (Design)

The final names, including additions to this design (`replace_mode`, `state_reason`, the error flags, the route's access), are in §13.

**Option.** `proxy_config.unique_exit: true` on `/network/auth-client`. `ProxyConfig` today has `lock_caller_ip`, `lock_ip_list`, `https_require_auth`, `enable_wg` and `initial_device_state` (`model/network_client_model.go:219-227`).
- **Location constraint:** reuse the existing `initial_device_state.location` or `country_code`. Today the server resolves these at `model/network_client_model.go:466-491`, and a missing location is refused as "Invalid location" (`:479-486`). The constraint limits the candidate pool. `best_available` means any country.
- **Optional `unique_exit_replace: "auto" | "manual"`.** The default `auto` replaces on failure (§6). `manual` keeps the proxy pinned and failing until the customer rotates it, for session-sticky use.

**Result.** `proxy_config_result.exit` and the proxies list both report:
- `exit_id`: an opaque id, stable while the proxy is pinned to one assignment;
- `country_code`, `region` and `city`, from the provider's egress location;
- `exit_since`;
- `state`: `assigned`, `replacing`, or `no_supply`.

**Exit IP:** not exposed by the API, by recommendation (open question Q1). The customer can see it through any IP echo over the proxy, and providers' IPs are not otherwise published. The server deliberately never stores exit IPs today (`controller/provider_egress_location_controller.go:31-36`).

**Rotate on demand.** `POST /network/proxy/rotate-exit {client_id}` re-assigns the proxy to a new unique exit. It is rate-limited per proxy (Design: 1 a minute, 60 a day).

**Errors, each a refusal with a message rather than a 500:**
- **`no_unique_supply`:** no healthy provider with an egress IP unused by this network exists in the requested location. The proxy is not created, and no cap count or create-rate count is used.
- **The cap and rate:** the existing messages and the free upgrade prompt, unchanged (`model/network_client_proxy_limit.go`).
- **`unique_exit` on a free network:** refused as Pro-only (§7).

**ur.io, docs and KB:**
- **UI:** a "Unique IP" toggle in the create-proxy flow (mmm `proxyFlow.js`), with a count field for batch creation at the rate limit, and the exit country and `exit_since` per proxy.
- **Guide:** a section in `ur.io/docs/HOSTED-PROXIES.md`, in English and Chinese.
- **KB:** a new `unique-exit-proxies` answer in factury `support/kb/answers.yml`, plus a `proxy-limits` cross-link.

## 3. What "unique" means

**The identity (fact, reused).** The verify protocol already has privacy-preserving egress identities:
- `VerifyEgressIndexHashWithSettings` is a keyed exact-address hash (`model/verify_model.go:564-593`).
- The reverse index counts IPv6 egress by its /64 (`model/verify_model.go:356-378`).
- The coarser scoring prefix is /29 for IPv4 and /48 for IPv6 (`:537-547`).

**The uniqueness key (Design):**
- IPv4: the keyed exact-address hash.
- IPv6: the keyed /64 hash, because a residential IPv6 host rotates addresses inside its /64.
- A proxy's key is its exit provider's current egress key in the family its traffic uses. Hosted proxies are IPv4-inner (`proxy/proxy_device.go:1414-1423`), so the IPv4 key is primary.

**Farms.** Providers that share an egress IP produce the same key, so the allocator gives at most one of them to a network. No farm heuristic is needed for correctness.

**Proxy-miner slots** share one connection address but may egress through distinct SOCKS5 upstreams (§1). That makes the connection address the wrong key: 11,745 of engageub's exits shared one. Uniqueness must be keyed on the observed egress, never on `client_address_hash`.

**Stronger modes (Design, later, open question Q3):**
- `unique_exit_scope: "ip"` is the default.
- `"subnet"` means /24 for IPv4 and /48 for IPv6.
- `"asn"` means distinct ASNs. It needs an ASN source; the egress location is resolved with GeoLite2 (`controller/provider_egress_location_controller.go:62-66`, `:91`), and the ASN database would be an addition.

**Churn (Design):** a residential provider's egress IP changes. It is caught three ways:
- the confirm step (§5) runs again on every reconnect of the pinned window client;
- it also runs every 30 minutes;
- the probes report a new egress hash (§4).

A changed key that now collides within the network triggers a replacement (§6). A changed key that is still unique only updates the assignment.

## 4. The allocator (Design)

**Egress keys need storing first.** The egress probe sees each provider's exit IP through its tunnel. Today the ingest resolves it to a location and stores only the location (`controller/provider_egress_location_controller.go:31-36`, `:206-225`).
- **Change:** also store the keyed IPv4 address hash and the keyed IPv6 /64 hash, plus `egress_key_observed_at`, on `provider_egress_location`. The IP itself is still never stored.
- **Privacy:** this matches the verify index's existing practice.

**The data model** is one new table:

```
proxy_exit_assignment (
  assignment_id       uuid PRIMARY KEY,
  network_id          uuid NOT NULL,
  proxy_client_id     uuid NOT NULL,          -- the hosted proxy's client
  provider_client_id  uuid NOT NULL,
  egress_key          bytea NOT NULL,         -- §3 key, keyed hash
  state               varchar(16) NOT NULL,   -- pending | active | replacing | released
  assigned_at, confirmed_at, released_at timestamp, release_reason varchar(32)
)
UNIQUE (network_id, egress_key) WHERE state IN ('pending','active')     -- uniqueness, enforced by Postgres
UNIQUE (proxy_client_id) WHERE state IN ('pending','active')            -- one live assignment per proxy
INDEX (provider_client_id) WHERE state IN ('pending','active')          -- fairness counts
```

**Selection,** for one proxy:
1. Draw candidates with the same primitive clients use, `/network/find-providers2` in its `quality` bucket. That bucket holds providers whose latest egress health run passed at least nine in ten real-site loads, and it always excludes blackhole and TLS-authentication verdicts (`connect/api/bringyour.yml:12953-12975`). Narrow by the location constraint, and exclude the network's live assignments with `exclude_client_ids`.
2. Drop candidates with no stored egress key, or a key older than 24 h. Drop keys the network already holds.
3. Prefer, in order:
   - keys observed stable across at least 2 probes, which down-ranks churning proxy-miner upstreams;
   - an ASN not yet used by the network, once ASN exists;
   - the egress-index rank `find-providers2` already applies.
4. Insert the assignment as `pending`. A unique-violation on `(network_id, egress_key)` means a concurrent allocation took that key, so take the next candidate. The constraint, not the application, guarantees uniqueness.
5. The proxy device confirms (§5) and moves the row to `active`.

**Fairness.** A provider may carry at most K live unique-exit assignments across all networks (Design: K = 3, open question Q4), counted on the provider index. A scraper's load then can't concentrate on a few homes.

**Supply by country.** Healthy is the share of probed providers passing egress health, measured by fork R3 from Mimir on 11-05:

| Country | Online | Healthy | Healthy providers (approx.) |
|---|---|---|---|
| US | 80.4k | 57% | 45.8k |
| VN | 11.2k | 28% | 3.1k |
| FR | 7.9k | 31% | 2.4k |
| DE | 7.2k | 33% | 2.4k |
| BR | 2.3k | 77% | 1.8k |
| GB | 5.2k | 31% | 1.6k |
| CA | 1.9k | 70% | 1.3k |
| IT | 2.1k | 32% | 0.7k |
| NL | 1.1k | 50% | 0.6k |
| ES | 2.1k | 17% | 0.4k |
| SG | 1.4k | 22% | 0.3k |
| TH | 1.8k | 16% | 0.3k |
| RU | 1.4k | 5% | 0.07k |

Unique keys are fewer than healthy providers wherever farms sit. Fleet-wide there are 22,810 connection addresses for 117,460 providers (§1), which is only an approximation, since egress is the real key. The first phase 1 deliverable is a per-country gauge of distinct healthy egress keys, so `no_unique_supply` can be predicted before a customer hits it.

## 5. The data plane (Design, with what already exists)

**Pinning exists today.**
- A location's `connect_location_id.client_id` names one device (`sdk/sdk.go:1076-1082`). The API documents it for `proxy_config.initial_device_state.location` (`connect/api/bringyour.yml:10936-10942`).
- The sdk turns a client-id location into a fixed destination with one window client: "`ApiMultiClientGenerator.FixedDestinationSize` returns one for its single ClientId spec, so the normal 6-quality + 2-speed auto-window maxima do not apply" (`sdk/device_local.go:280-284`).
- Hosted devices' guards block route-local, providing, extenders and direct mode (`sdk/device_local.go:3625-3631`, `:4608-4626`, `:5289-5292`), not device locations.
- Contract creation resolves a public relationship to a destination advertising public mode and allows it (`controller/connect_controller.go:915-933`).

**What changes, in proxy/proxy_device.go:**
1. On creation, or on a (re)assignment notice, the device sets its location to `ConnectLocation{ConnectLocationId{ClientId: assigned provider}}` through the same `SetConnectLocation` call it uses now (`:1330`). The window then becomes the one fixed exit.
2. **The confirm step.** After the fixed window client is up, the device requests the operator's IP echo (`/my-ip-info`, the endpoint the egress probe already uses, per `controller/provider_egress_location_controller.go:31-33`) through the tunnel. It computes the §3 key in process with the vault key, and moves the assignment to `active`, or to `replacing` on a collision. The proxy service already has the model and vault in process.
3. **Failure hooks:** window-client cancel or conviction, and the provider's connection loss (§6).

**What stays:**
- connect (no change);
- the sdk fixed-destination path;
- WireGuard and HTTPS/SOCKS listeners;
- proxy credentials;
- the 45-minute idle hold and CPU1's idle-refill hold (`proxy/proxy_device.go:1077`, `:1089`).

**Contracts and payouts:** unchanged. Pinned traffic is ordinary public contracts from the proxy's window client to its provider, paid by the network's balance and settled like any traffic.

## 6. Failure and replacement (Design)

**What detects a failure:**
- the fixed window client is convicted (continuous-ping failure after its first answer) or cancelled;
- the provider has no connected connection;
- the provider gets a blackhole or TLS-authentication verdict, or fails egress health;
- a confirm finds a changed key that collides.

**Grace:** 120 s for a lost connection, to ride out residential reconnects. No grace for verdicts or collisions.

**Replacement:**
- Release the assignment with its reason, and allocate a new one by §4, excluding the old provider for 24 h.
- `exit_id` and `exit_since` change. The customer sees this in the proxies list and the result, and later through an optional webhook.
- In `manual` mode, the proxy reports `state: replacing` and waits for rotate-exit.

**Caps:**
- at most 6 automatic replacements per proxy per hour;
- at most 10% of a network's unique-exit proxies replaced per hour, so a regional outage doesn't churn the whole set.

A proxy over its cap stays `replacing`, and the reason is shown.

**The trade-off.** Scraping wants both a stable IP per proxy and availability. `auto`, the default, prefers availability: IPs change only on failure. `manual` prefers stability: the IP never changes unless asked. Stickiness matters for sites that bind sessions to IPs; most scraping tolerates a replacement.

## 7. Plans and pricing (Design)

- Pro only.
- A unique-exit proxy counts against the same caps as any hosted proxy: 1,000 active and 20 a minute.
- No override is needed. engageub's 1,000 unique IPs fit the Pro cap.
- **Pricing is an open question (Q2):** included in Pro, or a premium tier. Each pinned exit concentrates traffic on one provider, and supply is finite per country (§4).

## 8. CPU and scale

**Fewer exits per proxy.** A unique-exit proxy holds one exit instead of up to 6 quality exits plus 1-2 speed exits (`connect/ip_remote_multi_client.go:226-239`). For engageub, 1,000 pinned proxies would mean about 1,000 provider window clients and contracts in flight. Today it has 41,213 open contracts across 16,343 providers and 17,324 window clients (§1), so about 40 times fewer.

**Fewer evaluations.** Hosted quality windows build about 48 candidates a second fleet-wide, and each costs a find, a mint, a TLS dial and an evaluation ping (fork CPU1, 11-05). 56% of hosted quality candidates fail their first ping (fork R3, 11-05). A fixed window evaluates one destination.

**Allocator load:**
- one insert per assignment, plus one per replacement, which the caps bound;
- one confirm update per reconnect and every 30 minutes per proxy: for 1,000 proxies, about 33 updates a minute;
- the egress-key columns ride the existing location ingest write.

**WireGuard and devices:** unchanged in count. One device per active proxy, and one WireGuard peer per proxy if WG is enabled. CPU1's active-only restore applies.

## 9. Abuse and policy (Design)

- **Concentration.** All of a pinned proxy's scraping leaves through one residential IP. There is no per-provider or per-plan rate limit in connect or the sdk today (fork R3, 11-05). Add a per-pinned-provider bandwidth share: Design, at most half the provider's estimated throughput (`find-providers2`'s `estimated_bytes_per_second`, `connect/api/bringyour.yml:13067`). Also add the K-pins-per-provider cap (§4).
- **Complaints.** An abuse report about an exit IP maps to the assignment rows of the time, so we can find the network. The assignment table is the audit trail. Retain released rows for 90 days.
- **Sanctioned regions.** A location constraint cannot name a country the platform does not offer. Existing location eligibility applies unchanged.
- **Terms.** Scraping on unique exits needs an acceptable-use statement in the terms (open question Q7).

## 10. Observability (Design)

**Metrics:**
- `urnetwork_proxy_unique_exit_assignments{state}`;
- `…_allocations_total{result=assigned|no_supply|collision_retry}`;
- `…_replacements_total{reason}`;
- `…_confirm_total{result=match|changed_unique|changed_collision}`;
- `…_supply_keys{country}`: distinct healthy egress keys;
- an assignment-age histogram.

**Audit:** a taskworker job asserts no two live assignments in a network share a key. The constraint makes that impossible, so a hit means a bug. Each run it also re-confirms a 1% sample.

**Monitor signals:**
- supply below 2× live demand in a country;
- a replacement storm, over 5% an hour fleet-wide;
- confirm collisions over 1%.

## 11. Rollout and tests

**Phase 0 (now, no code): engageub with existing pieces.**
1. They remove proxies until they're under the cap (ur.io, or `/network/remove-client`, `connect/api/bringyour.yml:1747`). Existing proxies keep working until removed.
2. They page `/network/find-providers2` with `specs` (a country `location_id` or `best_available`, `ProviderSpec` at `connect/api/bringyour.yml:12912`), `count` and growing `exclude_client_ids` (`:2786-2800`, `:12928-12975`).
3. They create one proxy per returned `client_id` with `proxy_config.initial_device_state.location.connect_location_id.client_id`. The create rate is 20 a minute, so 1,000 takes about 50 minutes.
4. They check each proxy's exit IP once with an IP echo, and replace any duplicates, since farms share IPs (§1).
5. When a provider goes offline, its proxy has no exit. They replace it with another `client_id`.

**Before telling them:** create one pinned proxy on a test network and confirm its exit IP is stable across requests. That path (hosted device + client-id location to a public provider) is supported by the code read in §5 but has not been exercised in production.

**Phase 0 test, done 2026-11-05 19:12-19:19Z (fact).** The test used a throwaway network from the seedphrase sign-up path (`/auth/network-create` with only `terms`; free plan). It created three hosted proxies, made 30 requests each to an IP echo about 12 s apart, then removed all of it (three remove-clients, `/auth/network-delete` HTTP 200).

| Proxy | Location | Requests | Distinct exit IPs |
|---|---|---|---|
| A | pinned to provider `01a199c3-…` via `connect_location_id.client_id` | 30 of 30 ok, HTTPS and SOCKS5 alternating | 1 (a 67.173.122.0/24 address) |
| B | pinned to provider `01a15e37-…` | 30 of 30 ok, HTTPS | 1 (a 96.27.49.0/24 address); none shared with A |
| C | `best_available`, unpinned | 30 of 30 ok, HTTPS | 2 (a 68.106.49.0/24 address, then from request 14 a 76.246.236.0/24 address) |

- Pinning works for a free network today; `unique_exit` itself will be Pro only (§7).
- Both pinned proxies held their single exit through the run. The unpinned proxy changed exits mid-run, which is the behavior §1 describes.
- On this result, the 8205 reply went to engageub at 19:21Z with the phase 0 recipe and notice that a `unique_exit` setting is being built, with no date.

**Phase 1 (server):**
- the egress-key columns at ingest;
- `proxy_exit_assignment`, the allocator and the option;
- device pinning, confirm and replacement;
- the rotate route.

Tests, deterministic per connect/CODESTYLE.md "Bug fixes" and "Tunables live in settings":
- **The cause, pinned:** a network's N auto-window proxies over a fixture pool with shared-IP providers produce overlapping exit keys. The same pool with `unique_exit` produces N distinct keys.
- **Uniqueness under concurrency:** many parallel allocations for one network over a pool with duplicate keys never commit two live assignments with one key. This is a DB test on the attested env.
- **Fairness cap K.** Location filter. `no_unique_supply` refuses without a cap or rate count.
- **Replacement, with an injected clock:** grace, the per-proxy and per-network caps, the 24 h exclusion, and `manual` mode holding.
- **Confirm:** a match activates, a changed-unique key updates, a changed-collision key replaces.
- **A device test:** a hosted device with a client-id location builds exactly one fixed window client.

**Phase 2:** the ur.io UI, the guide (English and zh) and the KB answer. Run mmm's non-browser docs and locale tests, and the support KB golden tests.

**Phase 3:** metrics, the audit job and monitor signals. Run the `./grafana/` metric-coverage test and the monitor alert-class tests.

## 12. Open questions for the owner

**Decided (owner 2026-11-05): accepted as recommended.** All eight below stand as the decisions. The owner also said "go ahead and fully build the unique exit feature" (phases 1-3).

| # | Question | Recommendation |
|---|---|---|
| Q1 | Expose each proxy's exit IP in the API? | No. Country, region and city plus an opaque `exit_id`; the IP is visible to the customer through an echo anyway. |
| Q2 | Price: included in Pro, or premium? | Included in Pro within the 1,000 cap at first; revisit with usage and supply data. |
| Q3 | Uniqueness scope | Exact IPv4 and IPv6 /64 by default; subnet and ASN modes later, if asked. |
| Q4 | Unique-exit pins per provider across networks (K) | 3, a setting; lower it if complaints concentrate. |
| Q5 | Replacement default | `auto`, with a 120 s grace, 6 an hour per proxy, 10% an hour per network. |
| Q6 | Proxy-miner slots as unique exits | Eligible only once their egress key is stable across at least 2 probes. |
| Q7 | Scraping policy and per-provider load | A per-pinned-provider bandwidth share of at most half its estimated throughput, plus an acceptable-use update to the terms. |
| Q8 | Do phase 0 for engageub now? | Yes, after one test proxy confirms the pinned exit is stable. |

## 13. Implementation (phase 1, built 2026-11-05)

Server branch `feat/unique-exit`, connect branch `feat/unique-exit-spec` (the API spec), both merged to main. Phase 1 is complete; the deferred items are at the end.

### 13.1 The API, final names

**`/network/auth-client`, `proxy_config`:**
- `unique_exit` (boolean): Pro only.
- `unique_exit_replace` (string): `auto` (default) or `manual`.
- The location constraint is the existing `initial_device_state` `location` or `country_code`. A location naming one provider (`connect_location_id.client_id`) is refused.

**`proxy_config_result.exit`, and `clients[].exit` on `/network/proxies`** (item level, beside `proxy_client`), a `ProxyExit`:

| Field | Meaning |
|---|---|
| `exit_id` | udid, opaque: the assignment id, new on every replacement or rotate; absent while the proxy has no exit |
| `country_code`, `region`, `city` | the provider's egress location |
| `exit_since` | when the proxy moved to this exit (RFC 3339) |
| `state` | `assigned`, `replacing`, or `no_supply` |
| `state_reason` | with `replacing` or `no_supply`: `offline`, `unreachable`, `verdict`, `collision`, `cap`, `manual`, or `no_supply` |
| `replace_mode` | `auto` or `manual` |

**Create refusals** (HTTP 200, `error.message`, nothing created). They are decided before the create is counted, so none spends the active proxy cap or the create rate:
- not Pro: `error.upgrade_required`, "Unique exit IPs are a UR Pro feature. Upgrade to UR Pro to create unique-exit proxies." With x402 enabled this answers 402 with the Pro terms, like the other `upgrade_required` refusals.
- a client-id location: "A unique exit picks its own provider: use a country, region, city, group or best available location."
- a bad `unique_exit_replace`: `unique_exit_replace must be "auto" or "manual".`
- no supply: `error.no_unique_supply`, "No unique exit IP is available in this location right now. Try another location, or best available."
- The cap and the rate refuse with their existing messages; a unique-exit proxy is a hosted proxy.

**`POST /network/proxy/rotate-exit`** `{client_id}` returns `{exit}` or `{error: {message, no_unique_supply?, rate_limited?}}`:
- Network access only (the network JWT or an API key): a client JWT gets the 403 `ClientJwtRefused`, like `/network/proxies`.
- 1 a minute and 60 a day per proxy, failing open. Only rotations count. A rotate reserves the minute's slot (`upe-rot-<proxy>-<minute>`, SET NX) and one of the day's rotations (`upe-rotd-<proxy>-<day>`, taken by a script only below the limit) before it moves the proxy, and gives both back when the proxy does not move. A refused call, rate limited or without supply, spends nothing, and concurrent rotates of one proxy move it once. The minute is a fixed UTC bucket, so two rotations a second apart across a bucket boundary both go.
- It skips the replacement caps and manual mode. Without supply, the proxy keeps its current exit.
- "Not a unique-exit proxy of this network." for any other `client_id`, including a removed proxy. remove-client only deactivates the client, and its exit stays live until the sweeper releases it (§13.3). The move's transaction share-locks the proxy's client row and requires it active, so a removal that commits during a rotate or a sweep leaves no new exit and no repinned device config.

### 13.2 Schema

The migrations are appended at the end of `db_migrations.go`, all additive (`db_proxy_unique_exit_schema.go`):
- `provider_egress_location` gains `egress_key bytea`, `egress_key_observed_at` and `egress_key_stable_count`. The ADD COLUMN bounds its lock wait at 2 s.
- **`proxy_unique_exit`**: one row per unique-exit proxy. It holds the replace mode, the location as provider specs, and the customer-facing state.
- **`proxy_exit_assignment`**: as §4, plus `location_id`, `country_code`, `estimated_bytes_per_second`, `failing_since` and `failing_reason`. Indexes:
  - unique `(network_id, egress_key)` over `pending` and `active`;
  - unique `(proxy_client_id)` over `pending`, `active` and `replacing`;
  - provider, `assigned_at` and released-row indexes.
- Released rows are kept 90 days, deleted 5,000 per sweep.

### 13.3 How it works, and where it differs from the design

**The key.** HMAC-SHA256 under vault `verify.yml` `egress_hash_key`, in its own domains:
- `urnetwork/unique-exit-ipv4/v1`: the exact address;
- `urnetwork/unique-exit-ipv6-64/v1`: the /64.

Without that key it falls back to the client-IP pepper. The egress ingest (`controller/provider_egress_location_controller.go`) stores the key. The stable count goes up on the same key and resets on a new one, and an older observation never overwrites a newer one.

**The allocator** (`model/proxy_unique_exit_model.go`):
- Candidates come from find-providers2's `quality` bucket under the location specs, with the network's live providers excluded.
- A candidate needs a key no older than 24 h. A slot-like provider needs a key stable across 2 probes; slot-like means its connection address is shared by 10 or more connected clients.
- Stable keys rank first.
- The pin cap is K = 3 live pins per provider, across networks.

**Locking differs from the design.** Every writer of a live key try-locks that `(network, key)`, and a pin also try-locks its provider. These are transaction advisory locks, tried and never waited for:
- A main-pool transaction may wait at most `PgLockTimeout` (2 s) for any lock.
- A burst of creates for one network must neither queue nor deadlock.

A busy candidate is skipped. A busy key update or confirm waits for the next pass. The partial unique index is the backstop.

**The supply check runs before the create is counted.**
- The create plan (Pro, specs, a draw of 16) runs before the rate counter.
- In the create transaction the assignment is inserted before any client rows.
- If a concurrent create took every candidate, the transaction ends with `no_unique_supply` and nothing written.

**The device** (`proxy/proxy_unique_exit.go`):
- A unique-exit proxy is created already pinned: `initial_device_state.location` is `connect_location_id.client_id` of its exit, a fixed window of one.
- It polls its assignment every 15 s (jittered), and repins through `SetConnectLocation` only when the provider changes.
- It confirms on every new assignment, and every 30 minutes after: a GET of `/my-ip-info` through its own tunnel, with the key computed in process.
  - A match activates the exit.
  - A changed key that is still unique updates the assignment.
  - A changed key that collides is replaced without a grace.

**Failure detection differs from the design.** It does not hook window-client conviction. It uses:
- the provider liveness lease (offline), with the 120 s grace;
- 3 consecutive failed confirms (unreachable), with the grace;
- find-providers2's hard exclusions (verdict) and key collisions, with no grace.

**The share.** Each pinned proxy is held to half its provider's estimated bytes per second. The token bucket's burst is max(64 KiB, rate/4).
- HTTP and SOCKS connections are paced, and nothing is dropped. With no rate yet (or no estimate) the share is unlimited, and a write goes out whole.
- The device reports an unreachable exit at each 3 failed confirms, not on every poll.
- WireGuard packets over the share are dropped and counted. A batch is policed all or nothing, so the batch contract holds.

**The sweeper** (taskworker `sweep_proxy_exits`, every 30 s):
- releases removed proxies;
- replaces on verdicts and collisions at once;
- updates or replaces on key changes;
- marks offline exits and replaces them after the grace;
- clears recovered exits;
- retries held proxies in `auto` mode, each at most once per 5 minutes (`HeldRetryInterval`), since a retry may draw from find-providers2;
- enforces 6 replacements an hour per proxy and max(1, ceil(10% of the network's unique-exit proxies)) an hour per network, over failure releases;
- excludes released providers for 24 h;
- in `manual` mode holds the proxy as `replacing`, reason `manual`.

Each assignment is swept in isolation, so one failure doesn't stop the pass.

**Tunables.** All of them are in `UniqueExitSettings` (`DefaultUniqueExitSettings`).

### 13.4 Observability

Metrics:
- `urnetwork_proxy_unique_exit_assignments{state}`;
- `…_allocations_total{result}`;
- `…_replacements_total{reason}`;
- `…_confirm_total{result=match|changed_unique|changed_collision|stale}`;
- `…_supply_keys{country}`;
- `…_assignment_lifetime_seconds`;
- `…_share_policed_packets_total{direction}`;
- the snapshot timestamps `…_sweep_snapshot_timestamp_seconds` and `…_supply_snapshot_timestamp_seconds`, so the dashboards read only the replica whose gauges are fresh.

Panels:
- `proxy.json` shows confirms and share policing, scoped to the proxy service.
- `taskworker.json` shows assignments, allocations, replacements, supply, lifetime and snapshot age.

No monitor alert class was added. The monitor's migrations signal has a schema contract for each of the eleven migrations (`monitor/migration_proxy_unique_exit.go`): columns, primary keys and complete index definitions, versioned by position.

### 13.5 Deploy order

1. `db migrate` first, from the local main working copy.
2. api and alt.
3. proxy.
4. taskworker.
5. grafana, with the dashboards.

The api refuses nothing new until it is deployed. A proxy built without the feature ignores `unique_exit` in its device config, so an api deployed ahead of the proxy creates pinned proxies whose confirm and share wait for the proxy roll. A taskworker roll before the Nov 6 close needs sn-81's sets on that commit first.

### 13.6 Deferred

- monitor signals (§10: supply below 2× demand, replacement storms, confirm collisions over 1%);
- the audit job and the 1% re-confirm sample;
- the optional webhook;
- `unique_exit_scope` subnet and ASN modes;
- the acceptable-use update to the terms.

A provider without a throughput estimate is not throttled.
