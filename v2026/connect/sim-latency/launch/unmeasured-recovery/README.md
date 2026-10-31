# Inactive unmeasured-start recovery

This standalone, standard-library-only administrative command is dry-run by
default. It never launches the evaluator. Applying recovery additionally requires
the exact pinned configuration, independent zero-measurement proof, and prospective
owner approval explicitly granting the fresh-clock exception. Historical start
records and every failed artifact are retained; no claim or journal is deleted.
The original deadline is acknowledged as historical, not relabeled as preserved.
The second seed, all identities and input bytes, source pins, and frozen evaluation
budgets remain unchanged.

## Tests

Ordinary developer and non-root CI runs can use:

```sh
go test ./connect/sim-latency/launch/unmeasured-recovery
```

Only `TestActualUid1000LegacyLockExceptionLeavesRootProtectionStrict` explicitly
skips when non-root. That is not production qualification. Under root it really
changes the owner of a synthetic temporary lock to uid 1000, exercises the exact
lock exception, and verifies ordinary root-owned path protection remains strict.
It does not touch the live scoring lock or substitute mocked ownership.

Mandatory root qualification uses the pinned Go 1.27.3 toolchain, all tests, the
race detector, three full passes, zero failures, and **zero skips**. For example,
from the server repository root, replacing only the toolchain placeholder:

```sh
sudo env GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  /absolute/path/to/go1.27.3/bin/go test -race -count=3 -json \
  ./connect/sim-latency/launch/unmeasured-recovery
```

This revision has 31 top-level tests. Qualification must bind the exact source
manifest and inspect the actual test census; success from an earlier source
revision does not qualify a later revision.

Tests and qualification confer no permission to invoke the operational CLI,
inspect private runtime inputs, create approval, apply recovery, change claims or
journals, or restart evaluation. Those actions require separate explicit authority.
