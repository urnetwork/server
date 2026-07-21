# API Spec ↔ Implementation Gap Report

**Spec:** `connect/api/bringyour.yml` (84 operations, 156 schemas)
**Implementation:** `server/api/api.go` + `server/api/handlers/` → `server/model`, `server/controller`
**Method:** every spec path/method extracted and normalized against every `router.NewRoute(...)`; for the 83 endpoints present in both, each handler was traced to its wrapped model/controller function and the request/response Go structs were diffed field-by-field against the spec schema (json-tag names, types, nesting, enums, and the `router.Wrap*` auth level vs spec `security`).

Type-mapping conventions treated as MATCH: `server.Id` ↔ spec `string`(uuid) (MarshalJSON emits quoted uuid, `id.go:87`); `ByteCount`/`NanoCents` = `int64` ↔ `integer`; `time.Time` ↔ `string`(datetime); `[]byte` ↔ base64 `string`; `ProvideMode` = `int` ↔ `integer`. Go can't express string enums, so enum-not-enforced is only flagged when the emitted values don't conform.

## Summary

| | Count |
|---|---|
| Endpoints in both spec and impl | 83 |
| Spec endpoints not implemented | 1 |
| Implemented routes not in spec | 22 (8 webhooks, 2 legacy aliases, 12 real client endpoints) |
| Matched endpoints that diverge | ~35 |
| — non-functional routes (stub/no-op) | 4 |
| — auth-level mismatches | 3 |
| — breaking field name/type mismatches | 5 |
| — spec field the impl drops | 2 |
| — undocumented extra fields (req/resp) | ~30 |
| Spec defects (fix the YAML) | 3+ |

---

## Part 1 — Route-level gaps

### A. Spec endpoint NOT implemented (1)
- **`POST /device/set-name`** (`DeviceSetNameArgs{device_id, device_name}` → `DeviceSetNameResult`, BearerAuth). Model logic exists but is **orphaned** — `DeviceSetName` at `model/network_client_model.go:1822-1862`, reachable by no route/handler. Distinct from the implemented `/device/set-association-name` (keys off association `code` and upserts `device_association_name`; set-name renames an owned device by `device_id`). Bonus bug in the orphaned struct: `DeviceId server.Id` is mis-tagged `json:"client_id"` (`:1823`).

### B. Implemented routes NOT in spec (22)
**Webhooks — external inbound, intentionally undocumented (8):** `POST /account/circle-wallet`, `POST /pay/circle`, `POST /pay/coinbase`, `POST /pay/play`, `POST /pay/solana`, `POST /pay/stripe`, `POST /updates/brevo`, `POST /apple/notification`.

**Legacy aliases — intentional, deprecated (2):** `GET /stats/providers-overview-last-90`, `POST /stats/provider-last-90`.

**Real client endpoints — should be added to spec (12):** `GET /auth/refresh`, `GET /connect`, `POST /connect`, `GET /key/{clientId}`, `GET /my-ip-info`, `GET /network/user`, `POST /network/user/update`, `POST /account/wallets/verify-seeker`, `POST /auth/upgrade-guest-existing`, `POST /log/{clientId}/upload`, `POST /stripe/customer-portal`, `POST /stripe/payment-intent`.

Inconsistency: `/solana/payment-intent` is speced but the parallel `/stripe/payment-intent` is not.

---

## Part 2 — Divergences on matched endpoints

### C1. Non-functional routes (route exists, does not work)
| Endpoint | Behavior | Location |
|---|---|---|
| `POST /network/create-provider-spec` | returns `"Not implemented."` | `model/network_client_location_model.go:3045` |
| `POST /device/set-provide` | returns `"Not implemented."` ("FIXME we don't support remote setting of local settings") | `model/network_client_model.go:1878` |
| `POST /network/find-providers` | no-op, always empty ("no longer supported") | `model/network_client_location_model.go:2054` |
| `POST /network/find-locations` | no-op, always empty | `model/network_client_location_model.go:2033` |

### C2. Auth-level mismatches
| Endpoint | Spec | Impl | Direction / note |
|---|---|---|---|
| `POST /referral-code/validate` | `BearerAuth` | public (`WrapWithInputNoAuth`, `account_handlers.go:20`) | impl under-protects |
| `POST /stats/leaderboard` | `BearerAuth` | public (`WrapWithInputNoAuth`, `leaderboard_handlers.go:11`) | impl under-protects |
| `POST /auth/code-create` | `security:[]` (public) | `WrapWithInputRequireAuth` (`auth_handlers.go:55`) | impl over-protects; spec description implies a session → **spec likely wrong** |

