# Verify cohort stale rotation

**Status:** owner decision 2026-10-29 (LEADERTOP design question 6, part two). Merged on 2026-10-31 as f45365d84 and deployed the same day, on the owner's go: migration 902 at 06:37Z, api 2026.10.31-outerwerld-1086286270 and alt 2026.10.31-outerwerld-1086288290, then taskworker 2026.10.31-outerwerld-1086289460 at 06:49Z. The first rotation seeded the census at 06:49:57Z with 2,001 members and logged "1 stale members out for 1 waiting (census 2000, active 1999)". Part one (claimed-fleet priority) is on main.

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

- Claimed fleets' members can be measured after the lifetime set fills. While there are 0–3 claimed fleets this rotates nothing: rotation runs only for claimed members waiting for room.
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
- After a Redis loss of every cohort key, no new provider is admitted until rotation restores the census from Postgres, and the restored census still bounds admissions.
- Without the census table, draws admit as before rotation; any other census read failure refuses new providers without failing the draw, and is not cached.

The existing cohort tests pass unchanged.
