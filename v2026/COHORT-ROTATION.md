# Verify cohort stale rotation

**Status:** owner decision 2026-10-29 (LEADERTOP design question 6, part two). Merged on 2026-10-31 as f45365d84 and deployed the same day, on the owner's go: migration 902 at 06:37Z, api 2026.10.31-outerwerld-1086286270 and alt 2026.10.31-outerwerld-1086288290, then taskworker 2026.10.31-outerwerld-1086289460 at 06:49Z. The first rotation, at 06:49:57Z, seeded the census with the 2,000 active members and logged "1 stale members out for 1 waiting (census 2000, active 1999)". The active set was full, so one waiting claimed member needed one stale member rotated out; 1,999 is the active set after rotation. Admitting that member made the census 2,001, the rows in `verify_cohort_census`. Only a claimed fleet's member can grow the census past 2,000. Part one (claimed-fleet priority) is on main.

## Problem

The verify cohort (`model/verify_cohort_model.go`) bounds what the SN25 validator measures:

- 1,500 providers per settlement epoch.
- 2,000 distinct providers over the deployment's life (the lifetime set).

The validator never forgets a provider it has scored, so its census is every provider ever admitted. The lifetime cap keeps that census at 2,000, under half of its signed bound of 4,096. The other half covers a Redis data loss, which could re-admit up to another 2,000.

Once the lifetime set is full, no new provider is ever measured again. A network that claims a UID after that point has no member in the set, so it can never earn a head score: claiming doesn't lead to measurement (LEADERTOP.md §3.5, §12).

## Design

The lifetime set becomes the **active** set: the providers drawn ahead of new ones, still capped at 2,000. A new monotone **census** set holds every provider ever admitted, which is what the validator carries. Rotation removes stale members from the active set only, never from the census.

**Admission** (one Redis script, so no race can pass a cap):
- The per-epoch size and the active cap are unchanged.
- An unclaimed new provider counts against the census within the old lifetime limit. Unclaimed providers can never grow the census past 2,000, exactly as before rotation.
- A claimed fleet's member (from the priority set of part one) may grow the census up to the **census limit**, 3,072: three quarters of the validator's 4,096.
- A census member that rotated out returns at no census cost. It is already in the validator's census, and like a carried member it skips the growth gate.

**Rotation** (`RotateVerifyCohort`, run by the taskworker after each binding sync, about every minute):
1. Make the census ready: load the durable census from Postgres into Redis, union in the active set, mirror anything Postgres lacks, then set the ready marker.
2. Count the claimed members that are eligible and not active (the demand).
3. Rotate out at most `demand − free active slots` stale members, and at most 64 per run.
   - The free active slots are the lifetime limit (2,000) less the active set before rotation. With the active set full, every waiting claimed member rotates one stale member out.
   - The log reports the active set before and after rotation.
   - A member is stale when it was not drawn in this or the previous settlement epoch, is not eligible now, and is not a claimed member.
   - Rotation frees room only for claimed members waiting for it. Freeing room eagerly would let unclaimed providers take it, and they cannot grow the census anyway.

**Durability:**
- `verify_cohort_census` (Postgres) mirrors the census. Every new census member is written through by the API when it is admitted, and rotation mirrors the Redis census each run.
- While a durable census exists but Redis has lost its ready marker, the admission script refuses every provider that is not already active. That lasts until rotation restores the census.
- A Redis data loss can therefore no longer re-admit a census. This is what lets the census limit rise above half of 4,096.

## Invariants

- The validator's census (every provider ever admitted) stays at or below the census limit of 3,072.
- Unclaimed providers alone stay at or below 2,000, as today.
- The per-epoch cap (1,500) and the active cap (2,000) are unchanged and enforced in the same script.
- Before the first rotation (the census not yet ready), admission is exactly today's.

## What changes in production

- Claimed fleets' members can be measured after the lifetime set fills. Rotation runs only for claimed members waiting for room, but the lifetime set is full, so each claimed member not yet active rotates one stale member out. The first claimed fleet did so on 2026-10-31.
- The validator's census can grow from 2,000 to at most 3,072.
  - It must stay under the validator's own per-transition limits: the settlement transition was estimated at about 414 bytes a provider against 2 MiB, about 5,000 providers (sn `VERSION2.md` §3.5).
  - sn-55 should confirm the headroom.
