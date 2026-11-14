# The demand deposit task

Every epoch each subnet operator owes a demand deposit: the previous epoch's usage
(bytes and users) priced at the rates in force for this epoch, at the conviction
snapshot's tier. The taskworker's `StDeposit` task pays it as early as it can and
keeps trying until it lands or the epoch ends. The rates come from the signed price
sheet; the policy document and its hash never change, and they are the baseline
wherever no signed entry is in force. At zero price nothing is owed: the task writes
one skipped `deposit` row, sends no transaction and raises nothing.

A priced deposit has three steps, each a durable transaction of the deposit key:

1. Fund. When the alpha wallet's stake on the deposit hotkey is short of what staging
   will draw, the funder (sn `demand`) converts USDC on Solana into alpha: a swap,
   a bridge send, an unwrap of the wTAO and a stake. The funder sizes and spends only
   after the deposit's own limits pass, and only for the chain's current epoch.
2. Stage. A `transferStake` moves the principal into the coordinator's staging
   position for this operator.
3. Deposit. `deposit()` moves the staged alpha into the reserve and credits the epoch.

Each attempt writes `deposit_push` and `deposit` publish rows. A row with status
`confirmed` ends the epoch's chain, as do `skipped` (nothing owed, already paid, or a
passed epoch) and `refused` (below the transfer minimum, terminal for the epoch). A
`failed` row does not: the chain retries. Retries never count against a budget.

| Outcome | Next attempt |
| --- | --- |
| Something under way: earlier transactions settling, a conversion step in flight, source totals not published yet | 1 minute |
| The price guard refused the swap | 5 minutes |
| The price is unknown | 5 minutes |
| Any refusal: a wallet short of USDC, SOL or gas, the epoch's USDC budget, a hotkey that cannot take stake | 15 minutes |

## Configuration

### demand.yml

The vault resource `demand.yml` holds the operator's two wallets and the conversion
limits. Without it nothing converts, and a deposit is paid from what the alpha wallet
already holds or fails as before. A `demand.yml` that does not parse, or whose alpha
wallet is not the deposit signer on the deposit hotkey that `st.yml` names, converts
nothing either. It is logged as `THE DEMAND DEPOSIT FUNDER IS UNAVAILABLE`. It never
stops a deposit the wallet can pay without converting. The file is read once per
process: a change takes effect at the taskworker's restart.

```yaml
alpha_wallet:
  address: ...       # the deposit signer's EVM address
  private_key: ...   # the deposit signer's key, the same as st.yml's deposit key
  hotkey: ...        # the deposit hotkey st.yml names
usdc_wallet:
  address: ...       # Solana
  private_key: ...
conversion:
  enabled: true      # without it a short wallet is a refusal, and no USDC moves
  # Optional, with their defaults:
  # max_usdc_per_epoch: 6000
  # max_price_gap: 0.01         # Jupiter's price at most 1% beyond Coinbase's ask
  # max_slippage_bps: 100
  # min_sol: 0.06
  # max_bridge_fee_sol: 0.05
  # alpha_buffer_bps: 100
  # gas_reserve_tao: 0.2        # at least 0.02
  # pool_price_tolerance: 0.01
  # jupiter_api_key: ...
```

`helius.yml` holds the Solana RPC key the funder uses. Neither file's values ever
reach a log, a publish row or the status command: endpoints are reduced to their
scheme and host.

### Wallet funding

- **SOL on the USDC wallet:** 0.06 SOL, plus about 0.025 SOL per conversion for the
  swap, the bridge send and the accounts they open. Below `min_sol`, or below what
  one route can spend, the funder refuses before any USDC moves.
- **USDC:** a few epochs' worth of deposits, and under about $10k. Jupiter's quotes
  worsened with the taker's USDC balance, so a larger wallet pays more per alpha. The
  funder converts at most `max_usdc_per_epoch` for one epoch. It converts nothing
  unless the whole shortfall fits, since part of a deposit does not pay it.
