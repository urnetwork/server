# Bootstrap preparation notes

These are setup observations, not evidence that bootstrap or activation ran.

## Disk headroom

At approximately 15:50 UTC, 3,786 regenerable, owner-root, single-link Go build
cache entries last used before 12:30 UTC were removed from the task-specific
`/var/cache/urnetwork/competition-go-build`. Only canonical hashed `-a`/`-d`
cache files were selected; no Go compiler/linker was active. Their total logical
size was 3,934,346,943 bytes; the sorted path/size/mtime census digest was
`19e095274bcd130f93d6ea0acd632b153590a8e65144276b175c32ccd26b26f7`.
Recent cache entries (about 5.1 GB), module caches, source, compiled test
binaries, frozen images, measurements and evidence were not removed. The
removed cache entries are recoverable by recompilation, not as measurements.
Free disk afterward was 17,114,955,776 bytes.

Read-only Git tree inventories count about 1.3 GB of frozen checkout blobs
across the ten repositories; existing Git object stores occupy about 3.2 GB.
These are estimates of clone/check-out demand, not a quota or guaranteed future
remote-pack size. The revised unexecuted bootstrap launcher requires 15 GiB
free at entry and budgets an estimated 6 GiB for staging. The separate 12 GiB
compiler reservation must first be released after qualification completes.
Actual bootstrap disk use must be observed; source cloning is not evaluation.

## Root Git transport

The first bounded root `ls-remote` probe, with an empty environment and the
existing Git configuration, failed host-key verification. It performed no
bootstrap, DB write or source change. Root has no configured SSH trust/key pair.

A second read-only probe succeeded against the frozen server branch using the
existing operator identity and trust store explicitly:

```text
/usr/bin/ssh -o BatchMode=yes -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile=/home/by/.ssh/known_hosts
  -o IdentitiesOnly=yes -i /home/by/.ssh/id_ecdsa
```

Known-hosts file SHA-256:
`86c8c14dc42951ab47ca8f04d699a0b477342781c3020ed09f81d3a96f8a8bce`.
Public-key file SHA-256:
`d8761733455d29ebe1a482c58bd78d0b397ed004cd9ef890cd88b096687de56a`.
No private-key bytes/hash were printed or copied, and no host-key check was
disabled. The native transport command preserves this explicit SSH helper;
untrusted local-source Git operations continue stripping transport credentials.

The updated **unexecuted** launcher is retained at
`/home/by/urnetwork/temp/production-bootstrap-static-20261027.v1/bootstrap-once.PROPOSED.sh`,
SHA-256 `de46ad7566e597203e561ae3d25031f9cfd0377d990ab0cc11415126c0bb06bf`.
It still needs root's execution authorization, finished qualification, fresh
ref/config checks and the canonical scoring slot. Config/main publication
requires its exact expected head; do not reset or overwrite unrelated updates.
