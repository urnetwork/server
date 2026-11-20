-- smtpedge's Postgres role (QD-992, QD-029; D-OPS-1, §14.8, §14.9, §19.4, §19.7, §21.5, §21.7; ABUSE-M9-01).
--
-- server/cli/smtpedge connects with this role, never with the platform's pg.yml, which its block does not mount.
-- No other file in server or xops grants this role anything; this one does, as the same owner step as
-- tlsedge.sql and mxedge.sql. The owner reviews this file and runs it once in each environment's database, as a
-- role that may create roles:
--
--     psql "<an admin connection to the database>" -f server/db/roles/smtpedge.sql
--
-- psql asks for the role's password at \password, at the end, twice and unechoed, and sends the server only its
-- SCRAM verifier. So no password is ever in this file, a shell's history or the server's log. The same password
-- goes into the vault file pg-smtpedge.yml, which warp/services/sidecar-blocks/smtpedge.yml mounts and server's main
-- pool reads before pg.yml (server/db.go's pgServiceVaultResourceName):
--
--     authority: <host:port of the PgBouncer listener the platform's pg.yml names>
--     user: smtpedge
--     password: <the password given at \password>
--     db: bringyour_smtpedge
--
-- db must name bringyour_smtpedge, the role's own PgBouncer database entry (xops' pgbouncer.ini.j2; QD-1134), which
-- reaches Postgres as smtpedge. The platform's entry, bringyour, sets user=bringyour, so a client of it reaches
-- Postgres as the platform's role whatever user it logged in as, which would void every grant below. PgBouncer
-- authenticates its clients from userlist.txt, which xops copies from vault/main/pgbouncer/userlist.txt, and logs in
-- to Postgres as an entry's user with that user's line, so the file needs the role's line: "smtpedge" and the
-- password given at \password. PgBouncer does not tie a user to an entry, so the line would also let a client that
-- holds the password name bringyour. It waits until the HBA rules that admit smtpedge to its own entry alone are
-- deployed (QD-1136), and until then the block cannot connect, the closed direction.
--
-- What the role may do is what smtpedge's process reaches, read from its code, and nothing else.
-- server/db_edge_roles_test.go creates the role from exactly this file, runs the block's calls under it, and holds
-- every SQL statement the block's model code reaches to it, so a query added to one of them fails that test until
-- this file grants it. Every read is off a preamble's path (I-12), and each grant names only the columns its
-- statements name.
--
--   - The block's start (server/sidecarhost.Run with Postgres, router.CheckStartupReadiness): the migration head,
--     from migration_audit's successful runs.
--   - The engine's store (model.NewSmtpEdgeStore), as sidecar/smtpedgeservice calls it in this process. The halt
--     reload, each 60 s, and the halt ladder's reads of a network's halts read smtp_halt (ActiveHalts,
--     FindHalts), and the ladder sets a halt (SetHalt, an upsert of smtp_halt by its id). The send log's batches
--     and a cut's record insert into smtp_send (InsertSends), and the ladder counts a network's cut sends
--     (CountSends) and its recipients for the hard-bounce rate (RecipientCount) from it. The store's other calls are
--     other processes': complaints, aggregates, DKIM keys and the join (the report intake's, in mxedge-platform),
--     the reconcile (taskworker's), a halt's lift (api's appeal), and the status hints' sending clients, which need
--     a Push this block's host is not given.
--   - The send gate's facts (cli/smtpedge's gate.go, model.GetSmtpGateFacts), loaded on a miss: the network's
--     label (dns_label), when its mail began (mail_network), its verified domains (network_domain), its strict
--     custom domains while strict_envelope is on (network_dns_custom_host), when it left Pro or Team
--     (model.NetworkDowngradeTime: transfer_balance and subscription_renewal), and its zone's report token
--     (network_dns_zone).
--   - §21.5's mail breaker, with the facts (model.GetMailBreakerState): the breaker's actions and their case
--     (abuse_action, abuse_case).
--   - §21.7's abuse gate, from memory loaded off the path (model.NewAbuseGate over NewAbuseHardDenySnapshot): the
--     hard-deny set (network_abuse_state), and each asked network's standing, trusted and network_verified
--     (network, network_abuse_state, abuse_case's barred, transfer_balance, network_member's owners, the owners'
--     verified sign-ins in network_user_auth_password and network_user_auth_sso, and network_domain,
--     stripe_invoice_payment_failure and abuse_action for trusted). Only the columns those reads name: no password
--     hash, no address, no other column of a user's.
--
-- What it may not do:
--
--   - Kick §21's mail pass (ABUSE-M9-01). The ladder's last rung, abuseSmtpSink.RequestSmtpSuspension, pulls the
--     pending pass to now with an UPDATE of pending_task (model.KickAbuseMailPass, task.KickTasks) in an owned
--     transaction (server.OwnedTx), which needs the direct maintenance resource pg_maintenance.yml. The block mounts
--     none, and mounting the platform's would hand it a platform role (D-OPS-1), so the kick fails before it
--     reaches Postgres, whatever this file grants; and this role is not granted UPDATE on pending_task, which would
--     let an edge reschedule every task queued on the platform. The request fails, and
--     urnetwork_smtpedge_suspension_requests_total counts it failed. The halts that reached the rung are written
--     before it, and taskworker runs the pass each minute (controller.DefaultAbuseMailSettings' PassInterval),
--     reading every halt §21 has not recorded and judging §21.4's rung on §21's own records. So a failed kick
--     delays a suspension by at most that minute and loses none.
--   - Open §21's cases. The sink's OpenSmtpCase is the report intake's, which only mxedge-platform builds, under
--     pg.yml, and a ladder's only for a complaint event. smtpedge's ladder is given cuts and hard bounces alone, so
--     it never opens one.
--   - Make a day's partition of smtp_send. A batch whose day has none makes it through the partition registry's
--     EnsureRangeFor on the maintenance pool (model's insertConnectionRecordsCreatingHours), which the block does
--     not have: QD-992's rule gives the maintenance pool pg_maintenance.yml, or else the platform's pg.yml, and
--     never a service's file, and the block mounts none of them. Mounting either would hand the block a platform
--     role (D-OPS-1), so the fallback fails, and the batch with it, the closed direction. The registry's runway
--     task (server.MaintainPartitionRunways in taskworker, every 10 min) creates every day from CreateBehind (3 h)
--     before now to AheadDays (3) after it, and a send starts at most MaxFutureSkew (5 min) ahead, so the runway
--     has made every day a batch writes while it runs.
--   - Anything else: no write but those two tables', no delete, no read of a complaint, an aggregate, a DKIM key or
--     a send's other columns, no other table. The file grants nothing to PUBLIC and revokes nothing from it, so
--     what PUBLIC holds stays the role's too: in a database made by PostgreSQL 15 or later that is USAGE on schema
--     public and TEMPORARY on the database, never CREATE on the schema, which an older database gave PUBLIC and the
--     owner revokes there, if it still does.

\set ON_ERROR_STOP on

BEGIN;

CREATE ROLE smtpedge WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

-- to connect, and to name the tables, where the database and the schema do not already give PUBLIC these
GRANT CONNECT ON DATABASE :"DBNAME" TO smtpedge;
GRANT USAGE ON SCHEMA public TO smtpedge;

-- the block's start: router.CheckStartupReadiness' migration head
GRANT SELECT (end_version_number, status) ON TABLE migration_audit TO smtpedge;

-- the halts: the reload's and the ladder's reads, and SetHalt's upsert by halt_id
GRANT SELECT (halt_id, network_id, label, trigger, start_time) ON TABLE smtp_halt TO smtpedge;
GRANT INSERT (halt_id, network_id, label, trigger, start_time) ON TABLE smtp_halt TO smtpedge;
GRANT UPDATE (network_id, label, trigger, start_time) ON TABLE smtp_halt TO smtpedge;

-- the sends: InsertSends, its ON CONFLICT on the key, and the ladder's counts
GRANT INSERT (send_day, send_id, network_id, client_id, sending_address, recipient_domain, mx_host, start_time, end_time, byte_count, message_id_hash, x_ur_ref_key_id, recipient_count, message_byte_count, tls_version, tls_level, result, retry_of_send_id) ON TABLE smtp_send TO smtpedge;
GRANT SELECT (send_day, send_id, network_id, start_time, result, recipient_count) ON TABLE smtp_send TO smtpedge;

-- the send gate's facts
GRANT SELECT (network_id, root, kind, label) ON TABLE dns_label TO smtpedge;
GRANT SELECT (network_id, create_time) ON TABLE mail_network TO smtpedge;
GRANT SELECT (network_id, domain, verify_time) ON TABLE network_domain TO smtpedge;
GRANT SELECT (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, map_version, ready_time, create_time) ON TABLE network_dns_custom_host TO smtpedge;
GRANT SELECT (network_id, subscription_type, end_time) ON TABLE subscription_renewal TO smtpedge;
GRANT SELECT (root, network_id, report_token) ON TABLE network_dns_zone TO smtpedge;

-- the facts' and the abuse gate's tier, trusted and network_verified reads of a network's balances
GRANT SELECT (network_id, pro, paid, tier, grant_kind, start_time, end_time) ON TABLE transfer_balance TO smtpedge;

-- the mail breaker's actions, the abuse gate's counted actions, and their cases
GRANT SELECT (action_id, case_id, network_id, category, kind, target, until_time, by_machine, review_state, reviewer_id, review_reason, review_time, applied_time, mailed_time, create_time) ON TABLE abuse_action TO smtpedge;
GRANT SELECT (case_id, join_kind, join_key, barred) ON TABLE abuse_case TO smtpedge;

-- the abuse gate: the hard-deny set and a network's standing
GRANT SELECT (network_id, state, case_id, until_time) ON TABLE network_abuse_state TO smtpedge;
GRANT SELECT (network_id, create_time, admin_user_id) ON TABLE network TO smtpedge;
-- the abuse gate: network_verified's owners and their verified sign-ins
GRANT SELECT (network_id, user_id, role, pending) ON TABLE network_member TO smtpedge;
GRANT SELECT (user_id, auth_type, verified) ON TABLE network_user_auth_password TO smtpedge;
GRANT SELECT (user_id, auth_type) ON TABLE network_user_auth_sso TO smtpedge;
-- the abuse gate: trusted's unresolved payment failures
GRANT SELECT (network_id, resolve_time) ON TABLE stripe_invoice_payment_failure TO smtpedge;

COMMIT;

\password smtpedge