### C3. Breaking wire mismatches (client-visible name/type)
| Endpoint | Field | Spec | Impl (Go) |
|---|---|---|---|
| `POST /auth/login` (resp) | wallet | `wallet_auth` | tagged `wallet_login` (`model/auth_model.go:60`) |
| `GET /account/points` (resp) | array | `network_points` | tagged `account_points` (`controller/account_point_controller.go:9`) |
| `GET /account/points` (resp) | `event` enum | `[referral]` | emits `payout`,`payout_linked_account`,`payout_multiplier`,`payout_reliability` (`model/account_point_model.go:19-24`) → **spec enum incomplete** |
| `POST /device/create-adopt-code` (resp) | secret | `adopt_secret` | tagged `share_code` (`model/device_association_model.go:548`) |
| `POST /connect/control` (req+resp) | `pack` | `array<string>` | scalar `string` (`controller/connect_controller.go:63,67`) |

### C4. Spec declares a field the impl drops
| Endpoint | Missing in impl |
|---|---|
| `POST /network/auth-client` (req) | `derived_client_id` (absent from entire codebase; `AuthNetworkClientArgs` at `model/network_client_model.go:88`) |
| `POST /network/find-locations` (+ `/network/provider-locations`, `/network/find-provider-locations`) | `locations[].city/region/country` names (Go FIXME `network_client_location_model.go:1298`) and result `specs: []ProviderSpec` |

### C5. Undocumented extra fields (impl super-set of spec)
**Response extras**
| Endpoint | Extra fields | Location |
|---|---|---|
| `GET /subscription/balance` | `subsidy_net_revenue_nano_cents`, `purchase_token`, `paid` | `model/subscription_model.go:215-218` |
| `GET /account/payments` | `reliability_subsidy_nano_cents` | `model/account_payment_model.go:85` |
| `GET /account/wallets` | `has_seeker_token` | `model/account_wallet_model.go:33` |
| `GET /account/points` | `account_point_id`, `payment_plan_id`, `account_payment_id`, `linked_network_id` | `model/account_point_model.go:27,31-33` |
| `POST /auth/network-create` | `user_auth`, `is_pro`, nested `network.is_pro` | `model/network_model.go:102,105,112` |
| `POST /auth/network-delete` | `error` (spec result is empty) | `controller/network_controller.go:200` |
| `POST /auth/upgrade-guest` | `user_auth` | `model/network_model.go:1050` |
| `GET /sn/pool/claim` | `error` | `controller/sn_controller.go:99` |
| `POST /verify` | `coverage` (VerifyProof), `egress_ip_hash` (VerifyProofHop) | `verify_wire.go:151,133` |
| `GET /network/clients`, `POST /network/auth-client` | `proxy_client` +7: `change_id,create_time,proxy_id,client_id,api_base_url,block,api_port` | `model/network_client_proxy_model.go:548-567` |
| `POST /network/auth-client` | result `client_id` | `model/network_client_model.go:115` |
| `GET/POST blocked/block/unblock-location` | `error` (spec results empty) | `controller/exclude_network_client_location_controller.go:18,44,67` |
| `POST /network/find-providers2` | `has_estimated_bytes_per_second` | `model/network_client_location_model.go:2091` |
| `POST /network/find-locations` etc. | `country_count,region_count,city_count,stable_count,strong_privacy_count`; `locations[].stable,strong_privacy` | `network_client_location_model.go:1306-1340` |
| `GET /device/associations` | `pending` on pending-adoption items | `model/device_association_model.go:1068` |

**Request extras**
| Endpoint | Extra fields | Location |
|---|---|---|
| `POST /account/wallet` | `network_id` (overwritten from session) | `model/account_wallet_model.go:37` |
| `POST /wallet/validate-address` | `chain` | `controller/circle_wallet_controller.go:113` |
| `POST /auth/login-with-password` | `verify_otp_numeric` | `model/auth_model.go:620` |
| `POST /auth/network-create` | `guest_mode`, `verify_use_numeric` | `model/network_model.go:84-85` |
| `POST /network/find-locations`, `/network/provider-locations` | `rank_mode` | `network_client_location_model.go:1321` |
| `POST /network/find-providers2` | `force_count`, `force_minimum` | `network_client_location_model.go:2077,2081` |
| `POST /stats/provider-last-n`, `/stats/providers-overview-last-n` | `lookback` | `model/provider_model.go:277,517` — **intentional** backward-compat; keep |

### C6. Type / nullability / cosmetic
- `GET /account/payments`: `wallet_address` `*string` vs spec non-null.
- `GET /sn/pool/claim`: `epoch` `required: true` in spec but optional in impl (defaults to latest finalized).
- `POST /verify`: `egress_ip_hash [32]byte` marshals as a 32-int JSON array (not base64).
- Enum-not-enforced but values conform: `auth_jwt_type`, `blockchain`, `wallet_type`, `default_token_type`.
- Go type names ≠ spec schema names (no wire impact): `NetworkRemoveResult`/`NetworkDeleteResult`, `SetNetworkRankingPublic*`/`…Visibility*`, `VerifyKey`/`VerifyServerKey`, `ReferralNetworkResult`.