- **TAO on the alpha wallet:** `gas_reserve_tao` (0.2 TAO) stays unstaked for gas. The
  conversion refills it, and the deposit's own sends draw on it. Below about 0.0021 TAO
  the funder refuses (`needs TAO for gas`) before TAO sets out for the wallet.

### st.yml

`deposit_epoch_cap_rao` is the server's own ceiling on one epoch's deposit. A priced
amount above it is an error and nothing is sent. Raise it to at least the largest
amount a signed entry can require before the entry takes effect.

The coordinator has two more caps of its own, set by the owner in a policy version:
an epoch cap per operator and a lifetime campaign cap on every deposit's reserve
together. A deposit above either reverts. The monitor pages when a scheduled version
or a published entry breaks the validators' rule for them
(`policy-deposit-caps-from-epoch-N`), and when the campaign headroom runs low.

### The operator gas policy

The deposit key's sends are charged against the signed operator gas policy at their
full ceilings. With prices on, one conversion per epoch adds a stake and an unwrap to
the close, staging and deposit. At the 30 gwei fee cap that is about 1.9·10^19 wei a
year, and about 2.4·10^19 with two conversions an epoch. When re-signing the policy
for prices:

- set `maximum_lifetime_liability_wei` to at least 2.5–3·10^19 (the launch runbook's
  floor of 2·10^19 lasts about a year at one conversion an epoch);
- check the validity window. A window that ends before the liability does stops every
  send at its end, deposits and the sweep's close alike;
- keep `maximum_lifetime_attempts` well above the few hundred signatures a year this
  needs.

## Deploying

Apply the migration from the merged main only, after it is pushed. Migration
versions are positional.

1. Apply the demand deposit migration, `st_demand_schema.go`. It creates four tables
   and changes no existing one: `st_demand_state`, `st_demand_event`,
   `st_demand_price_sheet` and `st_ledger_v2_epoch_usage`. Apply it before deploying
   the taskworker. Under settlement v2 the usage steps write
   `st_ledger_v2_epoch_usage` at zero price too, because the first priced epoch's
   deposit prices the last zero-price epoch's usage.
2. Deploy the taskworker and `bringyourctl`.
3. Deploy the monitor. Its migration contract for these tables follows the
   migration's position.

### The monitor's network access

The monitor reads the price sheet itself, from its published address
`https://ur.xyz/price.yml`, with an HTTPS GET at the subnet families' cadence. Allow
that egress. Everything else goes over access it already has. The settlement v2 audit
reads vault2 over the existing chain endpoints, so `sn.yml` must name `VaultV2`.
Statements come from the existing evidence origins, and `st_demand_event` rows through
the producer database read.

## Status

`bringyourctl st demand-status` prints, as JSON:

- the wallets as the funder reads them: the alpha wallet's stake, TAO, wTAO, its gas
  reserve, and whether the hotkey can take stake; the USDC wallet's USDC, SOL and TAO;
  and the conversion's state, including any swap or bridge send in flight;
- the price in force for the current epoch: whether it is a signed entry, the policy
  baseline or unknown; the pinned signer; the entry in force and the next one
  scheduled;
- the last usage statement recorded under settlement v2: its epoch, bytes, users,
  rows and content hash.

It signs nothing and fetches nothing. The price shown is the last verified sheet,
which every deposit attempt refreshes.

`bringyourctl st deposit` runs one attempt for the current epoch by hand.
`--alpha_rao=<rao>` names the amount instead of sizing it. A named amount converts
no USDC unless `--convert` is given, and only the chain's current epoch can be
funded either way.

## Alerts

The taskworker's alerts are log lines. The three that need a person also write an
`st_demand_event` row, which the monitor pages on (`subnet-demand-event`).

