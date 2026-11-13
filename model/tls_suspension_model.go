package model

// §13.9's kill switch (TLS-M5-13; R-TLS-5, D-TLS-9, D-PROTO-12, I-14; §2.8
// tls-4): a host of a network, or * for all of them, refused at tlsedge and
// at Find within 5 s of the write.
//
// A kill is a row of network_tls_suspension and a field of the network's
// {tes_<network_id>} hash, <host or *> to the entry's JSON
// (sidecar.TlsKillEntry). SetTlsSuspension writes the row, then one MULTI sets
// the field and INCRs {tes_<network_id>}n. Both keys share one hash tag, so
// the MULTI is one slot's, and the INCR is the trigger: production's keyspace
// classes emit nothing for HSET (D-PROTO-12), so tlsedge and sockets watch
// the n keys (TlsSuspensionKeyEventPattern) on their one shared subscriber and
// read the hash a trigger names.
//
// Row and key go only when the case lifts the kill (LiftTlsSuspension) or its
// until passes (LiftExpiredTlsSuspensions, taskworker's sweep). Nothing here
// reconciles one from the other: a reader drops a kill only once neither its
// row nor its key holds it, so a Postgres restore that lost a row lifts
// nothing (§21.10), and §19.10's abuse step re-applies the kills and lifts
// mailed after the dump. The sweep removes a key's field only while it still
// holds the entry the expired row wrote, so a kill set again meanwhile stays.
//
// api's abuse actions and taskworker's abuse worker call these directly.
// POST /tls/suspend is built only once a caller outside those two exists
// (RESOLUTIONS plan-TLS-internal-credentials), so no route reaches this file.
//
// The table is MG5's: a binary that does not link the group writes and reads
// nothing (server.SidecarMg5Linked).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

var (
	// This binary does not link MG5, so the table does not exist for it.
	ErrTlsSuspensionUnavailable = errors.New("tls suspensions are not linked in this binary")
	// No network, a host that is neither *, @ nor a lowercase host name, or a
	// reason that is empty or past its column.
	ErrTlsSuspensionInvalid = errors.New("tls suspension invalid")
)

// Tunables of the kill switch.
type TlsSuspensionSettings struct {
	// The longest reason a row keeps: network_tls_suspension's column.
	ReasonMaxByteCount int
	// The longest host and the longest label in one: RFC 1035's.
	HostMaxByteCount  int
	LabelMaxByteCount int
	// Expired rows one sweep pass lifts, the rest left to the next.
	SweepBatchCount int
}

func DefaultTlsSuspensionSettings() *TlsSuspensionSettings {
	return &TlsSuspensionSettings{
		ReasonMaxByteCount: 256,
		HostMaxByteCount:   253,
		LabelMaxByteCount:  63,
		SweepBatchCount:    1000,
	}
}

// Deletes a kill's field only while it still holds the entry its expired row
// wrote, with the trigger: a kill set again since keeps its field.
//
// KEYS[1] {tes_<network_id>}, KEYS[2] {tes_<network_id>}n
// ARGV[1] the host, ARGV[2] the expired entry's JSON
var tlsSuspensionExpireScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], ARGV[1]) ~= ARGV[2] then
	return 0