### C7. Spec defects (fix the YAML)
- `POST /wallet/circle-transfer-out`: `operationId` duplicated as `"Wallet Circle Init"` (`bringyour.yml:807`).
- `AuthVerifyResult`: malformed — `network`/`error` fields sit beside `type:` instead of under `properties:` (`bringyour.yml:1929`); their sub-fields are undeclared.
- `POST /auth/code-create`: `security:[]` contradicts its own description.
- `DeviceAssociationsResult`: spec defines 3 distinct item schemas but impl reuses one `DeviceAssociation` (superset).

---

## Part 3 — Remediation plan

Policy (per direction): **the API implements the spec faithfully on output; is lenient on input (accepts spec field names AND current variations); all routes are implemented; the spec is updated with the new endpoints and improved for convention/readability.**

### R1 — Faithful field naming + lenient input (C3, C5)
- **Response renames → emit the spec name.** `wallet_login`→`wallet_auth`, `account_points`→`network_points`, `share_code`→`adopt_secret`. Where clients may still read the old name, emit both during a transition (spec name is canonical). *(Decision: hard-rename vs dual-emit window.)*
- **Request aliases → accept both names.** Add lenient decoding (second alias field reconciled in the handler, or a custom `UnmarshalJSON`) so renamed request fields accept the spec name and the current one.
- **`connect/control.pack`**: change to `[]string` on output (spec), accept scalar-or-array on input.
- **Extra fields**: prefer to **document them in the spec** (make the spec a superset) rather than strip from impl — this preserves the "be lenient / don't break consumers" spirit. Strip only fields that are genuinely internal leakage.

### R2 — Auth reconciliation (C2) — **needs a decision, not covered by "faithful" blindly**
- `/stats/leaderboard`, `/referral-code/validate`: faithful → **add auth** (currently public). Confirm these should require a bearer token (leaderboard is often intentionally public).
- `/auth/code-create`: faithful-to-spec would make it **public → insecure**. Fix the **spec** (`security: [BearerAuth]`), keep the impl.

### R3 — Implement all routes (A, C1) — **triage required**
- Wire the orphaned **`/device/set-name`** (model exists; fix the `client_id`→`device_id` tag).
- Implement the FIXME stubs: **`/device/set-provide`**, **`/network/create-provider-spec`**.
- `/network/find-providers`, `/network/find-locations` are **no-ops ("no longer supported")** — decide **implement vs remove-and-deroute** (implementing a deprecated route may be counter to intent).

### R4 — Update the spec with new endpoints (B)
- Add the **12 real client endpoints** with request/response schemas.
- Decide webhook treatment (add under a separate `webhooks`/tag, or leave external).
- Add the `account_points.event` enum values; add `derived_client_id`/`specs`/location-name fields or remove them from spec (C4).

### R5 — Improve the spec (C7 + readability)
- Fix the malformed `AuthVerifyResult`, the duplicated `operationId`, and `code-create` security.
- Split `DeviceAssociationsResult` into its 3 real item shapes (or accept the superset and document).
- General: consistent naming, shared component reuse, regenerate `connect/api/build/api.html`.

### R6 — Guardrail (recommended addition, not in the original 5)
- Add a **spec-conformance test** that (re)runs this diff in CI — parse `bringyour.yml`, reflect over the handler arg/result structs, and assert field/type/auth parity — so the surface can't drift again.

---

## Decisions (resolved)
1. **Auth**: follow the spec for `/stats/leaderboard` + `/referral-code/validate` (add auth); **fix the spec** for `/auth/code-create` (keep the impl's `RequireAuth`).
2. **No-op routes** (`find-providers`, `find-locations`): **remove + deroute** (do not implement).
3. **Response renames**: **dual-emit** — emit both the spec name and the current name; spec name is canonical, legacy marked deprecated.
4. **Extra fields**: **document in the spec** (superset); strip only genuine internal leakage (case-by-case).
5. **Conformance guardrail test**: **add** — parse `bringyour.yml`, reflect over handler arg/result structs, assert field/type/auth parity in CI.

## Decisions still open (minor)
- **Webhooks in spec** (8 external): leave external / out-of-spec (recommended), or document under an OpenAPI 3.1 `webhooks:` block.
- **C4 — spec fields the impl does NOT emit** (mirror image of #4): `derived_client_id` (auth-client req) → remove from spec if unused; `locations[].city/region/country` display names → implement (there is a code FIXME) to satisfy spec; `FindLocationsResult.specs[]` → trim from spec.
- **`connect/control.pack`** scalar-vs-array: verify which side is authoritative from the connect client before changing the impl (the spec may be wrong here, like the enum/code-create cases).
- **Legacy stats aliases** (`/stats/provider-last-90`, `/stats/providers-overview-last-90`): leave undocumented (recommended) or add as deprecated.
