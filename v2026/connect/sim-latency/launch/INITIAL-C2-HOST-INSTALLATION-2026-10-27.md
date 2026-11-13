# Initial C2 host and preparation files

File-only installation on sille completed on October 27, before any independent
seed measurement. It does not assert readiness or authorize activation.

The reviewed script `c2f52be4…` acquired the canonical scoring lock, checked the
unchanged current boot and old host-config digest, retained the old bytes and
atomically installed the new initial C2 profile. The old staging profile remains
at `/var/lib/urnetwork/competition-production-initial-host-provisioning-20261028-v1/old-host.backup.json`.

- Old host profile: `24ac920db965d52905e88d44b95259510caccbe37599a4eef6e28e617d5aff64`.
- Installed `/etc/urnetwork/competition-host.json`:
  `8df2d636c8745a8fc0ec10f1a75ee2d042eba4f5f1bd019a9f1cc856f4ed6b2f`.
- Actual static fingerprint:
  `08ee3a5a4acf0f4b8fabf6279d4257bd9fdc3314ffb9f59e87f2981556cb300d`.
- Boot: `f0e880cb-c124-4ed6-9edb-43669191d84e`.
- Installation receipt:
  `/var/lib/urnetwork/competition-production-initial-host-provisioning-20261028-v1/installation-receipt.json`,
  SHA-256 `c8841c80c2f202183122736d459628f55f4cbb36e117e8590bec636144442b57`.

All readiness-marker hashes remain empty. The exact reserved-target native R1
qualification must create its genuine evidence later; no placeholder marker was
written. No checker, qualifier, worker, supervisor, source activation, API call
or DB write was performed by this installation.

Separately installed, inactive preparation files:

| File | SHA-256 |
| --- | --- |
| `/etc/urnetwork/competition-production-20261028/preparation.json` | `8031dd2bdb508ea964a351fe0360166293a58323d68e3ec235adc5a22c59cb05` |
| `/etc/urnetwork/competition-production-20261028/qualification.json` | `5411b30e76ce2639f7ea85b3d5e6917750737ce8d7ff30451e10d7a3f319b08c` |
| `/usr/local/libexec/urnetwork/competition-production-preparation-1a1-37ca9bd4-20261028-v1/competitionprepare` | `37ca9bd47a177339c9e04cfa9b1702665fd7f3affd9509c66d0ce693880f23fa` |

The profiles are root-owned, mode 0400, single-link regular files. The wrapper
is root-owned, mode 0555, single-link, beneath a root-owned mode-0555 release
directory; its bytes match the independently qualified source closure. No
preparation command was run. The wrapper supplies the existing worker's exact
`1a1bf5ae…` / `1996ccd1…` provenance, not the source supervisor's different build.
The model broker and evaluator image remain unchanged.
