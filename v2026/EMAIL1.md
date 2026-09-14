# EMAIL1: transactional email modernization

Status: implemented 2026-09-01 (steps 1 to 3). The eight templates live in `controller/email_templates/` behind `_layout.html` / `_layout.txt` composition in `RenderEmailTemplate`, `RenderSmsTemplate` reads `<name>.sms.txt`, subjects are trimmed and single-line, the send path takes an optional `reply_to_email` and `configuration_set` from email.yml (message tag `template=<name>` rides with the configuration set), the interview template and its ctl command are deleted, monitor 19.2 probes `https://ur.io/images/emails/`, and `controller/email_templates_test.go` renders every template. Still open: section 5 Phase A (DNS, SES identity, configuration set, Postmaster Tools), Phase B items 4 (feedback consumer) and 9 (seed-inbox test), and `subscription_ended`, which is still referenced with no files. Deploy order: the ur.io site (it carries the wordmark images) before the server.

Date: 2026-09-01. Scope: `controller/email_templates/*`, the SES send path in `controller/aws_controller.go`, the assets under `mmm/bringyour.com/res/emails/`, and monitor signal 19.2.

Proposal page with live mockups of the eight templates (paper and dark, desktop and phone, HTML, text, SMS, and Go bodies): https://claude.ai/code/artifact/90bfc7bb-7f6b-4522-9e4e-0c17636454b7

## 1. What ships today

Ten templates. Each is three embedded files (`<name>.subject.txt`, `<name>.html`, `<name>.txt`) rendered by `RenderEmailTemplate` with `html/template` and `text/template` against a small struct. Everything is sent as `URnetwork <support@ur.io>` (config/main/email.yml) through SES v1 `SendEmail` in us-west-1 with no configuration set. `SendAccountMessageTemplate` routes by user auth type: phone accounts get the `.txt` body as an SMS, so four text parts are held to SMS length.

| Template | Sent when | Subject today | Brand and links | Images |
| --- | --- | --- | --- | --- |
| auth_verify | Network create or sign-in needs a verified address. Email or SMS. 6-digit numeric or 8-char hex code, valid 4h (`model.VerifyCodeTimeout`). | Verify your email | UR. ur.io, security@ur.io | UR wordmark JPG (5 KB) |
| auth_password_reset | Password reset requested. Email or SMS. 128-hex reset code in URL. | Reset your password | UR. ur.io/?resetCode= | UR wordmark JPG |
| auth_password_set | After a password change. | Your password was changed | UR. security@ur.io | UR wordmark JPG |
| network_welcome | Network created, or verified after signup. | Welcome | UR. ur.io, docs.ur.io, brien@ur.io | Header JPG (177 KB), wordmark, animated GIF (4.9 MB) |
| subscription_send_payment | Provider payout sent on-chain. | You received a payment! | UR. ur.io, docs.ur.io, Google Play, Solana dApp Store, bringyour.com/discord | Wordmark, animated GIF (2.6 MB) |
| subscription_missing_wallet | Payout blocked, no wallet. Once per pay period with new earnings. | Action needed: Connect a wallet. | UR. ur.io/app, support@ur.io | Wordmark |
| subscription_transfer_balance_code | Data pack bought with an email (Stripe). 26-char base32 code. | Your data code | BringYour. app.bringyour.com | BringYour wordmark JPG |
| subscription_transfer_balance_company | Company shared data pack bought (Stripe). | Let's set up your shared data | BringYour. support@bringyour.com | BringYour wordmark JPG |
| x402_receipt | Agent paid inline over x402 and supplied an email. | Your URnetwork receipt (trailing newline) | BringYour. app.bringyour.com | BringYour wordmark JPG |
| network_user_interview_request_1 | By hand from bringyourctl, personal sender. | The future internet | BringYour. bringyour.com, calendly.com, brien@bringyour.com | Header JPG (160 KB), BringYour wordmark |

`subscription_ended` is referenced by `SubscriptionEndedTemplate` (subscription_controller.go, Play renewal lapse) but has no template files. `RenderEmailTemplate` returns a read error that the caller drops, so that email is never sent.

Retired by this proposal (decided 2026-09-01): `subscription_transfer_balance_company` was sent only by the `SpecialCompany` branch in subscription_stripe_controller.go for the one SKU in config/main/stripe.yml with `special: company` (`prod_PYvNGYsoREsTVZ`, 100TiB). Removed the same day: the SKU entry, the `Special` field and constant, the handler branch, the template struct, and the three template files. The Stripe product itself still needs archiving in the Stripe dashboard so nobody can buy it; a purchase of it now fails the webhook with "Stripe sku not found". `network_user_interview_request_1` is hand-sent from bringyourctl and will be replaced by a proper feedback system later; delete the template, the struct, and the command. Eight templates go forward and the rest of this document is about those eight.

Assets in `mmm/bringyour.com/res/emails/`: two wordmark JPGs with baked light backgrounds, two wordmark SVGs (unused), two header JPGs, a 3.5 MB PNG, and four GIFs between 2.6 MB and 10.8 MB.

## 2. Findings

Deliverability

- Warn: two brands in one inbox. Five templates carry the BringYour wordmark with app.bringyour.com and support@bringyour.com links while the sender is support@ur.io. Mixed From, link, and image domains is a filter signal and confuses recipients.
- Warn: images load from bringyour.com, not the sending domain.
- Critical: 2.6 MB and 4.9 MB animated GIFs in the two highest-volume emails (payout, welcome).
- Note: no preheader; inbox previews show the first body line or the street address.
- Note: SES v1 without a configuration set, so bounces and complaints never reach code. Gmail sends no complaint feedback to SES at all, so nothing today measures Gmail spam rate.
- Note: x402_receipt subject ends in a newline; subjects are not trimmed. Missing-wallet subject ends in a period.
- Note: SPF, DKIM, DMARC, and MAIL FROM for ur.io could not be resolved from the sandbox (DNS blocked). First item in the plan.

