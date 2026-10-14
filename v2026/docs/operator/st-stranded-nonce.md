# Freeing an account nonce stranded before the gas-reservation fix

The durable transaction engine signs the operator's EVM transactions with two
accounts, the root key and the deposit key. Every send first reconciles the
account's lower nonces, so one nonce that can never be signed blocks every
later send of that account. The root key is shared by the v1 tasks and the v2
driver.

Before server commit 8d63033c9 ("Take a new nonce only together with its first
attempt's gas reservation"), the engine reserved a new intent's nonce before
the operator gas policy decided whether it would sign the intent's first
attempt. A refused attempt left the intent holding its nonce with no
signature, and the account's sends stopped at it. Since that commit a refusal
takes no nonce. An intent stranded before the upgrade stays stranded.

## Recognizing a stranded intent

Every send of the account fails with the same refusal of one intent, for
example `operator gas allowance is unavailable or exhausted; original
liabilities retained: original logical intent <key>` or `unsigned
gas/fee/tip/intent envelope exceeds approval`. The account's first unresolved
intent is the one its sends stop at:

```sql
SELECT intent_id, intent_key, nonce, generation, status, attempt_count, error
FROM st_transaction_intent
WHERE from_address = lower('<account address>')
  AND status NOT IN ('finalized', 'reverted', 'canceled', 'superseded')
ORDER BY nonce, generation
LIMIT 1;
```

It is stranded when its status is `prepared`, its `attempt_count` is 0, and it
has no gas reservation:

```sql
SELECT count(*) FROM st_operator_gas_reservation WHERE intent_id = '<intent_id>';
```

The procedure below is only for that state. A nonce consumed outside the
engine can never retire an intent that holds a reservation or a signed
attempt: the engine keeps both as live liability, and the account stays
blocked for good.

An intent with a reservation and no attempt is not stranded. The process
stopped between the reservation and the signature, and the engine signs the
reserved bytes on the account's next send. On the root key it does so also
when the operation has since been spent or its close has expired: that send
reports the revert, and the next one goes out. On the deposit key, a deposit
reserved but not signed before its deadline stays refused (`pending original
signing outcome cannot become a different request`). No operator step frees
it; it needs a change to the engine.

## Intents that free themselves

- Refused for `maximum_gas`, the fee caps or the lifetime caps: a policy
  re-signed with limits that cover the attempt lets the engine sign it. A v1
  close past its close grace, a deposit past its deadline and a spent v2
  operation are instead cancelled with a 21,000-gas self-transfer, which fits
  any `maximum_gas`, once its fee is within the policy's caps.
- Refused because the operation's signatures (`maximum_intent_attempts`) or
  its per-intent liability are spent: a re-signed policy cannot lift them,
  since both caps are recorded at the operation's first signature, and the
  cancellation is refused for the same reason. Use the procedure.

## Procedure

1. Stop every send of the account, and keep them stopped until step 5: stop
   the taskworkers, which run the st tasks, and run no `bringyourctl st
   deposit`, `commit` or `finalize`. A running engine tries the stranded
   intent again on every send and signs it at its nonce as soon as its refusal
   lifts, for instance when an estimate drops below `maximum_gas`. If the
   self-transfer of step 3 then takes that nonce, the engine's signature can
   never be included, and the engine never retires an intent whose signed
   attempt has no receipt: the account stays blocked for good.
2. Confirm that the intent is stranded as above, and that the account's nonce
   at the finalized block equals the intent's `nonce`:
   `eth_getBlockByNumber("finalized", false)`, then
   `eth_getTransactionCount(<account>, <that block's number>)`.
3. Outside the engine, send one transaction from the account to itself at
   exactly that nonce, with value 0, a gas limit of 21,000 and a fee within
   the policy's `maximum_fee_per_gas_wei`. With Foundry, for example:
   `cast send <account> --value 0 --gas-limit 21000 --nonce <nonce>
   --rpc-url <url>`, signing with the account's key. The engine does not
   record this transaction, and it is not counted in the operator gas
   policy's liability.
4. Wait until it is finalized: the account's nonce at the finalized block is
   the intent's nonce plus one.
5. Start the sends again. The account's first send returns `st: account nonce
   <nonce> was consumed without a known canonical attempt` once and marks the
   intent `superseded`. The sends after it go out.

Do not delete or edit the rows of the intent or of the gas ledger.

In `controller/st_ledger_v2_gas_test.go`,
`TestStLedgerV2StrandedCloseIsFreedByConsumingItsNonce` runs this procedure,
and `TestStLedgerV2RecoveryRacingTheEngineStrandsTheRootKey` shows what
skipping step 1 does.
