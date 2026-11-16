// The provider work session journal for provider clients only (owner,
// 2026-11-16; DBDISK-STABILIZE.md S4, reversing the "not chosen" of
// PARTITION-MAINTENANCE-PLAN.md C4).
//
// provider_work_session_append, the journal's only writer, appended an admit
// and a retire for every connection of every client. The journal has no reader
// but the append's own retire lookup, and no signer is deployed
// (model/provider_work_session.go), so this restricts the append to provider
// clients: most connections belong to clients that are payers, chiefly the
// hosted proxy window clients, and their events were most of its growth.
//
// # Provider clients
//
// A provider client holds a provide key in friends and family (2) or public
// (3) mode, the modes under which another network's contract can reach it:
//   - a contract to a destination is admitted only in a mode the destination
//     advertises (controller resolveNonCompanionProvideMode); the relationship
//     between two networks is public, or friends and family, and a network
//     contract stays inside one network;
//   - a payout goes only to the endpoint that is not the contract's origin, and
//     only when its network is not the origin's (model
//     contractProviderPayoutRowsSql), so it is earned only under those two
//     modes, the companion return contracts included, which settle on the
//     provider of the contract they return.
//
// Network (1) and Stream (4) keys do not make a provider: every client
// registers Stream for return traffic, and hosted window clients register
// Network as well (connect's multi-client window; the sdk's provide modes), so
// a test on any key would journal every client again. The modes are the
// values of model.ProvideModeFriendsAndFamily and model.ProvideModePublic, a
// stored enum; a model test pins them.
//
// # What is appended
//
//   - An admit or a baseline: only for a provider client, read in provide_key
//     when the event is appended.
//   - A retire: when the bounded retire lookup finds its connection's admit,
//     whether or not the client still provides, so a connection admitted while
//     its client provided still retires with the admit's network after the
//     client stops providing, pauses or is deactivated (each deletes the keys).
//     Otherwise only for a provider client, as before: the unbounded fallback,
//     and without an admit (a connection that opened before its client
//     provided, or one whose admit was in the dropped legacy partition) a
//     retire without network id.
//
// A provider client's events are exactly what the append wrote before. A
// client that does not provide gets no head, takes no endpoint fence in the
// append, and its retire costs the bounded lookup and the key read. The
// fallback lookup reads every week, so a client that does not provide skips
// it: connection ids are minted by server.NewId at connect, and the bounded
// lookup always reaches their admits.
//
// Deploying the signer later needs a migration that journals every client
// again: a complete reservation needs both endpoints' heads, and the source of
// a paid contract is a payer (model providerWorkEndpointHeadInTx; sn
// payoutartifact endpointComplete).
package server

// The partition preparation's append (providerWorkSessionPartitionAppendSql)
// for provider clients only, as the file comment describes. The lookups that
// decide whether an event is appended come first; the endpoint fence, the head
// and the event follow unchanged. It replaces the function's body only, with
// no table lock, so it applies before or after any deploy: the running
// release's trigger performs the append and ignores its result, which is NULL
// when nothing is appended.
const providerWorkSessionProviderAppendSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION provider_work_session_append(client uuid,connection uuid,event_kind text,extender uuid)
 RETURNS bigint LANGUAGE plpgsql AS $session_append$
DECLARE next_sequence bigint; owner_network uuid;
BEGIN
 IF event_kind='retire' THEN
  SELECT network_id INTO owner_network FROM provider_work_session_event
   WHERE client_id=client AND connection_id=connection AND kind='admit'
   AND observed_at>=provider_work_session_admit_floor(connection) ORDER BY sequence DESC LIMIT 1;
  IF NOT FOUND THEN
   IF NOT EXISTS(SELECT 1 FROM provide_key WHERE client_id=client AND provide_mode IN (2,3)) THEN
    RETURN NULL;
   END IF;
   SELECT network_id INTO owner_network FROM provider_work_session_event
    WHERE client_id=client AND connection_id=connection AND kind='admit' ORDER BY sequence DESC LIMIT 1;
  END IF;
 ELSE
  IF NOT EXISTS(SELECT 1 FROM provide_key WHERE client_id=client AND provide_mode IN (2,3)) THEN
   RETURN NULL;
  END IF;
  SELECT network_id INTO owner_network FROM network_client WHERE client_id=client;
 END IF;
 PERFORM provider_work_endpoint_lock(client);
 INSERT INTO provider_work_session_head(client_id,sequence) VALUES(client,1)
 ON CONFLICT(client_id) DO UPDATE SET sequence=provider_work_session_head.sequence+1
 RETURNING sequence INTO next_sequence;
 INSERT INTO provider_work_session_event(client_id,sequence,network_id,connection_id,kind,observed_at,extender_id,transaction_id)
 VALUES(client,next_sequence,owner_network,connection,event_kind,clock_timestamp() AT TIME ZONE 'UTC',extender,txid_current());
 RETURN next_sequence;
END;
$session_append$;
`
