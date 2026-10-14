# sn25golive

Produces and checks the SN25 mainnet operator-server configuration with the
server's own types and validators: the operator gas policy and its approver
key, verify.yml, and a candidate st.yml against sn.yml.

- Every output file is created exclusively and never overwritten, including
  through a symlink. A failed write removes the partial file.
- Secret outputs (the approver key and verify.yml) are written 0600, and no
  secret is printed.
- Results go to stdout; the server packages log to stderr.
- The server package's init reads `settings.yml` from the config and site
  homes before `main` runs (never the vault). `main` then points `WARP_HOME`,
  `WARP_VAULT_HOME`, `WARP_CONFIG_HOME` and `WARP_SITE_HOME` at a new empty
  private directory, so the loaders only see the bytes the command passes them.

## Build

From the server checkout, with the sibling checkouts that `go.mod`'s
`replace` lines name (connect, proxy, sdk, glog, goidenticons, userwireguard,
sn, gvisor) next to it:

```sh
cd server
go build -o sn25golive ./local/sn25golive
go test ./local/sn25golive
```

The tests need no database. `TestVerifyConfigPassesTheServerLoaders` reads
`sn/deploy/mainnet/policy-v1.yml` from the sibling sn checkout and skips when it
is absent.

## Run

Without a command, or with an unknown one, it prints the usage to stderr and
exits 2. A refused command prints `sn25golive <command>: <reason>` to stderr
and exits 1. Positional arguments are refused, and `--now` takes an RFC3339
instant, by default the current time.

### gas-approver-keygen

```sh
sn25golive gas-approver-keygen --out KEY.json
```

| Flag | Meaning |
| --- | --- |
| `--out` | the new approver key file, created 0600 |

It writes a new Ed25519 key as JSON (`schema`
`urnetwork-operator-gas-approver-key-v1`, `public_key`, `seed`) and prints:

```
approver_public_key: <64 hex>
key_file: KEY.json (mode 0600)
The first admitted policy makes this key the permanent trust root of its scope st-operator-gas-v1:<chain>:<genesis>:<no_id>.
```

### gas-policy, dry run

```sh
sn25golive gas-policy --params PARAMS.json --dry-run [--now RFC3339]
```

| Flag | Meaning |
| --- | --- |
| `--params` | the unsigned policy JSON, the approval request, at most 64 KiB |
| `--dry-run` | validate and print the digest the approval covers; write nothing |
| `--now` | the instant of the validity checks |

`PARAMS.json` is decoded strictly into the server's `StOperatorGasPolicy`:
unknown fields and a second JSON value are refused, `signature` must be `""`,
and the server's approval-request validation applies. It is also refused with
`maximum_intent_attempts` above the server's 3 attempts per intent, or once
`valid_until_unix` has passed. A dry run takes no key and no outputs. It prints:

```
policy_sha256: <the digest the approver signs>
scope: st-operator-gas-v1:<chain_id>:<genesis_hash>:<no_id>
valid: <valid_from> .. <valid_until>
limits: gas <n>, fee <wei> wei/gas, tip <wei> wei/gas, intent <wei> wei, lifetime <wei> wei, attempts <n>/intent <n>/lifetime
note: <role> <address> pins an empty original history (no retained intent in the database)
```

A note says when signing is refused until `valid_from`, and each account's
note says whether it pins an empty or a nonempty original history. The launch
request `sn25-mainnet-gas-params.json` prints
`policy_sha256: e2de5acf1ef30de0b5c00f0e9cb5291db1b56b5fe6d0a17a524bb65806b5d64d`.

### gas-policy, signing

```sh
sn25golive gas-policy --params PARAMS.json --approver-key KEY.json \
  --out-policy POLICY.yml --out-authority AUTHORITY.yml [--now RFC3339]
```

| Flag | Meaning |
| --- | --- |
| `--params`, `--now` | as for the dry run |
| `--approver-key` | the key file from `gas-approver-keygen`: a regular file with no group or other permissions, at most 4,096 bytes |
| `--out-policy` | the new st.yml fragment holding the signed top-level `operator_gas_policy`, mode 0644 |
| `--out-authority` | the new `config/<env>/operator-gas-authority.yml`, mode 0644 |

The three are all required. It signs the policy, checks both outputs with the
server's authority parser and the policy's verification as the st.yml loader
decodes them, and only then writes them. If the authority cannot be written,
the policy output is removed. It prints the dry run's summary, with
`approver_public_key: <hex>` after `policy_sha256`, then:

