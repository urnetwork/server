// Every accepted binding consent survives replacement of the convenience head.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// Retain exact consent bytes in the same transaction as the mutable projection.
// Retry timestamps do not create a new consent; either signature change does.
func retainStFleetBindingOriginalInTx(ctx context.Context, tx server.PgTx, signature *StFleetBindingSignature) {
	original := *signature
	original.CreateTime = time.Time{}
	encoded, err := json.Marshal(&original)
	server.Raise(err)
	if len(encoded) > 16384 {
		panic(errors.New("binding original exceeds retention bound"))
	}
	hash := sha256.Sum256(encoded)
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_fleet_binding_original (deployment_key,client_id,generation,receipt_hash,original_body,observed_time)
 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (deployment_key,client_id,generation,receipt_hash) DO NOTHING`, requireStDeploymentKey(signature.DeploymentKey), signature.ClientId, int64(signature.Generation), hash[:], encoded, signature.CreateTime.UTC()))
}

// A bounded index exposes original consents without claiming on-chain inclusion.
func ListStFleetBindingOriginals(ctx context.Context, deploymentKey StDeploymentKey, clientId server.Id, limit int) (originals [][]byte) {
	if limit < 1 || limit > 10000 {
		panic(errors.New("invalid binding original limit"))
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original_body FROM st_fleet_binding_original WHERE deployment_key=$1 AND client_id=$2 ORDER BY generation,receipt_hash LIMIT $3`, requireStDeploymentKey(deploymentKey), clientId, limit)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var original []byte
				server.Raise(rows.Scan(&original))
				originals = append(originals, original)
			}
		})
	})
	return
}
