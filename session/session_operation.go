// Durable operation identities recover Redis-applied/SQL-uncommitted revocation.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Every association/code writer takes shared first; revocation/reset takes
// exclusive first. READ COMMITTED after the wait sees the preceding commit.
func LockSessionLifecycle(ctx context.Context, tx server.PgTx, networkId server.Id, exclusive bool) error {
	hash := sha256.Sum256(append([]byte("urnetwork:session-lifecycle:v1:"), networkId[:]...))
	lock := int64(binary.BigEndian.Uint64(hash[:8]))
	query := "SELECT pg_advisory_xact_lock_shared($1)"
	if exclusive {
		query = "SELECT pg_advisory_xact_lock($1)"
	}
	_, err := tx.Exec(ctx, query, lock)
	return err
}

func sessionTx(ctx context.Context, run func(server.PgTx) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if failure, ok := recovered.(error); ok {
				err = failure
			} else {
				panic(recovered)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) { server.Raise(run(tx)) }, server.TxReadCommitted)
	return nil
}

type RevokeSessionArgs struct {
	SessionId   server.Id `json:"session_id"`
	OperationId server.Id `json:"operation_id"`
}
type RevokeOtherSessionsArgs struct {
	OperationId server.Id `json:"operation_id"`
}

type storedSessionOperation struct {
	NetworkId   server.Id
	OperationId server.Id
	Action      string
	Fingerprint string
	Actor       ByJwt
	Epoch       time.Time
	Target      *server.Id
	Keep        *server.Id
	Status      string
	Result      *SessionOperationResult
	Targets     []server.Id
	RetainUntil time.Time
}

func loadSessionOperation(ctx context.Context, query server.PgCanQuery, networkId, operationId server.Id, lock bool) (*storedSessionOperation, error) {
	sql := `SELECT action,fingerprint,actor,credential_epoch,target_session_id,kept_session_id,status,result,target_session_ids,retain_until FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`
	if lock {
		sql += " FOR UPDATE"
	}
	rows, err := query.Query(ctx, sql, networkId, operationId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	op := &storedSessionOperation{NetworkId: networkId, OperationId: operationId}
	var actor, result, targets []byte
	if err = rows.Scan(&op.Action, &op.Fingerprint, &actor, &op.Epoch, &op.Target, &op.Keep, &op.Status, &result, &targets, &op.RetainUntil); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(actor, &op.Actor); err != nil {
		return nil, err
	}
	if len(result) > 0 {
		if err = json.Unmarshal(result, &op.Result); err != nil {
			return nil, err
		}
	}
	if err = json.Unmarshal(targets, &op.Targets); err != nil {
		return nil, err
	}
	return op, nil
}

// Preparing is committed before Redis is touched, and retries reserve no quota.
func prepareSessionOperation(clientSession *ClientSession, operationId server.Id, action string, target, keep *server.Id) error {
	if operationId == (server.Id{}) || clientSession == nil || clientSession.ByJwt == nil {
		return sessionError("invalid_request")
	}
	if clientSession.ByJwt.ClientId != nil {
		return sessionError("network_credential_required")
	}
	fingerprintInput, _ := json.Marshal(struct {
		Action       string
		Target, Keep *server.Id
	}{action, target, keep})
	hash := sha256.Sum256(fingerprintInput)
	fingerprint := hex.EncodeToString(hash[:])
	now := server.NowUtc()
	actor, _ := json.Marshal(clientSession.ByJwt)
	return sessionTx(clientSession.Ctx, func(tx server.PgTx) error {
		if err := LockSessionLifecycle(clientSession.Ctx, tx, clientSession.ByJwt.NetworkId, true); err != nil {
			return err
		}
		existing, err := loadSessionOperation(clientSession.Ctx, tx, clientSession.ByJwt.NetworkId, operationId, true)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Fingerprint != fingerprint {
				return sessionError("operation_conflict")
			}
			return nil
		}
		if err = ValidateByJwtStateInTx(clientSession.Ctx, tx, clientSession.ByJwt, false); err != nil {
			return err
		}
		var epoch time.Time
		if err = tx.QueryRow(clientSession.Ctx, `SELECT credential_change_time FROM network_user WHERE user_id=$1`, clientSession.ByJwt.UserId).Scan(&epoch); err != nil {
			return err
		}
		limit := 100
		if action == "others" {
			limit = 20
		}
		var count int
		if err = tx.QueryRow(clientSession.Ctx, `SELECT count(*) FROM network_session_operation WHERE network_id=$1 AND action=$2 AND quota_reserved AND create_time>$3`, clientSession.ByJwt.NetworkId, action, now.Add(-24*time.Hour)).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return sessionError("session_quota_exceeded")
		}
		_, err = tx.Exec(clientSession.Ctx, `INSERT INTO network_session_operation(network_id,operation_id,action,fingerprint,actor,credential_epoch,target_session_id,kept_session_id,status,create_time,update_time,retain_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'prepared',$9,$9,$10)`, clientSession.ByJwt.NetworkId, operationId, action, fingerprint, actor, epoch, target, keep, now, now.Add(24*time.Hour))
		return err
	})
}