Rendering

- Critical: div-only layout with no doctype, html, head, or viewport. Outlook and older clients fall to quirks mode; the inline-block footer breaks; text-shadow, user-select, background-clip are stripped.
- Warn: body text is font-weight 100 in generic sans-serif at 12pt; hairline on Windows. Footer is 8pt.
- Warn: no dark-mode handling. The wordmark JPG survives inversion only because its light background is baked in.
- Warn: verification code in magenta with a drop shadow, about 1.6:1 contrast on the off-white page.
- Note: `target="_blank"` on links, `<p>` inside a monospace box, nested-div spacing, px and pt mixed.

Content and brand

- Warn: "and unauthorized person" (four templates), "preseve" (two).
- Warn: text parts diverge from HTML: welcome text is one line, missing-wallet text contains a raw HTML anchor, balance-code text links a different URL than the HTML.
- Note: no "why you received this" line, which matters most for verification mail sent to an address someone else typed.
- Note: interview request is BringYour-branded, books via calendly.com, and `bringyourctl/main.go:1222` sends it as `brien@brienyour.com` (typo). SES rejects unverified identities, so that command panics.

Code

- Note: `.txt` doubles as the SMS body (`RenderSmsTemplate` reads the same file).
- Note: header and footer pasted into all ten HTML files; no layout composition.
- Note: the `Funcs` plumbing is inert; templates call methods on dot (`.CopyrightYear`, `.Balance`) rather than FuncMap functions.
- Note: monitor 19.2 (`monitor/signal_email_assets.go`, `TestEmailAssetsCatalogMatchesServerTemplates`) pins the asset catalog to `https://bringyour.com/res/emails/`. Any asset move updates both in the same change.

## 3. Proposal: one shell, three families, paper and dark

ur.io is #f8f8f8 type on a #101010 ground, one blue pill button, tracked uppercase labels, Inter body, Geist UI labels, ABC Gravity display. Email reads best on paper, so the proposal flips the same palette: ink on #f8f8f8 by default, and back to the site's dark whenever the mail client honors `prefers-color-scheme` (Apple Mail, iOS Mail, Outlook macOS, Outlook.com via `data-ogsc`). Every color is an existing token in `mmm/ur.io/react/src/styles/global.css`.

Principles

