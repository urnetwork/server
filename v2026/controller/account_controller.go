package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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

// Test-only scheduling hook between changeNetworkName's availability check and
// its write, in the write's transaction. Production leaves it nil; controller
// tests use it to commit the name for another network in between, without
// sleeps or scheduler luck.
var changeNetworkNameBeforeWrite func()

// Sets the name of the network the user administers. The name is checked
// before the rate limit, so a taken name costs no attempt, and again in the
// write's own transaction, so a name another network took or that entered its
// reclaim cooldown since is refused too. Another network committing the name
// after that transaction's snapshot meets the write on the unique index, which
// is refused the same way and rolls back the old name's cooldown. The write's
// transaction also replaces the network's entry in the network name search,
// and this process's in-memory index follows once it has committed.
func changeNetworkName(
	args ChangeNetworkNameArgs,
	session *session.ClientSession,
	reclaimCooldown bool,
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
	normalizedName, err := model.ValidateNetworkName(name)
	if err != nil {
		return &ChangeNetworkNameResult{
			Error: &ChangeNetworkNameError{
				Message: err.Error(),
			},
		}, nil
	}

	notAvailable := &ChangeNetworkNameResult{
		Error: &ChangeNetworkNameError{
			Message: "Network name not available.",
		},
	}

	available := false
	server.Db(session.Ctx, func(conn server.PgConn) {
		available = networkNameAvailableForUser(session.Ctx, conn, normalizedName, session.ByJwt.UserId)
	})
	if !available {
		return notAvailable, nil
	}

	// Claim (first-time name set, reachable once per account) and change
	// (renaming an existing name) get separate daily budgets: bundling them
	// would let a normal onboarding claim consume half of a new user's
	// same-day rename budget before they've even picked a real name.
	rateLimitAction := model.AccountActionClaimNetworkName
	rateLimitDailyLimit := model.AccountActionClaimNetworkNameDailyLimit
	if reclaimCooldown {
		rateLimitAction = model.AccountActionChangeNetworkName
		rateLimitDailyLimit = model.AccountActionChangeNetworkNameDailyLimit
	}
	if err := model.CheckAndRecordAccountActionRateLimit(
		session.Ctx,
		session.ByJwt.UserId,
		rateLimitAction,
		rateLimitDailyLimit,
		model.AccountActionDailyWindow,
	); err != nil {
		return &ChangeNetworkNameResult{
			Error: &ChangeNetworkNameError{
				Message: err.Error(),
			},
		}, nil
	}

	var posts []server.PostFunction
	taken := model.NetworkNameTx(session.Ctx, func(tx server.PgTx) {
		// a rerun starts over
		posts = nil
		available = networkNameAvailableForUser(session.Ctx, tx, normalizedName, session.ByJwt.UserId)
		if !available {
			return
		}

		var networkId *server.Id
		var oldName *string
		result, err := tx.Query(
			session.Ctx,
			`
			SELECT network_id, network_name FROM network
			WHERE admin_user_id = $1
			`,
			session.ByJwt.UserId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var name string
				server.Raise(result.Scan(&networkId, &name))
				oldName = &name
			}
		})

		if changeNetworkNameBeforeWrite != nil {
			changeNetworkNameBeforeWrite()
		}

		if reclaimCooldown && oldName != nil && *oldName != normalizedName {
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

		tag := model.RaiseNetworkNameWrite(tx.Exec(
			session.Ctx,
			`
				UPDATE network
				SET network_name = $2
				WHERE admin_user_id = $1
			`,
			session.ByJwt.UserId,
			normalizedName,
		))
		// the unique index lets the update set the name on one network only,
		// the one read above
		if networkId != nil && tag.RowsAffected() == 1 {
			posts = append(posts, model.IndexNetworkNameInTx(session.Ctx, tx, *networkId, normalizedName))
		}
	})
	if !available || taken {
		return notAvailable, nil
	}
	// the rename committed
	server.RunPosts(session.Ctx, posts...)

	return &ChangeNetworkNameResult{
		NetworkName: normalizedName,
	}, nil
}

// Whether the user may take the name: no other user's network holds it, and it
// is not in its reclaim cooldown. Reads through the query's snapshot, so the
// write's transaction repeats the check that came before it.
func networkNameAvailableForUser(
	ctx context.Context,
	query server.PgCanQuery,
	name string,
	userId server.Id,
) bool {
	available := true

	// check if the name is taken by another network
	result, err := query.Query(
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
		return false
	}

	// check if the name is in cooldown (network_name_reclaim)
	result, err = query.Query(
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

	return available
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
