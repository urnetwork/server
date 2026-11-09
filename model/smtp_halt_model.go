package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge's halts (§14.8; D-SMTP-9; SMTP-M9-18): smtp_halt, MG9's. A halt is a
// network, or one burner label of it, its §14.8 trigger and its start, the
// report's arrival, and it holds smtpedgeservice.SmtpHaltDuration from then
// (smtp-4, which lists no end). SMTP-M9-16 sets halts beside their Redis flag,
// and every edge reloads the ones still holding every 60 s, so a halt reaches
// Postgres first and survives Redis's loss (§19.7). Nothing here reads or
// writes the table unless the binary links MG9.

// Why halt cannot be a smtp_halt row, or nil.
func smtpHaltRefusal(halt *smtpedgeservice.SmtpHalt) error {
	refuse := func(field string) error {
		return fmt.Errorf("%w: a halt's %s", ErrSmtpRowRefused, field)
	}
	switch {
	case halt == nil:
		return refuse("row")
	case halt.HaltId == (sidecar.Id{}):
		return refuse("halt id")
	case halt.NetworkId == (sidecar.Id{}):
		return refuse("network")
	case !smtpTextStorable(halt.Label, smtpLabelMaxByteCount):
		return refuse("label")
	case halt.Trigger == "" || !smtpTextStorable(halt.Trigger, smtpWordMaxByteCount):
		return refuse("trigger")
	case halt.StartTime.IsZero():
		return refuse("start")
	}
	return nil
}

// Writes halt in tx, replacing one stored under the same halt id, so a halt
// and what set it (a case, an appeal's lift) can commit together.
func SetSmtpHaltInTx(ctx context.Context, tx server.PgTx, halt *smtpedgeservice.SmtpHalt) error {
	if !server.SidecarMg9Linked() {
		return ErrSmtpEdgeUnlinked
	}
	if err := smtpHaltRefusal(halt); err != nil {
		return err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO smtp_halt (halt_id, network_id, label, trigger, start_time)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (halt_id) DO UPDATE
		SET
			network_id = EXCLUDED.network_id,
			label = EXCLUDED.label,
			trigger = EXCLUDED.trigger,
			start_time = EXCLUDED.start_time
		`,
		server.Id(halt.HaltId),
		server.Id(halt.NetworkId),
		smtpNullableText(halt.Label),
		halt.Trigger,
		halt.StartTime.UTC(),
	))
	return nil
}

// SetSmtpHaltInTx in a transaction of its own, opened only for a row it can
// store.
func SetSmtpHalt(ctx context.Context, halt *smtpedgeservice.SmtpHalt) (returnErr error) {
	if !server.SidecarMg9Linked() {
		return ErrSmtpEdgeUnlinked
	}
	if err := smtpHaltRefusal(halt); err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = SetSmtpHaltInTx(ctx, tx, halt)
	})
	return
}

// Every halt still holding at now, by start: those that started less than
// smtpedgeservice.SmtpHaltDuration before it, a halt dated after now included.
func GetActiveSmtpHalts(ctx context.Context, now time.Time) (halts []*smtpedgeservice.SmtpHalt, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT halt_id, network_id, label, trigger, start_time
			FROM smtp_halt
			WHERE $1 < start_time
			ORDER BY start_time, halt_id
			`,
			now.UTC().Add(-smtpedgeservice.SmtpHaltDuration),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var haltId server.Id
				var networkId server.Id
				var label *string
				halt := &smtpedgeservice.SmtpHalt{}
				server.Raise(result.Scan(&haltId, &networkId, &label, &halt.Trigger, &halt.StartTime))
				halt.HaltId = sidecar.Id(haltId)
				halt.NetworkId = sidecar.Id(networkId)
				halt.Label = smtpTextOf(label)
				halts = append(halts, halt)
			}
		})
	})
	return
}
