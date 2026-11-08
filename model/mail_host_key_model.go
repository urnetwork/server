package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// Receivers' mail host keys (§15.5, §15.15; mx-3): each key a receiver
// published for a host through POST /dns/key-record, whose transaction writes
// the row here (UpsertMailHostKeyInTx) beside its TLSA 3 1 1 (DNS-M7-02). A
// key goes live two TTLs after publish_time (RFC 7671 §8.1), which readers
// judge from that column. A receiver's removal retires all its keys in the
// deactivation's transaction (RetireMailHostKeysInTx, MAIL-M7-22's removal
// effect; I-15), and a retired row goes 30 days after retire_time
// (RemoveRetiredMailHostKeys, work.MailRetention), a network under a live §21.9
// hold keeping its rows.
//
// The route projection reads a host's published keys, newest first
// (ListMailHostKeys; plan-MAIL-hints #2), and the direct-MX prober reads the
// SPKI hashes it verifies against.

// Where a host key stands.
type MailHostKeyState string

const (
	// Its TLSA is published and the receiver may serve with it.
	MailHostKeyStatePublished MailHostKeyState = "published"
	// Its TLSA is gone; the row stays 30 days after retire_time.
	MailHostKeyStateRetired MailHostKeyState = "retired"
)

const (
	// mx, or mx.<scope> with a scope of one LDH label: 3 + 63
	mailHostMaxByteCount = 66
	// a mail host key's DER SubjectPublicKeyInfo under SHA-256
	mailHostKeySpkiHashByteCount = sha256.Size
)

// A key that no row can hold: no network or receiver, a host other than mx
// or mx.<scope>, or a hash of another size. The caller's bug; nothing is
// written.
var ErrMailHostKeyInvalid = errors.New("mail host key invalid")

// One row of mail_host_key.
type MailHostKey struct {
	NetworkId server.Id
	// mx, or mx.<scope>.
	Host              string
	ReceiverSidecarId server.Id
	// SHA-256 of the key's DER SubjectPublicKeyInfo, TLSA's 3 1 1 data.
	SpkiSha256 []byte
	State      MailHostKeyState
	// When its TLSA was written. Zero on a key not yet stored.
	PublishTime time.Time
	// When it retired. Zero while it is published.
	RetireTime time.Time
}

// Tunables of the host key rows.
type MailHostKeySettings struct {
	// A retired row goes this long after retire_time (mx-3: 30 d).
	RetiredRetention time.Duration
	// Rows one sweep transaction deletes, and how long one sweep runs before
	// it leaves the rest to its next run.
	SweepBatchSize int
	SweepRunBudget time.Duration
}

func DefaultMailHostKeySettings() *MailHostKeySettings {
	return &MailHostKeySettings{
		RetiredRetention: 30 * 24 * time.Hour,
		SweepBatchSize:   10000,
		SweepRunBudget:   30 * time.Second,
	}
}