1. One column, no cards. 600px table, 32px gutters (20px on phones), single surface. Structure from type and hairlines. The only box is the code tile, because it is the thing you copy.
2. Uppercase headline as the brand echo. ABC Gravity cannot ship in email (client support, license), so the headline is Inter bold 30px uppercase, two to four words. Inter and Geist are self-hosted at ur.io/fonts and load in Apple Mail and Outlook for Mac; everyone else gets the system stack.
3. Three families, three tints, on the eyebrow pill only: Account #d6e6f4 (verify, reset, password changed), Money #87fb67 (payouts, wallet, data codes, receipts), Welcome #eff7bb (the welcome note). Links and the button: blue-deep #0039de on paper (7.9:1), blue-bright #0099ff on dark.
4. One image: the wordmark at 153x22 from a 320px PNG (about 5 KB), hosted on ur.io. Two `<img>` tags; CSS shows the white one in dark clients. The default is the black wordmark baked on a #f8f8f8 tile so Gmail's inversion (which ignores the media query) leaves a readable sticker.
5. Every email says why it arrived, in one muted line above the legal footer (copyright, address, ur.io / Support / Privacy, and the site's operator line).
6. Text parts are real: `.txt` mirrors the HTML; SMS gets its own `.sms.txt`.

Tokens

| Role | ur.io token | Paper | Dark |
| --- | --- | --- | --- |
| Ground | --color-dark #101010 | #f8f8f8 | #101010 |
| Headline, code, values | --color-white #f8f8f8 | #101010 | #f8f8f8 |
| Body text | --color-blue-light #d6e6f4 | #2b3036 | #d6e6f4 |
| Muted, footer | blue-light at 60% | #5f6a74 (5.0:1) | #879299 (5.9:1) |
| Hairline | rgba(239,247,187,.08) | #e1e5e9 | #2a2e33 |
| Link and button | --color-blue-bright | #0039de (--color-blue-deep) | #0099ff |
| Tints | blue-light, lime, yellow-light | #d6e6f4, #87fb67, #eff7bb with #101010 text | same |
| Radius | --radius 12px, pills 100px | 12px tiles, 100px pills | same |
| Type | Inter, Geist, PP NeueBit | Inter 16/26 body, 30/34 700 uppercase h1, 40/44 amount; Geist 11px 0.1em eyebrow, 16px 600 button; system mono 36px 0.22em code | same |

Anatomy, top to bottom: wordmark, eyebrow pill, headline, body, exactly one module (code tile, pill button, amount with detail rows, or a short list), why-line, legal footer.

Email-client facts that shaped this (Litmus, July 2026): Apple Mail about 62% of opens, Gmail 27%, Outlook 6%, Yahoo 3%. Apple honors `prefers-color-scheme`; Gmail apps ignore it and invert on their own (iOS full, Android partial); Outlook Windows fully inverts; Yahoo and AOL mangle the media query harmlessly.

## 4. Deliverability research

Baseline enforced by the large providers (Gmail and Yahoo since February 2024, Outlook.com since May 5 2025; Gmail began rejecting non-compliant traffic in late 2025):

| Requirement | Gmail | Yahoo | Outlook.com | iCloud |
| --- | --- | --- | --- | --- |
| SPF and DKIM passing | bulk: both; others: one | bulk: both | bulk: both | both |
| DMARC published, From aligned with SPF or DKIM (relaxed ok) | bulk: p=none minimum | bulk: p=none with rua | bulk: p=none minimum; rejects with 550 5.7.515 | required |
| Spam rate | under 0.10%, never 0.30% (Postmaster Tools) | under 0.3% of inbox-delivered | monitored | monitored |
| One-click unsubscribe (RFC 8058) | bulk, marketing and subscribed mail only; transactional explicitly excluded | same, honor within 2 days | functional unsubscribe link | unsubscribe link, honored promptly |
| TLS, PTR, RFC 5322, single From, valid Message-ID | required | required | required | expected |
| Consistent reply-capable From, no no-reply | same From per category | consistent | real From/Reply-To | consistent name and address |
| Separate streams | per category | separate IPs and DKIM domains for marketing | recommended | recommended |

"Bulk" is about 5,000 messages per day to one provider, counted across the primary domain and all subdomains. Verification codes plus payouts can reach that, so the plan assumes bulk.

Amazon SES adds account-wide guardrails: bounce rate under 2% (review at 5%, pause at 10%), complaint rate under 0.1% (review at 0.1%, pause at 0.5%), measured over a representative volume. Only hard bounces count. Gmail does not send complaints to SES. SES sets Message-ID, Date, Feedback-ID, and PTR itself; Reply-To is a request field; custom `Headers` on SESv2 Simple content may include `List-Unsubscribe` and `List-Unsubscribe-Post`. Easy DKIM defaults to 2048-bit and signs with d=<identity domain>, which is the DKIM path to DMARC alignment; a custom MAIL FROM subdomain (one MX to `feedback-smtp.<region>.amazonses.com`, one SPF TXT) adds the SPF path. The account-level suppression list is default-on for BOUNCE and COMPLAINT on accounts created after 2019-11-25. TLS delivery is opportunistic unless a configuration set sets `TlsPolicy=REQUIRE`, which drops mail to servers without TLS.

RFC 8058: `List-Unsubscribe: <https://...>` plus `List-Unsubscribe-Post: List-Unsubscribe=One-Click`; the receiver POSTs `List-Unsubscribe=One-Click`; the URL must be HTTPS, must not redirect, must not require confirmation, should carry an opaque per-recipient token, and both headers must be DKIM-signed (SES covers this).

BIMI (optional, later): DMARC at p=quarantine pct=100 or p=reject, an SVG Tiny PS logo, and a VMC (trademark) or CMC certificate; Gmail, Yahoo, and Apple Mail show the mark.

Where URnetwork stands

- OK: From is `URnetwork <support@ur.io>`, reply-capable and consistent for account mail. Marketing already leaves through a separate channel.
- Verify: DKIM signing with d=ur.io, SPF, DMARC. Unknown until DNS and a Gmail "Show original" are checked.
- Blind: no Postmaster Tools, no configuration set, no bounce or complaint consumer, so Gmail spam rate and SES reputation are unmonitored.
- Plan: custom MAIL FROM, feedback consumer in code. One-click unsubscribe is not needed while every template is transactional; it arrives with the feedback system or product updates.

## 5. Plan

Phase A. Authentication and feedback (before the templates ship; DNS and SES console; about a day plus propagation)

1. Prove the baseline: `bringyourctl` send of auth_verify to a Gmail seed, read "Show original" (SPF PASS, DKIM PASS d=ur.io, DMARC PASS); `dig TXT ur.io`, `dig TXT _dmarc.ur.io`, the three Easy DKIM CNAMEs. Record results here.
2. Easy DKIM 2048-bit on the ur.io domain identity (one key-length change allowed per 24h).
3. One SPF record on ur.io with `include:amazonses.com` plus any other legitimate @ur.io senders. Two SPF records on one name is a permerror.
4. Custom MAIL FROM `mail.ur.io`: `MX 10 feedback-smtp.us-west-1.amazonses.com`, `TXT v=spf1 include:amazonses.com -all`. Exactly one MX. Behavior on MX failure: use default.
5. DMARC at monitor: `_dmarc.ur.io TXT "v=DMARC1; p=none; rua=mailto:dmarc-reports@ur.io; adkim=r; aspf=r"`. Also decide bringyour.com: if nothing sends from it, publish `v=spf1 -all` and `p=reject`.
6. Configuration set `account-mail`: event destination (send, delivery, bounce, complaint, reject, delivery delay, rendering failure) to SNS and CloudWatch; alarms at bounce 2% and complaint 0.05%. Leave TLS opportunistic.
7. Confirm account-level suppression list is on for BOUNCE and COMPLAINT (`aws sesv2 get-account`). Enable Virtual Deliverability Manager dashboard.
8. Google Postmaster Tools for ur.io (DNS TXT verification): spam rate, reputation, authentication, compliance status.

Phase B. Templates and send path (code, with the new templates)

1. Templates as proposed (section 3): `_layout.html` and `_layout.txt` in `email_templates/`, each template a set of `{{define}}` blocks (title, preheader, eyebrow, accent, headline, content, why). `RenderEmailTemplate` parses `_layout.html` + `<name>.html` with `ParseFS` and executes `_layout.html`; same for text. Message size stays under 15 KB.
2. SMS split: `RenderSmsTemplate` reads `<name>.sms.txt` when present, else `.txt`. Six templates can reach a phone account (verify, reset, password set, welcome, payout, missing wallet).
3. Move sending to SESv2 `SendEmail` (aws-sdk-go-v2 sesv2): `ConfigurationSetName=account-mail`, `ReplyToAddresses=[support@ur.io]`, `EmailTags=[template=<name>]`, subject `TrimSpace` and reject on newline, a per-template flag that adds `List-Unsubscribe` and `List-Unsubscribe-Post` headers, unused until a subscribed message type exists. Retry with backoff on TooManyRequests only. Log MessageId with the network id.
4. Feedback consumer: SNS to HTTPS endpoint (or SQS drained by taskworker). Hard bounce marks the user_auth undeliverable, stops non-critical mail, and prompts in-app to fix the address. Complaint sets ProductUpdates=false and suppresses all but security mail.
5. Deferred until the feedback system: one-click unsubscribe endpoint (RFC 8058) wired to ProductUpdates: opaque per-recipient token; POST unsubscribes immediately, no confirmation, no redirect; GET renders a confirmation page.
6. Retire the interview template and fix the small things: delete the interview template, `NetworkUserInterviewRequest1Template`, and the bringyourctl command (which removes the `brien@brienyour.com` typo with it); add or remove `subscription_ended`; drop `target="_blank"`. (The company template, struct, Stripe branch, and SKU were removed on 2026-09-01; archive the Stripe product.)
7. Assets and monitor: commit `ur-wordmark-black-bg-320.png` and `ur-wordmark-white-320.png` to `mmm/ur.io/react/public/images/emails/` (react/public syncs into astro/public), update the monitor 19.2 catalog and regex test, then remove the JPG and GIF files from `mmm/bringyour.com/res/emails/`.
8. Render test in Go: render every template with sample data; assert no unresolved braces, one-line subject, every image URL in the monitor catalog, every link on ur.io or a ur.io mailto, existing x402 assertions hold, HTML under 100 KB.
9. Seed test before cutover: all eight via bringyourctl to Gmail, iCloud, Outlook.com, Yahoo seeds in light and dark; one mail-tester run per family; check iOS Mail code autofill (needs "code" near the digits).

Phase C. Tighten and keep watch (two to six weeks after cutover)

1. DMARC to `p=quarantine; pct=100` after two clean weeks of aggregate reports, then `p=reject`; `sp=reject` for non-sending subdomains.
2. Monthly review: Postmaster Tools, VDM per-provider delivery, CloudWatch bounce and complaint, suppression list growth. EmailTags traces spikes to a template.
3. Optional BIMI (CMC does not need a trademark; Gmail accepts it).
4. Optional dedicated IP only well past the bulk tier; shared pools with Optimized Shared Delivery are the better default now.

## 6. Content sheet (proposed defaults for step 2)

| Template | Subject | Preheader | Eyebrow | Headline | Button | Why line |
| --- | --- | --- | --- | --- | --- | --- |
| auth_verify | `{{.VerifyCode}}` is your URnetwork verification code | Enter this code to verify your email. It expires in 4 hours. | Verify your email | Your code | (code tile) | You received this because this address was entered to create or sign in to a URnetwork network. |
| auth_password_reset | Reset your URnetwork password | Use the button below to choose a new password. | Password reset | Reset your password | Reset password, https://ur.io/?resetCode= | You received this because a password reset was requested for the URnetwork account on this address. |
| auth_password_set | Your URnetwork password was changed | If this wasn't you, contact security@ur.io right away. | Security | Password changed | none | Sent to the address on your URnetwork account whenever its password changes. |
| network_welcome | Welcome to URnetwork | A note from the founder, and how to get connected. | Welcome | Welcome to URnetwork | Get connected, https://ur.io/install | You received this because a URnetwork network was created with this address. |
| subscription_send_payment | You got paid: `{{.AmountUsd}}` USDC | Your provider earnings were sent to your wallet. | Payout sent | You got paid | View this payout, https://ur.io/app/account/payouts?id=`{{.PaymentId}}` | You received this because a payout was sent to the wallet on your URnetwork account. |
| subscription_missing_wallet | Connect a wallet to receive `{{.AmountUsd}}` USDC | Your earnings are waiting. Connect a wallet and the payment retries automatically. | Action needed | Connect a wallet | Connect a wallet, https://ur.io/app/account/wallets | Sent once per pay period while you have new earnings and no wallet connected. |
| subscription_transfer_balance_code | Your `{{.Balance}}` URnetwork data code | Redeem this code on any network to add the data. | Data code | Your data code | Redeem code, https://ur.io/app/balance-codes#code=`{{.Secret}}` | You received this because a data pack was purchased with this address. |
| x402_receipt | Receipt: `{{.Description}}` | Paid `{{.Price}}` `{{.Asset}}` automatically over x402. | Receipt | Receipt | See your balance, https://ur.io/app/account | You received this because this address was given with an x402 purchase. |

Every link in the drafts was checked against the ur.io route table and the static build on 2026-09-01 (an earlier draft pointed at `/app/payouts`, `/app/wallets`, and `/provider`, none of which exist):

- Reset password: `https://ur.io/?resetCode=…`. The header island on every static page mounts the auth provider and dialog, which opens the password-reset view from that query parameter.
- View this payout: `https://ur.io/app/account/payouts?id=<payment id>`. The PayoutDetail screen reads `?id=` and shows that payment; `server.Id` prints the same string in templates and in the `/account/payments` JSON.
- Connect a wallet: `https://ur.io/app/account/wallets`.
- Redeem code: `https://ur.io/app/balance-codes#code=<secret>`. The code travels in the URL fragment, which is never sent to the server or written to the static host's access log; the Balance codes screen prefills the field from it and then strips it from the address bar (added to `react/src/app/screens/BalanceCodes.jsx`).
- See your balance: `https://ur.io/app/account`.
- Learn about providing (payout email): `https://ur.io/docs/faq#can-i-earn-by-sharing-my-connection`.
- Get connected (welcome): `https://ur.io/install`. Footer: `/support`, `/privacy`.

Signed-out readers: `/app` routes used to bounce a signed-out visitor to the home page with the destination lost. `AppLayout` now redirects to `/?auth&next=<path>`, which opens the login dialog, and `AuthContext` returns the visitor to `next` after a successful login (already-signed-in visitors go straight there). `next` is accepted only as a same-origin `/app…` or `/<lang>/app…` path, so a crafted link cannot turn the dialog into an open redirect. SMS bodies for the six phone-capable templates are in the prototype output.

## 7. Localization (asked 2026-09-01; design, not yet scheduled)

Yes, and the pieces mostly exist. What is missing is a locale on the account and a string source for the templates.

What exists today

- The site ships six languages (en, es, de, zh, ru, ar) from hand-maintained `react/src/i18n/<lang>.json`; the apps and extension ship many more from the localization store (`localizations/keys/*.yaml`, one file per key, translations per locale, `npm run gen` writes platform files into sibling repos, `npm run check` is the CI drift gate).
- The server knows nothing about a user's language: `NetworkCreateArgs` has no locale, the network row has no locale column, and `SendAccountMessageTemplate` takes only a user auth string.
- Emails are English only; the layout is `lang="en"`, left-to-right.

Design

1. Capture the locale. Add `locale` (BCP 47, optional) to `NetworkCreateArgs` and `AccountPreferencesSetArgs`, store it on the network (nullable column), and fall back to the request's `Accept-Language` when a client sends nothing. The apps, extension, and site already know their language; each sends it on network create (one line per client).
2. Source the strings from the store. Every email string becomes a key in `localizations/keys/email_<template>_<part>.yaml` with `platforms: [server]`; `npm run gen` writes `controller/email_templates/i18n/<locale>.yml` the way it writes android `strings.xml`, and the drift gate keeps them honest. Roughly 60 keys for the eight templates including subjects, preheaders, SMS bodies, and the why-lines. The legal footer (address, copyright, operator line) stays English.
3. Look them up in the templates. `RenderEmailTemplate` gains a locale argument and registers `T` (the FuncMap plumbing that is inert today gets its first use): `{{T "email_verify_body" "hours" 4}}` resolves in the account's locale with `{name}` interpolation and falls back to English for a missing key, the same rule the site's `t()` uses. Bodies keep their markup; only text moves into keys.
4. Make the layout locale-aware. `lang` and `dir` on `<html>` and the article wrapper from the locale (`dir="rtl"` for ar flips table text alignment in every client); subject and preheader from keys; SMS from keys; Inter stays first in the font stack and the system stack supplies CJK, Cyrillic, and Arabic glyphs.
5. Send in the account's locale. `SendAccountMessageTemplate` resolves the locale from the network (callers have the network id or session); bringyourctl's send commands take `--locale`.
6. Test. The render test loops every template over every shipped locale; a key missing in a locale is a build failure, not a silent English fallback in production.

Order: after the English set ships (step 3), because the keys should be cut from the final English copy. Effort: about a day on the server (column, capture, `T`, layout attributes, test), a day across the clients to send the locale, and the store's normal translation pass for the new keys.

## 8. Prototype source

The generator (`build.mjs`) holds the shell, the modules, and all eight emails as content blocks whose values carry both a sample and a Go expression, and writes rendered samples, Go bodies, text, SMS, and the proposal page from one source. It lived in the session scratchpad at `email-proto/` (session-specific path; the shell is reproduced below so this file stands alone). A standalone Go check parsed and executed every generated Go body with `html/template` and `text/template` against mock structs shaped like the controller's template types: all eight render, the reset code URL-escapes correctly in the href, a `<script>` in a sample value is escaped, and the x402 test strings are present.

## 9. Final draft (2026-09-01)

The eight templates as they ship, rendered with sample data so the copy reads as a recipient sees it (the plain-text part; the HTML carries the same words in the layout of section 3). The Go source of each is in `controller/email_templates/`. Subjects and preheaders are the inbox row. The welcome email's three paid-tier points render as a "+" list in both parts.

### Verify email (`auth_verify`)

- Subject: 482917 is your URnetwork verification code
- Preheader: Enter this code to verify your email. It expires in 4 hours.
- Eyebrow: Verify your email
- SMS (66 chars): Your URnetwork verification code is 482917. It expires in 4 hours.

```text
URnetwork

YOUR CODE

Enter this code in the app to verify your email. It expires in 4 hours.

    482917

Didn't ask for a code? Ignore this email. Nothing happens without it. If
you believe someone else is using your account, contact security@ur.io.

You received this because this address was entered to create or sign in
to a URnetwork network.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Password reset (`auth_password_reset`)

- Subject: Reset your URnetwork password
- Preheader: Use the button below to choose a new password.
- Eyebrow: Password reset
- SMS (181 chars): Reset your URnetwork password: https://ur.io/?resetCode=3f9a6c1e8b24d7f05a9c3e6b1d8f2a4c7e0b5d9f3a6c8e1b4d7f0a2c5e8b1d4f7a0c3e6b9d2f5a8c1e4b7d0f3a6c9e2b5d8f1a4c7e0b3d6f9a2c5e8b1d4f7

```text
URnetwork

RESET YOUR PASSWORD

We received a request to reset the password for your URnetwork account.
Choose a new one with the button below.

Reset password: https://ur.io/?resetCode=3f9a6c1e8b24d7f05a9c3e6b1d8f2a4c7e0b5d9f3a6c8e1b4d7f0a2c5e8b1d4f7a0c3e6b9d2f5a8c1e4b7d0f3a6c9e2b5d8f1a4c7e0b3d6f9a2c5e8b1d4f7

Didn't request this? You can ignore this email; your password won't
change until you create a new one. If you believe someone else is using
your account, contact security@ur.io.

You received this because a password reset was requested for the
URnetwork account on this address.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Password changed (`auth_password_set`)

- Subject: Your URnetwork password was changed
- Preheader: If this wasn't you, contact security@ur.io right away.
- Eyebrow: Security
- SMS (79 chars): Your URnetwork password was changed. If this wasn't you, contact security@ur.io

```text
URnetwork

PASSWORD CHANGED

The password for your URnetwork account was just changed. If that was
you, you're all set.

If you didn't make this change, contact security@ur.io right away and
we'll help you secure the account.

Sent to the address on your URnetwork account whenever its password
changes.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Welcome (`network_welcome`)

- Subject: Welcome to URnetwork
- Preheader: A note from the URnetwork team, and how to get connected.
- Eyebrow: Welcome
- SMS (60 chars): Welcome to URnetwork! Get connected at https://ur.io/install

```text
URnetwork

WELCOME TO URNETWORK

URnetwork started in 2023 with a mission: give people real encryption,
great privacy products, and transparency. Our vision for the future is
giving everyone, anywhere in the world, the choice on how to be seen and
what to see.

URnetwork is a free product. We’re building for the billions of people
and agents on the internet who want to be free. It also has paid
features that can be summarized as “more of what you get for free” +
“more tools for businesses and pro users” + “bespoke integration
services”.

Get connected: https://ur.io/install

Thanks for joining us. Your feedback is what drives us forward, and
hearing that URnetwork just works is the best part of the day.

❤️ The URnetwork team

You received this because a URnetwork network was created with this
address.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Payout sent (`subscription_send_payment`)

- Subject: You got paid: 5.00 USDC
- Preheader: Your provider earnings were sent to your wallet.
- Eyebrow: Payout sent
- SMS (119 chars): URnetwork: you got paid 5.00 USDC. Details at https://ur.io/app/account/payouts?id=018f6c2e-9a4b-7d3e-8c1a-2b5f4e6d7a90

```text
URnetwork

YOU GOT PAID

5.00 USDC

Earnings for providing on URnetwork through 8/31 14:02 UTC were sent to
your wallet.

Network: Solana
Wallet: 7Gx4kQ9pT2nVb8sLmR3wYcJ6dHfA5eN1zK2uP9tXq4Wb
Transaction: 5UyQnJf4h2Lw9xkT7ZbA3vRcPmN6sDqE1yG8tHrK2oXbVjW4eLpSa9dCz3nMfQ7u (https://explorer.solana.com/tx/5UyQnJf4h2Lw9xkT7ZbA3vRcPmN6sDqE1yG8tHrK2oXbVjW4eLpSa9dCz3nMfQ7u)
Payment ID: 018f6c2e-9a4b-7d3e-8c1a-2b5f4e6d7a90

View this payout: https://ur.io/app/account/payouts?id=018f6c2e-9a4b-7d3e-8c1a-2b5f4e6d7a90

The more decentralized we are, the better the product gets. Run
providers on more of your devices to earn more. Learn about providing
(https://ur.io/docs/faq#can-i-earn-by-sharing-my-connection).

The contracts behind this payment are deleted after 7 days to preserve
the anonymity of the network. If something looks wrong, contact
support@ur.io right away.

You received this because a payout was sent to the wallet on your
URnetwork account.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Missing wallet (`subscription_missing_wallet`)

- Subject: Connect a wallet to receive 5.00 USDC
- Preheader: Your earnings are waiting. Connect a wallet and the payment retries automatically.
- Eyebrow: Action needed
- SMS (86 chars): URnetwork: 5.00 USDC is waiting. Connect a wallet at https://ur.io/app/account/wallets

```text
URnetwork

CONNECT A WALLET

5.00 USDC waiting

You earned 5.00 USDC providing on URnetwork, but there's no wallet on
your account to send it to. Connect one and the payment is retried
automatically.

Connect a wallet: https://ur.io/app/account/wallets

This reminder is sent once per pay period while you have new earnings.
The contracts behind this payment are deleted after 7 days to preserve
the anonymity of the network. If something looks wrong, contact
support@ur.io.

Sent once per pay period while you have new earnings and no wallet
connected.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Data code (`subscription_transfer_balance_code`)

- Subject: Your 10TiB URnetwork data code
- Preheader: Redeem this code on any network to add the data.
- Eyebrow: Data code

```text
URnetwork

YOUR DATA CODE

Thanks for your purchase. This code adds 10TiB of data to whichever
network redeems it.

    K7QX3M2PNB4DLZ8R9YWC5AGHT6

Redeem code: https://ur.io/app/balance-codes#code=K7QX3M2PNB4DLZ8R9YWC5AGHT6

The button opens Balance codes with the code filled in; sign in if
asked. Anyone with the code can redeem it, so keep it private.

You received this because a data pack was purchased with this address.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

### Receipt (`x402_receipt`)

- Subject: Receipt: 1 TiB data pack
- Preheader: Paid $5.00 USDC automatically over x402.
- Eyebrow: Receipt

```text
URnetwork

RECEIPT

Receipt for 1 TiB data pack, paid automatically over x402.

Paid: $5.00 USDC
Network: base

Data added: 1TiB

Transaction: 0x3b7f2c9e5a1d48f6b0c2e7d9a4f1b3c5e8d2a6f4c9b1e3d7a5f2c8b4e6d1a9f3

See your balance: https://ur.io/app/account

You received this because this address was given with an x402 purchase.

--
© 2026 BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

## Appendix A: `_layout.html` (Go html/template layout)

```html
<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">
<html lang="en" xmlns="http://www.w3.org/1999/xhtml" xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office">
<head>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
<meta name="x-apple-disable-message-reformatting">
<meta name="format-detection" content="telephone=no, date=no, address=no, email=no">
<meta name="color-scheme" content="light dark">
<meta name="supported-color-schemes" content="light dark">
<title>{{block "title" .}}URnetwork{{end}}</title>
<!--[if mso]>
<xml><o:OfficeDocumentSettings><o:PixelsPerInch>96</o:PixelsPerInch></o:OfficeDocumentSettings></xml>
<style>table, td { border-collapse: collapse; } td, p, a, span, div, h1 { font-family: Arial, Helvetica, sans-serif !important; }</style>
<![endif]-->
<!--[if !mso]><!-->
<style>
@font-face { font-family: Inter; font-style: normal; font-weight: 400; src: url(https://ur.io/fonts/Inter-Regular.woff2) format('woff2'); }
@font-face { font-family: Inter; font-style: normal; font-weight: 700; src: url(https://ur.io/fonts/Inter-Bold.woff2) format('woff2'); }
@font-face { font-family: Geist; font-style: normal; font-weight: 600; src: url(https://ur.io/fonts/Geist-SemiBold.woff2) format('woff2'); }
</style>
<!--<![endif]-->
<style>
:root { color-scheme: light dark; supported-color-schemes: light dark; }
html, body { margin: 0 !important; padding: 0 !important; width: 100% !important; }
body { -webkit-text-size-adjust: 100%; -ms-text-size-adjust: 100%; }
table, td { mso-table-lspace: 0pt !important; mso-table-rspace: 0pt !important; }
img { border: 0; line-height: 100%; outline: none; text-decoration: none; -ms-interpolation-mode: bicubic; }
a[x-apple-data-detectors], u + #ur-body a { color: inherit !important; text-decoration: none !important; }
.ur-logo-dark { display: none; }
@media screen and (max-width: 600px) {
  .ur-pad { padding-left: 20px !important; padding-right: 20px !important; }
  .ur-h1 { font-size: 26px !important; line-height: 30px !important; }
  .ur-big { font-size: 34px !important; line-height: 38px !important; }
  .ur-code { font-size: 30px !important; line-height: 36px !important; }
  .ur-btn-t { width: 100% !important; }
  .ur-btn-a { display: block !important; text-align: center !important; }
}
@media (prefers-color-scheme: dark) {
  .ur-bg { background-color:#101010 !important; }
  .ur-ink { color:#f8f8f8 !important; }
  .ur-text { color:#d6e6f4 !important; }
  .ur-muted, .ur-muted a { color:#879299 !important; }
  .ur-rule { border-color:#2a2e33 !important; }
  .ur-link { color:#0099ff !important; }
  .ur-btn { background-color:#0099ff !important; }
  .ur-tile { background-color:#171717 !important; border-color:#2a2e33 !important; }
  .ur-logo-light { display:none !important; }
  .ur-logo-dark { display:block !important; }
}

[data-ogsb] .ur-bg { background-color:#101010 !important; }
[data-ogsb] .ur-tile { background-color:#171717 !important; }
[data-ogsb] .ur-btn { background-color:#0099ff !important; }
[data-ogsc] .ur-ink { color:#f8f8f8 !important; }
[data-ogsc] .ur-text { color:#d6e6f4 !important; }
[data-ogsc] .ur-muted, [data-ogsc] .ur-muted a { color:#879299 !important; }
[data-ogsc] .ur-link { color:#0099ff !important; }
[data-ogsc] .ur-rule { border-color:#2a2e33 !important; }
[data-ogsc] .ur-logo-light { display:none !important; }
[data-ogsc] .ur-logo-dark { display:block !important; }
</style>
</head>
<body id="ur-body" class="ur-bg" style="margin:0; padding:0; word-spacing:normal; background-color:#f8f8f8;">
<div role="article" aria-roledescription="email" lang="en" style="font-family:Inter, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size:16px; line-height:1.6;">
<div style="display:none; font-size:1px; line-height:1px; max-height:0; max-width:0; opacity:0; overflow:hidden; mso-hide:all; color:#f8f8f8;">{{block "preheader" .}}{{end}}&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;&#8204;&#160;</div>
<table role="presentation" class="ur-bg" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%; background-color:#f8f8f8;">
<tr><td align="center" style="padding:16px 8px 32px;">
<!--[if mso]><table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" align="center"><tr><td><![endif]-->
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%; max-width:600px; margin:0 auto;">
<tr><td class="ur-pad" style="padding:28px 32px 0;">
<a href="https://ur.io" style="display:inline-block; text-decoration:none;"><img class="ur-logo-light" src="https://ur.io/images/emails/ur-wordmark-black-bg-320.png" width="153" height="22" alt="URnetwork" style="display:block; width:153px; height:22px; border:0;"><!--[if !mso]><!--><img class="ur-logo-dark" src="https://ur.io/images/emails/ur-wordmark-white-320.png" width="153" height="22" alt="" aria-hidden="true" style="display:none; width:153px; height:22px; border:0;"><!--<![endif]--></a>
</td></tr>
<tr><td class="ur-pad" style="padding:40px 32px 8px;">
<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td style="background-color:{{block "accent" .}}#d6e6f4{{end}}; border-radius:100px; padding:6px 12px; font-family:Geist, Inter, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size:11px; line-height:14px; font-weight:600; letter-spacing:0.1em; text-transform:uppercase; color:#101010; mso-line-height-rule:exactly;">{{block "eyebrow" .}}URnetwork{{end}}</td></tr></table>
<h1 class="ur-h1 ur-ink" style="margin:16px 0 0; font-family:Inter, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size:30px; line-height:34px; font-weight:700; letter-spacing:-0.01em; text-transform:uppercase; color:#101010;">{{template "headline" .}}</h1>
{{template "content" .}}
</td></tr>
<tr><td class="ur-pad" style="padding:32px 32px 0;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td class="ur-rule ur-muted" style="border-top:1px solid #e1e5e9; padding:20px 0 0; font-family:Inter, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size:12px; line-height:18px; color:#5f6a74;">
{{template "why" .}}
<br><br>&copy; {{.CopyrightYear}} BringYour, Inc. &middot; 2261 Market Street #5245, San Francisco, CA 94114, United States<br>
<a href="https://ur.io" style="color:#5f6a74; text-decoration:underline;">ur.io</a> &middot; <a href="https://ur.io/support" style="color:#5f6a74; text-decoration:underline;">Support</a> &middot; <a href="https://ur.io/privacy" style="color:#5f6a74; text-decoration:underline;">Privacy</a><br>
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
</td></tr></table>
</td></tr>
</table>
<!--[if mso]></td></tr></table><![endif]-->
</td></tr>
</table>
</div>
</body>
</html>
```

## Appendix B: `_layout.txt`

```text
URnetwork

{{template "headline" .}}

{{template "content" .}}

{{template "why" .}}

--
© {{.CopyrightYear}} BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States
https://ur.io · Support https://ur.io/support · Privacy https://ur.io/privacy
URnetwork is provided by BringYour, Inc., a Network Operator for the UR protocol.
```

## Appendix C: example body, `auth_verify.html`

```html
{{define "title"}}Your code{{end}}
{{define "preheader"}}Enter this code to verify your email. It expires in 4 hours.{{end}}
{{define "eyebrow"}}Verify your email{{end}}
{{define "accent"}}#d6e6f4{{end}}
{{define "headline"}}Your code{{end}}
{{define "why"}}You received this because this address was entered to create or sign in to a URnetwork network.{{end}}
{{define "content"}}
<p class="ur-text" style="margin:16px 0 0; font-family:Inter, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size:16px; line-height:26px; color:#2b3036;">Enter this code in the app to verify your email. It expires in 4 hours.</p>

<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:24px 0 0;"><tr>
<td class="ur-tile" align="center" style="background-color:#ffffff; border:1px solid #e1e5e9; border-radius:12px; padding:24px 20px;">
<div class="ur-code ur-ink" style="font-family:ui-monospace, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace; font-size:36px; line-height:42px; font-weight:700; letter-spacing:0.22em; text-indent:0.22em; color:#101010; word-break:break-all; mso-line-height-rule:exactly;">{{.VerifyCode}}</div>
</td></tr></table>
<p class="ur-muted" style="margin:20px 0 0; font-family:Inter, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size:13px; line-height:20px; color:#5f6a74;">Didn't ask for a code? Ignore this email. Nothing happens without it. If you believe someone else is using your account, contact <a class="ur-link" href="mailto:security@ur.io" style="color:#0039de; text-decoration:underline;">security@ur.io</a>.</p>
{{end}}
```

## Appendix D: example text body and SMS, `auth_verify`

```text
{{define "headline"}}YOUR CODE{{end}}
{{define "why"}}You received this because this address was entered to create or sign in
to a URnetwork network.{{end}}
{{define "content"}}
Enter this code in the app to verify your email. It expires in 4 hours.

    {{.VerifyCode}}

Didn't ask for a code? Ignore this email. Nothing happens without it. If
you believe someone else is using your account, contact security@ur.io.
{{end}}

--- auth_verify.sms.txt ---
Your URnetwork verification code is {{.VerifyCode}}. It expires in 4 hours.
```

## Appendix E: SMS bodies

```text
auth_password_reset: Reset your URnetwork password: https://ur.io/?resetCode={{.ResetCode}}
auth_password_set: Your URnetwork password was changed. If this wasn't you, contact security@ur.io
auth_verify: Your URnetwork verification code is {{.VerifyCode}}. It expires in 4 hours.
network_welcome: Welcome to URnetwork! Get connected at https://ur.io/install
subscription_missing_wallet: URnetwork: {{.AmountUsd}} USDC is waiting. Connect a wallet at https://ur.io/app/wallets
subscription_send_payment: URnetwork: you got paid {{.AmountUsd}} USDC. Details at https://ur.io/app/payouts
```
