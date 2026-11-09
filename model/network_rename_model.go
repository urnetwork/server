package model

// §7.12's one rename (D-TEAM-13, R-TEAM-13): POST /account/change-name, POST
// /account/claim-name and POST /network/user/update all rename through
// RenameNetwork, so every path applies the same rules:
//
//   - ValidateNetworkName, then §12.5's label rules (sidecar.ValidateDnsName);
//   - only the network's owner renames it (its admin_user_id; roles replace
//     this at TEAM-M6-03);
//   - no other network holds the name or one a single edit from it (the
//     network name search), no reclaim tombstone holds it, and dns_label holds
//     it as no label or alias, live, retiring or tombstoned;
//   - two renames a day, a claim and a change each on its own budget, spent only
//     once the name is available, so a taken name costs no attempt;
//   - the released name gets a network_name_reclaim tombstone held 24 hours
//     (Q-TEAM-9), and the rename an admin event (§7.11), both in the rename's
//     transaction.
//
// Before this only a change wrote a tombstone, so a claim, an update and a
// network deletion released their names at once: the takeover paths I-14
// closes. Network create and every rename read the tombstones, and deletion
// writes one (RemoveNetworkWithStoreSnapshot). Policy names networks by id, so a
// reclaimed name captures no grant.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/sidecar"
)

// Tunables of the rename.
type NetworkRenameSettings struct {
	// How long a released name stays held for no one (Q-TEAM-9: 24 hours). §2.8
	// api-2 publishes this hold.
	ReclaimCooldown time.Duration
	// The renames a user's change budget allows in a window.
	ChangeLimit int
	// The renames a user's claim budget allows in a window. A claim, an
	// account's first name, has its own budget so onboarding spends none of
	// the change budget.
	ClaimLimit int
	// The window both budgets count over.
	LimitWindow time.Duration
	// The edit distance within which another network's name refuses a name.
	NearDistance int
}

func DefaultNetworkRenameSettings() *NetworkRenameSettings {
	return &NetworkRenameSettings{
		ReclaimCooldown: 24 * time.Hour,
		ChangeLimit:     2,
		ClaimLimit:      2,
		LimitWindow:     24 * time.Hour,
		NearDistance:    1,
	}
}

// Whether dns_label holds a name, as a label or an alias, live, retiring or
// tombstoned (§12.5). Labels and aliases share dns_label's primary key, so one
// lookup covers both. It never errs: a lookup that cannot answer raises.
type NetworkLabelHolder interface {
	HoldsNameInTx(ctx context.Context, query server.PgCanQuery, name string) bool
}

// dns_label itself (DNS-M5-10's DnsNameHeldInTx). RESOLUTIONS
// plan-DNS-labels-before-m5 moved the table from MG5 to MG2: a binary that does
// not link MG2 has no label or alias, so it holds no name.
func defaultNetworkLabelHolder() NetworkLabelHolder {
	return dnsLabelHolder{}
}

// §12.5's label rules a renamed network's name meets beyond ValidateNetworkName,
// sidecar's ValidateDnsName (DNS-M0-01): no edge hyphen, which Boulder refuses;
// no "--" at the third and fourth characters, which keeps out xn-- and IDNA
// homographs; not reserved; and no token confusable with a brand. Each refusal
// reads as a sentence about the network name.
func screenRenamedNetworkName(name string) error {
	if err := sidecar.ValidateDnsName(name); err != nil {
		return errors.New("Network " + err.Error())
	}
	return nil
}

// Why a rename was refused.
const (
	// the name fails ValidateNetworkName or §12.5's label rules
	RenameNetworkRefusalInvalid = "invalid"
	// the caller does not own the network
	RenameNetworkRefusalNotOwner = "not_owner"
	// another network, a tombstone or dns_label holds the name, or another
	// network's name is within the near distance
	RenameNetworkRefusalNotAvailable = "not_available"
	// the day's budget is spent
	RenameNetworkRefusalRateLimited = "rate_limited"
)

const renameNetworkNotAvailableMessage = "Network name not available."

const renameNetworkNotOwnerMessage = "Only the network's owner may rename it."

type RenameNetworkArgs struct {
	NewName string
	// the account's first name set, which spends the claim budget; every other
	// rename spends the change budget
	Claim bool
	// a test hook in the write's transaction, between its check and its
	// write; production leaves it nil
	BeforeWrite func()
}

type RenameNetworkError struct {
	// one of RenameNetworkRefusal*
	Code    string
	Message string
}

type RenameNetworkResult struct {
	// the name the network has now, as validated
	NetworkName string
	// the network already had the name, so nothing was written or spent
	Unchanged bool
	Error     *RenameNetworkError
}

// Renames the session's network (see the file header). A failed statement
// raises; every refusal is a result.
func RenameNetwork(clientSession *session.ClientSession, args *RenameNetworkArgs) *RenameNetworkResult {
	return newNetworkRenamer().rename(clientSession, args)
}

// What a rename depends on, apart from the database, so tests can replace it.
type networkRenamer struct {
	settings    *NetworkRenameSettings
	now         func() time.Time
	labelHolder NetworkLabelHolder
	screen      func(name string) error
}

func newNetworkRenamer() *networkRenamer {
	return &networkRenamer{
		settings:    DefaultNetworkRenameSettings(),
		now:         server.NowUtc,
		labelHolder: defaultNetworkLabelHolder(),
		screen:      screenRenamedNetworkName,
	}
}

