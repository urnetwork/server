package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// §15.15's mail tables (MAIL-M7-21; §2.8 mx-2, mx-3), MG7's §15 part: which
// networks take mail (mail_network), the receivers' published host keys
// (mail_host_key_model.go), a Team's direct MX (mail_direct_mx_model.go) and
// the router's session records (mail_connection_record_model.go). No row holds
// a sender's address, a burner label or any per-network mail address (owner
// decisions 32, 34).
//
// Nothing here reads or writes the tables unless the binary links MG7
// (server.SidecarMg7Linked): a read answers nothing and a write
// ErrMailUnlinked, so code reading them stays off until M7's Gn-out
// registers the group (D-OPS-10).

// The binary does not link MG7, so the tables are not there.
var ErrMailUnlinked = errors.New("mail tables need MG7")

// A mail-enabled network (mx-3).
type MailNetwork struct {
	NetworkId server.Id
	// When mail was turned on, which starts §14.7's 24 h send gate
	// (SMTP-M9-10 reads it).
	CreateTime time.Time
}

// Turns mail on for networkId inside the caller's transaction, POST
// /mail/enable's write (MAIL-M7-22 adds §12's mail records beside it). A
// network already enabled keeps its row and its create_time, so enabling
// again never restarts the send gate. Returns the row and whether this call
// created it.
func EnableMailInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (mailNetwork *MailNetwork, created bool, err error) {
	if !server.SidecarMg7Linked() {
		return nil, false, ErrMailUnlinked
	}
	now := server.NowUtc()
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO mail_network (network_id, create_time)
		VALUES ($1, $2)
		ON CONFLICT (network_id) DO NOTHING
		`,
		networkId,
		now,
	))
	created = tag.RowsAffected() == 1
	mailNetwork = &MailNetwork{NetworkId: networkId}
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT create_time FROM mail_network WHERE network_id = $1`,
		networkId,
	).Scan(&mailNetwork.CreateTime))
	return mailNetwork, created, nil
}

// The network's mail row, nil when mail is off or the binary does not link
// MG7.
func GetMailNetwork(ctx context.Context, networkId server.Id) (mailNetwork *MailNetwork) {
	if !server.SidecarMg7Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		mailNetwork = getMailNetworkInQuerier(ctx, conn, networkId)
	})
	return
}

// As GetMailNetwork, inside the caller's transaction.
func GetMailNetworkInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *MailNetwork {
	if !server.SidecarMg7Linked() {
		return nil
	}
	return getMailNetworkInQuerier(ctx, tx, networkId)
}

func getMailNetworkInQuerier(ctx context.Context, querier server.PgQuerier, networkId server.Id) (mailNetwork *MailNetwork) {
	result, err := querier.Query(
		ctx,
		`SELECT network_id, create_time FROM mail_network WHERE network_id = $1`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			mailNetwork = &MailNetwork{}
			server.Raise(result.Scan(&mailNetwork.NetworkId, &mailNetwork.CreateTime))
		}
	})
	return
}

// What a network's deletion does to its mail rows, in the deletion's
// transaction (mx-3's ttl): mail_network and every direct-MX row go, the
// network's receivers being removed with it, and its published host keys
// retire, to go 30 days later with the rest. A binary that does not link MG7
// does nothing.
func removeMailNetworkInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	if !server.SidecarMg7Linked() {
		return
	}
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM mail_network WHERE network_id = $1`, networkId))
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM mail_direct_mx WHERE network_id = $1`, networkId))
	retireNetworkMailHostKeysInTx(ctx, tx, networkId, server.NowUtc())
}
