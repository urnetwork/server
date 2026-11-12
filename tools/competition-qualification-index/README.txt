Private qualification-index regression module; no DB, Docker, network, native
agent or production materialization. Tests create only owned temporary Git
repositories with synthetic names and payloads. Run with pinned Go1.27.4,
GOWORK=off and offline dependencies. Ordinary/race count3 required.

The tested predicate.sh is sourced by the prospective materializer v3 after an
exact SHA check. Tests exercise those actual shell functions with real Git,
not a second implementation. The caller supplies a bounded run_owned wrapper,
core.fileMode=true and its exclusive private verification index. No function
updates source content or stages modified bytes. update-index refresh writes
only the private stat cache after content comparison; --ignore-missing permits
partial checkouts, not a completed-tree publication.

Required tests include the original fresh read-tree/diff-files false positive,
same-length real content mismatch, present executable-mode mismatch, partial
missing-only checkout, known skip-worktree baseline plus prefix-sibling missing
and present-file rejection, and assume-unchanged not bypassing authentication.
The first test must prove old quiet diff exit1 on identical actual blob hashes,
then new predicate exit0 with unchanged contents and object identities.

No test source contains production identities or captured data. Failure output
must not dump the private materializer source. Final physical source cleanliness
and ten-repository completion still require separately approved native v3 proof.