func (self *networkRenamer) rename(clientSession *session.ClientSession, args *RenameNetworkArgs) *RenameNetworkResult {
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	userId := clientSession.ByJwt.UserId
	refuse := func(code string, message string) *RenameNetworkResult {
		return &RenameNetworkResult{
			Error: &RenameNetworkError{
				Code:    code,
				Message: message,
			},
		}
	}

	name, err := ValidateNetworkName(args.NewName)
	if err != nil {
		return refuse(RenameNetworkRefusalInvalid, err.Error())
	}
	if err := self.screen(name); err != nil {
		return refuse(RenameNetworkRefusalInvalid, err.Error())
	}

	// the owner, until TEAM-M6-03's Admin permission
	var adminUserId *server.Id
	var currentName string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT admin_user_id, network_name FROM network WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&adminUserId, &currentName))
			}
		})
	})
	if adminUserId == nil || *adminUserId != userId {
		return refuse(RenameNetworkRefusalNotOwner, renameNetworkNotOwnerMessage)
	}
	if currentName == name {
		return &RenameNetworkResult{
			NetworkName: name,
			Unchanged:   true,
		}
	}

	// availability before the budget, so a taken name costs no attempt
	for nearNetworkId := range networkNameSearch().AroundIds(ctx, name, self.settings.NearDistance) {
		if nearNetworkId != networkId {
			return refuse(RenameNetworkRefusalNotAvailable, renameNetworkNotAvailableMessage)
		}
	}
	available := false
	server.Db(ctx, func(conn server.PgConn) {
		available = self.availableInTx(ctx, conn, networkId, name)
	})
	if !available {
		return refuse(RenameNetworkRefusalNotAvailable, renameNetworkNotAvailableMessage)
	}

	action := AccountActionChangeNetworkName
	limit := self.settings.ChangeLimit
	if args.Claim {
		action = AccountActionClaimNetworkName
		limit = self.settings.ClaimLimit
	}
	if err := checkAndRecordAccountActionRateLimitAt(ctx, userId, action, limit, self.settings.LimitWindow, self.now()); err != nil {
		return refuse(RenameNetworkRefusalRateLimited, err.Error())
	}

	var posts []server.PostFunction
	taken := NetworkNameTx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		posts = nil
		available = self.availableInTx(ctx, tx, networkId, name)
		if !available {
			return
		}
		var oldName string
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT network_name FROM network WHERE network_id = $1 FOR UPDATE`,
			networkId,
		).Scan(&oldName))

		if args.BeforeWrite != nil {
			args.BeforeWrite()
		}

		now := self.now()
		if oldName != name {
			releaseNetworkNameInTx(ctx, tx, oldName, now.Add(self.settings.ReclaimCooldown))
		}
		tag := RaiseNetworkNameWrite(tx.Exec(
			ctx,
			`UPDATE network SET network_name = $2 WHERE network_id = $1`,
			networkId,
			name,
		))
		if tag.RowsAffected() == 1 {
			posts = append(posts, IndexNetworkNameInTx(ctx, tx, networkId, name))
			addAdminEventInTxAt(
				ctx,
				tx,
				NewSessionAdminEvent(clientSession, AdminEventActionNetworkRename, AdminEventTargetNetwork, &networkId),
				now,
			)
		}
	})
	if !available || taken {
		return refuse(RenameNetworkRefusalNotAvailable, renameNetworkNotAvailableMessage)
	}
	// the rename committed
	server.RunPosts(ctx, posts...)

	return &RenameNetworkResult{
		NetworkName: name,
	}
}

// Whether the network may take the name, as of the query's snapshot: no other
// network holds it, no tombstone holds it now, and dns_label does not hold it.
// The write's transaction repeats the check that came before it.
func (self *networkRenamer) availableInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, name string) bool {
	available := true
	result, err := query.Query(
		ctx,
		`SELECT network_id FROM network WHERE network_name = $1`,
		name,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var holderNetworkId server.Id
			server.Raise(result.Scan(&holderNetworkId))
			if holderNetworkId != networkId {
				available = false
			}
		}
	})
	if !available {
		return false
	}
	if networkNameReclaimHeldInTx(ctx, query, name, self.now()) {
		return false
	}
	return !self.labelHolder.HoldsNameInTx(ctx, query, name)
}

// Whether a reclaim tombstone holds the name at now.
func networkNameReclaimHeldInTx(ctx context.Context, query server.PgCanQuery, name string, now time.Time) bool {
	held := false
	result, err := query.Query(
		ctx,
		`SELECT cool_down_until FROM network_name_reclaim WHERE old_name = $1`,
		name,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var coolDownUntil time.Time
			server.Raise(result.Scan(&coolDownUntil))
			held = now.Before(coolDownUntil)
		}
	})
	return held
}

// Holds a released name for no one until coolDownUntil, in the caller's
// transaction, never shortening a hold already there.
func releaseNetworkNameInTx(ctx context.Context, tx server.PgTx, name string, coolDownUntil time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_name_reclaim (old_name, cool_down_until)
			VALUES ($1, $2)
			ON CONFLICT (old_name) DO UPDATE
			SET cool_down_until = GREATEST(network_name_reclaim.cool_down_until, EXCLUDED.cool_down_until)
		`,
		name,
		coolDownUntil.UTC(),
	))
}

// Whether a network create may not take the name, as of the transaction's
// snapshot: a network holds it (networkNameHeldInTx), a reclaim tombstone holds
// it now, or dns_label holds it.
func networkNameTakenForCreateInTx(ctx context.Context, tx server.PgTx, name string) bool {
	if networkNameHeldInTx(ctx, tx, name) {
		return true
	}
	if networkNameReclaimHeldInTx(ctx, tx, name, server.NowUtc()) {
		return true
	}
	return defaultNetworkLabelHolder().HoldsNameInTx(ctx, tx, name)
}
