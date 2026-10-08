// Stream provenance is minted only at the live Redis cohort birth. Reused
// streams copy its durable reference; they never sign a reconstructed hop list.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

type providerWorkRequestFrameContextKey struct{}

// The authenticated control owner supplies the hash of the exact original
// request Frame, before CreateContract is normalized into a companion path.
func WithProviderWorkRequestFrameHash(ctx context.Context, hash [32]byte) context.Context {
	return context.WithValue(ctx, providerWorkRequestFrameContextKey{}, hash)
}

// Prepare original directory identities at live creation, before any Redis
// mutation. The signed cohort is retained only if this call actually births it.
func providerWorkPrepareStream(ctx context.Context, contractId, sourceId, destinationId server.Id, intermediaryIds []server.Id) *protocol.ProviderWorkStreamCohort {
	return providerWorkPrepareStreamWithDb(ctx, contractId, sourceId, destinationId, intermediaryIds, func(ctx context.Context, read func(server.PgConn)) { server.Db(ctx, read) })
}

// The owned database acquisition boundary is explicit so pool failures remain
// optional evidence loss, including failures raised before the read callback.
func providerWorkPrepareStreamWithDb(ctx context.Context, contractId, sourceId, destinationId server.Id, intermediaryIds []server.Id, db func(context.Context, func(server.PgConn))) *protocol.ProviderWorkStreamCohort {
	source := providerWorkSessionSourceFromContext(ctx)
	hash, _ := ctx.Value(providerWorkRequestFrameContextKey{}).([32]byte)
	if source == nil || hash == ([32]byte{}) || len(intermediaryIds) > int(source.authority.MaxCohortMembers) {
		return nil
	}
	body := &protocol.ProviderWorkStreamCohort{OriginContractId: contractId.String(), SourceId: sourceId.String(), DestinationId: destinationId.String(), RequestFrameHash: hash, Intermediaries: []protocol.ProviderWorkParticipant{}}
	if len(intermediaryIds) == 0 {
		return body
	}
	identities := map[server.Id]server.Id{}
	var resultErr error
	server.HandleError(func() {
		db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT client_id,network_id FROM network_client WHERE client_id=ANY($1)`, intermediaryIds)
			if err != nil {
				resultErr = err
				return
			}
			defer rows.Close()
			for rows.Next() {
				var clientId, networkId server.Id
				if err := rows.Scan(&clientId, &networkId); err != nil {
					resultErr = err
					return
				}
				identities[clientId] = networkId
			}
			resultErr = rows.Err()
		})
	}, func(err error) { resultErr = err })
	if resultErr != nil {
		return nil
	}
	for _, clientId := range intermediaryIds {
		networkId, found := identities[clientId]
		if !found {
			return nil
		}
		body.Intermediaries = append(body.Intermediaries, protocol.ProviderWorkParticipant{ClientId: clientId.String(), NetworkId: networkId.String()})
	}
	return body
}

// The Redis birth result is the sole admission to this issuer. If retention
// fails, the stream remains usable but can never acquire a later replacement.
func providerWorkRetainStreamBirth(ctx context.Context, streamId server.Id, body *protocol.ProviderWorkStreamCohort, at time.Time) {
	if body == nil {
		return
	}
	body.StreamId = streamId.String()
	body.CreatedAtUnixMicro = at.UnixMicro()
	source := providerWorkSessionSourceFromContext(ctx)
	_, raw, hash, err := source.sign(ctx, protocol.ProviderWorkReceipt{Stream: body}, body.CreatedAtUnixMicro)
	if err != nil {
		return
	}
	// Optional SQL ownership does not change the live stream's routing result.
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error {
				id, err := protocol.ParseProviderWorkId(body.OriginContractId)
				if err != nil {
					return err
				}
				_, err = optional.Exec(ctx, `INSERT INTO provider_work_stream_original(stream_id,origin_contract_id,receipt_hash,original) VALUES($1,$2,$3,$4) ON CONFLICT(stream_id) DO NOTHING`, streamId, server.Id(id), hash[:], raw)
				return err
			})
		})
	})
}

// Called by the real SQL stream attachment, including inherited pair and
// companion cohorts. The original birth must already exist; absence is unknown.
func providerWorkAttachStreamInTx(ctx context.Context, tx server.PgTx, contractId, streamId server.Id) {
	providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error {
		var raw []byte
		if err := optional.QueryRow(ctx, `SELECT original FROM provider_work_stream_original WHERE stream_id=$1`, streamId).Scan(&raw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderWorkEvidenceUnavailable
			}
			return err
		}
		original, err := protocol.DecodeProviderWorkReceipt(ctx, raw)
		if err != nil || original.Stream == nil {
			return errors.Join(errors.New("provider work original cohort is invalid"), err)
		}
		members := map[string]string{}
		for _, member := range original.Stream.Intermediaries {
			members[member.ClientId] = member.NetworkId
		}
		rows, err := optional.Query(ctx, `SELECT client_id,network_id FROM contract_participant WHERE stream_id=$1 ORDER BY client_id`, streamId)
		if err != nil {
			return err
		}
		matches := true
		for rows.Next() {
			var clientId, networkId server.Id
			if err := rows.Scan(&clientId, &networkId); err != nil {
				rows.Close()
				return err
			}
			if members[clientId.String()] != networkId.String() {
				matches = false
			}
			delete(members, clientId.String())
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !matches || len(members) != 0 {
			// Directory identity can change between Redis birth and SQL
			// attachment. That cut has no original proof of current parties.
			return errProviderWorkEvidenceUnavailable
		}
		_, err = optional.Exec(ctx, `INSERT INTO provider_work_stream_contract(contract_id,stream_id)
   SELECT $1,stream_id FROM provider_work_stream_original WHERE stream_id=$2
   ON CONFLICT(contract_id) DO NOTHING`, contractId, streamId)
		return err
	})
}

// The terminal owner signs the decision and both report counters in the same
// transaction that first makes the financial outcome visible.
func providerWorkRetainOutcomeInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome, closedAt time.Time) {
	providerWorkOptionalInTx(ctx, tx, func(optional server.PgTx) error {
		body := protocol.ProviderWorkOutcome{ContractId: contractId.String(), Outcome: outcome, ClosedAtUnixMicro: closedAt.UnixMicro()}
		var reservationHash, reservationRaw, streamHash []byte
		var streamId *server.Id
		var capacity int64
		if err := optional.QueryRow(ctx, `SELECT r.receipt_hash,r.original,c.stream_id,s.receipt_hash,c.transfer_byte_count
   FROM transfer_contract c JOIN provider_work_reservation_original r USING(contract_id)
   LEFT JOIN provider_work_stream_contract a USING(contract_id)
   LEFT JOIN provider_work_stream_original s ON s.stream_id=a.stream_id
   WHERE c.contract_id=$1`, contractId).Scan(&reservationHash, &reservationRaw, &streamId, &streamHash, &capacity); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProviderWorkEvidenceUnavailable
			}
			return err
		}
		reservation, err := protocol.DecodeProviderWorkReceipt(ctx, reservationRaw)
		if err != nil || reservation.Reservation == nil {
			return errors.Join(errors.New("provider work original reservation is invalid"), err)
		}
		if reservation.Reservation.RequestFrameHash == nil || reservation.Reservation.UsageOriginIsSource == nil || streamId != nil && len(streamHash) == 0 {
			return errProviderWorkEvidenceUnavailable
		}
		if len(reservationHash) != 32 || capacity < 0 || streamId != nil && len(streamHash) != 32 {
			return errors.New("provider work original reservation or stream is invalid")
		}
		copy(body.ReservationHash[:], reservationHash)
		copy(body.StreamHash[:], streamHash)
		body.Capacity = uint64(capacity)
		rows, err := optional.Query(ctx, `SELECT party,used_transfer_byte_count,checkpoint FROM contract_close WHERE contract_id=$1 ORDER BY party`, contractId)
		if err != nil {
			return err
		}
		for rows.Next() {
			var party ContractParty
			var count int64
			var checkpoint bool
			if err := rows.Scan(&party, &count, &checkpoint); err != nil {
				rows.Close()
				return err
			}
			if count < 0 {
				rows.Close()
				return errors.New("provider work close count is negative")
			}
			switch party {
			case ContractPartySource:
				body.SourceBytes = uint64(count)
				body.SourceComplete = !checkpoint
			case ContractPartyDestination:
				body.DestinationBytes = uint64(count)
				body.DestinationComplete = !checkpoint
			default:
				rows.Close()
				return errors.New("provider work close party is invalid")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, raw, hash, err := providerWorkSessionSourceFromContext(ctx).sign(ctx, protocol.ProviderWorkReceipt{Outcome: &body}, body.ClosedAtUnixMicro)
		if err != nil {
			return err
		}
		_, err = optional.Exec(ctx, `INSERT INTO provider_work_outcome_original(contract_id,receipt_hash,original) VALUES($1,$2,$3)`, contractId, hash[:], raw)
		return err
	})
}
