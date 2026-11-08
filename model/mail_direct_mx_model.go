package model

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// A Team's direct MX (§15.4, §12.8; owner decisions 33, 98; mx-3): up to two
// of the network's mail indexers answering public TCP 25 themselves. POST
// /mail/direct-mx writes a row pending (MAIL-M7-23), mxedge's prober moves it
// to verified once the address, STARTTLS and the receiver's mail_host_key
// SPKI check out, and to failing when a re-probe fails, and DELETE or the
// receiver's removal deletes it (I-15). The address is the receiver's own
// public server address, which §12.8 publishes as an A or AAAA record, never
// a sender's.

// Where a direct-MX receiver stands.
type MailDirectMxState string

const (
	// Written, not yet verified: §12.8 does not point mx at it.
	MailDirectMxStatePending MailDirectMxState = "pending"
	// Its last probe passed.
	MailDirectMxStateVerified MailDirectMxState = "verified"
	// A re-probe failed. It stays until DELETE, reverting nothing by itself (§15.4).
	MailDirectMxStateFailing MailDirectMxState = "failing"
)

// The most direct-MX receivers a network names (§15.4).
const MailDirectMxMaxReceiverCount = 2

var (
	// A row no network could name: no network or receiver, an address that is
	// not a public unicast one, or an unknown state.
	ErrMailDirectMxInvalid = errors.New("mail direct mx invalid")
	// The network already names MailDirectMxMaxReceiverCount others.
	ErrMailDirectMxFull = errors.New("mail direct mx full")
)

// One row of mail_direct_mx.
type MailDirectMx struct {
	NetworkId         server.Id
	ReceiverSidecarId server.Id
	// A public unicast address, in its canonical text.
	Address netip.Addr
	State   MailDirectMxState
	// When its last probe passed. Zero until one does.
	VerifyTime time.Time
}

// Whether addr is one a direct MX may name at all: a unicast address with no
// zone outside loopback, link-local, multicast and the private ranges. §12.7's
// full A rules (documentation, 100.64/10, 198.18/15, connect's stand-ins and
// the rest) run where they bite, in mxedge's verification (MAIL-M7-23) and in
// §12's write of the A or AAAA record, so no address reaches a record without
// them.
func mailDirectMxAddressValid(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsValid() && addr.Zone() == "" && addr.IsGlobalUnicast() && !addr.IsPrivate()
}

// Names receiverSidecarId at address as one of networkId's direct MX, pending,
// inside the caller's transaction. Writing the same row again changes
// nothing; a third receiver is ErrMailDirectMxFull.
func PutMailDirectMxInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id, address netip.Addr) error {
	if !server.SidecarMg7Linked() {
		return ErrMailUnlinked
	}
	if networkId == (server.Id{}) || receiverSidecarId == (server.Id{}) || !mailDirectMxAddressValid(address) {
		return ErrMailDirectMxInvalid
	}
	var otherCount int
	server.Raise(tx.QueryRow(
		ctx,
		`
		SELECT count(DISTINCT receiver_sidecar_id)
		FROM mail_direct_mx
		WHERE network_id = $1 AND receiver_sidecar_id <> $2
		`,
		networkId,
		receiverSidecarId,
	).Scan(&otherCount))
	if MailDirectMxMaxReceiverCount <= otherCount {
		return ErrMailDirectMxFull
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO mail_direct_mx (network_id, receiver_sidecar_id, address, state, verify_time)
		VALUES ($1, $2, $3, $4, NULL)
		ON CONFLICT (network_id, receiver_sidecar_id, address) DO NOTHING
		`,
		networkId,
		receiverSidecarId,
		address.Unmap().String(),
		string(MailDirectMxStatePending),
	))
	return nil
}

// Sets a row's state from a probe: verified with its time, or failing, which
// keeps the last verify_time. A row that is gone stays gone.
func SetMailDirectMxProbeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id, address netip.Addr, passed bool) error {
	if !server.SidecarMg7Linked() {
		return ErrMailUnlinked
	}
	if passed {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE mail_direct_mx SET state = $4, verify_time = $5
			WHERE network_id = $1 AND receiver_sidecar_id = $2 AND address = $3
			`,
			networkId,
			receiverSidecarId,
			address.Unmap().String(),
			string(MailDirectMxStateVerified),
			server.NowUtc(),
		))
		return nil
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE mail_direct_mx SET state = $4
		WHERE network_id = $1 AND receiver_sidecar_id = $2 AND address = $3
		`,
		networkId,
		receiverSidecarId,
		address.Unmap().String(),
		string(MailDirectMxStateFailing),
	))
	return nil
}

// Deletes every direct-MX row of networkId, DELETE /mail/direct-mx's write.
// Returns how many went.
func RemoveMailDirectMxInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (removedCount int64, err error) {
	if !server.SidecarMg7Linked() {
		return 0, ErrMailUnlinked
	}
	tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM mail_direct_mx WHERE network_id = $1`, networkId))
	return tag.RowsAffected(), nil
}

// Deletes receiverSidecarId's direct-MX rows in networkId: its removal's
// (I-15), idempotent, so §19.10's re-application deletes nothing more.
func RemoveMailDirectMxReceiverInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id) (removedCount int64, err error) {
	if !server.SidecarMg7Linked() {
		return 0, ErrMailUnlinked
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM mail_direct_mx WHERE network_id = $1 AND receiver_sidecar_id = $2`,
		networkId,
		receiverSidecarId,
	))
	return tag.RowsAffected(), nil
}

// The network's direct-MX rows, by receiver then address. Nil when the binary
// does not link MG7.
func ListMailDirectMx(ctx context.Context, networkId server.Id) (rows []*MailDirectMx) {
	if !server.SidecarMg7Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, receiver_sidecar_id, address, state, verify_time
			FROM mail_direct_mx
			WHERE network_id = $1
			ORDER BY receiver_sidecar_id, address
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &MailDirectMx{}
				var address string
				var state string
				var verifyTime *time.Time
				server.Raise(result.Scan(&row.NetworkId, &row.ReceiverSidecarId, &address, &state, &verifyTime))
				// a row this package wrote parses; one that does not is no
				// address to point mail at
				parsed, parseErr := netip.ParseAddr(address)
				if parseErr != nil {
					continue
				}
				row.Address = parsed
				row.State = MailDirectMxState(state)
				if verifyTime != nil {
					row.VerifyTime = *verifyTime
				}
				rows = append(rows, row)
			}
		})
	})
	return
}
