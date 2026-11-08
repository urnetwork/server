package controller

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

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
	return changeNetworkName(args, session, false)
}

func ClaimNetworkName(
	args ClaimNetworkNameArgs,
	session *session.ClientSession,
) (*ClaimNetworkNameResult, error) {
	return changeNetworkName(args, session, true)
}

// Test-only scheduling hook between changeNetworkName's availability check and
// its write, in the write's transaction. Production leaves it nil; controller
// tests use it to commit the name for another network in between, without
// sleeps or scheduler luck.
var changeNetworkNameBeforeWrite func()

// Sets the name of the session's network through model.RenameNetwork, which
// applies §7.12's rules to every rename path: a claim spends the claim budget
// and a change the change budget, and both release the old name to a 24-hour
// tombstone. A name the network already has is no change and succeeds.
func changeNetworkName(
	args ChangeNetworkNameArgs,
	session *session.ClientSession,
	claim bool,
) (*ChangeNetworkNameResult, error) {
	// Seedphrase users must have email/phone, SSO, or a wallet bound to
	// claim/change name — a signed wallet challenge is itself proof the
	// user controls that identity, same as a verified email or SSO login.
	if err := requireVerifiedIdentityBound(session.Ctx, session.ByJwt.UserId); err != nil {
		return &ChangeNetworkNameResult{
			Error: &ChangeNetworkNameError{
				Message: err.Error(),
			},
		}, nil
	}

	// Accept either network_name or new_name field
	name := args.NetworkName
	if name == "" {
		name = args.NewName
	}
	result := model.RenameNetwork(session, &model.RenameNetworkArgs{
		NewName:     name,
		Claim:       claim,
		BeforeWrite: changeNetworkNameBeforeWrite,
	})
	if result.Error != nil {
		return &ChangeNetworkNameResult{
			Error: &ChangeNetworkNameError{
				Message: result.Error.Message,
			},
		}, nil
	}
	return &ChangeNetworkNameResult{
		NetworkName: result.NetworkName,
	}, nil
}

// requireVerifiedIdentityBound checks that the user has at least one verified
// email/phone password auth, SSO auth, or wallet bound. Seedphrase-only users
// can't claim/change names — they need a verifiable identity method.
func requireVerifiedIdentityBound(ctx context.Context, userId server.Id) error {
	var hasBoundAuth bool

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1 FROM network_user_auth_password
					WHERE user_id = $1 AND verified = true
					UNION ALL
					SELECT 1 FROM network_user_auth_sso
					WHERE user_id = $1
					UNION ALL
					SELECT 1 FROM network_user_auth_wallet
					WHERE user_id = $1
				)
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&hasBoundAuth))
			}
		})
	})

	if !hasBoundAuth {
		return fmt.Errorf("You must verify an email, bind a social login, or connect a wallet before changing your network name.")
	}

	return nil
}
