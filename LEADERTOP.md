# LEADERTOP: an SN-aligned top leaderboard

**Status:** design settled, 2026-10-29: the owner answered every design question in [§11](#11-design-questions). Nothing is implemented; implementation is the next step.

**Sources read:** server origin/main 01fa336b6 (line citations checked at cb8e668c and e5e6d4c71, unchanged since), sn 3a8509e8, connect 47499613, mmm 1a0f07bc9, config 908942e5, subtensor v455, factury 1401d163. Paths without a repo prefix are in this repo.

## 1. Goal

The default leaderboard should tell a miner who **has not claimed a UID yet** whether claiming a top-miner UID (a Top 200 head seat on SN25) is worth it: *if I claimed now, would the validator rank me in its top 200?* Networks that have claimed a UID appear too, with their UID and the validator's actual ranking as extra metadata. (Owner, 2026-10-29.)

The board has to be conservative, because a wrong "yes" costs the miner:

- **Claiming costs a UID registration.**
- **A claimed fleet ranked 201 or lower earns nothing from either tier.** Its members leave the pool whether or not the head selects them (`controller/st_controller.go:3611-3617`).

The board is the default leaderboard in every app (ur.io, Android, Apple, Windows, Linux), as a tab named "Protocol" (Q14). This doc covers the ranking, the data and the operator API. The app design is in [apple/LEADERTOP.md](https://github.com/urnetwork/apple/blob/main/LEADERTOP.md).

## 2. Summary

- **Rank networks by a projected head score.** It is the validator's exact head formula, applied to each network as one hypothetical fleet of its eligible providers.
- **Show the cutoff and a recommendation.** The projected 200th score is the cutoff. Each network gets its margin to it and a label: likely, borderline, unlikely, or not measurable.
- **Add metadata for networks that have claimed.** Their UID and fleet, the exact replica score from verify trails, the validator's own ranking if it publishes its per-tempo head result, and on-chain incentive.
- **Serve it from per-tempo snapshots.** `GET /sn/top` is public, and `GET /sn/top/me` is the caller's detail. `GET /sn/head` is rebased on the same snapshot, so the Top 200 page and the leaderboard agree.
- **Biggest blocker: the verify cohort.** The validator can only measure providers in it, so most networks can't earn a head score today (§3.5, design question 6).

## 3. How the SN25 validator ranks miners

From sn 3a8509e8.

### 3.1 What it ranks

The validator's weight row has three kinds of recipient:

- one pool UID per operator (UR's is UID 169);
- up to 200 **head fleets**: hotkey UIDs bound on chain to client ids;
- the reserve UIDs 170 and 250.

The "top 200" is the head. The validator never ranks individual providers or networks on chain. Per-provider quality only feeds the pool scalar, which cancels out while there is one operator (`WHITEPAPER.md:1170-1173`).

### 3.2 Head evidence

- **Trails.** The validator walks `/verify` trails of depth 8, which is 7 server-assigned hops, at no more than about 30 seeds a minute (`validator/release_run.go:449-470`, `deploy/mainnet/policy-v1.yml:93`).
- **Egress hash.** Each hop's FINAL proof carries an egress hash. It is an HMAC, under the operator key `mainnet-v1`, of the **/29 (IPv4) or /48 (IPv6) prefix**, pinned once per provider per settlement epoch (`VALIDATOR.md:696-718`, `WHITEPAPER.md:1263-1268`; here `model/verify_head_model.go:116-123,210-237`).
- **What counts.** Only non-seed hops of **complete** trails, and only when two checks pass (`validator/attempt_cut_v2_head.go:78-108`, `validator/release_measurement.go:404-431`):
  - the binding recorded at the hop equals the fleet's current binding;
  - the client key matches the operator-signed key history.

### 3.3 Score and selection

- **Raw score per tempo** (360 blocks, about 72 min):

  `score(u) = Σ_h 1/claim(h)`

  - h ranges over the distinct prefix hashes fleet u showed.
  - claim(h) is the number of live bound fleets that showed h that tempo.
  - There is no minimum-assignment gate (`release_measurement.go:504-520`).
- **EMA:** `next = ¼·raw + ¾·prior`, a half-life of about 3 hours.
  - A new identity starts at its raw score.
  - A new binding generation inherits nothing.
  - A live fleet with no hops decays by ¾.
  - A fleet that isn't live gets no weight (`validator/head_ema.go:245-303`).
- **Selection:** fleets with a positive EMA, sorted descending, ties to the lower UID. The first `maximum_head_fleets` = 200 are selected (`validator/head_selection.go:23-55`, `protocol/policy.go:422-424`). Weight is proportional to the EMA, not to rank.
- **Bound members leave the pool whether or not they are selected.** So a bound fleet ranked 201 or lower earns nothing.

### 3.4 Weights

- **Split.** The head gets θ = 3/10 of the provider share and the pool 7/10. The treasury path scales the provider row to 1/10 and gives 9/10 to UIDs 170 and 250 (`release_math.go:24-82`, `validator/recycle_planner.go:314-385`).
- **Cadence.** One intent per native tempo, sent as a CRv4 timelocked commit and revealed about one tempo later.
- **What's published.** The per-tempo measurement artifact holds inputs, the head EMA transcript, and the selected and rejected UIDs. It stays private on the validator host; only its hash goes on chain (`release_measurement.go:1196-1215`).

### 3.5 Membership limits

- **Binding.** Each binding is dual-signed (client Ed25519 and hotkey sr25519) and needs a fresh mirrored manifest commitment. It starts at the next 7-day settlement epoch and lasts at most 12 epochs (`evm/src/STCoordinator.sol:725-794`, `policy-v1.yml:108-116`).
- **Cohort.** On mainnet the server draws next hops only from a **verify cohort** (`controller/verify_controller.go:286-330`, `model/verify_cohort_model.go:1-45`):
  - at most 1,500 providers per epoch and 2,000 per deployment lifetime;
  - carried members are admitted first, and membership never expires.

  A provider outside the cohort never appears on a trail, so **it can never earn a head score**. The cohort exists because the validator fails hard once its census passes 4,096 providers (sn `VERSION2.md:350`).

### 3.6 Production state on 2026-10-29

- **Validator.** UR runs the sole validator, UID 1, with the stake majority.
- **The head is empty.**
  - `bindFleetMember` reverts with `StaleCommitment()` because the commitment oracle isn't deployed (sn `mainnet/MAINNET.md:65-72`).
  - Production fleet commands need a mainnet runtime authority that hasn't shipped (`miner/FLEET-MAINNET-RUNTIME.md:1-16`).
- **Weights today** are pool 1/10 and reserves 9/20 each.
- **Support policy:** don't register for Top 200 until we announce that it works (factury `support/notes/policies.md:48`).

## 4. What the operator ranks today

| Board | Route | Ranks by | Window | Notes |
|---|---|---|---|---|
| Data (the default tab in every app) | `POST /stats/leaderboard` (auth) | payout MiB per network, top 100 (`model/leaderboard_model.go:35-73`) | the 4 newest subsidy epochs | Frozen: subsidy epochs stopped at the 2026-10-06 cutoff. `Rank`/`Total` are never filled, so ur.io's rank row never renders. |
| Own rank | `GET /network/ranking` | the same frozen bytes | the same | Feeds the epoch earnings email. |
| Points | `POST /stats/points-leaderboard` (public) | all-time points: payout, reliability, Seeker ×2, referrals | all time; weekly blocks | Hourly snapshots and keyset paging. Anonymous rows still return `network_id`. |
| Top 200 estimate | `GET /sn/head` (client auth) | Σ over a network's verify-eligible clients' live egress hashes of 1/(networks sharing it) (`controller/sn_earnings_controller.go:596-646`) | 10-minute snapshot | Uses the exact IPv4 address or IPv6 /64, not /29 or /48 (`model/verify_head_model.go:44-90`). Competition ranks, so ties put more than 200 networks "inside". Bound status comes from a dead legacy table. |

Clients default to Data: ur.io `Leaderboard.jsx:94`, Android `LeaderboardScreen.kt:96`, Apple `LeaderboardView.swift:37`.

**Gaps between the operator's rank and the validator's:**

| | Validator | Operator today |
|---|---|---|
| Measure | split-adjusted /29 and /48 prefix breadth | frozen bytes, all-time points, or exact-IP breadth |
| Unit | fleet (hotkey, fleet_id, generation, UID) | network |
| Population | bound fleets whose clients are in the cohort | everyone |
| Evidence | non-seed hops of complete trails, binding and key checked | any live lease (`/sn/head`), or none |
| Split | among live bound fleets, per tempo | among networks sharing an exact address, which almost never happens |
| Window | EMA, ¼ per tempo | 10-minute snapshot, all time, or frozen |
| Cutoff | 200, ties to the lower UID | competition ranks |
| Ground truth | — | none stored: no weights, incentive, UID map or validator result |

## 5. Proposed ranking

### 5.1 Unit

**The network.** Each network is scored as one hypothetical fleet made of its eligible providers. That is the choice an unclaimed miner faces: bind my devices as one fleet.

A claimed network also gets one or more **fleet rows**, keyed like the validator's: (fleet_id, hotkey, generation, UID). A network can have several fleets, and a fleet can bind clients of several networks.

### 5.2 Eligible providers

Clients in the verify-eligible set: egress attributed to the client alone, with a provide mode (`model/verify_model.go:638-675`). These are the clients the server can draw as trail hops.

Each row also carries:

- the client's **cohort** status (measured or not);
- its **gates**: key history present, location valid, not ARIN-risk excluded.

### 5.3 Evidence

- **Prefix hash.** For every eligible provider, compute its /29 (IPv4) or /48 (IPv6) prefix hash and pin it per settlement epoch, exactly as the trail stamp does (`model/verify_head_model.go:116-123,210-237`).
- **Presence.** For each tempo, presence(c) in [0, 1] is the fraction of the tempo that provider c was connected and verify-eligible. It comes from the per-minute reliability rows or the verify lease history.
- **Trails win.** For cohort members with complete trails in the tempo, the observed prefix set replaces the projection for that tempo.

### 5.4 Projected raw score, per tempo

`raw(n) = Σ_h p_n(h) / claim(h)`

- p_n(h) is the highest presence among network n's providers in prefix h.
- claim(h) is the number of ranked units (networks and claimed fleets) with presence of at least ½ in h, and at least 1. This assumes everyone sharing a prefix claims, which is the conservative case (design question 4).

### 5.5 Smoothing, rank and cutoff

- **EMA:** the validator's: ¼ per tempo; a new unit starts at its raw score; a unit with no presence decays by ¾.
- **Rank:** a total order by EMA, descending.
  - Network ties go to the older network.
  - Fleet ties go to the lower UID, as at the validator.
- **Cutoff:** the EMA of the 200th unit.
- **Margin:** (EMA − cutoff) / cutoff.

### 5.6 Recommendation label

| Label | When |
|---|---|
| **likely** | inside the cutoff by at least the margin M, and measurable (in the cohort) in at least 3 of the last 4 tempos |
| **borderline** | within ±M of the cutoff |
| **unlikely** | below the cutoff by more than M |
| **not measurable** | not in the cohort, or not verify-eligible, so the validator can't score it whatever the projection says |

The first value of M is design question 13. While fleet binding is blocked on mainnet, every response says so (§3.6).

**What the API serves for the apps (owner, 2026-10-29):**
- The top 400, so networks just outside the 200 line can track their progress.
- Each network's daily rank history.
- A claim-prompt flag for measurable networks projected inside the top 200.

The app presentation (the Protocol tab, the 200 line, the claim prompt on Earnings, progress charts, banners and states) is in [apple/LEADERTOP.md](https://github.com/urnetwork/apple/blob/main/LEADERTOP.md).

### 5.7 Claimed networks: metadata

- **Bindings:** a live sync of the coordinator's FleetBound, FleetBindingRevoked and FleetBindingCleaned events.
  - Today FleetBound reaches `st_event` only during v2 epoch builds, and revocations aren't mirrored (`controller/st_head_exclusion.go:163`).
  - Rows are joined to clients and networks through `network_client` (`model/sn_earnings_model.go:191-216`).
- **Validity:** the binding window (valid_from, valid_to) and the generation; whether the hotkey still holds the UID (`Keys(25, uid)` equals the hotkey, joined on hotkey and `BlockAtRegistration`); whether the key matches.
- **Replica score:** see §5.8.
- **The validator's own result** (§6.1): its score, its rank among eligible fleets, and whether it was selected.
- **On-chain incentive** (§6.2), with how fresh it is.

### 5.8 Exact replica for claimed fleets

- **Inputs:** the trail hops of each tempo, from the indexed hop table (§8).
- **Tempo boundaries:** the validator's own cut manifests, which it uploads to `/sn/attempt-artifact`.
- **Arithmetic:** the validator's exact integer arithmetic, imported from sn as a package (design question 8). EMA state is persisted.
- **Check:** the result is compared with the validator's published head result each tempo (§6.3).

## 6. Ground truth: optional data

### 6.1 The validator's head result

- **What exists.** UID 1's intent records `EligibleHeadUIDs`, `EligibleHeadScores` and `SelectedHeadUIDs` each tempo (sn `validator/intent.go:46-88`). They stay in the validator's private state on its host.
- **Proposal.** The validator uploads a signed per-tempo **head summary** to the operator: fleet keys, exact scores, ranks and the selection, with no private inputs. It would use a new `/sn` endpoint like the attempt-artifact uploads, and needs an sn change.
- **What it gives.** The board can then show the validator's actual ranking for claimed fleets.

### 6.2 On-chain incentive

- **Timing.** Read once per tempo, after `SubnetEpochIndex` advances and that block is finalized.
- **What to read:**
  - the selective metagraph: `Incentive`, `Keys`, `BlockAtRegistration`, `LastUpdate`, `ValidatorPermit`, `ValidatorTrust`;
  - the `Weights` rows of permitted validators.
- **Reader.** Reuse the subnet monitor's reader (`monitor/subnet_weight_reader.go:89-290`) against the snow archive. It costs under 100 KB per tempo.
- **Which field.** `Incentive` is Yuma's ranking output; rank and trust are no longer computed. While UID 1 holds more than κ of the stake, the set of UIDs earning incentive equals UID 1's row.
- **When to mark it stale:**
  - the row is the stopgap fallback (169:14563, 170:65535, 250:65535);
  - UID 1's `LastUpdate` is outside the activity cutoff;
  - `ValidatorTrust` is well below 1.
- **Lag.** Incentive trails the commit by 1–2 tempos.

### 6.3 Agreement monitoring

Each tempo, record:

- the replica against the validator's head summary. Exact equality is expected for claimed fleets; any difference alerts.
- the projected top 200 against the realized selection (precision at 200), once fleets bind.
- the selected UIDs against those earning incentive on chain.

These go to Prometheus with alerts, and the measured precision is shown on the board (design question 17).

## 7. API

### `GET /sn/top`

Public, with optional auth. Rate-limited like the points leaderboard.

**Query:**
- `limit`: 50 by default, at most 200 per page. Paging covers the top 400 (Q5), plus the caller's own `me` row wherever it ranks.
- `cursor`: keyset paging pinned to one snapshot.
- `filter`: `all`, `unclaimed` or `claimed`.
- `snapshot`: optional, to pin a snapshot.

**Response:**

```json
{
  "snapshot": {
    "id": "…", "tempo_index": 0, "block": 0, "computed_at": "…",
    "settlement_epoch": 0, "head_seats": 200, "claimed_fleets": 0, "open_seats": 200,
    "projected_cutoff": 0.0, "claimed_cutoff": null,
    "binding_open": false, "binding_note": "Top 200 binding isn't open on mainnet yet",
    "cohort": { "measured_providers": 0, "capacity": 1500 },
    "validator_summary_at": null, "incentive_at": null,
    "agreement": { "precision_at_200": null }
  },
  "rows": [
    {
      "position": 1,
      "name": "…", "tag": "…",
      "projected_score": 0.0, "raw_last_tempo": 0.0, "margin": 0.0, "trend": 0,
      "prefixes": 0, "shared_prefixes": 0, "presence": 0.0,
      "label": "likely",
      "gates": { "verify_eligible": true, "measured": true },
      "claimed": {
        "fleets": [
          { "uid": 0, "generation": 0, "valid_to_epoch": 0,
            "replica_score": 0.0, "validator_score": 0.0, "validator_rank": 0, "selected": true,
            "incentive": 0.0 }
        ]
      }
    }
  ],
  "me": { "…": "the caller's row when signed in" },
  "next": "cursor"
}
```

- `name` is filled only for networks that opted in.
- `claimed` is null for networks that haven't claimed.

### `GET /sn/top/me`

Client auth. Returns the caller's network in detail:

- the score broken down per prefix: presence and how many other units share it, as counts only, never hashes or IPs;
- the gates and the label;
- what would raise the score, such as more distinct prefixes;
- the solo upside: what the score would be if nobody else sharing its prefixes claimed;
- its claimed fleets with their validity and validator data.

### `GET /sn/head`

The route stays. It is computed from the same snapshot, so the ur.io Top 200 page and the apps agree with the board.

## 8. Pipeline and storage

**Tables:**

| Table | Holds |
|---|---|
| `sn_top_snapshot` | id, tempo index, block, computed_at, settlement epoch, cutoffs, counts, flags |
| `sn_top_network` | per network per snapshot: position, EMA, raw, prefixes, shared, presence, margin, label, gates |
| `sn_top_fleet` | per fleet per snapshot: fleet key, UID, hotkey, networks, replica EMA, validator score, rank and selection, incentive |
| `sn_top_ema_state` | each unit's current EMA and the tempo it was last folded |
| `sn_trail_hop` | one row per trail hop: tempo index, attempt id, hop index, client_id, network_id, prefix hash, complete. Written when a trail completes. `verify_trail` keeps hops as JSON with no time index. |

**Task:** one taskworker run per tempo, after the epoch boundary is finalized:
1. Pin the settlement epoch's prefix hashes.
2. Compute presence for the tempo.
3. Fold in complete-trail hops.
4. Compute the claim counts, raw scores and EMA.
5. Rank and label.
6. Attach binding data, the validator summary and incentive.
7. Write the snapshot and agreement metrics.

**Retention:** 7 days of per-tempo snapshots (about 140), plus 90 days of each network's daily rank and score in `sn_top_network_daily` for progress charts (Q11).

**Binding sync:** a live sync of FleetBound, revocation and cleanup events, replacing the epoch-build-only path.

**Cost:**
- Presence comes from per-minute reliability rows already rolled up, so there's no new raw scan.
- The claim map is a per-tempo aggregate over verify-eligible clients only.
- The chain read is under 100 KB per tempo.

## 9. Privacy

- **Public and anonymous by default.** Rows show the network's tag or emoji and no `network_id`. The points leaderboard's anonymous rows should drop `network_id` too.
- **Names** appear only for networks that opted in. Whether that's the existing `points_leaderboard_public` flag or a new one is design question 12.
- **Claimed fleets.** UIDs and hotkeys are public on chain, but putting one next to a network name reveals the network-to-hotkey link. So an anonymous claimed row shows "UID n" without the name.
- **Never exposed:** prefix hashes or IPs; only counts.

## 10. Rollout

1. The owner answers §11.
2. **sn changes,** coordinated with sn-55:
   - the validator's head summary upload (design question 9);
   - the cohort policy change (design question 6).
3. **Server:**
   - the hop table, the snapshot task and the API, behind a flag;
   - a backtest over past tempos;
   - `/sn/head` rebased on the snapshot.
4. **Apps:** build the Protocol tab in ur.io, Android, Apple, Windows and Linux as [apple/LEADERTOP.md](https://github.com/urnetwork/apple/blob/main/LEADERTOP.md) describes.
5. **When fleet binding opens on mainnet:** turn on the claimed-fleet metadata and the agreement alerts.

## 11. Design questions

Each question lists the options with a recommendation, then the decision. The owner answers before any implementation. How the apps present the decisions (Q5, Q11, Q12, Q13, Q14, Q16, Q17) is in [apple/LEADERTOP.md](https://github.com/urnetwork/apple/blob/main/LEADERTOP.md).

**Q1. Audience and purpose.**
**Decision (owner, 2026-10-29):** guide miners who haven't claimed a UID on whether to try for a top-miner UID spot. Show networks that have claimed a UID, with the validator ranking when available, as extra metadata.

**Q2. Ranking unit.**
- **Recommended:** the network, each scored as one hypothetical fleet, with claimed fleets as metadata.
- Alternative: rank fleets and clients separately.

**Decision (owner, 2026-10-29):** networks. Each network is scored as one hypothetical fleet of its eligible providers, and a claimed network also shows its fleet rows (UID, generation, validator score) as metadata.

**Q3. Evidence for unclaimed networks.**
- **(a) Recommended:** project from every eligible provider's pinned /29 or /48 prefix, weighted by presence and calibrated with complete trails.
- (b) Trails only: exact, but only cohort members have any, so most networks would be blank.
- (c) Live leases, as `/sn/head` does now: the wrong granularity and no smoothing.

**Decision (owner, 2026-10-29):** (a). Project from every eligible provider's pinned /29 or /48 prefix, weighted by presence. Where complete trails exist for a tempo they replace the projection, and they're used to calibrate it.

**Q4. Split rule for shared prefixes.**
- **(a) Recommended:** share each prefix's credit among every network and claimed fleet present in it. This is conservative: it assumes they all claim.
- (b) Share only among claimed fleets, as the validator does today. This is optimistic while few have claimed.
- (c) Rank by (a) and show (b) as "if you claimed alone today" in `/sn/top/me`.

The recommendation is (a) for the ranking plus (c) in `/me`.

**Decision (owner, 2026-10-29):** (a) for the ranking, splitting among every network and claimed fleet present in the prefix, plus (c): `/sn/top/me` also shows the "if you claimed alone today" score.

**Q5. Cutoff shown.**
- **Recommended:** the projected 200th score across all units, the current floor among claimed fleets, and the number of open seats while fewer than 200 have claimed.

**Decision (owner, 2026-10-29):** margins and labels use the projected 200th score across all units. The board also shows the current floor among claimed fleets, and the number of open seats while fewer than 200 have claimed.

**Added (owner, 2026-10-29):**
- **Show the top 400,** with the 200 line marked, so people can see and track their progress into the top 200.
- **Claim prompt:** a network projected inside the top 200 is directed to claim on the Earnings page.

**Q6. Cohort admission.** This is the biggest blocker.
- **(a) Recommended:** members of claimed fleets get priority admission to the cohort, capped per fleet, with stale members rotated out. This keeps the validator under its 4,096-provider census limit, and claiming then leads to measurement. Needs server and sn work, coordinated with sn-55.
- (b) Leave the cohort as it is. Most networks then stay unmeasurable forever.
- (c) Enlarge the cohort. The validator's census limit blocks this.

**Decision (owner, 2026-10-29):** (a). Members of claimed fleets get priority admission to the cohort, capped per fleet, with stale members rotated out, so the validator stays under its 4,096-provider census limit and claiming leads to measurement. Server and sn work, coordinated with sn-55.

**Q7. Networks the validator can't measure.**
- **Recommended:** rank them by projection, mark them "not measurable", and never label them "likely".
- Alternative: hide them.

**Decision (owner, 2026-10-29):** rank them by projection with a "not measurable" flag, and never label them "likely" until they can be measured.

**Q8. Exactness.**
- **Recommended:** import sn's head arithmetic as a Go package, so the replica is bit-identical for claimed fleets.
- Alternative: reimplement it in the server.

**Decision (owner, 2026-10-29):** import sn's head arithmetic (prefix split, ¼ EMA, selection) as a Go package, so the server's numbers for claimed fleets match the validator's exactly.

**Q9. Validator head summary.**
- **Recommended:** yes. UID 1 uploads its signed per-tempo head result to the operator (an sn change), so the board shows the validator's actual ranking.

**Decision (owner, 2026-10-29):** yes. UID 1 uploads a signed per-tempo head summary (fleet, exact score, rank, selected; no private inputs) to the operator, and the board shows the validator's actual ranking for claimed networks. Needs an sn change, coordinated with sn-55.

**Q10. On-chain incentive.**
- **Recommended:** yes, as metadata for claimed UIDs, with stale and fallback detection.

**Decision (owner, 2026-10-29):** yes. Claimed networks show their UID's on-chain `Incentive`, read each tempo, flagged stale on the validator's fallback row or when its weights are outside the activity cutoff.

**Q11. Cadence and history.**
- **Recommended:** a snapshot every tempo (about 72 min) with the validator's ¼ EMA, keeping 7 days, with a trend shown.

**Decision (owner, 2026-10-29):**
- A snapshot every tempo, with the validator's ¼ EMA.
- Per-tempo snapshots are kept for 7 days.
- Each network's daily rank and score are kept for 90 days, so `/sn/top/me` and the apps can chart progress toward the 200 line.

**Q12. Privacy.**
- **Recommended:** a public endpoint, anonymous by default (tag only, no `network_id`), names opt-in, and claimed rows showing "UID n" unless opted in.
- Open: reuse `points_leaderboard_public`, or add a new flag?

**Decision (owner, 2026-10-29):**
- The endpoint is public and anonymous by default: tag only, no `network_id`. The Points board's anonymous rows drop `network_id` too.
- Names appear only for networks with the existing `points_leaderboard_public` setting, so one "show my name on leaderboards" switch covers both boards.
- An anonymous claimed row shows only "UID n".

**Q13. Recommendation labels.**
- **Recommended:** likely, borderline, unlikely and not measurable, with M = 20% and "likely" also requiring measurability in at least 3 of the last 4 tempos. Every view carries the warning that a claimed fleet ranked 201 or lower earns nothing.

**Decision (owner, 2026-10-29):**
- **Labels:** likely, borderline, unlikely and not measurable, with M = 20%. "Likely" also requires measurability in at least 3 of the last 4 tempos.
- **Claim prompt:** every measurable network projected inside the top 200 is directed to claim on the Earnings page. Borderline networks get it with an extra "close to the line" warning.
- **Warning:** every claim prompt says that a claimed fleet ranked 201 or lower earns nothing.

**Q14. App tabs.**
- **Recommended:** SN Top is the default tab, Points second, and the frozen Data tab is retired.

**Decision (owner, 2026-10-29):** the SN top board is a tab named **"Protocol"**, first and the default, in every app. **"Points"** is the second tab and **"Data"** the third.

**Q15. Endpoint names.**
- **Recommended:** `GET /sn/top` and `GET /sn/top/me`, with `/sn/head` rebased on the same snapshot.

**Decision (owner, 2026-10-29):** `GET /sn/top` (public) and `GET /sn/top/me` (client auth). `GET /sn/head` is computed from the same snapshot, so the Top 200 page, the Earnings page and the Protocol tab agree.

**Q16. Launch timing.**
- **Recommended:** ship the board while binding is closed, with a "binding isn't open yet" banner and the not-measurable flags. Don't encourage registering before the announcement, per the support policy.
- Alternative: wait until binding opens.

**Decision (owner, 2026-10-29):**
- Ship the Protocol tab as soon as it's built, with a "claiming isn't open yet" banner while fleet binding is blocked on mainnet.
- While binding is closed, projected top-200 networks see a "claiming opens soon" note on Earnings instead of a live claim button, and nothing encourages registering early.
- The claim prompt goes live when binding opens.

**Q17. Precision target.**
- **Recommended:** publish the measured precision at 200 once fleets bind. Turn on the "likely" label only once it reaches at least 90%. Before then, show "estimate" and no label.

**Decision (owner, 2026-10-29):**
- Publish the measured precision at 200 on the board once fleets bind.
- The "likely" label switches on only once precision reaches at least 90%.
- Until then, rows inside 200 show "estimate" and still get the claim prompt and warning (Q13).

## 12. Risks and open issues

- **Cohort freeze.**
  - With a 2,000-provider lifetime cap and carried members first, the validator can only ever measure a fixed few thousand providers. About 700k are online.
  - Without design question 6, the board can only say "not measurable" to most miners.
- **Binding is blocked on mainnet.** The commitment oracle isn't deployed and the fleet runtime authority hasn't shipped. The board's main question can't turn into a claim until both land.
- **Possible validator bug, unverified.** The treasury path refuses the whole row when a selected head UID rounds to 0 in u16 (sn `validator/recycle_planner.go:352-358`). A bound fleet that stays in the census but isn't observed decays by ¾ each tempo and never reaches zero, so one idle fleet could stop steering within about a day. Test this before the head goes live.
- **Gaming.** Networks can split or merge to change claim counts, or run Sybil networks behind shared prefixes. The board mirrors the validator's rules and doesn't add its own; unusual patterns get a flag.
- **Validator randomness.** Hop draws and trail completion are random, so a projection is an estimate. The board shows its confidence (§5.6) and the measured agreement (§6.3).
- **Plumbing defects found while researching:**
  - `/stats/leaderboard` never sets `Rank`/`Total`, so the ur.io Earnings rank row is hidden.
  - The epoch earnings email's rank is frozen.
  - Anonymous Points rows expose `network_id`.
  - FleetBound revocations aren't mirrored.
  - `verify_trail` has no time index.
