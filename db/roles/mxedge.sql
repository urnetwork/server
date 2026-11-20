-- mxedge's router's Postgres role (QD-992, QD-029, QD-1103; D-OPS-1, §15.4, §15.15, §19.4, §19.7, §2.8 mx-2 and
-- mx-3).
--
-- server/cli/mxedge --role router, the warp service mxedge, connects with this role, never with the platform's
-- pg.yml, which its block does not mount. The platform listener, the service mxedge-platform, is another block:
-- it mounts pg.yml for its report intake and never reads this role. The owner reviews this file and runs it once
-- in each environment's database, as a role that may create roles:
--
--     psql "<an admin connection to the database>" -f server/db/roles/mxedge.sql
--
-- psql asks for the role's password at \password, at the end, twice and unechoed, and sends the server only its
-- SCRAM verifier. So no password is ever in this file, a shell's history or the server's log. The same password
-- goes into the vault file pg-mxedge.yml, which warp/services/sidecar-blocks/mxedge.yml mounts and server's main
-- pool reads before pg.yml (server/db.go's pgServiceVaultResourceName):
--
--     authority: <host:port of the PgBouncer listener the platform's pg.yml names>
--     user: mxedge
--     password: <the password given at \password>
--     db: <a PgBouncer database entry that reaches the database as mxedge>
--
-- PgBouncer (xops' pgbouncer.ini.j2) authenticates its clients from userlist.txt, so the role needs a line there.
-- Its one database entry, bringyour, sets user=bringyour, and a client of that entry reaches Postgres as the
-- platform's role whatever user it logged in as, which would void every grant below. db must name an entry that
-- reaches Postgres as mxedge (question pg-edge-roles-pgbouncer-forced-user).
--
-- What the role may do is what the router's process reaches, read from its code, and nothing else.
-- server/db_edge_roles_test.go creates the role from exactly this file, runs the block's calls under it, and holds
-- every SQL statement the router's model code reaches to it, so a query added to either store fails that test
-- until this file grants it. The router's packages (server/mxedgehost and server/cli/mxedge's main) name model
-- only for model.NewMxEdgeStore and model.NewMxDirectMxStore (mxedgehost's TestRouterStoresWriteNoDirectMxOrDnsTable
-- holds it), and its ServiceHost's task queue schedules nothing.
--
--   - The block's start (server/sidecarhost.Run with Postgres, router.CheckStartupReadiness): the migration head,
--     from migration_audit's successful runs.
--   - The session records, MxEdgeStore.InsertConnectionRecords (model.WriteMailConnectionRecords): one insert a
--     batch into mail_connection_record, from the 10 s batch goroutine only (I-12).
--   - The store's probe, MxEdgeStore.Ping, the router's readiness (QD-1133): whether mail_connection_record is a
--     partitioned table, read from the catalog on the block's main pool (model's mailConnectionTablePartitionedInConn).
--     pg_class and to_regclass answer any role, so the read needs no grant.
--   - The direct MX's prober, mxDirectMxStore.ListDirectMx (MAIL-M7-23), in one read-only transaction: every
--     mail_direct_mx row, each network's label (dns_label, GetNetworkLabel's read) and each network's published
--     keys for mx (mail_host_key). Each probe's outcome goes to api's internal route on the edge bearer
--     (RecordDirectMxProbe, QD-1103), which writes mail_direct_mx, the zone and the routes, so this role writes
--     none of them; the zone's writers stay api and the dns block.
--
-- What it may not do:
--
--   - Make an hour's partition. A batch whose hour has none makes it through the partition registry's
--     EnsureRangeFor on the maintenance pool (model's insertConnectionRecordsCreatingHours), which the block does
--     not have: QD-992's rule gives the maintenance pool pg_maintenance.yml, or else the platform's pg.yml, and
--     never a service's file, and the block mounts none of them. Mounting either would hand the block a platform
--     role (D-OPS-1), so the fallback fails, and the batch with it, the closed direction. The registry's runway
--     task (server.MaintainPartitionRunways in taskworker, every 10 min) creates every hour from CreateBehind (1 h)
--     before now to AheadHours (6) after it, and a session's record carries its start, at most §15.4's 30 min
--     splice and a 10 s batch before it is written, so the runway has made every hour a batch writes while it
--     runs.
--   - Anything of §21's. The abuse sink (model.NewAbuseSmtpSink) is the platform listener's report intake's, in the
--     block mxedge-platform under pg.yml; the router builds no intake and no sink, so neither the sink's cases nor
--     its kick of the mail pass run here (ABUSE-M9-01's question, answered in server/db_edge_roles_test.go).
--   - Anything else: no write but that insert, no read of another column, no other table. The file grants nothing
--     to PUBLIC and revokes nothing from it, so what PUBLIC holds stays the role's too: in a database made by
--     PostgreSQL 15 or later that is USAGE on schema public and TEMPORARY on the database, never CREATE on the
--     schema, which an older database gave PUBLIC and the owner revokes there, if it still does.
--
-- So the router reports ready under this role: MxEdgeStore.Ping passes on the block's own vault, as
-- server/db_edge_roles_test.go holds. The same catalog read on the maintenance pool is the partition family's
-- Partitioned hook (model's mailConnectionTablePartitioned), which only the registry's runway and retirement call,
-- in taskworker under the platform's role, never the router.

\set ON_ERROR_STOP on

BEGIN;

CREATE ROLE mxedge WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

-- to connect, and to name the tables, where the database and the schema do not already give PUBLIC these
GRANT CONNECT ON DATABASE :"DBNAME" TO mxedge;
GRANT USAGE ON SCHEMA public TO mxedge;

-- the block's start: router.CheckStartupReadiness' migration head
GRANT SELECT (end_version_number, status) ON TABLE migration_audit TO mxedge;

-- the session records
GRANT INSERT (network_id, start_time, byte_count, result) ON TABLE mail_connection_record TO mxedge;

-- the direct MX's prober: its rows
GRANT SELECT (network_id, receiver_sidecar_id, address, state, verify_time) ON TABLE mail_direct_mx TO mxedge;
-- the direct MX's prober: each network's label, getNetworkLabelInTx
GRANT SELECT (network_id, root, kind, label) ON TABLE dns_label TO mxedge;
-- the direct MX's prober: each network's published keys for mx
GRANT SELECT (network_id, host, receiver_sidecar_id, spki_sha256, state, publish_time, retire_time) ON TABLE mail_host_key TO mxedge;

COMMIT;

\password mxedge
