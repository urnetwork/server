// Client-key bulk publication owns finite local quota windows only after all
// real authority, signature and response-bound work has already completed.
package controller

import (
	"context"
	"errors"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

const stClientKeyPublicationBatchEnvelopes = 64

// Original retained wrappers are replayed without resigning. Every object
// still performs both immutable creates and both exact winner readbacks;
// no root lock spans the controller's preceding Rpc or Sql operations.
func publishStClientKeyObservationBatch(ctx context.Context, owner *stClientKeyAuthorityOwner, histories [][]model.StClientKeyHistoryRecord, result *protocol.ClientKeyObservationBatchResponse, maximumResponseBytes uint64) (resultErr error) {
	if ctx == nil || owner == nil || owner.store == nil || result == nil || len(result.Responses) == 0 || len(result.Responses) > protocol.MaxClientKeyObservationBatchClients || len(histories) != len(result.Responses) {
		return errors.New("client-key publication batch ownership is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var batch *server.LocalBlobWriteBatch
	defer func() { resultErr = errors.Join(resultErr, batch.Close(), ctx.Err()) }()
	published := 0
	publicationCtx := ctx
	publish := func(encoded []byte, contentHash string) error {
		if published%stClientKeyPublicationBatchEnvelopes == 0 {
			if err := batch.Close(); err != nil {
				return err
			}
			batch = nil
			publicationCtx = ctx
			var err error
			batch, err = server.BeginLocalBlobWriteBatch(ctx, []server.BlobStore{owner.store}, 2*stClientKeyPublicationBatchEnvelopes)
			if err != nil {
				return err
			}
			if batch != nil {
				publicationCtx = batch.Context()
			}
		}
		if _, err := startifact.PublishClientKeyEvidence(publicationCtx, owner.store, encoded, contentHash); err != nil {
			return err
		}
		published++
		return nil
	}
	for index, body := range result.Responses {
		for _, record := range histories[index] {
			if err := publish(record.EvidenceBytes, record.EvidenceHash); err != nil {
				return err
			}
		}
		response, err := protocol.DecodeClientKeyHistoryResponse(body, maximumResponseBytes)
		if err != nil {
			return err
		}
		envelope, err := protocol.DecodeClientKeyEvidence(response.Observation, owner.domain, protocol.ClientKeyObservationEvidenceKind)
		if err != nil {
			return err
		}
		if err := publish(response.Observation, envelope.ContentHash); err != nil {
			return err
		}
	}
	return nil
}
