# Provisional reference submissions

These patches exercise the exact current editable surface,
`connect/resident_contract_manager.go`, and are inputs to the noise and
separability campaign. They are not accepted competition references yet.

- `noop.patch` changes only a comment and should reproduce the base score.
- `worse.patch` adds a deterministic 25 ms delay to every live
  `HasActiveContract` call and should rank worse than the no-op.
- `better.patch` replaces as many as two pair-direction database queries with
  one source-or-destination query and filters the requested unordered pair. Its
  cache key is also made unordered so both ID orderings match the bulk lookup;
  a deterministic regression test protects that invariant. It is
  production-valid and plausibly faster under the current workload, but the
  official campaign—not this rationale—decides whether it is actually better.

The current development target is base Git SHA
`472e6ae1c0054452c7cf9cac9736325dfbcdd616`, evaluator-base image
`sha256:20e5700deffad5e5f32922f2318c5f3b3670b54f0493b61f4a5196ebea600c65`,
and patch-policy SHA-256
`2dba553cd94d6d901e0fc590fd147d3e39273b41c24317e987b1bbf479382460`.
The build and tests authenticate these provisional identities. When the public
season `BASE_SHA`, policy, or builder changes, regenerate and reauthenticate all
three patches rather than carrying hashes forward.

All three inputs passed the real local offline build path, including structural
patch authentication, a clean deterministic Git commit, networkless `go vet`,
compile-only package checks, simulator build provenance, embedded image
identity, authenticated cache reuse, and the hardened discarded-stage build
contract. Candidate package initialization runs unprivileged and cannot mutate
trusted final-image files. The development-only image IDs are:

- no-op: `sha256:c3cdba0cda79d23b2da78cf96c097b68963a35a9e37d6e006cfef04c8eaba931`;
- worse: `sha256:d9ad4a6dd14fbca1b133656757695f0baf58b217d487ae6e6e2e0eb211ac4e2e`;
- better: `sha256:25ba429f4b4e6eb77559334542e6af79a6c8a070bdee8b8e735306a9f3c76571`.

`manifest.json` pins each patch, candidate commit, image tag/digest/key, and
simulator digest. These successful local builds do not establish performance
ordering and are not production image publication.

Acceptance still requires, on the qualified authoritative host:

1. the frozen season base and exact allowlist;
2. the official independent-seed set and calibrated replicate policy;
3. no-op, worse, and better ranking in that order on at least 19/20 seeds; and
4. retained build, run, scoring, accounting, resource, and hash artifacts.
