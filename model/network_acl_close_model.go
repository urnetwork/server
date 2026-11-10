package model

// §8.6's one switch at the contract layer (D-ACL-8, ACL-M4-03): after api appends a Suspend, a revocation or a cascade
// park, CloseAclSubjectContracts runs off the request path and closes the subject's open contracts. It finds the
// subject's clients, every generation a sidecar binding names (sidecar_identity_binding) or the client itself, and
// every client whose stored acl_subject_client_id names one of them (D-ACL-3's window children), and for each open
// contract one of them holds, as source or destination:
//
//	forwarding  removes the pair's contract-hole member, so forwarding stops within the 5 s positive-cache bound
//	            (connect/CODESTYLE.md's contract-hole rule), a member already gone a no-op
//	close       adds close requests for both parties through AddClientCloseRequests, so each gets CloseSequences
//	            from its resident (server/model/client_close_model.go), the existing mechanism and no new message;
//	            a request already there a no-op
//
// Each step is idempotent, so a retried run closes nothing twice and a run killed half way is finished by the next.
// Contracts a narrowed rule no longer allows are not closed here: they lapse within 60 minutes (§8.6). A Suspend also
// owes the subject's rev/ object, WriteAclSuspendRevocationInTx, in the transaction that appends it (§7.8).
//
// Reads Postgres for the clients and contracts and holds no connection across Redis.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar"
)

// What one CloseAclSubjectContracts run found and closed.
type AclSubjectContractsClosed struct {
	// The subject's clients, sorted.
	ClientIds []server.Id
	// The open contracts they held, sorted.
	ContractIds []server.Id
}

// One open contract and its two parties.
type aclSubjectContract struct {
	contractId    server.Id
	sourceId      server.Id
	destinationId server.Id
}

// The subject's clients in the network: a sidecar's bound clients of every generation, or the client the id names,
// with every client whose stored subject is one of those. Empty when the network holds neither.
func aclSubjectClientIds(ctx context.Context, networkId server.Id, subjectId server.Id) []server.Id {
	bound := `SELECT NULL::uuid AS client_id WHERE false`
	if server.SidecarMg1Linked() {
		bound = `
			SELECT sidecar_identity_binding.client_id
			FROM sidecar_identity_binding
			INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
			WHERE sidecar_identity_binding.sidecar_id = $2 AND sidecar_identity.network_id = $1
		`
	}
	children := ``
	if server.SidecarMg2Linked() {
		children = `
			UNION
			SELECT network_client.client_id
			FROM network_client
			WHERE network_client.network_id = $1 AND network_client.acl_subject_client_id IN (SELECT client_id FROM subject)
		`
	}
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				WITH subject AS (
					`+bound+`
					UNION
					SELECT network_client.client_id
					FROM network_client
					WHERE network_client.client_id = $2 AND network_client.network_id = $1
				)
				SELECT client_id FROM subject
				`+children+`
				ORDER BY client_id
			`,
			networkId,
			subjectId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
}

// The open contracts the clients hold as source or destination, each once, in contract order: one indexed read per
// side (transfer_contract_open_source_id, transfer_contract_open_destination_id).
func aclSubjectOpenContracts(ctx context.Context, clientIds []server.Id) []aclSubjectContract {
	contracts := []aclSubjectContract{}
	if len(clientIds) == 0 {
		return contracts
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT contract_id, source_id, destination_id
				FROM transfer_contract
				WHERE open = true AND source_id = ANY($1)
				UNION
				SELECT contract_id, source_id, destination_id
				FROM transfer_contract
				WHERE open = true AND destination_id = ANY($1)
				ORDER BY contract_id
			`,
			clientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				contract := aclSubjectContract{}
				server.Raise(result.Scan(&contract.contractId, &contract.sourceId, &contract.destinationId))
				contracts = append(contracts, contract)
			}
		})
	})
	return contracts
}

// Closes the subject's open contracts, as the file's header says. subjectId is a sidecar_id or a client_id.
func CloseAclSubjectContracts(ctx context.Context, networkId server.Id, subjectId server.Id) (*AclSubjectContractsClosed, error) {
	closed := &AclSubjectContractsClosed{ClientIds: aclSubjectClientIds(ctx, networkId, subjectId)}
	contracts := aclSubjectOpenContracts(ctx, closed.ClientIds)
	closeContracts := map[server.Id][]server.Id{}
	closePeers := map[server.Id][]server.Id{}
	for _, contract := range contracts {
		closed.ContractIds = append(closed.ContractIds, contract.contractId)
		// the pair's member goes first: forwarding stops on it alone, whatever the parties do with the close
		if err := applyContractHoleEvent(ctx, contract.contractId, contract.sourceId, contract.destinationId, "remove"); err != nil {
			return closed, err
		}
		closeContracts[contract.sourceId] = append(closeContracts[contract.sourceId], contract.contractId)
		closeContracts[contract.destinationId] = append(closeContracts[contract.destinationId], contract.contractId)
		closePeers[contract.sourceId] = append(closePeers[contract.sourceId], contract.destinationId)
		closePeers[contract.destinationId] = append(closePeers[contract.destinationId], contract.sourceId)
	}
	var closeErr error
	for clientId, contractIds := range closeContracts {
		recovered := server.HandleError(func() {
			AddClientCloseRequests(ctx, clientId, contractIds, closePeers[clientId])
		})
		if recovered != nil && closeErr == nil {
			closeErr = fmt.Errorf("close requests for %s: %v", clientId, recovered)
		}
	}
	return closed, closeErr
}

// Owes a Suspend's rev/ object for its subject in tx (§7.8): evs and gkm refuse the subject's tokens issued before its
// not_before, so a suspended sidecar's 15-minute tokens end at once; tokens issued after an Unsuspend pass.
func WriteAclSuspendRevocationInTx(ctx context.Context, tx server.PgTx, networkId server.Id, subjectId server.Id, at time.Time) {
	WriteRevocations(ctx, tx, []*SidecarRevocation{{
		NetworkId: networkId,
		Kind:      sidecar.RevocationKindSidecar,
		SubjectId: subjectId,
		NotBefore: SidecarRevocationNotBefore(at, DefaultSidecarRevSettings()),
		Reason:    sidecar.RevocationReasonSuspended,
	}})
}
