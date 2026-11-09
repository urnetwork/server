package model

// §7.8's admin_token_use (MG1; §2.8 api-11): each one-command admin token's jti,
// spent once, in its command's own transaction, beside the command's digest and
// the answer it gave. A route whose model runs an admin command:
//
//	claim   ClaimAdminTokenUseInTx before the effect: the first use inserts the
//	        row and runs the command; a replay, a retry after a lost response
//	        or a second request racing the first, waits on the first's row and
//	        answers its stored result, running nothing
//	store   StoreAdminTokenUseResultInTx after the effect, in the same
//	        transaction, so the row and the effect commit together or not at all
//
// A row goes two hours after its use (RemoveOldAdminTokenUses), which is at
// least an hour past its token's exp, since a token lives an hour and is used
// within it. The result a row holds is the command's answer as the route sent
// it: for v0's POST /network/auth-keys, the single-use key of at most 5 minutes,
// which a retry needs and which is spent or expired well before the row goes.
//
// Nothing here reads or writes admin_token_use unless MG1 is linked; until then
// an admin token's command answers unavailable and runs nothing.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of admin_token_use.
type AdminTokenUseSettings struct {
	// How long a row stays after its use: two hours, an admin token's hour and
	// §2.8 api-11's hour past its exp.
	Retention time.Duration
}

func DefaultAdminTokenUseSettings() *AdminTokenUseSettings {
	return &AdminTokenUseSettings{
		Retention: 2 * time.Hour,
	}
}

// A replay of a jti whose first use stored no result: its transaction committed
// without one, which a route that claims and stores in one transaction never
// does.
var ErrAdminTokenUseNoResult = errors.New("the admin token's first use stored no result")

const adminTokenUnavailableMessage = "Admin tokens are not available yet."

// Spends use's jti in tx, the command's own transaction. first is true for the
// first use, which runs the command and then stores its answer; false for a
// replay, which runs nothing and answers storedResult. A concurrent first use
// holds the row's lock, so a replay waits for its commit and reads its result,
// or claims the jti itself if it rolled back.
func ClaimAdminTokenUseInTx(ctx context.Context, tx server.PgTx, use *session.AdminCommandUse, now time.Time) (storedResult []byte, first bool, err error) {
	if !server.SidecarMg1Linked() {
		return nil, false, fmt.Errorf("%d %s", http.StatusServiceUnavailable, adminTokenUnavailableMessage)
	}
	tag, err := tx.Exec(
		ctx,
		`
			INSERT INTO admin_token_use (
				jti,
				member_id,
				cmd,
				use_time
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (jti) DO NOTHING
		`,
		use.Jti,
		use.MemberId,
		use.Cmd,
		now.UTC(),
	)
	if err != nil {
		return nil, false, err
	}
	if tag.RowsAffected() == 1 {
		return nil, true, nil
	}
	var memberId server.Id
	var cmd string
	var result []byte
	if err := tx.QueryRow(
		ctx,
		`
			SELECT member_id, cmd, result
			FROM admin_token_use
			WHERE jti = $1
		`,
		use.Jti,
	).Scan(&memberId, &cmd, &result); err != nil {
		return nil, false, err
	}
	if memberId != use.MemberId || cmd != use.Cmd {
		// a jti is a token's, and a token names one member and one command
		return nil, false, errors.New("the admin token's jti names another use")
	}
	if result == nil {
		return nil, false, ErrAdminTokenUseNoResult
	}
	return result, false, nil
}

// Stores the first use's answer in its transaction, for a replay to return.
func StoreAdminTokenUseResultInTx(ctx context.Context, tx server.PgTx, jti string, result []byte) error {
	if result == nil {
		result = []byte{}
	}
	tag, err := tx.Exec(
		ctx,
		`
			UPDATE admin_token_use
			SET result = $2
			WHERE jti = $1 AND result IS NULL
		`,
		jti,
		result,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("the admin token's use was not claimed in this transaction")
	}
	return nil
}

// Deletes the rows used before minUseTime: what the hourly sweep runs with now
// less Retention. A no-op without MG1.
func RemoveOldAdminTokenUses(ctx context.Context, minUseTime time.Time) (removedCount int64) {
	if !server.SidecarMg1Linked() {
		return 0
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM admin_token_use
				WHERE use_time < $1
			`,
			minUseTime.UTC(),
		))
		removedCount = tag.RowsAffected()
	})
	return
}