end
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('INCR', KEYS[2])
return 1
`)

// The keyspace channel pattern of every network's {tes_<network_id>}n on db:
// the one family tlsedge and sockets watch for kills, through one shared
// subscription per process, never one per network.
func TlsSuspensionKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:%s", db, sidecar.TlsKillTriggerKeyPattern)
}

// The network whose {tes_<network_id>}n a channel matching
// TlsSuspensionKeyEventPattern names.
func ParseTlsSuspensionKeyEvent(channel string) (server.Id, bool) {
	key, ok := ParseKeyspaceChannelKey(channel)
	if !ok {
		return server.Id{}, false
	}
	networkId, ok := sidecar.ParseTlsKillTriggerKey(key)
	if !ok {
		return server.Id{}, false
	}
	return server.Id(networkId), true
}

// Whether host is one a kill can name: *, @, or a lowercase LDH host name with
// no trailing dot, as tlsedge's map and Find's names spell a published host.
func tlsSuspensionHostValid(host string, settings *TlsSuspensionSettings) bool {
	if host == sidecar.TlsKillAllHosts || host == "@" {
		return true
	}
	if host == "" || settings.HostMaxByteCount < len(host) {
		return false
	}
	labelByteCount := 0
	for i := 0; i < len(host); i += 1 {
		c := host[i]
		switch {
		case c == '.':
			if labelByteCount == 0 || host[i-1] == '-' {
				return false
			}
			labelByteCount = 0
			continue
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case c == '-':
			if labelByteCount == 0 {
				return false
			}
		default:
			return false
		}
		labelByteCount += 1
		if settings.LabelMaxByteCount < labelByteCount {
			return false
		}
	}
	return 0 < labelByteCount && host[len(host)-1] != '-'
}

// A kill's until as its row keeps it: UTC, whole to the microsecond, so the
// row and the key's entry say one time.
func tlsSuspensionUntil(until time.Time) time.Time {
	if until.IsZero() {
		return time.Time{}
	}
	return until.UTC().Truncate(time.Microsecond)
}

// The entry's JSON, a field's value in {tes_<network_id>}.
func tlsSuspensionEntryValue(host string, reason string, until time.Time) ([]byte, error) {
	return json.Marshal(&sidecar.TlsKillEntry{Host: host, Reason: reason, Until: tlsSuspensionUntil(until)})
}

// Sets the field and INCRs the trigger in one MULTI, or removes the field and
// INCRs it when value is nil.
func tlsSuspensionWriteKey(ctx context.Context, networkId server.Id, host string, value []byte) (returnErr error) {
	key := sidecar.TlsKillKey(sidecar.Id(networkId))
	triggerKey := sidecar.TlsKillTriggerKey(sidecar.Id(networkId))
	server.Redis(ctx, func(r server.RedisClient) {
		_, returnErr = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			if value == nil {
				pipe.HDel(ctx, key, host)
			} else {
				pipe.HSet(ctx, key, host, value)
			}
			pipe.Incr(ctx, triggerKey)
			return nil
		})
	})
	return
}

// Kills a host of a network, or every host with sidecar.TlsKillAllHosts,
// until its case lifts it or args.Until passes: the row first, then the key
// and its trigger in one MULTI. A kill already set takes the new reason and
// until. An error after the row is written leaves the row, which every
// reader's reload finds, and a second call finishes the key.
func SetTlsSuspension(ctx context.Context, args *sidecar.TlsSuspendArgs) error {
	return SetTlsSuspensionWithSettings(ctx, args, DefaultTlsSuspensionSettings())
}

func SetTlsSuspensionWithSettings(ctx context.Context, args *sidecar.TlsSuspendArgs, settings *TlsSuspensionSettings) (returnErr error) {
	if !server.SidecarMg5Linked() {
		return ErrTlsSuspensionUnavailable
	}
	switch {
	case args == nil || args.NetworkId == (sidecar.Id{}):
		return fmt.Errorf("%w: no network", ErrTlsSuspensionInvalid)
	case !tlsSuspensionHostValid(args.Host, settings):
		return fmt.Errorf("%w: host", ErrTlsSuspensionInvalid)
	case args.Reason == "" || settings.ReasonMaxByteCount < len(args.Reason):
		return fmt.Errorf("%w: reason", ErrTlsSuspensionInvalid)
	}
	networkId := server.Id(args.NetworkId)
	until := tlsSuspensionUntil(args.Until)
	value, err := tlsSuspensionEntryValue(args.Host, args.Reason, until)
	if err != nil {
		return err
	}
	var untilTime *time.Time
	if !until.IsZero() {
		untilTime = &until
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_tls_suspension (
					network_id,
					host,
					reason,
					until_time
				)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (network_id, host) DO UPDATE
				SET
					reason = EXCLUDED.reason,
					until_time = EXCLUDED.until_time
			`,
			networkId,
			args.Host,
			args.Reason,
			untilTime,
		))
	})
	return tlsSuspensionWriteKey(ctx, networkId, args.Host, value)
}

