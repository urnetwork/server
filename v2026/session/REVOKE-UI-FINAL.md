# Session management UI — final developer handoff

Status: final UI design, 2026-10-09. The owner took every decision in §1 on that day. No UI is implemented by this document.

The server, protocol, API, SDK and bindings are specified in [REVOKE-FINAL.md](REVOKE-FINAL.md). That implementation ends at **`sdk/client_session_view_controller.go`**. The app UI described here is built on that shared controller. Do not duplicate session fetching, credential selection, retry or logout logic inside each screen.

Paths in §7 are from the app repositories on 2026-10-09. Verify them before editing.

## 1. Decisions (owner, 2026-10-09)

1. **Entry point:** Account → **Sessions**, one row directly after Profile, in Android, iOS, macOS, Windows, Linux and ur.io.
2. **Account icon:** the Sessions row uses one side-facing head profile glyph on every platform (`session-face-profile`, §8).
   - Windows, Linux and ur.io Account rows have no icons today. Give every Account row there a leading icon so the list stays consistent.
3. **Session ID:** each row shows the first 8 characters of the session ID on its last line, copyable. It is never the row's primary label.
4. **Times:** each row shows **Last used** (the server's last observed authenticated use, relative) and **Signed in** (creation date). There is no separate "connected" time.
5. **Leading visual:** a filled circle in the session's country color, with the device-type logo in white inside. Device types use brand logos (§8).
6. **Swipe to sign out:**
   - Touch platforms swipe a row left to reveal **Sign out**.
   - Pointer platforms get a trailing **Sign out** button instead.
   - macOS adds a context menu.
7. **Confirmation:** every sign-out asks for confirmation, naming the session. The current session can be signed out from the list, with a warning that this app will be signed out.
8. **Sign out all other sessions** is a button at the bottom of the list.
9. **Refresh:**
   - Pull to refresh on Android, iOS and touch ur.io.
   - A header refresh button on macOS, Windows, Linux and pointer ur.io.
   - Every screen also loads on open and polls through the controller while visible.
10. **Relative times** run up to 7 days, then become a date. The accessible text is the full date and time.
11. **Partial coverage:** when legacy coverage is partial, a one-line note explains that older sign-ins appear once they renew.
12. **Always shown:** the Sessions row always appears. If the server does not support sessions yet, the screen says so. Nothing probes the server from the Account list.
13. **Client info:** every app reports its device type and app version through `Api.SetClientInfo` wherever it creates or replaces its Api. Windows reports its real build version instead of the hard-coded `0.0.1`.
14. **SDK builds:** each app builds against an SDK from sdk main that includes the controller and `SetClientInfo`. The local SDK builds of 2026-10-09 predate both.

## 2. Integration contract

**Construct the controller** with the account Api: `Api.OpenClientSessionViewController()` / `NewClientSessionViewControllerWithApi(ctx, api)`. Alternatively use `NewClientSessionViewControllerWithDevice(ctx, device)` when a Device is available for live notifications. API-only apps are fully supported.

**Bind it as follows:**
- `Start()` when the screen appears.
- `SetVisible(true/false)` as it shows and hides.
- `SetForeground(bool)` from the app lifecycle.
- `Refresh()` for explicit refresh.
- `RevokeSession(sessionId)` and `RevokeOtherSessions()` for the actions.
- `AddClientSessionListener(listener)` for typed snapshots.
- `Close()` when the screen is disposed, after unsubscribing.

The controller polls every 30 seconds while visible. It handles operation IDs, retries and status recovery itself.

**`ClientSessionSnapshot` fields:**
- `Sessions` (a `NetworkSessionInfoList`, in display order), `CurrentSessionId`, `LegacyCoverage`.
- State: `Loaded`, `Loading`, `Refreshing`, `Supported`.
- Actions: `BulkAction`, `Actions` (a `ClientSessionActionList`).
- `Error`.

**`ClientSessionAction` fields:** `SessionId`, `OperationId`, `Loading`, `Pending`, `Status`, `State`, `Error`.

**`ClientSessionError` fields:** `Message`, `Retryable`, `SignInRequired`, `Unsupported`.

**`NetworkSessionInfo` fields:** `SessionId`, `Current`, `Kind`, `CreateTime`, `LastMintTime`, `TokenExpireTime`, `AcceptUntil`, `OriginSessionId`, and nullable `LastUsed`.

| `SessionLastUsed` field | Meaning |
|---|---|
| `UnixTime` / `unix_time` | UTC Unix **seconds** of the retained server-observed authenticated use. |
| `City`, `Region`, `Country` | GeoLite2 location names; empty means unknown. |
| `CountryCode` | Lower-case ISO alpha-2 or empty. |
| `DeviceType` | `android`, `ios`, `macos`, `windows`, `linux`, `web`, `cli`, `server` or `unknown`. |
| `AppVersion` | App version reported with that observed use; empty means unknown. |

Apps consume typed SDK models and getters, not maps or decoded Redis/header JSON. The one exception is ur.io's JS host, which delivers the snapshot as JSON with **snake_case** keys (`last_used.unix_time`, `country_code`, `device_type`, `app_version`, `current_session_id`, `bulk_action`). Device and version metadata is advisory, not a verified device identity.

**Client info:** call `Api.SetClientInfo(NewClientInfo(deviceType, appVersion))` with:
- `android`, `ios`, `macos`, `windows` or `linux` for the native apps, `web` for ur.io;
- the app's real build version.

The SDK sends `X-UR-ClientInfo` and the QUIC auth equivalent itself; the screen never touches headers.

## 3. Rows

Show every session in the snapshot, in the controller's order: current first, then the most recent known use.

| Part | Content |
|---|---|
| Leading | A filled circle in the country color, with the device logo (§8) in white at about half the circle's size. The circle is 40 pt (Android, iOS, ur.io, Windows, Linux) or 30 pt (macOS, matching `ProviderColorCircle`). The color comes from the SDK's `getColorHex(country_code)`. An empty code uses the platform's existing unknown-country color. |
| Line 1 | Device label and app version, e.g. "Android · 2026.10.8-1067". Omit the version when empty. The current session adds a **This session** tag. |
| Line 2 | Location and last use, e.g. "Chicago, Illinois, United States · Last used 5m ago". Omit empty location parts. With `LastUsed == nil`, show "Last use unavailable". |
| Line 3 | Created date, method and short ID, e.g. "Signed in Oct 3 · Google · ID 01a1f3c2". The method label (§3.2) is omitted for legacy kinds. The ID is the first 8 characters, copyable via long-press, context menu or a copy affordance per platform. Copy places the full ID on the clipboard. |

### 3.1 Times

- **Last used** is relative:
  - "now" under 5 seconds;
  - then `{count}s ago`, `{count}m ago`, `{count}h ago`, `{count}d ago` up to 7 days;
  - then a localized date.
- **Signed in** is a localized date from `CreateTime`.
- Every relative or date string has an accessible description with the full localized date and time in the user's time zone.
- `LastUsed.UnixTime` is in seconds.
- Never present `LastMintTime` as activity, or `TokenExpireTime` as the session's accepted lifetime.

### 3.2 Device and method labels

| Device type | Label | Icon |
|---|---|---|
| `android` | Android | `device-android` |
| `ios` | iOS | `device-apple` |
| `macos` | macOS | `device-apple` |
| `windows` | Windows | `device-windows` |
| `linux` | Linux | `device-linux` |
| `web` | Web | `device-web` |
| `cli` | Command line | `device-cli` |
| `server` | Server | `device-server` |
| `unknown`, empty or other | Unknown device | `device-unknown` |

| `Kind` | Method label |
|---|---|
| `password` | Password |
| `verify` | Verification code |
| `apple` | Apple |
| `google` | Google |
| `sso` | Single sign-on |
| `wallet` | Wallet |
| `seedphrase` | Recovery phrase |
| `signup` | New account |
| `auth_code` | Auth code |
| `device_adopt` | Device pairing |
| `api_key_client` | API key |
| `legacy`, `legacy_proxy`, other | (omitted) |

These are the kinds the server mints today (`session.MintNetworkSession` / `RegisterAndSignInTx` call sites).

## 4. Actions

**Sign out one session:**
- Android: `SwipeToRevealRow` with the label "Sign out".
- iOS: `.swipeActions(edge: .trailing, allowsFullSwipe: false)` with a destructive "Sign out".
- ur.io on touch: a swipe-to-reveal row, new in ur.io.
- macOS: a trailing "Sign out" button plus a context-menu "Sign out".
- Windows, Linux and ur.io with a fine pointer: a trailing "Sign out" button.
- Every platform exposes "Sign out {device}" as a screen-reader action on the row.

**Confirmation** comes before any revoke:
- Title: "Sign out this session?"
- Body: "{device} in {place} will be signed out.", or "{device} will be signed out." when the place is unknown.
- The current session's body is "This is the session you're using. This app will be signed out."
- Cancel is the default.

After a successful self-sign-out, the app follows its normal logout flow, independently of the Sessions screen staying mounted.

**Sign out all other sessions:**
- Shown when the snapshot has a current session and at least one other row.
- Confirmation title: "Sign out all other sessions?"
- Confirmation body: "Every other session in this list will be signed out. This session stays signed in. Anyone who knows your sign-in details can still sign in again."
- Never say "all devices signed out".

**Pending:**
- While a row's action is `Loading` or `Pending`, the row shows progress and "Signing out…", and its control is disabled. The bulk action does the same with its button.
- A 202 stays pending until the controller confirms enforcement. The controller's next snapshot drops the confirmed row.
- Never declare completion on a timeout.
- Repeated attempts go through the controller, which reuses its operation.

## 5. Loading, errors and accessibility

- **States:**
  - never loaded: progress;
  - loaded and empty: "No active sessions";
  - refreshing: keep the rows and show the refresh indicator;
  - failed initial load: "Couldn't load sessions." with Try again;
  - failed refresh: keep the rows, with the non-blocking notice "Couldn't refresh. Showing the last list.";
  - `Supported == false`: "Sessions aren't available yet."
- **Errors use the flags, not raw server messages:**
  - `Unsupported`: the unsupported state.
  - `SignInRequired`: the app's existing sign-in flow, with generic sign-in-required wording. "This session was signed out from another device." appears only when the controller reports a trustworthy session-revoked cause.
  - `Retryable`: retry feedback that keeps credentials and the last list.
  - Action errors show on their row: "Couldn't sign out this session. Try again."
  - A target-session 404 is a stale row; the controller refreshes.
- **Footer notes:**
  - "Last used is the most recent sign-in activity the server saw. It can lag a few minutes, and the location is approximate."
  - When `LegacyCoverage == "partial"`: "Sign-ins from older app versions appear here once they renew. To end every sign-in, change your sign-in details."
- **Accessibility:** dynamic text sizes, long place and version strings, keyboard navigation, screen-reader action labels and focus restoration after dialogs. Status is never shown by color alone. Render all metadata as plain text.
- **Strings:** every user-facing string comes from the localization store (§9).

## 6. Refresh and lifecycle

- **On appear:** `Start()` and `SetVisible(true)`.
- **On disappear:** `SetVisible(false)`.
- **App lifecycle:** foreground/background goes to `SetForeground`.
- **On dispose:** unsubscribe, then `Close()`. No updates may land after disposal.
- **Pull to refresh:** Android `PullToRefreshBox`, iOS `.refreshable`, ur.io on touch.
- **Header refresh button:** macOS toolbar `arrow.clockwise`, Windows, Linux and pointer ur.io.
- **Refresh state:** both refresh paths call `Refresh()` and show the snapshot's `Refreshing`.

## 7. Platform work

| Platform | Implementation |
|---|---|
| Android | **Account row:** add it after Profile in `ui/account/AccountScreen.kt` (rows at ~427-498) using `URNavListItem` with a new vector drawable `nav_list_item_sessions` (the face glyph). **Navigation:** Sessions route and screen with a Hilt ViewModel; lifecycle as in `ProviderStatusViewModel` (`VisibleDeviceControllerOwner`, `ProcessLifecycleOwner` foreground), built on the Api controller as in `SdkPurchaseConfirmationSource`. **Rows:** `SwipeToRevealRow`, `PullToRefreshBox`, a `CircleImage(40.dp)` country circle colored by `Sdk.getColorHex`, and `device_*` vector drawables. **Shared code:** move the relative-time helper into a shared helper that covers days. **Client info:** set it in `MainApplication.updateActiveNetworkSpace` (~1318-1334) with `android` and `VERSION_NAME`. |
| iOS / macOS | **Account row:** an `AccountNavLink` (`Main/Account/AccountRootView.swift`) using a new template asset `ur.symbols.session.face`. **Navigation:** `.sessions` in `AccountNavigationPath` and `AccountNavStackView`'s `navigationDestination`. **Model:** a `SessionsStore` ObservableObject following `ContractDetailsStore`'s listener and teardown pattern and `SdkPurchaseConfirmation`'s Api-built controller; `presentationActive` drives `SetForeground`. **iOS:** `.swipeActions` and `.refreshable`. **macOS:** trailing button, `.contextMenu` and toolbar refresh. **Rows:** `ProviderColorCircle` plus device logo assets. **Client info:** set it in the `DeviceManager` `networkSpace` didSet with `ios` or `macos` and the bundle version. |
| Windows | **Row kit:** Account pane rows have no icon slot, so add a leading-glyph variant to the pane row kit. Give every Account row a glyph: Segoe Fluent for existing rows, `PathIcon` from §8 for Sessions and device logos. **Sessions page:** a sub-page like `OpenReferrals`, or a sheet. **Rows:** a trailing "Sign out" on each `rows::Row`. **Dialogs:** confirm with a `ContentDialog` whose default is Close, like `ConfirmRemoveAuth`. **Header:** a Refresh button. **Country circle:** a `MakeDot` ellipse with the device glyph. **Threading:** marshal the listener through `DispatcherQueue` with a weak/alive guard, as in WalletPage's provider status. **Controller:** open it from the in-process Api (`urnet_api_open_client_session_view_controller`). **Client info:** set it in `SdkHost::Initialize` and `SdkHost::ApplyNetworkServer` with `windows` and `urnw::version::kString`, and replace the hard-coded `0.0.1` app version. **Time:** extend `urnw::RelativeTime` with days. |
| Linux | **Row kit:** give every Account row a leading icon (PaneKit). **Sessions row:** after Profile, with the face glyph drawn in Cairo from §8 path data as a new `BrandIcons` kind; device logos are drawn the same way. **Sessions page:** a sub-page like Referrals (`on_open_referrals`, MainWindow navigation). **Rows:** a flat destructive trailing "Sign out", with a modal confirm whose default is Cancel, like `ConfirmRemoveAuth`. **Header:** a refresh action (`ur-pane-action`). **Country circle:** `kit::PaintDot` with the device glyph. **Threading:** listener through `PostToMain` guarded by `alive_`, as in the Earnings points board. **Controller:** opened from `host_.api()` in the GUI process. **Client info:** set it wherever the Api is created (SdkHost.cpp ~437, ~795, ~4320) with `linux` and `kAppVersion`. **Time:** extend `RelativeTime` with days. |
| ur.io | **Account rows:** add inline SVG icons to `NAV_ROWS` (`src/app/screens/Account.jsx`), and change the `.navRow svg` dimming so leading icons keep full contrast while the chevron stays dimmed. **Route:** `sessions` in `AppRoutes.jsx`'s account subtree. **Screen:** a hook built on `useControllerSource`/`openOn` (account host `api`, or `device` when attached). `setVisible` comes from document visibility (`documentActivity.js`), plus `setForeground`. **Rows:** a swipe-reveal row and pull to refresh on `(pointer: coarse)`, a trailing button and refresh button otherwise. **Country circle:** colored by `sdkColor.js`'s `dotColor`. **Client info:** call `host.setClientInfo("web", appVersion)` when the account host is created (`accountHost.js` ~63). **Keys:** register them in `scripts/panel-keys.mjs`. **Tests:** `node --test` with jsdom, as in `tests/app-embed-screen.test.mjs`. |

Reuse each app's existing logout listeners and persisted-auth cleanup. Do not modify API authorization or decode JWT claims to choose a credential.

## 8. Icons

All glyphs come from Material Design Icons 7.4.47 (Pictogrammers, Apache-2.0).
- Include the license notice wherever each app lists third-party licenses.
- Each glyph is one path on a 24×24 viewBox, filled with the current color.
- Device logos are drawn white on the country circle. The Account glyph takes the row's normal icon tint.
- Convert them to each platform's native form: Android vector drawable, Apple template asset, Windows `PathIcon`/`Geometry`, a Linux Cairo path, ur.io inline SVG.

| Glyph | MDI name | Use |
|---|---|---|
| `session-face-profile` | `head-outline` | Account → Sessions row |
| `device-android` | `android` | `android` |
| `device-apple` | `apple` | `ios`, `macos` |
| `device-windows` | `microsoft-windows` | `windows` |
| `device-linux` | `linux` | `linux` |
| `device-web` | `web` | `web` |
| `device-cli` | `console` | `cli` |
| `device-server` | `server` | `server` |
| `device-unknown` | `help` | `unknown`, empty, other |

Path data (`d`), each for a 24×24 viewBox:

```text
session-face-profile: M13 1C8.4 1 4.6 4.4 4.1 8.9L2.5 11C2 11.8 1.9 12.8 2.3 13.6C2.7 14.3 3.3 14.8 4 14.9V16C4 17.8 5.3 19.4 7 19.9V23H18V17.5C20.5 15.8 22 13.1 22 10C22 5 18 1 13 1M16 16.3V21H9V18H8C6.9 18 6 17.1 6 16V13H4.5C4.1 13 3.8 12.5 4.1 12.2L6 9.7C6.2 6 9.2 3 13 3C16.9 3 20 6.1 20 10C20 12.8 18.4 15.2 16 16.3Z
device-android: M16.61 15.15C16.15 15.15 15.77 14.78 15.77 14.32S16.15 13.5 16.61 13.5H16.61C17.07 13.5 17.45 13.86 17.45 14.32C17.45 14.78 17.07 15.15 16.61 15.15M7.41 15.15C6.95 15.15 6.57 14.78 6.57 14.32C6.57 13.86 6.95 13.5 7.41 13.5H7.41C7.87 13.5 8.24 13.86 8.24 14.32C8.24 14.78 7.87 15.15 7.41 15.15M16.91 10.14L18.58 7.26C18.67 7.09 18.61 6.88 18.45 6.79C18.28 6.69 18.07 6.75 18 6.92L16.29 9.83C14.95 9.22 13.5 8.9 12 8.91C10.47 8.91 9 9.24 7.73 9.82L6.04 6.91C5.95 6.74 5.74 6.68 5.57 6.78C5.4 6.87 5.35 7.08 5.44 7.25L7.1 10.13C4.25 11.69 2.29 14.58 2 18H22C21.72 14.59 19.77 11.7 16.91 10.14H16.91Z
device-apple: M18.71,19.5C17.88,20.74 17,21.95 15.66,21.97C14.32,22 13.89,21.18 12.37,21.18C10.84,21.18 10.37,21.95 9.1,22C7.79,22.05 6.8,20.68 5.96,19.47C4.25,17 2.94,12.45 4.7,9.39C5.57,7.87 7.13,6.91 8.82,6.88C10.1,6.86 11.32,7.75 12.11,7.75C12.89,7.75 14.37,6.68 15.92,6.84C16.57,6.87 18.39,7.1 19.56,8.82C19.47,8.88 17.39,10.1 17.41,12.63C17.44,15.65 20.06,16.66 20.09,16.67C20.06,16.74 19.67,18.11 18.71,19.5M13,3.5C13.73,2.67 14.94,2.04 15.94,2C16.07,3.17 15.6,4.35 14.9,5.19C14.21,6.04 13.07,6.7 11.95,6.61C11.8,5.46 12.36,4.26 13,3.5Z
device-windows: M3,12V6.75L9,5.43V11.91L3,12M20,3V11.75L10,11.9V5.21L20,3M3,13L9,13.09V19.9L3,18.75V13M20,13.25V22L10,20.09V13.1L20,13.25Z
device-linux: M14.62,8.35C14.2,8.63 12.87,9.39 12.67,9.54C12.28,9.85 11.92,9.83 11.53,9.53C11.33,9.37 10,8.61 9.58,8.34C9.1,8.03 9.13,7.64 9.66,7.42C11.3,6.73 12.94,6.78 14.57,7.45C15.06,7.66 15.08,8.05 14.62,8.35M21.84,15.63C20.91,13.54 19.64,11.64 18,9.97C17.47,9.42 17.14,8.8 16.94,8.09C16.84,7.76 16.77,7.42 16.7,7.08C16.5,6.2 16.41,5.3 16,4.47C15.27,2.89 14,2.07 12.16,2C10.35,2.05 9,2.81 8.21,4.4C8,4.83 7.85,5.28 7.75,5.74C7.58,6.5 7.43,7.29 7.25,8.06C7.1,8.71 6.8,9.27 6.29,9.77C4.68,11.34 3.39,13.14 2.41,15.12C2.27,15.41 2.13,15.7 2.04,16C1.85,16.66 2.33,17.12 3.03,16.96C3.47,16.87 3.91,16.78 4.33,16.65C4.74,16.5 4.9,16.6 5,17C5.65,19.15 7.07,20.66 9.24,21.5C13.36,23.06 18.17,20.84 19.21,16.92C19.28,16.65 19.38,16.55 19.68,16.65C20.14,16.79 20.61,16.89 21.08,17C21.57,17.09 21.93,16.84 22,16.36C22.03,16.1 21.94,15.87 21.84,15.63
device-web: M16.36,14C16.44,13.34 16.5,12.68 16.5,12C16.5,11.32 16.44,10.66 16.36,10H19.74C19.9,10.64 20,11.31 20,12C20,12.69 19.9,13.36 19.74,14M14.59,19.56C15.19,18.45 15.65,17.25 15.97,16H18.92C17.96,17.65 16.43,18.93 14.59,19.56M14.34,14H9.66C9.56,13.34 9.5,12.68 9.5,12C9.5,11.32 9.56,10.65 9.66,10H14.34C14.43,10.65 14.5,11.32 14.5,12C14.5,12.68 14.43,13.34 14.34,14M12,19.96C11.17,18.76 10.5,17.43 10.09,16H13.91C13.5,17.43 12.83,18.76 12,19.96M8,8H5.08C6.03,6.34 7.57,5.06 9.4,4.44C8.8,5.55 8.35,6.75 8,8M5.08,16H8C8.35,17.25 8.8,18.45 9.4,19.56C7.57,18.93 6.03,17.65 5.08,16M4.26,14C4.1,13.36 4,12.69 4,12C4,11.31 4.1,10.64 4.26,10H7.64C7.56,10.66 7.5,11.32 7.5,12C7.5,12.68 7.56,13.34 7.64,14M12,4.03C12.83,5.23 13.5,6.57 13.91,8H10.09C10.5,6.57 11.17,5.23 12,4.03M18.92,8H15.97C15.65,6.75 15.19,5.55 14.59,4.44C16.43,5.07 17.96,6.34 18.92,8M12,2C6.47,2 2,6.5 2,12A10,10 0 0,0 12,22A10,10 0 0,0 22,12A10,10 0 0,0 12,2Z
device-cli: M20,19V7H4V19H20M20,3A2,2 0 0,1 22,5V19A2,2 0 0,1 20,21H4A2,2 0 0,1 2,19V5C2,3.89 2.9,3 4,3H20M13,17V15H18V17H13M9.58,13L5.57,9H8.4L11.7,12.3C12.09,12.69 12.09,13.33 11.7,13.72L8.42,17H5.59L9.58,13Z
device-server: M4,1H20A1,1 0 0,1 21,2V6A1,1 0 0,1 20,7H4A1,1 0 0,1 3,6V2A1,1 0 0,1 4,1M4,9H20A1,1 0 0,1 21,10V14A1,1 0 0,1 20,15H4A1,1 0 0,1 3,14V10A1,1 0 0,1 4,9M4,17H20A1,1 0 0,1 21,18V22A1,1 0 0,1 20,23H4A1,1 0 0,1 3,22V18A1,1 0 0,1 4,17M9,5H10V3H9V5M9,13H10V11H9V13M9,21H10V19H9V21M5,3V5H7V3H5M5,11V13H7V11H5M5,19V21H7V19H5Z
device-unknown: M10,19H13V22H10V19M12,2C17.35,2.22 19.68,7.62 16.5,11.67C15.67,12.67 14.33,13.33 13.67,14.17C13,15 13,16 13,17H10C10,15.33 10,13.92 10.67,12.92C11.33,11.92 12.67,11.33 13.5,10.67C15.92,8.43 15.32,5.26 12,5A3,3 0 0,0 9,8H6A6,6 0 0,1 12,2Z
```

## 9. Strings

Add these keys to `localizations/keys/*.yaml`, translated for every locale the store carries, with platforms `apple`, `android`, `windows`, `linux` and `site`. Regenerate with the repository's normal command (`npm run gen`; ur.io's `scripts/build-locales.mjs`). Placeholders use `{name}`. `days_ago_abbrev` follows `hours_ago_abbrev`'s format and plural handling.