// Receipt lookup precedes actor validity: self-revocation must recover after
// Redis succeeds and the first SQL result commit is interrupted.
func EnforceSessionOperation(ctx context.Context, networkId, operationId server.Id) (result *SessionOperationResult, returnErr error) {
	var outcomeErr error
	returnErr = sessionTx(ctx, func(tx server.PgTx) error {
		if err := LockSessionLifecycle(ctx, tx, networkId, true); err != nil {
			return err
		}
		op, err := loadSessionOperation(ctx, tx, networkId, operationId, true)
		if err != nil {
			return err
		}
		if op == nil {
			return sessionError("operation_not_found")
		}
		if op.Status != "prepared" {
			if err := verifySessionOperationAuthority(ctx, op); err != nil {
				return err
			}
			result = op.Result
			if result != nil {
				result.TargetSessionIds = op.Targets
			}
			if result == nil {
				result = &SessionOperationResult{OperationId: operationId, State: op.Status, Status: op.Status}
			}
			return nil
		}
		receipt, err := readSessionReceipt(ctx, networkId, operationId)
		if err != nil {
			return err
		}
		if receipt == nil {
			if op.Action != "reset" && op.Action != "delete" {
				var epoch time.Time
				err = tx.QueryRow(ctx, `SELECT u.credential_change_time FROM network_user u JOIN network n ON n.admin_user_id=u.user_id WHERE n.network_id=$1 AND u.user_id=$2`, networkId, op.Actor.UserId).Scan(&epoch)
				if errors.Is(err, pgx.ErrNoRows) || err == nil && !epoch.Equal(op.Epoch) {
					_, err = tx.Exec(ctx, `UPDATE network_session_operation SET status='cancelled',quota_reserved=false,update_time=$3 WHERE network_id=$1 AND operation_id=$2`, networkId, operationId, server.NowUtc())
					result = &SessionOperationResult{OperationId: operationId, State: "cancelled", Status: "cancelled"}
					return err
				}
				if err != nil {
					return err
				}
				if err = ValidateByJwtStateInTx(ctx, tx, &op.Actor, false); err != nil {
					if errors.Is(err, ErrAuthUnavailable) || errors.Is(err, ErrSessionStoreUnavailable) {
						return err
					}
					// An unapplied request no longer has a live actor. Retrying
					// forever cannot help, and must not keep a quota reservation.
					_, err = tx.Exec(ctx, `UPDATE network_session_operation SET status='cancelled',quota_reserved=false,update_time=$3 WHERE network_id=$1 AND operation_id=$2`, networkId, operationId, server.NowUtc())
					result = &SessionOperationResult{OperationId: operationId, State: "cancelled", Status: "cancelled"}
					return err
				}
			}
			receipt, err = enforceSessionRevoke(ctx, networkId, operationId, op.Action, op.Target, op.Keep, op.Epoch)
			if err != nil {
				var refusal *SessionError
				if errors.As(err, &refusal) && refusal.Code == "session_not_found" {
					_, sqlErr := tx.Exec(ctx, `UPDATE network_session_operation SET status='failed',quota_reserved=false,update_time=$3 WHERE network_id=$1 AND operation_id=$2`, networkId, operationId, server.NowUtc())
					if sqlErr != nil {
						return sqlErr
					}
					outcomeErr = err
					return nil
				}
				return err
			}
		}
		receipt.SessionId = op.Target
		receipt.KeptSessionId = op.Keep
		receipt.State = "enforced"
		result = receipt
		if err = QueueSessionIndexInTx(ctx, tx, networkId, receipt.RetainUntil); err != nil {
			return err
		}
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		targets, err := json.Marshal(receipt.TargetSessionIds)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE network_session_operation SET status='enforced',result=$3,target_session_ids=$4,retain_until=GREATEST(retain_until,$5),update_time=$6,quota_reserved=CASE WHEN $7=0 THEN false ELSE quota_reserved END WHERE network_id=$1 AND operation_id=$2`, networkId, operationId, encoded, targets, receipt.RetainUntil, server.NowUtc(), receipt.RevokedCount)
		return err
	})
	if returnErr == nil {
		returnErr = outcomeErr
	}
	return
}

func RevokeNetworkSession(args *RevokeSessionArgs, clientSession *ClientSession) (*SessionOperationResult, error) {
	if args == nil || args.SessionId == (server.Id{}) {
		return nil, sessionError("invalid_request")
	}
	if err := prepareSessionOperation(clientSession, args.OperationId, "single", &args.SessionId, nil); err != nil {
		return nil, sessionOperationApiError(args.OperationId, err)
	}
	result, err := EnforceSessionOperation(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.OperationId)
	return result, sessionOperationApiError(args.OperationId, err)
}
func RevokeOtherNetworkSessions(args *RevokeOtherSessionsArgs, clientSession *ClientSession) (*SessionOperationResult, error) {
	if args == nil {
		return nil, sessionError("invalid_request")
	}
	if clientSession.ByJwt.SessionId == nil {
		if clientSession.ApiKeyAuthenticated || clientSession.UnsignedIdentity {
			return nil, sessionError("current_session_required")
		}
		return nil, sessionError("session_upgrade_required")
	}
	if err := prepareSessionOperation(clientSession, args.OperationId, "others", nil, clientSession.ByJwt.SessionId); err != nil {
		return nil, sessionOperationApiError(args.OperationId, err)
	}
	result, err := EnforceSessionOperation(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.OperationId)
	return result, sessionOperationApiError(args.OperationId, err)
}
func GetSessionOperation(clientSession *ClientSession, operationId server.Id) (result *SessionOperationResult, err error) {
	if clientSession.ByJwt.ClientId != nil {
		return nil, sessionError("network_credential_required")
	}
	err = sessionTx(clientSession.Ctx, func(tx server.PgTx) error {
		op, err := loadSessionOperation(clientSession.Ctx, tx, clientSession.ByJwt.NetworkId, operationId, false)
		if err != nil {
			return err
		}
		if op == nil {
			return sessionError("operation_not_found")
		}
		if op.Status == "prepared" {
			// A lost SQL commit can leave an already-applied cutoff labelled
			// prepared. Only report pending after checking the authority receipt.
			receipt, receiptErr := readSessionReceipt(clientSession.Ctx, clientSession.ByJwt.NetworkId, operationId)
			if receiptErr != nil {
				return receiptErr
			}
			if receipt != nil {
				receipt.SessionId = op.Target
				receipt.KeptSessionId = op.Keep
				receipt.State = "enforced"
				result = receipt
				return nil
			}
		}
		if err := verifySessionOperationAuthority(clientSession.Ctx, op); err != nil {
			return err
		}
		result = op.Result
		if result == nil {
			status := "pending"
			if op.Status == "cancelled" || op.Status == "failed" {
				status = op.Status
			}
			result = &SessionOperationResult{OperationId: operationId, State: op.Status, Status: status, CleanupPending: op.Status == "prepared"}
		} else {
			result.State = op.Status
		}
		return nil
	})
	return result, sessionOperationApiError(operationId, err)
}

type SessionCleanupOperation struct {
	NetworkId        server.Id
	OperationId      server.Id
	TargetSessionIds []server.Id
}

func PendingSessionOperations(ctx context.Context, limit int) (operations []SessionCleanupOperation, err error) {
	err = sessionTx(ctx, func(tx server.PgTx) error {
		rows, err := tx.Query(ctx, `SELECT network_id,operation_id,target_session_ids FROM network_session_operation WHERE status IN ('prepared','enforced') ORDER BY update_time,network_id,operation_id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var op SessionCleanupOperation
			var encoded []byte
			if err = rows.Scan(&op.NetworkId, &op.OperationId, &encoded); err != nil {
				return err
			}
			if err = json.Unmarshal(encoded, &op.TargetSessionIds); err != nil {
				return err
			}
			operations = append(operations, op)
		}
		return rows.Err()
	})
	return
}

