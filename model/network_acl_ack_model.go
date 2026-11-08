package model

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §8.9's acks (§2.8 api-15): POST /acl/ack records the newest projection
// version each client applied, one row per client, so a chatty network costs
// one row a client, and observes how long the version took to reach it, from
// its entry's time_ms, into a histogram, which is what measures the propagation
// targets (p50 1 s, p99 5 s on connected clients). A taskworker sweep deletes
// rows past 7 days, and no dump holds them (§19.6).
//
// An ack counts only for a network with a head at or above its version, so a
// network without a config repo writes nothing and a client cannot ack past the
// verified log. The table belongs to MG2 (server.SidecarMg2Linked).

// Tunables of the acks.
type NetworkAclAckSettings struct {
	// How long a row is kept: §2.8 api-15's 7 days.
	Retention time.Duration
	// The longest delay the histogram takes; an entry time further back, or in
	// the future, is a clock fault or a replay and is not observed.
	MaxObservedDelay time.Duration
}

func DefaultNetworkAclAckSettings() *NetworkAclAckSettings {
	return &NetworkAclAckSettings{
		Retention:        7 * 24 * time.Hour,
		MaxObservedDelay: 24 * time.Hour,
	}
}

var networkAclAckDelayHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "acl",
	Name:      "ack_delay_seconds",
	Help:      "From an authorization log entry's time to a client's first ack of its version (§8.9's propagation targets)",
	Buckets:   []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 300, 3600},
})

func init() {
	prometheus.MustRegister(networkAclAckDelayHistogram)
}

// Records that clientId of networkId applied version, whose entry api sequenced
// at entryTimeMs, when it is newer than the client's last ack and at or below
// the network's head. Reports whether it was recorded; only a recorded ack is
// observed, so a repeat counts once.
func AckNetworkAcl(ctx context.Context, networkId server.Id, clientId server.Id, version uint64, entryTimeMs int64, now time.Time) (recorded bool, returnErr error) {
	if !server.SidecarMg2Linked() {
		return false, ErrNetworkAclHeadUnavailable
	}
	if version == 0 || 1<<63 <= version {
		return false, ErrNetworkAclHeadInvalid
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_acl_ack (
					client_id,
					version,
					ack_time
				)
				SELECT $1, $2, $3
				WHERE EXISTS (
					SELECT 1
					FROM network_acl_head
					WHERE
						network_id = $4 AND
						$2 <= version
				)
				ON CONFLICT (client_id) DO UPDATE
				SET
					version = EXCLUDED.version,
					ack_time = EXCLUDED.ack_time
				WHERE network_acl_ack.version < EXCLUDED.version
			`,
			clientId,
			int64(version),
			now.UTC(),
			networkId,
		))
		recorded = tag.RowsAffected() == 1
	})
	if recorded && 0 < entryTimeMs {
		delay := now.Sub(time.UnixMilli(entryTimeMs))
		if 0 <= delay && delay <= DefaultNetworkAclAckSettings().MaxObservedDelay {
			networkAclAckDelayHistogram.Observe(delay.Seconds())
		}
	}
	return
}

// POST /acl/ack's body (C11): what the client applied.
type AclAckArgs struct {
	// The projection version it applied: the log position n of the newest entry
	// its engine installed.
	Version uint64 `json:"version"`
	// That entry's time_ms, api's clock when it sequenced the entry, as the
	// client read it from the verified entry. Zero: not sent, and nothing is
	// observed.
	TimeMs int64 `json:"time_ms,omitempty"`
}

// POST /acl/ack's answer.
type AclAckResult struct {
	// Whether the ack was recorded: false for a version not above the client's
	// last ack, above its network's head, or in a network with no head.
	Recorded bool `json:"recorded"`
}

const aclAckUnavailableMessage = "Acl acks are not available yet."

// POST /acl/ack (§8.9): records the bearer's own client's ack. Client class,
// on the strict_exp allowlist; the client and network come from the token.
func AclAck(ack *AclAckArgs, clientSession *session.ClientSession) (*AclAckResult, error) {
	if !server.SidecarMg2Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, aclAckUnavailableMessage)
	}
	if clientSession.ByJwt.ClientId == nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "An ack is posted by the client that applied the version.")
	}
	if ack == nil || ack.Version == 0 || 1<<63 <= ack.Version || ack.TimeMs < 0 {
		return nil, fmt.Errorf("%d %s", http.StatusBadRequest, "The version or time is invalid.")
	}
	recorded, err := AckNetworkAcl(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		*clientSession.ByJwt.ClientId,
		ack.Version,
		ack.TimeMs,
		server.NowUtc(),
	)
	if err != nil {
		return nil, err
	}
	return &AclAckResult{Recorded: recorded}, nil
}

// The version the client last acked, and false for none.
func GetNetworkAclAck(ctx context.Context, clientId server.Id) (version uint64, found bool) {
	if !server.SidecarMg2Linked() {
		return 0, false
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT version
				FROM network_acl_ack
				WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var ackVersion int64
				server.Raise(result.Scan(&ackVersion))
				version = uint64(ackVersion)
				found = true
			}
		})
	})
	return
}

const removeOldNetworkAclAcksSql = `
	DELETE FROM network_acl_ack
	WHERE client_id IN (
		SELECT client_id
		FROM network_acl_ack
		WHERE ack_time < $1
		ORDER BY ack_time
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	) AND ack_time < $1
`

// Removes the acks older than minTime, oldest first, for at most the retention
// run budget; the next run takes the rest of a backlog. Nothing without MG2.
func RemoveOldNetworkAclAcks(ctx context.Context, minTime time.Time) (removedCount int64) {
	if !server.SidecarMg2Linked() {
		return 0
	}
	removedCount, _ = removeRetentionBatches(
		ctx,
		RetentionTableNetworkAclAck,
		retentionBatchSize,
		time.Now().Add(retentionRunBudget),
		func(tx server.PgTx) int64 {
			tag := server.RaisePgResult(tx.Exec(ctx, removeOldNetworkAclAcksSql, minTime.UTC(), retentionBatchSize))
			return tag.RowsAffected()
		},
	)
	return
}