| Line | Meaning | Action |
| --- | --- | --- |
| `DEMAND DEPOSIT PRICE UNKNOWN, nothing is sent` | No verified sheet is held and the published file cannot be taken. | Restore the published address; the attempt retries every 5 minutes. |
| `DEMAND DEPOSIT PRICE SHEET PROBLEM, pricing from the verified sheet held` | The published file was not taken (unreadable, unverified, or dropping an entry), or the stored copy could not be used and the published file replaced it. | Check the published file. A file that does not verify may hide a real entry. |
| `fund demand deposit: ...` (error level) | A refusal before any USDC moved: USDC, SOL or gas short, conversion disabled, the epoch's USDC budget, a changed bridge, a refused simulation. | Fund or fix what it names; the next attempt is in 15 minutes. |
| `DEMAND DEPOSIT UNPAID, the price guard has refused its conversion for ...` | Jupiter has priced beyond `max_price_gap` over Coinbase's ask for 3 hours of one epoch. | Usually a passing market condition. Widen the gap only deliberately. |
| `DEMAND DEPOSIT REFUSED FOR THE EPOCH` | The deposit, or what is left to stage, is below the chain's transfer minimum. Validators will zero the pool for the epoch. | Nothing can be sent this epoch; check the price and the source totals. |
| `[st]v2 ALERT demand_swap_needs_person` | USDC left the wallet and no TAO came for it within 30 minutes of signing. The budget stays spent. | Find the swap's USDC from its signature in the event's data. Do not reset the funder's state. |
| `[st]v2 ALERT demand_funder_alert` | A swap's TAO is short of its floor a minute after signing, was paid below its floor, or never came. Nothing more is bought until 30 minutes after signing. | Check the wallet's Solana history. |
| `[st]v2 ALERT demand_hotkey_not_ready` | A dry run says the deposit hotkey cannot take stake. Nothing moved. | Check the hotkey's association; compare `subnet-deposit-hotkey-owner`. |
| `[st]v2 ALERT v2_usage_statement epoch N` | Epoch N can have no usage statement: its committed root declares it unrecoverable, no root was committed in its root window, or more than one was. The next epoch's priced deposit cannot be sized. | Settle it by hand with the validators' operators; nothing retries it. |
| `retiring unsigned staging intent ...` | A staging transfer reserved but never signed before its epoch rolled was cancelled at its nonce, so the deposit key's later sends go on. | None; see `st-stranded-nonce.md`. |
| `is priced by an entry published after its zero-price skip: its deposit is seeded` | An entry for the current epoch appeared after the epoch was skipped at zero price. | None: the deposit is paid before the deadline. Publish entries earlier next time. |

The monitor's classes for the task:

| Class | Meaning | Action |
| --- | --- | --- |
| `subnet-price-sheet` | The published file does not verify (page), or has been unreadable for 6 hours (warn). | Fix the published file or the site. |
| `subnet-policy-schedule`, reason `policy-deposit-caps-from-epoch-N` | From epoch N, a coordinator version's epoch cap is below the cap in force (the signed entry's, or the document's), or its campaign cap is below the larger of the document's campaign cap and its epoch cap. Validators stop accepting deposits there. | The owner schedules caps that fit, effective by epoch N, or the signer publishes a correcting entry. |
| `subnet-campaign-headroom` | The campaign cap's headroom covers fewer than 8 epochs at the latest required deposit (warn), or fewer than 2 (page). | The owner schedules a higher campaign cap before it runs out. |
| `subnet-usage-statement` | A v2 source epoch's statement is missing past its deadline, or does not verify, while a price is in force or scheduled. | Check the usage steps and the evidence store. |
| `subnet-deposit-landing` | A priced deposit has not landed 900 blocks (3 hours) after its source totals existed. | Read the task's refusal in the taskworker log, or run `st deposit` by hand. |
| `subnet-deposit-hotkey-owner` | The deposit hotkey's owner is not the deposit signer's EVM mirror coldkey: a foreign owner pages, an unset owner warns at zero price and pages once a price is in force or scheduled. | Associate the hotkey from the deposit signer's mirror coldkey; investigate a foreign owner before staking anything more. |
| `subnet-demand-event` | One of the three events above. | As above. |
| `subnet-operator-deposit-mismatch` | A deposit does not match its signed usage at the rates in force. | Notify the operator with the audit record. |