- Members not drawn for two settlement epochs that are offline lose their place in the active set. They keep their validator score and return at no census cost.

## Coordination

- **sn-55:** confirm that a census of up to 3,072 is safe for the production validator (its census bound, transition size, disk and memory), and agree on the merge window. Nothing in sn changes.
- **Census check (2026-10-31, against sn main `2cb50ac4`):** safe.
  - The validator unions this settlement window, every provider it ever scored and this tempo's egress providers into one census. It fails the measurement and the settlement past the signed `max_providers`, 4,096, a fixed capacity dimension (`validator/measurement_stats_v2.go:36-73`, `validator/attempt_settlement_v2_runtime.go:225-262`, `validator/production_capacity.go:85-94`).
  - Its quality EMAs are never evicted, and only server-assigned hops are scored, so the census is every provider the server ever admits: at most 3,072, which leaves 1,024 for anything the validator scored outside the cohort.
  - The transition stays near 1.3 MB of its 2 MiB. Per-epoch and per-tempo counts follow the cohort size, which rotation does not change.
  - `controller/verify_controller.go` names the 4,096 bound (`verifyValidatorMaxProviders`) and refuses a `cohort_lifetime_limit` above it.
- **Production check (2026-10-31 18:15Z, read-only, snow `/srv/sn25/sn-validator/durable/operators/no-1/stats.json`):** the live validator's census is 1,403 of 4,096. That is window 281, ema and ema_ppm 1,402 each (the same set), egress 149, with all four inside the union. The stored EMAs hold no set from outside the cohort, so a server census at its 3,072 cap stays well inside the bound. This settles the headroom question above. The sn-55 session that was to confirm it is no longer running.
- **Owner:** time the merge. With no claimed fleets it is inert, so it can merge with the first binding that opens on mainnet.

## Deploy

1. Apply the migration (a new table, `verify_cohort_census`).
   - An api deployed ahead of it admits exactly as before rotation: without the table no durable census can exist.
   - Any other failed census read fails closed without failing the draw: a ready Redis census admits as usual, and a lost one refuses new providers until a read succeeds.
2. Deploy every **api** first.
   - An api still on the old admission script checks only the active cap.
   - Such an api could give a rotated-out slot to an unclaimed provider and grow the census past 2,000.
3. Then deploy the **taskworker**.
   - Its first rotation seeds the census from the existing lifetime set and sets the ready marker, within a minute.
   - A rotation also unions the active set into the census on every run, which covers anything an older api admitted.
   - Until the first rotation, admission is exactly as before.

## Tests

`model/verify_cohort_rotation_test.go` (local test env):
- A full lifetime set refuses a claimed member until rotation frees room; claimed members then fill the census to its limit; an unclaimed provider is still refused; a rotated member returns at no census cost; every census member is durable.
- Rotation runs only while claimed members wait, at most that many, and never removes a member drawn this or last epoch, an eligible member or a claimed one.
- Free active slots count against the demand: one free slot covers one waiting member, and with the active set full one waiting member rotates one stale member out, as in the first production rotation.
- After a Redis loss of every cohort key, no new provider is admitted until rotation restores the census from Postgres, and the restored census still bounds admissions.
- Without the census table, draws admit as before rotation; any other census read failure refuses new providers without failing the draw, and is not cached.

The existing cohort tests pass unchanged.

## Verify cohort v2: the rolling census

**Status:** owner approval 2026-11-17 ~20:15Z ("Approve, ride 11-27"), design in sn `VERIFY-COHORT-V2.md` §2.4-2.6. Built on branch `feat/verify-cohort-v2-1117`; not merged or deployed.

The census above never shrinks, because the validator never forgot a provider it scored. From `VERIFY-COHORT-V2.md` on, the validator drops at each settlement fold the carried quality of every provider with no assignment in the closing window, unless nobody had one. Its census during window v is then within the providers assigned in windows v-1 and v, and the server counts a member only until the validator has folded two windows after the member's last assignment (`model/verify_cohort_rolling.go`).