// Lifts a kill, as its case does: the row, then the key's field with the
// trigger. A kill that has no row is still lifted from the key, since a
// restore may have lost the row and kept the key. Lifting what is not set
// changes nothing but the trigger.
func LiftTlsSuspension(ctx context.Context, networkId server.Id, host string) error {
	if !server.SidecarMg5Linked() {
		return ErrTlsSuspensionUnavailable
	}
	if networkId == (server.Id{}) || !tlsSuspensionHostValid(host, DefaultTlsSuspensionSettings()) {
		return fmt.Errorf("%w: network or host", ErrTlsSuspensionInvalid)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_tls_suspension
				WHERE network_id = $1 AND host = $2
			`,
			networkId,
			host,
		))
	})
	return tlsSuspensionWriteKey(ctx, networkId, host, nil)
}

// Every row of network_tls_suspension, as tlsedge's and sockets' reloads read
// them. A row whose until has passed may still be here until the sweep lifts
// it, and a reader takes it as lifted.
func GetTlsSuspensions(ctx context.Context) (suspensions []*sidecar.TlsSuspendArgs, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrTlsSuspensionUnavailable
	}
	suspensions = []*sidecar.TlsSuspendArgs{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, host, reason, until_time
				FROM network_tls_suspension
				ORDER BY network_id, host
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var untilTime *time.Time
				suspension := &sidecar.TlsSuspendArgs{}
				server.Raise(result.Scan(&networkId, &suspension.Host, &suspension.Reason, &untilTime))
				suspension.NetworkId = sidecar.Id(networkId)
				if untilTime != nil {
					suspension.Until = untilTime.UTC()
				}
				suspensions = append(suspensions, suspension)
			}
		})
	})
	return
}

// Lifts the kills whose until has passed at now, at most a batch: each row,
// then its key's field if that still holds the row's entry. A kill set again
// between the two keeps both. Returns how many rows went.
func LiftExpiredTlsSuspensions(ctx context.Context, now time.Time) (int, error) {
	return LiftExpiredTlsSuspensionsWithSettings(ctx, now, DefaultTlsSuspensionSettings())
}

func LiftExpiredTlsSuspensionsWithSettings(ctx context.Context, now time.Time, settings *TlsSuspensionSettings) (int, error) {
	if !server.SidecarMg5Linked() {
		return 0, nil
	}
	type expiredSuspension struct {
		networkId server.Id
		host      string
		reason    string
		until     time.Time
	}
	expired := []*expiredSuspension{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, host, reason, until_time
				FROM network_tls_suspension
				WHERE until_time <= $1
				ORDER BY until_time
				LIMIT $2
			`,
			now.UTC(),
			settings.SweepBatchCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				suspension := &expiredSuspension{}
				server.Raise(result.Scan(&suspension.networkId, &suspension.host, &suspension.reason, &suspension.until))
				expired = append(expired, suspension)
			}
		})
	})
	liftedCount := 0
	errs := []error{}
	for _, suspension := range expired {
		value, err := tlsSuspensionEntryValue(suspension.host, suspension.reason, suspension.until)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// only the row this pass read: one set again since has a later until, or none
		var rowCount int64
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					DELETE FROM network_tls_suspension
					WHERE network_id = $1 AND host = $2 AND until_time = $3
				`,
				suspension.networkId,
				suspension.host,
				suspension.until.UTC(),
			))
			rowCount = tag.RowsAffected()
		})
		if rowCount == 0 {
			continue
		}
		liftedCount += 1
		keys := []string{
			sidecar.TlsKillKey(sidecar.Id(suspension.networkId)),
			sidecar.TlsKillTriggerKey(sidecar.Id(suspension.networkId)),
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if err := tlsSuspensionExpireScript.Run(ctx, r, keys, suspension.host, string(value)).Err(); err != nil {
				errs = append(errs, err)
			}
		})
	}
	return liftedCount, errors.Join(errs...)
}