## The usage statement (settlement v2)

A v2 epoch has no signed payout artifact, so the operator attests its usage in a
usage statement, published to the evidence store, from which the next epoch's deposit
is sized. Users are counted once, as soon as the epoch's boundary is finalized, and
never again. The statement is built once the epoch's root commit is mirrored: the one
`DistributionCommitted` event in `[end, end + root window]`. It is built from the
provider chunks that root's header lists for the epoch, which are what validators
match it against. A rebuild of the rows before the commit changes nothing a validator
reads. The pool has no weight before the commit anyway, so the wait costs nothing.
Every retry signs and publishes the same bytes.

## The sweep hold

The configured phase C sweep's v1 close is sent by the deposit key and has no second
chance inside its grace. From 100 blocks before the sweep epoch's end until that close
is confirmed or its grace has passed, no conversion starts. The hold stops only the
conversion. Staging and the deposit of alpha the wallet already holds go on, as they
did beside the close before there were conversions. A deployment without `demand.yml`
is not affected at all. The hold covers that one boundary, and prices are not expected
to be on by then.

## Deadlines

A deposit must land by its epoch's last block, the coordinator's deadline. The task
reserves no new staging transfer within 150 blocks (30 minutes) of it. A dead worker's
task is reclaimed within 5 minutes, and the staging and the deposit can each wait 8
minutes for finality. No conversion runs past the same point: the funder runs under a
deadline at the staging window's close, and stops there wherever it is. Its journals
resume at the next epoch. No deposit is reserved, signed or replaced once the latest
block has reached the deadline. A deposit reserved before then is left to the expiry
handling, which signs its reserved bytes once the finalized head has passed the
deadline: they revert and free the nonce. A staging transfer already signed is never
cancelled. Its bytes are sent again, and if it lands after the epoch rolls, the next
epoch's deposit draws on the staged alpha.

A view of the chain behind the epoch, from a node that lags, never ends an epoch's
deposit chain. The attempt writes nothing and comes again in a minute. Only a view
past the epoch ends it.

## A price published late

An epoch's chain ends at its zero-price skip. If an entry for that epoch appears
later, validators audit the epoch at the new price as soon as they fetch it. So while
the current epoch's only concluding row is the zero-price skip, the sync turn keeps a
check of the sheet scheduled about every 15 minutes (`StDepositLateEntry`). Once a
verified sheet prices the epoch, the check seeds its deposit chain again. That chain
then retries on its own schedule, as in the table above. If it ends without a
confirmed or refused row, the next check seeds it again. Nothing is seeded once the
epoch's deadline is reached or the epoch has rolled. A late entry leaves the deposit
little time, so it is a safety net, not a way to schedule prices.

## Turning prices on

In this order:

1. **Coordinator caps.** The owner schedules a coordinator policy version, under the
   same policy hash, whose epoch cap is at least the first entry's cap and whose
   campaign cap is at least the larger of the document's campaign cap and the new
   epoch cap. It must take effect no later than the first priced entry's epoch, and
   leave the campaign headroom for many epochs. The monitor pages on a schedule that
   breaks this, naming the first epoch validators would stop at.
2. **Validators.** Every validator runs the build that applies the signed sheet, the
   cap rule and the v2 usage statement before any price can be in force.
3. **Usage statements.** Under settlement v2, the epoch before the first priced epoch
   must have its statement published by the end of its root window. The statement
   needs that epoch's root commit, so check the commit first, then
   `last_usage_statement` in `st demand-status`, and `subnet-usage-statement`.
4. **Operators.** `demand.yml` in place and bound, wallets funded as above, `st.yml`'s
   `deposit_epoch_cap_rao` raised, and the gas policy re-signed.
5. **Entries.** Publish each signed entry well ahead of its epoch, so that operators
   and validators have fetched it before the epoch starts.
