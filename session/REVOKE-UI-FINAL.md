# Session management UI — final developer handoff

Status: UI implementation plan, 2026-10-09. No UI has been implemented by this document.

The server, protocol, API, SDK and bindings are specified in [REVOKE-FINAL.md](REVOKE-FINAL.md). That implementation ends at **`sdk/client_session_view_controller.go`**. A separate developer owns the app UI described here. Use the shared controller; do not duplicate session fetching, credential selection, retry or logout logic inside each screen.

## 1. Integration contract

Construct the client session view controller from the existing account Api, optionally with a Device for live notifications. API-only apps are fully supported. Bind its typed snapshot/change listener and loading, refresh, per-session action, bulk-action, pending-operation and error state. Start it when appropriate, supply visible/foreground state, and close/unsubscribe when the screen is disposed. The controller provides `Refresh`, `RevokeSession(sessionId)` and `RevokeOtherSessions` and handles operation IDs, retries and status recovery.

Each session has an ID, `current`, sign-in kind, creation/mint/nominal-expiration/effective-acceptance times, optional origin ID, and nullable typed **`SessionLastUsed`**:

| Field | Meaning |
|---|---|
| `UnixTime` / `unix_time` | UTC Unix seconds of the retained server-observed authenticated use. |
| `City`, `Region`, `Country` | GeoLite2 location names; empty means unknown. |
| `CountryCode` | Lower-case ISO alpha-2 or empty. |
| `DeviceType` | Android, iOS, macOS, Windows, Linux, web, CLI, server or unknown enum value. |
| `AppVersion` | App version reported with that observed use; empty means unknown. |

JSON is a transport detail. Apps consume typed SDK models/getters, not maps or decoded Redis/header JSON. Timestamp conversion is from seconds, not milliseconds. Keep SDK generation/field spellings consistent with bindings.

At existing API/device initialization, configure typed ClientInfo with the app's build version and device type. The SDK emits `X-UR-ClientInfo` for HTTP/H1 and the equivalent QUIC auth metadata; the screen does not manipulate headers. App/device/version metadata is advisory, not a verified device identity.

## 2. Screen and actions

Show every session in the controller snapshot. Mark the current sign-in as **This session** rather than assuming one session means one physical device. A sign-in can own multiple client credentials. Sort current first, then most recent known use, preserving the controller's stable order.

Each row shows sign-in method, last-used time, approximate city/region/country, device type and app version from the same observation. Show creation time as secondary information. Do not label last-mint time as activity or nominal JWT expiration as the session's complete accepted lifetime. Unknown observation displays “Last use unavailable”; omit empty location/version parts cleanly. Do not display a raw session ID or origin ID as the primary label. Origin details and client-tree screens are not required.

“Last used” means observed authentication and can lag by about two minutes under healthy storage, plus screen refresh time. Existing packet traffic on an open connection does not continuously update it. If detail/help text is available, state this without showing implementation internals. Location is approximate and reflects the most recent retained observation from any client in the session. Do not imply physical location certainty or count clients from the observation's device type.

Provide explicit refresh/pull-to-refresh, **Sign out** on each row and **Sign out all other sessions**. Confirm destructive actions using the selected session's visible description. Self-sign-out explains that this app's sign-in will end. Revoke-others confirmation preserves the current session and concerns tracked sign-ins; it cannot promise that someone with the password cannot sign in again.

While an action is pending, disable duplicate activation for that target and show progress. A 202 operation stays pending until the controller confirms enforcement. On confirmed enforcement, use the refreshed snapshot/remove the affected row. Do not optimistically declare revocation complete on a timeout. Repeated attempts use the controller's existing operation, not a newly invented action ID.

A successful self-revoke or a confirmed current-credential rejection follows the app's existing logout flow. The UI must handle this independently of the Sessions screen remaining mounted. A 503 or network failure keeps credentials and the last known list, with retry feedback. A network credential rejection while a different client session continues may require account sign-in rather than shutting down that unrelated device; follow the controller/account state.

