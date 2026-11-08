# sidecar-migrations

The sidecar migration groups' two tools (DESIGN.md §19.7, D-OPS-6). Run them from
a server checkout.

## promote

```
go run ./local/sidecar-migrations promote MG1
```

A group's Gn-out registers it with this one edit. It moves the group's entries,
its marker last, from `db_migrations_sidecar_pending.go` into the migrations slice
in `db_migrations.go`, ahead of the pending tail, and removes the group's
function, its owners' part functions and its call in `sidecarPendingMigrations`.
It prints the versions the group now holds. From then on the group never changes.

- It refuses while an earlier group is still pending: groups register in order.
- It refuses a part that uses a name only the pending file declares, since that
  would not compile on a release leg once moved.
- It moves each entry's text byte for byte, raw SQL strings included, so each
  migration keeps the identity a local database recorded while it was pending.
- It writes nothing unless every check passes and both files parse. It compiles
  nothing: after it, build both legs (`go build .` and
  `go build -tags sidecar_pending .`), run the server's migration tests, and move
  the group's names out of `db_migrations_sidecar_release_test.go`.

The marker moves with its group, so code gated on `SidecarMgNLinked` turns on in
release builds with no other edit.

## check-tables

```
go run ./local/sidecar-migrations check-tables
```

Compares the tables the groups create, pending or registered, with xops's
`sidecar-tables.conf`, the one list behind D-OPS-18's dumps, and each line with
§2.8's `metadata_list.yml`. It fails on:

- a table a group creates with no line, or a line naming another group;
- a line whose row §2.8's list lacks or withdrew, or whose row's store does not
  name the table;
- a table marked `dump` whose ttl is at most 7 d, or one marked `none` whose ttl is
  past 7 d (§19.6, D-OPS-18). A ttl counted from an event of its own, "30 d after
  expiry", bounds nothing, so either mark stands for it.

It reads `../xops/...` and `../sidecar/metadata_list.yml` beside the server
checkout, so it runs in the monoroot; `-tables` and `-metadata` point elsewhere.
A table a group creates through SQL held in a constant declared elsewhere is not
seen: the groups write their SQL inline.

Neither tool reads vault, dials a database or touches the network.
