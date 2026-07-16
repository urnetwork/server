package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const networkNameReclaimCooldown = 24 * time.Hour

type ChangeNetworkNameArgs struct {
	NetworkName string `json:"network_name"`
	NewName     string `json:"new_name"`
}

type ChangeNetworkNameError struct {
	Message string `json:"message"`
}

type ChangeNetworkNameResult struct {
	NetworkName string                  `json:"network_name"`
	Error       *ChangeNetworkNameError `json:"error,omitempty"`
}

type ClaimNetworkNameArgs = ChangeNetworkNameArgs
type ClaimNetworkNameError = ChangeNetworkNameError
type ClaimNetworkNameResult = ChangeNetworkNameResult

func ChangeNetworkName(
	args ChangeNetworkNameArgs,
	session *session.ClientSession,
) (*ChangeNetworkNameResult, error) {
	return changeNetworkName(args, session, true)
}

func ClaimNetworkName(
	args ClaimNetworkNameArgs,
	session *session.ClientSession,
) (*ClaimNetworkNameResult, error) {
	return changeNetworkName(args, session, false)
}

func changeNetworkName(
	args ChangeNetworkNameArgs,
	session *session.ClientSession,
	reclaimCooldown bool,
) (*ChangeNetworkNameResult, error) {
	// Accept either network_name or new_name field
	name := args.NetworkName
	if name == "" {
		name = args.NewName
	}
	normalizedName, err := model.ValidateNetworkName(name)
	if err != nil {
		return &ChangeNetworkNameResult{
			Error: &ChangeNetworkNameError{
				Message: err.Error(),
			},
		}, nil
	}

	available, err := isNetworkNameAvailableForUser(session.Ctx, normalizedName, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}
	if !available {
		return &ChangeNetworkNameResult{
			Error: &ChangeNetworkNameError{
				Message: "Network name not available.",
			},
		}, nil
	}

	var oldName *string
	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
			SELECT network_name FROM network
			WHERE admin_user_id = $1
			`,
			session.ByJwt.UserId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				oldName = &name
			}
		})
	})

	server.Tx(session.Ctx, func(tx server.PgTx) {
		if reclaimCooldown && oldName != nil {
			coolDownUntil := server.NowUtc().Add(networkNameReclaimCooldown)
			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_name_reclaim (old_name, cool_down_until)
					VALUES ($1, $2)
					ON CONFLICT (old_name) DO UPDATE
					SET cool_down_until = EXCLUDED.cool_down_until
				`,
				*oldName,
				coolDownUntil,
			))
		}

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network
				SET network_name = $2
				WHERE admin_user_id = $1
			`,
			session.ByJwt.UserId,
			normalizedName,
		))
	})

	return &ChangeNetworkNameResult{
		NetworkName: normalizedName,
	}, nil
}

func isNetworkNameAvailableForUser(
	ctx context.Context,
	name string,
	userId server.Id,
) (bool, error) {
	available := true
	reasonErr := error(nil)

	server.Db(ctx, func(conn server.PgConn) {
		// check if the name is taken by another network
		result, err := conn.Query(
			ctx,
			`
				SELECT admin_user_id FROM network
				WHERE network_name = $1
			`,
			name,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var owner server.Id
				server.Raise(result.Scan(&owner))
				if owner != userId {
					available = false
				}
			}
		})

		if !available {
			return
		}

		// check if the name is in cooldown (network_name_reclaim)
		result, err = conn.Query(
			ctx,
			`
				SELECT cool_down_until FROM network_name_reclaim
				WHERE old_name = $1
			`,
			name,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var coolDownUntil time.Time
				server.Raise(result.Scan(&coolDownUntil))
				if server.NowUtc().Before(coolDownUntil) {
					available = false
				}
			}
		})
	})

	if reasonErr != nil {
		return false, fmt.Errorf("failed to check network name availability: %w", reasonErr)
	}

	return available, nil
}