Legacy coverage is explicitly partial. Where `legacy_coverage` is partial, include concise account-security help: older sign-ins may not appear until renewed; changing sign-in credentials is the broader account action. Do not show “all devices signed out” after revoke-others. A legacy caller may need the controller's network refresh before the action; API-key-only callers have no “current session” and do not expose revoke-others as if it were revoke-all.

## 3. Loading, errors and accessibility

- Distinguish initial loading, a successful empty list, stale data during refresh and failed loading. Preserve usable rows during transient refresh failures.
- Treat an unavailable server feature as unsupported/hide the entry using the controller's feature state. A target-session 404 is a stale-row result and triggers refresh; it does not mean the feature vanished.
- Present 429 retry timing, 409 upgrade/conflict/capacity information and 503 retry feedback without clearing credentials. Use specific “signed out from another device” wording only when a trustworthy session-revoked cause exists; generic 401s use generic sign-in-required language.
- Refresh on foreground and visible-screen controller polling, including API-only apps. Stop polling/work when hidden or disposed.
- Use localized relative times with an accessible absolute-time description and the user's timezone. Support long place/version strings, dynamic text sizes, keyboard navigation, screen-reader action labels, focus restoration and non-color status indicators.
- Place all user-facing strings in the existing localization system, including unknown values, confirmations, pending state, retry and legacy coverage. Escape/render all metadata as text.

## 4. Platform work

Use existing platform patterns and verify current paths before editing; these locations come from the proposal, not a claim of a fresh UI-source audit.

| Platform | Placement and implementation |
|---|---|
| Android | Settings Account block; add a Sessions route/screen and ViewModel using existing navigation/Hilt patterns. Bind the SDK controller through the established lifecycle adapter. |
| iOS | Sign-In Methods section; add sessions navigation destination using the existing account navigation stack. |
| macOS | Account/settings security area; reuse the Apple session model/controller integration with native presentation. |
| Windows | Settings security section; add a Sessions sheet using existing native sheet patterns and typed generated bindings. |
| Linux | Account security group; add a Sessions sheet/view using the existing SDK host/lifecycle patterns. |
| ur.io | Profile/account link and Sessions route; use the existing host bridge with typed session/controller data. Support the API-only path. |

Reuse existing app logout listeners and persisted-auth cleanup. The shared SDK supplies all list/revoke behavior; platform adapters own presentation and lifecycle only. Regenerate `localizations/keys/*.yaml` outputs using the repository's normal generation command. Do not modify API authorization or independently decode JWT claims to choose which credential to send.

## 5. Acceptance tests and delivery

Use deterministic controller fakes and platform presentation/ViewModel tests; follow each app's existing test tools, including vitest for ur.io. Required cases include:

- Current/other sessions; multiple rows; empty and never-loaded state; typed last-use known/unknown; Unix-second conversion; long/missing location, device type and app version.
- Refresh and foreground behavior, visible/hidden polling lifecycle, unsubscription and no updates after disposal.
- Single/self/other-session confirmation, cancellation, duplicate-click suppression, 202 pending status, confirmed enforcement and stale-row 404.
- Out-of-order refresh/action completion and switching accounts while a request is pending; the newer account's UI survives an older response.
- API-only 401 logout, device-backed logout, unrelated client-session preservation, and 503/network failure retaining the current login and last good list.
- Unsupported feature, 409 upgrade/conflict, 429 retry feedback and honest partial-legacy messaging.
- Accessibility, localization, keyboard/screen-reader operation and metadata rendered safely as text.

Every UI bug fix includes a deterministic regression test for its root cause, per repository instructions. The UI can develop against fakes before the backend is deployed. Release only after the shared controller/bindings contract is available and the final plan's server/protocol gates pass. No claim of universal legacy or unleased-P2P cutoff may be added by UI copy.
