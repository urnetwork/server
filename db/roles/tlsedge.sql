-- tlsedge's Postgres role (QD-992, QD-029; D-OPS-1, §13.5, §13.9, §19.4, §19.7, §2.8 tls-1 and tls-3).
--
-- server/cli/tlsedge connects with this role, never with the platform's pg.yml, which its block does not mount.
-- The owner reviews this file and runs it once in each environment's database, as a role that may create roles:
--
--     psql "<an admin connection to the database>" -f server/db/roles/tlsedge.sql
--
-- psql asks for the role's password at \password, at the end, twice and unechoed, and sends the server only its
-- SCRAM verifier. So no password is ever in this file, a shell's history or the server's log. The same password
-- goes into the vault file pg-tlsedge.yml, which warp/services/sidecar-blocks/tlsedge.yml mounts and server's main
-- pool reads before pg.yml (server/db.go's pgServiceVaultResourceName):
--
--     authority: <host:port of the PgBouncer listener the platform's pg.yml names>
--     user: tlsedge
--     password: <the password given at \password>
--     db: bringyour_tlsedge
--
-- db must name bringyour_tlsedge, the role's own PgBouncer database entry (xops' pgbouncer.ini.j2; QD-1134), which
-- reaches Postgres as tlsedge. The platform's entry, bringyour, sets user=bringyour, so a client of it reaches
-- Postgres as the platform's role whatever user it logged in as, which would void every grant below. PgBouncer
-- authenticates its clients from userlist.txt, which xops copies from vault/main/pgbouncer/userlist.txt, and logs in
-- to Postgres as an entry's user with that user's line, so the file needs the role's line: "tlsedge" and the password
-- given at \password. PgBouncer does not tie a user to an entry, so the line would also let a client that holds the
-- password name bringyour. It waits until the HBA rules that admit tlsedge to its own entry alone are deployed
-- (QD-1136), and until then the block cannot connect, the closed direction.
--
-- What the role may do is what tlsedge's process reaches, read from its code, and nothing else.
-- server/db_edge_roles_test.go creates the role from exactly this file, runs the block's store calls under it, and
-- holds every SQL statement the block's model code reaches to it, so a query added to the store fails that test
-- until this file grants it. The process's only Postgres is model.NewTlsEdgeStore: no startup check reads
-- migration_audit (server/cli/tlsedge's sidecarhost.RunOptions holds no Postgres), and its ServiceHost holds no
-- task queue. The store's calls come from the engine's names loop, its kills loop and its 10 s record batches,
-- never from a connection (I-12).
--
--   - The label map, TlsEdgeStore.NameSnapshot and NamesSince (model.readDnsLabelMap, in one read-only
--     transaction): dns_label's names, dns_root's restore serial, and for each network with a name its zone's
--     suspension (network_dns_zone) and its withheld hosts' owners (network_dns_record), §12.9's suspensions.
--   - The custom hosts, CustomHosts (model.DnsCustomHostActiveRows): network_dns_custom_host's rows.
--   - The kills, Suspensions (model.GetTlsSuspensions): every row of network_tls_suspension.
--   - The records, InsertConnectionRecords (model.WriteTlsConnectionRecords): one insert a batch into
--     tls_connection_record. Its ON CONFLICT names the key, connection_id and end_time, which Postgres reads to
--     find the conflict, so the role reads those two columns and no other.
--
-- What it may not do:
--
--   - Make an hour's partition. A batch whose hour has none makes it through the partition registry's
--     EnsureRangeFor on the maintenance pool (model's insertConnectionRecordsCreatingHours), which the block does
--     not have: QD-992's rule gives the maintenance pool pg_maintenance.yml, or else the platform's pg.yml, and
--     never a service's file, and the block mounts none of them. Mounting either would hand the block a platform
--     role (D-OPS-1), so the fallback fails, and the batch with it, the closed direction. The registry's runway
--     task (server.MaintainPartitionRunways in taskworker, every 10 min) creates every hour from CreateBehind (1 h)
--     before now to AheadHours (6) after it, and a record ends at most MaxFutureSkew (5 min) ahead and is written
--     within its 10 s batch, so the runway has made every hour a batch writes while it runs; an outage of the
--     runway task longer than six hours stops the records, not the routing.
--   - Anything else: no write but that insert, no read of another column, no other table. Retiring hours, their
--     roll-up into tls_connection_day and the §21.9 hold checks are taskworker's, under the platform's role. The
--     file grants nothing to PUBLIC and revokes nothing from it, so what PUBLIC holds stays the role's too: in a
--     database made by PostgreSQL 15 or later that is USAGE on schema public and TEMPORARY on the database, never
--     CREATE on the schema, which an older database gave PUBLIC and the owner revokes there, if it still does.

\set ON_ERROR_STOP on

BEGIN;

CREATE ROLE tlsedge WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

-- to connect, and to name the tables, where the database and the schema do not already give PUBLIC these
GRANT CONNECT ON DATABASE :"DBNAME" TO tlsedge;
GRANT USAGE ON SCHEMA public TO tlsedge;

-- the label map: readDnsLabelMap's names
GRANT SELECT (root, label, network_id, kind, state, change_serial) ON TABLE dns_label TO tlsedge;
-- the label map: getDnsRootStateInTx, the root's restore serial
GRANT SELECT (root, serial, restore_serial, restoring, restore_incident_id) ON TABLE dns_root TO tlsedge;
-- the label map: a network's zone suspension (§12.9)
GRANT SELECT (root, network_id, suspend_reason, suspend_until) ON TABLE network_dns_zone TO tlsedge;
-- the label map: a network's withheld hosts (§12.9), by kind
GRANT SELECT (root, network_id, kind, owner) ON TABLE network_dns_record TO tlsedge;

-- the custom hosts: getDnsCustomHostRowsInTx's columns, for DnsCustomHostActiveRows' state = active
GRANT SELECT (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, map_version, ready_time, create_time) ON TABLE network_dns_custom_host TO tlsedge;

-- the kills
GRANT SELECT (network_id, host, reason, until_time) ON TABLE network_tls_suspension TO tlsedge;

-- the records, and the key their ON CONFLICT reads
GRANT INSERT (connection_id, network_id, host, start_time, end_time, byte_count, visitor_hash) ON TABLE tls_connection_record TO tlsedge;
GRANT SELECT (connection_id, end_time) ON TABLE tls_connection_record TO tlsedge;

COMMIT;

\password tlsedge
