# Fixed public-pilot input construction

The owner-approved replacement pilot uses the prospectively fixed public seed
`20261030` and the reduced 100 ns/byte reference. This small Go helper calls the
same pinned `controller.GenerateSimLatencyWorkload` as the frozen native
operator; it does not duplicate provider serialization or change scoring policy.

Three independent four-test race processes passed: **12 top-level passes,
zero failures or skips**. The separately built Go 1.27.3 native helper is
`daa33481f4dc78ea9c371f15276231dc3d0bbc786ccc124ae151aef068285244`.
The exact source, imported-controller resolution, dependency closure and integrity
checks are recorded in [the terminal receipt](TERMINAL-RECEIPT.txt).
Go source is stored as inert `.go.txt` evidence here; its original packet names
remain in `SOURCE-PINS.sha256` and `ROOT-RECIPE.txt`. `MANIFEST.sha256` authenticates
the renamed publication files.

After independent source/build/test review, Root dispatched the helper once at
02:29:57 UTC on CPUs 0,2 with a 4 GiB hard limit, no swap and an empty LOCAL
configuration/vault environment. The [unit journal](construction-journal.log)
records the invocation and clean deactivation, not the piped stdout report.
Root observed exit 0 and the three-file report, then separately rehashed the
files, checked their private modes and verified the exact request projection.
The [construction record](CONSTRUCTION.json) records those observations.

Only eight request fields changed: job/round identifiers, the public seed label,
provider path/hash, patch path/hash and artifact directory. Every other JSON
value is unchanged, including R9, the 26.9% practical margin and 10,800-second
cap. The new request has no prepopulated baseline. Inputs are Root-owned 0600
files in an exclusively created 0700 directory; no overwrite or partial retry is
allowed.

This was deterministic **public input construction, not measurement or hidden
seed generation**. It does not qualify the separate operator, admit a pilot,
establish regression direction, accept calibration or enable production scoring.
The native operator must still independently authenticate the generated workload
and source census before preparing a complete prospective plan.