**Two modes, one script.** The admission script (`model/verify_cohort_model.go`) reads the rolling state hash `{verify_cohort:<deployment>}state` itself, so the mode can't change between the read and the write. Without its `rolling` field it is exactly the legacy rule above, and the taskworker runs `RotateVerifyCohort` as before. Nothing changes until the validator reports its census.

**Starting.** The validator's head summaries carry `census_providers` from `VERIFY-COHORT-V2.md` on (sn `docs/spec/head-summary-v2.md`), stored on `sn_head_summary`. The taskworker starts the rolling census once a summary carries a census and the validator has folded a settlement window since the first such summary: that fold used the retirement rule. It records the start once in `verify_cohort_state` (summaries are kept only 7 days) and from then on runs `ExpireVerifyCohort` instead of rotation.

**Counting.** The census set holds the members still counted, mirrored in `verify_cohort_census` with each member's `last_epoch` (null for a legacy row).
- A member expires once some server epoch w with `max(last+2, E0) <= w <= v*-1` had an assignment. v* is the validator's settlement epoch in its latest signed head summary, and E0 the first window it folded with the rule. Boundary skew puts a member in validator windows `last-1` to `last+1` at most, so `last+2` is the first window that can retire it; an epoch with no assignment may have been an empty validator window, which retires nobody, and never counts.
- A legacy member (last assigned before the rolling census started) expires at the first fold with the rule, unless it was assigned since.
- A stale report (over 3 hours old) or one without a census frees nothing and admits no new provider.
- One script per batch removes the expired members and re-checks this epoch's and the next epoch's cohorts, so an admission racing the expiry is never lost. Postgres rows go after; the next run mirrors anything Postgres lacks.
- A Redis that lost the census restores it from Postgres and counts every member as assigned that epoch, since its epoch sets are gone too.

**Admitting.** Draws go claimed fleets' members (tier A, 7 a fleet as before), then one representative of each unclaimed network ranked in the board's top 400 in 3 of the last 4 folds, one per /29 or /48 prefix (tier B, `model/verify_cohort_ranked.go`, computed at each board fold), then carried members, then the uniform sample. A new member needs:
- the census under the lifetime limit (2,000) for a claimed member, or under it less the headroom for any other tier. The headroom is the claimed members waiting now plus 7 × the fleets whose bindings start at the next epoch;
- the validator's reported census plus the admissions since its last two reports under 3,584 (7/8 of its 4,096), never with the census unknown.

**Durability and limits.** No new verify.yml key: the lifetime limit bounds the rolling census, and the 3,072 claimed census limit applies only to the legacy census. The validator holds at most two windows' members, each counted under 2,000 when admitted, so 4,000 < 4,096 even if it keeps one window more than the server counts (an empty-window edge).

### Deploy

1. Apply the migration: `sn_head_summary` gains `census_providers`, `bound_clients` and `bound_clients_observed`; `verify_cohort_census` gains `last_epoch`; `verify_cohort_state` is new. Additive, under a 2 s lock wait.
2. Deploy every **api** and **alt**: they store the census from summaries and run the two-mode script. A summary with the new members is refused by an older api's decoder, so the operator goes before the validator.
3. Deploy the **taskworker**: it reads the report, writes the state, and runs rotation or expiry. A database without the migration runs the legacy census.
4. Deploy the validator with the retirement rule (sn). Its first summaries carry the census; after its first settlement fold the taskworker starts the rolling census on its own.

Rollback: the taskworker first (expiry stops and the census is monotone again), then apis and alt. If the validator goes back to a build that keeps every prior, the interlock stops admissions as its reported census climbs.

### Tests

Pure (`model/verify_cohort_rolling_rule_test.go`, `model/verify_cohort_ranked_test.go`, `controller/verify_cohort_rolling_test.go`): the expiry horizon, exhaustively over boundary skew; when the rolling census starts; tier B's selection; the headroom from bindings; the summary's census and coverage.

Test environment (`model/verify_cohort_rolling_test.go`): a full legacy census of idle members refuses a top-ranked network's representative until the rolling census starts, then frees the idle members, keeps the measured one and admits the representative (root cause); the interlock; a stale report; the headroom; an expiry racing a re-draw; a Redis restore. The existing cohort and rotation tests run unchanged.
