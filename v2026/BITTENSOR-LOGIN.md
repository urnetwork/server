BITTENSOR LOGIN

The contract for signing in, creating a network, and adding or removing a sign-in method with a Bittensor (TAO) wallet. The server side is complete; the client must sign the exact message the server issues. Re-verified against the code on 2026-09-15: `model/wallet_auth_challenge_model.go`, `model/auth_bittensor.go`, `model/auth_model.go` (`handleLoginWallet`), `model/network_model.go` (`NetworkCreate`, wallet path), `model/network_user_model.go` (`AddAuth`, `RemoveAuth`).

WHY THE CURRENT CLIENT IS DENIED

A WalletConnect sign-message flow that signs a random string fails with `400 invalid message format`. The server only accepts a signature over the message it issued for that login attempt; the parser (`parseWalletAuthChallengeMessage`) requires exactly three lines — the header, a `Challenge:` line and a `Timestamp:` line — and then checks the challenge against the database: it must exist, be issued for the same blockchain (and address, when one was given), carry the same timestamp, be unexpired and unused. This is the anti-replay rule; it is the same for Solana and Bittensor.

THE FLOW (three calls)

1. Request a challenge (no auth):

   POST /auth/wallet-challenge
   {"blockchain": "tao", "wallet_address": "<ss58 address>"}

   Response:
   {
     "challenge": "<32 random bytes, base64url>",
     "timestamp": 1757340000,
     "expires_in": 300,
     "message_template": "Sign in to URnetwork\nChallenge: <challenge>\nTimestamp: 1757340000"
   }

   `wallet_address` is optional but recommended: when given, the challenge is bound to that address and any other address is rejected with `400 challenge wallet address mismatch`. The challenge lives 5 minutes (`expires_in`, honoured end to end) and is single use. Failed requests are rate limited per client address (`429` after too many).

2. Sign `message_template` with the wallet, byte for byte, as UTF-8. Do not add, reorder or trim anything; the three lines are separated by `\n` (LF) only. It is the finished text, not a template with holes — the challenge and the timestamp are already interpolated into it, and `challenge`/`timestamp` are returned alongside only so a client can display or check them. Never rebuild the message from those two fields.

   There is no QR payload in this response, by design. A WalletConnect pairing QR encodes a relay topic and a symmetric key and structurally cannot carry an application payload; the challenge and timestamp reach the wallet after pairing, as the `polkadot_signMessage` request built from `message_template`. So a pairing QR legitimately contains neither value.

   Bittensor keys are substrate sr25519 keys. The standard signing path (polkadot-js `signRaw`, WalletConnect `polkadot_signMessage`, most mobile wallets) uses the `substrate` signing context and wraps the payload in `<Bytes>…</Bytes>` before signing. The server accepts both the wrapped and the raw form of the signature, so use whatever the wallet does — but always submit the unwrapped `message_template` text as `wallet_message`.

   Signature encoding: the 64-byte sr25519 signature as hex, with or without the `0x` prefix (WalletConnect returns `0x…`). ed25519 keys are not accepted for TAO.

3. Submit the signed challenge. All three endpoints take the same `wallet_auth` object:

   {
     "wallet_auth": {
       "blockchain": "tao",
       "wallet_address": "<ss58 address>",
       "wallet_message": "<message_template exactly as issued>",
       "wallet_signature": "0x<128 hex chars>"
     }
   }

   - `POST /auth/login` with `{"wallet_auth": …}` → `{"network": {"by_jwt": …}}` when the wallet is bound to a network; `{"wallet_auth": …}` (echo, no network) when it is not yet bound, meaning the client should continue to network creation.
   - `POST /auth/network-create` with `{"network_name": …, "terms": true, "wallet_auth": …}` (a fresh challenge; the login one was consumed) → creates the network and the user with auth type `bittensor` and returns `by_jwt`.
   - `POST /auth/add-auth` (bearer JWT) with `{"wallet_auth": …}` → binds the wallet as an additional sign-in method of the calling user. Since 2026-09-09 this also requires the issued challenge; a signature over any other text is rejected (`400 invalid message format`).
   - `POST /auth/remove-auth` (bearer JWT) with `{"auth_type": "bittensor"}` removes the wallet method (the last remaining method cannot be removed: `cannot remove your last auth method`). Accepted values: `email`, `phone`, `apple`, `google`, `solana`, `bittensor`, `seedphrase`.

   `blockchain` accepts `tao` or `bittensor` (case-insensitive; the server canonicalizes to `TAO`). Solana uses `solana`/`sol` with a base64 ed25519 signature; the challenge and message format are identical.

ADDRESS FORMAT

An ss58 address of the 32-byte public key. The checksum is verified and the network prefix is pinned to 42 (Bittensor and generic substrate). Re-encoding the same public key under another prefix is rejected with `400 invalid wallet address` — one key therefore has exactly one accepted address string, which is what the account uniqueness checks rely on. One wallet address can be bound to one user; binding it elsewhere fails with `This wallet is already linked to another account.`, and binding a *second, different* wallet to a user that already has one fails with `A different wallet is already linked to this account.` — the previous behaviour silently replaced it. Rotating a wallet is therefore remove-then-add via `/auth/remove-auth`; note a wallet-only account cannot remove its wallet (`cannot remove your last auth method`) and must add an email or phone first.

ERRORS (message text, prefixed with the HTTP-style code)

- `400 invalid message format` — the signed text is not the issued template (random string, CRLF, `<Bytes>` wrapper submitted as the message, extra lines).
- `400 invalid wallet address` — not a valid ss58 address (bad base58, bad checksum, or a network prefix other than 42).
- `400 challenge timestamp too old` / `too far in the future` — the message timestamp is outside the challenge lifetime (5 minutes back, plus a minute of slack) or more than a minute ahead of now. In practice the binding deadline is `403 challenge expired` at 5 minutes.
- `400 challenge timestamp mismatch` / `challenge blockchain mismatch` / `challenge wallet address mismatch` — the message or address does not match the issued challenge.
- `400 invalid signature encoding` — the signature could not be decoded at all: not hex, not 64 bytes, or 64 bytes that are not a well formed sr25519 signature.
- `401 invalid signature` — the signature decoded but does not verify over the message (wrong key, wrong text, wrong context).
- `401 challenge not found` — unknown challenge value.
- `403 challenge already used` / `403 challenge expired` — request a new challenge.

NOTES FOR THE APPS

- The Solana add-wallet flows on Apple (`AddAuthSheet.swift`) and Android (`AddAuthMethodSheet.kt` via `requestAndSignSolanaChallenge`) already fetch the challenge and sign `message_template`; the Bittensor path should mirror them with `blockchain: "tao"`, the sr25519 signature in hex, and the WalletConnect `polkadot_signMessage` request carrying the template text.
- The SDK already exposes `Api.AuthWalletChallenge` and the `TAO` blockchain constant; no SDK change was needed for this contract.
- Never re-sign or re-use a challenge: login, network create and add-auth each consume one.
- The legacy `wallet_nonce` field on `wallet_auth` is unused by the server; ignore it.