// Whether host is mx, or mx.<scope> with scope one lowercase LDH label.
func mailHostValid(host string) bool {
	if host == "mx" {
		return true
	}
	scope, found := cutMailHostScope(host)
	if !found || scope == "" || mailHostMaxByteCount < len(host) || scope[0] == '-' || scope[len(scope)-1] == '-' {
		return false
	}
	for i := range len(scope) {
		c := scope[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func cutMailHostScope(host string) (scope string, found bool) {
	const prefix = "mx."
	if len(host) <= len(prefix) || host[:len(prefix)] != prefix {
		return "", false
	}
	return host[len(prefix):], true
}

// Writes key as published inside the caller's transaction, POST
// /dns/key-record's (reading 4 of the MAIL plan). A key already published keeps
// its publish_time, so a receiver re-registering after restore_required (§5,
// §19.10) changes nothing; a retired key published again is a new
// publication, from now. Returns whether the key's row was written or
// changed.
func UpsertMailHostKeyInTx(ctx context.Context, tx server.PgTx, key *MailHostKey) (changed bool, err error) {
	if !server.SidecarMg7Linked() {
		return false, ErrMailUnlinked
	}
	if key == nil || key.NetworkId == (server.Id{}) || key.ReceiverSidecarId == (server.Id{}) ||
		!mailHostValid(key.Host) || len(key.SpkiSha256) != mailHostKeySpkiHashByteCount {
		return false, ErrMailHostKeyInvalid
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO mail_host_key (network_id, host, receiver_sidecar_id, spki_sha256, state, publish_time, retire_time)
		VALUES ($1, $2, $3, $4, $5, $6, NULL)
		ON CONFLICT (network_id, host, receiver_sidecar_id, spki_sha256) DO UPDATE SET
			state = excluded.state,
			publish_time = excluded.publish_time,
			retire_time = NULL
		WHERE mail_host_key.state <> excluded.state
		`,
		key.NetworkId,
		key.Host,
		key.ReceiverSidecarId,
		key.SpkiSha256,
		string(MailHostKeyStatePublished),
		server.NowUtc(),
	))
	return tag.RowsAffected() == 1, nil
}

// Retires every published key of receiverSidecarId in networkId inside the
// caller's transaction: a receiver's removal (I-15), MAIL-M7-22's removal
// effect, which §19.10 may apply again, so a second run retires nothing and
// returns none. Returns the keys it retired, whose TLSA the same transaction
// deletes.
func RetireMailHostKeysInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id) (retired []*MailHostKey, err error) {
	if !server.SidecarMg7Linked() {
		return nil, ErrMailUnlinked
	}
	now := server.NowUtc()
	result, queryErr := tx.Query(
		ctx,
		`
		UPDATE mail_host_key
		SET state = $3, retire_time = $4
		WHERE network_id = $1 AND receiver_sidecar_id = $2 AND state = $5
		RETURNING network_id, host, receiver_sidecar_id, spki_sha256, state, publish_time, retire_time
		`,
		networkId,
		receiverSidecarId,
		string(MailHostKeyStateRetired),
		now,
		string(MailHostKeyStatePublished),
	)
	server.WithPgResult(result, queryErr, func() {
		for result.Next() {
			retired = append(retired, scanMailHostKey(result))
		}
	})
	return retired, nil
}

// Retires every published key of networkId at now: its deletion's.
func retireNetworkMailHostKeysInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE mail_host_key
		SET state = $2, retire_time = $3
		WHERE network_id = $1 AND state = $4
		`,
		networkId,
		string(MailHostKeyStateRetired),
		now,
		string(MailHostKeyStatePublished),
	))
}

func scanMailHostKey(result interface{ Scan(dest ...any) error }) *MailHostKey {
	key := &MailHostKey{}
	var state string
	var retireTime *time.Time
	server.Raise(result.Scan(
		&key.NetworkId,
		&key.Host,
		&key.ReceiverSidecarId,
		&key.SpkiSha256,
		&state,
		&key.PublishTime,
		&retireTime,
	))
	key.State = MailHostKeyState(state)
	if retireTime != nil {
		key.RetireTime = *retireTime
	}
	return key
}

// The network's host keys, published ones first, each group newest first: what
// the route projection's receiver_sidecar_ids (plan-MAIL-hints #2) and the
// direct-MX prober read. Retired rows stay listed until their sweep, so a
// reader filters on State. Nil when the binary does not link MG7.
func ListMailHostKeys(ctx context.Context, networkId server.Id) (keys []*MailHostKey) {
	if !server.SidecarMg7Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, host, receiver_sidecar_id, spki_sha256, state, publish_time, retire_time
			FROM mail_host_key
			WHERE network_id = $1
			ORDER BY (state = $2) DESC, publish_time DESC, host, receiver_sidecar_id, spki_sha256
			`,
			networkId,
			string(MailHostKeyStatePublished),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				keys = append(keys, scanMailHostKey(result))
			}
		})
	})
	return
}

const removeRetiredMailHostKeysSql = `
	DELETE FROM mail_host_key
	WHERE (network_id, host, receiver_sidecar_id, spki_sha256) IN (
		SELECT key_row.network_id, key_row.host, key_row.receiver_sidecar_id, key_row.spki_sha256
		FROM mail_host_key key_row
		WHERE
			key_row.retire_time < $1 AND
			NOT EXISTS (
				SELECT 1
				FROM abuse_legal_hold hold
				WHERE
					hold.network_id = key_row.network_id AND
					hold.part = 0 AND
					(hold.expire_time IS NULL OR $2 < hold.expire_time)
			)
		ORDER BY key_row.retire_time
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// Deletes the host keys retired more than 30 days before now, oldest first,
// in batches, skipping a network that holds a live §21.9 hold (which keeps
// more and deletes less on any doubt). Returns how many went and whether the
// run left some for the next.
func RemoveRetiredMailHostKeys(ctx context.Context, now time.Time) (removedCount int64, more bool) {
	if !server.SidecarMg7Linked() {
		return 0, false
	}
	settings := DefaultMailHostKeySettings()
	cutoff := now.UTC().Add(-settings.RetiredRetention)
	return removeRetentionBatches(
		ctx,
		RetentionTableMailHostKey,
		settings.SweepBatchSize,
		time.Now().Add(settings.SweepRunBudget),
		func(tx server.PgTx) int64 {
			tag := server.RaisePgResult(tx.Exec(ctx, removeRetiredMailHostKeysSql, cutoff, now.UTC(), settings.SweepBatchSize))
			return tag.RowsAffected()
		},
	)
}
