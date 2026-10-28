# Review of FP2-MODERN

Reviewed 2026-10-28 against server `c20f2bc6fc89e189e8df834d8a2ce3a8bb17cc87` and connect `28bed463716b3d0838c9c8f66afd9c8540ad6154`. Proposal SHA-256: `febf54bac2f7ba826bbc68c8eb6dc8a53b264c5b88fe78ed179593f3544241b9`.

The predicate belongs in the exported score, and applying its refusal before live reads and quota accounting is the right approach. Keeping band membership separate from preference also preserves supply when modern providers are scarce. The proposal needs four corrections before implementation: strict preference must reach the page reader, probe eligibility needs the actual Online policy inputs, named classification must distinguish a Network provider from a client with no provider key, and client rollout must wait for published data.

This review treats the recorded owner decisions as requirements. Network-only declarations, the existing modern-evidence definition, the unchanged response, capability-blind location lists, and export-time freshness are accepted choices. It does not propose reopening them.

## 1. [P2] A draw wrapper cannot enforce modern priority ahead of family facets

**Proposal:** [Priority within each band](FP2-MODERN.md#priority-within-each-band), lines 336–343; deferred page ordering, lines 627–633.

The proposal requires all modern candidates in a band to precede non-modern candidates, including across family facets, but retains the existing load and quota. Those mechanisms can stop before a modern candidate is read:

- [`newClientScoreCursor`](model/network_client_score_cursor.go#L40) appends each whole facet group in preference order, shuffling only within the group.
- The [native cursor](model/network_client_score_native.go#L523) does the same.
- [`refillPool`](model/network_client_location_model.go#L6596) stops when ordinary eligible membership satisfies the quota. Under the proposal, non-modern natives still count toward that quota.

A deterministic counterexample is a target with 1,000 eligible non-modern dualstack natives and 100 modern IPv4-only natives. A request for 20 that accepts both facets loads the first 1,000 dualstack rows and stops. The wrapper sees no modern providers and returns non-modern providers, despite modern supply exceeding 9% of the target. This contradicts both strict modern-before-family precedence and the assertion that a 1,000-row load covers targets with at least 2% modern supply. Even an unbiased random sample would give a probability, not a guarantee.

**Required change:** reconcile the priority guarantee with the accepted read budget. If priority covers all eligible supply in a band, promote modern-aware page selection into this design: traverse modern pages across all requested facets before traversing non-modern pages across those facets, and count modern survivors through live validation until quota or known exhaustion. The proposed page partition and boundary metadata can support this without duplicating the whole pool. Bind metadata to the page generation and define compatibility when it is absent or incomplete. The legacy union count document is a gob `[]int` ([reader](model/network_client_score_read.go#L128)); preserve that format for old readers when adding metadata.

The reader also needs an explicit policy when bounded legacy reads cannot establish modern exhaustion. Strict global priority, an unchanged finite sample, and unrestricted non-modern fill cannot all be guaranteed. If the intended scope is only the loaded sample, the wrapper is sufficient, but record that narrower scope in the priority decision and remove the target-wide 2% coverage claim. The current wording promises both.

**Regression coverage:** exercise both native and legacy readers with the cross-facet population above; include modern providers beyond the initial load, modern candidates rejected by live checks, and genuine modern exhaustion followed by non-modern fill. Tests restricted to an already-loaded candidate map would miss this failure.

## 2. [P2] The widened probe does not currently have the inputs that define Online

**Proposal:** [The capability probe covers every Online provider](FP2-MODERN.md#the-capability-probe-covers-every-online-provider), lines 427–434.

Replacing `qualityBeforeModernGate || speedBeforeModernGate` with `decision.online` does not by itself preserve the stated `no_success` and `low_success` exclusions. [`FilterProviderCapabilityProbeEligible`](model/provider_capability_probe.go#L114) builds `newProviderCountFilterForClients`, whose [constructor](model/provider_status_model.go#L229) leaves `onlineSuccessBaseline` unset. The [eligibility loop](model/provider_capability_probe.go#L133) also passes no published country to `egressFacts`.

An absent baseline exempts countries from the URL-success rule ([`egress_online_success.go`](model/egress_online_success.go#L146)). The current native-only probe predicate rejects these providers through native URL requirements; the proposed Online predicate would admit them. Consequently the measured population would include providers the export places in no bucket, and the population/capacity estimate would omit that extra work.

**Required change:** load the published country and the export's country success baseline for the probe cohort before evaluating Online, following the [provider-status path](model/provider_status_model.go#L843). Specify the same behavior as the exporter when that baseline is absent or stale. Keep the existing Public-key requirement.

**Regression coverage:** compare probe eligibility with exported Online membership for a non-exempt provider with no URL success, one below the Online success threshold, an exempt-country provider, an unprobed provider, an ordinary failing-ratio provider that remains Online, and a hard exclusion. Merely constructing a unit-test filter with a baseline would not verify that the production loader supplies it.

## 3. [P2] Absence of a Public key does not prove a named client is a Network provider

**Proposal:** [Named providers](FP2-MODERN.md#the-request-filter), lines 276–289, applying the definition at lines 182–185.

The proposed named query reads Public-key presence, evidence and a capability declaration. It cannot distinguish a current Network-only provider from a client with neither a Public nor a Network key.

This matters because [`SetProvideWithCapabilities`](model/network_client_model.go#L3044) deliberately retains the capability row when a subsequent provide has neither key ([write condition](model/network_client_model.go#L3164)). For example, a client can announce Public + post-quantum, then replace its keys with a return-traffic-only provide. The old declaration remains. A classification based only on `!hasPublic && declaration` would now accept it as a modern Network provider, even though it has no Network key and its retained declaration came from its Public provide.

The named path does not obtain the export's provider-key eligibility implicitly: [`appendSpecProviders`](model/network_client_location_model.go#L6093) appends a named id after its exclusions, without reading current keys. In particular, the ordinary Speed/Online exclusions do not supply the missing Network-key check.

**Required change:** include current Network-key presence in the named query. Use the declaration branch only for `!hasPublic && hasNetwork`; refuse clients with neither provider key. When Public is present, continue to require measured evidence even if a Network key is also present. Keep these fields in the same query snapshot.

**Regression coverage:** announce Public + post-quantum, replace it with a return-only provide, verify that the declaration remains, and confirm that both named-only and named-plus-discovery modern requests refuse the id. Include valid Network-only and Public-plus-Network controls.

## 4. [P2] Client rollout needs a completed-export prerequisite

**Proposal:** [Rollout](FP2-MODERN.md#rollout), lines 512–531.

Step 2 correctly includes taskworkers and readers, but step 5 releases clients once every reader runs the new server. Upgrading the processes does not establish that the index has been rewritten. The proposal explicitly makes old pages decode as `Modern == false`, so newly released end-to-end clients can receive empty discovery results from healthy new readers backed by old data.

Score publication is asynchronous. Its [task](taskworker/work/network_client_location_work.go#L20) schedules a later run, permits a 120-minute execution budget, and publishes with a five-hour TTL. Publication spans separate batches and finishes only after the [export workers join](model/network_client_location_model.go#L5765). A successful reader deployment is not a publication barrier; an older export still running during deployment can also rewrite pages without the field.

**Required change:** gate client release on all readers being upgraded, all old exporters being drained, and one complete successful export from the new code after the last old writer. Verify representative modern-only discovery answers, including native, union/fallback and forced paths, before releasing clients. Record the export completion/generation used for this check. Either-order server deployment can remain.

**Validation:** cover new readers with old pages and mixed publication, and make the rollout checklist explicitly reject client release until the new export completes. Do not rely on elapsed deployment time or reader versions alone.

## Clarifications and validation to retain

- **Probe capacity is an estimate.** Lines 437–445 assume one measurement per provider per 72 hours and say settings enforce freshness. Missing or inconclusive evidence remains due ([page query](model/provider_capability_probe.go#L52)); unsuccessful measurements can recur each sweep ([sweep advancement](capprobe/sweep.go#L235)). [Settings validation](capprobe/settings.go#L175) checks the refresh age plus the minimum sweep interval, not actual sweep duration. Treat 690 measurements/hour as a baseline estimate. The proposed 32/128 settings remain a reasonable starting experiment; acceptance should include attempted versus recorded measurements, measured sweep duration, evidence-age tail and shard balance, including unsuccessful attempts.
- **Modern evidence does not establish that a required-encryption client can seal.** The probe intentionally records negotiated post-quantum sessions as positive even when they remain unsealed ([`reading.evidence`](capprobe/reading.go#L155), [probe documentation](capprobe/README.md#L87)). Preserve the owner's existing classifier, but document this limitation and retain a test that an unsealed positive cannot cause plaintext delivery. Discovery can still select a provider that fails session validation; the availability claim should not imply successful sealed transport for every modern result.
- **The generator guarantee is about requests it makes.** Fixed `spec.ClientId` destinations and restored destinations bypass discovery ([connect generator](../connect/ip_remote_multi_client_api.go#L799)). The proposal's wording at lines 67–70 is appropriately narrower than “every destination is modern”; retain that distinction. Do not route probe-pinned destinations through modern-only discovery, since capability probes must reach providers with no modern evidence.
- **Make status tests match the explicit gate override.** Lines 420–421 allow `not_modern` with priority off when the gate is explicitly true; the test bullet at line 590 says no provider reports it with priority off. Test all priority/gate combinations and limit that negative assertion to gate-off cases.

The November 11 config deadline remains independent of these corrections. This review does not change configuration or implement the proposal.

## Review method

Three parallel code reviews examined selection/index publication, probe policy, capability storage, named-provider handling, generator behavior and rollout. Findings above are based on source inspection and explicit counterexamples. No FP2 implementation changes or regression tests were run for this design review. The proposed regressions are implementation acceptance criteria, not claimed passing results.
