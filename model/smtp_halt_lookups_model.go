package model

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// What §14.8's halt rule reads beyond the six SmtpEdgeStore calls
// (smtpedgeservice.SmtpHaltLookups; SMTP-M9-16), over MG5's smtp_complaint and
// MG9's smtp_halt and smtp_send, each on an index the tables already carry: a
// network's complaints by arrival or by Message-ID hash, its halts by start,
// and its sends by start. Nothing here reads a sealed copy. A binary that does
// not link a call's group answers that group's error.

var _ smtpedgeservice.SmtpHaltLookups = (*SmtpEdgeStore)(nil)

// The network's complaints key matches, oldest first, sealed copies left out,
// at most limit.
func FindSmtpComplaints(ctx context.Context, key *smtpedgeservice.SmtpComplaintKey, limit int) (complaints []*smtpedgeservice.SmtpComplaint, returnErr error) {
	if !server.SidecarMg5Linked() {
		returnErr = ErrSmtpReportUnlinked
		return
	}
	if key == nil || key.NetworkId == (sidecar.Id{}) || limit <= 0 {
		returnErr = ErrSmtpRowRefused
		return
	}
	since := key.Since
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				complaint_id, reporter, complaint_type, arrival_time, domain, dkim_domain,
				dkim_selector, dkim_result, message_id_hash, x_ur_ref, feedback_id, join_result
			FROM smtp_complaint
			WHERE network_id = $1 AND $2 <= arrival_time AND ($3::bytea IS NULL OR message_id_hash = $3)
			ORDER BY arrival_time, complaint_id
			LIMIT $4
			`,
			server.Id(key.NetworkId),
			since.UTC(),
			key.MessageIdHash,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var complaintId server.Id
				var reporter, domain, dkimDomain, dkimSelector, dkimResult, xUrRef, feedbackId, joinResult *string
				complaint := &smtpedgeservice.SmtpComplaint{NetworkId: key.NetworkId}
				server.Raise(result.Scan(
					&complaintId, &reporter, &complaint.Type, &complaint.ArrivalTime, &domain, &dkimDomain,
					&dkimSelector, &dkimResult, &complaint.MessageIdHash, &xUrRef, &feedbackId, &joinResult,
				))
				complaint.ComplaintId = sidecar.Id(complaintId)
				complaint.Reporter = smtpTextOf(reporter)
				complaint.Domain = smtpTextOf(domain)
				complaint.DkimDomain = smtpTextOf(dkimDomain)
				complaint.DkimSelector = smtpTextOf(dkimSelector)
				complaint.DkimResult = smtpTextOf(dkimResult)
				complaint.XUrRef = smtpTextOf(xUrRef)
				complaint.FeedbackId = smtpTextOf(feedbackId)
				complaint.JoinResult = smtpTextOf(joinResult)
				complaints = append(complaints, complaint)
			}
		})
	})
	return
}

// The network's halts started at or after since, by start.
func FindSmtpHalts(ctx context.Context, networkId server.Id, since time.Time) (halts []*smtpedgeservice.SmtpHalt, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT halt_id, label, trigger, start_time
			FROM smtp_halt
			WHERE network_id = $1 AND $2 <= start_time
			ORDER BY start_time, halt_id
			`,
			networkId,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var haltId server.Id
				var label *string
				halt := &smtpedgeservice.SmtpHalt{NetworkId: sidecar.Id(networkId)}
				server.Raise(result.Scan(&haltId, &label, &halt.Trigger, &halt.StartTime))
				halt.HaltId = sidecar.Id(haltId)
				halt.Label = smtpTextOf(label)
				halts = append(halts, halt)
			}
		})
	})
	return
}

// Removes a halt a recheck lifted (SmtpHaltLadder.Lift). An absent one is no
// error.
func DeleteSmtpHalt(ctx context.Context, haltId server.Id) (returnErr error) {
	if !server.SidecarMg9Linked() {
		return ErrSmtpEdgeUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM smtp_halt WHERE halt_id = $1`, haltId))
	})
	return
}

// How many of the network's sends started at or after since ended with result.
func CountSmtpSends(ctx context.Context, networkId server.Id, since time.Time, result string) (count uint64, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		var counted int64
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COUNT(*)::bigint
			FROM smtp_send
			WHERE network_id = $1 AND $2 <= start_time AND $3 <= send_day AND result = $4
			`,
			networkId,
			since.UTC(),
			smtpUtcDay(since),
			result,
		).Scan(&counted))
		count = uint64(counted)
	})
	return
}

// The clients of the network with a send started at or after since.
func SmtpSendingClients(ctx context.Context, networkId server.Id, since time.Time) (clientIds []sidecar.Id, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT client_id
			FROM smtp_send
			WHERE network_id = $1 AND $2 <= start_time AND $3 <= send_day
			`,
			networkId,
			since.UTC(),
			smtpUtcDay(since),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, sidecar.Id(clientId))
			}
		})
	})
	return
}

// See smtpedgeservice.SmtpHaltLookups: FindSmtpComplaints, at most the
// engine's HaltComplaintReadMaxCount.
func (self *SmtpEdgeStore) FindComplaints(ctx context.Context, key *smtpedgeservice.SmtpComplaintKey) ([]*smtpedgeservice.SmtpComplaint, error) {
	var complaints []*smtpedgeservice.SmtpComplaint
	err := self.call(ctx, server.SidecarMg5Linked, ErrSmtpReportUnlinked, func(ctx context.Context) (err error) {
		complaints, err = FindSmtpComplaints(ctx, key, smtpedgeservice.DefaultSmtpEdgeSettings().HaltComplaintReadMaxCount)
		return
	})
	if err != nil {
		return nil, err
	}
	return complaints, nil
}

// See smtpedgeservice.SmtpHaltLookups: FindSmtpHalts.
func (self *SmtpEdgeStore) FindHalts(ctx context.Context, networkId sidecar.Id, since time.Time) ([]*smtpedgeservice.SmtpHalt, error) {
	var halts []*smtpedgeservice.SmtpHalt
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		halts, err = FindSmtpHalts(ctx, server.Id(networkId), since)
		return
	})
	if err != nil {
		return nil, err
	}
	return halts, nil
}

// See smtpedgeservice.SmtpHaltLookups: CountSmtpSends.
func (self *SmtpEdgeStore) CountSends(ctx context.Context, networkId sidecar.Id, since time.Time, result string) (uint64, error) {
	var count uint64
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		count, err = CountSmtpSends(ctx, server.Id(networkId), since, result)
		return
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// See smtpedgeservice.SmtpHaltLookups: SmtpSendingClients.
func (self *SmtpEdgeStore) SendingClients(ctx context.Context, networkId sidecar.Id, since time.Time) ([]sidecar.Id, error) {
	var clientIds []sidecar.Id
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		clientIds, err = SmtpSendingClients(ctx, server.Id(networkId), since)
		return
	})
	if err != nil {
		return nil, err
	}
	return clientIds, nil
}

// See smtpedgeservice.SmtpHaltLookups: DeleteSmtpHalt.
func (self *SmtpEdgeStore) DeleteHalt(ctx context.Context, haltId sidecar.Id) error {
	return self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) error {
		return DeleteSmtpHalt(ctx, server.Id(haltId))
	})
}