**Reused existing keys:** `sign_out`, `cancel`, `refresh`, `try_again`, `copy`, `copied`, `loading`, `now`, `seconds_ago_abbrev`, `minutes_ago_abbrev` and `hours_ago_abbrev`.

| Key | English |
|---|---|
| `sessions_title` | Sessions |
| `sessions_this_session` | This session |
| `sessions_last_used` | Last used {time} |
| `sessions_last_use_unavailable` | Last use unavailable |
| `sessions_signed_in` | Signed in {date} |
| `sessions_id` | ID {id} |
| `sessions_copy_id` | Copy session ID |
| `sessions_device_android` | Android |
| `sessions_device_ios` | iOS |
| `sessions_device_macos` | macOS |
| `sessions_device_windows` | Windows |
| `sessions_device_linux` | Linux |
| `sessions_device_web` | Web |
| `sessions_device_cli` | Command line |
| `sessions_device_server` | Server |
| `sessions_device_unknown` | Unknown device |
| `sessions_kind_password` | Password |
| `sessions_kind_verify` | Verification code |
| `sessions_kind_apple` | Apple |
| `sessions_kind_google` | Google |
| `sessions_kind_sso` | Single sign-on |
| `sessions_kind_wallet` | Wallet |
| `sessions_kind_seedphrase` | Recovery phrase |
| `sessions_kind_signup` | New account |
| `sessions_kind_auth_code` | Auth code |
| `sessions_kind_device_adopt` | Device pairing |
| `sessions_kind_api_key_client` | API key |
| `sessions_sign_out_all_others` | Sign out all other sessions |
| `sessions_sign_out_accessibility` | Sign out {device} |
| `sessions_confirm_title` | Sign out this session? |
| `sessions_confirm_body` | {device} in {place} will be signed out. |
| `sessions_confirm_body_no_place` | {device} will be signed out. |
| `sessions_confirm_self_body` | This is the session you're using. This app will be signed out. |
| `sessions_confirm_others_title` | Sign out all other sessions? |
| `sessions_confirm_others_body` | Every other session in this list will be signed out. This session stays signed in. Anyone who knows your sign-in details can still sign in again. |
| `sessions_signing_out` | Signing out… |
| `sessions_empty` | No active sessions |
| `sessions_load_failed` | Couldn't load sessions. |
| `sessions_refresh_failed` | Couldn't refresh. Showing the last list. |
| `sessions_action_failed` | Couldn't sign out this session. Try again. |
| `sessions_unsupported` | Sessions aren't available yet. |
| `sessions_signed_out_remotely` | This session was signed out from another device. |
| `sessions_last_used_help` | Last used is the most recent sign-in activity the server saw. It can lag a few minutes, and the location is approximate. |
| `sessions_legacy_note` | Sign-ins from older app versions appear here once they renew. To end every sign-in, change your sign-in details. |
| `days_ago_abbrev` | {count}d ago |

