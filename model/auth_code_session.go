// Auth-code redemption commits one immutable session and one use per request ID.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A code that descends from an API key is refused once the key is removed (§7.4),
// and the login it mints carries the key's id, so removing the key ends the login
// too (session.ByJwt.ApiKeyId). DeleteApiKey deletes the key's codes in its own
// transaction; these reads refuse a code whose key went any other way, and they
// hold up against that delete in either order: a redemption that locks the code
// first commits a login the delete then ends, and one that comes second finds no
// code. A binary that does not link MG0 has no api_key_id to read.
func authCodeLoginSession(args *AuthCodeLoginArgs, clientSession *session.ClientSession) (response *AuthCodeLoginResult, returnErr error) {
	apiKeyColumn := "NULL::uuid"
	liveApiKeySql := ""
	if server.SidecarMg0Linked() {
		apiKeyColumn = "c.api_key_id"
		liveApiKeySql = ` AND (c.api_key_id IS NULL OR EXISTS (SELECT 1 FROM account_api_key k WHERE k.api_key_id=c.api_key_id AND k.network_id=c.network_id))`
	}
	return server.HandleError2(func() (*AuthCodeLoginResult, error) {
		ctx := clientSession.Ctx
		hash := sha256.Sum256([]byte(args.AuthCode))
		codeHash := hex.EncodeToString(hash[:])
		requestId := server.NewId()
		if args.RequestId != nil {
			requestId = *args.RequestId
			if requestId == (server.Id{}) {
				return nil, &session.SessionError{Code: "invalid_request", Status: 400}
			}
		}
		var networkId server.Id
		var encoded []byte
		var status string
		var saved []byte
		var fingerprint string
		server.Db(ctx, func(conn server.PgConn) {
			err := conn.QueryRow(ctx, `SELECT network_id,credential,status,result,fingerprint FROM auth_code_redemption WHERE request_id=$1`, requestId).Scan(&networkId, &encoded, &status, &saved, &fingerprint)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
		})
		if fingerprint != "" && fingerprint != codeHash {
			return nil, &session.SessionError{Code: "request_id_conflict", Status: 409}
		}
		var credential *session.ByJwt
		if len(encoded) > 0 {
			credential = &session.ByJwt{}
			server.Raise(json.Unmarshal(encoded, credential))
		}
		if status == "consumed" {
			if err := session.ValidateByJwtState(ctx, credential, false); err != nil {
				return nil, err
			}
			result := &AuthCodeLoginResult{}
			server.Raise(json.Unmarshal(saved, result))
			clientSession.WithByJwt(credential).ObserveAuthenticatedUse()
			return result, nil
		}
		if credential == nil {
			var userId server.Id
			var name, principal string
			var lineage, end time.Time
			var apiKeyId *server.Id
			found := false
			server.Db(ctx, func(conn server.PgConn) {
				err := conn.QueryRow(ctx, `SELECT c.network_id,c.user_id,c.create_time,c.end_time,c.principal,n.network_name,`+apiKeyColumn+` FROM auth_code c JOIN network n ON n.network_id=c.network_id JOIN network_user u ON u.user_id=c.user_id AND n.admin_user_id=u.user_id WHERE c.auth_code=$1 AND c.active AND c.remaining_uses>0 AND $2<c.end_time AND u.credential_change_time<=c.create_time`+liveApiKeySql, args.AuthCode, server.NowUtc()).Scan(&networkId, &userId, &lineage, &end, &principal, &name, &apiKeyId)
				if errors.Is(err, pgx.ErrNoRows) {
					return
				}
				server.Raise(err)
				found = true
			})
			if !found {
				return &AuthCodeLoginResult{Error: &AuthCodeLoginError{Message: "Invalid auth code."}}, nil
			}
			credential = session.NewByJwtWithCreateTime(networkId, userId, name, lineage, false, IsProFresh(ctx, &networkId))
			credential.Principal = principal
			credential.ApiKeyId = apiKeyId
			session.PrepareSessionMint(credential, true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(session.LockSessionLifecycle(ctx, tx, networkId, false))
				rows, err := tx.Query(ctx, `SELECT r.role FROM auth_code_role r JOIN auth_code c ON c.auth_code_id=r.auth_code_id WHERE c.auth_code=$1 ORDER BY r.role`, args.AuthCode)
				credential.Roles = nil
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var role string
						server.Raise(rows.Scan(&role))
						credential.Roles = append(credential.Roles, role)
					}
				})
				encoded, err = json.Marshal(credential)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO auth_code_redemption(code_sha256,request_id,network_id,fingerprint,credential,status,retain_until) VALUES($1,$2,$3,$1,$4,'prepared',$5) ON CONFLICT(request_id) DO NOTHING`, codeHash, requestId, networkId, encoded, end.Add(24*time.Hour)))
			}, server.TxReadCommitted)
		}
		var result *AuthCodeLoginResult
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.LockSessionLifecycle(ctx, tx, networkId, false))
			server.Raise(tx.QueryRow(ctx, `SELECT credential,status,result,fingerprint FROM auth_code_redemption WHERE request_id=$1 FOR UPDATE`, requestId).Scan(&encoded, &status, &saved, &fingerprint))
			// A competing payload may win preparation after the initial read.
			// Refuse it before interpreting another account's saved credential.
			if fingerprint != codeHash {
				server.Raise(&session.SessionError{Code: "request_id_conflict", Status: 409})
			}
			server.Raise(json.Unmarshal(encoded, credential))
			server.Raise(session.ValidateByJwtStateInTx(ctx, tx, credential, false))
			if status == "consumed" {
				result = &AuthCodeLoginResult{}
				server.Raise(json.Unmarshal(saved, result))
				return
			}
			var codeId server.Id
			var origin *server.Id
			var uses int
			err := tx.QueryRow(ctx, `SELECT c.auth_code_id,c.origin_session_id,c.remaining_uses FROM auth_code c WHERE c.auth_code=$1 AND c.network_id=$2 AND c.active AND $3<c.end_time AND c.remaining_uses>0`+liveApiKeySql+` FOR UPDATE`, args.AuthCode, networkId, server.NowUtc()).Scan(&codeId, &origin, &uses)
			if errors.Is(err, pgx.ErrNoRows) {
				result = &AuthCodeLoginResult{Error: &AuthCodeLoginError{Message: "Invalid auth code."}}
				return
			}
			server.Raise(err)
			signed, err := session.RegisterAndSignInTx(ctx, tx, credential, "auth_code", origin, true)
			server.Raise(err)
			if uses > 1 {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE auth_code SET remaining_uses=remaining_uses-1 WHERE auth_code_id=$1`, codeId))
			} else {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM auth_code_role WHERE auth_code_id=$1`, codeId))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM auth_code WHERE auth_code_id=$1`, codeId))
			}
			result = &AuthCodeLoginResult{ByJwt: signed}
			saved, err = json.Marshal(result)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE auth_code_redemption SET status='consumed',result=$3 WHERE code_sha256=$1 AND request_id=$2`, codeHash, requestId, saved))
		}, server.TxReadCommitted)
		if result != nil && result.Error == nil {
			clientSession.WithByJwt(credential).ObserveAuthenticatedUse()
		}
		return result, nil
	}, func(err error) (*AuthCodeLoginResult, error) { return nil, err })
}
