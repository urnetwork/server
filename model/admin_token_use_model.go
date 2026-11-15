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
// An answer that carries a lasting credential once keeps it out of the row, so
// the server never holds it, and a replay answers without it: an OAuth client's
// secret, an invite's bearer and an audit webhook's signing key.
//
// Nothing here reads or writes admin_token_use unless MG1 is linked; until then
// an admin token's command answers unavailable and runs nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// ClaimAdminTokenUseInTx for a route whose answer is JSON: true with the first
// use's answer decoded into replayed for a replay, false for the first use,
// which runs the command and then stores its answer
// (StoreAdminTokenUseAnswerInTx). A route calls it once its caller passed its
// checks, so a refused caller spends nothing.
func ReplayAdminTokenUseInTx(ctx context.Context, tx server.PgTx, use *session.AdminCommandUse, now time.Time, replayed any) (replay bool, err error) {
	storedResult, first, err := ClaimAdminTokenUseInTx(ctx, tx, use, now)
	if err != nil {
		return false, err
	}
	if first {
		return false, nil
	}
	if err := json.Unmarshal(storedResult, replayed); err != nil {
		return false, err
	}
	return true, nil
}

// Stores answer's JSON as the use's result, for a replay to return. A route
// whose answer carries a lasting credential once stores the answer without it.
func StoreAdminTokenUseAnswerInTx(ctx context.Context, tx server.PgTx, use *session.AdminCommandUse, answer any) error {
	resultJson, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return StoreAdminTokenUseResultInTx(ctx, tx, use.Jti, resultJson)
}

// The scope a Team route's admin command needs: §7.8's OAuth client scopes
// hold none narrower for the Team routes, so only all.
const adminCommandTeamScope = "all"

// 403 for an admin token without the all scope; nil for any other credential.
func requireAdminCommandTeamScope(clientSession *session.ClientSession) error {
	adminCommand := clientSession.AdminCommand
	if adminCommand != nil && !slices.Contains(adminCommand.Scopes, adminCommandTeamScope) {
		return fmt.Errorf("%d %s", http.StatusForbidden, "The admin token does not carry the all scope.")
	}
	return nil
}

// The refusal of a replay that met a first use with no stored answer. A route
// whose command spans two transactions, as domain-verify's lookup between them
// does, claims the use in its first and stores the answer in its last, so a
// replay racing that first use, or one after a first use that ended between
// them, runs nothing and answers this.
const adminTokenUseUnansweredMessage = "The admin token's command has not answered. Run the command again."

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
