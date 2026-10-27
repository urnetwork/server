# Native fee settlement

`stnativefees settle --intent ID --request /absolute/request.json
--request-sha256 sha256:DIGEST --transaction 0xHASH --budget 5m` invokes the
approved original verifier and durably settles one retained operator signature.
The request must contain original proof references accepted by `sn-mainnet
verify-admitted-native-fees`; imported reports are not accepted.

Provision two independent protected public Config resources before use:

- `native-fee-denomination-authority.yml` pins the denomination approver's
  public key, exact signed policy digest and operator/network identity.
- `native-fee-denomination-policy.yml` contains that signed policy, including
  the approved verifier ELF, native semantics roots, original runtime and
  native block interval, and an explicit reduced Rao-to-wei ratio.

No policy, conversion ratio, signature, live chain outcome or executable pin
is provided by the command. Missing approval or unknown original proof keeps
the complete prior fee ceiling. The command performs no RPC, signing or chain
send and never loads `st.yml` or operator signer keys. The protected public
authority selects the exact deployment and the model checks its original intent
scope. It writes only the verified idempotent settlement through the model owner.
A lost command response is recovered by repeating the exact original request;
changed outcomes or authorities cannot overwrite an existing settlement.

These source changes and their deterministic Go fixtures remain unexecuted.
The newly added CLI package must be included in the selected Server build;
the selected `sn-mainnet` executable must include `verify-native-fee-outcome`.
