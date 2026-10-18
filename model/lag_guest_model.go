package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Guest networks of lag.dev (lag.dev/PLAN.md A1, A2).
//
// A lag guest is a network shaped like the retired guest signup (auth_type
// 'guest', no sign-in method, no referral code, no name in the search index)
// and marked by a `lag_guest` row that holds its expiry. Only marked networks
// expire: the apps' guest networks have no row and are never touched.
//
// A lag guest is not a signup. Its creation and removal write no network
// audit events, so the network statistics, which replay created and deleted
// events, never count a page view of lag.dev as an account. Nor does it get
// the initial transfer balance a signup gets.

// The user name of a lag guest's admin user.
const lagGuestUserName = "lag"

// One lag guest network.
type LagGuest struct {
	NetworkId   server.Id
	UserId      server.Id
	NetworkName string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// The name of a lag guest network. Network names are unique, and a network id
// is, so the name never collides and needs no search.
func lagGuestNetworkName(networkId server.Id) string {
	return "lag-" + networkId.String()
}

// Creates a guest network marked as lag, with its admin user, expiring at
// expiresAt. The caller creates its one client (`AuthNetworkClient`).
func CreateLagGuestNetwork(ctx context.Context, createdAt time.Time, expiresAt time.Time) *LagGuest {
	guest := &LagGuest{
		NetworkId: server.NewId(),
		UserId:    server.NewId(),
		CreatedAt: createdAt.UTC(),
		ExpiresAt: expiresAt.UTC(),
	}
	guest.NetworkName = lagGuestNetworkName(guest.NetworkId)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_user (user_id, user_name, auth_type)
			VALUES ($1, $2, $3)
			`,
			guest.UserId,
			lagGuestUserName,
			AuthTypeGuest,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network (network_id, network_name, admin_user_id)
			VALUES ($1, $2, $3)
			`,
			guest.NetworkId,
			guest.NetworkName,
			guest.UserId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_guest (network_id, user_id, created_at, expires_at)
			VALUES ($1, $2, $3, $4)
			`,
			guest.NetworkId,
			guest.UserId,
			guest.CreatedAt,
			guest.ExpiresAt,
		))
	})
	return guest
}

// The lag guest mark of a network, or nil when the network is no lag guest.
func GetLagGuest(ctx context.Context, networkId server.Id) *LagGuest {
	var guest *LagGuest
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				lag_guest.user_id,
				lag_guest.created_at,
				lag_guest.expires_at,
				COALESCE(network.network_name, '')
			FROM lag_guest
			LEFT JOIN network ON network.network_id = lag_guest.network_id
			WHERE lag_guest.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				guest = &LagGuest{NetworkId: networkId}
				server.Raise(result.Scan(
					&guest.UserId,
					&guest.CreatedAt,
					&guest.ExpiresAt,
					&guest.NetworkName,
				))
			}
		})
	})
	return guest
}

// What one sweep did: the guests it removed, and the guests it kept because
// someone paid for them, which stop being lag guests.
type RemoveExpiredLagGuestsResult struct {
	Removed int `json:"removed"`
	Kept    int `json:"kept"`
}

// Removes up to limit lag guests whose expiry is at or before now, oldest
// first, each in its own transaction (A2). A removed guest's clients are
// deactivated and its network and admin user deleted, as an account deletion
// does, so every token it holds fails ValidateByJwtState. Each guest is
// removed whole or not at all, and a guest that fails stays marked for the
// next sweep.
func RemoveExpiredLagGuests(ctx context.Context, now time.Time, limit int) RemoveExpiredLagGuestsResult {
	result := RemoveExpiredLagGuestsResult{}
	networkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
			SELECT network_id
			FROM lag_guest
			WHERE expires_at <= $1
			ORDER BY expires_at, network_id
			LIMIT $2
			`,
			now.UTC(),
			limit,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var networkId server.Id
				server.Raise(rows.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	for _, networkId := range networkIds {
		server.HandleError(func() {
			removed, kept := removeExpiredLagGuest(ctx, networkId, now)
			if removed {
				result.Removed += 1
			}
			if kept {
				result.Kept += 1
			}
		})
	}
	return result
}

// Removes one expired lag guest under the network's session lifecycle lock,
// which orders the removal against every mint for the network.
func removeExpiredLagGuest(ctx context.Context, networkId server.Id, now time.Time) (removed bool, kept bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		removed = false
		kept = false

		server.Raise(session.LockSessionLifecycle(ctx, tx, networkId, true))

		var expiresAt time.Time
		found := false
		rows, err := tx.Query(
			ctx,
			`
			SELECT expires_at
			FROM lag_guest
			WHERE network_id = $1
			FOR UPDATE
			`,
			networkId,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&expiresAt))
				found = true
			}
		})
		if !found || now.Before(expiresAt) {
			// another sweep got there first
			return
		}

		// The network's admin user, under the row lock account deletion and
		// the payment paths take. The guest may already be gone, e.g. deleted
		// from its own client token; only the mark is left then.
		var adminUserId server.Id
		networkFound := false
		rows, err = tx.Query(
			ctx,
			`
			SELECT admin_user_id
			FROM network
			WHERE network_id = $1
			FOR UPDATE
			`,
			networkId,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&adminUserId))
				networkFound = true
			}
		})

		if networkFound {
			// A lag client token reaches the purchase routes like any client
			// token. A guest someone paid for is kept, unmarked, rather than
			// deleting what was paid for: account deletion refuses an active
			// renewal for the same reason.
			paid := false
			rows, err = tx.Query(
				ctx,
				`
				SELECT
					EXISTS (
						SELECT 1
						FROM subscription_renewal
						WHERE network_id = $1 AND $2 < end_time
					) OR EXISTS (
						SELECT 1
						FROM transfer_balance
						WHERE
							network_id = $1 AND
							active AND
							$2 < end_time AND
							0 < net_revenue_nano_cents
					)
				`,
				networkId,
				now.UTC(),
			)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&paid))
				}
			})
			if paid {
				server.RaisePgResult(tx.Exec(
					ctx,
					`DELETE FROM lag_guest WHERE network_id = $1`,
					networkId,
				))
				kept = true
				return
			}

			clientIds := []server.Id{}
			rows, err = tx.Query(
				ctx,
				`
				SELECT client_id
				FROM network_client
				WHERE network_id = $1 AND active
				`,
				networkId,
			)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					server.Raise(rows.Scan(&clientId))
					clientIds = append(clientIds, clientId)
				}
			})
			if 0 < len(clientIds) {
				_, err := deactivateNetworkClientsInTx(ctx, tx, clientIds, networkId)
				server.Raise(err)
			}

			// as RemoveNetworkWithStoreSnapshot deletes an account. A lag guest
			// has no sign-in method, so there are no auth rows to delete.
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_user WHERE user_id = $1`,
				adminUserId,
			))
			server.Raise(session.JournalSessionRetirementInTx(ctx, tx, networkId, server.NewId(), "delete", now.UTC()))
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network WHERE network_id = $1`,
				networkId,
			))
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM lag_guest WHERE network_id = $1`,
			networkId,
		))
		removed = true
	}, server.TxReadCommitted)

	if kept {
		glog.Infof("[lag]kept the paid lag guest network %s; it no longer expires\n", networkId)
	}
	return
}

// Removes a lag guest at once, whatever its expiry: the guest session's own
// rollback when its client could not be created.
func RemoveLagGuest(ctx context.Context, networkId server.Id) bool {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE lag_guest SET expires_at = LEAST(expires_at, $2) WHERE network_id = $1`,
			networkId,
			server.NowUtc(),
		))
	})
	removed, _ := removeExpiredLagGuest(ctx, networkId, server.NowUtc())
	return removed
}

// The lag guests among networkIds, inside the caller's transaction. A lag
// guest gets no automatic grant of any kind (lag.dev/PLAN.md A1).
func lagGuestNetworkIdsInTx(ctx context.Context, tx server.PgTx, networkIds []server.Id) map[server.Id]bool {
	lagGuests := map[server.Id]bool{}
	if len(networkIds) == 0 {
		return lagGuests
	}
	rows, err := tx.Query(
		ctx,
		`SELECT network_id FROM lag_guest WHERE network_id = ANY($1)`,
		networkIds,
	)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var networkId server.Id
			server.Raise(rows.Scan(&networkId))
			lagGuests[networkId] = true
		}
	})
	return lagGuests
}

// Every network of a referral map, referrers and the networks they referred.
func referralNetworkIdsOf(referrals map[server.Id][]server.Id) []server.Id {
	networkIds := []server.Id{}
	for referralNetworkId, referredNetworkIds := range referrals {
		networkIds = append(networkIds, referralNetworkId)
		networkIds = append(networkIds, referredNetworkIds...)
	}
	return networkIds
}