```
out_policy: POLICY.yml
out_authority: AUTHORITY.yml
```

Merge the fragment's `operator_gas_policy` key into st.yml.

A re-signed policy cannot lift an operation's spent signatures or per-intent
liability, which its first signature recorded. An account whose sends all stop
at one unsigned intent, stranded before server commit 8d63033c9, is freed as
`docs/operator/st-stranded-nonce.md` describes.

### verify-config

```sh
sn25golive verify-config --policy policy-v1.yml --expect-policy-hash 0xHASH --out verify.yml
```

| Flag | Meaning |
| --- | --- |
| `--policy` | the approved protocol policy, such as `sn/deploy/mainnet/policy-v1.yml` |
| `--expect-policy-hash` | the approved 0x policy hash; a policy with another hash is refused |
| `--out` | the new verify.yml, created 0600 |

It generates a new 32-byte egress hash key and an Ed25519 seed for
`server_key_id` 0, copies the policy's verify section into `settings`, and
runs the server's own verify.yml loaders on the result before it writes it.
It prints:

```
profile: <network profile>
policy_hash: 0x<hash>
egress_hash_key_id: <id>
server_key_id: 0
verify_public_key_hex: <hex>
verify_public_key_base64: <base64>
out: verify.yml (mode 0600)
```

### st-check

```sh
sn25golive st-check --st st.yml [--profile mainnet] [--authority AUTHORITY.yml] \
  [--sn sn.yml] [--v2-activation ACTIVATION.json] [--now RFC3339]
```

| Flag | Meaning |
| --- | --- |
| `--st` | required: the candidate st.yml, at most 1 MiB. It holds private keys, which are read and never printed |
| `--profile` | `mainnet` (the default) or `testnet` |
| `--authority` | the operator-gas-authority.yml, at most 16 KiB, to verify the embedded policy's signature and pin against |
| `--sn` | the sn.yml, at most 32 KiB, to check the payout identity and readiness digest against |
| `--v2-activation` | the output of `bringyourctl st v2-activation` on the server this st.yml is for, at most 16 KiB |
| `--now` | the instant of the policy validity checks |

It validates st.yml with the server loader's checks for the profile, as if the
profile were enabled. It stops with an error when st.yml is not one YAML
mapping, names neither `authority` nor `rpc_urls` (with the `testnet-` prefix
for testnet), or fails the loader's validation. Connection values are never
printed. Otherwise it prints one line per finding, `<kind>: <text>`:

- `ok`, `note`, `warn` and `FAIL` lines. Warnings cover unknown top-level keys,
  `lightnode-*` keys on mainnet, an empty `launch_readiness_sha256`, a missing
  or invalid attempt upload budget, `wallet_allow_unsigned`, and a policy not
  verified for lack of `--authority`.
- The gas policy's summary lines, as `gas-policy` prints them.
- With `--sn`, a note with the sn.yml `config_sha256`, earning identity digest
  and activation, then the result of the server's mainnet admission at the
  later of `--now` and the sn.yml cutoff. Admission compares the whole selected
  identity, the v2 addresses included.
- With `--v2-activation`, the server's settlement v2 activation latch. Once
  the server has recorded its pool's activation, it refuses to run an st.yml
  whose v2 pair is not the recorded one. An armed latch gives `ok` for an
  st.yml that names the recorded pair, and `FAIL` with the server's own
  refusal for one that lacks or changes it. A latch that is not armed gives a
  note that the server would not refuse an st.yml without the pair. A status
  of another deployment, or a file of another shape, is a `FAIL`. Without
  `--v2-activation` a note says that st.yml was not checked against the latch.
- `identity` lines: the profile, chain id, genesis hash, netuid and no_id; the
  deployment id, policy hash and deploy block; the coordinator, settlement
  vault and reserve sink; when st.yml declares them, `settlement_v2_address`
  and `vault_v2_address` in the lowercase form st.yml and sn.yml declare; the
  deposit hotkey and any treasury hotkey; the deposit, root and artifact
  signer addresses; and the deposit tiers, zero-rate action and epoch cap.
- `receipt_public_config_sha256`: the server's public projection
  `urnetwork.st.public_config.v1` of the file as it will run, with `enabled`
  true and `launch_readiness_sha256` empty.

The last line is `result: ok`. With any `FAIL` line it exits 1 with
`<n> check(s) failed`.