## 10. Acceptance tests and delivery

Use deterministic controller fakes and platform presentation/ViewModel tests, following each app's existing test tools (`node --test` with jsdom for ur.io). Required cases:

- **Snapshot shapes:** current and other sessions, multiple rows, empty and never-loaded states, `LastUsed` known and nil, Unix-second conversion, and long or missing location, device type and app version.
- **Row content:** the device label/icon mapping for every type including unknown, the method label for every kind including omitted legacy kinds, the country circle color and empty-code fallback, the 8-character ID with full-ID copy, and relative time across seconds, minutes, hours, days and the 7-day date cutoff.
- **Refresh and lifecycle:** pull to refresh / refresh button calls `Refresh()`, foreground and visibility forwarding, unsubscription, and no updates after disposal.
- **Actions:** swipe-reveal or trailing button opens the confirmation, cancel does nothing, confirm calls `RevokeSession`, duplicate activation is suppressed, 202 pending shows progress until confirmed, and a stale-row 404 refreshes. Self-sign-out shows the self body and follows the logout flow. Sign out all other sessions appears only with a current session plus others, and calls `RevokeOtherSessions` after confirmation.
- **Races:** out-of-order refresh and action completion, and switching accounts while a request is pending; the newer account's UI survives an older response.
- **Errors and coverage:** unsupported, sign-in-required, retryable, the action error on its row, and honest partial-legacy messaging.
- **Accessibility and localization:** keyboard and screen-reader operation (including the "Sign out {device}" action), localization of every string, and metadata rendered as text.

Every UI bug fix includes a deterministic regression test for its root cause, per repository instructions. The UI can be developed against fakes before the backend is deployed. Release only after the shared controller and bindings contract is available and REVOKE-FINAL.md's server and protocol gates pass. No UI copy may claim universal legacy or unleased-P2P cutoff.
