# Host configuration renderer

`go run ./cli/competitionhost --input=<plan.json>` renders the exact closed
Docker, containerd and nft documents used by the sub-evaluator admission code.
Use `--input=-` for stdin. It does not install files, execute host commands,
contact services, load main config/vault, or qualify a host.

Input has `boundary` (`competitionagent.RuntimeResourceBoundary`) and `scope`
(`competitionagent.RuntimeResourceScope`). Scope uses its exported Go field
names, including `BaseDirectory`, `StateDirectory`, `WorkDirectory`,
`ScratchDirectory`, `TrustedOperatorUid`, `WorkerUid` and `WorkerGid`.

Leave only the three output document hashes empty when planning new files.
The output includes their real SHA-256 hashes and completed boundary metadata.
An existing nonmatching hash refuses rather than silently changing authority.
Document content is base64 in JSON; preserve its exact decoded bytes.

Every result has `runtime_qualified: false`. Protect installed assets and obtain
native filesystem, namespace, network, resource, cleanup and host-admission
evidence separately. A renderer result cannot replace any of those gates.