// The business-model cleaner performs its guarded client/code batch, then marks
// complete in the same transaction only after no matching active rows remain.
func CompleteSessionOperationInTx(ctx context.Context, tx server.PgTx, networkId, operationId server.Id) error {
	_, err := tx.Exec(ctx, `UPDATE network_session_operation SET status='complete',result=jsonb_set(jsonb_set(result,'{cleanup_pending}','false'),'{state}','"complete"'),update_time=$3 WHERE network_id=$1 AND operation_id=$2 AND status='enforced'`, networkId, operationId, server.NowUtc())
	return err
}

// The SQL authority change and retirement intent commit together. No Redis
// outage can roll back a password change or restore a deleted account.
func JournalSessionRetirementInTx(ctx context.Context, tx server.PgTx, networkId, operationId server.Id, action string, cutoff time.Time) error {
	if action != "reset" && action != "delete" {
		return sessionError("invalid_request")
	}
	_, err := tx.Exec(ctx, `INSERT INTO network_session_operation(network_id,operation_id,action,fingerprint,actor,credential_epoch,status,quota_reserved,create_time,update_time,retain_until) VALUES($1,$2,$3,$3,'{}',$4,'prepared',false,$4,$4,$5) ON CONFLICT(network_id,operation_id) DO NOTHING`, networkId, operationId, action, cutoff, cutoff.Add(24*time.Hour))
	return err
}

// Bounded deletion only for terminal operations/receipts. Unfinished work is
// retained regardless of age and remains visible to recovery/backlog alerts.
func ExpireSessionReceipts(ctx context.Context, now time.Time, limit int) error {
	return sessionTx(ctx, func(tx server.PgTx) error {
		_, err := tx.Exec(ctx, `DELETE FROM network_session_operation WHERE (network_id,operation_id) IN (SELECT network_id,operation_id FROM network_session_operation WHERE status IN ('complete','cancelled','failed') AND retain_until<=$1 ORDER BY retain_until LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, limit)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM auth_code_redemption WHERE (code_sha256,request_id) IN (SELECT code_sha256,request_id FROM auth_code_redemption WHERE retain_until<=$1 ORDER BY retain_until LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, limit)
		return err
	})
}
