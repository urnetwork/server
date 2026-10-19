// Historical custody is an index into previously published original windows.
// A matching SQL row is only a locator; the caller must verify all signed bytes.
package model

import (
	"context"
	"errors"
	"strconv"

	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urnetwork/server/v2026"
)

// Query every supplied candidate, independently of any proposed exclusions.
// Bounded overflow refuses the whole read rather than silently losing history.
func ListProviderWorkPriorEpochs(ctx context.Context, domain [32]byte, deployment StDeploymentKey, noId, before uint64, candidates []server.Id, maximum int) (epochs []uint64, resultErr error) {
	if ctx == nil {
		return nil, ErrProviderWorkInvalid
	}
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err(), context.Cause(ctx))
		if resultErr != nil {
			epochs = nil
		}
	}()
	defer providerWorkRecover(&resultErr)
	if domain == ([32]byte{}) || maximum < 1 || maximum > 64 || len(candidates) > payoutartifact.MaxClosedWorkRecords {
		return nil, ErrProviderWorkInvalid
	}
	if len(candidates) == 0 {
		return []uint64{}, nil
	}
	ids := make([]string, len(candidates))
	for index, id := range candidates {
		if id == (server.Id{}) {
			return nil, ErrProviderWorkInvalid
		}
		ids[index] = id.String()
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT DISTINCT a.epoch::text
			FROM provider_work_authority a
			JOIN provider_work_window w ON w.authority_hash=a.authority_hash
			JOIN st_payout_artifact p ON p.deployment_key=$2 AND p.no_id=$3
			 AND p.epoch=a.epoch AND p.content_hash='sha256:' || encode(w.artifact_hash,'hex')
			WHERE a.domain_hash=$1 AND a.epoch<$4
			 AND EXISTS (
			  SELECT 1 FROM jsonb_array_elements(convert_from(w.original,'UTF8')::jsonb->'window'->'records') r
			  WHERE r->>'contract_id'=ANY($5::text[])
			 )
			ORDER BY a.epoch::text LIMIT $6
		`, domain[:], requireStDeploymentKey(deployment), strconv.FormatUint(noId, 10), strconv.FormatUint(before, 10), ids, maximum+1)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var text string
				server.Raise(rows.Scan(&text))
				epoch, err := strconv.ParseUint(text, 10, 64)
				server.Raise(err)
				epochs = append(epochs, epoch)
			}
		})
	})
	if len(epochs) > maximum {
		return nil, payoutartifact.ErrClosedWorkCapacity
	}
	return epochs, nil
}
